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
// A superfície é o mínimo do fluxo da ADR-0008: abrir o PR com o pacote de
// evidência, reaplicar sobre a base atual, mergear um de cada vez, e perguntar
// se o provedor já tem fila própria. Falar com GitHub/GitLab é adaptador, não
// domínio.
//
// Uma instância fala por UMA credencial e por UM ator. O token chega PRONTO no
// construtor do adaptador (ADR-0013: token de provedor é credencial de recurso,
// guardada atrás de ports.SecretStore). O adaptador de git não conhece o cofre,
// não o consulta e não sabe que ele existe — quem monta resolve o segredo e
// entrega o valor.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//
//  1. CONFLITO É DADO, ERRO É FALHA. Rebase e Merge devolvem Conflicted=true
//     com erro NIL quando o provedor diz que aquele conteúdo não integra. Erro
//     fica para o que impede de SABER: rede, provedor fora do ar, credencial
//     inválida, permissão, repositório inexistente, resposta ilegível. A
//     distinção é operacional, não estética — conflito vira tarefa do agente e,
//     se ele não resolver, item da caixa de atenção (ADR-0008 §2), enquanto
//     erro vira retry e alerta de infra. Trocar um pelo outro ou esconde o
//     conflito do humano ou enche a caixa de atenção com queda de rede;
//
//  2. Conflicted=true SEMPRE traz Detail não vazio, e Files é BEST-EFFORT: pode
//     vir vazia mesmo havendo conflito. Nenhum dos dois provedores publica a
//     lista de arquivos em conflito na API de PR/MR — o GitHub não a expõe, e
//     no GitLab ela só existe numa rota interna do Rails, fora do /api/v4, sem
//     versão e sem promessa. Prometer Files seria prometer o que só sai de um
//     `git merge` local; quem decidir com base nela estará decidindo com base
//     em sorte. Detail carrega o que o provedor de fato diz, e é o campo que a
//     caixa de atenção mostra;
//
//  3. ABRIR PR É IDEMPOTENTE por (repositório, branch de origem, branch de
//     destino). Chamar duas vezes NÃO cria dois PRs e NÃO é erro: a segunda
//     chamada devolve o PR que já existe. Os dois provedores recusam o segundo
//     PR — o GitHub com 422, o GitLab com 409 — e é o adaptador que transforma
//     essa recusa em "aqui está o que existe". Sem isso, cada timeout de rede
//     numa frota de agentes viraria um item de atenção sobre um PR que foi
//     aberto com sucesso;
//
//  4. reabrir com título ou corpo diferentes NÃO reescreve o PR existente: a
//     porta devolve o que está lá, e atualizar PR fica FORA (ver abaixo).
//     Idempotência que sobrescreve não é idempotência — é a última chamada
//     ganhando, e o pacote de evidência da ADR-0007 §4 é justamente o que não
//     pode ser trocado por um retry;
//
//  5. com erro nil, ProviderPR tem ExternalID e URL NÃO VAZIOS. ExternalID é a
//     identidade do PR no provedor e é o que Merge recebe depois: devolver PR
//     sem identificador é devolver algo que não se consegue mergear;
//
//  6. MERGE É IDEMPOTENTE: mergear um PR já mergeado devolve Merged=true com o
//     commit de merge que já existe — não erro, não conflito. Cumprir isto
//     custa uma leitura extra, porque os DOIS provedores recusam o PR já
//     mergeado com o MESMO código HTTP que usam para "há conflito": a resposta
//     crua é ambígua e o adaptador precisa ler o PR para desempatar;
//
//  7. Merged=true só quando o provedor CONFIRMA o merge, nunca por aceitação de
//     pedido, e nesse caso MergeCommit não é vazio. "Aceitei seu pedido" e
//     "está na main" são fatos diferentes, e a fila da ADR-0008 libera a
//     próxima posição com base no segundo;
//
//  8. Merged=false COM Conflicted=false é resposta LEGÍTIMA: o merge não
//     aconteceu e o motivo não é conflito — pipeline do provedor rodando,
//     aprovação faltando, PR em rascunho, regra de proteção de branch. Detail
//     diz qual. Sem esse terceiro estado o adaptador seria obrigado a mentir em
//     um dos dois campos, e "conflito" viraria o balde de tudo o que não
//     mergeou — mandando um humano resolver um pipeline que ainda está rodando;
//
//  9. REBASE É SÍNCRONO NA PORTA. Os provedores respondem antes de terminar (o
//     GitHub aceita a mutação e processa depois; o GitLab devolve "rebase em
//     andamento" e faz o trabalho num worker), e é o ADAPTADOR que espera o
//     desfecho dentro do contexto do chamador. Contexto cancelado ou prazo
//     esgotado é KindUnavailable — nunca um Conflicted=false, que significaria
//     "não conflitou" quando o que houve foi "não sei";
//
//  10. REBASE EXIGE PR ABERTO, e `Onto` NÃO É LIVRE. RebaseSpec parece uma
//     operação de git e não é: nenhum dos dois provedores reaplica um branch
//     solto. O GitHub reaplica o branch de um PR, e só por GraphQL — o REST
//     dele não tem rebase nenhum, só um `update-branch` que MERGEIA a base
//     dentro do branch. O GitLab reaplica o branch de um MR. Os dois reaplicam
//     sempre sobre o destino DAQUELE PR/MR. Por isso: sem PR aberto para
//     (Branch → Onto), a resposta é KindPrecondition com a explicação — nunca
//     uma reaplicação silenciosa sobre outra base, que é o que a fila da
//     ADR-0008 re-verificaria acreditando ser outro estado do código;
//
//  11. NOMES DO PROVEDOR NÃO CRUZAM A PORTA. `mergeable_state`, `merge_status`,
//     `detailed_merge_status`, número de PR e iid de MR ficam do lado de lá.
//     ExternalID é OPACO: é o que a porta devolveu e o que ela aceita de volta,
//     sem formato prometido. É esta garantia que faz trocar de provedor ser
//     fiação;
//
//  12. REPOSITÓRIO INEXISTENTE OU INVISÍVEL é KindNotFound; token que existe
//     mas não pode agir sobre o recurso é KindPermission; token ausente,
//     inválido ou expirado é KindUnauthorized. E a ressalva honesta: o GitHub
//     responde 404 para repositório privado que o token não enxerga, DE
//     PROPÓSITO, para não revelar que ele existe. O adaptador não adivinha qual
//     dos dois é — repassa NotFound. Prometer distinguir seria prometer o que o
//     provedor esconde;
//
//  13. O TOKEN NÃO APARECE EM LUGAR NENHUM: nem em mensagem de erro, nem em
//     log, nem em campo de struct, nem no texto de %v, %+v ou %#v do
//     adaptador. Erro sobe para log, e token de provedor em log é credencial em
//     repouso: quem lê o log abre PR e mergeia como o dono. A suíte de contrato
//     usa um token sentinela e varre TODA saída de erro atrás dele;
//
//  14. ActorID é CONFERIDO, não resolvido. A instância foi construída para um
//     ator (ADR-0003: quem conduziu assina); pedido em nome de OUTRO ator é
//     recusado com KindPermission. Ignorar o campo seria pior que recusar: o PR
//     sairia assinado por quem quer que seja o dono do token fiado, e "quem
//     conduziu assina" viraria mentira silenciosa no lugar exato onde a
//     rastreabilidade importa;
//
//  15. HasNativeQueue NUNCA INVENTA. Quando o adaptador não consegue OLHAR —
//     sem permissão para ler as regras do repositório, escopo de token que não
//     alcança o projeto — devolve ERRO com a causa, jamais `false`. `false` é
//     uma AFIRMAÇÃO ("este repositório não tem fila nativa, pode orquestrar por
//     cima") e afirmá-la sem ter olhado é o mesmo defeito de degradar
//     isolamento em silêncio: o sistema segue parecendo saudável e a diferença
//     só aparece no dia do incidente — aqui, com duas filas mergeando o mesmo
//     repositório. Repare no que a pergunta ESCONDE: a merge queue do GitHub é
//     por BRANCH (é regra de ruleset) e o merge train do GitLab é por PROJETO.
//     A porta pergunta por repositório, e a resposta é sobre o branch que a
//     fila da ADR-0008 disputa — o padrão;
//
//  16. HasNativeQueue é LEITURA: não cria, não altera e não configura fila
//     nenhuma. A porta responde se existe; entrar nela não está aqui;
//
//  17. as quatro operações são seguras para uso concorrente.
//
// FORA da porta, de propósito:
//
//   - ENTRAR na fila nativa do provedor. Perguntar se existe é uma pergunta só
//     nos dois; ENTRAR não é a mesma operação: no GitHub a merge queue é
//     configurada por ruleset de branch e o PR entra por auto-merge; no GitLab
//     o merge train é uma fila de pipelines, de plano pago, em que se entra por
//     auto merge. A ADR-0008 §4 pede que a plataforma saiba quando NÃO duplicar
//     a fila — não que ela dirija a fila alheia;
//
//   - REVISÃO: pedir revisor, aprovar, comentar, resolver conversa. É a
//     superfície mais assimétrica entre os dois — o GitHub tem revisões com
//     veredito por pessoa, o GitLab tem aprovações por REGRA, com contagem
//     mínima e escopo de plano. Não existe denominador comum que não seja o
//     vocabulário de um dos dois disfarçado de porta;
//
//   - STATUS E CHECKS do provedor. O verde do DOP é a evidência da ADR-0007,
//     produzida no sandbox e gravada como VerificationRun. Trazer o check do
//     provedor para cá faria a plataforma aceitar como prova um verde que não
//     foi ela que produziu — que é exatamente a confiança que a ADR-0007 recusa;
//
//   - CRIAR, APAGAR E EMPURRAR BRANCH, e qualquer operação de git. Esta porta é
//     sobre o PEDIDO DE INTEGRAÇÃO, não sobre o repositório;
//
//   - ATUALIZAR o PR (título, corpo, destino), rascunho, rótulo, marco,
//     responsável, e FECHAR sem mergear. Nada disso é exigido pelo fluxo da
//     ADR-0008, e cada um traz um vocabulário que diverge entre os dois;
//
//   - MÉTODO DE MERGE (merge commit, squash, rebase) e mensagem do commit. É
//     política do fluxo git, que a ADR-0013 trata como recurso governado
//     (`git_flow`) e o composition root injeta no adaptador. E é traduzível só
//     em parte: no GitHub o método vai na chamada de merge; no GitLab ele é
//     configuração do PROJETO, e a chamada só aceita `squash`. A fila da
//     ADR-0008 precisa que o merge aconteça, não que ele aconteça de um jeito;
//
//   - WEBHOOKS e assinatura de evento. É o provedor chamando a plataforma, não
//     a plataforma chamando o provedor — outra direção, outra porta.
type GitProvider interface {
	OpenPullRequest(ctx context.Context, spec OpenPRSpec) (ProviderPR, error)
	Rebase(ctx context.Context, spec RebaseSpec) (RebaseResult, error)
	Merge(ctx context.Context, spec MergeSpec) (MergeResult, error)
	// HasNativeQueue diz se o provedor tem merge queue própria (merge queue do
	// GitHub, merge trains do GitLab). A fila do DOP orquestra por cima e cobre
	// quem não tem.
	HasNativeQueue(ctx context.Context, repoExternalID string) (bool, error)
}

// GitProviders resolve QUAL provedor atende um repositório.
//
// Não é escolha de boot, como SecretStore ou EventBus: o provedor é do
// REPOSITÓRIO (ADR-0013), e é justamente por isso que `ProjectRepo` carrega
// `IntegrationID` — um projeto com um repo no GitHub e outro no GitLab tem que
// ser representável. Um provedor único escolhido por configuração tornaria isso
// impossível, silenciosamente.
//
// É a mesma natureza da porta de provedor de agente (ADR-0022): escolhida por
// requisição, vários adaptadores ativos ao mesmo tempo.
//
// Quem implementa também resolve a CREDENCIAL, no cofre — por isso a porta
// devolve um `GitProvider` já pronto, e nenhum adaptador de git conhece o cofre.
type GitProviders interface {
	For(ctx context.Context, accountID, repoID string) (GitProvider, error)
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
