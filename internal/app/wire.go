// Package app is the COMPOSITION ROOT: where the ports receive their adapters.
//
// It is the only place in the system that knows both ends. The domain knows only
// the ports; the adapters know only their technology. The choice happens here,
// by configuration — never through a conditional scattered across the code
// (ADR-0001).
package app

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/adapter/identity"
	"github.com/Digital-Business-One/dop-core/internal/adapter/mailer"
	"github.com/Digital-Business-One/dop-core/internal/adapter/objectstore"
	"github.com/Digital-Business-One/dop-core/internal/adapter/sandbox"
	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/config"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
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
	Mailer   ports.Mailer
	Cfg      *config.Config
}

func Build(ctx context.Context, cfg *config.Config) (*Deps, func(), error) {
	log := logging.From(ctx)

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "falha ao abrir o pool do Postgres")
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

	// The execution substrate is also a port with two REAL adapters (ADR-0001):
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
		})
	}

	// The email channel is also a port with two real adapters: SendGrid for
	// whoever uses a managed service, SMTP for self-hosted — the same GCP/OKD
	// pairing as the others. With an empty SMTP_ADDR the adapter enters the
	// LOCAL DRY RUN: it prints instead of sending, and the whole pipeline stays
	// testable without spending a send or polluting anybody's inbox.
	var correio ports.Mailer
	switch cfg.MailBackend {
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

	// Ponte STORAGE_EMULATOR_HOST ↔ FIREBASE_STORAGE_EMULATOR_HOST (ADR-0020).
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

	deps := &Deps{Pool: pool, Bus: bus, Secrets: secrets, Objects: objects,
		Identity: idp, Launcher: launcher, Mailer: correio, Cfg: cfg}
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
