package execution

import (
	"context"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Config is the substrate's deployment policy. It is not infrastructure: these
// are product decisions the composition root injects.
type Config struct {
	// DevboxImage is the sandbox's image. It runs as an arbitrary NON-root user
	// from the very first image — OKD/OpenShift refuse root through their SCC, and
	// that is an image requirement, not a deployment one (spec §2).
	DevboxImage string
	// IngressDomain is the suffix of the URLs <service>--<demand>.<domain> (spec §5).
	IngressDomain string
}

// Service concentra as regras do substrato. Recebe apenas PORTAS.
//
// Note what does NOT exist here: no method returns a credential, a kubeconfig
// or a socket. The sandbox receives a short-lived derived token as a projected
// volume (spec §5) — none of that travels back through an RPC.
type Service struct {
	repo     Repository
	launcher ports.SandboxLauncher
	access   Access
	demands  Demands
	clock    ports.Clock
	cfg      Config
	library  Library
}

// NewService requires the ports it depends on. The panic here is deliberate: it
// is a wiring error, caught at boot, not in production at three in the morning.
//
// The clock follows the Clock port's rule: no fallback to time.Now(), because
// the fallback switches the port off without anyone noticing and hands the test
// back the wall-clock dependency the port exists to remove.
func NewService(repo Repository, launcher ports.SandboxLauncher, access Access, demands Demands, clock ports.Clock, cfg Config) *Service {
	if repo == nil || launcher == nil || access == nil || demands == nil {
		panic("execution.NewService: repository, launcher, access and demands are required")
	}
	if clock == nil {
		panic("execution.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{repo: repo, launcher: launcher, access: access, demands: demands, clock: clock, cfg: cfg}
}

// caller resolves the account and the actor. A request with no active account is
// invalid by definition (SP-0).
func (s *Service) caller(ctx context.Context) (accountID, userID string, role identity.Role, err error) {
	accountID, err = ctxutil.MustAccount(ctx)
	if err != nil {
		return "", "", "", err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return "", "", "", errs.New(errs.KindUnauthorized, "actor not identified")
	}
	m, err := s.access.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return "", "", "", err
	}
	return accountID, call.ActorID, m.Role, nil
}

// load fetches the active account's sandbox. Another account's sandbox is "not
// found", never "forbidden": the second answer would confirm the id exists.
func (s *Service) load(ctx context.Context, accountID, id string) (*Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("sandbox not provided")
	}
	sb, err := s.repo.ByID(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if sb == nil {
		return nil, errs.NotFound("sandbox")
	}
	return sb, nil
}

// ── provisionamento ──────────────────────────────────────────────────────────

// Provision creates the demand's sandbox.
//
// The order of the checks is the rule, and the first one matters most:
//
//  1. the tier has to arrive DECLARED. Without it, refuse — never a default;
//  2. a repeat of the same idempotency key returns the same sandbox;
//  3. the demand has to exist and belong to the active account;
//  4. an active demand has ONE sandbox (spec §1);
//  5. the substrate has to OFFER the requested tier. If it does not, it refuses
//     BEFORE writing any state — a refusal that provisions half is worse than
//     refusing none.
//
// Only after that is the state written. Two transactions, each atomic with its
// own event (ADR-0019): the first records the INTENT (provisioning), the second
// records what the substrate DELIVERED. A crash between them leaves the
// row in provisioning — visible, reconcilable and with no invisible orphan
// sandbox, which is exactly what a single transaction could not give: writing
// after Launch would lose the trace of what already came up.
func (s *Service) Provision(ctx context.Context, demandID string, tier ports.IsolationTier, idempotencyKey string) (*Sandbox, error) {
	// Before anything: the isolation tier is declared, never presumed.
	if err := RequireTier(tier); err != nil {
		return nil, err
	}
	accountID, userID, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demand not provided")
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("a viewer does not provision a sandbox")
	}

	if idempotencyKey != "" {
		existing, err := s.repo.ByIdempotencyKey(ctx, accountID, idempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	owner, err := s.demands.DemandAccount(ctx, demandID)
	if err != nil {
		return nil, err
	}
	if owner != accountID {
		return nil, errs.NotFound("demand")
	}

	// One active demand, one sandbox. Returning what exists is the useful
	// behaviour; swapping the tier underneath it is NOT — that would silently
	// degrade (or promote) an isolation somebody already declared.
	if live, err := s.repo.LiveByDemand(ctx, accountID, demandID); err != nil {
		return nil, err
	} else if live != nil {
		if live.Tier != tier {
			return nil, errs.Precondition(
				"the demand already has a sandbox with isolation %q; destroy it before asking for %q",
				live.Tier, tier)
		}
		// A row in provisioning is the trace of an attempt that did not finish —
		// a crash between the two transactions. Returning it as it stands would hand
		// the client a half-built sandbox that would never be fixed; resuming from
		// there is what makes the second transaction genuinely idempotent.
		if live.State != StateProvisioning {
			return live, nil
		}
		return s.finishProvision(ctx, accountID, live, tier)
	}

	if err := s.requireTierSupported(ctx, tier); err != nil {
		return nil, err
	}

	ns := NamespaceFor(demandID)
	created, err := s.repo.Create(ctx, &Sandbox{
		AccountID:      accountID,
		DemandID:       demandID,
		State:          StateProvisioning,
		Tier:           tier,
		Namespace:      ns,
		IdempotencyKey: idempotencyKey,
		CreatedBy:      userID,
		LastActiveAt:   s.clock.Now(),
	})
	if err != nil {
		return nil, err
	}

	return s.finishProvision(ctx, accountID, created, tier)
}

// finishProvision runs the SECOND half of provisioning: it brings the sandbox up
// and records what the substrate delivered. It lives apart because it is exactly
// the stretch that has to be redone when the first attempt died halfway.
func (s *Service) finishProvision(ctx context.Context, accountID string, sb *Sandbox, tier ports.IsolationTier) (*Sandbox, error) {
	spec := s.specFor(sb)
	// The documents are assembled at every provisioning, Resume included: the
	// shelf carries the project's CURRENT knowledge, not a snapshot of the day
	// the sandbox was born.
	docs, err := s.documentsFor(ctx, sb)
	if err != nil {
		return nil, err
	}
	spec.Documents = docs

	status, err := s.launcher.Launch(ctx, spec)
	if err != nil {
		// The row stays in provisioning on purpose: it is the trace that somebody
		// tried. Deleting it here would hide a half-started sandbox.
		return nil, err
	}
	if status.Tier != tier {
		// The adapter broke the port's guarantee 1. Undoing is mandatory:
		// delivering isolation different from what was declared is worse than not delivering.
		_ = s.launcher.Destroy(ctx, sb.Handle())
		_, _ = s.repo.Transition(ctx, accountID, sb.ID, DestroyTransition)
		return nil, errs.Internal(
			"the substrate delivered isolation %q for a request of %q — sandbox discarded",
			status.Tier, tier)
	}
	return s.repo.MarkProvisioned(ctx, accountID, sb.ID, status.Tier,
		s.endpoints(sb.DemandID, status.Endpoints))
}

// requireTierSupported turns "this cluster has no Kata" into a refusal with a
// message, which is the spec's R-4: the adapter detects and applies the tier
// policy, it never degrades in silence.
func (s *Service) requireTierSupported(ctx context.Context, tier ports.IsolationTier) error {
	supported, err := s.launcher.SupportedTiers(ctx)
	if err != nil {
		return err
	}
	for _, t := range supported {
		if t == tier {
			return nil
		}
	}
	names := make([]string, 0, len(supported))
	for _, t := range supported {
		names = append(names, string(t))
	}
	return errs.Precondition(
		"this substrate does not offer isolation %q; available: %s",
		tier, strings.Join(names, ", "))
}

// Library is what execution needs from the knowledge domain: the project's
// documents, ready to be mounted.
//
// It is a port and not a direct call for the usual reason, and for one more:
// what goes on the shelf is a decision about CONTEXT, and context belongs to
// whoever owns the knowledge — not to whoever raises containers.
type Library interface {
	LibraryFor(ctx context.Context, demandID string) ([]ports.SandboxFile, error)
}

// WithLibrary wires the shelf. Without it the sandbox comes up with no
// documents, which is what the contract suite wants and not what a demand wants.
func (s *Service) WithLibrary(l Library) *Service {
	s.library = l
	return s
}

// documentsFor assembles the shelf, and a failure here FAILS the provisioning.
//
// The temptation is to carry on without documents — the sandbox would come up,
// after all. That is exactly the failure to avoid: an agent that finds an empty
// shelf does not conclude "the shelf failed", it concludes "this project has no
// rules", and works against conventions it was never shown.
func (s *Service) documentsFor(ctx context.Context, sb *Sandbox) ([]ports.SandboxFile, error) {
	if s.library == nil {
		return nil, nil
	}
	return s.library.LibraryFor(ctx, sb.DemandID)
}

func (s *Service) specFor(sb *Sandbox) ports.SandboxSpec {
	return ports.SandboxSpec{
		SandboxHandle: sb.Handle(),
		AccountID:     sb.AccountID,
		DemandID:      sb.DemandID,
		Tier:          sb.Tier,
		Image:         s.cfg.DevboxImage,
		Env: map[string]string{
			"DOP_SANDBOX_ID": sb.ID,
			"DOP_DEMAND_ID":  sb.DemandID,
			"DOP_WORKSPACE":  ports.SandboxWorkspacePath,
		},
	}
}

// endpoints composes each service's public URL. The naming rule belongs to the
// domain, not to the adapter — see EndpointURL.
func (s *Service) endpoints(demandID string, in []ports.SandboxEndpoint) []Endpoint {
	out := make([]Endpoint, 0, len(in))
	for _, e := range in {
		out = append(out, Endpoint{
			Name:  e.Name,
			URL:   EndpointURL(s.cfg.IngressDomain, demandID, e.Name),
			Port:  e.Port,
			State: e.State,
		})
	}
	return out
}

// ── ciclo de vida ────────────────────────────────────────────────────────────

// Suspend is the SAVING operation: the execution dies, the workspace stays.
//
// Repeating is harmless: suspending what is already suspended returns the
// sandbox as it stands, without touching the substrate and WITHOUT emitting an
// event. A change event that changed nothing poisons the demand's dossier and
func (s *Service) Suspend(ctx context.Context, id string) (*Sandbox, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("a viewer does not change a sandbox life cycle")
	}
	if sb.State == StateSuspended {
		return sb, nil
	}
	if !CanApply(sb.State, SuspendTransition) {
		return nil, errs.Precondition(
			"a sandbox in %q cannot be suspended", sb.State)
	}
	if err := s.launcher.Suspend(ctx, sb.Handle()); err != nil {
		return nil, err
	}
	return s.repo.Transition(ctx, accountID, sb.ID, SuspendTransition)
}

// Resume recreates the execution OVER the existing workspace (spec §3).
//
// Destroyed does not resume. It is the difference between the two operations
// materialized in the one place where it hurts: whoever calls here expecting an
// "undo" needs to hear there is nothing to undo.
func (s *Service) Resume(ctx context.Context, id string) (*Sandbox, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("a viewer does not change a sandbox life cycle")
	}
	if sb.State.IsTerminal() {
		return nil, errs.Precondition(
			"a destroyed sandbox does not resume — destruction takes the workspace with it; " +
				"provision a new one for the demand")
	}
	if sb.State == StateActive {
		if err := s.repo.TouchActivity(ctx, accountID, sb.ID); err != nil {
			return nil, err
		}
		return sb, nil
	}
	if !CanApply(sb.State, ResumeTransition) {
		return nil, errs.Precondition("a sandbox in %q cannot be resumed", sb.State)
	}

	spec := s.specFor(sb)
	// The documents are assembled at every provisioning, Resume included: the
	// shelf carries the project's CURRENT knowledge, not a snapshot of the day
	// the sandbox was born.
	docs, err := s.documentsFor(ctx, sb)
	if err != nil {
		return nil, err
	}
	spec.Documents = docs

	status, err := s.launcher.Resume(ctx, spec)
	if err != nil {
		return nil, err
	}
	if status.Tier != sb.Tier {
		// Resuming with an isolation different from the declared one is silent
		// degradation coming in through the back door: the sandbox already existed,
		// so nobody would recheck the tier.
		return nil, errs.Internal(
			"the substrate resumed the sandbox with isolation %q, declared as %q",
			status.Tier, sb.Tier)
	}
	resumed, err := s.repo.Transition(ctx, accountID, sb.ID, ResumeTransition)
	if err != nil {
		return nil, err
	}
	// The endpoints in the return come from the substrate, but they are NOT
	// rewritten: doing so would emit a second "provisioned" for a sandbox that was
	// only resumed, and every projection would start counting two provisionings
	// where there was one. An endpoint is current state, read through Describe.
	resumed.Endpoints = s.endpoints(sb.DemandID, status.Endpoints)
	return resumed, nil
}

// Destroy is IRREVERSIBLE: it takes execution and workspace.
//
// Idempotent by state: destroying what was already destroyed returns true
// without touching anything. An error there would be hostile — whoever repeats
// the call wants the same result, and the result is already there.
func (s *Service) Destroy(ctx context.Context, id string) (bool, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return false, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return false, err
	}
	if role == identity.RoleViewer {
		return false, errs.Permission("a viewer does not destroy a sandbox")
	}
	if sb.State.IsTerminal() {
		return true, nil
	}
	// The substrate first, the row second. Destroy is idempotent by contract
	// (guarantee 8), so failing to write leaves a clean retry. The reverse order
	// would leave the row saying "destroyed" with the microVM alive and billing,
	// and nobody would look for it again.
	if err := s.launcher.Destroy(ctx, sb.Handle()); err != nil {
		return false, err
	}
	if _, err := s.repo.Transition(ctx, accountID, sb.ID, DestroyTransition); err != nil {
		return false, err
	}
	return true, nil
}

// Describe returns the sandbox as it IS.
//
// It queries the substrate when the sandbox is alive, and not only the database,
// for a specific reason: each endpoint's state (running/stopped) is the demand's
// inner compose, which changes without going through any RPC of ours. Reading
// only the row would return an old photograph with the face of current truth.
func (s *Service) Describe(ctx context.Context, id string) (*Sandbox, error) {
	accountID, _, _, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if !sb.IsLive() {
		return sb, nil
	}

	status, err := s.launcher.Describe(ctx, sb.Handle())
	if err != nil {
		if errs.KindOf(err) == errs.KindNotFound {
			// A real divergence: the row says it exists, the substrate does not have
			// it. Somebody deleted the namespace from outside, or Launch never finished.
			// Saying "active" here would be lying to the cockpit.
			return nil, errs.Precondition(
				"sandbox %s no longer exists in the substrate (recorded state: %s); "+
					"destroy it and provision another", sb.ID, sb.State)
		}
		return nil, err
	}
	sb.Endpoints = s.endpoints(sb.DemandID, status.Endpoints)
	// The tier comes from the DATABASE, not from the substrate: it is what was
	// provisioning. Letting the substrate redeclare it on every read would open
	// door for the value to change without anybody having asked.
	return sb, nil
}

// ── command execution ────────────────────────────────────────────────────────

// RunCommand runs a command in the demand's LIVE sandbox.
//
// This is how the agent acts (ADR-0023 + substrate spec §4): the agent runtime
// asks by DEMAND, which is its vocabulary, and this domain resolves demand →
// sandbox → substrate. The runtime never sees a sandbox id, never sees
// `ports.SandboxLauncher` and never chooses where the command runs.
//
// Three decisions that are not obvious:
//
//  1. THE ERROR IS THE SUBSTRATE'S ALONE. A command that exits non-zero, blows
//     its deadline or has its output cut comes back in `ExecResult` with a nil
//     error — it is the port's guarantee 15, propagated intact. The agent HAS to
//     read the output of the test that failed in order to fix it; returning that
//     as an error would take from it the only information that solves the
//     problem;
//
//  2. AGENT WORK IS ACTIVITY. The touch postpones idle suspension
//     (spec §3). Without it, the saving sweeper would drop the sandbox from
//     under an agent that is precisely working in it — and the symptom would be a
//     tool loop failing on the next turn with "sandbox suspended", with nothing
//     explaining why;
//
//  3. A VIEWER DOES NOT RUN COMMANDS. Running a command in the sandbox is
//     writing into the demand's workspace and spending the time of a machine the
//     account pays for — it is the line that separates a viewer from whoever
//     provisions.
func (s *Service) RunCommand(ctx context.Context, demandID string, req ports.ExecRequest) (*ports.ExecResult, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demand not provided")
	}
	if len(req.Command) == 0 {
		return nil, errs.Invalid("command not provided")
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("a viewer does not run commands in the sandbox")
	}

	sb, err := s.repo.LiveByDemand(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	if sb == nil {
		// A demand with no sandbox and a demand of another account come out the
		// same, for `load`'s reason: the second answer would confirm the id exists.
		return nil, errs.NotFound("sandbox of demand %s", demandID)
	}
	if sb.State != StateActive {
		return nil, errs.Precondition(
			"the sandbox of demand %s is in %q and runs no command; resume it first",
			demandID, sb.State)
	}

	if err := s.repo.TouchActivity(ctx, accountID, sb.ID); err != nil {
		return nil, err
	}
	return s.launcher.Exec(ctx, sb.Handle(), req)
}

// ── logs ─────────────────────────────────────────────────────────────────────

// streamTailLines is how much history accompanies a reconnection.
//
// With no ceiling, opening the logs of a sandbox that has run for hours would
// dump the entire log before the first new line — and the dev who only wanted to
// see what is happening now would wait through megabytes. Deep history is the
// timeline projection's business, not this stream's.
const streamTailLines = 500

// Emitter delivers a line to the client. Its error ends the stream — it is how
// server finds out the client is gone.
type Emitter func(LogLine) error

// StreamLogs follows the sandbox's logs for as long as the client is listening.
//
// A connected dev is ACTIVITY: the touch below postpones idle suspension.
// Without it, the saving sweeper would drop the sandbox from under whoever is
// precisely looking at it (spec §3).
//
// A suspended sandbox has NO logs, and that is an explicit refusal rather than
// an empty stream: suspension erases the execution, and what each substrate
// still keeps of what ran before differs between them — k8s deletes the pod and
// loses everything, Docker keeps the stopped container's log file. Promising
// "sometimes something comes back" would expose that divergence to the client.
func (s *Service) StreamLogs(ctx context.Context, sandboxID string, f LogFilter, emit Emitter) error {
	accountID, _, _, err := s.caller(ctx)
	if err != nil {
		return err
	}
	sb, err := s.load(ctx, accountID, sandboxID)
	if err != nil {
		return err
	}
	switch sb.State {
	case StateDestroyed:
		return errs.Precondition("a destroyed sandbox has no logs")
	case StateSuspended:
		return errs.Precondition("a suspended sandbox has no execution; resume it to see logs")
	}
	if err := s.repo.TouchActivity(ctx, accountID, sb.ID); err != nil {
		return err
	}

	return s.launcher.Tail(ctx, sb.Handle(),
		ports.LogQuery{Service: f.Service, Follow: true, TailLines: streamTailLines},
		func(raw ports.LogLine) error {
			src, tt, text := Classify(raw.Text)
			line := LogLine{
				Source:   src,
				Service:  raw.Service,
				TestType: tt,
				Text:     text,
				At:       raw.At,
			}
			if line.At.IsZero() {
				line.At = s.clock.Now()
			}
			if !f.Matches(line) {
				return nil
			}
			return emit(line)
		})
}

// ── economia ─────────────────────────────────────────────────────────────────

// SweepIdle suspends the account's idle sandboxes and returns how many it
// suspended.
//
// It is spec §3 turned into code: demands wait on humans for hours, and an idle
// sandbox is what separates real parallelism from a drowning machine. A failure
// in one does not interrupt the others — a stubborn pod must not make the whole
// economizar.
func (s *Service) SweepIdle(ctx context.Context) (int, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return 0, err
	}
	idle, err := s.repo.ListIdle(ctx, accountID, int(IdleTimeout.Seconds()))
	if err != nil {
		return 0, err
	}
	now := s.clock.Now()
	suspended := 0
	for i := range idle {
		sb := idle[i]
		if !sb.ShouldSuspend(now) {
			continue
		}
		if err := s.launcher.Suspend(ctx, sb.Handle()); err != nil {
			continue
		}
		if _, err := s.repo.Transition(ctx, accountID, sb.ID, SuspendTransition); err != nil {
			continue
		}
		suspended++
	}
	return suspended, nil
}

// NewSweeper assembles the service for SWEEPING only.
//
// The scheduler serves nobody: it authorizes no caller and queries no demand, it
// only suspends what is idle. Assembling the whole graph in there to satisfy a
// constructor would mean inventing dependencies the sweep does not use — and an
// invented dependency is a dependency somebody starts using one day.
//
// The returned service PANICS if anybody calls Provision or anything that needs
// an actor: it is a wiring error, and failing loudly beats authorizing with
// an empty double.
func NewSweeper(repo Repository, launcher ports.SandboxLauncher, clock ports.Clock) *Service {
	if repo == nil || launcher == nil || clock == nil {
		panic("execution.NewSweeper: repository, launcher and clock are required")
	}
	return &Service{repo: repo, launcher: launcher, clock: clock}
}

// SweepAllAccounts is the saving sweeper running as SYSTEM.
//
// The scheduler has no active account — and all the rest of this domain requires
// one. The way out is to visit account by account: `AccountsWithIdle` says WHICH
// ones to sweep, and each sweep happens with that account in the context, down
// the same path a user call would take. Isolation is not loosened; what changes is who
// decides the visiting order.
//
// An error in one account does not interrupt the others: one account's idle
// sandbox must not stay lit because the previous account has a problem.
func (s *Service) SweepAllAccounts(ctx context.Context) (contas, suspensos int, err error) {
	ids, err := s.repo.AccountsWithIdle(ctx, int(IdleTimeout.Seconds()))
	if err != nil {
		return 0, 0, err
	}
	for _, accountID := range ids {
		// A system actor, with the account at hand: it is the same Call the
		// interceptor would build, and it is what makes MustAccount work without
		// carving out an exception in the domain.
		porConta := ctxutil.Into(ctx, ctxutil.Call{
			AccountID: accountID,
			ActorID:   "scheduler",
			ActorKind: ctxutil.ActorSystem,
		})
		n, err := s.SweepIdle(porConta)
		if err != nil {
			continue
		}
		contas++
		suspensos += n
	}
	return contas, suspensos, nil
}
