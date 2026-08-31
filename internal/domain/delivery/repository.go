package delivery

import (
	"context"
	"time"
)

// Repository é a PORTA de persistência do domínio de entrega.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
//
// Duas obrigações valem para TODA implementação, e não são negociáveis:
//
//   - toda leitura e toda escrita filtram por accountID — isolamento
//     multi-tenant é constraint, não confiança no chamador;
//   - toda escrita grava o estado novo e o evento na MESMA transação
//     (ADR-0019). Commit ⇒ os dois, ou nenhum.
type Repository interface {
	// ── evidência de verde (ADR-0007) ──

	// RecordVerification grava UMA execução. Repetir a mesma suíte no mesmo
	// commit ATUALIZA a linha e incrementa o contador de tentativas — o
	// histórico de quantas vezes se tentou é parte da evidência, não ruído.
	RecordVerification(ctx context.Context, run *VerificationRun, idemKey string) (*VerificationRun, error)

	// EvidenceFor devolve tudo o que se sabe sobre o verde de um commit. Nunca
	// devolve erro por ausência: evidência vazia é uma resposta legítima — e é
	// exatamente a que faz a fila recusar.
	EvidenceFor(ctx context.Context, accountID, demandID, repoID, commit string) (Evidence, error)

	// ── pull requests ──

	ListPullRequests(ctx context.Context, accountID string, f PRFilter) ([]PullRequest, error)
	PullRequestByID(ctx context.Context, accountID, id string) (*PullRequest, error)
	// PullRequestOf devolve (nil, nil) quando a demanda ainda não abriu PR no
	// repositório: ausência não é erro do adaptador, é decisão do domínio.
	PullRequestOf(ctx context.Context, accountID, demandID, repoID string) (*PullRequest, error)
	// OpenPullRequest grava o PR. A evidência vai junto porque o banco confere
	// a regra "sem verde, sem PR" por trigger — o serviço confere antes, com
	// mensagem útil; o banco confere sempre, inclusive nos caminhos que
	// ninguém previu.
	OpenPullRequest(ctx context.Context, pr *PullRequest, idemKey string) (*PullRequest, error)

	// ── fila de merge (ADR-0008) ──

	// QueueOfRepo devolve a fila do repositório. A ORDEM final é do domínio
	// (SortQueue); o adaptador devolve o que está gravado, incluindo Seq.
	QueueOfRepo(ctx context.Context, accountID, repoID string, includeMerged bool) ([]MergeQueueEntry, error)
	QueueEntryByID(ctx context.Context, accountID, id string) (*MergeQueueEntry, error)
	// Enqueue ATRIBUI a sequência do repositório, serializando as entradas
	// concorrentes. Devolver Seq preenchido é obrigação da implementação: sem
	// ela a ordem da fila é ambígua.
	Enqueue(ctx context.Context, e *MergeQueueEntry, idemKey string) (*MergeQueueEntry, error)
	// SetQueueState move a entrada. O relato de conflito é opcional em toda
	// transição menos a que vai para `conflict`.
	SetQueueState(ctx context.Context, accountID, entryID string, to QueueState, c *ConflictReport, idemKey string) (*MergeQueueEntry, error)

	// ── diretrizes (ADR-0015) ──

	ListDirectives(ctx context.Context, accountID, projectID string) ([]Directive, error)
	DirectiveByID(ctx context.Context, accountID, id string) (*Directive, error)
	CreateDirective(ctx context.Context, d *Directive, idemKey string) (*Directive, error)
	// DecideDirective grava a decisão E aplica, na MESMA transação, a
	// coordenação que este domínio sabe aplicar sozinho — hoje, a ordem
	// preferencial na fila de merge. As demais instruções viajam no evento
	// para quem é dono delas.
	DecideDirective(ctx context.Context, accountID, id string, dec Decision, ins []Instruction, idemKey string) (*Directive, error)
}

// PRFilter é o recorte de ListPullRequests. Campo vazio = sem filtro; a conta
// nunca é filtro opcional, vem à parte.
type PRFilter struct {
	DemandID  string
	ProjectID string
	RepoID    string
	OnlyOpen  bool
}

// Demands é a porta ESTREITA para o domínio da demanda — e é SOMENTE LEITURA.
//
// A superfície é o argumento inteiro: entrega precisa saber que a demanda
// existe na conta ativa e em que projeto ela vive. Só. Não há Pause, Block,
// Suspend nem Advance aqui, e a ausência é a regra de ouro da ADR-0015 §5
// escrita como tipo — este domínio não tem como parar demanda nenhuma, nem por
// engano, nem por um caminho que alguém acrescente distraído daqui a seis meses.
//
// Foi desenhada para que o serviço do domínio `demand` a satisfaça com um
// adaptador de cola mínimo no composition root; o pacote dele NÃO é importado
// aqui (está sendo escrito em paralelo, e domínio não depende de domínio irmão
// além do estritamente necessário).
type Demands interface {
	Demand(ctx context.Context, accountID, demandID string) (*DemandInfo, error)
}

// DemandInfo é o mínimo que a entrega enxerga de uma demanda.
type DemandInfo struct {
	ID        string
	ProjectID string
	// Active diz se a demanda ainda está andando. É informação de LEITURA:
	// serve para o evento contar a verdade ("a diretriz foi decidida e a
	// demanda 1 continua em andamento"), nunca para decidir se ela para.
	Active bool
}

// ─────────────────────────── GitProvider ───────────────────────────

// GitProvider é a porta do provedor de código (GitHub, GitLab).
//
// IMPLEMENTAÇÃO PENDENTE — declarada aqui para que o domínio já fale a língua
// de que precisa e para que a fila do DOP (ADR-0008 §4) saiba perguntar se o
// provedor tem fila nativa. Falar com GitHub/GitLab é adaptador, não domínio.
//
// A superfície foi mantida no mínimo do fluxo da ADR-0008: abrir o PR com o
// pacote de evidência, reaplicar sobre a base atual, mergear um de cada vez.
// Repare que Rebase devolve CONFLITO COMO DADO, não como erro: conflito é o
// caminho normal do fluxo — vira tarefa do agente e, se ele não resolver,
// item da caixa de atenção. Erro fica para o que é falha de infraestrutura.
type GitProvider interface {
	OpenPullRequest(ctx context.Context, spec OpenPRSpec) (ProviderPR, error)
	Rebase(ctx context.Context, spec RebaseSpec) (RebaseResult, error)
	Merge(ctx context.Context, spec MergeSpec) (MergeResult, error)
	// HasNativeQueue diz se o provedor tem merge queue própria (merge queue do
	// GitHub, merge trains do GitLab). A fila do DOP orquestra por cima e cobre
	// quem não tem.
	HasNativeQueue(ctx context.Context, repoExternalID string) (bool, error)
}

type OpenPRSpec struct {
	RepoExternalID string
	SourceBranch   string
	TargetBranch   string
	Title          string
	// Body é o pacote de evidência já renderizado (ADR-0007 §4): resultado da
	// aceitação, parecer do crítico, links do trace, quem pediu.
	Body string
	// Actor é a credencial de AUTORIA: quem conduziu assina os commits
	// (ADR-0003). A resolução da credencial é do adaptador; o domínio só diz
	// em nome de quem.
	ActorID string
}

type ProviderPR struct {
	ExternalID   string
	URL          string
	HeadCommit   string
	TargetBranch string
	CreatedAt    time.Time
}

type RebaseSpec struct {
	RepoExternalID string
	Branch         string
	Onto           string
}

// RebaseResult carrega o conflito como dado — ver o comentário de GitProvider.
type RebaseResult struct {
	HeadCommit string
	BaseCommit string
	Conflicted bool
	Files      []string
	Detail     string
}

type MergeSpec struct {
	RepoExternalID string
	PRExternalID   string
	ActorID        string
}

type MergeResult struct {
	Merged       bool
	MergeCommit  string
	Conflicted   bool
	Files        []string
	Detail       string
	MergedAtUnix int64
}
