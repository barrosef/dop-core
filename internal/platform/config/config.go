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

	// SecretProject é o projeto GCP que hospeda os segredos. Só usado com
	// SECRET_BACKEND=gcp.
	SecretProject string
	// SecretEndpoint aponta o adaptador do GCP para o EMULADOR ("host:porta").
	// Vazio = Secret Manager de verdade, com credencial padrão do ambiente.
	//
	// Não existe variável oficial de emulador para o Secret Manager — o Google
	// publica STORAGE_EMULATOR_HOST e PUBSUB_EMULATOR_HOST, mas nenhuma aqui, e
	// a biblioteca oficial não lê nenhuma. Esta é NOSSA, e por isso precisa ser
	// passada explicitamente ao adaptador.
	SecretEndpoint string
	// SecretPropagation é quanto o Put espera o alias `latest` enxergar a
	// versão recém-gravada antes de desistir. No emulador é instantâneo; no
	// GCP real o alias é eventualmente consistente e esta espera é o que
	// separa "leitura-após-escrita" de promessa vazia (ver o adaptador).
	SecretPropagation time.Duration
	// SandboxBackend escolhe o substrato de execução. Docker é o caminho do
	// desenvolvimento local sem cluster; k8s é o do cluster de execução.
	SandboxBackend string // k8s | docker
	DockerSocket   string
	WorkspaceSize  string
	StorageClass   string

	// DevboxImage roda como usuário arbitrário NÃO-root desde a primeira
	// imagem: OKD recusa root por SCC, e isso é requisito de imagem.
	DevboxImage   string
	IngressDomain string

	K8sAPIServer string
	K8sNamespace string
	K8sToken     string

	StorageBucket   string
	StorageEndpoint string

	FirebaseProject string

	// IdentityBackend escolhe o provedor de identidade (ADR-0001). firebase é o
	// caminho do GCP; oidc é o de cluster self-hosted — Keycloak, Dex,
	// Authentik — e não depende de fornecedor nenhum.
	IdentityBackend string // firebase | oidc
	// OIDCIssuer é o `iss` EXATO dos tokens, e a base da descoberta em
	// /.well-known/openid-configuration.
	OIDCIssuer string
	// OIDCAudience é o client_id registrado no emissor. Vazio DESLIGA a
	// checagem de audiência, e desligar aceita token emitido para outra
	// aplicação do mesmo realm — é escolha, não default de conveniência.
	OIDCAudience string
	// Ajuste do adaptador, não vocabulário do domínio (ver ports.IdentityProvider).
	OIDCClockSkew      time.Duration
	OIDCKeysMinRefresh time.Duration

	RelayInterval time.Duration
	LogLevel      string
}

func Load(mode string) (*Config, error) {
	c := &Config{
		Mode:           mode,
		GRPCPort:       envInt("GRPC_PORT", 9090),
		HTTPPort:       envInt("HTTP_PORT", 9091),
		DatabaseURL:    env("DATABASE_URL", "postgres://dop:dop-local-dev@postgres.dop-local.svc:5432/dop?sslmode=disable"),
		NATSUrl:        env("NATS_URL", "nats://nats.dop-local.svc:4222"),
		SecretBackend:  env("SECRET_BACKEND", "k8s"),
		ObjectBackend:  env("OBJECT_BACKEND", "gcs"),
		SecretProject:  env("SECRET_PROJECT", ""),
		SecretEndpoint: env("SECRET_MANAGER_EMULATOR_HOST", ""),
		SecretPropagation: time.Duration(
			envInt("SECRET_PROPAGATION_SECONDS", 30)) * time.Second,
		SandboxBackend:  env("SANDBOX_BACKEND", "k8s"),
		DockerSocket:    env("DOCKER_SOCKET", "/var/run/docker.sock"),
		WorkspaceSize:   env("SANDBOX_WORKSPACE_SIZE", "10Gi"),
		StorageClass:    env("SANDBOX_STORAGE_CLASS", ""),
		DevboxImage:     env("DEVBOX_IMAGE", "dop-registry:5000/dop/devbox:0.1.0"),
		IngressDomain:   env("INGRESS_DOMAIN", "localtest.me:8080"),
		K8sAPIServer:    env("KUBERNETES_API", "https://kubernetes.default.svc"),
		K8sNamespace:    env("SECRET_NAMESPACE", "dop-local"),
		StorageBucket:   env("STORAGE_BUCKET", "dop-local.firebasestorage.app"),
		StorageEndpoint: env("STORAGE_EMULATOR_HOST", ""),
		FirebaseProject: env("FIREBASE_PROJECT", "dop-local"),
		IdentityBackend: env("IDENTITY_BACKEND", "firebase"),
		OIDCIssuer:      env("OIDC_ISSUER", ""),
		OIDCAudience:    env("OIDC_AUDIENCE", ""),
		OIDCClockSkew:   time.Duration(envInt("OIDC_CLOCK_SKEW_SECONDS", 60)) * time.Second,
		OIDCKeysMinRefresh: time.Duration(
			envInt("OIDC_KEYS_MIN_REFRESH_SECONDS", 30)) * time.Second,
		RelayInterval: time.Duration(envInt("RELAY_INTERVAL_MS", 500)) * time.Millisecond,
		LogLevel:      env("LOG_LEVEL", "info"),
	}
	// Token da service account, quando rodando dentro do cluster.
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		c.K8sToken = string(b)
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL é obrigatória")
	}
	// Falha no BOOT, e não no primeiro login: emissor vazio faria o adaptador
	// aceitar token de qualquer origem, e a descoberta apontaria para lugar
	// nenhum. É o tipo de erro de configuração que precisa aparecer no deploy.
	if c.IdentityBackend == "oidc" && c.OIDCIssuer == "" {
		return nil, fmt.Errorf("OIDC_ISSUER é obrigatória quando IDENTITY_BACKEND=oidc")
	}
	// Mesmo motivo: sem projeto o adaptador montaria nomes "projects//secrets/…"
	// e só quebraria na primeira credencial gravada, com o processo já verde.
	if c.SecretBackend == "gcp" && c.SecretProject == "" {
		return nil, fmt.Errorf("SECRET_PROJECT é obrigatória quando SECRET_BACKEND=gcp")
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
