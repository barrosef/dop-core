package demand

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// Emission é o EVENTO que a operação produz, do jeito que o domínio o enxerga:
// tipo e conteúdo. Agregado e agregado_id não entram porque são sempre os
// mesmos — `demand` e o id da demanda —, e isso não é economia de digitação: é
// a regra da ADR-0006 ("log append-only POR DEMANDA") escrita de forma que não
// dê para violar por descuido. Mensagem de thread, achado e decisão de portão
// pertencem ao log da demanda; quem precisa recortar por thread lê `thread_id`
// no payload.
type Emission struct {
	Type    string
	Payload map[string]any
}

// Tipos de evento da demanda. O outbox deriva o assunto NATS deles (Subject).
const (
	EventStarted          = "dop.demand.started"
	EventStageAdvanced    = "dop.demand.stage.advanced"
	EventGateDecided      = "dop.demand.gate.decided"
	EventThreadCreated    = "dop.demand.thread.created"
	EventThreadBlocked    = "dop.demand.thread.blocked"
	EventThreadResumed    = "dop.demand.thread.resumed"
	EventThreadConcluded  = "dop.demand.thread.concluded"
	EventMessagePosted    = "dop.demand.message.posted"
	EventFindingPublished = "dop.demand.finding.published"
)

// Aggregate é o nome do agregado no log — um só para tudo que é da demanda.
const Aggregate = "demand"

// Repository é a PORTA de persistência do domínio de demanda.
//
// Repare no formato de TODA escrita: recebe o estado novo, a Emissão e a chave
// de idempotência, e devolve o resultado. É proposital — a assinatura força o
// adaptador a gravar estado e evento na MESMA transação (ADR-0019). Uma porta
// com `Save` de um lado e `Emit` de outro deixaria a atomicidade a cargo da
// disciplina de quem chama, que é exatamente o que a ADR existe para não
// depender.
//
// Toda operação recebe accountID explicitamente: isolamento multi-tenant é
// parâmetro obrigatório da porta, não algo que o adaptador possa esquecer.
type Repository interface {
	// ── demanda ──
	List(ctx context.Context, accountID, projectID string, limit int, after string) ([]Demand, error)
	ByID(ctx context.Context, accountID, id string) (*Demand, error)
	ByExternalKey(ctx context.Context, accountID, projectID, externalKey string) (*Demand, error)
	// Create grava a demanda com o fluxo já congelado e suas etapas iniciais.
	Create(ctx context.Context, d *Demand, ev Emission, idemKey string) (*Demand, error)
	// SaveStage grava a etapa e o status projetado da demanda. Devolve a etapa
	// como ficou gravada.
	SaveStage(ctx context.Context, accountID, demandID string, st Stage, status DopStatus, ev Emission, idemKey string) (*Stage, error)

	// ── threads ──
	ThreadsOf(ctx context.Context, accountID, demandID string) ([]Thread, error)
	ThreadByID(ctx context.Context, accountID, id string) (*Thread, error)
	CreateThread(ctx context.Context, t *Thread, ev Emission, idemKey string) (*Thread, error)
	SaveThreadState(ctx context.Context, accountID, threadID string, state ThreadState, ev Emission, idemKey string) (*Thread, error)

	// AppendMessage acrescenta ao log. Não existe `UpdateMessage` nem
	// `DeleteMessage`: o log é append-only (ADR-0006).
	AppendMessage(ctx context.Context, m *Message, ev Emission, idemKey string) (*Message, error)

	// ── achados ──
	CreateFinding(ctx context.Context, f *Finding, ev Emission, idemKey string) (*Finding, error)
	// ListFindings devolve os achados JÁ PUBLICADOS na demanda.
	//
	// Existe para o pacote de contexto (ADR-0009): sem os achados, um agente
	// que retoma a demanda refaz investigação que outro já concluiu — que é
	// exatamente o desperdício que o quadro de achados existe para evitar.
	ListFindings(ctx context.Context, accountID, demandID string) ([]Finding, error)

	// HasFinding responde se a thread já publicou achado — é o que destrava a
	// conclusão dela.
	HasFinding(ctx context.Context, accountID, threadID string) (bool, error)
}

// FlowResolver é a porta ESTREITA para o domínio de fluxo.
//
// A demanda precisa de UMA coisa dele, uma vez na vida: o fluxo efetivo no
// instante em que começa, resolvido pela cadeia plataforma ◁ conta ◁ workspace
// ◁ projeto ◁ demanda (ADR-0014). Depois disso o fluxo vivo deixa de importar —
// o que dirige a demanda é o snapshot congelado. Por isso a porta tem um método
// e nenhuma noção de edição, versionamento ou promoção de fluxo: essas são do
// domínio workflow, e este pacote não as conhece.
type FlowResolver interface {
	// Resolve devolve o fluxo efetivo do escopo pedido (scope: "project",
	// "demand", "workspace", "account"), com versão e etapas — o bastante para
	// congelar.
	Resolve(ctx context.Context, accountID, scope, scopeID string) (Flow, error)
}

// Watcher é a porta de ASSINATURA de eventos, para o WatchDemand.
//
// Desenhada sobre a superfície que o domínio `event` já oferece (fan-out único
// por processo, replay por cursor, isolamento por conta): assinar por cliente
// no barramento criaria um consumidor durável por aba aberta do cockpit, e
// reimplementar fan-out aqui duplicaria a política de consumidor lento em dois
// lugares que divergem com o tempo.
//
// O filtro é por agregado e tipo porque é o que o serviço de eventos sabe
// fazer; o recorte por DEMANDA é feito neste pacote, comparando o
// aggregate_id — ver Service.Watch.
type Watcher interface {
	Watch(ctx context.Context, sinceEventID string, aggregates, types []string, emit func(ports.Event) error) error
}
