package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/barrosef/dop-core/internal/adapter/clock"
	"github.com/barrosef/dop-core/internal/adapter/eventbus"
	"github.com/barrosef/dop-core/internal/adapter/postgres"
	"github.com/barrosef/dop-core/internal/platform/config"
	"github.com/barrosef/dop-core/internal/platform/logging"
)

// RunServe brings up the domain's gRPC server.
//
// It never migrates (ADR-0024 §2): several instances would race. It waits for
// the worker to have done so, and refuses a database newer than itself.
func RunServe(ctx context.Context, cfg *config.Config) error {
	log := logging.From(ctx)
	if err := postgres.NewSchema(cfg.DatabaseURL).WaitFor(ctx, clock.NewSystem(), cfg.SchemaWait); err != nil {
		return err
	}
	deps, cleanup, err := Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(UnaryLogging(), UnaryCallContext(deps.CallAuth), UnaryRecover()),
		grpc.ChainStreamInterceptor(StreamLogging(), StreamCallContext(deps.CallAuth)),
	)
	// The domain services are registered here as they are implemented.
	if err := RegisterServices(ctx, srv, deps); err != nil {
		return err
	}

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	reflection.Register(srv) // lets grpcurl and Bruno explore the surface

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		return err
	}
	go serveHealthHTTP(ctx, cfg.HTTPPort)

	go func() {
		<-ctx.Done()
		log.Info("SIGTERM received, draining connections")
		srv.GracefulStop()
	}()

	log.Info("gRPC listening", "addr", lis.Addr().String())
	return srv.Serve(lis)
}

// RunWorker consumes events and builds projections.
//
// It is the single-instance process, so it is the one that migrates and seeds
// before doing anything else (ADR-0024 §2–3). A failed migration stops the
// worker here, loudly, instead of a consumer failing on a missing column.
func RunWorker(ctx context.Context, cfg *config.Config) error {
	log := logging.From(ctx)
	schema := postgres.NewSchema(cfg.DatabaseURL)
	if err := schema.Up(ctx); err != nil {
		return err
	}
	if v, err := schema.Current(ctx); err == nil {
		log.Info("schema up to date", "version", v)
	}
	deps, cleanup, err := Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := postgres.Seed(ctx, deps.Pool, cfg.SeedProfile); err != nil {
		return err
	}

	// The outbox's relay runs alongside the worker: it is what takes the event
	// written in the transaction to the broker (ADR-0014).
	relay := postgres.NewRelay(deps.Pool, deps.Bus, 100)
	go func() {
		if err := relay.Run(ctx, cfg.RelayInterval); err != nil && ctx.Err() == nil {
			log.Error("relay parou", "error", err)
		}
	}()
	log.Info("outbox relay active", "intervalo", cfg.RelayInterval.String())

	if err := RegisterProjections(ctx, deps); err != nil {
		return err
	}
	if info, err := eventbus.StreamInfo(ctx, deps.Bus.(*eventbus.NATS)); err == nil {
		log.Info("JetStream pronto", "info", info)
	}

	go serveHealthHTTP(ctx, cfg.HTTPPort)
	<-ctx.Done()
	return ctx.Err()
}

// RunSched runs the periodic tasks.
func RunSched(ctx context.Context, cfg *config.Config) error {
	log := logging.From(ctx)
	deps, cleanup, err := Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	go serveHealthHTTP(ctx, cfg.HTTPPort)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	log.Info("scheduler active")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			RunScheduledTasks(ctx, deps)
		}
	}
}

// RunLauncher provisions sandboxes on the execution cluster.
func RunLauncher(ctx context.Context, cfg *config.Config) error {
	log := logging.From(ctx)
	deps, cleanup, err := Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	// It subscribes to the sandbox commands. The connection is always
	// outbound: the client's cluster opens no inbound port at all.
	if err := RegisterLauncher(ctx, deps); err != nil {
		return err
	}
	go serveHealthHTTP(ctx, cfg.HTTPPort)
	log.Info("launcher active, waiting for commands")
	<-ctx.Done()
	return ctx.Err()
}

// serveHealthHTTP is the plain-HTTP side of a process: the health check, and —
// in the process that hosts the projects' root repositories — git's smart HTTP
// and the platform-side API (ADR-0021). One port, because the sandboxes'
// egress allowlist names one address.
func serveHealthHTTP(ctx context.Context, port int, extra ...struct {
	prefix string
	h      http.Handler
}) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	for _, e := range extra {
		if e.h != nil {
			mux.Handle(e.prefix, e.h)
		}
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	_ = srv.ListenAndServe()
}
