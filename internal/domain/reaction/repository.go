package reaction

import "context"

// ScopeRef is one level of the chain. It is declared here rather than imported
// from `workflow` for the reason every narrow port in this codebase exists:
// this domain needs the SHAPE of a level, not the workflow domain's entities.
type ScopeRef struct {
	Scope string
	ID    string
}

// Repository is the reaction domain's persistence PORT.
//
// RulesFor returns the chain's rules ordered from the MOST GENERIC level to the
// most specific. That order is not a convenience: `disables` is directional, and
// a project's rule arriving before the platform's would silently fail to switch
// it off. The ordering belongs in the query, and an integration test asserts it.
//
// Within one level the order is the writing order (created_at, then id). Two
// rules at the same level share a chain position, so without that tiebreaker
// their relative order would be whatever the planner happened to return — and
// because a rule only disables what came BEFORE it, the same two rules would
// decide differently between two runs. A total order is what makes the decision
// reproducible; which of the two comes first matters less than that it is
// always the same one.
//
// Create is idempotent by key (ADR-0017): repeating a creation returns the rule
// already written rather than a twin. The key is unique across the whole table,
// so an implementation resolving the repeat MUST filter by account before
// handing a row back — otherwise a key reused by another account returns that
// account's rule.
//
// SetEnabled is an account's switch over its OWN rules. It cannot reach the
// platform's: an account refuses an inherited rule by naming it in `disables`,
// which is an act recorded in the account's own row and visible to whoever
// audits the policy — not a flag flipped on somebody else's.
type Repository interface {
	RulesFor(ctx context.Context, accountID, eventType string, chain []ScopeRef) ([]Rule, error)
	Create(ctx context.Context, r *Rule, idempotencyKey string) (*Rule, error)
	SetEnabled(ctx context.Context, accountID, ruleID string, enabled bool) error
}
