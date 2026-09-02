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

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/platform/config"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// RunServe brings up the domain's gRPC server.
func RunServe(ctx context.Context, cfg *config.Config) error {
	log := logging.From(ctx)
	deps, cleanup, err := Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(UnaryLogging(), UnaryCallContext(), UnaryRecover()),
		grpc.ChainStreamInterceptor(StreamLogging(), StreamCallContext()),
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

	log.Info("gRPC ouvindo", "addr", lis.Addr().String())
	return srv.Serve(lis)
}

// RunWorker consumes events and builds projections.
func RunWorker(ctx context.Context, cfg *config.Config) error {
	log := logging.From(ctx)
	deps, cleanup, err := Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	// The outbox's relay runs alongside the worker: it is what takes the event
	// written in the transaction to the broker (ADR-0019).
	relay := postgres.NewRelay(deps.Pool, deps.Bus, 100)
	go func() {
		if err := relay.Run(ctx, cfg.RelayInterval); err != nil && ctx.Err() == nil {
			log.Error("relay parou", "error", err)
		}
	}()
	log.Info("relay do outbox ativo", "intervalo", cfg.RelayInterval.String())

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
	log.Info("scheduler ativo")
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
	log.Info("launcher ativo, aguardando comandos")
	<-ctx.Done()
	return ctx.Err()
}

func serveHealthHTTP(ctx context.Context, port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	_ = srv.ListenAndServe()
}
