package contract

// Duplo local do GitHub.
//
// ── Por que ele existe, e qual é o limite dele ───────────────────────────────
//
// Não temos GitHub de verdade na esteira nem no laptop de quem mexe no
// adaptador. Sem um duplo, a suíte de contrato do GitProvider seria um arquivo
// que ninguém roda — e a lição desta sessão é que suíte que não roda não
// verifica nada (o adaptador k8s de SecretStore passou meses sem nunca ter sido
// exercitado, e o duplo em memória só provava que ele era consistente consigo
// mesmo).
//
// O RISCO do duplo é o oposto e é pior: um duplo escrito a partir do que o MEU
// adaptador espera não prova nada — ele confirma as minhas suposições e o
// primeiro contato com o provedor real desmente tudo. Por isso as respostas
// daqui vêm da DOCUMENTAÇÃO do GitHub (a descrição OpenAPI publicada em
// github/rest-api-description e as páginas de docs.github.com), copiadas dos
// exemplos publicados, com os nomes e os códigos que eles publicam. Onde a
// documentação NÃO diz, está escrito `NÃO DOCUMENTADO` no comentário — e o
// adaptador foi desenhado para não depender desses pontos.
//
// O que este duplo NÃO prova, e nada aqui pode fingir que prova:
//   - que a mensagem não documentada do 422 de PR duplicado é essa;
//   - que a mensagem não documentada do erro de GraphQL num rebase conflitado é
//     essa;
//   - latência real, paginação, limites de taxa, e o comportamento assíncrono
//     de `mergeable` (aqui ele nasce calculado).
// Para isso existe o caminho sob a tag `integration`, contra um GitHub real.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHubFake é o servidor. Ele guarda estado de verdade (PRs abertos e
// mergeados) porque a idempotência das garantias 3 e 6 é sobre ESTADO: um duplo
// sem memória responderia sempre a mesma coisa e não provaria nenhuma das duas.
type GitHubFake struct {
	srv   *httptest.Server
	Token string

	mu   sync.Mutex
	prs  map[string][]*ghFakePR // repo → PRs
	seq  int
	log  []string
	subs int
}

type ghFakePR struct {
	Number   int
	NodeID   string
	Origem   string
	Destino  string
	Titulo   string
	Corpo    string
	Head     string
	Base     string
	Merged   bool
	MergeSHA string
	MergedAt time.Time
	Criado   time.Time
}

// Repositórios que este duplo conhece. Qualquer outro nome dá 404 — que é
// exatamente o que o GitHub faz, inclusive para repositório privado que o token
// não enxerga (a documentação é explícita: 404 em vez de 403 "to avoid
// confirming the existence of private repositories").
const (
	GHRepoOK           = "dop/plataforma"
	GHRepoComFila      = "dop/com-fila"
	GHRepoSemFila      = "dop/sem-fila"
	GHRepoFilaProibida = "dop/fila-proibida"
	GHRepoInvisivel    = "dop/nao-existe"
)

// Marcadores no NOME DO BRANCH escolhem o cenário. É a forma mais barata de dar
// à suíte um conflito sob demanda sem inventar uma API de configuração que o
// provedor real não teria.
const (
	MarcaConflito  = "conflito"
	MarcaBloqueado = "bloqueado"
	MarcaSemCommit = "sem-commits"
)

func NewGitHubFake(t *testing.T, token string) *GitHubFake {
	f := &GitHubFake{Token: token, prs: map[string][]*ghFakePR{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.rotear))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *GitHubFake) URL() string { return f.srv.URL }

// GraphQLURL é separado de propósito: no GitHub Enterprise o REST mora em
// /api/v3 e o GraphQL em /api/graphql, e um adaptador que deduzisse um do outro
// funcionaria contra o github.com e quebraria em toda instalação self-hosted.
func (f *GitHubFake) GraphQLURL() string { return f.srv.URL + "/graphql" }

// Chamadas devolve o log de requisições, para os testes que precisam afirmar
// sobre o que NÃO foi enviado (ver garantia 4).
func (f *GitHubFake) Chamadas() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *GitHubFake) responder(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// erroBasico é o schema `Basic Error` do OpenAPI do GitHub: message,
// documentation_url, url e status — TODOS opcionais.
func (f *GitHubFake) erroBasico(w http.ResponseWriter, code int, msg string) {
	f.responder(w, code, map[string]any{
		"message":           msg,
		"documentation_url": "https://docs.github.com/rest",
	})
}

func (f *GitHubFake) rotear(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	// O User-Agent é um dos poucos cabeçalhos que o GitHub documenta como
	// obrigatório de verdade: "Requests without a valid User-Agent header will
	// be rejected… you will receive a 403 Forbidden response".
	if r.Header.Get("User-Agent") == "" {
		f.erroBasico(w, http.StatusForbidden, "Request forbidden by administrative rules. Please make sure your request has a User-Agent header.")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		// NÃO DOCUMENTADO: o texto "Bad credentials" não aparece em nenhum
		// exemplo publicado pelo GitHub. O que É documentado é o STATUS 401
		// para credencial inválida e o schema Basic Error do corpo. O
		// adaptador classifica pelo status, nunca por este texto.
		f.erroBasico(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	p := strings.Trim(r.URL.Path, "/")
	if p == "graphql" {
		f.graphql(w, r)
		return
	}
	seg := strings.Split(p, "/")
	// /repos/{dono}/{nome}/...
	if len(seg) < 3 || seg[0] != "repos" {
		f.erroBasico(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := seg[1] + "/" + seg[2]
	if !ghRepoConhecido(repo) {
		f.erroBasico(w, http.StatusNotFound, "Not Found")
		return
	}
	resto := seg[3:]

	switch {
	case len(resto) == 0 && r.Method == http.MethodGet:
		// Só o campo que o adaptador usa; o objeto real é enorme e copiá-lo
		// inteiro não acrescentaria verificação nenhuma.
		f.responder(w, http.StatusOK, map[string]any{
			"full_name": repo, "default_branch": "main", "private": false,
		})

	case len(resto) == 3 && resto[0] == "rules" && resto[1] == "branches":
		f.regrasDeBranch(w, repo)

	case len(resto) == 1 && resto[0] == "pulls" && r.Method == http.MethodPost:
		f.criarPR(w, r, repo)

	case len(resto) == 1 && resto[0] == "pulls" && r.Method == http.MethodGet:
		f.listarPRs(w, r, repo)

	case len(resto) == 2 && resto[0] == "pulls" && r.Method == http.MethodGet:
		f.lerPR(w, repo, resto[1])

	case len(resto) == 3 && resto[0] == "pulls" && resto[2] == "merge" && r.Method == http.MethodPut:
		f.mergearPR(w, repo, resto[1])

	default:
		// O GitHub documenta que método não suportado devolve 404, e NÃO 405.
		f.erroBasico(w, http.StatusNotFound, "Not Found")
	}
}

func ghRepoConhecido(r string) bool {
	switch r {
	case GHRepoOK, GHRepoComFila, GHRepoSemFila, GHRepoFilaProibida:
		return true
	}
	return false
}

// regrasDeBranch reproduz GET /repos/{o}/{r}/rules/branches/{branch}: um ARRAY
// de regras, cada uma com `type`, `ruleset_source_type`, `ruleset_source`,
// `ruleset_id` e `parameters` — a forma publicada no exemplo da documentação.
func (f *GitHubFake) regrasDeBranch(w http.ResponseWriter, repo string) {
	if repo == GHRepoFilaProibida {
		// O caso da garantia 14: o adaptador não consegue OLHAR. Precisa virar
		// erro, jamais um `false` de conveniência.
		f.erroBasico(w, http.StatusForbidden,
			"Resource not accessible by personal access token")
		return
	}
	if repo != GHRepoComFila {
		// Regra de outro tipo, e não lista vazia: assim o teste prova que o
		// adaptador procura `merge_queue`, e não que ele conta elementos.
		f.responder(w, http.StatusOK, []any{
			map[string]any{
				"type": "required_linear_history", "ruleset_source_type": "Repository",
				"ruleset_source": repo, "ruleset_id": 42,
			},
		})
		return
	}
	// Os sete parâmetros de `merge_queue` são todos obrigatórios quando
	// `parameters` está presente, e os enums são MAIÚSCULOS aqui (ALLGREEN,
	// MERGE) — ao contrário do enum minúsculo do endpoint de merge. A
	// inconsistência é do GitHub e está copiada de propósito.
	f.responder(w, http.StatusOK, []any{
		map[string]any{
			"type": "merge_queue", "ruleset_source_type": "Organization",
			"ruleset_source": "dop", "ruleset_id": 73,
			"parameters": map[string]any{
				"check_response_timeout_minutes":    60,
				"grouping_strategy":                 "ALLGREEN",
				"max_entries_to_build":              5,
				"max_entries_to_merge":              5,
				"merge_method":                      "MERGE",
				"min_entries_to_merge":              1,
				"min_entries_to_merge_wait_minutes": 5,
			},
		},
	})
}

func (f *GitHubFake) achar(repo, origem, destino string) *ghFakePR {
	for _, pr := range f.prs[repo] {
		if pr.Origem == origem && pr.Destino == destino && !pr.Merged {
			return pr
		}
	}
	return nil
}

func (f *GitHubFake) criarPR(w http.ResponseWriter, r *http.Request, repo string) {
	var corpo struct {
		Head, Base, Title, Body string
	}
	_ = json.NewDecoder(r.Body).Decode(&corpo)

	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.Contains(corpo.Head, MarcaSemCommit) {
		// 422 com o schema Validation Error. NÃO DOCUMENTADO: o GitHub publica
		// só o schema, sem exemplo de `errors[]` para este caso. O texto abaixo
		// é plausível, não é promessa — e o adaptador NÃO o lê: diante de
		// qualquer 422 ele vai procurar o PR aberto, e é a AUSÊNCIA dele que
		// transforma o 422 em erro.
		f.responder(w, http.StatusUnprocessableEntity, map[string]any{
			"message":           "Validation Failed",
			"documentation_url": "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
			"errors": []any{map[string]any{
				"resource": "PullRequest", "code": "custom", "field": "base",
				"message": fmt.Sprintf("No commits between %s and %s", corpo.Base, corpo.Head),
			}},
		})
		return
	}
	if existe := f.achar(repo, corpo.Head, corpo.Base); existe != nil {
		// NÃO DOCUMENTADO: nem o `code`, nem o texto. O GitHub nunca publicou
		// um exemplo desta resposta — só o schema do 422. O adaptador foi
		// desenhado para NÃO depender dela: ele reage ao STATUS e depois
		// consulta a listagem, que é um fato, não uma string.
		f.responder(w, http.StatusUnprocessableEntity, map[string]any{
			"message":           "Validation Failed",
			"documentation_url": "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
			"errors": []any{map[string]any{
				"resource": "PullRequest", "code": "custom",
				"message": fmt.Sprintf("A pull request already exists for dop:%s.", corpo.Head),
			}},
		})
		return
	}

	f.seq++
	f.subs++
	pr := &ghFakePR{
		Number: 1300 + f.seq, NodeID: fmt.Sprintf("PR_kwDO%08d", f.seq),
		Origem: corpo.Head, Destino: corpo.Base, Titulo: corpo.Title, Corpo: corpo.Body,
		Head:   fmt.Sprintf("%040x", 0xC0FFEE00+f.seq),
		Base:   fmt.Sprintf("%040x", 0xBA5E0000),
		Criado: time.Date(2026, 8, 31, 12, 0, f.seq, 0, time.UTC),
	}
	f.prs[repo] = append(f.prs[repo], pr)
	f.responder(w, http.StatusCreated, f.json(repo, pr))
}

func (f *GitHubFake) listarPRs(w http.ResponseWriter, r *http.Request, repo string) {
	q := r.URL.Query()
	head, base := q.Get("head"), q.Get("base")
	// A documentação só descreve o formato COM prefixo ("user:ref-name" ou
	// "organization:ref-name") — então o duplo EXIGE o prefixo. Aceitá-lo sem
	// prefixo deixaria passar um adaptador que só funcionaria aqui.
	if head != "" && !strings.Contains(head, ":") {
		f.responder(w, http.StatusUnprocessableEntity, map[string]any{
			"message":           "Validation Failed",
			"documentation_url": "https://docs.github.com/rest/pulls/pulls#list-pull-requests",
			"errors": []any{map[string]any{
				"resource": "PullRequest", "field": "head", "code": "invalid",
				"message": "head must be in the format user:ref-name or organization:ref-name",
			}},
		})
		return
	}
	if i := strings.Index(head, ":"); i >= 0 {
		head = head[i+1:]
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	out := []any{}
	for _, pr := range f.prs[repo] {
		if pr.Merged && q.Get("state") == "open" {
			continue
		}
		if head != "" && pr.Origem != head {
			continue
		}
		if base != "" && pr.Destino != base {
			continue
		}
		out = append(out, f.json(repo, pr))
	}
	f.responder(w, http.StatusOK, out)
}

func (f *GitHubFake) lerPR(w http.ResponseWriter, repo, num string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.porNumero(repo, num)
	if pr == nil {
		f.erroBasico(w, http.StatusNotFound, "Not Found")
		return
	}
	f.responder(w, http.StatusOK, f.json(repo, pr))
}

func (f *GitHubFake) porNumero(repo, num string) *ghFakePR {
	n, _ := strconv.Atoi(num)
	for _, pr := range f.prs[repo] {
		if pr.Number == n {
			return pr
		}
	}
	return nil
}

func (f *GitHubFake) mergearPR(w http.ResponseWriter, repo, num string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.porNumero(repo, num)
	if pr == nil {
		f.erroBasico(w, http.StatusNotFound, "Not Found")
		return
	}
	// AQUI ESTÁ A AMBIGUIDADE QUE A PORTA PRECISA DESFAZER: o GitHub responde
	// exatamente a mesma coisa — 405, com a mesma frase — para PR já mergeado,
	// PR com conflito e PR bloqueado por checagem. O corpo publicado NÃO traz
	// `documentation_url`, e o duplo copia essa ausência.
	if pr.Merged || strings.Contains(pr.Origem, MarcaConflito) || strings.Contains(pr.Origem, MarcaBloqueado) {
		f.responder(w, http.StatusMethodNotAllowed, map[string]any{
			"message": "Pull Request is not mergeable",
		})
		return
	}
	pr.Merged = true
	pr.MergeSHA = fmt.Sprintf("%040x", 0x0EADBEEF00+pr.Number)
	pr.MergedAt = time.Date(2026, 8, 31, 13, 0, 0, 0, time.UTC)
	f.responder(w, http.StatusOK, map[string]any{
		"sha": pr.MergeSHA, "merged": true, "message": "Pull Request successfully merged",
	})
}

// json monta o objeto de PR com os nomes e a forma do exemplo publicado.
func (f *GitHubFake) json(repo string, pr *ghFakePR) map[string]any {
	estado := "open"
	if pr.Merged {
		estado = "closed"
	}
	// `mergeable` é NULÁVEL no schema (nulo enquanto o GitHub calcula). Aqui
	// ele nasce calculado — é uma SIMPLIFICAÇÃO do duplo, e por isso o caminho
	// do "ainda calculando" do adaptador só é exercitado contra o provedor
	// real. Está anotado no relatório.
	mergeable := !strings.Contains(pr.Origem, MarcaConflito)
	estadoMerge := "clean"
	switch {
	case strings.Contains(pr.Origem, MarcaConflito):
		estadoMerge = "dirty"
	case strings.Contains(pr.Origem, MarcaBloqueado):
		estadoMerge = "blocked"
	}
	m := map[string]any{
		"url":        fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repo, pr.Number),
		"id":         pr.Number,
		"node_id":    pr.NodeID,
		"html_url":   fmt.Sprintf("https://github.com/%s/pull/%d", repo, pr.Number),
		"number":     pr.Number,
		"state":      estado,
		"locked":     false,
		"title":      pr.Titulo,
		"body":       pr.Corpo,
		"created_at": pr.Criado.Format(time.RFC3339),
		"updated_at": pr.Criado.Format(time.RFC3339),
		"head": map[string]any{
			"label": "dop:" + pr.Origem, "ref": pr.Origem, "sha": pr.Head,
		},
		"base": map[string]any{
			"label": "dop:" + pr.Destino, "ref": pr.Destino, "sha": pr.Base,
		},
		"draft":           false,
		"merged":          pr.Merged,
		"mergeable":       mergeable,
		"rebaseable":      mergeable,
		"mergeable_state": estadoMerge,
	}
	if pr.Merged {
		m["merged_at"] = pr.MergedAt.Format(time.RFC3339)
		m["merge_commit_sha"] = pr.MergeSHA
	} else {
		m["merged_at"] = nil
		m["merge_commit_sha"] = nil
	}
	return m
}

// graphql atende a mutação `updatePullRequestBranch`.
//
// O ponto INTEIRO deste trecho: o GraphQL do GitHub responde HTTP 200 mesmo
// quando a mutação FALHA — o fracasso vem no array `errors` do corpo. Um
// adaptador que olhasse só o status relataria "rebase feito" para todo
// conflito, e a fila da ADR-0008 mergearia sobre um branch não reaplicado. O
// duplo reproduz essa armadilha de propósito.
func (f *GitHubFake) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !strings.Contains(req.Query, "updatePullRequestBranch") {
		f.responder(w, http.StatusOK, map[string]any{
			"errors": []any{map[string]any{"message": "unsupported query in this double"}},
		})
		return
	}
	// A mutação recebe o NODE ID, não o número — é a diferença que obriga o
	// adaptador a localizar o PR antes.
	node, _ := req.Variables["pr"].(string)

	f.mu.Lock()
	defer f.mu.Unlock()
	var alvo *ghFakePR
	for _, prs := range f.prs {
		for _, pr := range prs {
			if pr.NodeID == node {
				alvo = pr
			}
		}
	}
	if alvo == nil {
		f.responder(w, http.StatusOK, map[string]any{
			"errors": []any{map[string]any{
				"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of '" + node + "'.",
			}},
		})
		return
	}
	if strings.Contains(alvo.Origem, MarcaConflito) {
		// NÃO DOCUMENTADO: o GitHub não publica o texto do erro de rebase
		// conflitado, nem um campo estruturado que diga "conflito". Este texto
		// é uma APOSTA razoável, e é exatamente por isso que o adaptador
		// classifica por marcador e cai no lado do ERRO quando não reconhece —
		// errar para o lado do conflito mandaria um humano resolver um
		// problema de permissão na caixa de atenção.
		f.responder(w, http.StatusOK, map[string]any{
			"data": map[string]any{"updatePullRequestBranch": nil},
			"errors": []any{map[string]any{
				"type":    "UNPROCESSABLE",
				"message": "merge conflict between base and head",
			}},
		})
		return
	}
	// Rebase feito: o topo muda. É o que a fila re-verifica na posição seguinte.
	alvo.Head = fmt.Sprintf("%040x", 0xEBA5E00+alvo.Number)
	f.responder(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"updatePullRequestBranch": map[string]any{
				"pullRequest": map[string]any{"id": alvo.NodeID},
			},
		},
	})
}
