package knowledge

import (
	"context"
	"time"
)

// Repository é a PORTA de persistência do domínio de conhecimento.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
//
// Toda operação recebe accountID explicitamente. Não é redundância com o
// Scope: é a leitura, onde não há escopo para carregar a conta. Conhecimento
// que vaza de uma conta para outra é o pior defeito concebível nesta
// plataforma — o filtro é parâmetro obrigatório da porta, nunca confiança no
// chamador.
type Repository interface {
	// Put grava o artefato e emite o evento na MESMA transação, honrando a
	// chave de idempotência: repetir a mesma escrita devolve o mesmo artefato
	// em vez de criar uma versão nova (ADR-0017/0019).
	Put(ctx context.Context, a *Artifact, idem Idempotency) (*Artifact, error)

	// IndexOf devolve o mapa de UM repositório do projeto. (nil, nil) quando
	// não existe: "índice ausente" é resposta legítima, e quem decide se isso
	// é erro é o caso de uso.
	IndexOf(ctx context.Context, accountID, projectID, repo string) (*Artifact, error)

	// IndexFor traz o índice DOS REPOSITÓRIOS PEDIDOS — os da demanda, não os
	// do projeto inteiro. Lista vazia devolve nada, e não "tudo": é a
	// diferença entre um pacote que cresce com a demanda e um que cresce com o
	// projeto.
	IndexFor(ctx context.Context, accountID, projectID string, repos []string) ([]Artifact, error)

	// RulesFor devolve as regras que ALCANÇAM o projeto: as dele, as do
	// workspace que o contém e as da conta. A resolução da herança é do
	// domínio (ResolveRules); a esta porta cabe apenas trazer as candidatas —
	// assim a regra de precedência existe em um lugar só.
	RulesFor(ctx context.Context, accountID, projectID string) ([]Artifact, error)

	// SearchMemory busca na camada de memória. Ver MemoryQuery.
	SearchMemory(ctx context.Context, q MemoryQuery) ([]ScoredArtifact, error)

	// RecordContextBuild registra a MEDIÇÃO da montagem como evento (ADR-0009
	// §3, ADR-0012 §1). Não é telemetria opcional: "o pacote cresce com a
	// demanda" precisa ter número, senão vira impressão — e o corte silencioso
	// é justamente o defeito que ninguém percebe.
	RecordContextBuild(ctx context.Context, accountID, demandID string, m PackageMetrics) error
}

// Idempotency é a chave da escrita e a assinatura do conteúdo que ela carrega.
//
// As duas viajam juntas porque a MESMA chave com corpo diferente é conflito,
// não repetição: sem o hash, um bug de cliente reaproveitando chave viraria
// corrupção silenciosa da base de conhecimento.
type Idempotency struct {
	Key         string
	RequestHash string
}

// MemoryQuery é uma busca na memória. Os dois caminhos convivem de propósito:
//
//   - com Embedding preenchido, a busca é SEMÂNTICA (pgvector): encontra a
//     lição sobre "timeout de conexão" quando a demanda fala em "queda
//     intermitente do banco";
//   - sem ele, a busca é LEXICAL (trigrama). Não é o caminho pretendido, é o
//     que sobra quando não há serviço de embedding ligado — e devolver nada
//     nesse caso seria pior: uma memória encontrada por palavra ainda é
//     melhor do que um agente que começa do zero.
type MemoryQuery struct {
	AccountID string
	ProjectID string
	Text      string
	Embedding []float32
	Limit     int
}

// PackageMetrics é o que a medição da montagem publica.
type PackageMetrics struct {
	// At vem do relógio do serviço (ports.Clock), não do banco: é o instante
	// da MONTAGEM, e é ele que torna a medição determinística em teste.
	At              time.Time
	EstimatedTokens int
	Budget          int
	Rules           int
	Findings        int
	Index           int
	Memories        int
	Dropped         Dropped
}

// ── portas estreitas para fora do domínio ────────────────────────────────────

// Embedder vetoriza texto para a busca semântica.
//
// É porta, e é ESTREITA de propósito: o domínio não sabe se do outro lado há
// um modelo hospedado, uma API de terceiro ou um serviço local, e não deve
// saber. Duas operações e nada além.
//
// Dimensions existe para que a incompatibilidade apareça no boot, não em
// silêncio: gravar vetor de 768 dimensões numa coluna vector(1536) é erro do
// banco, mas BUSCAR com um embedder diferente do que gerou os vetores devolve
// resultado plausível e errado — o pior modo de falha de uma busca.
//
// Não há adaptador para esta porta ainda; a fiação a satisfaz quando houver
// serviço de embedding. Até lá, o serviço aceita Embedder nulo e cai na busca
// lexical (ver MemoryQuery), o que está documentado em NewService.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimensions() int
}

// EmbeddingDim é a dimensão da coluna `vector` da migração 0007. Trocar de
// modelo de embedding implica migração da coluna E reindexação de toda a
// memória: os vetores antigos não são comparáveis com os novos.
const EmbeddingDim = 1536

// Demands é a porta ESTREITA para o domínio de demanda — a demanda é a unidade
// que o pacote de contexto serve, e o conhecimento precisa saber três coisas
// sobre ela: em qual projeto vive, quais repositórios toca e o que já foi
// concluído nela. Nada além disso.
//
// Declarada aqui, e não importada de lá, pelo mesmo motivo que resource.Access
// existe: o domínio de conhecimento não pode depender do formato interno de
// outro domínio, e a fiação liga as duas pontas.
type Demands interface {
	ContextOf(ctx context.Context, accountID, demandID string) (*DemandContext, error)
}

// DemandContext é o recorte da demanda que a montagem consome.
type DemandContext struct {
	DemandID  string
	ProjectID string
	Title     string
	// Spec é o enunciado da demanda. Alimenta a BUSCA por memórias relevantes
	// — a relevância é medida contra o que a demanda pede, não contra o
	// projeto inteiro.
	Spec string
	// Repos são os repositórios que a demanda toca. É o filtro que mantém o
	// índice proporcional à demanda (ADR-0009 §3).
	Repos []string
	// Findings são os achados já publicados na demanda — vazio no início,
	// povoado em retomada (ADR-0012 §3).
	Findings []Finding
}
