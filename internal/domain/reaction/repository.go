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
// Within one level the order is the writing order, and where two rules were
// written at the same instant, their id. Two rules at the same level share a
// chain position, so without that tiebreaker their relative order would be
// whatever the storage happened to return — and because a rule only disables
// what came BEFORE it, the same two rules would decide differently between two
// runs. A total order is what makes the decision reproducible; which of the two
// comes first matters less than that it is always the same one.
//
// Create is idempotent by key (ADR-0013): repeating a creation returns the rule
// already written rather than a twin.
//
// The key is unique WITHIN AN ACCOUNT, not across the whole table — the
// platform level, which has no account, sharing one namespace of its own. Two
// accounts sending the same key therefore get two different rules, and every
// implementation owes that: the key is client-supplied and passed through
// verbatim, so a table-wide namespace would let account B's write collide with
// a row B is not allowed to see, and the conflict path would hand it over. That
// leak already shipped once through flowByKey, where the namespace was global
// and only the lookup's filter stood between the accounts.
//
// The lookup that resolves the repeat MUST still filter by account. With the
// key scoped it cannot find another account's rule, and it is written that way
// anyway: one guard is the invariant, the other is what stops a later change to
// the namespace from silently reopening the leak.
//
// Read this paragraph, not the Postgres index, when writing a second
// implementation: a fake with a table-wide key would pass its own tests and
// disagree with the adapter on exactly the case the tenancy depends on.
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
