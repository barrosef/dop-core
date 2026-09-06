package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/reaction"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ReactionRepo implements reaction.Repository. It is the ONLY place with rule
// SQL — the domain never sees a query.
type ReactionRepo struct{ pool *pgxpool.Pool }

func NewReaction(pool *pgxpool.Pool) *ReactionRepo { return &ReactionRepo{pool: pool} }

// ruleCols casts every nullable column to text with a COALESCE for the reason
// spelled out in workflow.go's VersionOf: a platform rule has NO account and NO
// owner, and the domain represents that absence as the empty string. Scanning
// `uuid` straight into a Go string would need a pointer per column and a nil
// check per column; casting once here keeps the absence in one shape.
//
// disables is cast to text[] rather than scanned as uuid[]: the domain holds
// ids as strings, and going through the driver's uuid array codec would only
// convert them back.
const ruleCols = `id::text, COALESCE(account_id::text,''), owner_scope,
	COALESCE(owner_id::text,''), trigger::text, COALESCE(event_type,''),
	when_match, actions, disables::text[], enabled, why,
	COALESCE(created_by::text,'')`

func scanRule(row pgx.Row) (*reaction.Rule, error) {
	var (
		r          reaction.Rule
		trigger    string
		whenRaw    []byte
		actionsRaw []byte
	)
	if err := row.Scan(&r.ID, &r.AccountID, &r.OwnerScope, &r.OwnerID, &trigger,
		&r.EventType, &whenRaw, &actionsRaw, &r.Disables, &r.Enabled, &r.Why,
		&r.CreatedBy); err != nil {
		return nil, err
	}
	r.Trigger = reaction.TriggerKind(trigger)

	when, err := decodeWhen(whenRaw)
	if err != nil {
		return nil, err
	}
	r.When = when
	r.Actions = decodeActions(actionsRaw)
	return &r, nil
}

// decodeWhen refuses a `when` that is not field-to-STRING.
//
// when_match is jsonb and nothing in the schema stops `{"count": 5}` from being
// written by hand or by a migration. Dropping the entry we cannot read would be
// the worst outcome available: the rule would then match every event instead of
// the one event it was written for, and it would do it silently. So this fails
// the whole read, loudly, naming the row — a policy that cannot be read is a
// policy nobody should be running on.
func decodeWhen(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var when map[string]string
	if err := json.Unmarshal(raw, &when); err != nil {
		return nil, errs.Invalid(
			"a rule's `when` is not a map of field to string (%v): matching is equality "+
				"between strings, and a value this reader cannot compare would make the "+
				"rule match events it was never written for", err)
	}
	if len(when) == 0 {
		return nil, nil
	}
	return when, nil
}

// actionDoc is the shape in the database, kept separate from the domain type
// for the same reason stageDoc is: renaming a domain field must not silently
// rewrite rows that have been sitting in the policy for months.
type actionDoc struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params,omitempty"`
}

func encodeActions(actions []reaction.Action) []byte {
	docs := make([]actionDoc, 0, len(actions))
	for _, a := range actions {
		docs = append(docs, actionDoc{Name: string(a.Name), Params: a.Params})
	}
	return mustJSON(docs)
}

// decodeActions tolerates an unreadable array by returning none — and that is
// safe in the direction that matters: a rule with no actions does nothing,
// where a rule with a misread action would do the wrong thing. `when` cannot
// take the same way out, which is why it does not.
func decodeActions(raw []byte) []reaction.Action {
	if len(raw) == 0 {
		return nil
	}
	var docs []actionDoc
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil
	}
	out := make([]reaction.Action, 0, len(docs))
	for _, d := range docs {
		out = append(out, reaction.Action{Name: reaction.ActionName(d.Name), Params: d.Params})
	}
	return out
}

// ── reads ────────────────────────────────────────────────────────────────────

// RulesFor brings every rule of the chain for one event type, in ONE query.
//
// One query and not one per level (nor a UNION ALL of four) because this table
// holds policy — hundreds of rows, not millions. Measured with EXPLAIN ANALYZE
// over a seeded table, the single predicate drives event_type through
// reaction_rules_lookup and resolves owner_scope/owner_id in a Filter, which on
// this table costs a fraction of a millisecond. A per-level UNION would use all
// three index columns and would also be four copies of the tenancy predicate
// below — four places for the leak to come back.
//
// Two filters, doing different jobs:
//
//   - the chain pairs decide WHICH LEVELS the caller asked about. Platform rules
//     come back only because ("platform","") is in the chain, never because the
//     query assumes it: the chain is the caller's, and a caller resolving
//     without the platform level must not be handed it anyway.
//   - `owner_scope = 'platform' OR account_id = $2` is the tenancy guard. A
//     platform rule has no account and applies to everyone; everything else has
//     to BE the caller's. account_id is compared as text because the caller's
//     account may be the empty string, and an empty string is not a uuid — the
//     same trap VersionOf documents.
//
// The ORDER BY is the contract. array_position over the caller's chain puts the
// most generic level first, and created_at then id break the tie WITHIN a level:
// two rules at one level share a position, and `Decide` only lets a rule disable
// what came before it, so an unstable order there would make the same two rules
// decide differently between two runs.
func (r *ReactionRepo) RulesFor(ctx context.Context, accountID, eventType string, chain []reaction.ScopeRef) ([]reaction.Rule, error) {
	if len(chain) == 0 {
		return nil, nil
	}
	scopes := make([]string, 0, len(chain))
	owners := make([]string, 0, len(chain))
	for _, ref := range chain {
		scopes = append(scopes, ref.Scope)
		owners = append(owners, ref.ID)
	}

	rows, err := r.pool.Query(ctx, `SELECT `+ruleCols+`
		  FROM reaction_rules
		 WHERE enabled
		   AND trigger = 'event'
		   AND event_type = $1
		   AND (owner_scope, COALESCE(owner_id::text,''))
		       IN (SELECT * FROM unnest($3::text[], $4::text[]))
		   AND (owner_scope = 'platform' OR account_id::text = $2)
		 ORDER BY array_position($3::text[], owner_scope), created_at, id`,
		eventType, accountID, scopes, owners)
	if err != nil {
		return nil, Translate(err, "the chain's rules")
	}
	defer rows.Close()

	var out []reaction.Rule
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, Translate(err, "the chain's rules")
		}
		out = append(out, *rule)
	}
	return out, Translate(rows.Err(), "the chain's rules")
}

// ── writes ───────────────────────────────────────────────────────────────────

// Create writes one rule, idempotently by key (ADR-0017).
//
// The repetition is resolved by the key and not by "query before inserting":
// another request fits between the query and the insert, and the result would
// be two rules where the caller asked for one.
func (r *ReactionRepo) Create(ctx context.Context, rule *reaction.Rule, idempotencyKey string) (*reaction.Rule, error) {
	var saved *reaction.Rule
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// A platform rule has neither account nor owner, and the domain writes
		// that absence as "". Sent as a bare parameter against a uuid column,
		// '' is not a value the type accepts: Postgres raises `invalid input
		// syntax for type uuid: ""` before any constraint is consulted. nil is
		// the only thing that reaches the column as NULL.
		var accountID, ownerID, createdBy any
		if rule.AccountID != "" {
			accountID = rule.AccountID
		}
		if rule.OwnerID != "" {
			ownerID = rule.OwnerID
		}
		if rule.CreatedBy != "" {
			createdBy = rule.CreatedBy
		}
		// A nil slice encodes as NULL, and disables is NOT NULL. The domain
		// says "disables nothing" with an empty slice or with nil; the column
		// says it with '{}'.
		disables := rule.Disables
		if disables == nil {
			disables = []string{}
		}
		when := rule.When
		if when == nil {
			when = map[string]string{}
		}

		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO reaction_rules (account_id, owner_scope, owner_id, trigger,
			                            event_type, when_match, actions, disables,
			                            enabled, why, idempotency_key, created_by)
			VALUES ($1, $2, $3, $4::reaction_trigger, NULLIF($5,''), $6, $7, $8::uuid[],
			        $9, $10, $11, $12)
			ON CONFLICT (COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid),
			             idempotency_key) DO NOTHING
			RETURNING id::text`,
			accountID, rule.OwnerScope, ownerID, string(rule.Trigger), rule.EventType,
			mustJSON(when), encodeActions(rule.Actions), disables, rule.Enabled,
			rule.Why, idempotencyKey, createdBy).Scan(&id)
		if NoRows(err) {
			saved, err = ruleByKey(ctx, tx, rule.AccountID, idempotencyKey)
			return err
		}
		if err != nil {
			return Translate(err, "reaction rule")
		}
		saved, err = loadRule(ctx, tx, rule.AccountID, id)
		return err
	})
	return saved, err
}

// SetEnabled switches one of the ACCOUNT'S OWN rules.
//
// The account filter is the whole guard: no row belonging to another account,
// and no row belonging to the platform, can be reached from here. An account
// refuses an inherited rule by naming its id in `disables` — an act written in
// the account's own row, which whoever audits the policy can see and argue
// with. Flipping `enabled` on somebody else's row would be the same effect with
// none of the evidence.
func (r *ReactionRepo) SetEnabled(ctx context.Context, accountID, ruleID string, enabled bool) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE reaction_rules SET enabled = $3, updated_at = now()
		 WHERE id = $1 AND account_id::text = $2`, ruleID, accountID, enabled)
	if err != nil {
		return Translate(err, "reaction rule")
	}
	if tag.RowsAffected() == 0 {
		// NotFound and not Permission: from this account's side the rule does
		// not exist, and saying "it exists but is not yours" would confirm the
		// id to whoever guessed it.
		return errs.NotFound("reaction rule %s", ruleID)
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func loadRule(ctx context.Context, tx pgx.Tx, accountID, id string) (*reaction.Rule, error) {
	rule, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleCols+`
		  FROM reaction_rules
		 WHERE id = $1 AND (owner_scope = 'platform' OR account_id::text = $2)`, id, accountID))
	if err != nil {
		return nil, Translate(err, "reaction rule")
	}
	return rule, nil
}

// ruleByKey is the repetition's return: the key already written points at what
// the caller wanted to create, and returning that is what makes the write
// genuinely idempotent (ADR-0017).
//
// The account filter is load-bearing, not decoration. It is the second of the
// two guards described on reaction_rules_idempotency: the index already scopes
// the key to the account, so a key reused by another account cannot collide
// here at all — but flowByKey shipped this same lookup without the filter once,
// and account B received account A's flow. The filter costs nothing and it is
// what keeps that from depending on an index definition somebody may widen
// later.
func ruleByKey(ctx context.Context, tx pgx.Tx, accountID, key string) (*reaction.Rule, error) {
	rule, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleCols+`
		  FROM reaction_rules
		 WHERE idempotency_key = $1 AND COALESCE(account_id::text,'') = $2`, key, accountID))
	if err != nil {
		return nil, Translate(err, "reaction rule")
	}
	return rule, nil
}

var _ reaction.Repository = (*ReactionRepo)(nil)
