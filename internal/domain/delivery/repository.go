package delivery

import (
	"context"
	"time"
)

// Repository is the delivery domain's persistence PORT.
//
// Declared here, in domain language; implemented in
// internal/adapter/postgres. The domain never sees SQL.
//
// Two obligations hold for EVERY implementation, and they are not negotiable:
//
//   - every read and every write filters by accountID — multi-tenant isolation
//     is a constraint, not trust in the caller;
//   - every write stores the new state and the event in the SAME transaction
//     (ADR-0019). A commit means both, or neither.
type Repository interface {
	// ── evidence of green (ADR-0007) ──

	// RecordVerification stores ONE run. Repeating the same suite on the same
	// commit UPDATES the row and increments the attempt counter — the history of
	// how many times it was tried is part of the evidence, not noise.
	RecordVerification(ctx context.Context, run *VerificationRun, idemKey string) (*VerificationRun, error)

	// EvidenceFor returns everything known about a commit's green. It never
	// returns an error for absence: empty evidence is a legitimate answer — and it
	// is exactly the one that makes the queue refuse.
	EvidenceFor(ctx context.Context, accountID, demandID, repoID, commit string) (Evidence, error)

	// ── pull requests ──

	ListPullRequests(ctx context.Context, accountID string, f PRFilter) ([]PullRequest, error)
	PullRequestByID(ctx context.Context, accountID, id string) (*PullRequest, error)
	// PullRequestOf returns (nil, nil) when the demand has not opened a PR on the
	// repository yet: absence is not the adapter's error, it is the domain's decision.
	PullRequestOf(ctx context.Context, accountID, demandID, repoID string) (*PullRequest, error)
	// OpenPullRequest stores the PR. The evidence goes with it because the
	// database checks the "no green, no PR" rule through a trigger — the service
	// checks first, with a useful message; the database checks always, including
	// on the paths nobody anticipated.
	OpenPullRequest(ctx context.Context, pr *PullRequest, idemKey string) (*PullRequest, error)

	// ── merge queue (ADR-0008) ──

	// QueueOfRepo returns the repository's queue. The final ORDER belongs to the
	// domain (SortQueue); the adapter returns what is stored, including Seq.
	QueueOfRepo(ctx context.Context, accountID, repoID string, includeMerged bool) ([]MergeQueueEntry, error)
	QueueEntryByID(ctx context.Context, accountID, id string) (*MergeQueueEntry, error)
	// Enqueue ASSIGNS the repository's sequence, serializing concurrent entries.
	// Returning Seq filled in is the implementation's obligation: without it the
	// queue's order is ambiguous.
	Enqueue(ctx context.Context, e *MergeQueueEntry, idemKey string) (*MergeQueueEntry, error)
	// SetQueueState moves the entry. The conflict report is optional on every
	// transition except the one going to `conflict`.
	SetQueueState(ctx context.Context, accountID, entryID string, to QueueState, c *ConflictReport, idemKey string) (*MergeQueueEntry, error)

	// ── directives (ADR-0015) ──

	ListDirectives(ctx context.Context, accountID, projectID string) ([]Directive, error)
	DirectiveByID(ctx context.Context, accountID, id string) (*Directive, error)
	CreateDirective(ctx context.Context, d *Directive, idemKey string) (*Directive, error)
	// DecideDirective stores the decision AND applies, in the SAME transaction,
	// the coordination this domain knows how to apply alone — today, the preferred
	// ordering in the merge queue. The other instructions travel in the event to
	// whoever owns them.
	DecideDirective(ctx context.Context, accountID, id string, dec Decision, ins []Instruction, idemKey string) (*Directive, error)
}

// PRFilter is ListPullRequests' slice. An empty field means no filter; the
// account is never an optional filter, it comes separately.
type PRFilter struct {
	DemandID  string
	ProjectID string
	RepoID    string
	OnlyOpen  bool
}

// Demands is the NARROW port into the demand domain — and it is READ-ONLY.
//
// The surface is the entire argument: delivery needs to know that the demand
// exists in the active account and which project it lives in. That is all. There
// is no Pause, Block, Suspend or Advance here, and the absence is ADR-0015 §5's
// golden rule written as a type — this domain has no way to stop any demand, not
// by mistake, not through a path somebody adds absent-mindedly six months from now.
//
// It was designed so the `demand` domain's service satisfies it with a minimal
// glue adapter in the composition root; its package is NOT imported here (it was
// being written in parallel, and a domain does not depend on a sibling domain
// beyond what is strictly necessary).
type Demands interface {
	Demand(ctx context.Context, accountID, demandID string) (*DemandInfo, error)
}

// DemandInfo is the minimum delivery sees of a demand.
type DemandInfo struct {
	ID        string
	ProjectID string
	// Active says whether the demand is still moving. It is READ information:
	// serve para o evento contar a verdade ("a diretriz foi decidida e a
	// demanda 1 continua em andamento"), nunca para decidir se ela para.
	Active bool
}

// ─────────────────────────── GitProvider ───────────────────────────

// GitProvider is the code provider's port (GitHub, GitLab).
//
// The surface is the minimum of ADR-0008's flow: open the PR with the evidence
// package, reapply over the current base, merge one at a time, and ask whether
// the provider already has a queue of its own. Talking to GitHub/GitLab is
// adapter work, not domain work.
//
// One instance speaks for ONE credential and ONE actor. The token arrives READY
// in the adapter's constructor (ADR-0013: a provider token is a resource
// credential, kept behind ports.SecretStore). The git adapter does not know the
// vault, does not query it and does not know it exists — whoever assembles
// entrega o valor.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//
//  1. A CONFLICT IS DATA, AN ERROR IS A FAILURE. Rebase and Merge return
//     Conflicted=true with a NIL error when the provider says that content does
//     fica para o que impede de SABER: rede, provedor fora do ar, credencial
//     invalid, permission, nonexistent repository, unreadable response. The
//     distinction is operational, not aesthetic — a conflict becomes the agent's
//     task and, if it does not resolve it, an attention box item (ADR-0008 §2),
//     erro vira retry e alerta de infra. Trocar um pelo outro ou esconde o
//     the human's conflict or fills the attention box with a network outage;
//
//  2. Conflicted=true ALWAYS carries a non-empty Detail, and Files is
//     vir vazia mesmo havendo conflito. Nenhum dos dois provedores publica a
//     conflicted files in the PR/MR API — GitHub does not expose it, and on
//     GitLab it only exists on an internal Rails route, outside /api/v4, with no
//     version and no promise. Promising Files would be promising what only comes
//     out of a local `git merge`; whoever decided on it would be deciding on
//     luck. Detail carries what the provider actually says, and it is the field
//     the attention box shows;
//
//  3. OPENING A PR IS IDEMPOTENT by (repository, source branch, target branch).
//     Calling twice does NOT create two PRs and is NOT an error: the second call
//     returns the PR that already exists. Both providers refuse the second PR —
//     GitHub with 422, GitLab with 409 — and it is the adapter that turns that
//     refusal into "here is what exists". Without it, every network timeout in a
//     fleet of agents would become an attention item about a PR that was
//     aberto com sucesso;
//
//  4. reopening with a different title or body does NOT rewrite the existing PR:
//     the port returns what is there, and updating a PR stays OUT (see below).
//     Idempotency that overwrites is not idempotency — it is the last call
//     winning, and ADR-0007 §4's evidence package is precisely what must not
//     pode ser trocado por um retry;
//
//  5. with a nil error, ProviderPR has a NON-EMPTY ExternalID and URL.
//     ExternalID is the PR's identity at the provider and it is what Merge
//     receives later: returning a PR with no identifier is returning something
//
//  6. MERGE IS IDEMPOTENT: merging an already merged PR returns Merged=true with
//     the merge commit that already exists — not an error, not a conflict.
//     Honouring this costs an extra read, because BOTH providers refuse an
//     already merged PR with the SAME HTTP code they use for "there is a
//     conflict": the raw response is ambiguous and the adapter has to read the PR
//
//  7. Merged=true only when the provider CONFIRMS the merge, never on the
//     acceptance of a request, and in that case MergeCommit is not empty. "I
//     accepted your request" and "it is on main" are different facts, and
//     ADR-0008's queue releases the next position based on the second;
//
//  8. Merged=false WITH Conflicted=false is a LEGITIMATE answer: the merge did
//     not happen and the reason is not a conflict — the provider's pipeline is
//     running, an approval is missing, the PR is a draft, a branch protection
//     diz qual. Sem esse terceiro estado o adaptador seria obrigado a mentir em
//     one of the two fields, and "conflict" would become the bucket for
//     everything that did not merge — sending a human to resolve a pipeline that
//
//  9. REBASE IS SYNCHRONOUS AT THE PORT. The providers answer before finishing
//     (GitHub accepts the mutation and processes later; GitLab returns "rebase in
//     progress" and does the work in a worker), and it is the ADAPTER that waits
//     desfecho dentro do contexto do chamador. Contexto cancelado ou prazo
//     is KindUnavailable — never a Conflicted=false, which would mean "it did
//     not conflict" when what happened was "I do not know";
//
//  10. REBASE REQUIRES AN OPEN PR, and `Onto` IS NOT FREE. RebaseSpec looks like
//     a git operation and is not: neither provider reapplies a loose branch.
//     GitHub reapplies a PR's branch, and only through GraphQL — its REST has no
//     rebase at all, only an `update-branch` that MERGES the base
//     dentro do branch. O GitLab reaplica o branch de um MR. Os dois reaplicam
//     sempre sobre o destino DAQUELE PR/MR. Por isso: sem PR aberto para
//     (Branch → Onto), the answer is KindPrecondition with the explanation —
//     never a silent reapplication over another base, which is what ADR-0008's
//     queue would re-verify believing it to be another state of the code;
//
//  11. NOMES DO PROVEDOR NÃO CRUZAM A PORTA. `mergeable_state`, `merge_status`,
//     `detailed_merge_status`, PR numbers and MR iids stay on the far side.
//     ExternalID is OPAQUE: it is what the port returned and what it accepts
//     back, with no promised format. It is this guarantee that makes swapping
//     providers a wiring change;
//
//  12. A NONEXISTENT OR INVISIBLE REPOSITORY is KindNotFound; a token that
//     exists but cannot act on the resource is KindPermission; a missing,
//     invalid or expired token is KindUnauthorized. And the honest caveat:
//     GitHub answers 404 for a private repository the token cannot see, ON
//     PURPOSE, so as not to reveal that it exists. The adapter does not guess
//     which of the two it is — it passes NotFound through. Promising to tell them
//     provedor esconde;
//
//  13. O TOKEN NÃO APARECE EM LUGAR NENHUM: nem em mensagem de erro, nem em
//     log, nem em campo de struct, nem no texto de %v, %+v ou %#v do
//     adapter. Errors go to logs, and a provider token in a log is a credential
//     at rest: whoever reads the log opens PRs and merges as the owner. The
//     contract suite uses a sentinel token and sweeps EVERY error output for it;
//
//  14. ActorID is CHECKED, not resolved. The instance was built for one actor
//     (ADR-0003: whoever conducted signs); a request on behalf of ANOTHER actor
//     recusado com KindPermission. Ignorar o campo seria pior que recusar: o PR
//     sairia assinado por quem quer que seja o dono do token fiado, e "quem
//     conduziu assina" viraria mentira silenciosa no lugar exato onde a
//     rastreabilidade importa;
//
//  15. HasNativeQueue NEVER INVENTS. When the adapter cannot LOOK — no
//     permission to read the repository's rules, a token scope that does not
//     reach the project — it returns an ERROR with the cause, never `false`.
//     `false` is an ASSERTION ("this repository has no native queue, you may
//     orchestrate on top") and asserting it without having looked is the same
//     defect as silently degrading isolation: the system keeps looking healthy
//     and the difference only shows up on the day of the incident — here, with
//     two queues merging the same repository. Note what the question HIDES:
//     GitHub's merge queue is per BRANCH (it is a ruleset rule) and GitLab's
//     merge train is per PROJECT. The port asks per repository, and the answer
//     is about the branch ADR-0008's queue contends for — the default one;
//
//  16. HasNativeQueue is a READ: it creates nothing, changes nothing and
//     configures no queue. The port answers whether one exists; joining it is
//
//  17. all four operations are safe for concurrent use.
//
// OUT of the port, on purpose:
//
//   - JOINING the provider's native queue. Asking whether one exists is a single
//     question in both; JOINING is not the same operation: on GitHub the merge
//     configurada por ruleset de branch e o PR entra por auto-merge; no GitLab
//     the merge train is a pipeline queue, on a paid plan, joined through
//     auto merge. A ADR-0008 §4 pede que a plataforma saiba quando NÃO duplicar
//     the queue — not that it drives somebody else's;
//
//   - REVIEW: requesting a reviewer, approving, commenting, resolving a thread.
//     It is the most asymmetric surface between the two — GitHub has reviews with
//     a verdict per person, GitLab has approvals per RULE, with a minimum count
//     and a plan-dependent scope. There is no common denominator that is not one
//     of the two's vocabulary disguised as a port;
//
//   - the provider's STATUS AND CHECKS. DOP's green is ADR-0007's evidence,
//     produzida no sandbox e gravada como VerificationRun. Trazer o check do
//     provider in here would make the platform accept as proof a green it did not
//     produce — which is exactly the trust ADR-0007 refuses;
//
//   - CREATING, DELETING AND PUSHING BRANCHES, and any git operation. This port
//     is about the INTEGRATION REQUEST, not about the repository;
//
//   - UPDATING the PR (title, body, target), draft status, label, milestone,
//     assignee, and CLOSING without merging. None of that is required by
//     ADR-0008's flow, and each brings a vocabulary that diverges between the two;
//
//   - THE MERGE METHOD (merge commit, squash, rebase) and the commit message. It
//     is git-flow policy, which ADR-0013 treats as a governed resource
//     (`git_flow`) and the composition root injects into the adapter. And it is
//     only partly translatable: on GitHub the method goes in the merge call; on
//     GitLab it is PROJECT configuration, and the call only accepts `squash`.
//     ADR-0008's queue needs the merge to happen, not to happen a particular way;
//
//   - WEBHOOKS and event subscription. That is the provider calling the platform,
//     not the platform calling the provider — another direction, another port.
type GitProvider interface {
	OpenPullRequest(ctx context.Context, spec OpenPRSpec) (ProviderPR, error)
	Rebase(ctx context.Context, spec RebaseSpec) (RebaseResult, error)
	Merge(ctx context.Context, spec MergeSpec) (MergeResult, error)
	// HasNativeQueue says whether the provider has a merge queue of its own
	// GitHub, merge trains do GitLab). A fila do DOP orquestra por cima e cobre
	// who does not.
	HasNativeQueue(ctx context.Context, repoExternalID string) (bool, error)
}

// GitProviders resolves WHICH provider serves a repository.
//
// It is not a boot-time choice, like SecretStore or EventBus: the provider
// belongs to the REPOSITORY (ADR-0013), and that is precisely why `ProjectRepo`
// `IntegrationID` — um projeto com um repo no GitHub e outro no GitLab tem que
// representable. A single provider chosen by configuration would make that
// impossible, silently.
//
// It is the same nature as the agent provider port (ADR-0022): chosen per
// request, several adapters active at once.
//
// Whoever implements it also resolves the CREDENTIAL, in the vault — which is
// why the port returns a ready `GitProvider`, and no git adapter knows the vault.
type GitProviders interface {
	For(ctx context.Context, accountID, repoID string) (GitProvider, error)
}

type OpenPRSpec struct {
	RepoExternalID string
	SourceBranch   string
	TargetBranch   string
	Title          string
	// Body is the evidence package already rendered (ADR-0007 §4): the
	// acceptance outcome, the critic's opinion, trace links, who asked.
	Body string
	// Actor is the AUTHORSHIP credential: whoever conducted signs the commits
	// (ADR-0003). Resolving the credential is the adapter's job; the domain only
	// em nome de quem.
	ActorID string
}

type ProviderPR struct {
	ExternalID   string
	URL          string
	HeadCommit   string
	TargetBranch string
	CreatedAt    time.Time
}

type RebaseSpec struct {
	RepoExternalID string
	Branch         string
	Onto           string
}

// RebaseResult carries the conflict as data — see GitProvider's comment.
type RebaseResult struct {
	HeadCommit string
	BaseCommit string
	Conflicted bool
	Files      []string
	Detail     string
}

type MergeSpec struct {
	RepoExternalID string
	PRExternalID   string
	ActorID        string
}

type MergeResult struct {
	Merged       bool
	MergeCommit  string
	Conflicted   bool
	Files        []string
	Detail       string
	MergedAtUnix int64
}
