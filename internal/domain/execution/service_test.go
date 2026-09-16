package execution_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/execution"
	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a cluster, WITHOUT Docker and WITHOUT a
// database: repository, launcher, access and clock are ports, and in-memory
// doubles go in here. It is the practical return on hexagonal architecture.
//
// The doubles live in THIS file, and not in internal/adapter: the architecture
// test sweeps every .go under internal/domain, the _test.go files included, and
// importing an adapter from here would break the very boundary it protects. They
// satisfy the SAME ports — the same contract k8s and Docker fulfil.

// ── o modelo: suspender ≠ destruir ───────────────────────────────────────────

// TestSuspendingIsNotDestroying is this domain's central test. Both operations
// stop the execution; only one takes the work with it. If somebody one day
// "simplifies" the two into one, this is where the build breaks.
func TestSuspendingIsNotDestroying(t *testing.T) {
	sus, des := execution.SuspendTransition, execution.DestroyTransition

	if !sus.StopsRuntime || !des.StopsRuntime {
		t.Error("both stop the execution — that is what makes them look alike from outside")
	}
	if !sus.PreservesWork() {
		t.Error("suspending is SAVING: the workspace survives")
	}
	if des.PreservesWork() {
		t.Error("destroying takes the workspace with it — that is what makes it irreversible")
	}
	if !sus.Reversible || des.Reversible {
		t.Error("suspending comes back; destroying does not")
	}
	if !execution.StateDestroyed.IsTerminal() {
		t.Error("destroyed is absorbing")
	}
	// NOTHING leaves destroyed — not to active, not to suspended.
	for _, tr := range []execution.Transition{sus, des, execution.ResumeTransition} {
		if execution.CanApply(execution.StateDestroyed, tr) {
			t.Errorf("destroyed accepted a transition to %q", tr.To)
		}
	}
	if !execution.CanApply(execution.StateActive, sus) {
		t.Error("active should suspend")
	}
	if !execution.CanApply(execution.StateSuspended, execution.ResumeTransition) {
		t.Error("suspended should resume")
	}
	if execution.CanApply(execution.StateSuspended, sus) {
		t.Error("suspending the already suspended is not a transition — it is a repeat")
	}
}

// ── isolationTier: declarado, nunca presumido ────────────────────────────────

func TestAnUndeclaredTierIsRefused(t *testing.T) {
	f := novoCenario(t)

	_, err := f.svc.Provision(f.ctx, "demand-1", ports.TierUnspecified, "")
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a missing tier has to be refused, got: %v", err)
	}
	if !strings.Contains(err.Error(), "does not choose for you") {
		t.Errorf("the message has to say the executor does not choose: %q", err)
	}
	// And above all: nothing was written and nothing was launched.
	if n := f.repo.total(); n != 0 {
		t.Errorf("recusa gravou %d sandbox(es)", n)
	}
	if f.launcher.launches != 0 {
		t.Errorf("recusa chamou o executor %d vez(es)", f.launcher.launches)
	}
}

func TestAnUnknownTierIsRefused(t *testing.T) {
	f := novoCenario(t)
	if _, err := f.svc.Provision(f.ctx, "demand-1", "microvm-turbinada", ""); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a tier outside the vocabulary has to be refused, got: %v", err)
	}
}

// TestAnUnofferedTierRefusesWithoutWriting is the spec's R-4: a Kata
// RuntimeClass is missing from most distributions, and the answer is a refusal with a message.
func TestAnUnofferedTierRefusesWithoutWriting(t *testing.T) {
	f := novoCenario(t)
	f.launcher.tiers = []ports.IsolationTier{ports.TierNamespace}

	_, err := f.svc.Provision(f.ctx, "demand-1", ports.TierHardware, "")
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("expected a precondition refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("the message has to say what IS available: %q", err)
	}
	if n := f.repo.total(); n != 0 {
		t.Errorf("the refusal wrote %d sandbox(es) — a refusal that provisions half is worse than no refusal", n)
	}
}

// TestAExecutorThatDegradesIsDiscarded: if the launcher delivers a tier
// different from the declared one, the sandbox is DESTROYED. Accepting it would
// turn the port's guarantee into a recommendation.
func TestAExecutorThatDegradesIsDiscarded(t *testing.T) {
	f := novoCenario(t)
	f.launcher.tiers = []ports.IsolationTier{ports.TierHardware, ports.TierNamespace}
	f.launcher.delivers = ports.TierNamespace // we asked for hardware, it gives namespace

	_, err := f.svc.Provision(f.ctx, "demand-1", ports.TierHardware, "")
	if err == nil {
		t.Fatal("silent degradation was accepted")
	}
	if f.launcher.destroys == 0 {
		t.Error("the degraded sandbox stayed up, billing")
	}
	sb := f.repo.only(t)
	if sb.State != execution.StateDestroyed {
		t.Errorf("the row is in %q; it should read as destroyed", sb.State)
	}
}

func TestTheDeliveredTierIsStored(t *testing.T) {
	f := novoCenario(t)
	sb, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if sb.Tier != ports.TierNamespace {
		t.Errorf("the client has to see what it RECEIVED: %q", sb.Tier)
	}
	if sb.State != execution.StateActive {
		t.Errorf("state after provisioning: %q", sb.State)
	}
	if sb.Namespace != execution.NamespaceFor("demand-1") {
		t.Errorf("namespace: %q", sb.Namespace)
	}
}

// ── idempotency and uniqueness ───────────────────────────────────────────────

func TestTheIdempotencyKeyDoesNotDuplicateASandbox(t *testing.T) {
	f := novoCenario(t)
	a, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, "key-1")
	if err != nil {
		t.Fatalf("1º Provision: %v", err)
	}
	b, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, "key-1")
	if err != nil {
		t.Fatalf("2º Provision: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("the repeat created another sandbox: %s != %s", a.ID, b.ID)
	}
	if f.launcher.launches != 1 {
		t.Errorf("the executor was invoked %d times for a single key", f.launcher.launches)
	}
}

// TestAnInterruptedProvisioningIsResumed covers the crash between the TWO
// transactions of provisioning: the row stayed in provisioning and the
// executor never came up. The repeat has to finish the job, not hand the client
// half a sandbox nobody can fix afterwards.
func TestAnInterruptedProvisioningIsResumed(t *testing.T) {
	f := novoCenario(t)
	f.launcher.launchFailures = 1

	if _, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, ""); err == nil {
		t.Fatal("the first attempt should have failed")
	}
	half := f.repo.only(t)
	if half.State != execution.StateProvisioning {
		t.Fatalf("o rastro da tentativa gone: estado %q", half.State)
	}

	sb, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("the repeat should finish the provisioning: %v", err)
	}
	if sb.ID != half.ID {
		t.Errorf("the repeat created another sandbox: %s != %s", sb.ID, half.ID)
	}
	if sb.State != execution.StateActive {
		t.Errorf("state after resuming the provisioning: %q", sb.State)
	}
}

func TestOneDemandOneSandbox(t *testing.T) {
	f := novoCenario(t)
	a, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	b, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("2º Provision: %v", err)
	}
	if a.ID != b.ID {
		t.Error("a mesma demand ganhou dois sandboxes")
	}
	// Swapping the isolation of a sandbox that already exists would silently
	// degrade (or promote) what somebody already declared.
	if _, err := f.svc.Provision(f.ctx, "demand-1", ports.TierHardware, ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("expected a refusal when changing an existing sandbox's tier, got: %v", err)
	}
}

// ── isolation and permission ─────────────────────────────────────────────────

func TestAnotherAccountsDemandDoesNotExist(t *testing.T) {
	f := novoCenario(t)
	f.demands.owner["demand-alheia"] = "outra-account"

	if _, err := f.svc.Provision(f.ctx, "demand-alheia", ports.TierNamespace, ""); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("another account's demand has to be 'not found' — the id's existence must not leak: %v", err)
	}
}

func TestAViewerDoesNotProvision(t *testing.T) {
	f := novoCenario(t)
	f.access.papel = identity.RoleViewer
	if _, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, ""); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("viewer provisionou: %v", err)
	}
}

func TestARequestWithNoActiveAccount(t *testing.T) {
	f := novoCenario(t)
	without := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "u1"})
	if _, err := f.svc.Provision(without, "demand-1", ports.TierNamespace, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a request with no active account is invalid by definition: %v", err)
	}
}

func TestAnotherAccountsSandboxIsNotFound(t *testing.T) {
	f := novoCenario(t)
	sb, err := f.svc.Provision(f.ctx, "demand-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	outra := ctxutil.Into(context.Background(), ctxutil.Call{AccountID: "account-b", ActorID: "u2"})
	if _, err := f.svc.Describe(outra, sb.ID); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("sandbox de outra account vazou: %v", err)
	}
}

// ── ciclo de vida ────────────────────────────────────────────────────────────

func TestARepeatedSuspendEmitsNoTransition(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)

	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("1º Suspend: %v", err)
	}
	before := f.repo.transitions
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("the 2nd Suspend should be harmless: %v", err)
	}
	if f.repo.transitions != before {
		t.Error("suspending the already suspended emitted a transition — an event that changed nothing poisons the dossier")
	}
}

func TestDestroyingIsIrreversible(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)

	ok, err := f.svc.Destroy(f.ctx, sb.ID)
	if err != nil || !ok {
		t.Fatalf("Destroy: %v", err)
	}
	// Repeating is harmless: whoever repeats wants the same result, and it is already there.
	if ok, err := f.svc.Destroy(f.ctx, sb.ID); err != nil || !ok {
		t.Fatalf("2º Destroy: %v (%v)", err, ok)
	}
	_, err = f.svc.Resume(f.ctx, sb.ID)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("destroyed resumed: %v", err)
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("the message has to say the work went with it: %q", err)
	}
}

func TestResumingRecreatesOverTheWorkspace(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	got, err := f.svc.Resume(f.ctx, sb.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.State != execution.StateActive {
		t.Fatalf("state after resuming: %q", got.State)
	}
	if f.launcher.resumes != 1 {
		t.Errorf("o executor foi retomado %d vezes", f.launcher.resumes)
	}
}

// TestResumingWithADifferentTierIsRefused closes the back door: the sandbox
// already existed, so nobody would recheck the isolation.
func TestResumingWithADifferentTierIsRefused(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	f.launcher.delivers = ports.TierKernelEmulated
	if _, err := f.svc.Resume(f.ctx, sb.ID); err == nil {
		t.Fatal("retomada degradada foi aceita")
	}
}

func TestDescribeReportsDivergenceWithTheExecutor(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	f.launcher.gone = true // somebody deleted the namespace from outside

	_, err := f.svc.Describe(f.ctx, sb.ID)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("saying 'active' for a sandbox that does not exist is lying to the cockpit: %v", err)
	}
}

// ── economia ─────────────────────────────────────────────────────────────────

func TestTheSweepSuspendsOnlyWhatIsIdle(t *testing.T) {
	f := novoCenario(t)
	ocioso := f.provisionado(t)
	f.repo.get(ocioso.ID).LastActiveAt = f.clock.Now().Add(-2 * execution.IdleTimeout)

	f.demands.owner["demand-2"] = "account-a"
	recente, err := f.svc.Provision(f.ctx, "demand-2", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	n, err := f.svc.SweepIdle(f.ctx)
	if err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	if n != 1 {
		t.Fatalf("suspended %d; expected only the idle one", n)
	}
	if f.repo.get(ocioso.ID).State != execution.StateSuspended {
		t.Error("the idle one stayed up — that is what drowns the machine")
	}
	if f.repo.get(recente.ID).State != execution.StateActive {
		t.Error("o sandbox em uso foi derrubado")
	}
}

func TestShouldSuspendIsPure(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	ativo := execution.Sandbox{State: execution.StateActive, LastActiveAt: now.Add(-execution.IdleTimeout)}
	if !ativo.ShouldSuspend(now) {
		t.Error("no limit de idleness o sandbox suspende")
	}
	quase := execution.Sandbox{State: execution.StateActive, LastActiveAt: now.Add(-execution.IdleTimeout + time.Second)}
	if quase.ShouldSuspend(now) {
		t.Error("one second before the limit it stays")
	}
	suspenso := execution.Sandbox{State: execution.StateSuspended, LastActiveAt: now.Add(-time.Hour)}
	if suspenso.ShouldSuspend(now) {
		t.Error("suspending the already suspended saves nothing")
	}
}

// ── logs ─────────────────────────────────────────────────────────────────────

func TestLineClassification(t *testing.T) {
	casos := []struct {
		raw  string
		src  execution.Source
		tt   execution.TestType
		text string
	}{
		{"[app] subiu na 3000", execution.SourceApp, "", "subiu na 3000"},
		{"[test:e2e] 3 passaram", execution.SourceTest, execution.TestE2E, "3 passaram"},
		{"[infra] docker pronto", execution.SourceInfra, "", "docker pronto"},
		// No prefix means it is what the executor itself printed.
		{"npm ERR! algo", execution.SourceInfra, "", "npm ERR! algo"},
		// A bracket that is not our tag must not be eaten: the line stands as it came.
		{"[2026-08-31] backup ok", execution.SourceInfra, "", "[2026-08-31] backup ok"},
	}
	for _, c := range casos {
		src, tt, text := execution.Classify(c.raw)
		if src != c.src || tt != c.tt || text != c.text {
			t.Errorf("Classify(%q) = (%q,%q,%q); expected (%q,%q,%q)",
				c.raw, src, tt, text, c.src, c.tt, c.text)
		}
	}
}

func TestLogFilter(t *testing.T) {
	line := execution.LogLine{Source: execution.SourceTest, TestType: execution.TestE2E, Service: "backend"}
	if !(execution.LogFilter{}).Matches(line) {
		t.Error("an empty filter asks for everything")
	}
	if !(execution.LogFilter{Source: execution.SourceTest}).Matches(line) {
		t.Error("the same origin should match")
	}
	if (execution.LogFilter{Source: execution.SourceApp}).Matches(line) {
		t.Error("a different source does not match")
	}
	if (execution.LogFilter{TestType: execution.TestAAA}).Matches(line) {
		t.Error("a different test type does not match")
	}
}

func TestStreamLogsFiltersAndCountsAsActivity(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	f.launcher.lines = []ports.LogLine{
		{Text: "[app] subiu"},
		{Text: "[test:e2e] verde"},
		{Text: "docker pronto"},
	}

	var seen []string
	err := f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{Source: execution.SourceApp},
		func(l execution.LogLine) error {
			seen = append(seen, l.Text)
			return nil
		})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if len(seen) != 1 || seen[0] != "subiu" {
		t.Fatalf("the filter did not cut: %v", seen)
	}
	// A connected dev is activity: without it the sweeper would drop the sandbox
	// of whoever is looking right at it.
	if f.repo.touches == 0 {
		t.Error("following logs did not count as activity")
	}
}

func TestStreamLogsRefusesSuspendedAndDestroyed(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	err := f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{}, func(execution.LogLine) error { return nil })
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("suspended has no execution; expected a refusal, got: %v", err)
	}

	if _, err := f.svc.Destroy(f.ctx, sb.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	err = f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{}, func(execution.LogLine) error { return nil })
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("destroyed has no logs; got: %v", err)
	}
}

func TestAnEmitErrorInterruptsTheStream(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	f.launcher.lines = []ports.LogLine{{Text: "one"}, {Text: "two"}, {Text: "three"}}

	boom := errs.Internal("cliente gone")
	n := 0
	err := f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{}, func(execution.LogLine) error {
		n++
		return boom
	})
	if err == nil {
		t.Fatal("the emit error has to bubble up")
	}
	if n != 1 {
		t.Errorf("continuou emitindo after do err: %d lines", n)
	}
}

// ── montagem ─────────────────────────────────────────────────────────────────

// TestTheClockIsRequired: accepting nil would keep the port decorative — the
// service would fall back to time.Now() inside and no idleness test would be
// deterministic.
func TestTheClockIsRequired(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewService accepted a nil clock")
		}
	}()
	execution.NewService(novoRepo(), &fakeLauncher{}, &fakeAccess{}, &fakeDemands{}, nil, execution.Config{})
}

func TestTheEndpointURLBelongsToTheDomain(t *testing.T) {
	got := execution.EndpointURL("dev.dop.app", "5f3a9c21-0000-0000-0000-000000000000", "portal-frontend")
	want := "https://portal-frontend--5f3a9c21.dev.dop.app"
	if got != want {
		t.Errorf("EndpointURL = %q; expected %q", got, want)
	}
	if execution.EndpointURL("", "d1", "app") != "" {
		t.Error("with no ingress domain there is no URL to promise")
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// Duplos
// ═════════════════════════════════════════════════════════════════════════════

type cenario struct {
	ctx      context.Context
	svc      *execution.Service
	repo     *fakeRepo
	launcher *fakeLauncher
	access   *fakeAccess
	demands  *fakeDemands
	clock    *relogioFixo
}

func novoCenario(t *testing.T) *cenario {
	t.Helper()
	c := &cenario{
		ctx: ctxutil.Into(context.Background(), ctxutil.Call{
			RequestID: "req-1", AccountID: "account-a", ActorID: "u1", ActorKind: ctxutil.ActorUser,
		}),
		repo:     novoRepo(),
		launcher: &fakeLauncher{tiers: []ports.IsolationTier{ports.TierNamespace, ports.TierKernelEmulated}},
		access:   &fakeAccess{papel: identity.RoleDeveloper},
		demands:  &fakeDemands{owner: map[string]string{"demand-1": "account-a"}},
		clock:    &relogioFixo{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)},
	}
	c.repo.now = c.clock.Now
	c.svc = execution.NewService(c.repo, c.launcher, c.access, c.demands, c.clock,
		execution.Config{DevboxImage: "dop/devbox:1", IngressDomain: "dev.dop.app"})
	return c
}

func (c *cenario) provisionado(t *testing.T) *execution.Sandbox {
	t.Helper()
	sb, err := c.svc.Provision(c.ctx, "demand-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return sb
}

// ── clock ────────────────────────────────────────────────────────────────────

type relogioFixo struct{ t time.Time }

func (r *relogioFixo) Now() time.Time { return r.t.UTC() }

// ── repository ───────────────────────────────────────────────────────────────

type fakeRepo struct {
	mu sync.Mutex
	// now comes from the service's SAME clock. A double that reads the wall
	// clock reintroduces, in the test, exactly the dependency the Clock port
	// exists to remove — and the test passes or fails depending on the time of day.
	now         func() time.Time
	lines       map[string]*execution.Sandbox
	seq         int
	transitions int
	touches     int
}

func novoRepo() *fakeRepo {
	return &fakeRepo{
		lines: map[string]*execution.Sandbox{},
		now:   func() time.Time { return time.Now().UTC() },
	}
}

func (r *fakeRepo) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

func (r *fakeRepo) get(id string) *execution.Sandbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lines[id]
}

func (r *fakeRepo) only(t *testing.T) *execution.Sandbox {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) != 1 {
		t.Fatalf("expected exactly one line, there are %d", len(r.lines))
	}
	for _, s := range r.lines {
		return s
	}
	return nil
}

func (r *fakeRepo) ByID(_ context.Context, accountID, id string) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.lines[id]
	if !ok || s.AccountID != accountID { // every query filters by account
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (r *fakeRepo) ByIdempotencyKey(_ context.Context, accountID, key string) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.lines {
		if s.AccountID == accountID && s.IdempotencyKey == key && key != "" {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) LiveByDemand(_ context.Context, accountID, demandID string) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.lines {
		if s.AccountID == accountID && s.DemandID == demandID && !s.State.IsTerminal() {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) Create(_ context.Context, s *execution.Sandbox) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	cp := *s
	cp.ID = fmt.Sprintf("sbx-%d", r.seq)
	cp.CreatedAt, cp.UpdatedAt = r.now(), r.now()
	r.lines[cp.ID] = &cp
	out := cp
	return &out, nil
}

func (r *fakeRepo) MarkProvisioned(_ context.Context, accountID, id string, tier ports.IsolationTier, eps []execution.Endpoint) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.lines[id]
	if !ok || s.AccountID != accountID {
		return nil, errs.NotFound("sandbox")
	}
	s.State, s.Tier, s.Endpoints = execution.StateActive, tier, eps
	cp := *s
	return &cp, nil
}

func (r *fakeRepo) Transition(_ context.Context, accountID, id string, t execution.Transition) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.lines[id]
	if !ok || s.AccountID != accountID {
		return nil, errs.NotFound("sandbox")
	}
	// The double carries the SAME invariant as the database trigger: destroyed is
	// absorbing. A double more permissive than the real thing lets through the bug
	// the real one would block — in production, far from here.
	if s.State.IsTerminal() && t.To != execution.StateDestroyed {
		return nil, errs.Precondition("a destroyed sandbox does not resume")
	}
	r.transitions++
	s.State = t.To
	cp := *s
	return &cp, nil
}

func (r *fakeRepo) TouchActivity(_ context.Context, accountID, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.lines[id]; ok && s.AccountID == accountID {
		r.touches++
	}
	return nil
}

func (r *fakeRepo) AccountsWithIdle(_ context.Context, olderThanSeconds int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	corte := time.Duration(olderThanSeconds) * time.Second
	seen := map[string]bool{}
	var out []string
	for _, s := range r.lines {
		if s.State == execution.StateActive && r.now().Sub(s.LastActiveAt) >= corte &&
			!seen[s.AccountID] {
			seen[s.AccountID] = true
			out = append(out, s.AccountID)
		}
	}
	return out, nil
}

func (r *fakeRepo) ListIdle(_ context.Context, accountID string, olderThanSeconds int) ([]execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	corte := time.Duration(olderThanSeconds) * time.Second
	var out []execution.Sandbox
	for _, s := range r.lines {
		if s.AccountID == accountID && s.State == execution.StateActive &&
			r.now().Sub(s.LastActiveAt) >= corte {
			out = append(out, *s)
		}
	}
	return out, nil
}

// ── launcher ─────────────────────────────────────────────────────────────────

type fakeLauncher struct {
	launchFailures int
	tiers          []ports.IsolationTier
	delivers       ports.IsolationTier // empty = it delivers what was requested
	gone           bool
	lines          []ports.LogLine
	launches       int
	resumes        int
	destroys       int
	phases         map[string]ports.SandboxPhase
	// execs keeps the commands received, and execOutput what to return. A command
	// that fails is a RESULT on this port (guarantee 15), so the double has to
	// know how to return a non-zero code without returning an error.
	execs      []ports.ExecRequest
	execOutput *ports.ExecResult
	execErr    error
}

func (l *fakeLauncher) deliveredTier(request ports.IsolationTier) ports.IsolationTier {
	if l.delivers != "" {
		return l.delivers
	}
	return request
}

func (l *fakeLauncher) SupportedTiers(context.Context) ([]ports.IsolationTier, error) {
	return l.tiers, nil
}

func (l *fakeLauncher) Launch(_ context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	l.launches++
	if l.launchFailures > 0 {
		l.launchFailures--
		return nil, errs.New(errs.KindUnavailable, "the executor did not answer")
	}
	if l.phases == nil {
		l.phases = map[string]ports.SandboxPhase{}
	}
	l.phases[spec.ID] = ports.PhaseActive
	return &ports.SandboxStatus{Phase: ports.PhaseActive, Tier: l.deliveredTier(spec.Tier)}, nil
}

func (l *fakeLauncher) Suspend(_ context.Context, h ports.SandboxHandle) error {
	if l.phases != nil {
		l.phases[h.ID] = ports.PhaseSuspended
	}
	return nil
}

func (l *fakeLauncher) Resume(_ context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	l.resumes++
	if l.phases != nil {
		l.phases[spec.ID] = ports.PhaseActive
	}
	return &ports.SandboxStatus{Phase: ports.PhaseActive, Tier: l.deliveredTier(spec.Tier)}, nil
}

func (l *fakeLauncher) Destroy(_ context.Context, h ports.SandboxHandle) error {
	l.destroys++
	delete(l.phases, h.ID)
	return nil
}

func (l *fakeLauncher) Describe(_ context.Context, h ports.SandboxHandle) (*ports.SandboxStatus, error) {
	if l.gone {
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	phase, ok := l.phases[h.ID]
	if !ok {
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	return &ports.SandboxStatus{Phase: phase, Tier: ports.TierNamespace}, nil
}

func (l *fakeLauncher) Exec(_ context.Context, h ports.SandboxHandle, req ports.ExecRequest) (*ports.ExecResult, error) {
	l.execs = append(l.execs, req)
	if l.execErr != nil {
		return nil, l.execErr
	}
	if phase, ok := l.phases[h.ID]; !ok || phase != ports.PhaseActive {
		// The real adapter refuses through Describe before trying; the double does
		// the same so that the domain test does not take a path the port does not
		// allow.
		return nil, errs.Precondition("sandbox %s is not active", h.ID)
	}
	if l.execOutput != nil {
		return l.execOutput, nil
	}
	return &ports.ExecResult{ExitCode: 0, Stdout: "ok\n"}, nil
}

func (l *fakeLauncher) Tail(_ context.Context, _ ports.SandboxHandle, _ ports.LogQuery, emit func(ports.LogLine) error) error {
	for _, ln := range l.lines {
		if err := emit(ln); err != nil {
			return err
		}
	}
	return nil
}

// ── narrow ports into other domains ──────────────────────────────────────────

type fakeAccess struct{ papel identity.Role }

func (a *fakeAccess) Authorize(_ context.Context, userID, accountID string) (*identity.Membership, error) {
	if userID == "" || accountID == "" {
		return nil, errs.Permission("no membership")
	}
	return &identity.Membership{UserID: userID, AccountID: accountID, Role: a.papel}, nil
}

type fakeDemands struct{ owner map[string]string }

func (d *fakeDemands) DemandAccount(_ context.Context, demandID string) (string, error) {
	acc, ok := d.owner[demandID]
	if !ok {
		return "", errs.NotFound("demand")
	}
	return acc, nil
}

func (d *fakeDemands) DemandProject(_ context.Context, demandID string) (string, error) {
	if _, err := d.DemandAccount(context.Background(), demandID); err != nil {
		return "", err
	}
	return "proj-1", nil
}

// The scheduler is a SYSTEM actor and has no active account. The sweep visits
// account by account, and each visit still happens INSIDE an account — the
// isolation is not loosened, only the visiting order is decided from outside.
func TestTheSystemSweepCrossesAccountsWithoutLooseningIsolation(t *testing.T) {
	repo := novoRepo()
	launcher := &fakeLauncher{tiers: []ports.IsolationTier{ports.TierNamespace}}
	relogio := &relogioFixo{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}

	// Two accounts, each with one sandbox idle past the limit.
	for _, account := range []string{"acc-1", "acc-2"} {
		id := "sb-" + account
		repo.lines[id] = &execution.Sandbox{
			ID: id, AccountID: account, State: execution.StateActive,
			LastActiveAt: relogio.t.Add(-2 * execution.IdleTimeout),
		}
	}

	varredor := execution.NewSweeper(repo, launcher, relogio)
	accounts, suspended, err := varredor.SweepAllAccounts(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if accounts != 2 || suspended != 2 {
		t.Fatalf("expected 2 accounts and 2 suspended, got %d and %d", accounts, suspended)
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// RunCommand — the bridge through which the agent ACTS (execution spec §4).
//
// The rule these tests protect is the port's guarantee 15, one floor up: the
// error is the EXECUTOR's; what the command did, including failing, is a result.
// ═════════════════════════════════════════════════════════════════════════════

func TestRunCommandRunsInTheDemandsSandbox(t *testing.T) {
	c := novoCenario(t)
	sb := c.provisionado(t)

	res, err := c.svc.RunCommand(c.ctx, "demand-1", ports.ExecRequest{
		Command: []string{"sh", "-c", "echo oi"},
	})
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d", res.ExitCode)
	}
	if len(c.launcher.execs) != 1 {
		t.Fatalf("the executor received %d command(s)", len(c.launcher.execs))
	}
	// The agent runtime asks by DEMAND; the one that resolves demand → sandbox is
	// this domain. The agent never sees a sandbox id.
	if sb.DemandID != "demand-1" {
		t.Fatalf("sandbox da demand errada: %+v", sb)
	}
}

// A command that fails is a RESULT. If this becomes an error, the agent's tool
// loop loses the only information the model can use to correct itself.
func TestRunCommandExitCodeIsNotAnError(t *testing.T) {
	c := novoCenario(t)
	c.provisionado(t)
	c.launcher.execOutput = &ports.ExecResult{ExitCode: 3, Stderr: "reprovou"}

	res, err := c.svc.RunCommand(c.ctx, "demand-1", ports.ExecRequest{Command: []string{"x"}})
	if err != nil {
		t.Fatalf("a non-zero code became a domain error: %v", err)
	}
	if res.ExitCode != 3 || res.Stderr != "reprovou" {
		t.Fatalf("the command result did not arrive whole: %+v", res)
	}
}

// Agent work is ACTIVITY: without the touch, the saving sweeper drops the
// sandbox from under the agent that is precisely working in it (spec §3).
func TestRunCommandPostponesIdleSuspension(t *testing.T) {
	c := novoCenario(t)
	sb := c.provisionado(t)
	c.repo.touches = 0

	if _, err := c.svc.RunCommand(c.ctx, "demand-1", ports.ExecRequest{Command: []string{"x"}}); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if c.repo.touches == 0 {
		t.Fatalf("running a command did not count as activity in sandbox %s: the sweeper "+
			"suspenderia o sandbox no half do trabalho do agente", sb.ID)
	}
}

func TestRunCommandRefusesWhatItCannotExecute(t *testing.T) {
	t.Run("demand_with_no_sandbox", func(t *testing.T) {
		c := novoCenario(t)
		_, err := c.svc.RunCommand(c.ctx, "demand-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindNotFound {
			t.Fatalf("expected KindNotFound, got %v", err)
		}
	})

	t.Run("sandbox_suspenso", func(t *testing.T) {
		c := novoCenario(t)
		sb := c.provisionado(t)
		if _, err := c.svc.Suspend(c.ctx, sb.ID); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		// A precondition, and never a made-up exit code: a executor with no
		// execution runs no command, and saying that is different from saying the
		// the command failed.
		_, err := c.svc.RunCommand(c.ctx, "demand-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindPrecondition {
			t.Fatalf("expected KindPrecondition, got %v", err)
		}
		if len(c.launcher.execs) != 0 {
			t.Fatal("the command went to the executor even with the sandbox suspended")
		}
	})

	t.Run("a_viewer_does_not_run", func(t *testing.T) {
		c := novoCenario(t)
		c.provisionado(t)
		c.access.papel = identity.RoleViewer
		_, err := c.svc.RunCommand(c.ctx, "demand-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindPermission {
			t.Fatalf("expected KindPermission, got %v", err)
		}
	})

	t.Run("empty_command", func(t *testing.T) {
		c := novoCenario(t)
		c.provisionado(t)
		_, err := c.svc.RunCommand(c.ctx, "demand-1", ports.ExecRequest{})
		if errs.KindOf(err) != errs.KindInvalid {
			t.Fatalf("expected KindInvalid, got %v", err)
		}
	})

	t.Run("sandbox_of_another_account", func(t *testing.T) {
		c := novoCenario(t)
		c.provisionado(t)
		outra := ctxutil.Into(context.Background(), ctxutil.Call{
			AccountID: "account-b", ActorID: "u2", ActorKind: ctxutil.ActorUser,
		})
		// Every query filters by account: account A's sandbox does not exist for
		// account B, and the answer is the same as "does not exist" — the other
		// would confirm the id is real.
		_, err := c.svc.RunCommand(outra, "demand-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindNotFound {
			t.Fatalf("expected KindNotFound, got %v", err)
		}
	})
}
