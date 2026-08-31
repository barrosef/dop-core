// Package knowledge é a base de conhecimento do projeto — regras, índice e
// memória — e a montagem do pacote de contexto por demanda (ADR-0009).
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK nenhum. Ele
// declara o que precisa como PORTA (repository.go) e o composition root liga.
//
// A ideia que organiza o arquivo inteiro: contexto NÃO é "juntar arquivos e
// mandar para o modelo". O pacote é SELECIONADO — cresce com a demanda, não
// com o projeto — e a seleção é uma função pura, testável sem banco, porque é
// ela que decide se o agente nasce sabendo ou nasce escavando.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── as três camadas (ADR-0009 §1) ────────────────────────────────────────────

type Kind string

const (
	// KindRule é convenção que o agente OBEDECE ("nunca mergear desenv na
	// feature"). Regra que não entrou no pacote é regra violada.
	KindRule Kind = "rule"
	// KindIndex é o mapa de UM repositório: o que vive onde, como buildar,
	// como testar. Sem ele, cada demanda gasta os primeiros 30 minutos
	// redescobrindo o repositório.
	KindIndex Kind = "index"
	// KindMemory é achado, lição e análise forense de demandas passadas.
	// É a camada que mais cresce e a que mais vira ruído sem curadoria (R-1).
	KindMemory Kind = "memory"
)

func ValidKind(k Kind) bool {
	switch k {
	case KindRule, KindIndex, KindMemory:
		return true
	}
	return false
}

// ── escopo e herança ─────────────────────────────────────────────────────────

// ScopeLevel é onde o artefato vive na hierarquia. A herança é o motivo de o
// nível existir: uma regra da conta vale para todo projeto dela, e um projeto
// pode substituí-la por uma regra de mesmo nome — sem copiar a regra em todo
// lugar, que é como bases de conhecimento apodrecem.
type ScopeLevel string

const (
	ScopeAccount   ScopeLevel = "account"
	ScopeWorkspace ScopeLevel = "workspace"
	ScopeProject   ScopeLevel = "project"
)

// Specificity ordena a herança: o mais específico ganha do mais geral.
func (l ScopeLevel) Specificity() int {
	switch l {
	case ScopeProject:
		return 2
	case ScopeWorkspace:
		return 1
	case ScopeAccount:
		return 0
	}
	return -1
}

// Scope amarra o artefato à conta SEMPRE, e ao workspace ou projeto quando o
// nível pede. AccountID nunca é opcional: conhecimento vazado entre contas é o
// pior defeito possível nesta plataforma, então a conta é campo do escopo, não
// parâmetro que o adaptador possa esquecer.
type Scope struct {
	Level       ScopeLevel
	AccountID   string
	WorkspaceID string
	ProjectID   string
}

func AccountScope(accountID string) Scope {
	return Scope{Level: ScopeAccount, AccountID: accountID}
}

func WorkspaceScope(accountID, workspaceID string) Scope {
	return Scope{Level: ScopeWorkspace, AccountID: accountID, WorkspaceID: workspaceID}
}

func ProjectScope(accountID, projectID string) Scope {
	return Scope{Level: ScopeProject, AccountID: accountID, ProjectID: projectID}
}

// Validate recusa escopo incoerente na ESCRITA. O banco repete a checagem por
// CHECK constraint; aqui a mensagem é útil, lá é o último anteparo.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.AccountID) == "" {
		return errs.Invalid("artefato de conhecimento sem conta")
	}
	switch s.Level {
	case ScopeAccount:
		if s.WorkspaceID != "" || s.ProjectID != "" {
			return errs.Invalid("escopo de conta não aponta para workspace nem projeto")
		}
	case ScopeWorkspace:
		if s.WorkspaceID == "" || s.ProjectID != "" {
			return errs.Invalid("escopo de workspace exige workspace e nenhum projeto")
		}
	case ScopeProject:
		if s.ProjectID == "" || s.WorkspaceID != "" {
			return errs.Invalid("escopo de projeto exige projeto e nenhum workspace")
		}
	default:
		return errs.Invalid("escopo desconhecido: %q", s.Level)
	}
	return nil
}

// ── o artefato ───────────────────────────────────────────────────────────────

// Artifact é uma peça de conhecimento versionada.
//
// O conteúdo mora em UM de dois lugares, nunca nos dois:
//
//   - Body, no Postgres, quando é pequeno — porque o que é pequeno precisa ser
//     indexável (trigrama e vetor) e lido sem uma segunda viagem de rede;
//   - ObjectRef, no ObjectStore, quando é grande — porque um mapa de
//     repositório de 4 MB dentro de uma linha transforma toda leitura da tabela
//     numa leitura de 4 MB, e o Postgres não é object store (ADR-0009 §2).
type Artifact struct {
	ID        string
	Scope     Scope
	Kind      Kind
	Name      string // rule: título; index: NOME DO REPO; memory: título do achado
	Version   int32
	Body      string
	ObjectRef string
	SizeBytes int
	EstTokens int
	Embedding []float32 // só memória tem; vazio quando não há Embedder ligado
	Meta      map[string]any
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AccountID é usado o tempo todo — nas queries, nos eventos e na conversão de
// borda. Vale o atalho.
func (a Artifact) AccountID() string { return a.Scope.AccountID }

// Externalized diz se o conteúdo está no ObjectStore. Quem lê um artefato
// externalizado recebe a REFERÊNCIA, não os bytes: o sandbox busca no storage,
// somente leitura, sem passar o binário pelo core.
func (a Artifact) Externalized() bool { return a.ObjectRef != "" }

// InlineMaxBytes é a fronteira entre "cabe na linha" e "vai para o storage".
//
// 16 KiB não é número mágico de sorte: é a ordem de grandeza de um documento
// que um agente lê inteiro sem estourar orçamento (≈4k tokens), e é pequeno o
// bastante para o Postgres guardar em linha sem TOAST na maioria dos casos.
const InlineMaxBytes = 16 * 1024

// ArtifactContentType é o Content-Type usado ao gravar no ObjectStore.
//
// Cuidado documentado, e o motivo de NÃO ser application/json: o emulador de
// Storage do ambiente local PENDURA — a conexão fica aberta até o timeout do
// cliente — em upload com esse tipo exato (dop-infra/docs/ambiente-local.md,
// pendência P-13 do ROADMAP). "text/plain; charset=utf-8" responde 200 no
// emulador e no GCS de verdade, e o conteúdo de conhecimento é texto (markdown,
// JSON de índice) de qualquer forma. Trocar isto por application/json trava o
// ambiente local sem nenhuma mensagem de erro — o pior tipo de regressão.
const ArtifactContentType = "text/plain; charset=utf-8"

// KnowledgeBucket concentra os artefatos de conhecimento. A chave é OPACA e
// PLANA por contrato da porta (ports.ObjectStore); o prefixo com a conta serve
// para operação e auditoria, não para navegação — o isolamento real vem da
// query, que sempre filtra por conta antes de devolver qualquer referência.
const KnowledgeBucket = "dop-knowledge"

// ObjectRefFor deriva a referência do conteúdo a partir da IDENTIDADE do
// artefato (conta, escopo, tipo, nome) — nunca do id da linha.
//
// Duas consequências, ambas desejadas: o conteúdo pode ser gravado ANTES de a
// linha existir (o id é do banco, e gravar o objeto primeiro é o que evita
// linha apontando para objeto inexistente); e uma versão nova SUBSTITUI o
// objeto atomicamente, que é a garantia 2 da porta. O histórico de conteúdo
// não é promessa desta camada: a versão é o número na linha.
func ObjectRefFor(s Scope, kind Kind, name string) ports.ObjectRef {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{string(s.Level), s.AccountID, s.WorkspaceID, s.ProjectID, string(kind), name}, "|")))
	return ports.ObjectRef{
		Bucket: KnowledgeBucket,
		Key:    s.AccountID + "/" + hex.EncodeToString(sum[:16]),
	}
}

const nameMaxLen = 200

func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errs.Invalid("o artefato de conhecimento precisa de um nome")
	}
	if len(name) > nameMaxLen {
		return errs.Invalid("o nome do artefato pode ter no máximo %d caracteres", nameMaxLen)
	}
	return nil
}

// EstimateTokens estima o custo do texto em tokens.
//
// A medição REAL do pacote é por token counting na borda (endpoint gratuito,
// ADR-0012) — mas o CORTE precisa acontecer aqui, offline e determinístico:
// uma seleção que dependesse de chamada de rede seria não-determinística, e
// pacote não-determinístico invalida o prefixo cacheado do prompt, que é de
// onde vem 90% do desconto. Quatro bytes por token é a aproximação usual para
// texto latino; erra por pouco e erra SEMPRE do mesmo jeito, que é o que
// importa para o cache.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// perItemOverhead cobre o cabeçalho que a serialização do pacote acrescenta a
// cada item (nome, delimitador, rótulo da camada). Sem contá-lo, o orçamento
// vaza um pouco por item — e "um pouco por item" numa memória longa é o
// suficiente para estourar a janela.
const perItemOverhead = 8

// TokenCost é o que o artefato custa DENTRO do pacote. Artefato externalizado
// custa só o cabeçalho: o pacote leva a referência, não os bytes.
func (a Artifact) TokenCost() int {
	cost := EstimateTokens(a.Name) + perItemOverhead
	if a.Externalized() {
		return cost
	}
	if a.EstTokens > 0 {
		return cost + a.EstTokens
	}
	return cost + EstimateTokens(a.Body)
}

// ScoredArtifact é o artefato com a relevância que a busca lhe atribuiu.
// Score é comparável dentro de UMA busca, não entre buscas.
type ScoredArtifact struct {
	Artifact Artifact
	Score    float32
}

// Finding é a conclusão publicada por um subagente na demanda (ADR-0010).
// Vive no domínio de demanda; aqui entra pela porta Demands, com a superfície
// mínima que a montagem do pacote consome.
type Finding struct {
	ID       string
	ThreadID string
	Title    string
	Summary  string
}

func (f Finding) TokenCost() int {
	return EstimateTokens(f.Title) + EstimateTokens(f.Summary) + perItemOverhead
}

// ── herança de regras ────────────────────────────────────────────────────────

// ResolveRules aplica a herança da hierarquia sobre as regras de um escopo.
//
// Duas decisões, ambas visíveis no resultado:
//
//  1. o mais específico GANHA: uma regra de projeto com o mesmo nome de uma
//     regra da conta substitui a da conta. É o que permite "a política de teste
//     da casa, exceto neste projeto legado" sem duplicar a política;
//  2. a lista sai do mais específico para o mais geral. Isso não é estética: é
//     o que faz o corte por orçamento sacrificar primeiro a regra genérica —
//     a que o projeto tem menos motivo para depender.
//
// Regra externalizada NÃO entra: regra é texto que o agente lê inteiro, e
// PutArtifact recusa regra que não caiba inline exatamente por isto.
func ResolveRules(rules []Artifact) []string {
	ordered := make([]Artifact, len(rules))
	copy(ordered, rules)
	sort.SliceStable(ordered, func(i, j int) bool {
		si, sj := ordered[i].Scope.Level.Specificity(), ordered[j].Scope.Level.Specificity()
		if si != sj {
			return si > sj
		}
		// Desempate por nome: a ordem do pacote precisa ser estável entre
		// execuções, senão o prefixo cacheado do prompt muda sem motivo.
		return ordered[i].Name < ordered[j].Name
	})

	seen := make(map[string]bool, len(ordered))
	out := make([]string, 0, len(ordered))
	for _, r := range ordered {
		if r.Kind != KindRule || seen[r.Name] || strings.TrimSpace(r.Body) == "" {
			continue
		}
		seen[r.Name] = true
		out = append(out, r.Body)
	}
	return out
}

// ── orçamento e seleção do pacote (ADR-0012) ─────────────────────────────────

// Budget é o orçamento do pacote, em tokens.
//
// É PARÂMETRO, não constante enterrada: o teto muda por projeto, por modelo e
// por decisão de custo, e um número escondido no meio da montagem seria
// impossível de ajustar sem recompilar. As frações limitam CADA camada, para
// que a memória — a única que cresce sem fim — não coma o pacote inteiro (R-1).
type Budget struct {
	Total        int     // teto do pacote inteiro
	FindingShare float64 // fração do total reservada aos achados da demanda
	IndexShare   float64 // ...ao índice dos repositórios da demanda
	MemoryShare  float64 // ...às memórias relevantes
}

// DefaultBudget é o ponto de partida — e nada além disso. Quem monta o
// serviço escolhe o teto; este valor existe para que "não escolhi" não
// signifique "sem teto".
func DefaultBudget() Budget {
	return Budget{Total: 24000, FindingShare: 0.20, IndexShare: 0.35, MemoryShare: 0.30}
}

// Normalize preenche o que veio zerado. Orçamento zero é "use o padrão", nunca
// "não cabe nada": um pacote vazio por engano de configuração seria um agente
// que nasce cego, e isso precisa ser uma decisão explícita, não um default.
func (b Budget) Normalize() Budget {
	d := DefaultBudget()
	if b.Total <= 0 {
		b.Total = d.Total
	}
	if b.FindingShare <= 0 {
		b.FindingShare = d.FindingShare
	}
	if b.IndexShare <= 0 {
		b.IndexShare = d.IndexShare
	}
	if b.MemoryShare <= 0 {
		b.MemoryShare = d.MemoryShare
	}
	return b
}

func share(total int, frac float64) int { return int(float64(total) * frac) }

// Dropped conta o que FICOU DE FORA por camada.
//
// Existe para ser emitido junto com a medição: "o pacote coube" e "o pacote
// coube porque jogamos fora metade da memória relevante" são fatos diferentes,
// e só o segundo explica um agente que não sabia o que devia saber.
type Dropped struct {
	Rules    int
	Findings int
	Index    int
	Memories int
}

func (d Dropped) Any() bool { return d.Rules+d.Findings+d.Index+d.Memories > 0 }

// Candidates é tudo que PODERIA entrar no pacote, já filtrado por conta e por
// demanda pelas consultas. A seleção decide o que de fato entra.
type Candidates struct {
	Rules    []Artifact
	Findings []Finding
	Index    []Artifact
	Memories []ScoredArtifact
}

// Package é a bagagem de bordo do agente (ADR-0009 §3).
//
// Sem timestamp e sem id volátil de propósito: o pacote entra no PREFIXO
// cacheado do prompt, e um byte que muda a cada montagem queima o desconto de
// cache em silêncio (ADR-0012 §1).
type Package struct {
	DemandID        string
	Rules           []string
	Findings        []Finding
	Index           []Artifact
	Memories        []Artifact
	EstimatedTokens int
	Budget          int
	Dropped         Dropped
}

// Truncated diz se a curadoria precisou cortar. É o gatilho do alerta: teto
// batendo com frequência significa demanda grande demais ou memória mal podada.
func (p Package) Truncated() bool { return p.Dropped.Any() }

// SelectPackage monta o pacote dentro do orçamento. É O CORAÇÃO DESTE DOMÍNIO,
// e é função pura para poder ser testada sem banco, sem rede e sem modelo.
//
// O que entra, em qual ordem e por qual critério:
//
//  1. REGRAS, já resolvidas pela herança (mais específica primeiro). Entram
//     antes de tudo porque regra ignorada é retrabalho garantido: o agente
//     abre PR contra a branch errada e o custo é um ciclo inteiro de revisão.
//  2. ACHADOS já publicados na demanda. Em retomada, é o que impede o agente
//     de refazer investigação que um irmão já concluiu (ADR-0010/0012 §3).
//  3. ÍNDICE DOS REPOSITÓRIOS DA DEMANDA — não do projeto inteiro. É aqui que
//     mora a disciplina "cresce com a DEMANDA": o projeto pode ter 40 repos, a
//     demanda toca dois. Quem selecionou os dois foi a consulta; esta função
//     apenas respeita o orçamento.
//  4. MEMÓRIAS, por relevância decrescente. Ficam por último porque são a
//     camada mais volumosa e a de menor precisão: é o primeiro lugar onde
//     cortar dói pouco, e o agente pode pedir mais em execução (SearchMemory).
//
// O que fica de fora, e por quê:
//
//   - o que não couber no teto da própria camada — a memória não invade a
//     cota do índice mesmo quando há espaço sobrando;
//   - dentro de uma camada, TUDO a partir do primeiro item que não coube. Não
//     pulamos o item grande para encaixar o próximo menor: isso trocaria a
//     ordem de relevância por uma heurística de empacotamento, e devolveria um
//     pacote onde a 7ª memória entrou e a 3ª não. Curadoria não é mochila.
func SelectPackage(b Budget, in Candidates) Package {
	b = b.Normalize()
	p := Package{Budget: b.Total}

	restante := b.Total
	// gastar tenta pagar `custo` respeitando o teto da camada e o do pacote.
	gastar := func(custo int, tetoCamada *int) bool {
		if custo > restante || custo > *tetoCamada {
			return false
		}
		restante -= custo
		*tetoCamada -= custo
		return true
	}

	// Regras não têm cota própria: o teto delas é o pacote inteiro. Uma base de
	// regras que sozinha estoura o orçamento é um problema de curadoria de
	// regras, e o Dropped o denuncia — mas cortar regra para caber memória
	// seria a troca errada.
	tetoRegras := b.Total
	regras := ResolveRules(in.Rules)
	for i, r := range regras {
		if !gastar(EstimateTokens(r)+perItemOverhead, &tetoRegras) {
			p.Dropped.Rules = len(regras) - i
			break
		}
		p.Rules = append(p.Rules, r)
	}

	tetoAchados := share(b.Total, b.FindingShare)
	for i, f := range in.Findings {
		if !gastar(f.TokenCost(), &tetoAchados) {
			p.Dropped.Findings = len(in.Findings) - i
			break
		}
		p.Findings = append(p.Findings, f)
	}

	// Índice em ordem estável de nome: o repositório A vem antes do B em toda
	// montagem, hoje e daqui a um mês.
	index := make([]Artifact, len(in.Index))
	copy(index, in.Index)
	sort.SliceStable(index, func(i, j int) bool { return index[i].Name < index[j].Name })

	tetoIndice := share(b.Total, b.IndexShare)
	for i := range index {
		if !gastar(index[i].TokenCost(), &tetoIndice) {
			p.Dropped.Index = len(index) - i
			break
		}
		p.Index = append(p.Index, index[i])
	}

	// Memórias por relevância; empate desfeito pelo id, de novo por
	// determinismo — duas memórias com o mesmo score não podem trocar de lugar
	// entre duas montagens da mesma demanda.
	mem := make([]ScoredArtifact, len(in.Memories))
	copy(mem, in.Memories)
	sort.SliceStable(mem, func(i, j int) bool {
		if mem[i].Score != mem[j].Score {
			return mem[i].Score > mem[j].Score
		}
		return mem[i].Artifact.ID < mem[j].Artifact.ID
	})

	tetoMemoria := share(b.Total, b.MemoryShare)
	for i := range mem {
		if !gastar(mem[i].Artifact.TokenCost(), &tetoMemoria) {
			p.Dropped.Memories = len(mem) - i
			break
		}
		p.Memories = append(p.Memories, mem[i].Artifact)
	}

	p.EstimatedTokens = b.Total - restante
	return p
}
