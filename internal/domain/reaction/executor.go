package reaction

import (
	"context"
	"errors"
	"fmt"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Handler runs one decided action.
//
// It is an interface and not a plain func type because a real handler (the
// mailer, the attention repository) is a value with dependencies, not a bare
// function — the same shape ports.EventBus's consumers already take. Tests
// that only need a function still get one, through an adapter defined next to
// them, the way net/http's HandlerFunc adapts a func to http.Handler.
type Handler interface {
	Run(ctx context.Context, p PlannedAction) error
}

// Registry is the vocabulary's other half. ValidActionName says which names
// EXIST; Registry says which ones THIS PROCESS can run right now. The two can
// disagree — a rule can be written for a handler that ships next week, or
// survive a deploy that removed one — and that disagreement is exactly what
// Run turns into an error instead of a silent no-op.
type Registry map[ActionName]Handler

// Run refuses an unregistered action name rather than skipping it.
//
// The vocabulary is validated when a rule is WRITTEN (Rule.Validate), so
// reaching an unknown name here means a rule outlived its handler — a deploy
// problem, not something a user typed — and that is a contract error
// (KindInternal), not a business failure. Skipping it quietly would make a
// policy stop working with nothing in the logs to point at: the message below
// names the action, the rule and the event so whoever is paged at 3am does not
// have to reconstruct that from a stack trace.
func (r Registry) Run(ctx context.Context, p PlannedAction) error {
	h, ok := r[p.Name]
	if !ok {
		return errs.New(errs.KindInternal,
			"reaction: no handler registered for action %q required by rule %s (event %s, type %s)",
			p.Name, p.RuleRef, p.Event.ID, p.Event.Type)
	}
	return h.Run(ctx, p)
}

// Applied is the idempotency gate every decider's plan runs through before an
// action is allowed to execute.
//
// It is two methods and not one "already ran?" check, because at-least-once
// delivery (ADR-0019) means two deliveries of the SAME event can be in flight
// at the same time. If the check ran before the handler and the mark was
// written after, both deliveries could see "not yet applied", both would run
// the handler, and the row written afterwards would only ever prove the second
// one wrong too late. Claiming with MarkApplied BEFORE the handler runs closes
// that window: only the caller whose write wins the row gets `true`, and
// everybody else — including the same caller retrying its own earlier attempt
// — gets `false` and must not run the handler. Release gives the claim back
// when the handler then fails, so a redelivery retries the action instead of
// finding the row and skipping it as already done.
//
// KNOWN GAP, left for plan 3 (the failure path) and not built here: the claim
// has no LEASE. If the process crashes after MarkApplied returns `true` but
// before the handler finishes, the row is left in applied_actions with nothing
// to expire it and nothing sweeping it. Every future redelivery of that event
// calls MarkApplied, gets `false` — the row is there — and skips the handler,
// forever: the action is now permanently believed to have run, when it never
// did. There is no operator-visible symptom beyond "this action never
// happened" unless someone thinks to compare applied_actions against what
// actually fired. Recovering needs one of: a lease (a claim that carries an
// expiry and can be reclaimed once it lapses), or a sweep that finds rows older
// than some ceiling with no corresponding success and deletes them so a
// redelivery can retry. Neither exists yet.
type Applied interface {
	// MarkApplied claims the right to run this action, for this event, once.
	// It answers true ONLY for the caller whose write won the claim.
	MarkApplied(ctx context.Context, eventID, ruleRef, actionName string) (bool, error)
	// Release gives the claim back after the handler failed, so the next
	// redelivery retries the action instead of finding the row and skipping.
	Release(ctx context.Context, eventID, ruleRef, actionName string) error
}

// Execute runs a decided plan, once per action.
//
// The gate is claimed BEFORE the handler runs and released only if the handler
// fails, rather than written after success: two deliveries of the same event
// can be in flight at once (JetStream is at-least-once), and a gate written
// afterwards would let both run the same action before either finished.
//
// One action failing does not stop the plan: every other action still gets to
// run, and the error still reaches the caller so the whole delivery is nacked.
// On redelivery, MarkApplied skips everything that already has a row — only
// the action(s) that failed, and so were released, run again.
//
// A Release that itself fails is NOT treated as a reason to stop the loop,
// unlike an early draft of this function which returned immediately. Returning
// there would abandon every action still left in the plan because ONE action's
// claim could not be given back — turning one stuck action into N of them. The
// action whose release failed is still recorded as a failure (its message says
// so, loudly, because that row is now stuck: see Applied's doc comment on the
// missing lease), and the loop moves on to the rest of the plan.
//
// errors.Join, not one fixed errs.Kind wrapping a formatted string: each
// action's own error is preserved as-is (reachable through errors.As/Is) rather
// than collapsed into a single made-up Kind for the whole batch. That still
// leaves a caller who only calls errs.KindOf(err) with ONE Kind for
// potentially several different failures — see the package's self-review notes
// on what that Kind is when the failures disagree.
func Execute(ctx context.Context, reg Registry, applied Applied, plans []PlannedAction) error {
	var failures []error
	for _, p := range plans {
		claimed, err := applied.MarkApplied(ctx, p.Event.ID, p.RuleRef, string(p.Name))
		if err != nil {
			// The gate itself could not be read or written: nothing from here
			// on can be trusted to tell "already ran" from "not yet", so this
			// is not a per-action failure to collect and move past. Stop, and
			// let the whole delivery be retried from the top.
			return err
		}
		if !claimed {
			continue // this delivery, or an earlier one, already ran it
		}
		if err := reg.Run(ctx, p); err != nil {
			if relErr := applied.Release(ctx, p.Event.ID, p.RuleRef, string(p.Name)); relErr != nil {
				failures = append(failures, fmt.Errorf(
					"%s/%s: %w; releasing its claim also failed (%v) — it is now stuck "+
						"marked-applied and will be silently skipped by any redelivery",
					p.RuleRef, p.Name, err, relErr))
				continue
			}
			failures = append(failures, fmt.Errorf("%s/%s: %w", p.RuleRef, p.Name, err))
			continue
		}
	}
	return errors.Join(failures...)
}
