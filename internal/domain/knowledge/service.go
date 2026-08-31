package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/idem"
)

// Service concentra as regras de conhecimento. Recebe apenas PORTAS.
type Service struct {
	repo     Repository
	objects  ports.ObjectStore
	demands  Demands
	embedder Embedder
	clock    ports.Clock
	budget   Budget
}

// NewService exige repositório, ObjectStore, demandas e relógio; o Embedder é
// o único opcional.
//
// Panic aqui é deliberado — é erro de MONTAGEM, detectado no boot, não em
// produção às três da manhã. Vale em especial para o relógio: aceitar nil o
// faria cair em time.Now() por dentro, e a porta viraria enfeite.
//
// O Embedder é opcional porque a alternativa seria pior. Sem serviço de
// embedding ligado, exigir a porta deixaria o domínio inteiro fora do ar; com
// ela nula, a busca de memória cai no caminho LEXICAL (trigrama) e o restante
// funciona. A degradação é explícita, não silenciosa: quem monta o serviço
// escolhe, e SearchMemory diz por qual caminho respondeu.
//
// O orçamento é PARÂMETRO, e não constante escondida na montagem: o teto do
// pacote muda por projeto, por modelo e por decisão de custo. Budget zerado
// significa "use o padrão" (ver Budget.Normalize), nunca "não cabe nada".
func NewService(repo Repository, objects ports.ObjectStore, demands Demands,
	embedder Embedder, clock ports.Clock, budget Budget) *Service {
	if repo == nil {
		panic("knowledge.NewService: repositório obrigatório")
	}
	if objects == nil {
		panic("knowledge.NewService: ObjectStore obrigatório — artefato grande não cabe na linha")
	}
	if demands == nil {
		panic("knowledge.NewService: porta de demandas obrigatória — o pacote de contexto é POR demanda")
	}
	if clock == nil {
		panic("knowledge.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	return &Service{
		repo: repo, objects: objects, demands: demands,
		embedder: embedder, clock: clock, budget: budget.Normalize(),
	}
}

// memoryCandidates é quantas memórias a busca traz para a montagem CONSIDERAR.
// O corte final é do orçamento; trazer mais candidatas do que cabe é o que
// permite ao orçamento escolher entre elas por relevância em vez de aceitar o
// que a consulta devolveu.
const memoryCandidates = 24

// BuildContextPackage monta a bagagem de bordo do agente para uma demanda.
//
// É aqui que a economia de token acontece (ADR-0012): o pacote é SELECIONADO,
// não despejado. A composição vem da ADR-0009 §3 — regras + índice DOS
// REPOSITÓRIOS DA DEMANDA + memórias relevantes + achados já publicados — e o
// critério de corte está inteiro em SelectPackage, que é função pura.
//
// budget zerado usa o orçamento do serviço; passar um valor permite ao
// chamador apertar o teto sem remontar o serviço.
func (s *Service) BuildContextPackage(ctx context.Context, demandID string, budget Budget) (*Package, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demanda não informada")
	}
	if budget.Total <= 0 {
		budget = s.budget
	}

	dc, err := s.demands.ContextOf(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	if dc == nil {
		return nil, errs.NotFound("demanda")
	}

	rules, err := s.repo.RulesFor(ctx, accountID, dc.ProjectID)
	if err != nil {
		return nil, err
	}

	// Índice: só os repositórios que a demanda toca. O projeto pode ter
	// quarenta; a demanda toca dois. Esta linha É a disciplina "o pacote cresce
	// com a DEMANDA, não com o projeto".
	index, err := s.repo.IndexFor(ctx, accountID, dc.ProjectID, dc.Repos)
	if err != nil {
		return nil, err
	}

	// Relevância é medida contra o que a demanda PEDE — título e enunciado —,
	// não contra o projeto. Memória relevante para o projeto inteiro é a
	// memória inteira, e aí não há seleção nenhuma.
	memories, err := s.searchMemory(ctx, accountID, dc.ProjectID,
		strings.TrimSpace(dc.Title+"\n"+dc.Spec), memoryCandidates)
	if err != nil {
		return nil, err
	}

	pkg := SelectPackage(budget, Candidates{
		Rules:    rules,
		Findings: dc.Findings,
		Index:    index,
		Memories: memories,
	})
	pkg.DemandID = demandID

	// Medição da montagem como EVENTO (ADR-0009 §3): sem número, "o pacote
	// cresce com a demanda" é impressão. Dropped acompanha porque "coube" e
	// "coube porque jogamos fora metade da memória" são fatos diferentes.
	if err := s.repo.RecordContextBuild(ctx, accountID, demandID, PackageMetrics{
		At:              s.clock.Now(),
		EstimatedTokens: pkg.EstimatedTokens,
		Budget:          pkg.Budget,
		Rules:           len(pkg.Rules),
		Findings:        len(pkg.Findings),
		Index:           len(pkg.Index),
		Memories:        len(pkg.Memories),
		Dropped:         pkg.Dropped,
	}); err != nil {
		return nil, err
	}
	return &pkg, nil
}

const (
	searchLimitDefault = 10
	searchLimitMax     = 50
)

// SearchMemory é a consulta que o agente faz DURANTE a execução: o que não
// coube no pacote entra por aqui (ADR-0009 §3).
//
// projectID vazio busca a memória de escopo de conta — a que vale para todos
// os projetos. O inverso não existe: memória de um projeto nunca aparece na
// busca de outro, nem de outra conta.
func (s *Service) SearchMemory(ctx context.Context, projectID, query string, limit int) ([]ScoredArtifact, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, errs.Invalid("busca de memória sem consulta")
	}
	switch {
	case limit <= 0:
		limit = searchLimitDefault
	case limit > searchLimitMax:
		limit = searchLimitMax
	}
	return s.searchMemory(ctx, accountID, projectID, query, limit)
}

// searchMemory escolhe o caminho da busca. Semântico quando há Embedder;
// lexical quando não há — ver MemoryQuery.
func (s *Service) searchMemory(ctx context.Context, accountID, projectID, text string, limit int) ([]ScoredArtifact, error) {
	q := MemoryQuery{AccountID: accountID, ProjectID: projectID, Text: text, Limit: limit}
	if s.embedder != nil && strings.TrimSpace(text) != "" {
		vec, err := s.embedder.Embed(ctx, text)
		if err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao vetorizar a consulta")
		}
		// Dimensão errada não é detalhe: buscar com um embedder diferente do
		// que gerou os vetores devolve resultado PLAUSÍVEL e errado, que é o
		// pior modo de falha de uma busca. Melhor recusar.
		if len(vec) != EmbeddingDim {
			return nil, errs.Internal(
				"embedder devolveu %d dimensões; a memória foi indexada com %d", len(vec), EmbeddingDim)
		}
		q.Embedding = vec
	}
	return s.repo.SearchMemory(ctx, q)
}

// ReadIndex devolve o mapa de um repositório. Índice ausente é NotFound de
// propósito: o agente precisa saber que não há mapa — índice desatualizado é
// pior que índice ausente porque mente com confiança (spec §5, R-2), e índice
// silenciosamente vazio seria a mesma mentira em outra forma.
func (s *Service) ReadIndex(ctx context.Context, projectID, repo string) (*Artifact, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, errs.Invalid("projeto não informado")
	}
	if strings.TrimSpace(repo) == "" {
		return nil, errs.Invalid("repositório não informado")
	}
	a, err := s.repo.IndexOf(ctx, accountID, projectID, repo)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errs.NotFound("índice do repositório %q", repo)
	}
	return a, nil
}

// ListRules devolve as regras que VALEM para o projeto, com a herança da
// hierarquia já resolvida (conta → workspace → projeto).
func (s *Service) ListRules(ctx context.Context, projectID string) ([]string, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, errs.Invalid("projeto não informado")
	}
	rules, err := s.repo.RulesFor(ctx, accountID, projectID)
	if err != nil {
		return nil, err
	}
	return ResolveRules(rules), nil
}

// PutInput é uma escrita na base de conhecimento. WorkspaceID e ProjectID
// vazios significam escopo de CONTA — a regra que vale em todo lugar.
type PutInput struct {
	Kind           Kind
	WorkspaceID    string
	ProjectID      string
	Name           string
	Content        []byte
	Meta           map[string]any
	IdempotencyKey string
}

// PutArtifact grava conhecimento — é o lado da ESCRITA DE VOLTA do ciclo
// (ADR-0009 §4): o achado de hoje é o contexto da demanda de amanhã.
//
// A decisão que estrutura o método: onde o conteúdo mora.
//
//   - pequeno vai para o Postgres, porque é lá que ele é indexável (vetor e
//     trigrama) e legível sem uma segunda viagem;
//   - grande vai para o ObjectStore pela porta, e a LINHA GUARDA SÓ A
//     REFERÊNCIA. Guardar 4 MB de mapa numa coluna transformaria toda leitura
//     da tabela numa leitura de 4 MB.
//
// A ordem é storage primeiro, linha depois — mesma lógica da credencial: se a
// linha falhar, sobra um objeto órfão (inerte, e substituído na próxima
// tentativa pela chave derivada); a ordem inversa deixaria a linha afirmando
// ter conteúdo que não existe, e a falha apareceria longe daqui.
func (s *Service) PutArtifact(ctx context.Context, in PutInput) (*Artifact, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if !ValidKind(in.Kind) {
		return nil, errs.Invalid("tipo de conhecimento desconhecido: %q", in.Kind)
	}
	name := strings.TrimSpace(in.Name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if len(in.Content) == 0 {
		return nil, errs.Invalid("artefato de conhecimento sem conteúdo")
	}

	scope := Scope{
		Level:       ScopeAccount,
		AccountID:   accountID,
		WorkspaceID: strings.TrimSpace(in.WorkspaceID),
		ProjectID:   strings.TrimSpace(in.ProjectID),
	}
	switch {
	case scope.ProjectID != "":
		scope.Level = ScopeProject
	case scope.WorkspaceID != "":
		scope.Level = ScopeWorkspace
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}

	// Regra é texto que o agente lê INTEIRO, em todo pacote de todo projeto do
	// escopo. Se não cabe inline, não é regra — é documento, e documento é
	// memória ou índice.
	if in.Kind == KindRule && len(in.Content) > InlineMaxBytes {
		return nil, errs.Invalid(
			"regra excede %d bytes; conteúdo desse tamanho é memória ou índice, não regra", InlineMaxBytes)
	}

	a := &Artifact{
		Scope:     scope,
		Kind:      in.Kind,
		Name:      name,
		SizeBytes: len(in.Content),
		EstTokens: EstimateTokens(string(in.Content)),
		Meta:      in.Meta,
		CreatedBy: call.ActorID,
	}

	if len(in.Content) > InlineMaxBytes {
		ref := ObjectRefFor(scope, in.Kind, name)
		if err := s.objects.Put(ctx, ref, in.Content, ArtifactContentType); err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao guardar o conteúdo do artefato")
		}
		a.ObjectRef = ref.Bucket + "/" + ref.Key
	} else {
		a.Body = string(in.Content)
	}

	// Só memória é vetorizada: regra e índice são buscados por identidade
	// (nome do repositório, escopo), não por similaridade. Vetorizar os três
	// gastaria embedding para responder pergunta que ninguém faz.
	if in.Kind == KindMemory && s.embedder != nil {
		vec, err := s.embedder.Embed(ctx, name+"\n"+embedText(in.Content))
		if err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao vetorizar o artefato")
		}
		if len(vec) != EmbeddingDim {
			return nil, errs.Internal(
				"embedder devolveu %d dimensões; a coluna é vector(%d)", len(vec), EmbeddingDim)
		}
		a.Embedding = vec
	}

	// A assinatura da requisição cobre a IDENTIDADE e o CONTEÚDO: repetir a
	// chave com outro conteúdo é conflito, não repetição (ADR-0017).
	sum := sha256.Sum256(in.Content)
	return s.repo.Put(ctx, a, Idempotency{
		Key: strings.TrimSpace(in.IdempotencyKey),
		RequestHash: idem.Hash(accountID, string(scope.Level), scope.WorkspaceID, scope.ProjectID,
			string(in.Kind), name, hex.EncodeToString(sum[:])),
	})
}

// embedMaxBytes limita o texto enviado ao Embedder. Documento longo não é
// vetorizado inteiro por nenhum modelo útil, e mandar 4 MB para descobrir isso
// custa dinheiro e latência. O começo do documento é onde mora o resumo.
const embedMaxBytes = 8 * 1024

func embedText(content []byte) string {
	if len(content) > embedMaxBytes {
		return string(content[:embedMaxBytes])
	}
	return string(content)
}
