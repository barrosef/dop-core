package app

import (
	"context"
	"time"

	"google.golang.org/grpc"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres/projection"
	appgrpc "github.com/Digital-Business-One/dop-core/internal/app/grpc"
	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
	"github.com/Digital-Business-One/dop-core/internal/domain/cost"
	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/execution"
	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/resource"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// RegisterServices liga os serviços de domínio ao servidor gRPC.
//
// Este é o composition root em ação: o domínio recebe PORTAS (repositório,
// SecretStore), os adaptadores concretos são escolhidos aqui.
// Recebe ctx porque o serviço de eventos abre UMA assinatura por processo, e
// essa assinatura vive enquanto o processo viver — é o contexto do servidor
// que a encerra, não o de um cliente.
func RegisterServices(ctx context.Context, srv *grpc.Server, deps *Deps) error {
	// identity é a raiz: hierarquia e recursos autorizam CONTRA ela. Por isso
	// nasce primeiro e é passada adiante como porta (resource.Access), não
	// como dependência concreta.
	// O relógio é porta, e agora é obrigatório: o serviço recusa nil, porque
	// aceitar nil era o que mantinha a abstração de enfeite.
	relogio := clock.NewSystem()

	identitySvc := identity.NewService(postgres.NewIdentityRepo(deps.Pool), relogio)
	dopv1.RegisterIdentityServiceServer(srv, appgrpc.NewIdentityServer(identitySvc))

	hierarchySvc := hierarchy.NewService(postgres.NewHierarchyRepo(deps.Pool))
	dopv1.RegisterHierarchyServiceServer(srv, appgrpc.NewHierarchyServer(hierarchySvc))

	// O SecretStore chega aqui já escolhido por configuração (wire.go): o
	// domínio de recursos guarda credencial sem saber se o cofre é k8s ou GCP.
	resourceSvc := resource.NewService(postgres.NewResourceRepo(deps.Pool), identitySvc, deps.Secrets)
	dopv1.RegisterResourceServiceServer(srv, appgrpc.NewResourceServer(resourceSvc))

	// Eventos ao vivo: UMA assinatura no barramento por processo, com fan-out
	// em memória para os assinantes. Uma assinatura POR CLIENTE criaria um
	// consumidor durável no broker por aba aberta do cockpit — e a porta não
	// tem como removê-los.
	eventSvc := event.NewService(postgres.NewEventRepo(deps.Pool), deps.Bus, relogio)
	if err := eventSvc.Start(ctx, "", []string{"dop.>"}); err != nil {
		return err
	}
	dopv1.RegisterEventServiceServer(srv, appgrpc.NewEventServer(eventSvc))

	// O repositório de fluxo satisfaz DUAS portas — o conteúdo e a cadeia de
	// ancestrais. São perguntas diferentes que a mesma tabela responde; separar
	// as portas mantém o domínio dizendo o que precisa, não de quem precisa.
	wfRepo := postgres.NewWorkflowRepo(deps.Pool)
	workflowSvc := workflow.NewService(wfRepo, wfRepo, workflowAccess{identitySvc}, relogio)
	dopv1.RegisterWorkflowServiceServer(srv, appgrpc.NewWorkflowServer(workflowSvc))

	// O roteador é POLÍTICA deste pacote, não porta: nil escolhe o padrão
	// escrito na ADR-0011, que é rascunho a calibrar com telemetria (P-7).
	costSvc := cost.NewService(postgres.NewCostRepo(deps.Pool), relogio, cost.NewRouter(nil))
	dopv1.RegisterCostServiceServer(srv, appgrpc.NewCostServer(costSvc))

	// A demanda congela o fluxo resolvido e assiste ao próprio log de eventos.
	// Nenhuma das duas coisas ela sabe fazer sozinha — e nenhuma das duas ela
	// precisa saber de quem vem (ver internal/app/glue.go).
	demandSvc := demand.NewService(
		postgres.NewDemandRepo(deps.Pool),
		demandFlows{workflowSvc},
		demandWatcher{eventSvc},
		relogio,
	)
	dopv1.RegisterDemandServiceServer(srv, appgrpc.NewDemandServer(demandSvc))

	// Embedder nulo é DECLARADO, não esquecido: sem serviço de embedding
	// ligado, a busca cai no caminho lexical (trigrama). O dia em que houver
	// um, é aqui que ele entra — e só aqui.
	knowledgeSvc := knowledge.NewService(
		postgres.NewKnowledgeRepo(deps.Pool),
		deps.Objects,
		knowledgeDemands{demandSvc, hierarchySvc},
		nil,
		relogio,
		knowledge.Budget{},
	)
	dopv1.RegisterKnowledgeServiceServer(srv, appgrpc.NewKnowledgeServer(knowledgeSvc))

	// O provedor de git é resolvido POR REPOSITÓRIO (ADR-0013), não escolhido
	// no boot — ver internal/app/gitproviders.go.
	deliverySvc := delivery.NewService(
		postgres.NewDeliveryRepo(deps.Pool),
		deliveryDemands{demandSvc},
		relogio,
		gitProviders{deps.Pool, resourceSvc, deps.Secrets, deps.Cfg},
	)
	dopv1.RegisterDeliveryServiceServer(srv, appgrpc.NewDeliveryServer(deliverySvc))

	// O launcher chega já escolhido por configuração (wire.go): o domínio
	// provisiona sandbox sem saber se o substrato é Docker ou Kubernetes.
	executionSvc := buildExecution(deps, identitySvc, demandSvc, relogio)
	dopv1.RegisterExecutionServiceServer(srv, appgrpc.NewExecutionServer(executionSvc))

	// A caixa de atenção é PROJEÇÃO, e o serviço dela é só leitura + streaming:
	// item nasce e morre de evento, nunca de RPC.
	attentionSvc := attention.NewService(
		postgres.NewAttentionRepo(deps.Pool),
		attentionWatcher{eventSvc},
		relogio,
	)
	dopv1.RegisterAttentionServiceServer(srv, appgrpc.NewAttentionServer(attentionSvc))

	// O runtime de agente vive AQUI, e não no BFF (ADR-0023): a credencial do
	// provedor sai do cofre e é usada no mesmo processo, sem atravessar rede
	// nenhuma. O BFF é a camada exposta à internet — e comprometê-la não pode
	// entregar as credenciais de agente de todas as contas.
	agentSvc := agent.NewService(
		agentProviders{resourceSvc, deps.Secrets},
		agentKnowledge{knowledgeSvc},
		agentRouting{costSvc},
		agentConversation{demandSvc},
	)
	dopv1.RegisterAgentServiceServer(srv, appgrpc.NewAgentServer(agentSvc))

	return nil
}

// buildExecution monta o serviço de execução.
//
// Existe como função porque DOIS processos precisam dele: o `serve`, que atende
// as RPCs, e o `sched`, que varre sandboxes ociosos. Construir nos dois lugares
// separadamente é como as duas montagens divergem — uma ganha uma dependência
// nova e a outra não, e o comportamento passa a depender do modo.
func buildExecution(deps *Deps, id *identity.Service, dm *demand.Service, relogio ports.Clock) *execution.Service {
	return execution.NewService(
		postgres.NewExecutionRepo(deps.Pool),
		deps.Launcher,
		id,
		executionDemands{dm},
		relogio,
		execution.Config{
			DevboxImage:   deps.Cfg.DevboxImage,
			IngressDomain: deps.Cfg.IngressDomain,
		},
	)
}

// RegisterProjections assina os consumidores que constroem as projeções:
// dossiê, timeline, caixa de atenção, métricas e custo (ADR-0006).
func RegisterProjections(ctx context.Context, deps *Deps) error {
	log := logging.From(ctx)

	// Timeline: a linha do tempo por agregado. Consumidor idempotente — a
	// entrega do JetStream é ao-menos-uma-vez (ADR-0019).
	timeline := projection.NewTimeline(deps.Pool)
	if err := deps.Bus.Subscribe(ctx, "", "timeline", []string{"dop.>"}, timeline.Handle); err != nil {
		return err
	}

	// Caixa de atenção: assina SÓ os assuntos que a regra sabe traduzir. Assinar
	// `dop.>` e descartar a maioria seria desperdiçar entrega; assinar de menos
	// faria o item nunca chegar, em silêncio — há teste no domínio garantindo
	// que os assuntos cobrem todos os eventos tratados.
	caixa := projection.NewAttention(deps.Pool)
	if err := deps.Bus.Subscribe(ctx, "", "attention", attention.Subjects(), caixa.Handle); err != nil {
		return err
	}

	log.Info("projeções registradas", "total", 2,
		"consumidores", []string{"timeline", "attention"})
	return nil
}

// RegisterLauncher assina os comandos de sandbox no cluster de execução.
// RegisterLauncher prepara o substrato de execução no processo de launcher.
//
// Hoje o ciclo de vida do sandbox é dirigido por CHAMADA (ProvisionSandbox e
// companhia) e por VARREDURA (o scheduler suspende os ociosos). Não há
// assinatura de evento: provisionar automaticamente ao iniciar demanda é
// política que ainda não foi decidida, e criar sandbox — que custa dinheiro —
// por evento sem essa decisão seria inventar governança de gasto.
//
// Quando a política existir, é aqui que a assinatura entra.
func RegisterLauncher(ctx context.Context, deps *Deps) error {
	log := logging.From(ctx)
	tiers, err := deps.Launcher.SupportedTiers(ctx)
	if err != nil {
		// Não saber qual isolamento o substrato oferece é motivo para NÃO
		// subir: `isolationTier` é declarado e conferido, e um launcher que
		// não sabe responder aceitaria qualquer coisa mais tarde.
		return err
	}
	log.Info("launcher pronto", "substrato", deps.Cfg.SandboxBackend, "isolamentos", tiers)
	return nil
}

// RunScheduledTasks executa o ciclo periódico: polling de PRs, suspensão de
// sandboxes ociosos, criação de partições futuras de events, expiração de
// convites e limpeza da tabela de idempotência.
func RunScheduledTasks(ctx context.Context, deps *Deps) {
	log := logging.From(ctx)

	// Partições futuras PRIMEIRO, antes de qualquer outra tarefa: sem elas,
	// na virada do mês toda escrita de evento falha — e falha no caminho do
	// outbox, derrubando qualquer operação que mude estado. As migrações
	// criam partições fixas e param; quem continua daqui é isto.
	criadas, err := postgres.EnsureMonthlyPartitions(ctx, deps.Pool, time.Now().UTC(), partitionsAhead)
	if err != nil {
		// Não derruba o ciclo: o próximo tenta de novo, e há meses de folga
		// antes de a falta virar problema. Mas sobe como ERRO, porque é o
		// aviso que separa "meses de folga" de "amanhã para tudo".
		log.Error("falha ao garantir partições futuras", logging.FieldError, err.Error())
	}
	if len(criadas) > 0 {
		log.Info("partições criadas", "particoes", criadas)
	}

	// Varredura de economia: sandbox parado além do limite suspende. O pod
	// morre, o workspace sobrevive no volume. A spec do substrato é direta
	// sobre o custo de não fazer isso — "sandbox ocioso é o que separa
	// paralelismo real de máquina afogada".
	{
		varredor := execution.NewSweeper(
			postgres.NewExecutionRepo(deps.Pool), deps.Launcher, clock.NewSystem())
		contas, suspensos, err := varredor.SweepAllAccounts(ctx)
		if err != nil {
			log.Error("varredura de sandboxes ociosos falhou", logging.FieldError, err.Error())
		} else if suspensos > 0 {
			log.Info("sandboxes ociosos suspensos", "contas", contas, "sandboxes", suspensos)
		}
	}

	log.Debug("ciclo do scheduler")
}

// partitionsAhead é folga, não previsão: com 3 meses o scheduler pode ficar
// fora do ar semanas sem que ninguém perceba a diferença.
const partitionsAhead = 3
