// Package config resolves the process configuration from the environment.
// It is the composition root that chooses WHICH adapter each port receives
// (ADR-0001).
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
	HTTPPort int // health and metrics

	DatabaseURL string
	NATSUrl     string

	// Adapter choice, per environment.
	SecretBackend string // k8s | gcp | memory
	ObjectBackend string // gcs (real or emulated)

	// SecretProject is the GCP project that hosts the secrets. Only used with
	// SECRET_BACKEND=gcp.
	SecretProject string
	// SecretEndpoint points the GCP adapter at the EMULATOR ("host:port").
	// Empty = the real Secret Manager, with the environment's default
	// credential.
	//
	// There is no official emulator variable for Secret Manager — Google
	// publishes STORAGE_EMULATOR_HOST and PUBSUB_EMULATOR_HOST, but none here,
	// and the official library reads none. This one is OURS, which is why it has
	// to be passed to the adapter explicitly.
	SecretEndpoint string
	// SecretPropagation is how long Put waits for the `latest` alias to see the
	// version just written before giving up. In the emulator it is instant; on
	// real GCP the alias is eventually consistent, and this wait is what
	// separates "read-after-write" from an empty promise (see the adapter).
	SecretPropagation time.Duration
	// SandboxBackend chooses the executor. Docker is the path for
	// local development with no cluster; k8s is the execution cluster's.
	SandboxBackend string // k8s | docker
	DockerSocket   string
	WorkspaceSize  string
	StorageClass   string

	// RunnerImage is the PLATFORM's image for a verification (ADR-0023): fat on
	// purpose, with the toolchains, cached on the nodes. It is never an image of
	// the customer's project — that one is built from source inside this one and
	// pushed nowhere.
	RunnerImage string
	// RunnerCacheSize is the account cache volume's size, and RunnerCacheRoot is
	// where that cache lives on the host under the Docker adapter. Without the
	// cache, building from source every time is slower than the registry
	// sequence the decision removed.
	RunnerCacheSize string
	RunnerCacheRoot string

	// DevboxImage runs as an arbitrary NON-root user from the very first image:
	// OKD refuses root through its SCC, and that is an image requirement.
	DevboxImage   string
	IngressDomain string

	K8sAPIServer string
	K8sNamespace string
	K8sToken     string

	// The projects' root repositories (ADR-0021). ProjectRepoBackend picks the adapter:
	// `local` hosts the repositories in THIS process, under ProjectRepoRoot, and serves
	// them on the health/HTTP port; `remote` talks to a server in another
	// process at ProjectRepoServerURL. ProjectRepoBaseURL is the address a SANDBOX reaches the
	// server at — a fact about the network, not about the repositories.
	ProjectRepoBackend   string // local | remote
	ProjectRepoRoot      string
	ProjectRepoKey       string // the HMAC key the sandbox tokens are minted with
	ProjectRepoAdminKey  string // the platform-side API's key; no sandbox holds it
	ProjectRepoBaseURL   string
	ProjectRepoServerURL string

	StorageBucket   string
	StorageEndpoint string

	FirebaseProject string

	// IdentityBackend chooses the identity provider (ADR-0001). firebase is the
	// GCP path; oidc is the self-hosted cluster path — Keycloak, Dex, Authentik
	// — and depends on no vendor at all.
	IdentityBackend string // firebase | oidc
	// OIDCIssuer is the tokens' EXACT `iss`, and the base for discovery at
	// /.well-known/openid-configuration.
	OIDCIssuer string
	// OIDCAudience is the client_id registered with the issuer. Empty TURNS OFF
	// the audience check, and turning it off accepts a token issued to another
	// application in the same realm — a choice, not a convenience default.
	OIDCAudience string
	// Adapter tuning, not domain vocabulary (see ports.IdentityProvider).
	OIDCClockSkew      time.Duration
	OIDCKeysMinRefresh time.Duration

	// ── code provider (ADR-0005, the delivery.GitProvider port) ──
	//
	// Note what is NOT here: the TOKEN. A provider token is a RESOURCE
	// credential (ADR-0009) — it lives in the vault, behind ports.SecretStore,
	// it differs per account and per resource, and so it cannot be a process
	// environment variable. What is here is the adapter's TUNING: address,
	// deadlines and merge policy, which are the same for the whole installation.
	//
	// GitBackend chooses the installation's default adapter. It is only a
	// default: the real connection is assembled per resource, because it is the
	// resource that says which provider and which credential (ADR-0009).
	// CallAuthMode is how the core treats its own callers (ADR-0022):
	// `strict` refuses to fill in an actor without a verified signature,
	// `permissive` warns and lets it through, `off` is the old behaviour.
	// The default is permissive and the DEPLOYMENT is strict — see the
	// comment in internal/app/callauth.go.
	CallAuthMode string
	// CallAuthKeyBFF is the key the edge signs its assertions with.
	CallAuthKeyBFF string
	// ── the collector (P-23 phase 1) ────────────────────────────────────────
	//
	// It runs BESIDE the agent, in the sandbox's pod, so its configuration is
	// per sandbox and comes from the launcher — not from the platform's
	// ConfigMap.
	CollectorSessionDir string        // where Claude Code writes its sessions
	CollectorAccountID  string        // whose demand this is
	CollectorDemandID   string        // and which demand
	CollectorProjectID  string        //
	CollectorInterval   time.Duration // how often to look for what is new
	// CoreTarget is the core's address as the SANDBOX reaches it.
	CoreTarget string
	// CollectorImage is what the launcher raises beside the agent. It is the
	// CORE's own image — the collector is a mode of this binary.
	CollectorImage string

	// CallAuthKeyCollector is the metrics collector's — a container beside the
	// agent in the sandbox's pod. A key of its own so that a leak there forges
	// only what the collector may do, which is write telemetry.
	CallAuthKeyCollector string

	GitBackend string // github | gitlab
	// GitHubAPI and GitLabAPI point at the public service OR at a self-hosted
	// installation (GitHub Enterprise, GitLab CE/EE). Having both at once is
	// deliberate: one account may have resources in both.
	GitHubAPI string
	// GitHubGraphQL is separate from the REST base on purpose: on GitHub
	// Enterprise the GraphQL URL is /api/graphql, not the REST base plus a
	// suffix.
	GitHubGraphQL string
	GitLabAPI     string
	// GitTimeout is the deadline for ONE call to the provider.
	GitTimeout time.Duration
	// GitRebaseTimeout is the deadline for the WHOLE rebase, which is
	// asynchronous in both providers and which the port promises to deliver
	// already settled (guarantee 9). Separate from GitTimeout because they are
	// different magnitudes: a call taking 30s is broken; a rebase taking 30s is
	// normal.
	GitRebaseTimeout time.Duration
	// GitMergeMethod is git-flow policy (ADR-0009, the `git_flow` resource), not
	// domain vocabulary — the ADR-0005 queue needs the merge to happen, not to
	// happen in a particular way. It stays in the adapter.
	GitMergeMethod string // merge | squash | rebase

	// ── communication (ADR-0018) ──
	// MailBackend chooses the Mailer port's adapter. `smtp` is the self-hosted
	// path; `sendgrid` the SaaS one. Both pass the same contract suite.
	MailBackend string // onesignal | sendgrid | smtp
	// MailReplyTo is where a person's answer lands when MailFrom is a noreply
	// address. Empty leaves it to the provider's default.
	MailReplyTo string
	// MailFrom/MailFromName are the INSTALLATION's sender. Not domain
	// vocabulary: the platform is who notifies, and its address changes per
	// installation.
	MailFrom     string
	MailFromName string
	// SendGridAPI exists to point the adapter at another host — the service has
	// a regional endpoint in the EU, and the contract suite points at a double.
	// The KEY is deliberately not here: it is a credential, it lives in the
	// vault (ADR-0016), and it reaches the adapter already resolved by the
	// composition root.
	SendGridAPI string
	// SendGridTemplates is kind → `template_id`, read from
	// SENDGRID_TEMPLATE_<KIND>. It is the half of resolution that changes per
	// installation; the other half — WHICH kinds exist — is compiled into the
	// adapter, where the contract suite exercises it.
	SendGridTemplates map[string]string
	// An empty SMTPAddr turns on the LOCAL REHEARSAL: the adapter prints instead
	// of sending. It is the same gesture as an empty key in SendGrid, and it is
	// what lets the development environment need no mail server at all.
	SMTPAddr string
	SMTPUser string
	// SendGridAPIKey and SMTPPassword are the INSTALLATION's credentials, which
	// is why they come from the environment — like DATABASE_URL, delivered by
	// the Kubernetes Secret the Deployment mounts.
	//
	// They do NOT go to the vault, and the distinction matters: the vault exists
	// for CUSTOMER credentials (an account's integration, ADR-0009), with
	// per-account isolation in the secret's name. An installation credential
	// belongs to no account — keeping it there would invent a fictitious account
	// to own it, and would loosen the port's guarantee 5 to accommodate the
	// exception.
	SendGridAPIKey string
	// OneSignal is the multi-channel provider (e-mail here; SMS and push are
	// their own ports). An empty key OR an empty app id turns on the LOCAL DRY
	// RUN — both are needed to send anything, so either being absent means the
	// same thing.
	OneSignalAppID string
	// OneSignalAuthScheme prefixes the Authorization header. OneSignal moved
	// from "Basic" to "Key" as it rotated its credential format and both are
	// alive in the wild; getting it wrong produces a 401 indistinguishable from
	// a bad key, which is a bad afternoon.
	OneSignalAuthScheme string
	OneSignalAPI        string
	OneSignalAPIKey     string
	SMTPPassword        string
	SMTPStartTLS        bool
	// SMSBackend chooses the SMSer port's adapter (ADR-0020 §4). An empty
	// credential turns on the LOCAL REHEARSAL, the same gesture as SMTP's: the
	// adapter prints the message instead of sending it, which is the local
	// environment's only mode — there is no SMS emulator (P-35).
	SMSBackend string // twilio | zenvia
	SMSFrom    string
	// TwilioAccountSID/TwilioAuthToken and ZenviaToken are the INSTALLATION's
	// credentials. They are read here and go into the adapter's constructor,
	// which captures them in a CLOSURE — never as a field, so a %+v of the
	// adapter cannot print them.
	TwilioAPI        string
	TwilioAccountSID string
	TwilioAuthToken  string
	ZenviaAPI        string
	ZenviaToken      string
	// CockpitBaseURL is the base of the links in emails. Empty makes the notice
	// go out with no link — a declared degradation: a broken link costs more
	// trust than a missing one.
	CockpitBaseURL string
	// DigestDelay is the attention notice's delay (ADR-0018). Configurable
	// because 15 minutes is an informed guess, not a measurement: the right
	// value for an on-call team is not the one for somebody who checks the box
	// in the morning.
	DigestDelay time.Duration

	RelayInterval time.Duration
	LogLevel      string

	// SeedProfile names the seed subdirectory the worker applies after the
	// root seeds (ADR-0024 §3): "local" for development, empty in production.
	SeedProfile string
	// SchemaWait is how long `serve` waits for the worker to bring the
	// database up to the binary's schema before giving up (ADR-0024 §2).
	SchemaWait time.Duration
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
		SandboxBackend:       env("SANDBOX_BACKEND", "k8s"),
		DockerSocket:         env("DOCKER_SOCKET", "/var/run/docker.sock"),
		WorkspaceSize:        env("SANDBOX_WORKSPACE_SIZE", "10Gi"),
		StorageClass:         env("SANDBOX_STORAGE_CLASS", ""),
		RunnerImage:          env("RUNNER_IMAGE", "dop-registry:5000/dop/runner:0.1.0"),
		RunnerCacheSize:      env("RUNNER_CACHE_SIZE", "10Gi"),
		RunnerCacheRoot:      env("RUNNER_CACHE_ROOT", "/var/lib/dop/cache"),
		DevboxImage:          env("DEVBOX_IMAGE", "dop-registry:5000/dop/devbox:0.1.0"),
		IngressDomain:        env("INGRESS_DOMAIN", "localtest.me:8080"),
		K8sAPIServer:         env("KUBERNETES_API", "https://kubernetes.default.svc"),
		ProjectRepoBackend:   env("PROJECT_REPO_BACKEND", "local"),
		ProjectRepoRoot:      env("PROJECT_REPO_ROOT", "/var/lib/dop/git"),
		ProjectRepoKey:       env("PROJECT_REPO_KEY", ""),
		ProjectRepoAdminKey:  env("PROJECT_REPO_ADMIN_KEY", ""),
		ProjectRepoBaseURL:   env("PROJECT_REPO_BASE_URL", "http://dop-core.dop-local.svc:9091"),
		ProjectRepoServerURL: env("PROJECT_REPO_SERVER_URL", "http://dop-core.dop-local.svc:9091"),
		K8sNamespace:         env("SECRET_NAMESPACE", "dop-local"),
		StorageBucket:        env("STORAGE_BUCKET", "dop-local.firebasestorage.app"),
		StorageEndpoint:      env("STORAGE_EMULATOR_HOST", ""),
		FirebaseProject:      env("FIREBASE_PROJECT", "dop-local"),
		IdentityBackend:      env("IDENTITY_BACKEND", "firebase"),
		OIDCIssuer:           env("OIDC_ISSUER", ""),
		OIDCAudience:         env("OIDC_AUDIENCE", ""),
		OIDCClockSkew:        time.Duration(envInt("OIDC_CLOCK_SKEW_SECONDS", 60)) * time.Second,
		OIDCKeysMinRefresh: time.Duration(
			envInt("OIDC_KEYS_MIN_REFRESH_SECONDS", 30)) * time.Second,
		CallAuthMode:         env("CALL_AUTH_MODE", "permissive"),
		CallAuthKeyBFF:       env("CALL_AUTH_KEY_BFF", ""),
		CallAuthKeyCollector: env("CALL_AUTH_KEY_COLLECTOR", ""),
		CollectorSessionDir:  env("COLLECTOR_SESSION_DIR", "/sessions"),
		CollectorAccountID:   env("COLLECTOR_ACCOUNT_ID", ""),
		CollectorDemandID:    env("COLLECTOR_DEMAND_ID", ""),
		CollectorProjectID:   env("COLLECTOR_PROJECT_ID", ""),
		CollectorInterval: time.Duration(
			envInt("COLLECTOR_INTERVAL_SECONDS", 5)) * time.Second,
		CoreTarget:    env("CORE_TARGET", "dop-core.dop-local.svc:9090"),
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
		MailReplyTo:    env("MAIL_REPLY_TO", ""),
		SendGridAPI:    env("SENDGRID_API", "https://api.sendgrid.com"),
		SMTPAddr:       env("SMTP_ADDR", ""),
		SMTPUser:       env("SMTP_USER", ""),
		SendGridAPIKey: env("SENDGRID_API_KEY", ""),

		OneSignalAppID:      env("ONESIGNAL_APP_ID", ""),
		OneSignalAPI:        env("ONESIGNAL_API", "https://api.onesignal.com"),
		OneSignalAuthScheme: env("ONESIGNAL_AUTH_SCHEME", "Key"),
		OneSignalAPIKey:     env("ONESIGNAL_API_KEY", ""),
		SMTPPassword:        env("SMTP_PASSWORD", ""),
		SMTPStartTLS:        env("SMTP_STARTTLS", "") == "true",
		SMSBackend:          env("SMS_BACKEND", "twilio"),
		SMSFrom:             env("SMS_FROM", ""),
		TwilioAPI:           env("TWILIO_API", ""),
		TwilioAccountSID:    env("TWILIO_ACCOUNT_SID", ""),
		TwilioAuthToken:     env("TWILIO_AUTH_TOKEN", ""),
		ZenviaAPI:           env("ZENVIA_API", ""),
		ZenviaToken:         env("ZENVIA_TOKEN", ""),
		CockpitBaseURL:      env("COCKPIT_BASE_URL", ""),
		DigestDelay: time.Duration(
			envInt("DIGEST_DELAY_SECONDS", 900)) * time.Second,
		RelayInterval: time.Duration(envInt("RELAY_INTERVAL_MS", 500)) * time.Millisecond,
		LogLevel:      env("LOG_LEVEL", "info"),
		SeedProfile:   env("DOP_SEED_PROFILE", ""),
		SchemaWait:    time.Duration(envInt("SCHEMA_WAIT_SECONDS", 90)) * time.Second,
	}
	// Template ids come through one variable PER KIND, not in a
	// separator-joined string: a flattened list fails silently when somebody
	// changes the order, and the symptom would be an invite arriving with the
	// digest's layout.
	//
	// The scan is by PREFIX, not against a list of known kinds, so this package
	// does not have to import the notification domain just to learn which kinds
	// exist — checking that all of them have a template is the adapter's
	// contract suite's job, which is where that check has teeth.
	c.SendGridTemplates = map[string]string{}
	const prefix = "SENDGRID_TEMPLATE_"
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || !strings.HasPrefix(kv, prefix) || i+1 >= len(kv) {
			continue
		}
		c.SendGridTemplates[strings.ToLower(kv[len(prefix):i])] = kv[i+1:]
	}
	// The service account token, when running inside the cluster.
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		c.K8sToken = string(b)
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	// Fail at BOOT, not at the first login: an empty issuer would make the
	// adapter accept a token from any origin, and discovery would point
	// nowhere. It is the kind of configuration error that has to surface at
	// deploy time.
	if c.IdentityBackend == "oidc" && c.OIDCIssuer == "" {
		return nil, fmt.Errorf("OIDC_ISSUER is required when IDENTITY_BACKEND=oidc")
	}
	// Same reason: with no project the adapter would build names like
	// "projects//secrets/…" and would only break on the first credential
	// written, with the process already green.
	if c.SecretBackend == "gcp" && c.SecretProject == "" {
		return nil, fmt.Errorf("SECRET_PROJECT is required when SECRET_BACKEND=gcp")
	}
	// Same reasoning for the code provider: an unknown backend would make the
	// composition root fall back to the default and the whole installation talk
	// to the wrong provider — discovered on the first attempt to open a PR, with
	// the process green for weeks.
	switch c.GitBackend {
	case "github", "gitlab":
	default:
		return nil, fmt.Errorf("unknown GIT_BACKEND: %q (use github or gitlab)", c.GitBackend)
	}
	// A merge method outside the vocabulary would become a provider refusal at
	// the exact moment the ADR-0005 queue tries to integrate — the worst
	// possible time to discover a typo in an environment variable.
	switch c.GitMergeMethod {
	case "merge", "squash", "rebase":
	default:
		return nil, fmt.Errorf("unknown GIT_MERGE_METHOD: %q (use merge, squash or rebase)", c.GitMergeMethod)
	}
	// Same reasoning in the email channel: an unknown backend would fall back to
	// the default and the whole installation would send notices down the wrong
	// path — discovered on the first invite that never arrives, with the process
	// green for weeks.
	switch c.MailBackend {
	case "onesignal", "sendgrid", "smtp":
	default:
		return nil, fmt.Errorf("unknown MAIL_BACKEND: %q (use onesignal, sendgrid or smtp)", c.MailBackend)
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
