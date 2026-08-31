// Package config resolve a configuração do processo a partir do ambiente.
// É o composition root que escolhe QUAL adaptador cada porta recebe (ADR-0001).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Mode string

	GRPCPort int
	HTTPPort int // health e métricas

	DatabaseURL string
	NATSUrl     string

	// Escolha de adaptadores por ambiente.
	SecretBackend string // k8s | gcp | memory
	ObjectBackend string // gcs (real ou emulado)

	K8sAPIServer string
	K8sNamespace string
	K8sToken     string

	StorageBucket   string
	StorageEndpoint string

	FirebaseProject string

	RelayInterval time.Duration
	LogLevel      string
}

func Load(mode string) (*Config, error) {
	c := &Config{
		Mode:            mode,
		GRPCPort:        envInt("GRPC_PORT", 9090),
		HTTPPort:        envInt("HTTP_PORT", 9091),
		DatabaseURL:     env("DATABASE_URL", "postgres://dop:dop-local-dev@postgres.dop-local.svc:5432/dop?sslmode=disable"),
		NATSUrl:         env("NATS_URL", "nats://nats.dop-local.svc:4222"),
		SecretBackend:   env("SECRET_BACKEND", "k8s"),
		ObjectBackend:   env("OBJECT_BACKEND", "gcs"),
		K8sAPIServer:    env("KUBERNETES_API", "https://kubernetes.default.svc"),
		K8sNamespace:    env("SECRET_NAMESPACE", "dop-local"),
		StorageBucket:   env("STORAGE_BUCKET", "dop-local.firebasestorage.app"),
		StorageEndpoint: env("STORAGE_EMULATOR_HOST", ""),
		FirebaseProject: env("FIREBASE_PROJECT", "dop-local"),
		RelayInterval:   time.Duration(envInt("RELAY_INTERVAL_MS", 500)) * time.Millisecond,
		LogLevel:        env("LOG_LEVEL", "info"),
	}
	// Token da service account, quando rodando dentro do cluster.
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		c.K8sToken = string(b)
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL é obrigatória")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
