package attention

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service serve a caixa. É deliberadamente pequeno: a inteligência está na
// tabela de impacto e no mapa de eventos, ambos testáveis sem banco.
type Service struct {
	repo    Repository
	watcher Watcher
	clock   ports.Clock
}

func NewService(repo Repository, watcher Watcher, clock ports.Clock) *Service {
	if repo == nil || watcher == nil {
		panic("attention.NewService: repositório e watcher são obrigatórios")
	}
	if clock == nil {
		panic("attention.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	return &Service{repo: repo, watcher: watcher, clock: clock}
}

// DefaultLimit existe porque a caixa é para DECIDIR, não para navegar: se
// alguém tem mais de cem pendências abertas, o problema não é a paginação.
const DefaultLimit = 100

// List devolve a caixa da conta ativa, já ordenada.
//
// A prioridade é calculada na LEITURA, não gravada: ela depende da idade, e
// idade muda sozinha. Prioridade materializada ficaria velha em silêncio.
func (s *Service) List(ctx context.Context, demandID string, includeResolved bool, limit int) ([]Item, int, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > DefaultLimit {
		limit = DefaultLimit
	}
	itens, err := s.repo.List(ctx, accountID, demandID, includeResolved, limit)
	if err != nil {
		return nil, 0, err
	}
	Sort(itens, s.clock.Now())

	total, err := s.repo.OpenTotal(ctx, accountID)
	if err != nil {
		return nil, 0, err
	}
	return itens, total, nil
}

// Watch acompanha a caixa ao vivo, traduzindo evento em mudança.
//
// Reusa o fan-out do domínio de evento pela porta `Watcher`: uma segunda
// implementação de fan-out seria uma segunda chance de errar isolamento entre
// contas.
func (s *Service) Watch(ctx context.Context, sinceEventID string, emit func(change, eventID string, it Item) error) error {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return err
	}
	if emit == nil {
		return errs.Invalid("Watch exige um emissor")
	}
	return s.watcher.Watch(ctx, sinceEventID, nil, nil, func(e Event) error {
		d := Apply(e)
		switch {
		case d.Open != nil:
			return emit("opened", e.ID, *d.Open)
		case d.Close != nil:
			// O fechamento não carrega o item inteiro — quem fecha conhece o
			// ALVO, não o id. O cliente casa pelo alvo, como a projeção faz.
			return emit("resolved", e.ID, Item{
				AccountID: e.AccountID, Kind: d.Close.Kind,
				TargetKind: d.Close.TargetKind, TargetID: d.Close.TargetID,
			})
		}
		return nil
	})
}
