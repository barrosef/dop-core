// Outbox transacional — o mecanismo que dá atomicidade SEM 2PC (ADR-0019).
//
// A regra, em uma frase: toda mudança de estado grava, na MESMA transação, o
// estado novo e o evento. Commit ⇒ atômico por construção. Nunca "gravei mas
// não publiquei".
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type Outbox struct{ pool *pgxpool.Pool }

func NewOutbox(pool *pgxpool.Pool) *Outbox { return &Outbox{pool: pool} }

// Emit grava evento + outbox dentro de uma transação JÁ ABERTA pelo caso de uso.
// Recebe pgx.Tx de propósito: se aceitasse o pool, alguém acabaria publicando
// fora da transação — que é exatamente a janela de perda que o outbox fecha.
func Emit(ctx context.Context, tx pgx.Tx, e ports.Event) error {
	call, _ := ctxutil.From(ctx)
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if len(e.Payload) == 0 {
		e.Payload = []byte(`{}`)
	}

	// Nem todo evento tem conta: `user.ensured` ocorre no primeiro login, antes
	// de a conta pessoal existir. String vazia não é UUID — vai NULL.
	var accountID any
	if e.AccountID != "" {
		accountID = e.AccountID
	}
	var actorID any
	if call.ActorID != "" {
		actorID = call.ActorID
	}

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO events (account_id, aggregate, aggregate_id, type, payload,
		                    actor_kind, actor_id, request_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		accountID, e.Aggregate, e.AggregateID, e.Type, e.Payload,
		string(call.ActorKind), actorID, call.RequestID, e.OccurredAt,
	).Scan(&id)
	if err != nil {
		return errs.Wrap(errs.KindInternal, err, "falha ao gravar evento")
	}

	env, _ := json.Marshal(map[string]any{
		"id": id, "account_id": e.AccountID, "aggregate": e.Aggregate,
		"aggregate_id": e.AggregateID, "type": e.Type,
		"payload": json.RawMessage(e.Payload), "occurred_at": e.OccurredAt,
	})

	// Mesma transação. É o ponto inteiro do padrão.
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (event_id, occurred_at, subject, payload)
		VALUES ($1,$2,$3,$4)`,
		id, e.OccurredAt, Subject(e.Type), env,
	); err != nil {
		return errs.Wrap(errs.KindInternal, err, "falha ao enfileirar evento no outbox")
	}
	return nil
}

// Subject deriva o assunto NATS do tipo do evento: demand.stage.advanced
// vira dop.demand.stage.advanced.
func Subject(eventType string) string {
	return "dop." + strings.TrimPrefix(eventType, "dop.")
}

// Relay lê o outbox e publica no broker, marcando o publicado.
// Entrega ao-menos-uma-vez: consumidores DEVEM ser idempotentes.
type Relay struct {
	pool  *pgxpool.Pool
	bus   ports.EventBus
	batch int
}

func NewRelay(pool *pgxpool.Pool, bus ports.EventBus, batch int) *Relay {
	if batch <= 0 {
		batch = 100
	}
	return &Relay{pool: pool, bus: bus, batch: batch}
}

// Drain publica um lote de pendentes. Devolve quantos foram publicados.
//
// FOR UPDATE SKIP LOCKED permite várias réplicas do relay sem publicar o mesmo
// evento duas vezes — e sem uma travar a outra.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, errs.Wrap(errs.KindUnavailable, err, "falha ao abrir transação do relay")
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT event_id, subject, payload, occurred_at
		  FROM outbox
		 WHERE published_at IS NULL
		 ORDER BY occurred_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, r.batch)
	if err != nil {
		return 0, errs.Wrap(errs.KindInternal, err, "falha ao ler o outbox")
	}

	type pending struct {
		id      string
		subject string
		payload []byte
		at      time.Time
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.subject, &p.payload, &p.at); err != nil {
			rows.Close()
			return 0, errs.Wrap(errs.KindInternal, err, "linha do outbox ilegível")
		}
		batch = append(batch, p)
	}
	rows.Close()
	if len(batch) == 0 {
		return 0, tx.Commit(ctx)
	}

	published := make([]string, 0, len(batch))
	for _, p := range batch {
		ev := ports.Event{ID: p.id, Type: p.subject, Payload: p.payload, OccurredAt: p.at}
		if err := r.bus.Publish(ctx, ev); err != nil {
			// Falha de publicação não perde o evento: fica pendente e o
			// próximo ciclo tenta de novo.
			_, _ = tx.Exec(ctx, `
				UPDATE outbox SET attempts = attempts + 1, last_error = $2
				 WHERE event_id = $1`, p.id, err.Error())
			continue
		}
		published = append(published, p.id)
	}

	if len(published) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE outbox SET published_at = now() WHERE event_id = ANY($1)`, published,
		); err != nil {
			return 0, errs.Wrap(errs.KindInternal, err, "falha ao marcar publicados")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errs.Wrap(errs.KindInternal, err, "falha ao confirmar o relay")
	}
	return len(published), nil
}

// Run mantém o relay drenando até o contexto encerrar.
func (r *Relay) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			for {
				n, err := r.Drain(ctx)
				if err != nil {
					return fmt.Errorf("relay: %w", err)
				}
				if n < r.batch {
					break // esvaziou o lote; espera o próximo tick
				}
			}
		}
	}
}
