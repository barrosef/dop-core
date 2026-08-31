package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/idem"
)

// DemandRepo implementa demand.Repository. É o ÚNICO lugar com SQL de demanda.
//
// Toda escrita daqui segue a mesma forma: abre transação, grava estado, grava o
// evento com Emit na MESMA transação, confirma (ADR-0019). Não existe caminho
// que mude estado sem evento — a assinatura da porta não oferece um.
type DemandRepo struct{ pool *pgxpool.Pool }

func NewDemandRepo(pool *pgxpool.Pool) *DemandRepo { return &DemandRepo{pool: pool} }

const demandCols = `id::text, account_id::text, project_id::text, external_key, title,
	card_type, provider_status, dop_status, flow_id, flow_version, flow_snapshot,
	created_by, created_at, updated_at`

func scanDemand(row pgx.Row) (*demand.Demand, error) {
	var d demand.Demand
	var status string
	var snap []byte
	if err := row.Scan(&d.ID, &d.AccountID, &d.ProjectID, &d.ExternalKey, &d.Title,
		&d.CardType, &d.ProviderStatus, &status, &d.Flow.FlowID, &d.Flow.Version,
		&snap, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.Status = demand.DopStatus(status)
	// O snapshot é o fluxo congelado: se ele não voltar legível, a máquina de
	// etapas perde a régua. Erro alto e claro é melhor que uma demanda sem
	// fluxo circulando pelo sistema.
	if len(snap) > 0 {
		if err := json.Unmarshal(snap, &d.Flow); err != nil {
			return nil, errs.Wrap(errs.KindInternal, err,
				"snapshot do fluxo da demanda %s ilegível", d.ID)
		}
	}
	return &d, nil
}

const stageCols = `demand_id::text, key, name, type, gate, status, position,
	artifacts, started_at, finished_at, gate_approved, gate_comment`

func (r *DemandRepo) List(ctx context.Context, accountID, projectID string, limit int, after string) ([]demand.Demand, error) {
	if limit <= 0 {
		limit = 50
	}
	// A paginação anda pelo id da última lida dentro da mesma ordenação
	// (created_at, id): o par ordena de forma total, o created_at sozinho
	// empata entre demandas criadas na mesma sincronização do provedor.
	rows, err := r.pool.Query(ctx, `
		SELECT `+demandCols+`
		  FROM demands
		 WHERE account_id = $1
		   AND ($2::text = '' OR project_id = $2::uuid)
		   AND ($3::text = '' OR (created_at, id) > (
		        SELECT created_at, id FROM demands WHERE id = $3::uuid AND account_id = $1))
		 ORDER BY created_at, id
		 LIMIT $4`,
		accountID, projectID, after, limit)
	if err != nil {
		return nil, Translate(err, "demandas")
	}
	defer rows.Close()

	out := make([]demand.Demand, 0, limit)
	ids := make([]string, 0, limit)
	for rows.Next() {
		d, err := scanDemand(rows)
		if err != nil {
			return nil, Translate(err, "demandas")
		}
		out = append(out, *d)
		ids = append(ids, d.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, Translate(err, "demandas")
	}
	if len(out) == 0 {
		return out, nil
	}

	// Etapas de todas as demandas da página em UMA consulta: a lista do
	// cockpit não pode custar uma ida ao banco por linha.
	byDemand, err := r.stagesOf(ctx, r.pool, accountID, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Stages = byDemand[out[i].ID]
	}
	return out, nil
}

func (r *DemandRepo) ByID(ctx context.Context, accountID, id string) (*demand.Demand, error) {
	if !looksLikeUUID(id) {
		return nil, errs.NotFound("demanda")
	}
	d, err := scanDemand(r.pool.QueryRow(ctx,
		`SELECT `+demandCols+` FROM demands WHERE id = $1 AND account_id = $2`, id, accountID))
	if err != nil {
		if NoRows(err) {
			return nil, nil // ausência é resposta, não falha: o domínio decide
		}
		return nil, Translate(err, "demanda")
	}
	stages, err := r.stagesOf(ctx, r.pool, accountID, []string{d.ID})
	if err != nil {
		return nil, err
	}
	d.Stages = stages[d.ID]
	return d, nil
}

func (r *DemandRepo) ByExternalKey(ctx context.Context, accountID, projectID, externalKey string) (*demand.Demand, error) {
	if !looksLikeUUID(projectID) {
		return nil, errs.Invalid("projeto inválido")
	}
	d, err := scanDemand(r.pool.QueryRow(ctx, `SELECT `+demandCols+`
		  FROM demands
		 WHERE account_id = $1 AND project_id = $2 AND external_key = $3`,
		accountID, projectID, externalKey))
	if err != nil {
		if NoRows(err) {
			return nil, nil
		}
		return nil, Translate(err, "demanda")
	}
	stages, err := r.stagesOf(ctx, r.pool, accountID, []string{d.ID})
	if err != nil {
		return nil, err
	}
	d.Stages = stages[d.ID]
	return d, nil
}

// stagesOf carrega as etapas na ordem do fluxo congelado.
func (r *DemandRepo) stagesOf(ctx context.Context, q pgxQuerier, accountID string, demandIDs []string) (map[string][]demand.Stage, error) {
	rows, err := q.Query(ctx, `
		SELECT `+stageCols+`
		  FROM demand_stages
		 WHERE account_id = $1 AND demand_id = ANY($2::uuid[])
		 ORDER BY demand_id, position`, accountID, demandIDs)
	if err != nil {
		return nil, Translate(err, "etapas da demanda")
	}
	defer rows.Close()

	out := map[string][]demand.Stage{}
	for rows.Next() {
		var demandID string
		var st demand.Stage
		var typ, gate, status string
		var artifacts []byte
		if err := rows.Scan(&demandID, &st.Key, &st.Name, &typ, &gate, &status,
			&st.Position, &artifacts, &st.StartedAt, &st.FinishedAt,
			&st.GateApproved, &st.GateComment); err != nil {
			return nil, Translate(err, "etapas da demanda")
		}
		st.Type, st.Gate, st.Status = demand.StageType(typ), demand.Gate(gate), demand.StageStatus(status)
		if len(artifacts) > 0 {
			_ = json.Unmarshal(artifacts, &st.Artifacts)
		}
		out[demandID] = append(out[demandID], st)
	}
	return out, rows.Err()
}

// ── escritas ─────────────────────────────────────────────────────────────────

func (r *DemandRepo) Create(ctx context.Context, d *demand.Demand, ev demand.Emission, idemKey string) (*demand.Demand, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Demand, error) {
		saved, err := scanDemand(tx.QueryRow(ctx, `
			INSERT INTO demands (account_id, project_id, external_key, title,
			                     card_type, provider_status, dop_status,
			                     flow_id, flow_version, flow_snapshot, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING `+demandCols,
			d.AccountID, d.ProjectID, d.ExternalKey, d.Title, d.CardType,
			d.ProviderStatus, string(d.Status), d.Flow.FlowID, d.Flow.Version,
			mustJSON(d.Flow), d.CreatedBy))
		if err != nil {
			return nil, Translate(err, "demanda")
		}
		for _, st := range d.Stages {
			if _, err := tx.Exec(ctx, `
				INSERT INTO demand_stages (demand_id, account_id, key, name, type,
				                           gate, status, position, artifacts)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				saved.ID, d.AccountID, st.Key, st.Name, string(st.Type),
				string(st.Gate), string(st.Status), st.Position, mustJSON(st.Artifacts)); err != nil {
				return nil, Translate(err, "etapa da demanda")
			}
		}
		saved.Stages = d.Stages
		return saved, emit(ctx, tx, saved.AccountID, saved.ID, ev, nil)
	})
}

func (r *DemandRepo) SaveStage(ctx context.Context, accountID, demandID string, st demand.Stage, status demand.DopStatus, ev demand.Emission, idemKey string) (*demand.Stage, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Stage, error) {
		row := tx.QueryRow(ctx, `
			UPDATE demand_stages
			   SET status = $4, started_at = $5, finished_at = $6,
			       gate_approved = $7, gate_comment = $8, artifacts = $9
			 WHERE demand_id = $1 AND key = $2 AND account_id = $3
			RETURNING `+stageCols,
			demandID, st.Key, accountID, string(st.Status), st.StartedAt,
			st.FinishedAt, st.GateApproved, st.GateComment, mustJSON(st.Artifacts))

		var demandRef, typ, gate, statusCol string
		var saved demand.Stage
		var artifacts []byte
		if err := row.Scan(&demandRef, &saved.Key, &saved.Name, &typ, &gate, &statusCol,
			&saved.Position, &artifacts, &saved.StartedAt, &saved.FinishedAt,
			&saved.GateApproved, &saved.GateComment); err != nil {
			return nil, Translate(err, "etapa da demanda")
		}
		saved.Type, saved.Gate, saved.Status = demand.StageType(typ), demand.Gate(gate), demand.StageStatus(statusCol)
		if len(artifacts) > 0 {
			_ = json.Unmarshal(artifacts, &saved.Artifacts)
		}

		// O status da demanda é PROJEÇÃO das etapas — recalculado pelo domínio
		// e gravado junto, na mesma transação, para que a lista do cockpit não
		// precise abrir cada demanda para saber em que pé ela está.
		if _, err := tx.Exec(ctx, `
			UPDATE demands SET dop_status = $3, updated_at = now()
			 WHERE id = $1 AND account_id = $2`, demandID, accountID, string(status)); err != nil {
			return nil, Translate(err, "demanda")
		}
		return &saved, emit(ctx, tx, accountID, demandID, ev, nil)
	})
}

const threadCols = `id::text, account_id::text, demand_id::text, key, purpose, tools,
	model, effort, budget_micros, state, created_by, created_at, updated_at`

func scanThread(row pgx.Row) (*demand.Thread, error) {
	var t demand.Thread
	var state string
	if err := row.Scan(&t.ID, &t.AccountID, &t.DemandID, &t.Key, &t.Card.Purpose,
		&t.Card.Tools, &t.Card.Model, &t.Card.Effort, &t.Card.BudgetMicros,
		&state, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.State = demand.ThreadState(state)
	return &t, nil
}

func (r *DemandRepo) ThreadsOf(ctx context.Context, accountID, demandID string) ([]demand.Thread, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+threadCols+`
		  FROM demand_threads
		 WHERE account_id = $1 AND demand_id = $2
		 ORDER BY created_at`, accountID, demandID)
	if err != nil {
		return nil, Translate(err, "threads da demanda")
	}
	defer rows.Close()

	out := []demand.Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, Translate(err, "threads da demanda")
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *DemandRepo) ThreadByID(ctx context.Context, accountID, id string) (*demand.Thread, error) {
	if !looksLikeUUID(id) {
		return nil, errs.NotFound("thread")
	}
	t, err := scanThread(r.pool.QueryRow(ctx, `SELECT `+threadCols+`
		  FROM demand_threads WHERE id = $1 AND account_id = $2`, id, accountID))
	if err != nil {
		if NoRows(err) {
			return nil, nil
		}
		return nil, Translate(err, "thread")
	}
	return t, nil
}

func (r *DemandRepo) CreateThread(ctx context.Context, t *demand.Thread, ev demand.Emission, idemKey string) (*demand.Thread, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Thread, error) {
		saved, err := scanThread(tx.QueryRow(ctx, `
			INSERT INTO demand_threads (account_id, demand_id, key, purpose, tools,
			                            model, effort, budget_micros, state, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			RETURNING `+threadCols,
			t.AccountID, t.DemandID, t.Key, t.Card.Purpose, t.Card.Tools,
			t.Card.Model, t.Card.Effort, t.Card.BudgetMicros, string(t.State), t.CreatedBy))
		if err != nil {
			return nil, Translate(err, "thread da demanda")
		}
		// O id da thread só existe depois do INSERT, e o log tem que poder
		// reconstruir a thread sem consultar a tabela — por isso ele entra no
		// payload aqui, e não no domínio.
		return saved, emit(ctx, tx, saved.AccountID, saved.DemandID, ev,
			map[string]any{"thread_id": saved.ID})
	})
}

func (r *DemandRepo) SaveThreadState(ctx context.Context, accountID, threadID string, state demand.ThreadState, ev demand.Emission, idemKey string) (*demand.Thread, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Thread, error) {
		saved, err := scanThread(tx.QueryRow(ctx, `
			UPDATE demand_threads SET state = $3, updated_at = now()
			 WHERE id = $1 AND account_id = $2
			RETURNING `+threadCols, threadID, accountID, string(state)))
		if err != nil {
			// A trigger de conclusão sem achado chega aqui como P0001 e
			// Translate a devolve como falha de pré-condição, com a mensagem
			// da regra — não como 500.
			return nil, Translate(err, "thread da demanda")
		}
		return saved, emit(ctx, tx, saved.AccountID, saved.DemandID, ev, nil)
	})
}

// AppendMessage grava a mensagem SÓ no log (ADR-0006).
//
// Não há tabela de mensagens: o texto já viaja no evento, e a leitura do
// histórico da conversa é a projeção `timeline`, que indexa por agregado. Uma
// tabela aqui seria o mesmo texto gravado duas vezes para servir uma consulta
// que já existe.
func (r *DemandRepo) AppendMessage(ctx context.Context, m *demand.Message, ev demand.Emission, idemKey string) (*demand.Message, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Message, error) {
		saved := *m
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&saved.ID); err != nil {
			return nil, Translate(err, "mensagem")
		}
		// Primeira mensagem tira a thread de `aberta`: o ciclo da conversa é
		// aberta → ativa, e quem o move é a fala, não um comando à parte.
		if _, err := tx.Exec(ctx, `
			UPDATE demand_threads SET state = 'ativa', updated_at = now()
			 WHERE id = $1 AND account_id = $2 AND state = 'aberta'`,
			m.ThreadID, m.AccountID); err != nil {
			return nil, Translate(err, "thread da demanda")
		}
		return &saved, emit(ctx, tx, m.AccountID, m.DemandID, ev,
			map[string]any{"message_id": saved.ID, "at": saved.At})
	})
}

func (r *DemandRepo) CreateFinding(ctx context.Context, f *demand.Finding, ev demand.Emission, idemKey string) (*demand.Finding, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Finding, error) {
		saved := *f
		if err := tx.QueryRow(ctx, `
			INSERT INTO demand_findings (account_id, demand_id, thread_id, title, payload, created_by)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id::text, created_at`,
			f.AccountID, f.DemandID, f.ThreadID, f.Title, mustJSON(f.Payload), f.CreatedBy,
		).Scan(&saved.ID, &saved.CreatedAt); err != nil {
			return nil, Translate(err, "achado")
		}
		return &saved, emit(ctx, tx, f.AccountID, f.DemandID, ev,
			map[string]any{"finding_id": saved.ID})
	})
}

func (r *DemandRepo) HasFinding(ctx context.Context, accountID, threadID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM demand_findings
		                WHERE thread_id = $1 AND account_id = $2)`,
		threadID, accountID).Scan(&exists)
	if err != nil {
		return false, Translate(err, "achados da thread")
	}
	return exists, nil
}

// ── estado + evento + idempotência, numa transação só ────────────────────────

// emit publica o evento da demanda no log da plataforma.
//
// Agregado e agregado_id são fixados AQUI, não pelo chamador: todo evento da
// demanda — inclusive mensagem de thread e achado — pertence ao log daquela
// demanda (ADR-0006). `extra` acrescenta ao payload o que só existe depois do
// INSERT (ids gerados pelo banco), para que o log continue reconstruível
// sozinho.
func emit(ctx context.Context, tx pgx.Tx, accountID, demandID string, ev demand.Emission, extra map[string]any) error {
	payload := make(map[string]any, len(ev.Payload)+len(extra))
	for k, v := range ev.Payload {
		payload[k] = v
	}
	for k, v := range extra {
		payload[k] = v
	}
	return Emit(ctx, tx, ports.Event{
		AccountID: accountID, Aggregate: demand.Aggregate, AggregateID: demandID,
		Type: ev.Type, Payload: mustJSON(payload),
	})
}

// writeIdem executa a escrita com a chave de idempotência reservada DENTRO da
// mesma transação do estado e do evento.
//
// Reservar fora da transação abriria a janela clássica: a chave fica marcada,
// a transação falha, e a repetição legítima passa a receber uma resposta que
// nunca foi gravada. Aqui reserva, escrita e evento têm o mesmo destino —
// commitam juntos ou somem juntos.
//
// A mesma chave com conteúdo diferente é CONFLITO, não repetição: sem isso um
// bug de cliente viraria corrupção silenciosa (ADR-0017).
func writeIdem[T any](ctx context.Context, pool *pgxpool.Pool, key string, ev demand.Emission, fn func(pgx.Tx) (*T, error)) (*T, error) {
	var out *T
	err := InTx(ctx, pool, func(tx pgx.Tx) error {
		if key != "" {
			replay, done, err := reserveIdem(ctx, tx, key, idem.Hash(ev.Type, string(mustJSON(ev.Payload))))
			if err != nil {
				return err
			}
			if done {
				var prev T
				if err := json.Unmarshal(replay, &prev); err != nil {
					return errs.Wrap(errs.KindInternal, err,
						"resposta idempotente gravada é ilegível")
				}
				out = &prev
				return nil
			}
		}
		result, err := fn(tx)
		if err != nil {
			return err
		}
		out = result
		return CompleteTx(ctx, tx, key, mustJSON(result))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func reserveIdem(ctx context.Context, tx pgx.Tx, key, hash string) ([]byte, bool, error) {
	var existingHash string
	var response []byte
	var completed *time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency (key, request_hash, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (key) DO UPDATE SET key = EXCLUDED.key
		RETURNING request_hash, response, completed_at`,
		key, hash, idem.DefaultTTL.String(),
	).Scan(&existingHash, &response, &completed)
	if err != nil {
		return nil, false, errs.Wrap(errs.KindInternal, err, "falha no controle de idempotência")
	}
	if existingHash != hash {
		return nil, false, errs.Conflict("a chave de idempotência já foi usada com outro conteúdo")
	}
	if completed != nil {
		return response, true, nil
	}
	return nil, false, nil
}

var _ demand.Repository = (*DemandRepo)(nil)
