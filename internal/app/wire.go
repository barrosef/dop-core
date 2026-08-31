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
	var secrets ports.SecretStore
	switch cfg.SecretBackend {
	case "memory":
		secrets = secretstore.NewMemory()
	default: // k8s — usado no local e em self-hosted; não há emulador do Secret Manager
		// Sem Client: quem sabe que o apiserver usa a CA do cluster é o
		// adaptador, não o composition root. O campo existe para teste.
		secrets = secretstore.NewK8s(secretstore.K8sConfig{
			APIServer: cfg.K8sAPIServer,
			Token:     cfg.K8sToken,
			Namespace: cfg.K8sNamespace,
		})
	}

	// Ponte STORAGE_EMULATOR_HOST ↔ FIREBASE_STORAGE_EMULATOR_HOST (ADR-0020).
	// Sem ela, upload local vai para o bucket REAL.
	if ep := objectstore.ResolveEmulatorHost(); ep != "" {
		log.Info("armazenamento apontado para o emulador", "endpoint", ep)
	}
	objects := objectstore.NewGCS(objectstore.GCSConfig{Endpoint: cfg.StorageEndpoint})

	idp := identity.NewFirebase(cfg.FirebaseProject)
	if idp.UsingEmulator() {
		log.Info("identidade apontada para o emulador do Firebase Auth")
	}

	deps := &Deps{Pool: pool, Bus: bus, Secrets: secrets, Objects: objects, Identity: idp, Cfg: cfg}
	cleanup := func() {
		_ = bus.Close()
		pool.Close()
	}
	return deps, cleanup, nil
}
