package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/execution"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// ExecutionRepo implementa execution.Repository. É o ÚNICO lugar com SQL de
// sandbox — o domínio nunca vê uma query.
//
// Três coisas valem por toda a leitura deste arquivo:
//
//   - account_id entra em TODA cláusula WHERE. Isolamento multi-tenant é
//     constraint, não confiança no chamador;
//   - toda mudança de estado grava o evento na MESMA transação, por InTx +
//     Emit. Commit ⇒ estado e evento, ou nenhum dos dois (ADR-0019);
//   - a irreversibilidade da destruição NÃO é responsabilidade deste arquivo:
//     ela é trigger no banco (migração 0010). Aqui só se traduz a exceção da
//     trigger em erro de domínio, o que o Translate já faz por P0001.
type ExecutionRepo struct{ pool *pgxpool.Pool }

func NewExecutionRepo(pool *pgxpool.Pool) *ExecutionRepo { return &ExecutionRepo{pool: pool} }

var _ execution.Repository = (*ExecutionRepo)(nil)

const sandboxCols = `id, account_id, demand_id, state, tier, namespace, endpoints,
	COALESCE(idempotency_key,''), last_active_at, suspended_at, destroyed_at,
	COALESCE(created_by::text,''), created_at, updated_at`

func scanSandbox(row pgx.Row) (*execution.Sandbox, error) {
	var s execution.Sandbox
	var state, tier string
	var endpoints []byte
	var suspended, destroyed *time.Time
	if err := row.Scan(&s.ID, &s.AccountID, &s.DemandID, &state, &tier, &s.Namespace,
		&endpoints, &s.IdempotencyKey, &s.LastActiveAt, &suspended, &destroyed,
		&s.CreatedBy, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.State = execution.State(state)
	s.Tier = ports.IsolationTier(tier)
	if suspended != nil {
		s.SuspendedAt = *suspended
	}
	if destroyed != nil {
		s.DestroyedAt = *destroyed
	}
	_ = json.Unmarshal(endpoints, &s.Endpoints)
	return &s, nil
}

// ByID devolve (nil, nil) quando não há linha: se a ausência é erro, quem
// decide é o domínio.
func (r *ExecutionRepo) ByID(ctx context.Context, accountID, id string) (*execution.Sandbox, error) {
	s, err := scanSandbox(r.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE id = $1 AND account_id = $2`, id, accountID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "sandbox")
	}
	return s, nil
}

func (r *ExecutionRepo) ByIdempotencyKey(ctx context.Context, accountID, key string) (*execution.Sandbox, error) {
	if key == "" {
		return nil, nil
	}
	s, err := scanSandbox(r.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes
		  WHERE account_id = $1 AND idempotency_key = $2`, accountID, key))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "sandbox")
	}
	return s, nil
}

// LiveByDemand: vivo é tudo que não foi destruído. O índice parcial
// sandboxes_demanda_viva_uniq garante que existe no máximo um.
func (r *ExecutionRepo) LiveByDemand(ctx context.Context, accountID, demandID string) (*execution.Sandbox, error) {
	s, err := scanSandbox(r.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes
		  WHERE account_id = $1 AND demand_id = $2 AND state <> 'destroyed'`,
		accountID, demandID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "sandbox")
	}
	return s, nil
}

// Create grava a INTENÇÃO de provisionar e emite o evento na mesma transação.
//
// O evento sai antes de o substrato responder de propósito: é ele que permite
// reconciliar um sandbox meio subido depois de uma queda. Emitir só no fim
// deixaria uma microVM viva sem nenhum registro de que alguém a pediu.
func (r *ExecutionRepo) Create(ctx context.Context, s *execution.Sandbox) (*execution.Sandbox, error) {
	var saved *execution.Sandbox
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO sandboxes (account_id, demand_id, state, tier, namespace,
			                       idempotency_key, last_active_at, created_by)
			VALUES ($1, $2, $3::sandbox_state, $4::isolation_tier, $5,
			        NULLIF($6,''), $7, NULLIF($8,'')::uuid)
			RETURNING `+sandboxCols,
			s.AccountID, s.DemandID, string(s.State), string(s.Tier), s.Namespace,
			s.IdempotencyKey, s.LastActiveAt, s.CreatedBy)
		var err error
		saved, err = scanSandbox(row)
		if err != nil {
			// Chave repetida e "essa demanda já tem sandbox" viram 409, não
			// 500: são erros do cliente, com resposta útil.
			return Translate(err, "sandbox")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "sandbox", AggregateID: saved.ID,
			Type: "dop.sandbox.provisioning",
			Payload: mustJSON(map[string]any{
				"demand_id": saved.DemandID,
				"tier":      saved.Tier,
				"namespace": saved.Namespace,
			}),
		})
	})
	return saved, err
}

// MarkProvisioned registra o que o substrato ENTREGOU.
//
// O tier é gravado de novo, com o valor devolvido pelo launcher, porque é o que
// o cliente de fato recebeu — e o contrato promete que ele vê o que recebeu, não
// o que pediu. Quando os dois divergem, o serviço já descartou o sandbox antes
// de chegar aqui.
func (r *ExecutionRepo) MarkProvisioned(ctx context.Context, accountID, id string, tier ports.IsolationTier, endpoints []execution.Endpoint) (*execution.Sandbox, error) {
	var saved *execution.Sandbox
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE sandboxes
			   SET state          = 'active',
			       tier           = $3::isolation_tier,
			       endpoints      = $4,
			       last_active_at = now(),
			       updated_at     = now()
			 WHERE id = $1 AND account_id = $2
			RETURNING `+sandboxCols,
			id, accountID, string(tier), mustJSON(endpoints))
		var err error
		saved, err = scanSandbox(row)
		if err != nil {
			return Translate(err, "sandbox")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "sandbox", AggregateID: saved.ID,
			Type: "dop.sandbox.provisioned",
			Payload: mustJSON(map[string]any{
				"demand_id": saved.DemandID,
				"tier":      saved.Tier,
				"namespace": saved.Namespace,
				"endpoints": saved.Endpoints,
			}),
		})
	})
	return saved, err
}

// Transition aplica a mudança de estado e emite o evento correspondente.
//
// O evento carrega `preserva_trabalho`, e não só o estado novo, porque é essa a
// pergunta que a auditoria vai fazer daqui a seis meses: "quando o sandbox
// dessa demanda sumiu, o trabalho foi junto?". Deixar isso implícito no nome do
// tipo obrigaria todo consumidor a redescobrir a regra.
func (r *ExecutionRepo) Transition(ctx context.Context, accountID, id string, t execution.Transition) (*execution.Sandbox, error) {
	var saved *execution.Sandbox
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE sandboxes
			   SET state          = $3::sandbox_state,
			       suspended_at   = CASE WHEN $3 = 'suspended' THEN now() ELSE suspended_at END,
			       destroyed_at   = CASE WHEN $3 = 'destroyed' THEN now() ELSE destroyed_at END,
			       last_active_at = CASE WHEN $3 = 'active'    THEN now() ELSE last_active_at END,
			       updated_at     = now()
			 WHERE id = $1 AND account_id = $2
			RETURNING `+sandboxCols,
			id, accountID, string(t.To))
		var err error
		saved, err = scanSandbox(row)
		if err != nil {
			// A trigger de irreversibilidade fala por RAISE EXCEPTION (P0001):
			// o Translate a devolve como Precondition, com a mensagem inteira.
			return Translate(err, "sandbox")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "sandbox", AggregateID: saved.ID,
			Type: transitionEvent(t),
			Payload: mustJSON(map[string]any{
				"demand_id":         saved.DemandID,
				"namespace":         saved.Namespace,
				"tier":              saved.Tier,
				"preserva_trabalho": t.PreservesWork(),
				"reversivel":        t.Reversible,
			}),
		})
	})
	return saved, err
}

func transitionEvent(t execution.Transition) string {
	switch t.To {
	case execution.StateSuspended:
		return "dop.sandbox.suspended"
	case execution.StateActive:
		return "dop.sandbox.resumed"
	case execution.StateDestroyed:
		return "dop.sandbox.destroyed"
	}
	return "dop.sandbox.changed"
}

// TouchActivity adia a suspensão por ociosidade. NÃO emite evento: batimento de
// atividade é a coisa mais frequente que acontece com um sandbox, e no log de
// eventos ele afogaria o dossiê da demanda — que existe para contar a história,
// não para registrar cada vez que alguém abriu o terminal.
func (r *ExecutionRepo) TouchActivity(ctx context.Context, accountID, id string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE sandboxes SET last_active_at = now(), updated_at = now()
		 WHERE id = $1 AND account_id = $2 AND state <> 'destroyed'`, id, accountID)
	return Translate(err, "sandbox")
}

// ListIdle alimenta o varredor de economia. O corte vem do DOMÍNIO em segundos
// — o SQL não tem opinião sobre quanto tempo é "ocioso".
func (r *ExecutionRepo) ListIdle(ctx context.Context, accountID string, olderThanSeconds int) ([]execution.Sandbox, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+sandboxCols+`
		  FROM sandboxes
		 WHERE account_id = $1
		   AND state = 'active'
		   AND last_active_at < now() - make_interval(secs => $2)
		 ORDER BY last_active_at`, accountID, olderThanSeconds)
	if err != nil {
		return nil, Translate(err, "sandboxes")
	}
	defer rows.Close()

	var out []execution.Sandbox
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, Translate(err, "sandboxes")
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}
