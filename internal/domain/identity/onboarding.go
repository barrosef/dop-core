package identity

import (
	"context"

	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The onboarding journey's vocabulary and rules (spec 2026-09-20 §2, D-5).
//
// The journey is seven moments in the cockpit; five of them are STEPS the core
// records. Entry is the sign-up itself, and "ready" is the completion. The
// state lives here and nowhere else (D-2): a flag in the browser is lost on
// another device and forged on this one.

// Step is one recorded moment of the journey.
type Step string

const (
	StepProfile Step = "profile"
	StepContact Step = "contact"
	StepCode    Step = "code"
	StepTasks   Step = "tasks"
	StepPlan    Step = "plan"
)

// Steps is the journey's order. `Current` is computed from it, so the order
// lives in exactly one place.
func Steps() []Step { return []Step{StepProfile, StepContact, StepCode, StepTasks, StepPlan} }

func ValidStep(s Step) bool {
	for _, k := range Steps() {
		if k == s {
			return true
		}
	}
	return false
}

// Required says which steps `Complete` insists on (D-5): profile, code and
// plan. Contact depends on a carrier and tasks can come later — neither is a
// locked door.
func (s Step) Required() bool { return s == StepProfile || s == StepCode || s == StepPlan }

// StepStatus is what a recorded step says about itself. A step absent from the
// map is pending.
type StepStatus string

const (
	StepDone    StepStatus = "done"
	StepSkipped StepStatus = "skipped"
)

func ValidStepStatus(s StepStatus) bool { return s == StepDone || s == StepSkipped }

// Onboarding is the recorded state: step → status. It is stored as jsonb and
// read as a map; the domain never trusts it to contain valid keys.
type Onboarding map[Step]StepStatus

// Current is the first step, in journey order, that is neither done nor
// skipped — or "" when every step was answered.
func (o Onboarding) Current() Step {
	for _, s := range Steps() {
		if o[s] == "" {
			return s
		}
	}
	return ""
}

// MissingRequired returns the first required step that is not done. Skipped
// does not count for a required step: it cannot be skipped in the first
// place, and a row that says so is a row to distrust.
func (o Onboarding) MissingRequired() Step {
	for _, s := range Steps() {
		if s.Required() && o[s] != StepDone {
			return s
		}
	}
	return ""
}

// Translation keys for the journey's refusals.
const (
	KeyStepUnknown            = "identity.onboarding.step_unknown"
	KeyStepStatusUnknown      = "identity.onboarding.status_unknown"
	KeyStepNotSkippable       = "identity.onboarding.step_not_skippable"
	KeyProfileIncomplete      = "identity.onboarding.profile_incomplete"
	KeyCodeConnectionRequired = "identity.onboarding.code_connection_required"
	KeyPlanRequired           = "identity.onboarding.plan_required"
	KeyStepMissing            = "identity.onboarding.step_missing"
)

// Connections is the narrow question the journey asks the resource domain:
// how many integrations of each category the personal account holds. It is a
// port declared here, satisfied in the composition root, for the same reason
// StepUpGate is — identity does not import resource.
type Connections interface {
	CountIntegrations(ctx context.Context, accountID string) (git, tasks int, err error)
}

// WithConnections wires the counter. Without it every count is zero, which
// makes `code` impossible to complete — the domain tests wire a fake, and the
// composition root always wires the real one.
func (s *Service) WithConnections(c Connections) *Service {
	s.connections = c
	return s
}

func (s *Service) countConnections(ctx context.Context, accountID string) (int, int, error) {
	if s.connections == nil {
		return 0, 0, nil
	}
	return s.connections.CountIntegrations(ctx, accountID)
}

// OnboardingState is the journey as the cockpit needs it: every step with its
// status, where the person is, and the facts the closing screen lists.
type OnboardingState struct {
	Steps             Onboarding
	Current           Step
	Complete          bool
	EmailVerified     bool
	Phone             string
	PhoneVerified     bool
	CodeConnections   int
	TaskConnections   int
	PlanKey           string
	PersonalAccountID string
}

// Onboarding answers for the actor in the context.
func (s *Service) Onboarding(ctx context.Context) (*OnboardingState, error) {
	u, err := s.actorUser(ctx)
	if err != nil {
		return nil, err
	}
	return s.stateOf(ctx, u)
}

func (s *Service) stateOf(ctx context.Context, u *User) (*OnboardingState, error) {
	personalID, err := s.PersonalAccountOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	personal, err := s.repo.AccountByID(ctx, personalID)
	if err != nil {
		return nil, err
	}
	git, tasks, err := s.countConnections(ctx, personalID)
	if err != nil {
		return nil, err
	}
	steps := Onboarding{}
	for k, v := range u.Onboarding {
		steps[k] = v
	}
	current := steps.Current()
	if u.Onboarded() {
		// A finished journey has no current step, whatever the optional ones say.
		current = ""
	}
	return &OnboardingState{
		Steps:             steps,
		Current:           current,
		Complete:          u.Onboarded(),
		EmailVerified:     u.EmailVerified,
		Phone:             u.Phone,
		PhoneVerified:     u.PhoneVerified(),
		CodeConnections:   git,
		TaskConnections:   tasks,
		PlanKey:           personal.PlanKey,
		PersonalAccountID: personalID,
	}, nil
}

// RecordStep writes one step's status after checking it is allowed: a
// required step cannot be skipped, and "done" is only accepted when the fact
// it stands for exists — a name for profile, a git integration for code, a
// plan for plan. The cockpit may say "done"; the core checks.
func (s *Service) RecordStep(ctx context.Context, step Step, status StepStatus) (*OnboardingState, error) {
	if !ValidStep(step) {
		return nil, errs.Invalid("unknown onboarding step: %q", step).
			WithCode(KeyStepUnknown, map[string]any{"step": string(step)})
	}
	if !ValidStepStatus(status) {
		return nil, errs.Invalid("unknown step status: %q", status).
			WithCode(KeyStepStatusUnknown, map[string]any{"status": string(status)})
	}
	if status == StepSkipped && step.Required() {
		return nil, errs.Invalid("the %s step cannot be skipped", step).
			WithCode(KeyStepNotSkippable, map[string]any{"step": string(step)})
	}
	u, err := s.actorUser(ctx)
	if err != nil {
		return nil, err
	}
	if status == StepDone {
		state, err := s.stateOf(ctx, u)
		if err != nil {
			return nil, err
		}
		switch step {
		case StepProfile:
			if u.Name == "" {
				return nil, errs.Precondition("the profile needs a name before it counts as done").
					WithCode(KeyProfileIncomplete, nil)
			}
		case StepCode:
			if state.CodeConnections == 0 {
				return nil, errs.Precondition("connect at least one code provider first").
					WithCode(KeyCodeConnectionRequired, nil)
			}
		case StepPlan:
			if state.PlanKey == "" {
				return nil, errs.Precondition("choose a plan first").
					WithCode(KeyPlanRequired, nil)
			}
		}
	}
	saved, err := s.repo.SetOnboardingStep(ctx, u.ID, step, status)
	if err != nil {
		return nil, err
	}
	return s.stateOf(ctx, saved)
}

// CompleteOnboarding marks the journey finished. It refuses naming the first
// required step still undone, and it is idempotent: a second call changes
// nothing and answers the same.
func (s *Service) CompleteOnboarding(ctx context.Context) (*OnboardingState, error) {
	u, err := s.actorUser(ctx)
	if err != nil {
		return nil, err
	}
	if u.Onboarded() {
		return s.stateOf(ctx, u)
	}
	if missing := u.Onboarding.MissingRequired(); missing != "" {
		return nil, errs.Precondition("the %s step is not done yet", missing).
			WithCode(KeyStepMissing, map[string]any{"step": string(missing)})
	}
	saved, err := s.repo.SetOnboardedAt(ctx, u.ID, s.now())
	if err != nil {
		return nil, err
	}
	return s.stateOf(ctx, saved)
}

// actorUser resolves the person behind the call. Every journey operation is
// about the actor and nobody else: there is no "record a step for user X".
func (s *Service) actorUser(ctx context.Context) (*User, error) {
	call, ok := ctxutil.From(ctx)
	if !ok || call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "no actor in the call")
	}
	u, err := s.repo.UserByID(ctx, call.ActorID)
	if err != nil {
		return nil, err
	}
	// A copy, so that a patch applied to it and then refused never leaks into
	// whatever the repository handed out.
	cp := *u
	return &cp, nil
}
