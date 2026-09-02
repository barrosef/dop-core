// dop-core — the platform's core: domain, state, transactions and events.
//
// ONE image, FOUR modes (ADR-0016). One artifact, one pipeline:
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

	"github.com/Digital-Business-One/dop-core/internal/app"
	"github.com/Digital-Business-One/dop-core/internal/platform/config"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

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

	log.Info("starting", "grpc_port", cfg.GRPCPort, "secret_backend", cfg.SecretBackend)

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
	case "version":
		fmt.Println("dop-core dev")
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
  version    version
`)
}
