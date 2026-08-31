package app

import (
	"context"

	"google.golang.org/grpc"

	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// RegisterServices registra os serviços gRPC do domínio.
// Cada domínio entra aqui conforme sai do esqueleto.
func RegisterServices(_ *grpc.Server, _ *Deps) {
	// identityv1.RegisterIdentityServiceServer(srv, identitygrpc.New(deps))
	// ...
}

// RegisterProjections assina os consumidores que constroem as projeções:
// dossiê, timeline, caixa de atenção, métricas e custo (ADR-0006).
func RegisterProjections(ctx context.Context, deps *Deps) error {
	log := logging.From(ctx)
	// Exemplo de forma; os consumidores reais entram com seus domínios.
	// return deps.Bus.Subscribe(ctx, "DOP", "timeline", []string{"dop.demand.>"}, timeline.Handle(deps.Pool))
	log.Info("projeções registradas", "total", 0)
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
