// Adaptador de delivery.GitProvider sobre o GitLab (API REST v4).
//
// ── O que é diferente do GitHub, e por quê importa ───────────────────────────
//
// O vocabulário diverge em quase tudo, e cada divergência custou uma decisão:
//
//   - PR × MR, `number` × `iid` (o iid é por PROJETO, não global — o `id` global
//     existe e NÃO serve nas rotas de MR);
//   - já existe PR para o branch: 422 no GitHub, 409 no GitLab, com corpos de
//     formatos diferentes — e o `message` do GitLab é ora string, ora LISTA de
//     strings, ora OBJETO de campo→lista;
//   - conflito: `mergeable:false` no GitHub, `detailed_merge_status:"conflict"`
//     no GitLab (com um `merge_status` legado que ainda vem junto);
//   - rebase: o GitHub SÓ tem por GraphQL; o GitLab tem rota REST própria, mas
//     ASSÍNCRONA, com resultado que se descobre por polling;
//   - fila nativa: merge queue do GitHub é POR BRANCH e sai de ruleset; merge
//     train do GitLab é POR PROJETO e sai de um campo do projeto — que some da
//     resposta quando o plano não tem o recurso.
//
// ── Onde o GitLab documenta e onde não ───────────────────────────────────────
//
// A recusa por rebase conflitado É documentada, palavra por palavra
// ("Rebase failed. Please rebase locally"), o que torna este lado mais firme que
// o do GitHub. Já a recusa por MR duplicado NÃO é documentada em lugar nenhum —
// e por isso, aqui também, o adaptador não casa texto: ele CONSULTA.
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

type GitLab struct {
	rest          *client
	actorID       string
	squash        bool
	rebaseTimeout time.Duration
	poll          time.Duration
}

type GitLabConfig struct {
	// APIBase inclui o /api/v4: "https://gitlab.com/api/v4" ou
	// "https://gitlab.interno/api/v4".
	APIBase string
	// Token é o valor JÁ RESOLVIDO da credencial de recurso (ADR-0013).
	Token   string
	ActorID string
	// MergeMethod é o vocabulário do fluxo git (ADR-0013). O GitLab não aceita
	// o método na chamada de merge — ele é configuração do PROJETO — mas aceita
	// `squash`, que é o único pedaço traduzível. Ver a conversão abaixo.
	MergeMethod   string
	Timeout       time.Duration
	RebaseTimeout time.Duration
	Poll          time.Duration
	Client        httpDoer
	// UseBearer troca o cabeçalho PRIVATE-TOKEN (token pessoal/de projeto) por
	// Authorization: Bearer (token de OAuth). Os dois são documentados; qual
	// usar depende do TIPO da credencial, e só quem a guardou sabe disso.
	UseBearer bool
}

func NewGitLab(cfg GitLabConfig) *GitLab {
	base := cfg.APIBase
	if base == "" {
		base = "https://gitlab.com/api/v4"
	}
	autorizar := func(r *http.Request) {
		if cfg.Token == "" {
			return
		}
		if cfg.UseBearer {
			r.Header.Set("Authorization", "Bearer "+cfg.Token)
			return
		}
		r.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	rt := cfg.RebaseTimeout
	if rt <= 0 {
		rt = DefaultRebaseTimeout
	}
	p := cfg.Poll
	if p <= 0 {
		p = defaultPoll
	}
	return &GitLab{
		rest:    newClient(base, "GitLab", cfg.Client, cfg.Timeout, autorizar, cfg.Token),
		actorID: cfg.ActorID,
		// Tradução HONESTA e incompleta do vocabulário de fluxo git: no GitLab
		// o método de merge é configuração do PROJETO (merge / rebase_merge /
		// ff), não parâmetro da chamada. O único pedaço que a chamada aceita é
		// `squash`. Forçar os três valores aqui seria fingir um controle que a
		// API não dá.
		squash:        cfg.MergeMethod == "squash",
		rebaseTimeout: rt,
		poll:          p,
	}
}

var _ delivery.GitProvider = (*GitLab)(nil)

func (g GitLab) String() string { return "gitprovider.GitLab{ator=" + g.actorID + "}" }

// ── formas do GitLab (só o que a porta usa) ──────────────────────────────────

type glMR struct {
	ID              int64  `json:"id"`
	IID             int64  `json:"iid"`
	Title           string `json:"title"`
	State           string `json:"state"` // opened | closed | locked | merged
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	WebURL          string `json:"web_url"`
	SHA             string `json:"sha"`
	MergeCommitSHA  string `json:"merge_commit_sha"`
	SquashCommitSHA string `json:"squash_commit_sha"`
	// MergeStatus é o campo LEGADO (depreciado no 15.6) e DetailedMergeStatus é
	// o que substitui. Lemos os dois: instalações self-hosted antigas — que são
	// o caso de uso do adaptador de cluster — ainda não têm o detalhado.
	MergeStatus         string  `json:"merge_status"`
	DetailedMergeStatus string  `json:"detailed_merge_status"`
	HasConflicts        bool    `json:"has_conflicts"`
	MergeError          *string `json:"merge_error"`
	RebaseInProgress    bool    `json:"rebase_in_progress"`
	CreatedAt           string  `json:"created_at"`
	MergedAt            *string `json:"merged_at"`
	DiffRefs            struct {
		BaseSHA string `json:"base_sha"`
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
}

// explicarGL achata o corpo de erro do GitLab.
//
// Três formatos diferentes na mesma API, todos vistos em produção:
//
//	{"message":"404 Project Not Found"}                       // string
//	{"message":["Another open merge request already exists…"]} // lista
//	{"message":{"source_branch":["does not exist"]}}           // objeto
//	{"error":"insufficient_scope"}                             // OAuth
func explicarGL(body []byte) string {
	var e struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	_ = jsonUnmarshalTolerante(body, &e)
	if s := strings.TrimSpace(textoDe(e.Message)); s != "" {
		return s
	}
	if s := strings.TrimSpace(e.Error); s != "" {
		return s
	}
	return strings.TrimSpace(string(body))
}

// projeto codifica o identificador para a rota. O GitLab aceita o id numérico
// OU o caminho, e o caminho precisa vir com a barra escapada ("grupo%2Fprojeto")
// — sem isso a rota vira outra rota e a resposta é um 404 que parece "não
// existe" quando o problema é a codificação.
func projeto(externo string) (string, error) {
	e := strings.Trim(strings.TrimSpace(externo), "/")
	if e == "" {
		return "", errs.Invalid("identificador de projeto do GitLab vazio")
	}
	return url.PathEscape(e), nil
}

func (g *GitLab) mrToPorta(mr glMR) delivery.ProviderPR {
	return delivery.ProviderPR{
		// ExternalID é OPACO (garantia 10): aqui é o `iid`, que é por projeto.
		// O `id` global do GitLab NÃO serve nas rotas de MR — usar o campo
		// errado dá 404 em um MR que existe.
		ExternalID:   strconv.FormatInt(mr.IID, 10),
		URL:          mr.WebURL,
		HeadCommit:   mr.SHA,
		TargetBranch: mr.TargetBranch,
		CreatedAt:    instante(mr.CreatedAt),
	}
}

// ── OpenPullRequest ──────────────────────────────────────────────────────────

// OpenPullRequest abre o MR, e é IDEMPOTENTE por (projeto, origem, destino).
//
// Mesma estratégia do GitHub e pelo mesmo motivo: a recusa por MR duplicado NÃO
// é documentada — nem o código, nem o texto. Então o adaptador não casa string:
// diante de 409 ou 422, ele PROCURA o MR aberto daquele par de branches. Achou,
// era duplicidade; não achou, era outra coisa e vira erro com o texto junto.
func (g *GitLab) OpenPullRequest(ctx context.Context, spec delivery.OpenPRSpec) (delivery.ProviderPR, error) {
	if err := conferirAtor(g.actorID, spec.ActorID, "a abertura do MR"); err != nil {
		return delivery.ProviderPR{}, err
	}
	proj, err := projeto(spec.RepoExternalID)
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	destino := spec.TargetBranch
	if destino == "" {
		destino = "main"
	}

	code, body, err := g.rest.do(ctx, http.MethodPost, "/projects/"+proj+"/merge_requests",
		map[string]any{
			"source_branch": spec.SourceBranch,
			"target_branch": destino,
			"title":         spec.Title,
			"description":   spec.Body,
		})
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	switch {
	case code == http.StatusCreated || code == http.StatusOK:
		var mr glMR
		if err := g.rest.decode(body, &mr, "abertura de MR"); err != nil {
			return delivery.ProviderPR{}, err
		}
		return g.mrToPorta(mr), nil

	case code == http.StatusConflict || code == http.StatusUnprocessableEntity || code == http.StatusBadRequest:
		existente, err := g.mrAberto(ctx, proj, spec.SourceBranch, destino)
		if err != nil {
			return delivery.ProviderPR{}, err
		}
		if existente != nil {
			// Garantia 4: sem PUT de título/descrição — o pacote de evidência
			// da ADR-0007 §4 não é substituível por um retry de rede.
			return g.mrToPorta(*existente), nil
		}
		return delivery.ProviderPR{}, errs.Invalid(
			"o GitLab recusou a abertura do MR de %q para %q: %s",
			spec.SourceBranch, destino, g.rest.redact(explicarGL(body)))
	}
	return delivery.ProviderPR{}, g.rest.fail(code, explicarGL(body),
		fmt.Sprintf("a abertura de MR em %s", spec.RepoExternalID))
}

// mrAberto devolve (nil, nil) quando não há MR aberto para o par de branches.
func (g *GitLab) mrAberto(ctx context.Context, proj, origem, destino string) (*glMR, error) {
	q := url.Values{}
	// Sem prefixo de dono, ao contrário da listagem do GitHub: aqui o filtro é
	// o nome puro do branch.
	q.Set("source_branch", origem)
	q.Set("target_branch", destino)
	q.Set("state", "opened")
	q.Set("per_page", "100")
	code, body, err := g.rest.do(ctx, http.MethodGet,
		"/projects/"+proj+"/merge_requests?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explicarGL(body), "a busca de MR aberto")
	}
	var mrs []glMR
	if err := g.rest.decode(body, &mrs, "listagem de MRs"); err != nil {
		return nil, err
	}
	for i := range mrs {
		if mrs[i].SourceBranch == origem && mrs[i].TargetBranch == destino {
			return &mrs[i], nil
		}
	}
	return nil, nil
}

// mrPorIID lê o MR. `rebase` liga o parâmetro que faz o GitLab informar se o
// Sidekiq ainda está reaplicando — sem ele o campo simplesmente não vem, e a
// ausência pareceria "terminou".
func (g *GitLab) mrPorIID(ctx context.Context, proj string, iid int64, rebase bool) (*glMR, error) {
	rota := fmt.Sprintf("/projects/%s/merge_requests/%d", proj, iid)
	if rebase {
		rota += "?include_rebase_in_progress=true"
	}
	code, body, err := g.rest.do(ctx, http.MethodGet, rota, nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explicarGL(body), fmt.Sprintf("a leitura do MR !%d", iid))
	}
	var mr glMR
	if err := g.rest.decode(body, &mr, "leitura de MR"); err != nil {
		return nil, err
	}
	return &mr, nil
}

// ── Rebase ───────────────────────────────────────────────────────────────────

// Rebase reaplica o branch do MR sobre o destino.
//
// A rota existe (ao contrário do GitHub, que só tem no GraphQL), mas é
// ASSÍNCRONA: o GitLab responde 202 {"rebase_in_progress":true} e vai fazer o
// trabalho num worker. A garantia 9 da porta diz que quem chama recebe o
// DESFECHO, então a espera é aqui — e o desfecho se lê pelo par
// (rebase_in_progress, merge_error) do MR.
//
// Este é o lado FIRME da normalização: o texto do conflito é documentado
// palavra por palavra ("Rebase failed. Please rebase locally"). Ainda assim a
// classificação passa por `pareceConflito`, e não por igualdade exata — o texto
// é documentado, não é congelado, e a única coisa pior que depender de uma
// string é depender dela com `==`.
func (g *GitLab) Rebase(ctx context.Context, spec delivery.RebaseSpec) (delivery.RebaseResult, error) {
	proj, err := projeto(spec.RepoExternalID)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.rebaseTimeout)
	defer cancel()

	mr, err := g.mrAberto(ctx, proj, spec.Branch, spec.Onto)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if mr == nil {
		return delivery.RebaseResult{}, errs.Precondition(
			"não há MR aberto de %q para %q em %s — nem o GitLab nem o GitHub "+
				"reaplicam um branch solto: os dois só reaplicam o branch de um MR/PR, "+
				"e sempre sobre o destino DELE",
			spec.Branch, spec.Onto, spec.RepoExternalID)
	}

	code, body, err := g.rest.do(ctx, http.MethodPut,
		fmt.Sprintf("/projects/%s/merge_requests/%d/rebase", proj, mr.IID), nil)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if code >= 300 {
		// 403 do GitLab aqui é documentado com três textos, e todos são falha
		// de PERMISSÃO ou de branch ausente — nenhum é conflito. Cair no `fail`
		// comum é o certo: vira KindPermission e não polui a caixa de atenção.
		return delivery.RebaseResult{}, g.rest.fail(code, explicarGL(body),
			fmt.Sprintf("a reaplicação do branch %q", spec.Branch))
	}

	for {
		atual, err := g.mrPorIID(ctx, proj, mr.IID, true)
		if err != nil {
			return delivery.RebaseResult{}, err
		}
		if !atual.RebaseInProgress {
			detalhe := strings.TrimSpace(g.rest.redact(deref(atual.MergeError)))
			if detalhe != "" {
				// Garantia 1 e 2: conflito é DADO, com Detail. Files vazio —
				// o GitLab NÃO publica a lista de arquivos em conflito em rota
				// nenhuma da API v4; o que a interface usa é uma rota interna
				// do Rails, fora do /api/v4, sem versão e sem promessa.
				if pareceConflito(detalhe) {
					return delivery.RebaseResult{
						HeadCommit: atual.SHA,
						BaseCommit: atual.DiffRefs.BaseSHA,
						Conflicted: true,
						Detail:     detalhe,
					}, nil
				}
				return delivery.RebaseResult{}, errs.Internal(
					"o GitLab não conseguiu reaplicar o branch %q: %s", spec.Branch, detalhe)
			}
			return delivery.RebaseResult{
				HeadCommit: atual.SHA,
				BaseCommit: atual.DiffRefs.BaseSHA,
			}, nil
		}
		// Contexto cancelado ou prazo estourado sai como KindUnavailable —
		// nunca como Conflicted=false, que diria "não conflitou" no lugar de
		// "não sei" (garantia 9).
		if err := esperar(ctx, g.poll, "espera pela reaplicação do branch"); err != nil {
			return delivery.RebaseResult{}, err
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── Merge ────────────────────────────────────────────────────────────────────

// Merge integra o MR.
//
// Mesma ambiguidade do GitHub, com números diferentes: `405 Method Not Allowed`
// ("The merge request cannot merge") e `422 Branch cannot be merged` cobrem já
// mergeado, conflito e bloqueio. Como lá, a recusa provoca uma LEITURA do MR, e
// é ela que separa a garantia 6 da garantia 1 da garantia 8.
func (g *GitLab) Merge(ctx context.Context, spec delivery.MergeSpec) (delivery.MergeResult, error) {
	if err := conferirAtor(g.actorID, spec.ActorID, "o merge"); err != nil {
		return delivery.MergeResult{}, err
	}
	proj, err := projeto(spec.RepoExternalID)
	if err != nil {
		return delivery.MergeResult{}, err
	}
	iid, err := strconv.ParseInt(strings.TrimSpace(spec.PRExternalID), 10, 64)
	if err != nil || iid <= 0 {
		return delivery.MergeResult{}, errs.Invalid(
			"identificador de MR do GitLab deve ser o iid do MR, veio %q", spec.PRExternalID)
	}

	code, body, err := g.rest.do(ctx, http.MethodPut,
		fmt.Sprintf("/projects/%s/merge_requests/%d/merge", proj, iid),
		map[string]any{"squash": g.squash})
	if err != nil {
		return delivery.MergeResult{}, err
	}
	if code == http.StatusOK {
		var mr glMR
		if err := g.rest.decode(body, &mr, "merge de MR"); err != nil {
			return delivery.MergeResult{}, err
		}
		return g.resultadoMerge(mr), nil
	}
	switch code {
	case http.StatusMethodNotAllowed, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusNotAcceptable:
		return g.classificarRecusaGL(ctx, proj, iid, explicarGL(body))
	}
	return delivery.MergeResult{}, g.rest.fail(code, explicarGL(body),
		fmt.Sprintf("o merge do MR !%d em %s", iid, spec.RepoExternalID))
}

// resultadoMerge traduz um MR mergeado. Garantia 7: Merged=true exige commit.
//
// Repare no `squash_commit_sha`: com squash ligado, o commit que entrou na base
// é ELE, e `merge_commit_sha` pode vir vazio. Ler só o segundo devolveria
// Merged=true com commit vazio — a garantia 7 quebrada em silêncio, e a fila da
// ADR-0008 registrando um merge sem dizer o que entrou.
func (g *GitLab) resultadoMerge(mr glMR) delivery.MergeResult {
	commit := mr.MergeCommitSHA
	if commit == "" {
		commit = mr.SquashCommitSHA
	}
	return delivery.MergeResult{
		Merged:       mr.State == "merged",
		MergeCommit:  commit,
		MergedAtUnix: unixDe(instantePtr(mr.MergedAt)),
	}
}

func (g *GitLab) classificarRecusaGL(ctx context.Context, proj string, iid int64, motivo string) (delivery.MergeResult, error) {
	prazo := time.Now().Add(g.rebaseTimeout)
	for {
		mr, err := g.mrPorIID(ctx, proj, iid, false)
		if err != nil {
			return delivery.MergeResult{}, err
		}
		if mr.State == "merged" {
			// Garantia 6.
			r := g.resultadoMerge(*mr)
			r.Merged = true
			r.Detail = "o MR já estava mergeado"
			return r, nil
		}
		if conflitoGL(*mr) {
			// Garantia 1.
			return delivery.MergeResult{
				Conflicted: true,
				Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
					"o GitLab recusou o merge por conteúdo que não integra (%s; "+
						"detailed_merge_status=%q; merge_status=%q)",
					motivo, mr.DetailedMergeStatus, mr.MergeStatus))),
			}, nil
		}
		// `unchecked`, `checking` e `preparing` são o "não sei" do GitLab: ele
		// calcula a mergeabilidade de forma ASSÍNCRONA, e é a própria
		// documentação que manda voltar a perguntar. Responder agora seria
		// afirmar "não é conflito" sem que ninguém tenha olhado.
		if !calculando(*mr) {
			// Garantia 8: não mergeou, não é conflito, e diz qual é o motivo —
			// com o vocabulário do GitLab dentro do Detail, que é TEXTO, e não
			// promovido a campo da porta (garantia 10).
			return delivery.MergeResult{
				Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
					"o GitLab ainda não permite o merge (%s; detailed_merge_status=%q; merge_status=%q)",
					motivo, mr.DetailedMergeStatus, mr.MergeStatus))),
			}, nil
		}
		if time.Now().After(prazo) {
			return delivery.MergeResult{}, errs.New(errs.KindUnavailable,
				"o GitLab não terminou de calcular a mergeabilidade do MR !%d "+
					"(detailed_merge_status=%q) — sem essa resposta não dá para distinguir "+
					"conflito de bloqueio, e chutar manda a fila para o lado errado",
				iid, mr.DetailedMergeStatus)
		}
		if err := esperar(ctx, g.poll, "espera pela mergeabilidade do MR"); err != nil {
			return delivery.MergeResult{}, err
		}
	}
}

// conflitoGL lê os TRÊS sinais, porque eles não são redundantes:
//
//   - `detailed_merge_status == "conflict"` é o campo atual e o único que
//     nomeia o conflito sem ambiguidade;
//   - `merge_status == "cannot_be_merged"` é o legado, e é o único que existe em
//     instalações self-hosted anteriores ao 15.6 — que são exatamente o público
//     do modo self-hosted do DOP;
//   - `has_conflicts` é derivado do legado (o próprio GitLab documenta que ele
//     é `false` a menos que `merge_status` seja `cannot_be_merged`), então
//     sozinho ele não acrescenta — mas confirma.
func conflitoGL(mr glMR) bool {
	return mr.DetailedMergeStatus == "conflict" ||
		mr.MergeStatus == "cannot_be_merged" ||
		mr.HasConflicts
}

// calculando cobre os estados em que o GitLab ainda não sabe responder.
func calculando(mr glMR) bool {
	switch mr.DetailedMergeStatus {
	case "checking", "unchecked", "preparing", "approvals_syncing":
		return true
	}
	switch mr.MergeStatus {
	case "checking", "unchecked", "cannot_be_merged_recheck":
		return true
	}
	return false
}

// ── HasNativeQueue ───────────────────────────────────────────────────────────

// HasNativeQueue diz se este projeto tem merge train nativo (ADR-0008 §4).
//
// A fonte é `merge_trains_enabled` do próprio projeto, e não a API de merge
// trains: aquela exige papel de Developer para mais, é de plano pago, e o que
// ela devolve quando o recurso não existe NÃO é documentado — 403, 404 e lista
// vazia são todos plausíveis, e ramificar em cima disso é adivinhação.
//
// A sutileza que decide a garantia 14 está no TRI-ESTADO do campo. Ele é
// exposto por código de Enterprise Edition sob condição de licença: numa
// instalação sem o recurso ele NÃO VEM `false` — ele NÃO VEM. Por isso o
// adaptador distingue ausente de false com ponteiro:
//
//   - presente e true  → tem fila nativa; a do DOP sai da frente;
//   - presente e false → não tem; a do DOP orquestra (ADR-0008 §4);
//   - AUSENTE          → a instalação não oferece merge train nenhum, então
//     também não há o que duplicar: `false`. Esta é a única inferência do
//     adaptador, e ela é conservadora — erra para o lado de MANTER a fila do
//     DOP, que é o lado que serializa os merges em vez de deixá-los soltos.
//
// O caso "não consigo olhar" continua sendo ERRO: 401, 403 e 404 sobem, porque
// aí não se sabe nem se o projeto existe.
func (g *GitLab) HasNativeQueue(ctx context.Context, repoExternalID string) (bool, error) {
	proj, err := projeto(repoExternalID)
	if err != nil {
		return false, err
	}
	code, body, err := g.rest.do(ctx, http.MethodGet, "/projects/"+proj, nil)
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, g.rest.fail(code, explicarGL(body),
			fmt.Sprintf("a leitura do projeto %s", repoExternalID))
	}
	var p struct {
		MergeTrainsEnabled *bool `json:"merge_trains_enabled"`
	}
	if err := g.rest.decode(body, &p, "leitura de projeto"); err != nil {
		return false, err
	}
	if p.MergeTrainsEnabled == nil {
		return false, nil
	}
	return *p.MergeTrainsEnabled, nil
}
