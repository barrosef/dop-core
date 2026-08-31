package contract

// Duplo local do GitLab. Ver o cabeçalho de gitprovider_github_fake.go para o
// PORQUÊ de existir um duplo e para o limite do que ele prova.
//
// As respostas daqui vêm da documentação da API v4 do GitLab (docs.gitlab.com,
// seções merge_requests, projects e rest/authentication). Onde a documentação
// não diz, está escrito `NÃO DOCUMENTADO`.
//
// Este lado é mais firme que o do GitHub num ponto e mais frouxo em outro:
//   - o texto do rebase conflitado É documentado palavra por palavra
//     ("Rebase failed. Please rebase locally") — aqui não há aposta;
//   - a recusa por MR duplicado NÃO é documentada em lugar nenhum: nem o
//     código, nem o corpo. O 409 e o texto abaixo vêm do código-fonte do
//     GitLab, não das docs, e o adaptador foi desenhado para não depender deles.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type GitLabFake struct {
	srv   *httptest.Server
	Token string

	mu  sync.Mutex
	mrs map[string][]*glFakeMR
	seq int
	log []string
}

type glFakeMR struct {
	IID      int64
	ID       int64
	Origem   string
	Destino  string
	Titulo   string
	Corpo    string
	SHA      string
	Base     string
	Merged   bool
	MergeSHA string
	MergedAt time.Time
	Criado   time.Time
	// Rebases restantes até o worker "terminar". Existe para exercitar de
	// verdade o polling da garantia 9: um duplo que respondesse
	// `rebase_in_progress:false` de primeira nunca faria o laço de espera
	// rodar, e o adaptador poderia estar quebrado ali sem ninguém saber.
	RebasesPendentes int
	ErroDeMerge      string
}

const (
	GLProjOK         = "dop/plataforma"
	GLProjComTrem    = "dop/com-trem"
	GLProjSemTrem    = "dop/sem-trem"
	GLProjSemLicenca = "dop/sem-licenca"
	GLProjEscopoRuim = "dop/escopo-insuficiente"
	GLProjInvisivel  = "dop/nao-existe"
)

func NewGitLabFake(t *testing.T, token string) *GitLabFake {
	f := &GitLabFake{Token: token, mrs: map[string][]*glFakeMR{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.rotear))
	t.Cleanup(f.srv.Close)
	return f
}

// URL já inclui o /api/v4, como a base real.
func (f *GitLabFake) URL() string { return f.srv.URL + "/api/v4" }

func (f *GitLabFake) Chamadas() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *GitLabFake) responder(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// erro devolve o formato padrão do GitLab: {"message": "<texto>"}. Repare que
// o `message` do GitLab é polimórfico — string aqui, LISTA na duplicidade,
// OBJETO na validação de campo. É a divergência que obriga o adaptador a achatar
// os três formatos em vez de decodificar um struct.
func (f *GitLabFake) erro(w http.ResponseWriter, code int, msg any) {
	f.responder(w, code, map[string]any{"message": msg})
}

func (f *GitLabFake) rotear(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	// Os dois cabeçalhos são documentados: PRIVATE-TOKEN para token pessoal/de
	// projeto, Authorization: Bearer para OAuth. O duplo aceita os dois porque
	// os dois são legítimos, e qual usar depende do TIPO da credencial.
	tok := r.Header.Get("PRIVATE-TOKEN")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if tok != f.Token {
		// Documentado palavra por palavra: {"message": "401 Unauthorized"}.
		f.erro(w, http.StatusUnauthorized, "401 Unauthorized")
		return
	}

	// EscapedPath, e NÃO Path: o identificador de projeto viaja com a barra
	// escapada ("dop%2Fplataforma"), e `r.URL.Path` já vem decodificado — o
	// que transformaria um segmento em dois e faria todo projeto com caminho
	// virar 404. É a mesma armadilha do lado do cliente, do outro lado do fio.
	p := strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4"), "/")
	seg := strings.Split(p, "/")
	if len(seg) < 2 || seg[0] != "projects" {
		f.erro(w, http.StatusNotFound, "404 Not Found")
		return
	}
	// O caminho vem escapado ("dop%2Fplataforma"); o servidor real desescapa.
	proj, err := url.PathUnescape(seg[1])
	if err != nil {
		f.erro(w, http.StatusNotFound, "404 Project Not Found")
		return
	}
	if !glProjConhecido(proj) {
		// Documentado: o mesmo 404 para projeto inexistente E para projeto que
		// o token não alcança — o GitLab mascara o 403 nesse caso.
		f.erro(w, http.StatusNotFound, "404 Project Not Found")
		return
	}
	resto := seg[2:]

	switch {
	case len(resto) == 0 && r.Method == http.MethodGet:
		f.lerProjeto(w, proj)

	case len(resto) == 1 && resto[0] == "merge_requests" && r.Method == http.MethodPost:
		f.criarMR(w, r, proj)

	case len(resto) == 1 && resto[0] == "merge_requests" && r.Method == http.MethodGet:
		f.listarMRs(w, r, proj)

	case len(resto) == 2 && resto[0] == "merge_requests" && r.Method == http.MethodGet:
		f.lerMR(w, r, proj, resto[1])

	case len(resto) == 3 && resto[0] == "merge_requests" && resto[2] == "rebase" && r.Method == http.MethodPut:
		f.rebasear(w, proj, resto[1])

	case len(resto) == 3 && resto[0] == "merge_requests" && resto[2] == "merge" && r.Method == http.MethodPut:
		f.mergear(w, proj, resto[1])

	default:
		f.erro(w, http.StatusNotFound, "404 Not Found")
	}
}

func glProjConhecido(p string) bool {
	switch p {
	case GLProjOK, GLProjComTrem, GLProjSemTrem, GLProjSemLicenca, GLProjEscopoRuim:
		return true
	}
	return false
}

// lerProjeto reproduz o TRI-ESTADO de `merge_trains_enabled`, que é a sutileza
// inteira da garantia 14 deste lado.
//
// O campo é exposto por código de Enterprise Edition sob condição de licença
// (`if: project.feature_available?(:merge_pipelines)`): numa instalação sem o
// recurso a CHAVE NÃO APARECE — ela não vem `false`, ela some. Um cliente que
// desserializasse em `bool` leria `false` nos dois casos e nunca saberia a
// diferença; por isso o adaptador usa ponteiro.
func (f *GitLabFake) lerProjeto(w http.ResponseWriter, proj string) {
	if proj == GLProjEscopoRuim {
		// O caso "não consigo olhar" da garantia 14. Formato documentado para
		// falha de escopo de OAuth: {"error": "insufficient_scope", …} — que é
		// a exceção ao {"message": …} do resto da API.
		f.responder(w, http.StatusForbidden, map[string]any{
			"error":             "insufficient_scope",
			"error_description": "The request requires higher privileges than provided by the access token.",
			"scope":             "api read_api",
		})
		return
	}
	m := map[string]any{
		"id": 3, "path_with_namespace": proj, "default_branch": "main",
		"merge_method": "merge",
	}
	switch proj {
	case GLProjComTrem:
		m["merge_pipelines_enabled"] = true
		m["merge_trains_enabled"] = true
	case GLProjSemLicenca:
		// A chave simplesmente não vai. É o caso que o ponteiro existe para ver.
	default:
		m["merge_pipelines_enabled"] = false
		m["merge_trains_enabled"] = false
	}
	f.responder(w, http.StatusOK, m)
}

func (f *GitLabFake) achar(proj, origem, destino string) *glFakeMR {
	for _, mr := range f.mrs[proj] {
		if mr.Origem == origem && mr.Destino == destino && !mr.Merged {
			return mr
		}
	}
	return nil
}

func (f *GitLabFake) criarMR(w http.ResponseWriter, r *http.Request, proj string) {
	var corpo struct {
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Description  string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&corpo)

	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.Contains(corpo.SourceBranch, MarcaSemCommit) {
		// Fonte: código do GitLab, NÃO as docs. `validate_branch_existence`
		// adiciona o erro na chave :source_branch, que não está na lista de
		// chaves 422, então cai no 400 de validação — com `message` como
		// OBJETO de campo→lista. Terceiro formato de `message` da mesma API.
		f.erro(w, http.StatusBadRequest, map[string]any{
			"source_branch": []string{"does not exist"},
		})
		return
	}
	if existe := f.achar(proj, corpo.SourceBranch, corpo.TargetBranch); existe != nil {
		// NÃO DOCUMENTADO. Vem do código (`conflicting_mr_message` +
		// `conflict!` → 409, corpo `{"message" => errors[:validate_branches]}`,
		// que é um Array). O adaptador não lê este texto: reage ao status e
		// depois CONSULTA a listagem.
		f.erro(w, http.StatusConflict, []string{
			fmt.Sprintf("Another open merge request already exists for this source branch: !%d", existe.IID),
		})
		return
	}

	f.seq++
	mr := &glFakeMR{
		IID: int64(f.seq), ID: int64(100 + f.seq),
		Origem: corpo.SourceBranch, Destino: corpo.TargetBranch,
		Titulo: corpo.Title, Corpo: corpo.Description,
		SHA:    fmt.Sprintf("%040x", 0xC0FFEE00+f.seq),
		Base:   fmt.Sprintf("%040x", 0xBA5E0000),
		Criado: time.Date(2026, 8, 31, 12, 0, f.seq, 0, time.UTC),
	}
	f.mrs[proj] = append(f.mrs[proj], mr)
	f.responder(w, http.StatusCreated, f.json(proj, mr, false))
}

func (f *GitLabFake) listarMRs(w http.ResponseWriter, r *http.Request, proj string) {
	q := r.URL.Query()
	origem, destino, estado := q.Get("source_branch"), q.Get("target_branch"), q.Get("state")

	f.mu.Lock()
	defer f.mu.Unlock()
	out := []any{}
	for _, mr := range f.mrs[proj] {
		if estado == "opened" && mr.Merged {
			continue
		}
		if origem != "" && mr.Origem != origem {
			continue
		}
		if destino != "" && mr.Destino != destino {
			continue
		}
		out = append(out, f.json(proj, mr, false))
	}
	f.responder(w, http.StatusOK, out)
}

func (f *GitLabFake) porIID(proj, iid string) *glFakeMR {
	n, _ := strconv.ParseInt(iid, 10, 64)
	for _, mr := range f.mrs[proj] {
		if mr.IID == n {
			return mr
		}
	}
	return nil
}

func (f *GitLabFake) lerMR(w http.ResponseWriter, r *http.Request, proj, iid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr := f.porIID(proj, iid)
	if mr == nil {
		f.erro(w, http.StatusNotFound, "404 Not found")
		return
	}
	// `include_rebase_in_progress` é o parâmetro documentado: SEM ele o campo
	// nem aparece na resposta. Um duplo que devolvesse o campo sempre deixaria
	// passar um adaptador que esquecesse o parâmetro — e contra o GitLab real
	// esse adaptador leria a ausência como "terminou".
	comRebase := r.URL.Query().Get("include_rebase_in_progress") == "true"
	if comRebase && mr.RebasesPendentes > 0 {
		mr.RebasesPendentes--
		if mr.RebasesPendentes == 0 && mr.ErroDeMerge == "" {
			mr.SHA = fmt.Sprintf("%040x", 0xEBA5E00+mr.IID)
		}
	}
	f.responder(w, http.StatusOK, f.json(proj, mr, comRebase))
}

// rebasear reproduz a rota ASSÍNCRONA: 202 com {"rebase_in_progress": true},
// exatamente o corpo publicado.
func (f *GitLabFake) rebasear(w http.ResponseWriter, proj, iid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr := f.porIID(proj, iid)
	if mr == nil {
		f.erro(w, http.StatusNotFound, "404 Not found")
		return
	}
	// Duas voltas de polling: a primeira leitura ainda encontra o worker
	// trabalhando. É o que faz o laço de espera do adaptador rodar de verdade.
	mr.RebasesPendentes = 2
	if strings.Contains(mr.Origem, MarcaConflito) {
		// DOCUMENTADO palavra por palavra, incluindo a ausência de ponto final.
		mr.ErroDeMerge = "Rebase failed. Please rebase locally"
	}
	f.responder(w, http.StatusAccepted, map[string]any{"rebase_in_progress": true})
}

func (f *GitLabFake) mergear(w http.ResponseWriter, proj, iid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr := f.porIID(proj, iid)
	if mr == nil {
		f.erro(w, http.StatusNotFound, "404 Not found")
		return
	}
	// A MESMA ambiguidade do GitHub, com outro número: 405 cobre já mergeado,
	// conflito e bloqueio. Documentado: "405 | 405 Method Not Allowed | The
	// merge request cannot merge" — com o prefixo numérico dentro do texto.
	if mr.Merged || strings.Contains(mr.Origem, MarcaConflito) || strings.Contains(mr.Origem, MarcaBloqueado) {
		f.erro(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
		return
	}
	mr.Merged = true
	mr.MergeSHA = fmt.Sprintf("%040x", 0x0EADBEEF00+mr.IID)
	mr.MergedAt = time.Date(2026, 8, 31, 13, 0, 0, 0, time.UTC)
	f.responder(w, http.StatusOK, f.json(proj, mr, false))
}

func (f *GitLabFake) json(proj string, mr *glFakeMR, comRebase bool) map[string]any {
	estado := "opened"
	if mr.Merged {
		estado = "merged"
	}
	// `detailed_merge_status` é o campo atual; `merge_status` é o legado
	// (depreciado no 15.6) e continua vindo — o duplo manda os DOIS, como o
	// GitLab manda, para que o adaptador que lê só um seja pego.
	detalhado, legado := "mergeable", "can_be_merged"
	switch {
	case mr.Merged:
		detalhado, legado = "not_open", "can_be_merged"
	case strings.Contains(mr.Origem, MarcaConflito):
		detalhado, legado = "conflict", "cannot_be_merged"
	case strings.Contains(mr.Origem, MarcaBloqueado):
		detalhado, legado = "ci_still_running", "can_be_merged"
	}
	m := map[string]any{
		"id": mr.ID, "iid": mr.IID, "project_id": 3,
		"title": mr.Titulo, "description": mr.Corpo,
		"state":         estado,
		"source_branch": mr.Origem, "target_branch": mr.Destino,
		"web_url":               fmt.Sprintf("http://gitlab.example.com/%s/-/merge_requests/%d", proj, mr.IID),
		"sha":                   mr.SHA,
		"merge_status":          legado,
		"detailed_merge_status": detalhado,
		// Documentado: has_conflicts "Returns false unless merge_status is
		// cannot_be_merged". O duplo respeita essa dependência em vez de
		// preencher os dois independentemente.
		"has_conflicts": legado == "cannot_be_merged",
		"created_at":    mr.Criado.Format("2006-01-02T15:04:05.000Z"),
		"updated_at":    mr.Criado.Format("2006-01-02T15:04:05.000Z"),
		"draft":         false,
		"squash":        false,
		"diff_refs": map[string]any{
			"base_sha": mr.Base, "head_sha": mr.SHA, "start_sha": mr.Base,
		},
		"squash_commit_sha": nil,
	}
	if mr.Merged {
		m["merged_at"] = mr.MergedAt.Format("2006-01-02T15:04:05.000Z")
		// A documentação mostra `merge_commit_sha: null` MESMO na resposta de
		// merge — é artefato do exemplo enlatado que o GitLab reusa em todas as
		// seções. O duplo devolve um SHA de verdade, que é o comportamento real
		// e o único que permite verificar a garantia 7. A divergência está
		// anotada no relatório.
		m["merge_commit_sha"] = mr.MergeSHA
	} else {
		m["merged_at"] = nil
		m["merge_commit_sha"] = nil
	}
	if comRebase {
		m["rebase_in_progress"] = mr.RebasesPendentes > 0
		if mr.RebasesPendentes == 0 && mr.ErroDeMerge != "" {
			m["merge_error"] = mr.ErroDeMerge
		} else {
			m["merge_error"] = nil
		}
	} else {
		m["merge_error"] = nil
	}
	return m
}
