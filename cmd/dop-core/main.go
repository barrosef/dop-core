// dop-core — the platform's core: domain, state, transactions and events.
//
// ONE image, FOUR modes (ADR-0012). One artifact, one pipeline:
//
//	serve     gRPC server — the domain's surface
//	worker    consumes NATS events and builds projections
//	sched     periodic tasks: PR polling, sandbox suspension, partitions
//	launcher  a daemon per execution cluster: provisions sandboxes
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/barrosef/dop-core/internal/app"
	"github.com/barrosef/dop-core/internal/platform/config"
	"github.com/barrosef/dop-core/internal/platform/logging"
	"github.com/barrosef/dop-core/internal/platform/tracing"
)

// version is stamped by the build (-ldflags "-X main.version=…"); "dev" when
// it is not.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]

	log := logging.New(mode)
	cfg, err := config.Load(mode)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown: SIGTERM starts the wind-down; Kubernetes gives the
	// grace period for the connections to drain.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx = logging.Into(ctx, log)

	// Traces (ADR-0024 §4): the exporter is configuration; with none, only the
	// propagator is installed and the trace context still crosses the process.
	shutdownTracing, err := tracing.Setup(ctx, tracing.Config{
		Backend: cfg.TraceBackend, OTLPEndpoint: cfg.TraceOTLPEndpoint,
		GCPProject: cfg.SecretProject, SampleRatio: cfg.TraceSampleRatio,
		Service: "dop-core", Mode: mode, Version: version,
	})
	if err != nil {
		log.Error("invalid tracing configuration", "error", err)
		os.Exit(1)
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flush)
	}()

	log.Info("starting", "grpc_port", cfg.GRPCPort, "secret_backend", cfg.SecretBackend,
		"trace_backend", cfg.TraceBackend)

	var runErr error
	switch mode {
	case "serve":
		runErr = app.RunServe(ctx, cfg)
	case "worker":
		runErr = app.RunWorker(ctx, cfg)
	case "sched":
		runErr = app.RunSched(ctx, cfg)
	case "launcher":
		runErr = app.RunLauncher(ctx, cfg)
	case "collector":
		// The only mode that does NOT run in the platform's namespace: it runs
		// beside the agent, in the sandbox's pod (P-23 phase 1).
		runErr = app.RunCollector(ctx, cfg)
	case "migrate":
		// Applies the embedded migrations (ADR-0024 §2). The worker does this
		// at boot; this mode exists for an operator: `status`, and the
		// one-time `baseline` of a database migrated by hand.
		runErr = app.RunMigrate(ctx, cfg, os.Args[2:])
	case "seed":
		runErr = app.RunSeed(ctx, cfg)
	case "version":
		fmt.Println("dop-core " + version)
		return
	default:
		usage()
		os.Exit(2)
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		log.Error("shut down with an error", "error", runErr)
		os.Exit(1)
	}
	log.Info("shut down")
}

func usage() {
	fmt.Fprintf(os.Stderr, `dop-core — the platform's core

usage: dop-core <mode>

modes:
  serve      the domain's gRPC server
  worker     event consumers and projections
  sched      periodic tasks
  launcher   sandbox provisioning on the execution cluster
  collector  follows the agent's session file and pushes its consumption —
             the only mode that runs INSIDE a sandbox, beside the agent
  migrate    [up|status|baseline <version>] — the embedded migrations
  seed       the embedded seeds (root + DOP_SEED_PROFILE)
  version    version
`)
}
