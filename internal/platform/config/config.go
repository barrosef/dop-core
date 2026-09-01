// Package config resolve a configuração do processo a partir do ambiente.
// É o composition root que escolhe QUAL adaptador cada porta recebe (ADR-0001).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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

	// ── provedor de código (ADR-0008, porta delivery.GitProvider) ──
	//
	// Repare no que NÃO está aqui: o TOKEN. Token de provedor é credencial de
	// RECURSO (ADR-0013) — vive no cofre, atrás de ports.SecretStore, é
	// diferente por conta e por recurso, e por isso não pode ser variável de
	// ambiente do processo. O que está aqui é o AJUSTE do adaptador: endereço,
	// prazos e política de merge, que são iguais para toda a instalação.
	//
	// GitBackend escolhe o adaptador padrão da instalação. Ele é só o default:
	// a conexão de verdade é montada por recurso, porque o recurso é que diz
	// qual provedor e qual credencial (ADR-0013).
	GitBackend string // github | gitlab
	// GitHubAPI e GitLabAPI apontam para o serviço público OU para uma
	// instalação self-hosted (GitHub Enterprise, GitLab CE/EE). Existirem os
	// dois ao mesmo tempo é proposital: uma conta pode ter recursos nos dois.
	GitHubAPI string
	// GitHubGraphQL é separado da REST de propósito: no GitHub Enterprise a
	// URL do GraphQL é /api/graphql, e não a base REST com sufixo.
	GitHubGraphQL string
	GitLabAPI     string
	// GitTimeout é o prazo de UMA chamada ao provedor.
	GitTimeout time.Duration
	// GitRebaseTimeout é o prazo do rebase INTEIRO, que é assíncrono nos dois
	// provedores e que a porta promete entregar já resolvido (garantia 9).
	// Separado de GitTimeout porque são grandezas diferentes: uma chamada que
	// demora 30s está quebrada; um rebase que demora 30s é normal.
	GitRebaseTimeout time.Duration
	// GitMergeMethod é política do fluxo git (ADR-0013, recurso `git_flow`),
	// não vocabulário do domínio — a fila da ADR-0008 precisa que o merge
	// aconteça, não que ele aconteça de um jeito. Fica no adaptador.
	GitMergeMethod string // merge | squash | rebase

	// ── comunicação (ADR-0025) ──
	// MailBackend escolhe o adaptador da porta Mailer. `smtp` é o caminho do
	// self-hosted; `sendgrid`, o do SaaS. Os dois passam pela mesma suíte de
	// contrato.
	MailBackend string // sendgrid | smtp
	// MailFrom/MailFromName são o remetente da INSTALAÇÃO. Não é vocabulário do
	// domínio: quem avisa é a plataforma, e o endereço dela muda por instalação.
	MailFrom     string
	MailFromName string
	// SendGridAPI existe para apontar o adaptador para outro host — o serviço
	// tem endpoint regional na UE, e a suíte de contrato aponta para um duplo.
	// A CHAVE não está aqui de propósito: ela é credencial, mora no cofre
	// (ADR-0023), e chega ao adaptador já resolvida pelo composition root.
	SendGridAPI string
	// SendGridTemplates é tipo → `template_id`, lido de SENDGRID_TEMPLATE_<TIPO>.
	// É a metade da resolução que muda por instalação; a outra — QUE tipos
	// existem — é compilada no adaptador, onde a suíte de contrato a exercita.
	SendGridTemplates map[string]string
	// SMTPAddr vazio liga o ENSAIO LOCAL: o adaptador imprime em vez de enviar.
	// É o mesmo gesto da chave vazia no SendGrid, e é ele que faz o ambiente de
	// desenvolvimento não precisar de servidor de e-mail nenhum.
	SMTPAddr string
	SMTPUser string
	// SendGridAPIKey e SMTPPassword são credenciais DA INSTALAÇÃO, e por isso
	// vêm do ambiente — como o DATABASE_URL, entregues pelo Secret do
	// Kubernetes que o Deployment monta.
	//
	// NÃO vão para o cofre, e a distinção importa: o cofre existe para
	// credencial de CLIENTE (integração de conta, ADR-0013), com isolamento
	// por conta no nome do segredo. Credencial da instalação não pertence a
	// conta nenhuma — guardá-la lá inventaria uma conta fictícia para ser dona
	// dela, e afrouxaria a garantia 5 da porta para acomodar a exceção.
	SendGridAPIKey string
	SMTPPassword   string
	SMTPStartTLS   bool
	// CockpitBaseURL é a base dos links do e-mail. Vazio faz o aviso sair sem
	// link — degradação declarada: link quebrado custa mais confiança que
	// ausência de link.
	CockpitBaseURL string
	// DigestDelay é o atraso do aviso de atenção (ADR-0025). Configurável
	// porque 15 minutos é palpite informado, não medição: o valor certo para
	// uma equipe de plantão não é o de quem olha a caixa de manhã.
	DigestDelay time.Duration

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
		GitBackend:    env("GIT_BACKEND", "github"),
		GitHubAPI:     env("GITHUB_API", "https://api.github.com"),
		GitHubGraphQL: env("GITHUB_GRAPHQL", "https://api.github.com/graphql"),
		GitLabAPI:     env("GITLAB_API", "https://gitlab.com/api/v4"),
		GitTimeout: time.Duration(
			envInt("GIT_TIMEOUT_SECONDS", 30)) * time.Second,
		GitRebaseTimeout: time.Duration(
			envInt("GIT_REBASE_TIMEOUT_SECONDS", 180)) * time.Second,
		GitMergeMethod: env("GIT_MERGE_METHOD", "merge"),
		MailBackend:    env("MAIL_BACKEND", "smtp"),
		MailFrom:       env("MAIL_FROM", "noreply@dop.local"),
		MailFromName:   env("MAIL_FROM_NAME", "DOP"),
		SendGridAPI:    env("SENDGRID_API", "https://api.sendgrid.com"),
		SMTPAddr:       env("SMTP_ADDR", ""),
		SMTPUser:       env("SMTP_USER", ""),
		SendGridAPIKey: env("SENDGRID_API_KEY", ""),
		SMTPPassword:   env("SMTP_PASSWORD", ""),
		SMTPStartTLS:   env("SMTP_STARTTLS", "") == "true",
		CockpitBaseURL: env("COCKPIT_BASE_URL", ""),
		DigestDelay: time.Duration(
			envInt("DIGEST_DELAY_SECONDS", 900)) * time.Second,
		RelayInterval: time.Duration(envInt("RELAY_INTERVAL_MS", 500)) * time.Millisecond,
		LogLevel:      env("LOG_LEVEL", "info"),
	}
	// Os ids de template vêm por variável POR TIPO, e não numa string com
	// separador: uma lista achatada erra em silêncio quando alguém troca a
	// ordem, e o sintoma seria o convite chegar com o layout do resumo.
	//
	// A varredura é por PREFIXO, e não por uma lista de tipos conhecidos, para
	// que este pacote não precise importar o domínio de notificação só para
	// saber que tipos existem — quem confere se todos têm template é a suíte de
	// contrato do adaptador, que é onde essa checagem tem dente.
	c.SendGridTemplates = map[string]string{}
	const prefixo = "SENDGRID_TEMPLATE_"
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || !strings.HasPrefix(kv, prefixo) || i+1 >= len(kv) {
			continue
		}
		c.SendGridTemplates[strings.ToLower(kv[len(prefixo):i])] = kv[i+1:]
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
	// Mesmo raciocínio, no provedor de código: backend desconhecido faria o
	// composition root cair no default e a instalação inteira falar com o
	// provedor errado — descoberto na primeira tentativa de abrir PR, com o
	// processo verde há semanas.
	switch c.GitBackend {
	case "github", "gitlab":
	default:
		return nil, fmt.Errorf("GIT_BACKEND desconhecido: %q (use github ou gitlab)", c.GitBackend)
	}
	// Método de merge fora do vocabulário viraria uma recusa do provedor no
	// momento exato em que a fila da ADR-0008 tenta integrar — o pior momento
	// possível para descobrir um erro de digitação em variável de ambiente.
	switch c.GitMergeMethod {
	case "merge", "squash", "rebase":
	default:
		return nil, fmt.Errorf("GIT_MERGE_METHOD desconhecido: %q (use merge, squash ou rebase)", c.GitMergeMethod)
	}
	// Mesmo raciocínio no canal de e-mail: backend desconhecido cairia no
	// default e a instalação inteira mandaria aviso pelo caminho errado —
	// descoberto no primeiro convite que não chega, com o processo verde há
	// semanas.
	switch c.MailBackend {
	case "sendgrid", "smtp":
	default:
		return nil, fmt.Errorf("MAIL_BACKEND desconhecido: %q (use sendgrid ou smtp)", c.MailBackend)
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
