// Package app is the COMPOSITION ROOT: where the ports receive their adapters.
//
// It is the only place in the system that knows both ends. The domain knows only
// the ports; the adapters know only their technology. The choice happens here,
// by configuration — never through a conditional scattered across the code
// (ADR-0001).
package app

import (
	"context"
	"crypto/rand"
	"github.com/barrosef/dop-core/internal/adapter/clock"
	"github.com/barrosef/dop-core/internal/adapter/postgres"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/adapter/eventbus"
	"github.com/barrosef/dop-core/internal/adapter/identity"
	"github.com/barrosef/dop-core/internal/adapter/mailer"
	"github.com/barrosef/dop-core/internal/adapter/objectstore"
	"github.com/barrosef/dop-core/internal/adapter/projectrepo"
	"github.com/barrosef/dop-core/internal/adapter/sandbox"
	"github.com/barrosef/dop-core/internal/adapter/secretstore"
	"github.com/barrosef/dop-core/internal/adapter/smser"
	"github.com/barrosef/dop-core/internal/adapter/verification"
	"github.com/barrosef/dop-core/internal/domain/agentmetrics"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/config"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
)

// Deps gathers everything the use cases need — always as a PORT, never as an
// adapter's concrete type.
type Deps struct {
	Pool     *pgxpool.Pool
	Bus      ports.EventBus
	Secrets  ports.SecretStore
	Objects  ports.ObjectStore
	Identity ports.IdentityProvider
	Launcher ports.SandboxLauncher
	// Runner is where a verification runs and where an application runs at all
	// (ADR-0030). It is a DIFFERENT port from Launcher on purpose: the bench and
	// the runner have opposite promises — one preserves the work, the other
	// starts from nothing.
	Runner ports.VerificationRunner
	Mailer ports.Mailer
	SMS    ports.SMSer
	Cfg    *config.Config
	// Repos is the projects' root repositories (ADR-0028). GitHTTP and GitAPI
	// are non-nil only when this process HOSTS them (GitBackend=local): the
	// smart-HTTP handler the sandboxes clone from, and the platform-side API a
	// remote core would use.
	Repos   ports.ProjectRepository
	GitHTTP http.Handler
	GitAPI  http.Handler
	// CallAuth authenticates the core's OWN callers (ADR-0029). Nil means the
	// old behaviour — the metadata taken on faith — which only the tests that
	// are about something else should want.
	CallAuth *callAuth
}

func Build(ctx context.Context, cfg *config.Config) (*Deps, func(), error) {
	log := logging.From(ctx)

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "failed to open the Postgres pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "Postgres unreachable")
	}

	bus, err := eventbus.NewNATS(ctx, cfg.NATSUrl)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}

	// ── choosing the adapters by configuration ──
	// closers accumulates what has to be closed in the process's cleanup.
	var closers []func() error

	var secrets ports.SecretStore
	switch cfg.SecretBackend {
	case "memory":
		secrets = secretstore.NewMemory()
	case "gcp":
		// The port's guarantee 1 (read-after-write) is NOT deliverable on real
		// GCP with this design — see ADR-0021. The adapter confirms by version
		// number, which is strong, and then waits for the `latest` alias to
		// catch up; if it does not converge, it refuses with KindUnavailable
		// instead of returning "it does not exist" for a credential that was
		// just written.
		gcp, err := secretstore.NewGCP(ctx, secretstore.GCPConfig{
			ProjectID:   cfg.SecretProject,
			Endpoint:    cfg.SecretEndpoint,
			Propagation: cfg.SecretPropagation,
		})
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		closers = append(closers, gcp.Close)
		secrets = gcp
	default: // k8s — used locally and in self-hosted; there is no Secret Manager emulator
		// No Client: the one who knows the apiserver uses the cluster's CA is
		// the adapter, not the composition root. The field exists for tests.
		secrets = secretstore.NewK8s(secretstore.K8sConfig{
			APIServer: cfg.K8sAPIServer,
			Token:     cfg.K8sToken,
			Namespace: cfg.K8sNamespace,
		})
	}

	// The executor is also a port with two REAL adapters (ADR-0001):
	// Docker for local development with no cluster, k8s for the execution
	// cluster. Both pass the same contract suite.
	var launcher ports.SandboxLauncher
	switch cfg.SandboxBackend {
	case "docker":
		launcher = sandbox.NewDocker(sandbox.DockerConfig{Socket: cfg.DockerSocket})
	default:
		launcher = sandbox.NewK8s(sandbox.K8sConfig{
			APIServer:     cfg.K8sAPIServer,
			Token:         cfg.K8sToken,
			WorkspaceSize: cfg.WorkspaceSize,
			StorageClass:  cfg.StorageClass,
			// The sandbox's egress allowlist names this namespace as its only
			// in-cluster destination — the BFF and the git server.
			PlatformNamespace: cfg.K8sNamespace,
		})
	}

	// The verification runner follows the SAME executor choice as the sandbox,
	// and for the same reason: a cluster deployment has no host daemon to talk
	// to, and a laptop has no cluster.
	var runner ports.VerificationRunner
	switch cfg.SandboxBackend {
	case "docker":
		runner = verification.NewDocker(verification.DockerConfig{
			Socket:    cfg.DockerSocket,
			CacheRoot: cfg.RunnerCacheRoot,
		})
	default:
		runner = verification.NewK8s(verification.K8sConfig{
			APIServer:         cfg.K8sAPIServer,
			Token:             cfg.K8sToken,
			CacheSize:         cfg.RunnerCacheSize,
			StorageClass:      cfg.StorageClass,
			PlatformNamespace: cfg.K8sNamespace,
		})
	}

	// The email channel is also a port with two real adapters: SendGrid for
	// whoever uses a managed service, SMTP for self-hosted — the same GCP/OKD
	// pairing as the others. With an empty SMTP_ADDR the adapter enters the
	// LOCAL DRY RUN: it prints instead of sending, and the whole pipeline stays
	// testable without spending a send or polluting anybody's inbox.
	var correio ports.Mailer
	switch cfg.MailBackend {
	case "onesignal":
		correio = mailer.NewOneSignal(mailer.OneSignalConfig{
			AppID:      cfg.OneSignalAppID,
			APIKey:     cfg.OneSignalAPIKey,
			AuthScheme: cfg.OneSignalAuthScheme,
			BaseURL:    cfg.OneSignalAPI,
			From:       cfg.MailFrom,
			FromName:   cfg.MailFromName,
			ReplyTo:    cfg.MailReplyTo,
		})
	case "sendgrid":
		correio = mailer.NewSendGrid(mailer.SendGridConfig{
			APIKey:    cfg.SendGridAPIKey,
			BaseURL:   cfg.SendGridAPI,
			From:      cfg.MailFrom,
			FromName:  cfg.MailFromName,
			Templates: cfg.SendGridTemplates,
		})
	default:
		correio = mailer.NewSMTP(mailer.SMTPConfig{
			Addr:     cfg.SMTPAddr,
			Username: cfg.SMTPUser,
			Password: cfg.SMTPPassword,
			From:     cfg.MailFrom,
			FromName: cfg.MailFromName,
			StartTLS: cfg.SMTPStartTLS,
		})
	}

	// The SMS channel, the port ADR-0025 foresaw and the second factor brought
	// into being. Two real adapters for ADR-0001's same reason — and with no
	// credential either of them REHEARSES: locally there is no gateway, so the
	// path is exercised and the delivery is not (P-35).
	var texto ports.SMSer
	switch cfg.SMSBackend {
	case "zenvia":
		texto = smser.NewZenvia(smser.ZenviaConfig{
			BaseURL: cfg.ZenviaAPI,
			Token:   cfg.ZenviaToken,
			From:    cfg.SMSFrom,
		})
	default:
		texto = smser.NewTwilio(smser.TwilioConfig{
			BaseURL:    cfg.TwilioAPI,
			AccountSID: cfg.TwilioAccountSID,
			AuthToken:  cfg.TwilioAuthToken,
			From:       cfg.SMSFrom,
		})
	}

	// The STORAGE_EMULATOR_HOST ↔ FIREBASE_STORAGE_EMULATOR_HOST bridge (ADR-0020).
	// Without it, a local upload goes to the REAL bucket.
	if ep := objectstore.ResolveEmulatorHost(); ep != "" {
		log.Info("storage pointed at the emulator", "endpoint", ep)
	}
	objects := objectstore.NewGCS(objectstore.GCSConfig{Endpoint: cfg.StorageEndpoint})

	// Identity is also a port with two real adapters (ADR-0001): Firebase for
	// GCP, generic OIDC (Keycloak, Dex, Authentik) for self-hosted. Both pass
	// the same contract suite.
	var idp ports.IdentityProvider
	switch cfg.IdentityBackend {
	case "oidc":
		idp = identity.NewOIDC(identity.OIDCConfig{
			Issuer:         cfg.OIDCIssuer,
			Audience:       cfg.OIDCAudience,
			ClockSkew:      cfg.OIDCClockSkew,
			KeysMinRefresh: cfg.OIDCKeysMinRefresh,
		})
		log.Info("identity through OIDC", "issuer", cfg.OIDCIssuer)
	default:
		fb := identity.NewFirebase(cfg.FirebaseProject)
		// The emulator issues `alg: none`, so the SIGNATURE verification is
		// skipped in that mode — and only in it. It is worth recording at boot:
		// it is the difference between the local environment and production, and
		// it was exactly this kind of silent difference that has let an
		// authentication bypass through before.
		if fb.UsingEmulator() {
			log.Warn("identity on the EMULATOR: token signature is NOT verified",
				"project", cfg.FirebaseProject)
		}
		idp = fb
	}

	// The projects' root repositories: two adapters, one contract suite
	// (ADR-0001). Local hosts them here; Remote reaches a server elsewhere.
	var repos ports.ProjectRepository
	var gitHTTP, gitAPI http.Handler
	switch cfg.ProjectRepoBackend {
	case "remote":
		repos = projectrepo.NewRemote(cfg.ProjectRepoServerURL, "/git", cfg.ProjectRepoAdminKey)
	default:
		key := []byte(cfg.ProjectRepoKey)
		if len(key) == 0 {
			// A generated key is fine for a single process on a laptop; a
			// deployment sets PROJECT_REPO_KEY, or a restart would orphan every token.
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, nil, err
			}
			log.Warn("PROJECT_REPO_KEY not set: minting sandbox tokens with a key that dies with this process")
		}
		srv, err := projectrepo.NewServer(projectrepo.Config{Root: cfg.ProjectRepoRoot, Key: key, Prefix: "/git"})
		if err != nil {
			return nil, nil, err
		}
		srv.WithSecrets(secrets)
		repos = projectrepo.NewLocal(srv, cfg.ProjectRepoBaseURL)
		gitHTTP = srv
		gitAPI = projectrepo.NewAPI(srv, cfg.ProjectRepoAdminKey, cfg.ProjectRepoBaseURL)
	}

	// Who may assert who the actor is (ADR-0029). One key per caller: a
	// compromised component forges only its own calls.
	keys := map[string][]byte{}
	if k := strings.TrimSpace(cfg.CallAuthKeyBFF); k != "" {
		keys["bff"] = []byte(k)
	}
	if k := strings.TrimSpace(cfg.CallAuthKeyCollector); k != "" {
		keys[agentmetrics.CollectorCaller] = []byte(k)
	}
	if len(keys) == 0 && cfg.CallAuthMode == "strict" {
		return nil, nil, errs.Invalid(
			"CALL_AUTH_MODE=strict with no key: every call would arrive with no actor")
	}
	auth := newCallAuth(cfg.CallAuthMode, keys, idp,
		postgres.NewIdentityRepo(pool), clock.NewSystem())
	log.Info("caller authentication", "mode", cfg.CallAuthMode, "callers", len(keys))

	deps := &Deps{Pool: pool, Bus: bus, Secrets: secrets, Objects: objects,
		Identity: idp, Launcher: launcher, Runner: runner, Mailer: correio, SMS: texto, Cfg: cfg,
		Repos: repos, GitHTTP: gitHTTP, GitAPI: gitAPI, CallAuth: auth}
	cleanup := func() {
		// Adapters that open a connection of their own register the close here.
		// The port has no Close — closing is the concern of whoever ASSEMBLES,
		// not of the domain, which must not know there is a connection in the
		// middle.
		for _, fechar := range closers {
			_ = fechar()
		}
		_ = bus.Close()
		pool.Close()
	}
	return deps, cleanup, nil
}
