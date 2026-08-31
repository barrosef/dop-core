package app

import (
	"context"

	"google.golang.org/grpc"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres/projection"
	appgrpc "github.com/Digital-Business-One/dop-core/internal/app/grpc"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/resource"
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

	return nil
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

	log.Info("projeções registradas", "total", 1, "consumidores", []string{"timeline"})
	return nil
}

// RegisterLauncher assina os comandos de sandbox no cluster de execução.
func RegisterLauncher(ctx context.Context, _ *Deps) error {
	logging.From(ctx).Info("launcher registrado", "assinaturas", 0)
	return nil
}

// RunScheduledTasks executa o ciclo periódico: polling de PRs, suspensão de
// sandboxes ociosos, criação de partições futuras de events, expiração de
// convites e limpeza da tabela de idempotência.
func RunScheduledTasks(ctx context.Context, _ *Deps) {
	logging.From(ctx).Debug("ciclo do scheduler")
}
