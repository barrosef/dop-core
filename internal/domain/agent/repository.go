package agent

import "context"

// ════════════════════════════════════════════════════════════════════════════
// AS PORTAS DO RUNTIME
//
// Este domínio não tem persistência própria: um turno não guarda estado, ele
// PRODUZ estado nos domínios vizinhos (mensagem, consumo, achado). O que este
// arquivo declara são duas famílias de porta:
//
//   - AgentProvider, a porta de INFRAESTRUTURA — o fornecedor do modelo, com
//     um adaptador por fornecedor em internal/adapter/agentprovider;
//   - Knowledge, Routing e Conversation, as portas ESTREITAS para os domínios
//     vizinhos. O runtime não importa `knowledge`, `cost` nem `demand`: ele diz
//     o que precisa, no vocabulário dele, e o composition root liga. É a mesma
//     escolha de `resource.Access` sobre `identity.Service`, e o preço dela é a
//     cola em internal/app — o que se compra é que mudar a forma interna de um
//     vizinho não quebra o runtime.
// ════════════════════════════════════════════════════════════════════════════

// ── a porta do fornecedor ────────────────────────────────────────────────────

// AgentProvider é o fornecedor do modelo. Um adaptador por fornecedor, todos
// com ESTE contrato.
//
// Consequência dura, e é a razão de o vocabulário deste pacote existir: nada
// acima desta porta pode ver tipo de fornecedor. Nem struct de resposta de API,
// nem `map[string]any` cru, nem nome de campo de SDK. O ciclo do turno
// (service.go) conversa só com os tipos de entity.go; trocar de fornecedor é
// trocar o adaptador.
//
// GARANTIAS verificadas pela suíte de contrato, em TODO adaptador:
//
//  1. Info() não faz I/O e é estável: mesma instância, mesma ficha. É ficha,
//     não consulta — a borda a exibe e o relatório a audita sem gastar rede;
//
//  2. Info().ResolveModel devolve nome concreto e NÃO VAZIO para as três
//     classes, e é PURA: mesma classe, mesmo nome, sem I/O e sem relógio;
//
//  3. Render põe o prefixo estável ANTES das mensagens na requisição
//     SERIALIZADA. É a economia da ADR-0012 §1, e ela quebra sem barulho: um
//     prefixo que foi para o fim do corpo não erra, só custa 10× e ninguém vê;
//
//  4. Render é DETERMINÍSTICO: mesmo Turn, mesmos bytes. Um mapa iterado em
//     ordem aleatória, um timestamp ou um id de requisição no corpo invalidam o
//     prefixo cacheado a cada turno — o invalidador mais silencioso que existe;
//
//  5. a mensagem de operador NUNCA é serializada como fala do usuário (D3).
//     Quando o modelo não tem canal próprio, o adaptador pode recuar para um
//     bloco MARCADO dentro do turno do usuário — e nesse caso devolve aviso.
//     Recuo sem aviso é a mesma coisa que injeção com nossa assinatura;
//
//  6. Send devolve Usage com as quatro parcelas DISJUNTAS (D2), qualquer que
//     seja a semântica do fornecedor. Quem inclui cacheado no total subtrai;
//
//  7. StopReason está sempre no vocabulário do domínio (D5). Motivo novo do
//     fornecedor vira StopUnknown, nunca erro;
//
//  8. EffortApplied nunca AFIRMA mais do que o provedor aplicou (D4). Quando
//     houve rebaixamento, há aviso legível em Warnings;
//
//  9. credencial ausente, credencial recusada, rede fora e erro do fornecedor
//     viram *Unavailable com a razão certa — nunca erro cru de transporte e
//     nunca erro interno nosso (D6);
//
//  10. a CREDENCIAL não aparece em Error(), em Render(), nem na formatação do
//     próprio adaptador (`%v`, `%+v`). É a garantia cuja falha custa a conta
//     inteira: chave de agente em log é chave em repouso;
//
//  11. quem anuncia CapExplicitPrefixCache MARCA o breakpoint no corpo, e o
//     marcador fica no FIM DO PREFIXO — não no fim do prompt. As três garantias
//     de cache (3, 4 e esta) são independentes: dá para acertar a ordem, acertar
//     o determinismo e ainda assim não pedir cache nenhum. Nenhuma das três
//     falha com erro; as três falham com fatura;
//
//  12. quem anuncia CapStructuredOutput manda o `Turn.OutputSchema` NO FIO. A
//     decodificação acontece do nosso lado e continua funcionando enquanto o
//     modelo colaborar — o que torna a ausência do schema invisível até o dia em
//     que ele não colabora (ADR-0012 §2);
//
//  13. `Turn.MaxOutputTokens` chega ao fornecedor. Um teto que o adaptador
//     escolhe sozinho corta a resposta num limite que ninguém pediu, e o corte
//     sai como StopReason normal, sem nada que o explique;
//
//  14. `Send` envia exatamente o que `Render` mostra. Sem isso, `Render` é uma
//     vitrine ao lado de um envio diferente, e as garantias 3, 5, 11, 12 e 13 —
//     todas auditadas sobre `Render` — estariam olhando para o lugar errado.
//
// ── O QUE FICOU DE FORA, EXPLICITAMENTE ─────────────────────────────────────
//
// A regra das portas deste projeto: o que não é cumprível por TODOS os
// adaptadores não entra — porque uma porta que só um fornecedor honra é o
// fornecedor com outro nome.
//
//   - FERRAMENTAS e o laço de tool use. Os formatos divergem em três eixos ao
//     mesmo tempo: schema (`input_schema` × `parameters`), resultado (bloco
//     `tool_result` no turno do usuário × mensagem `role:"tool"` própria) e
//     paralelismo (por padrão × por flag). Um laço escrito sobre a média dos
//     dois seria um laço que nenhum dos dois executa bem. `Turn.Tools` já
//     existe, sempre vazio, para que a assinatura não mude quando entrar;
//
//   - STREAMING. O acompanhamento ao vivo da plataforma é o log de eventos
//     (ADR-0006): a mensagem publicada VIRA evento e chega ao cockpit pelo
//     WatchDemand que já existe. Um segundo caminho de streaming aqui seria uma
//     segunda fonte da verdade para a mesma timeline;
//
//   - JANELA DE CONTEXTO e compaction. Cada fornecedor tem a sua, e a ADR-0012
//     §3 já decidiu que a retomada é por RECONSTRUÇÃO (pacote + achados), não
//     por replay do transcript. Compaction é rede de segurança de sessão
//     contínua — coisa do adaptador, não da porta;
//
//   - RETENTATIVA. Quem decide tentar de novo é quem sabe se ainda vale a pena
//     gastar: o orçamento da ADR-0011 §2 e o humano na caixa de atenção. Um
//     retry escondido no adaptador gastaria duas vezes e reportaria uma.
type AgentProvider interface {
	// Info é a ficha: nome, catálogo, capacidades e preços. Sem I/O.
	Info() ProviderInfo

	// Render monta a requisição do fornecedor SEM enviá-la, já serializada.
	//
	// Devolve BYTES, e não um mapa, por duas razões que valem o método
	// público. A primeira é auditoria: é assim que a suíte de contrato
	// verifica a ORDEM do prefixo em QUALQUER fornecedor, procurando as duas
	// fatias no corpo real sem conhecer o formato de nenhum — um mapa
	// remarshalado sairia com as chaves em ordem alfabética e apagaria
	// justamente o que está sendo verificado. A segunda é que permite
	// registrar o que SERIA enviado sem gastar uma chamada.
	//
	// Os avisos devolvidos são os da montagem (effort rebaixado, canal de
	// operador recuado) e viajam para o `Reply`.
	Render(t Turn, model string, effort Effort) (body []byte, warnings []string, err error)

	// Send envia a conversa. Falha SEMPRE como *Unavailable (garantia 9).
	Send(ctx context.Context, t Turn, model string, effort Effort) (*Reply, error)
}

// Providers resolve QUAL adaptador atende esta chamada, já com a credencial.
//
// A escolha é POR REQUISIÇÃO, não de boot: provedor de agente é recurso de
// categoria `agent` (ADR-0013), escolhido por conta e por projeto, e várias
// contas convivem no mesmo processo. Não existe "o adaptador" montado no boot
// como acontece com o SecretStore.
//
// O que esta porta esconde é o ponto inteiro da ADR-0023: quem a implementa
// (internal/app/agentproviders.go) lê a credencial do cofre — no núcleo, no
// mesmo processo — e entrega o adaptador pronto. Este domínio não conhece
// `ports.SecretStore`, não recebe cofre por parâmetro e não sabe que cofre
// existe. A credencial não atravessa fronteira nenhuma, nem de rede nem de
// pacote.
//
// resourceID vazio significa "o provedor padrão desta conta". Provedor
// DESCONHECIDO é recusa explícita, nunca queda silenciosa para outro: cair para
// outro fornecedor sem avisar trocaria o modelo, o preço e a semântica de cache
// de uma demanda inteira, e o único lugar onde isso apareceria seria a fatura.
type Providers interface {
	For(ctx context.Context, resourceID string) (AgentProvider, error)
}

// ── portas estreitas para os vizinhos ────────────────────────────────────────

// ContextArtifact é um artefato do pacote de contexto, com o MÍNIMO que a
// montagem do prompt consome.
//
// Repare no que NÃO está aqui: `id` e `version`. Eles mudam quando o núcleo
// regrava o artefato sem que o conteúdo mude, e entrariam no prefixo cacheado
// para invalidá-lo à toa (ADR-0012 §1). Deixá-los fora da porta é mais forte que
// lembrar de não usá-los.
type ContextArtifact struct {
	Name      string
	Body      string
	ObjectRef string
}

// ContextFinding é um achado já publicado na demanda, como ele entra no prompt.
type ContextFinding struct {
	Title   string
	Summary string
}

// ContextDropped é o que ficou de FORA do pacote por orçamento de tokens.
//
// É informação de primeira classe (ADR-0012), e não detalhe: sem ela o agente
// afirma sobre o que não leu, e o humano lê uma conclusão errada que ninguém
// consegue explicar depois. Ela aparece em DOIS lugares por isso — no prefixo,
// falando com o agente, e na thread, falando com o humano.
type ContextDropped struct {
	Rules    int
	Findings int
	Index    int
	Memories int
}

func (d ContextDropped) Any() bool { return d.Rules+d.Findings+d.Index+d.Memories > 0 }

// ContextPackage é a bagagem de bordo do agente (ADR-0009 §3), no vocabulário
// do runtime. A ORDEM das listas é a da curadoria do núcleo e é PRIORIDADE —
// reordenar aqui desfaria a seleção que custou o orçamento inteiro.
type ContextPackage struct {
	Rules    []string
	Index    []ContextArtifact
	Memories []ContextArtifact
	Findings []ContextFinding
	Dropped  ContextDropped
}

func (p ContextPackage) Truncated() bool { return p.Dropped.Any() }

// Knowledge é a porta estreita para o domínio de conhecimento: uma pergunta só.
type Knowledge interface {
	ContextPackage(ctx context.Context, demandID string) (ContextPackage, error)
}

// Decision é a decisão de roteamento do domínio de custo, com a CLASSE junto.
//
// A classe é o campo que a fronteira de rede comia. Enquanto o runtime vivia no
// BFF, `dop.v1.RoutingDecision` levava só o nome do catálogo do núcleo, e o outro
// lado precisava de uma tabela (o extinto `catalog.py`) para desfazer o caminho
// nome → classe e então pedir ao fornecedor ATIVO o nome DELE. Em processo a
// classe chega inteira, aquela tabela não existe, e o risco que ela carregava —
// adivinhar a classe de um nome desconhecido e trocar em silêncio o modelo que o
// núcleo escolheu — desapareceu junto.
//
// Reason viaja INTEIRA: é o que permite auditar "por que esta demanda rodou no
// modelo caro?" sem abrir o código (ADR-0011 §3).
type Decision struct {
	TaskKind string
	Class    ModelClass
	Model    string
	Effort   Effort
	Reason   string
}

// Consumption é um consumo a registrar, no vocabulário do runtime.
type Consumption struct {
	DemandID            string
	ThreadID            string
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CostMicros          Micros
	Currency            string
}

// BudgetView é um teto estourado, como a caixa de atenção o mostra.
type BudgetView struct {
	Scope       string
	ScopeID     string
	LimitMicros Micros
	SpentMicros Micros
	Currency    string
}

// Accounting é o que o domínio de custo respondeu ao registro.
//
// Exceeded é ESTADO, não transição — a repetição de uma chamada devolve o mesmo
// aviso da original, porque quem pergunta "posso seguir?" precisa da resposta
// certa mesmo quando a escrita não aconteceu de novo.
type Accounting struct {
	BudgetExceeded bool
	Exceeded       []BudgetView
}

// Routing é a porta estreita para o domínio de custo: decidir e medir.
//
// As duas operações moram na mesma porta porque são as duas metades do mesmo
// fato — escolher quanto gastar e registrar quanto gastou. Separá-las daria ao
// composition root duas colas para o mesmo serviço sem ninguém ganhar nada.
type Routing interface {
	Route(ctx context.Context, taskKind, demandID string) (Decision, error)
	// RecordUsage EXIGE chave de idempotência: uma duplicata de consumo não
	// colide com nada, entraria como gasto legítimo e o orçamento viraria
	// ficção (é a mesma exigência que `cost.Service.RecordUsage` faz).
	RecordUsage(ctx context.Context, c Consumption, idemKey string) (Accounting, error)
}

// AgentCard é a ficha da thread (ADR-0010 §2), no vocabulário do runtime.
//
// Ela entra no PREFIXO porque é estável por thread: propósito e ferramentas
// concedidas não mudam a cada turno. E quando declara modelo, ela VENCE o
// roteador — ficha é o contrato congelado daquela thread, e trocar o modelo dela
// no meio invalidaria o prefixo cacheado de todos os turnos anteriores, porque
// cache é por modelo (ADR-0012 §1).
type AgentCard struct {
	Purpose      string
	Tools        []string
	Model        string
	Effort       string
	BudgetMicros int64
}

// Thread é o mínimo que o runtime precisa saber da conversa: como ela se chama e
// com que ficha ela nasceu.
type Thread struct {
	ID   string
	Key  string
	Card AgentCard
}

// FindingRef é o achado publicado, como referência.
type FindingRef struct {
	ID    string
	Title string
}

// Conversation é a porta estreita para o domínio de demanda.
//
// PostMessage devolve só o id: o runtime publica e segue, não relê o que
// escreveu. E a AUTORIA não é parâmetro aqui de propósito — ela vem do contexto
// de chamada (`ctxutil.Call`), como em todo o resto do núcleo. É o serviço que
// troca o ator para o AGENTE antes de publicar a resposta; ver service.go.
type Conversation interface {
	Thread(ctx context.Context, demandID, threadID string) (Thread, error)
	PostMessage(ctx context.Context, threadID, text, idemKey string) (string, error)
	PublishFinding(ctx context.Context, demandID, threadID, title string,
		payload map[string]any, idemKey string) (FindingRef, error)
}
