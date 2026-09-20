package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ── the fake's journey methods ──────────────────────────────────────────────

func (f *fakeRepo) UpdateProfile(_ context.Context, u *identity.User, _ string) (*identity.User, error) {
	cur := f.byID[u.ID]
	// The adapter's rule: a changed phone arrives unverified.
	if cur != nil && cur.Phone != u.Phone {
		u.PhoneVerifiedAt = nil
	}
	cp := *u
	f.byID[u.ID] = &cp
	f.users[u.Subject] = &cp
	return &cp, nil
}

func (f *fakeRepo) SetPhoneVerified(_ context.Context, userID, phone string, at time.Time, _ string) error {
	u := f.byID[userID]
	if u == nil || u.Phone != phone {
		return nil
	}
	u.PhoneVerifiedAt = &at
	return nil
}

func (f *fakeRepo) SetOnboardingStep(_ context.Context, userID string, step identity.Step, status identity.StepStatus) (*identity.User, error) {
	u := f.byID[userID]
	if u == nil {
		return nil, errs.NotFound("user")
	}
	if u.Onboarding == nil {
		u.Onboarding = identity.Onboarding{}
	}
	u.Onboarding[step] = status
	return u, nil
}

func (f *fakeRepo) SetOnboardedAt(_ context.Context, userID string, at time.Time) (*identity.User, error) {
	u := f.byID[userID]
	if u == nil {
		return nil, errs.NotFound("user")
	}
	u.OnboardedAt = &at
	return u, nil
}

func (f *fakeRepo) UpdateAccountProfile(_ context.Context, accountID, handle, displayName string) (*identity.Account, error) {
	a := f.accounts[accountID]
	if a == nil {
		return nil, errs.NotFound("account")
	}
	if handle != "" && handle != a.Handle {
		if _, taken := f.byHandle[handle]; taken {
			return nil, errs.Conflict("handle taken").WithCode(identity.KeyHandleTaken, nil)
		}
		delete(f.byHandle, a.Handle)
		a.Handle = handle
		f.byHandle[handle] = a
	}
	if displayName != "" {
		a.DisplayName = displayName
	}
	return a, nil
}

func (f *fakeRepo) SetAccountPlan(_ context.Context, accountID, planKey string) (*identity.Account, error) {
	a := f.accounts[accountID]
	if a == nil {
		return nil, errs.NotFound("account")
	}
	a.PlanKey = planKey
	return a, nil
}

// ── collaborators ───────────────────────────────────────────────────────────

type fakeWorkspaces struct{ calls [][2]string }

func (w *fakeWorkspaces) EnsurePersonalWorkspace(_ context.Context, accountID, name string) error {
	w.calls = append(w.calls, [2]string{accountID, name})
	return nil
}

type fakePlans struct{ known map[string]bool }

func (p fakePlans) PlanExists(_ context.Context, key string) (bool, error) { return p.known[key], nil }

type fakeConnections struct{ git, tasks int }

func (c *fakeConnections) CountIntegrations(context.Context, string) (int, int, error) {
	return c.git, c.tasks, nil
}

// signedUp creates a user through EnsureUser and returns a context acting as
// them, the way every journey call arrives.
func signedUp(t *testing.T, svc *identity.Service, subject string) (context.Context, *identity.User, *identity.Account) {
	t.Helper()
	u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: subject, Email: subject + "@dop.local", Name: "Dev", EmailVerified: true,
		Providers: []string{"google.com"},
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{AccountID: acct.ID, ActorID: u.ID, ActorKind: ctxutil.ActorUser})
	return ctx, u, acct
}

// ── the personal workspace (D-3) ────────────────────────────────────────────

func TestEnsureUserProvisionsThePersonalWorkspace(t *testing.T) {
	repo := newFakeRepo()
	ws := &fakeWorkspaces{}
	svc := identity.NewService(repo, fixedClock{now}).WithWorkspaces(ws)

	_, _, acct := signedUp(t, svc, "sub-1")
	signedUp(t, svc, "sub-1") // the second login

	if len(ws.calls) != 2 {
		t.Fatalf("the provisioner should be asked on every login, got %d calls", len(ws.calls))
	}
	for _, c := range ws.calls {
		if c[0] != acct.ID || c[1] != identity.PersonalWorkspaceName {
			t.Errorf("provisioner called with %v, want [%s %s]", c, acct.ID, identity.PersonalWorkspaceName)
		}
	}
}

// ── the journey's rules (D-5) ───────────────────────────────────────────────

func TestStepsOrderAndRequirement(t *testing.T) {
	want := []identity.Step{identity.StepProfile, identity.StepContact, identity.StepCode, identity.StepTasks, identity.StepPlan}
	got := identity.Steps()
	if len(got) != len(want) {
		t.Fatalf("Steps() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Steps()[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	for _, s := range got {
		req := s == identity.StepProfile || s == identity.StepCode || s == identity.StepPlan
		if s.Required() != req {
			t.Errorf("%s.Required() = %v, want %v", s, s.Required(), req)
		}
	}
}

func TestRecordStepRefusesSkippingARequiredStep(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	ctx, _, _ := signedUp(t, svc, "sub-1")
	_, err := svc.RecordStep(ctx, identity.StepCode, identity.StepSkipped)
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("skipping code should be invalid, got %v", err)
	}
	if _, err := svc.RecordStep(ctx, identity.StepContact, identity.StepSkipped); err != nil {
		t.Errorf("skipping contact should be allowed: %v", err)
	}
	if _, err := svc.RecordStep(ctx, "photo", identity.StepDone); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("an unknown step should be invalid, got %v", err)
	}
}

func TestRecordStepCodeNeedsAConnection(t *testing.T) {
	conns := &fakeConnections{}
	svc := identity.NewService(newFakeRepo(), fixedClock{now}).WithConnections(conns)
	ctx, _, _ := signedUp(t, svc, "sub-1")
	svc.RecordStep(ctx, identity.StepProfile, identity.StepDone)
	svc.RecordStep(ctx, identity.StepContact, identity.StepSkipped)

	if _, err := svc.RecordStep(ctx, identity.StepCode, identity.StepDone); errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("code with no connection should be a precondition failure, got %v", err)
	}
	conns.git = 1
	state, err := svc.RecordStep(ctx, identity.StepCode, identity.StepDone)
	if err != nil {
		t.Fatal(err)
	}
	if state.Current != identity.StepTasks || state.CodeConnections != 1 {
		t.Errorf("after code, current should be tasks with 1 connection: %+v", state)
	}
}

func TestCompleteRefusesNamingTheMissingStep(t *testing.T) {
	conns := &fakeConnections{git: 1}
	svc := identity.NewService(newFakeRepo(), fixedClock{now}).WithConnections(conns).
		WithPlans(fakePlans{known: map[string]bool{"free": true}})
	ctx, _, _ := signedUp(t, svc, "sub-1")
	svc.RecordStep(ctx, identity.StepProfile, identity.StepDone)
	svc.RecordStep(ctx, identity.StepCode, identity.StepDone)

	_, err := svc.CompleteOnboarding(ctx)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("complete with plan undone should be refused, got %v", err)
	}
	if _, params := errs.CodeOf(err); params["step"] != "plan" {
		t.Errorf("the refusal should name the plan step: %v", err)
	}
	// Plan must be chosen before its step is done.
	if _, err := svc.RecordStep(ctx, identity.StepPlan, identity.StepDone); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("plan step done without a plan should be refused: %v", err)
	}
	if _, err := svc.SetPlan(ctx, "free"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordStep(ctx, identity.StepPlan, identity.StepDone); err != nil {
		t.Fatal(err)
	}
	state, err := svc.CompleteOnboarding(ctx)
	if err != nil || !state.Complete {
		t.Fatalf("complete should succeed now: %v %+v", err, state)
	}
	// Idempotent: same answer, same timestamp.
	again, err := svc.CompleteOnboarding(ctx)
	if err != nil || !again.Complete || again.Current != "" {
		t.Errorf("second complete should answer the same: %v %+v", err, again)
	}
}

func TestOnboardingCurrentSkipsSkipped(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	ctx, _, _ := signedUp(t, svc, "sub-1")
	svc.RecordStep(ctx, identity.StepProfile, identity.StepDone)
	svc.RecordStep(ctx, identity.StepContact, identity.StepSkipped)
	state, err := svc.Onboarding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Current != identity.StepCode {
		t.Errorf("current should be code, got %s", state.Current)
	}
	if state.Steps[identity.StepContact] != identity.StepSkipped {
		t.Errorf("contact should read as skipped: %+v", state.Steps)
	}
	if state.PersonalAccountID == "" || state.EmailVerified != true {
		t.Errorf("state should carry the personal account and the e-mail status: %+v", state)
	}
}
