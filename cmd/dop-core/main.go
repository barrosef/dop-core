// dop-core — núcleo da plataforma: domínio, estado, transações e eventos.
//
// UMA imagem, QUATRO modos (ADR-0016). Um artefato, um pipeline:
//
//	serve     servidor gRPC — a superfície do domínio
//	worker    consome eventos do NATS e constrói projeções
//	sched     tarefas periódicas: polling de PRs, suspensão de sandbox, partições
//	launcher  daemon por cluster de execução: provisiona sandboxes
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
		log.Error("configuração inválida", "error", err)
		os.Exit(1)
	}

	// Encerramento gracioso: SIGTERM inicia o desligamento; o Kubernetes dá o
	// grace period para as conexões drenarem.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx = logging.Into(ctx, log)

	log.Info("iniciando", "grpc_port", cfg.GRPCPort, "secret_backend", cfg.SecretBackend)

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
		log.Error("encerrado com erro", "error", runErr)
		os.Exit(1)
	}
	log.Info("encerrado")
}

func usage() {
	fmt.Fprintf(os.Stderr, `dop-core — núcleo da plataforma

uso: dop-core <modo>

modos:
  serve      servidor gRPC do domínio
  worker     consumidores de evento e projeções
  sched      tarefas periódicas
  launcher   provisionamento de sandboxes no cluster de execução
  version    versão
`)
}
