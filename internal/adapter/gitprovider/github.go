// Adaptador de delivery.GitProvider sobre o GitHub.
//
// ── Por que REST **e** GraphQL ────────────────────────────────────────────────
//
// Porque o GitHub NÃO TEM REBASE NO REST. O único endpoint parecido é
// `PUT /pulls/{n}/update-branch`, e a documentação dele é explícita: ele atualiza
// o branch do PR "merging HEAD from the base branch into the pull request
// branch". Não existe parâmetro `update_method`, não existe outra rota, e o
// anúncio de 2022 que trouxe a opção de rebase é só da interface web.
//
// Isso importa muito mais do que parece: usar `update-branch` para cumprir
// `Rebase` seria fazer uma operação DIFERENTE da pedida, com sucesso aparente. A
// fila da ADR-0008 reaplica cada PR sobre a `main` atualizada e RE-VERIFICA; um
// merge de main→branch também "atualiza", mas produz um histórico e um commit de
// topo diferentes do que o domínio pediu, e nada no retorno denunciaria a troca.
// A alternativa honesta é o GraphQL, onde o rebase existe de verdade:
//
//	updatePullRequestBranch(input: { pullRequestId: <node id>, updateMethod: REBASE })
//
// Repare no preço: a mutação recebe o NODE ID do PR (não o número), e por isso o
// adaptador precisa localizar o PR antes. Ver Rebase.
//
// ── O que o GitHub esconde ───────────────────────────────────────────────────
//
// Duas respostas de que este adaptador depende NÃO são publicadas pelo GitHub:
// o texto do 422 quando já existe PR para o branch, e o texto do erro de
// GraphQL quando o rebase conflita. Onde deu, o adaptador foi desenhado para
// não precisar do texto (ver OpenPullRequest); onde não deu, o comentário diz
// exatamente onde está a aposta.
package gitprovider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type GitHub struct {
	rest *client
	gql  *client
	// actorID é o ator pelo qual ESTA conexão fala (garantia 13). Não é
	// credencial: é o nome que a conferência compara.
	actorID       string
	mergeMethod   string
	rebaseTimeout time.Duration
	poll          time.Duration
}

type GitHubConfig struct {
	// APIBase é https://api.github.com no serviço público e
	// https://<host>/api/v3 no GitHub Enterprise.
	APIBase string
	// GraphQLURL é separado porque no Enterprise ele NÃO é APIBase+"/graphql":
	// o REST mora em /api/v3 e o GraphQL em /api/graphql. Deduzir um do outro
	// funcionaria no github.com e quebraria em toda instalação self-hosted —
	// exatamente o tipo de diferença que só aparece no cliente.
	GraphQLURL string
	// Token é o valor JÁ RESOLVIDO da credencial de recurso (ADR-0013). Este
	// pacote não conhece ports.SecretStore.
	Token   string
	ActorID string
	// MergeMethod é política do fluxo git (ADR-0013), não do domínio.
	MergeMethod   string // merge | squash | rebase
	Timeout       time.Duration
	RebaseTimeout time.Duration
	Poll          time.Duration
	Client        httpDoer
}

func NewGitHub(cfg GitHubConfig) *GitHub {
	base := cfg.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	base = strings.TrimRight(base, "/")
	gql := cfg.GraphQLURL
	if gql == "" {
		gql = base + "/graphql"
	}
	autorizar := func(r *http.Request) {
		if cfg.Token != "" {
			r.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		// Versão FIXADA no adaptador, não configurável. É a promessa que o
		// GitHub faz de não mudar formato de resposta debaixo de nós; deixá-la
		// de fora significa aceitar a versão default, que muda sozinha.
		r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		r.Header.Set("Accept", "application/vnd.github+json")
		// O GitHub REJEITA com 403 requisição sem User-Agent — é um dos poucos
		// cabeçalhos que a documentação chama de obrigatório de verdade.
		r.Header.Set("User-Agent", "dop-core")
	}
	mm := cfg.MergeMethod
	if mm == "" {
		mm = "merge"
	}
	rt := cfg.RebaseTimeout
	if rt <= 0 {
		rt = DefaultRebaseTimeout
	}
	p := cfg.Poll
	if p <= 0 {
		p = defaultPoll
	}
	return &GitHub{
		rest:          newClient(base, "GitHub", cfg.Client, cfg.Timeout, autorizar, cfg.Token),
		gql:           newClient(gql, "GitHub", cfg.Client, cfg.Timeout, autorizar, cfg.Token),
		actorID:       cfg.ActorID,
		mergeMethod:   mm,
		rebaseTimeout: rt,
		poll:          p,
	}
}

var _ delivery.GitProvider = (*GitHub)(nil)

// String: receptor por VALOR, para valer também em `%+v` de um valor. Sem ele,
// o fmt percorreria os campos por reflexão — inclusive os não exportados, cujo
// String() ele não consegue chamar.
func (g GitHub) String() string { return "gitprovider.GitHub{ator=" + g.actorID + "}" }

// ── formas do GitHub (só o que a porta usa) ──────────────────────────────────

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type ghPR struct {
	Number         int     `json:"number"`
	NodeID         string  `json:"node_id"`
	HTMLURL        string  `json:"html_url"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	Merged         bool    `json:"merged"`
	Mergeable      *bool   `json:"mergeable"` // nulo enquanto o GitHub calcula
	MergeableState string  `json:"mergeable_state"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	CreatedAt      string  `json:"created_at"`
	MergedAt       *string `json:"merged_at"`
	Head           ghRef   `json:"head"`
	Base           ghRef   `json:"base"`
}

type ghErro struct {
	Message string `json:"message"`
	Errors  []struct {
		Resource string `json:"resource"`
		Field    string `json:"field"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// explain achata o corpo de erro do GitHub numa frase.
func explicarGH(body []byte) string {
	var e ghErro
	_ = jsonUnmarshalTolerante(body, &e)
	partes := make([]string, 0, 1+len(e.Errors))
	if s := strings.TrimSpace(e.Message); s != "" {
		partes = append(partes, s)
	}
	for _, it := range e.Errors {
		if s := strings.TrimSpace(it.Message); s != "" {
			partes = append(partes, s)
			continue
		}
		if it.Field != "" || it.Code != "" {
			partes = append(partes, strings.TrimSpace(it.Resource+"."+it.Field+": "+it.Code))
		}
	}
	if len(partes) == 0 {
		return strings.TrimSpace(string(body))
	}
	return strings.Join(partes, "; ")
}

// dono e nome a partir de "owner/repo".
func partesRepo(externo string) (dono, nome string, err error) {
	p := strings.SplitN(strings.Trim(externo, "/"), "/", 3)
	if len(p) != 2 || p[0] == "" || p[1] == "" {
		return "", "", errs.Invalid(
			"identificador de repositório do GitHub deve ser \"dono/nome\", veio %q", externo)
	}
	return p[0], p[1], nil
}

func (g *GitHub) prPath(dono, nome string, n int) string {
	return fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(dono), url.PathEscape(nome), n)
}

func (g *GitHub) prToPorta(pr ghPR) delivery.ProviderPR {
	return delivery.ProviderPR{
		// ExternalID é OPACO para o domínio (garantia 10): aqui é o número do
		// PR, e nada fora deste arquivo pode contar com isso.
		ExternalID:   strconv.Itoa(pr.Number),
		URL:          pr.HTMLURL,
		HeadCommit:   pr.Head.SHA,
		TargetBranch: pr.Base.Ref,
		CreatedAt:    instante(pr.CreatedAt),
	}
}

// ── OpenPullRequest ──────────────────────────────────────────────────────────

// OpenPullRequest abre o PR, e é IDEMPOTENTE por (repo, origem, destino).
//
// O jeito de cumprir a garantia 3 aqui merece explicação, porque o caminho
// óbvio é uma armadilha: o GitHub recusa o segundo PR com 422 e uma mensagem de
// validação — e essa mensagem NÃO É DOCUMENTADA. O corpo publicado do 422 tem
// só o schema; o texto "A pull request already exists for …" é folclore de
// observação, e o GitHub nunca prometeu mantê-lo.
//
// Então o adaptador NÃO casa texto. Diante de QUALQUER 422 ele vai PROCURAR o
// PR aberto para aquele par de branches: se existe, o 422 era duplicidade e a
// resposta é o PR existente; se não existe, o 422 era outra coisa (branch sem
// commits, destino inválido) e vira erro com a mensagem do provedor junto.
// A idempotência passa a depender de um FATO consultável em vez de uma string
// que pode mudar sem aviso.
func (g *GitHub) OpenPullRequest(ctx context.Context, spec delivery.OpenPRSpec) (delivery.ProviderPR, error) {
	if err := conferirAtor(g.actorID, spec.ActorID, "a abertura do PR"); err != nil {
		return delivery.ProviderPR{}, err
	}
	dono, nome, err := partesRepo(spec.RepoExternalID)
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	destino := spec.TargetBranch
	if destino == "" {
		destino = "main"
	}

	code, body, err := g.rest.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(dono), url.PathEscape(nome)),
		map[string]any{
			// `head` SEM o prefixo do dono: no CORPO da criação ele só é
			// necessário para PR entre forks. Na LISTAGEM, logo abaixo, o
			// prefixo é obrigatório. A assimetria é do GitHub, não nossa.
			"head":  spec.SourceBranch,
			"base":  destino,
			"title": spec.Title,
			"body":  spec.Body,
		})
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	switch {
	case code == http.StatusCreated:
		var pr ghPR
		if err := g.rest.decode(body, &pr, "abertura de PR"); err != nil {
			return delivery.ProviderPR{}, err
		}
		return g.prToPorta(pr), nil

	case code == http.StatusUnprocessableEntity:
		existente, err := g.prAberto(ctx, dono, nome, spec.SourceBranch, destino)
		if err != nil {
			return delivery.ProviderPR{}, err
		}
		if existente != nil {
			// Garantia 4: devolve o que existe, com o título e o corpo
			// ORIGINAIS. Não há PATCH aqui de propósito — o pacote de
			// evidência da ADR-0007 §4 não pode ser substituído por um retry.
			return g.prToPorta(*existente), nil
		}
		return delivery.ProviderPR{}, errs.Invalid(
			"o GitHub recusou a abertura do PR de %q para %q: %s",
			spec.SourceBranch, destino, g.rest.redact(explicarGH(body)))
	}
	return delivery.ProviderPR{}, g.rest.fail(code, explicarGH(body),
		fmt.Sprintf("a abertura de PR em %s/%s", dono, nome))
}

// prAberto localiza o PR ABERTO de um par de branches. Devolve (nil, nil)
// quando não há — ausência não é erro aqui, é a informação pedida.
func (g *GitHub) prAberto(ctx context.Context, dono, nome, origem, destino string) (*ghPR, error) {
	// `head` na listagem EXIGE o prefixo do dono ("dono:branch") — é o formato
	// que a documentação descreve, e o único que ela descreve.
	q := url.Values{}
	q.Set("head", dono+":"+origem)
	q.Set("base", destino)
	q.Set("state", "open")
	q.Set("per_page", "100")
	code, body, err := g.rest.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls?%s", url.PathEscape(dono), url.PathEscape(nome), q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explicarGH(body),
			fmt.Sprintf("a busca de PR aberto em %s/%s", dono, nome))
	}
	var prs []ghPR
	if err := g.rest.decode(body, &prs, "listagem de PRs"); err != nil {
		return nil, err
	}
	for i := range prs {
		if prs[i].Head.Ref == origem && prs[i].Base.Ref == destino {
			return &prs[i], nil
		}
	}
	return nil, nil
}

func (g *GitHub) prPorNumero(ctx context.Context, dono, nome string, n int) (*ghPR, error) {
	code, body, err := g.rest.do(ctx, http.MethodGet, g.prPath(dono, nome, n), nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explicarGH(body), fmt.Sprintf("a leitura do PR #%d", n))
	}
	var pr ghPR
	if err := g.rest.decode(body, &pr, "leitura de PR"); err != nil {
		return nil, err
	}
	return &pr, nil
}

// ── Rebase ───────────────────────────────────────────────────────────────────

// Rebase reaplica o branch do PR sobre o destino, pela mutação de GraphQL.
//
// Duas coisas que a assinatura da porta esconde e que valem mais que o código:
//
//  1. NÃO EXISTE "rebase de branch" nos provedores. O que existe é "reaplique o
//     branch DESTE PR". Os dois — GitHub e GitLab — exigem um PR/MR aberto e
//     reaplicam sobre o DESTINO DELE. Por isso `Onto` não é livre: se não bate
//     com o destino do PR, o pedido é recusado em vez de reaplicado em cima do
//     lugar errado em silêncio;
//
//  2. o GitHub responde a mutação com HTTP 200 mesmo quando ela FALHA — o
//     fracasso vem no array `errors` do corpo. Um adaptador que olhasse só o
//     código de status relataria "rebase feito" para todo conflito, e a fila da
//     ADR-0008 mergearia em cima de um branch que não foi reaplicado.
func (g *GitHub) Rebase(ctx context.Context, spec delivery.RebaseSpec) (delivery.RebaseResult, error) {
	dono, nome, err := partesRepo(spec.RepoExternalID)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.rebaseTimeout)
	defer cancel()

	pr, err := g.prAberto(ctx, dono, nome, spec.Branch, spec.Onto)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if pr == nil {
		return delivery.RebaseResult{}, errs.Precondition(
			"não há PR aberto de %q para %q em %s/%s — nem o GitHub nem o GitLab "+
				"reaplicam um branch solto: os dois só reaplicam o branch de um PR/MR, "+
				"e sempre sobre o destino DELE",
			spec.Branch, spec.Onto, dono, nome)
	}

	const mut = `mutation($pr:ID!,$head:GitObjectID!){` +
		`updatePullRequestBranch(input:{pullRequestId:$pr,expectedHeadOid:$head,updateMethod:REBASE})` +
		`{pullRequest{id}}}`
	code, body, err := g.gql.do(ctx, http.MethodPost, "", map[string]any{
		"query": mut,
		// expectedHeadOid é concorrência otimista: se o branch andou entre a
		// leitura e a mutação, o GitHub recusa em vez de reaplicar sobre um
		// topo que não é o que a fila verificou.
		"variables": map[string]any{"pr": pr.NodeID, "head": pr.Head.SHA},
	})
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if code >= 300 {
		return delivery.RebaseResult{}, g.gql.fail(code, explicarGH(body), "a reaplicação do branch")
	}
	var resp struct {
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := g.gql.decode(body, &resp, "reaplicação do branch"); err != nil {
		return delivery.RebaseResult{}, err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		detalhe := g.gql.redact(strings.Join(msgs, "; "))
		// AQUI ESTÁ A APOSTA, e ela está escrita para quem vier depois: o
		// GitHub NÃO publica o texto do erro de rebase conflitado. Não há
		// campo estruturado que diga "conflito" — `type` vem genérico. Então a
		// classificação é por marcador no texto, e o default é ERRO, não
		// conflito: classificar errado para o lado do conflito mandaria um
		// humano resolver um problema de permissão na caixa de atenção, e o
		// erro pelo menos aparece no lugar certo com a mensagem inteira.
		if pareceConflito(detalhe) {
			return delivery.RebaseResult{
				BaseCommit: pr.Base.SHA,
				HeadCommit: pr.Head.SHA,
				Conflicted: true,
				// Files vazio: o GitHub não publica a lista (garantia 2).
				Detail: detalhe,
			}, nil
		}
		return delivery.RebaseResult{}, errs.Internal(
			"o GitHub recusou a reaplicação do branch %q: %s", spec.Branch, detalhe)
	}

	// Releitura do PR para os commits de topo e de base.
	//
	// Ressalva honesta: o GitHub NÃO documenta se a mutação termina antes de
	// responder. Se ela for assíncrona, este SHA pode ser o de ANTES. O que o
	// adaptador NÃO faz é fingir: ele devolve o que leu, e a fila re-verifica
	// sobre esse commit — que é a proteção da ADR-0008 §1 justamente contra
	// "achei que era outro estado do código".
	atual, err := g.prPorNumero(ctx, dono, nome, pr.Number)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	return delivery.RebaseResult{
		HeadCommit: atual.Head.SHA,
		BaseCommit: atual.Base.SHA,
	}, nil
}

// pareceConflito é compartilhado pelos dois adaptadores. Português e inglês
// porque a mensagem vem do provedor, não de nós.
func pareceConflito(s string) bool {
	l := strings.ToLower(s)
	for _, m := range []string{"conflict", "conflito", "rebase failed", "not mergeable", "cannot be merged"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// ── Merge ────────────────────────────────────────────────────────────────────

// Merge integra o PR.
//
// O trabalho de verdade está na RECUSA: o GitHub responde `405 {"message":"Pull
// Request is not mergeable"}` — a MESMA resposta — para PR já mergeado, PR com
// conflito, PR bloqueado por checagem obrigatória e PR em rascunho. Quatro fatos
// com consequências opostas atrás de um código só.
//
// Por isso a recusa provoca uma LEITURA do PR, e é ela que decide entre a
// garantia 6 (já mergeado ⇒ sucesso idempotente), a garantia 1 (conflito ⇒
// dado) e a garantia 8 (bloqueado ⇒ não mergeou e não é conflito).
func (g *GitHub) Merge(ctx context.Context, spec delivery.MergeSpec) (delivery.MergeResult, error) {
	if err := conferirAtor(g.actorID, spec.ActorID, "o merge"); err != nil {
		return delivery.MergeResult{}, err
	}
	dono, nome, err := partesRepo(spec.RepoExternalID)
	if err != nil {
		return delivery.MergeResult{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(spec.PRExternalID))
	if err != nil || n <= 0 {
		return delivery.MergeResult{}, errs.Invalid(
			"identificador de PR do GitHub deve ser o número do PR, veio %q", spec.PRExternalID)
	}

	code, body, err := g.rest.do(ctx, http.MethodPut, g.prPath(dono, nome, n)+"/merge",
		map[string]any{"merge_method": g.mergeMethod})
	if err != nil {
		return delivery.MergeResult{}, err
	}
	if code == http.StatusOK {
		var ok struct {
			SHA    string `json:"sha"`
			Merged bool   `json:"merged"`
		}
		if err := g.rest.decode(body, &ok, "merge de PR"); err != nil {
			return delivery.MergeResult{}, err
		}
		// Uma leitura a mais: a resposta do merge não traz o INSTANTE, e a
		// garantia 7 pede o commit E o quando. A fila da ADR-0008 registra os
		// dois no evento que alimenta o cockpit.
		pr, err := g.prPorNumero(ctx, dono, nome, n)
		if err != nil {
			return delivery.MergeResult{}, err
		}
		commit := ok.SHA
		if commit == "" {
			commit = pr.MergeCommitSHA
		}
		return delivery.MergeResult{
			Merged: ok.Merged || pr.Merged, MergeCommit: commit,
			MergedAtUnix: unixDe(instantePtr(pr.MergedAt)),
		}, nil
	}
	switch code {
	case http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity:
		return g.classificarRecusa(ctx, dono, nome, n, explicarGH(body))
	}
	return delivery.MergeResult{}, g.rest.fail(code, explicarGH(body),
		fmt.Sprintf("o merge do PR #%d em %s/%s", n, dono, nome))
}

// classificarRecusa desfaz a ambiguidade do 405.
func (g *GitHub) classificarRecusa(ctx context.Context, dono, nome string, n int, motivo string) (delivery.MergeResult, error) {
	pr, err := g.prPorNumero(ctx, dono, nome, n)
	if err != nil {
		return delivery.MergeResult{}, err
	}
	// `mergeable` é NULO enquanto o GitHub calcula. Nulo não é "sem conflito":
	// é "não sei", e garantia 9 diz que "não sei" nunca vira Conflicted=false.
	// Então esperamos a resposta, e desistir vira KindUnavailable.
	prazo := time.Now().Add(g.rebaseTimeout)
	for pr.Mergeable == nil && !pr.Merged {
		if time.Now().After(prazo) {
			return delivery.MergeResult{}, errs.New(errs.KindUnavailable,
				"o GitHub não terminou de calcular a mergeabilidade do PR #%d — "+
					"sem essa resposta não dá para distinguir conflito de bloqueio, e "+
					"chutar qualquer um dos dois manda a fila para o lado errado", n)
		}
		if err := esperar(ctx, g.poll, "espera pela mergeabilidade do PR"); err != nil {
			return delivery.MergeResult{}, err
		}
		if pr, err = g.prPorNumero(ctx, dono, nome, n); err != nil {
			return delivery.MergeResult{}, err
		}
	}
	if pr.Merged {
		// Garantia 6. A fila reprocessa a posição depois de uma queda e precisa
		// reconhecer o que já entrou: devolver erro aqui faria a entrada
		// mergeada voltar para a caixa de atenção como problema. Esta checagem
		// é ÚNICA de propósito — havia uma segunda, antes do laço, e a
		// duplicata fazia o experimento de quebra passar: apagar uma das duas
		// não mudava comportamento nenhum, e garantia que sobrevive a ser
		// apagada não está sendo verificada.
		return delivery.MergeResult{
			Merged: true, MergeCommit: pr.MergeCommitSHA,
			MergedAtUnix: unixDe(instantePtr(pr.MergedAt)),
			Detail:       "o PR já estava mergeado",
		}, nil
	}
	if pr.Mergeable != nil && !*pr.Mergeable {
		// Garantia 1: conflito é DADO.
		return delivery.MergeResult{
			Conflicted: true,
			Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
				"o GitHub recusou o merge por conteúdo que não integra (%s; mergeable_state=%q)",
				motivo, pr.MergeableState))),
		}, nil
	}
	// Garantia 8: não mergeou, não é conflito. Rascunho, checagem obrigatória
	// pendente, revisão faltando, regra de proteção do branch.
	return delivery.MergeResult{
		Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
			"o GitHub ainda não permite o merge (%s; mergeable_state=%q; rascunho=%v)",
			motivo, pr.MergeableState, pr.Draft))),
	}, nil
}

// ── HasNativeQueue ───────────────────────────────────────────────────────────

// HasNativeQueue diz se este repositório tem merge queue nativa (ADR-0008 §4).
//
// Divergência que a normalização teve de absorver: a merge queue do GitHub é
// POR BRANCH — ela é uma regra de ruleset que casa com um padrão de ref — e os
// merge trains do GitLab são POR PROJETO. A porta pergunta por REPOSITÓRIO, e a
// resposta honesta para o GitHub é sobre o branch que a fila da ADR-0008
// realmente disputa: o branch PADRÃO. Por isso as duas chamadas — descobrir o
// default e perguntar as regras dele.
//
// Garantia 14 em duas linhas: 403 vira ERRO. Sem permissão para ler as regras,
// o adaptador não SABE — e responder `false` seria afirmar "pode orquestrar por
// cima" sem ter olhado, com duas filas mergeando o mesmo repositório como
// prêmio.
func (g *GitHub) HasNativeQueue(ctx context.Context, repoExternalID string) (bool, error) {
	dono, nome, err := partesRepo(repoExternalID)
	if err != nil {
		return false, err
	}
	code, body, err := g.rest.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s", url.PathEscape(dono), url.PathEscape(nome)), nil)
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, g.rest.fail(code, explicarGH(body), fmt.Sprintf("a leitura do repositório %s/%s", dono, nome))
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.rest.decode(body, &repo, "leitura de repositório"); err != nil {
		return false, err
	}
	if repo.DefaultBranch == "" {
		return false, errs.New(errs.KindUnavailable,
			"o GitHub não informou o branch padrão de %s/%s — sem ele não há sobre "+
				"qual branch perguntar pela fila nativa", dono, nome)
	}

	// Esta rota resolve rulesets de repositório E de organização, já filtrados
	// pelos que estão em vigor. A alternativa (listar rulesets e abrir um a um)
	// devolve resumo sem as regras e obrigaria a casar os globos de ref na mão.
	code, body, err = g.rest.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/rules/branches/%s",
			url.PathEscape(dono), url.PathEscape(nome), url.PathEscape(repo.DefaultBranch)), nil)
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, g.rest.fail(code, explicarGH(body),
			fmt.Sprintf("a leitura das regras do branch %q de %s/%s", repo.DefaultBranch, dono, nome))
	}
	var regras []struct {
		Type string `json:"type"`
	}
	if err := g.rest.decode(body, &regras, "regras de branch"); err != nil {
		return false, err
	}
	for _, r := range regras {
		if r.Type == "merge_queue" {
			return true, nil
		}
	}
	return false, nil
}
