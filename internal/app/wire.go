// Package app é o COMPOSITION ROOT: onde as portas recebem seus adaptadores.
//
// É o único lugar do sistema que conhece as duas pontas. O domínio conhece
// apenas as portas; os adaptadores conhecem apenas sua tecnologia. A escolha
// acontece aqui, por configuração — nunca por condicional espalhada no código
// (ADR-0001).
package app

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/adapter/identity"
	"github.com/Digital-Business-One/dop-core/internal/adapter/objectstore"
	"github.com/Digital-Business-One/dop-core/internal/adapter/sandbox"
	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/config"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// Deps reúne tudo que os casos de uso precisam — sempre como PORTA, nunca como
// tipo concreto de adaptador.
type Deps struct {
	Pool     *pgxpool.Pool
	Bus      ports.EventBus
	Secrets  ports.SecretStore
	Objects  ports.ObjectStore
	Identity ports.IdentityProvider
	Launcher ports.SandboxLauncher
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
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "Postgres inacessível")
	}

	bus, err := eventbus.NewNATS(ctx, cfg.NATSUrl)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}

	// ── escolha dos adaptadores por configuração ──
	// closers acumula o que precisa ser fechado no cleanup do processo.
	var closers []func() error

	var secrets ports.SecretStore
	switch cfg.SecretBackend {
	case "memory":
		secrets = secretstore.NewMemory()
	case "gcp":
		// A garantia 1 da porta (leitura-após-escrita) NÃO é cumprível no GCP
		// real com este desenho — ver ADR-0021. O adaptador confirma por número
		// de versão, que é forte, e depois espera o alias `latest` alcançar;
		// se não convergir, recusa com KindUnavailable em vez de devolver
		// "não existe" para uma credencial que acabou de ser gravada.
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
	default: // k8s — usado no local e em self-hosted; não há emulador do Secret Manager
		// Sem Client: quem sabe que o apiserver usa a CA do cluster é o
		// adaptador, não o composition root. O campo existe para teste.
		secrets = secretstore.NewK8s(secretstore.K8sConfig{
			APIServer: cfg.K8sAPIServer,
			Token:     cfg.K8sToken,
			Namespace: cfg.K8sNamespace,
		})
	}

	// O substrato de execução também é porta com dois adaptadores REAIS
	// (ADR-0001): Docker para o desenvolvimento local sem cluster, k8s para o
	// cluster de execução. Os dois passam pela mesma suíte de contrato.
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

	// Ponte STORAGE_EMULATOR_HOST ↔ FIREBASE_STORAGE_EMULATOR_HOST (ADR-0020).
	// Sem ela, upload local vai para o bucket REAL.
	if ep := objectstore.ResolveEmulatorHost(); ep != "" {
		log.Info("armazenamento apontado para o emulador", "endpoint", ep)
	}
	objects := objectstore.NewGCS(objectstore.GCSConfig{Endpoint: cfg.StorageEndpoint})

	// Identidade também é porta com dois adaptadores reais (ADR-0001): Firebase
	// para GCP, OIDC genérico (Keycloak, Dex, Authentik) para self-hosted. Os
	// dois passam pela mesma suíte de contrato.
	var idp ports.IdentityProvider
	switch cfg.IdentityBackend {
	case "oidc":
		idp = identity.NewOIDC(identity.OIDCConfig{
			Issuer:         cfg.OIDCIssuer,
			Audience:       cfg.OIDCAudience,
			ClockSkew:      cfg.OIDCClockSkew,
			KeysMinRefresh: cfg.OIDCKeysMinRefresh,
		})
		log.Info("identidade por OIDC", "emissor", cfg.OIDCIssuer)
	default:
		fb := identity.NewFirebase(cfg.FirebaseProject)
		// O emulador emite `alg: none`, então a verificação de ASSINATURA é
		// pulada nesse modo — e só nele. Vale registrar no boot: é a diferença
		// entre o ambiente local e a produção, e foi exatamente esse tipo de
		// diferença silenciosa que já deixou passar um bypass de autenticação.
		if fb.UsingEmulator() {
			log.Warn("identidade no EMULADOR: assinatura de token NÃO é verificada",
				"projeto", cfg.FirebaseProject)
		}
		idp = fb
	}

	deps := &Deps{Pool: pool, Bus: bus, Secrets: secrets, Objects: objects,
		Identity: idp, Launcher: launcher, Cfg: cfg}
	cleanup := func() {
		// Adaptadores que abrem conexão própria registram o fechamento aqui.
		// A porta não tem Close — fechar é preocupação de quem MONTA, não do
		// domínio, que não deve saber que existe conexão no meio.
		for _, fechar := range closers {
			_ = fechar()
		}
		_ = bus.Close()
		pool.Close()
	}
	return deps, cleanup, nil
}
