package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/domain/agentmetrics"
)

// The agent's metrics (P-23 phase 1). Analytical, next to `cost` and not inside
// it: one row per turn, wide and raw, for study — while `cost` answers "may this
// demand still spend?".
type AgentMetricsRepo struct{ pool *pgxpool.Pool }

func NewAgentMetricsRepo(pool *pgxpool.Pool) *AgentMetricsRepo {
	return &AgentMetricsRepo{pool: pool}
}

// EnsureSession is an upsert by (account, external id) that RETURNS THE STORED
// CURSOR — which is the whole reason it is an upsert and not an insert: the
// second pass over the same file has to learn where the first one stopped.
//
// The metadata is refreshed on the way (branch, cwd, version, the end instant),
// because a session that is still running knows more about itself now than it
// did on the first line.
func (r *AgentMetricsRepo) EnsureSession(ctx context.Context, s agentmetrics.Session) (*agentmetrics.Session, error) {
	out := s
	err := r.pool.QueryRow(ctx, `
		INSERT INTO agent_sessions
			(account_id, demand_id, project_id, external_id, cwd, git_branch,
			 tool_version, started_at, ended_at,
			 auth_method, api_provider, subscription_type, api_key_source)
		VALUES ($1, nullif($2,'')::uuid, nullif($3,'')::uuid, $4, $5, $6, $7,
		        nullif($8,'0001-01-01 00:00:00+00'::timestamptz),
		        nullif($9,'0001-01-01 00:00:00+00'::timestamptz),
		        $10, $11, $12, $13)
		ON CONFLICT (account_id, external_id) DO UPDATE SET
			cwd          = excluded.cwd,
			git_branch   = excluded.git_branch,
			tool_version = excluded.tool_version,
			ended_at     = greatest(agent_sessions.ended_at, excluded.ended_at),
			-- The auth is refreshed only when the batch KNOWS it: a collector
			-- that could not read the tool's answer must not erase what an
			-- earlier batch established.
			auth_method       = coalesce(nullif(excluded.auth_method,''), agent_sessions.auth_method),
			api_provider      = coalesce(nullif(excluded.api_provider,''), agent_sessions.api_provider),
			subscription_type = coalesce(nullif(excluded.subscription_type,''), agent_sessions.subscription_type),
			api_key_source    = coalesce(nullif(excluded.api_key_source,''), agent_sessions.api_key_source),
			demand_id    = coalesce(agent_sessions.demand_id, excluded.demand_id),
			project_id   = coalesce(agent_sessions.project_id, excluded.project_id),
			updated_at   = now()
		RETURNING id, byte_offset`,
		s.AccountID, s.DemandID, s.ProjectID, s.ExternalID, s.CWD, s.GitBranch,
		s.ToolVersion, s.StartedAt, s.EndedAt,
		s.Auth.Method, s.Auth.Provider, s.Auth.Subscription, s.Auth.KeySource).
		Scan(&out.ID, &out.ByteOffset)
	if err != nil {
		return nil, Translate(err, "agent session")
	}
	return &out, nil
}

// RecordTurns writes the turns and moves the cursor in the SAME transaction.
//
// Idempotent by (session, uuid): re-reading a stretch of the file is the normal
// case, not the exception, and it has to cost nothing. `DO NOTHING` and not
// `DO UPDATE` on purpose — a turn does not change after it happened, so a
// conflict means we have already seen it.
func (r *AgentMetricsRepo) RecordTurns(ctx context.Context, sessionID string, turns []agentmetrics.Turn, offset int64) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		for _, t := range turns {
			if _, err := tx.Exec(ctx, `
				INSERT INTO agent_turns
					(session_id, uuid, parent_uuid, occurred_at, model, service_tier,
					 stop_reason, sidechain, input_tokens, output_tokens,
					 cache_creation_tokens, cache_read_tokens, tool_uses, thinking,
					 texts, tools, raw_usage)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
				ON CONFLICT (session_id, uuid) DO NOTHING`,
				sessionID, t.UUID, t.ParentUUID, t.OccurredAt, t.Model, t.ServiceTier,
				t.StopReason, t.Sidechain, t.InputTokens, t.OutputTokens,
				t.CacheCreationTokens, t.CacheReadTokens, t.ToolUses, t.Thinking,
				t.Texts, mustJSON(t.Tools), mustJSON(t.RawUsage)); err != nil {
				return Translate(err, "agent turn")
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE agent_sessions SET byte_offset = $2, updated_at = now()
			 WHERE id = $1 AND byte_offset <= $2`, sessionID, offset); err != nil {
			return Translate(err, "agent session")
		}
		return nil
	})
}

// ConsumptionOf aggregates a demand's turns in ONE round trip.
//
// The account is a parameter and not a filter added later: a demand of another
// account has to come back empty, and the way to guarantee that is for the
// isolation to be in the join, where it cannot be forgotten.
//
// It is a query and not a stored projection because the aggregates that matter
// change as the questions change — and an aggregate written to disk ages and
// starts to lie about a session that is still running.
func (r *AgentMetricsRepo) ConsumptionOf(ctx context.Context, accountID, demandID string) (*agentmetrics.Consumption, error) {
	out := agentmetrics.Consumption{
		DemandID:      demandID,
		TokensByModel: map[string]int64{},
		TokensByAuth:  map[string]int64{},
		CallsByTool:   map[string]int{},
	}
	var first, last *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT count(DISTINCT s.id), count(t.id),
		       coalesce(sum(t.input_tokens),0), coalesce(sum(t.output_tokens),0),
		       coalesce(sum(t.cache_creation_tokens),0), coalesce(sum(t.cache_read_tokens),0),
		       min(t.occurred_at), max(t.occurred_at)
		  FROM agent_sessions s
		  LEFT JOIN agent_turns t ON t.session_id = s.id
		 WHERE s.account_id = $1 AND s.demand_id = $2`, accountID, demandID).
		Scan(&out.Sessions, &out.Turns, &out.InputTokens, &out.OutputTokens,
			&out.CacheCreationTokens, &out.CacheReadTokens, &first, &last)
	if err != nil {
		return nil, Translate(err, "consumption")
	}
	if first != nil {
		out.FirstTurnAt = *first
	}
	if last != nil {
		out.LastTurnAt = *last
	}

	// Split by how it was PAID FOR. It is what makes phase 1 and phase 2
	// comparable instead of merely adjacent — the same token count means a
	// different thing on a plan and on a metered key.
	authRows, err := r.pool.Query(ctx, `
		SELECT coalesce(nullif(s.subscription_type,''), nullif(s.auth_method,''), 'unknown'),
		       sum(t.input_tokens + t.output_tokens + t.cache_creation_tokens + t.cache_read_tokens)
		  FROM agent_sessions s JOIN agent_turns t ON t.session_id = s.id
		 WHERE s.account_id = $1 AND s.demand_id = $2
		 GROUP BY 1`, accountID, demandID)
	if err != nil {
		return nil, Translate(err, "consumption")
	}
	for authRows.Next() {
		var how string
		var n int64
		if err := authRows.Scan(&how, &n); err != nil {
			authRows.Close()
			return nil, Translate(err, "consumption")
		}
		out.TokensByAuth[how] = n
	}
	authRows.Close()
	if err := authRows.Err(); err != nil {
		return nil, Translate(err, "consumption")
	}

	rows, err := r.pool.Query(ctx, `
		SELECT t.model,
		       sum(t.input_tokens + t.output_tokens + t.cache_creation_tokens + t.cache_read_tokens)
		  FROM agent_sessions s JOIN agent_turns t ON t.session_id = s.id
		 WHERE s.account_id = $1 AND s.demand_id = $2
		 GROUP BY t.model`, accountID, demandID)
	if err != nil {
		return nil, Translate(err, "consumption")
	}
	for rows.Next() {
		var model string
		var n int64
		if err := rows.Scan(&model, &n); err != nil {
			rows.Close()
			return nil, Translate(err, "consumption")
		}
		out.TokensByModel[model] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, Translate(err, "consumption")
	}

	// Which tool burned the turns. The names live in a jsonb array, so the
	// counting happens in the database — bringing every turn back to count them
	// here would be moving a session's worth of rows to add integers.
	toolRows, err := r.pool.Query(ctx, `
		SELECT tool, count(*)
		  FROM agent_sessions s
		  JOIN agent_turns t ON t.session_id = s.id
		  CROSS JOIN LATERAL jsonb_array_elements_text(t.tools) AS tool
		 WHERE s.account_id = $1 AND s.demand_id = $2
		 GROUP BY tool`, accountID, demandID)
	if err != nil {
		return nil, Translate(err, "consumption")
	}
	defer toolRows.Close()
	for toolRows.Next() {
		var tool string
		var n int
		if err := toolRows.Scan(&tool, &n); err != nil {
			return nil, Translate(err, "consumption")
		}
		out.CallsByTool[tool] = n
	}
	return &out, toolRows.Err()
}

var _ agentmetrics.Repository = (*AgentMetricsRepo)(nil)
