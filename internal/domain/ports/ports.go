// Package ports declares the infrastructure PORTS, in the domain's language.
//
// The ADR-0001 rule: the domain defines the port with the narrowest surface it
// needs; vendor adapters live in internal/adapter and are chosen by
// configuration at the composition root. No SDK crosses this boundary, and any
// capability that does not map across adapters stays OUT of the port.
package ports

import (
	"context"
	"time"
)

// ───────────────────────── SecretStore ─────────────────────────

// SecretRef is a LOGICAL, opaque reference: only the adapter knows how to
// resolve it (a path in Secret Manager, a Secret name in k8s). The domain never
// knows a path, a namespace or a secret name.
type SecretRef struct {
	AccountID string
	Kind      string // integration_credential
	OwnerID   string
}

// SecretValue is opaque by construction: with no useful String(), it does not
// serialize into a log.
type SecretValue []byte

func (SecretValue) String() string { return "***" }

// SecretStore — four operations and nothing more.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//  1. read-after-write: Put followed by Get returns the same value, immediately;
//  2. Get of a missing reference returns (nil, nil) — not an error;
//  3. Delete is idempotent;
//  4. Put over an existing reference replaces it;
//  5. isolation: a reference of account A never resolves a secret of account B;
//  6. the value never appears in a log, an error or a stack trace.
//
// Versioning stays OUT of the port: Secret Manager has versions, a k8s Secret
// is flat. A capability that does not map does not get in.
type SecretStore interface {
	Put(ctx context.Context, ref SecretRef, v SecretValue) error
	Get(ctx context.Context, ref SecretRef) (SecretValue, error)
	Delete(ctx context.Context, ref SecretRef) error
	Exists(ctx context.Context, ref SecretRef) (bool, error)
}

// ───────────────────────── ObjectStore ─────────────────────────

type ObjectRef struct {
	Bucket string
	Key    string
}

type ObjectMeta struct {
	Size        int64
	ContentType string
	UpdatedAt   time.Time
}

// ObjectStore holds knowledge artifacts, chat uploads and diagrams.
// SignedPutURL exists so the upload's bytes do NOT pass through the BFF.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//  1. read-after-write: Put followed by Get returns the same bytes, immediately;
//  2. Put REPLACES the whole object — no merge, no version — and the swap is
//     atomic to a reader: half an object is never read;
//  3. absence is an ERROR, with KindNotFound, in Get and Stat. Unlike
//     SecretStore, which returns (nil, nil): there, absence is a normal state of
//     the credential flow; here, the requested artifact not existing is a
//     failure of the use case;
//  4. Delete is idempotent: removing what does not exist returns nil;
//  5. the key is OPAQUE and FLAT. It may contain "/", but that does NOT create
//     hierarchy: "a/b" and "a/b/c" are two independent objects, "a/b" is not a
//     navigable prefix, and no key escapes the bucket ("../x" is a literal name);
//  6. the bucket isolates: the same key in different buckets are different
//     objects;
//  7. Stat.Size is the exact size of the stored content (zero is valid);
//     ContentType returns what was stored, and "application/octet-stream" when
//     the Put did not say; UpdatedAt is never zero and does not go backwards
//     between writes;
//  8. SignedPutURL/SignedGetURL either return a non-empty URL without touching
//     the object, or fail with KindUnavailable — NEVER an empty string with a
//     nil error. The capability does not exist in every backend (file storage
//     has nothing to sign) and the alternative is pushing the bytes through the
//     BFF; the caller has to be able to DISCOVER that instead of receiving a URL
//     that does not work;
//  9. safe for concurrent use.
type ObjectStore interface {
	Put(ctx context.Context, ref ObjectRef, content []byte, contentType string) error
	Get(ctx context.Context, ref ObjectRef) ([]byte, error)
	Delete(ctx context.Context, ref ObjectRef) error
	Stat(ctx context.Context, ref ObjectRef) (*ObjectMeta, error)
	SignedPutURL(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
	SignedGetURL(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
}

// ───────────────────────── IdentityProvider ─────────────────────────

// Principal is the NORMALIZED result of token verification. Firebase claims do
// not cross this boundary — that is what allows swapping in Keycloak, Zitadel or
// Ory without touching the domain.
type Principal struct {
	// Subject is the ONLY required field (guarantee 3).
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	AvatarURL     string
	Providers     []string
}

// IdentityProvider answers ONE question: who is calling?
//
// The port has a single operation on purpose. Issuing, renewing, revoking and
// administering users is the identity provider's job, not the domain's; what the
// domain needs is for the answer to "who is this" to have the SAME shape coming
// from Firebase (GCP) or from a Keycloak/Dex/Authentik inside the cluster.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//
//  1. a token that cannot prove itself is KindUnauthorized, ALWAYS and only
//     that: empty, malformed, missing one of the three parts, unreadable base64
//     or JSON, an algorithm that is not accepted ("none" and HS* included —
//     swapping the algorithm is the classic way to turn a public key into a
//     shared secret), an invalid signature, signed by an unknown key, expired,
//     not yet valid (nbf), from another issuer, from another audience, or with
//     no subject. A single Kind, because from the domain's point of view there
//     is a single decision: this call is not authenticated;
//
//  2. the error message NEVER contains the token or any piece of it — not a
//     prefix, not a suffix, not the decoded payload, not the raw claim. Errors
//     go to logs, and a token in a log is a credential at rest: whoever reads
//     the log signs in as its owner. The message describes the CAUSE for the
//     operator ("token expired", "unexpected issuer"); the token material stays
//     out;
//
//  3. with a nil error, Subject is NEVER empty. It is the Principal's only
//     required field, and it is the subject's stable identity WITHIN the issuer
//     — a different issuer is a different identity space, which is why the
//     domain stores Subject together with the issuer that produced it, never
//     alone;
//
//  4. Email, Name and AvatarURL are OPTIONAL: empty means "the issuer did not
//     say", never an error. Phone login, passkey login or corporate SSO without
//     profile scope have no email to give, and an adapter that demanded one
//     would make the port unusable in those cases;
//
//  5. EmailVerified is FALSE when the issuer does not say — that is not an
//     error. Absence of an assertion is not an assertion: if the default were
//     true, a silent issuer would promote everyone to verified, and the check
//     that guards email-based account linking would become a rubber stamp;
//
//  6. Providers is INFORMATIONAL: how the subject authenticated and/or which
//     identities are linked, in lowercase names, without repetition and without
//     empty entries ("password", "google.com", "oidc"). An adapter that cannot
//     tell returns an EMPTY list — never invents, never returns an internal
//     vendor name, never returns nil "meaning something". Because a legitimate
//     adapter may not know, NO AUTHORIZATION DECISION MAY DEPEND ON THIS FIELD;
//
//  7. VerifyToken MAY do I/O — discovering the issuer and fetching the public
//     key is part of verifying. When that I/O fails (network, issuer down,
//     unreadable response, cancelled context or deadline exceeded) the error is
//     KindUnavailable, NEVER KindUnauthorized. Confusing the two turns an issuer
//     outage into "your login is invalid" for ALL users at once, sends the user
//     to change a password that is correct, and sends the team hunting the
//     defect in the wrong place;
//
//  8. the public key is CACHED, and revalidated when an unknown `kid` shows up.
//     Both halves are mandatory: fetching the key on every request is a denial
//     of service against the issuer itself, and not revalidating turns key
//     rotation — routine in Keycloak and Firebase — into a total login outage.
//     Revalidation is rate-limited: an attacker sending tokens with random `kid`
//     must not become a traffic generator aimed at the issuer;
//
//  9. verifying has no side effect, is deterministic and is safe for concurrent
//     use: the same token, at the same instant, yields the same result, and
//     nothing in the provider changes for having verified it;
//
//  10. the token arrives RAW, with or without the "Bearer " prefix the HTTP edge
//     carries. An empty token is KindUnauthorized decided with NO I/O at all —
//     a caller with no credential must not cost a round trip to the issuer;
//
//  11. raw claims do not cross the port. The domain sees this struct and nothing
//     else: no claims map, no original token, no vendor-specific block. It is
//     this guarantee that makes swapping issuers a wiring change.
//
// OUT of the port, on purpose:
//
//   - ISSUING, renewing and revoking tokens, and all user CRUD. It is the most
//     asymmetric surface across providers and the one that binds hardest: the
//     domain never needed it to answer "who is calling";
//
//   - PER-REQUEST REVOCATION CHECKS. Firebase offers them (checkRevoked, with a
//     server round trip on every call); generic OIDC offers nothing equivalent
//     short of introspection, which not every issuer publishes. Promising what
//     only one can deliver would be the abstraction leaking — short token expiry
//     is what bounds the window in both;
//
//   - ROLES, custom claims and scopes. Authorization belongs to the domain
//     (account, hierarchy, role), and bringing it from the issuer would move the
//     access policy into each installation's identity provider;
//
//   - the provider's MULTI-TENANCY, clock skew, cache TTL and discovery URL:
//     those are adapter TUNING, done at the composition root through environment
//     variables. They are not domain vocabulary.
type IdentityProvider interface {
	VerifyToken(ctx context.Context, raw string) (*Principal, error)
}

// ───────────────────────── EventBus ─────────────────────────

// json tags below exist for ONE reason: event.DeadLetter embeds this struct
// as-is and it rides inside the dead-letter envelope, which JetStream holds
// for 30 days (see eventbus.deadLetterEnvelope). Without them, the field
// serializes under its Go NAME — renaming ActorID would silently change the
// wire format of every dead letter already sitting in the queue, and a
// consumer built against the old field name would decode a zero-valued
// struct with no error at all. This struct itself is never the wire format
// for an ORDINARY event — that is eventbus.Envelope — so these tags matter
// only for the one path that marshals the struct whole.
type Event struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Aggregate   string `json:"aggregate"`
	AggregateID string `json:"aggregate_id"`
	// AggregateKey is the aggregate's readable handle — "acme" for an account,
	// the workspace's slug. It exists so a human or an agent can talk about a
	// fact without a uuid, in a log, in a panel or in a conversation.
	//
	// It is a SNAPSHOT, not the truth of now: renaming the workspace does not
	// rewrite the events that already happened, and the old event keeps saying
	// the old name. That is what a log of facts is for.
	//
	// Empty where the aggregate has no natural stable handle — a grant, a
	// notification. An honest blank beats a uuid wearing a nickname.
	AggregateKey string    `json:"aggregate_key,omitempty"`
	Type         string    `json:"type"`
	Payload      []byte    `json:"payload,omitempty"`
	OccurredAt   time.Time `json:"occurred_at"`

	// ── Who caused this ────────────────────────────────────────────────────
	// Recorded by the outbox from the call's context, and carried all the way
	// to the consumer. Before this existed the envelope dropped them, so a
	// consumer failed without being able to say who caused the work — the
	// answer was one join away in Postgres, which is archaeology at the moment
	// of an error rather than a payload.
	//
	// All optional: a message published before this change has none, and an
	// event raised by the scheduler has no person behind it.
	ActorKind string `json:"actor_kind,omitempty"`
	ActorID   string `json:"actor_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Caller is the COMPONENT that signed the call — "bff", "collector"
	// (ADR-0029). Empty when the call was proven only by a person's token.
	Caller string `json:"caller,omitempty"`
}

// Handler processes an event. It MUST be idempotent: delivery is at-least-once
// (ADR-0019). A returned error triggers a retry with backoff; past the ceiling,
// the DLQ.
type Handler func(ctx context.Context, e Event) error

// EventBus transports BYTES, not structs.
//
// Payload is the event's JSON envelope — the same one the outbox writes. That
// sentence is this port's most important guarantee, because violating it is
// silent: an adapter that re-serializes ports.Event sends Payload []byte as
// base64, the other side recognizes nothing, and EVERY delivery is dropped with
// no error at all.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//  1. Payload's bytes arrive IDENTICAL to those published, with no re-encoding;
//  2. the delivered event's other fields are read from the envelope, not copied
//     from the published struct — the subscriber sees what travelled the wire;
//  3. Publish with no Payload builds an envelope from the event (never a
//     json.Marshal of ports.Event), preserving the identifying fields;
//  4. Type is the publication subject; subscriptions filter by subject with
//     NATS semantics ("*" matches one token, ">" matches the tail), and an empty
//     list matches everything;
//  5. at-least-once delivery, order NOT guaranteed — the Handler must be
//     idempotent;
//  6. what was published BEFORE the subscription is delivered once the durable
//     appears (retention); otherwise the event spine would only work with the
//     right boot order;
//  7. a Handler error triggers redelivery; success does not;
//  8. an unreadable message is DROPPED with a log entry, not redelivered
//     forever: a poison message must not block the queue;
//  9. Publish is safe for concurrent use; an event with no Type is refused.
//
// OUT of the port, on purpose: deduplication. JetStream deduplicates by ID
// within a window; the in-memory adapter does not. Since delivery is
// at-least-once in both, an idempotent consumer is mandatory anyway — promising
// dedup would be promising what only one adapter delivers.
type EventBus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(ctx context.Context, stream, durable string, subjects []string, h Handler) error
	Close() error
}

// ───────────────────────── SandboxLauncher ─────────────────────────

// IsolationTier is the isolation level of the executor where a demand runs.
//
// It is DECLARED, never presumed. Whoever provisions says which one they want;
// the launcher delivers EXACTLY that or refuses. There is no silent
// degradation: a sandbox that asked for a microVM and got a container still
// looks healthy, and the difference only shows up on the day of the incident.
// A Kata RuntimeClass is missing from most distributions (execution spec §2 and
// R-4) — which is why its absence has to become a refusal with a message, not
// one tier less with no warning.
type IsolationTier string

const (
	TierUnspecified    IsolationTier = ""
	TierHardware       IsolationTier = "hardware"        // Kata/Firecracker — microVM
	TierKernelEmulated IsolationTier = "kernel_emulated" // gVisor / Edera
	TierNamespace      IsolationTier = "namespace"       // container with a strict securityContext
)

func ValidIsolationTier(t IsolationTier) bool {
	switch t {
	case TierHardware, TierKernelEmulated, TierNamespace:
		return true
	}
	return false
}

// SandboxWorkspacePath is where the demand's workspace is mounted INSIDE the
// sandbox, identical in every adapter.
//
// It is a constant of the PORT, and not a field of the spec, because it is the
// only path whose survival across suspension is promised. If each adapter chose
// its own, "the work survives suspension" would become a promise that depends on
// which deployment served the call — exactly the kind of difference the contract
// suite exists to catch.
const SandboxWorkspacePath = "/workspace"

// SandboxDocumentsPath is where the project's ROOT REPOSITORY is cloned inside
// the sandbox (ADR-0028): the shelf — rules, repository maps, memories, and
// this demand's spec and plan — as a working copy, read-write.
//
// It is separate from the workspace on purpose. The workspace is the demand's
// WORK; the shelf is the project's KNOWLEDGE, shared by every sandbox of the
// project through push and pull. From the agent's point of view the content
// was always there: the clone happens before the agent exists, in the image's
// entrypoint, and what the agent commits is there for the next agent.
const SandboxDocumentsPath = "/project"

// SandboxTokenPath is where the sandbox finds its ONE credential: the token
// that opens the project's root repository (ADR-0028 §3). A projected FILE and
// not an environment variable — environ is inherited by every child process,
// and the sandbox runs agent code.
//
// `/etc/dop` and not `/var/run/dop`: on Alpine `/var/run` is a SYMLINK to
// `/run`, and Docker's archive extractor refuses to replace a symlink with a
// directory — the token would never land, and the sandbox would come up with
// no way to clone.
const SandboxTokenPath = "/etc/dop/git-token"

// SandboxSessionsPath is where the agent's tool writes its session files, and
// where the collector reads them. Both containers mount it; only the collector
// has to be told, because the tool writes where it always writes.
const SandboxSessionsPath = "/sessions"

// SandboxCollector is the sidecar's configuration (P-23 phase 1).
//
// Note what is here and what is NOT: an image, an address and a key — and no
// database, no vault, no provider credential. The collector's whole authority is
// to write telemetry for one account, and the shape of this struct is what says
// so.
type SandboxCollector struct {
	Image      string
	CoreTarget string
	// Key signs the collector's assertions (ADR-0029). It is mounted ONLY in the
	// collector's container: the agent's never sees it.
	Key       string
	AccountID string
	DemandID  string
	ProjectID string
}

// SandboxRepository is the project's root repository as the sandbox sees it.
//
// The clone URL is not secret and travels in the environment
// (`DOP_PROJECT_REPO`); the token is, and travels as a file. Both are minted by
// the core at provisioning: the token is per demand, opens exactly this
// repository, read-write, and dies with the sandbox.
type SandboxRepository struct {
	CloneURL string
	Token    string
}

// SandboxHandle identifies an ALREADY provisioned sandbox. The ID belongs to the
// domain: the adapter never invents identity, it only stamps it on what it
// creates.
type SandboxHandle struct {
	ID        string
	Namespace string // dop-<short-id> — one per demand (spec §1)
}

// SandboxSpec is everything the executor needs to materialize a sandbox. Note
// what is NOT here: no kubeconfig, no socket, no runtimeClassName, no internal
// registry image name, no cgroup limit. That is vendor vocabulary and it lives
// on the far side of the port.
type SandboxSpec struct {
	SandboxHandle
	AccountID string
	DemandID  string
	Tier      IsolationTier
	Image     string
	// An empty Command means the image's entrypoint. It exists so the executor
	// can be exercised with a generic image in the contract suite — without it,
	// proving the guarantees would require a published devbox image, and the
	// suite would stop running on the laptop of whoever works on the adapter.
	Command []string
	Env     map[string]string
	// Collector, when set, raises a container BESIDE the agent that follows the
	// session files and pushes the consumption to the core (P-23 phase 1).
	//
	// It is in the spec and not a method of its own for the same reason the
	// repository is: on Kubernetes a sidecar is part of the pod, and a pod is
	// created once. "Add a container afterwards" is not a thing either executor
	// does.
	//
	// Empty means no collector — which is what the contract suite wants, and
	// what a sandbox raised for something other than measurement wants.
	Collector SandboxCollector

	// Repository is the project's root repository (ADR-0028). An empty CloneURL
	// means no shelf — the right thing for a sandbox raised by the contract
	// suite's lifecycle tests. With one, the adapter delivers the token at
	// SandboxTokenPath and the clone URL in the environment, and the image's
	// entrypoint clones (guarantees 18 to 23).
	Repository SandboxRepository
}

// SandboxPhase is what the EXECUTOR sees. It is not the domain's state: there
// is no "destroyed" here, because to the launcher destroyed and never-existed
// are the same thing — the memory of a destroyed sandbox lives in Postgres.
type SandboxPhase string

const (
	PhaseProvisioning SandboxPhase = "provisioning"
	PhaseActive       SandboxPhase = "active"
	PhaseSuspended    SandboxPhase = "suspended"
)

// SandboxEndpoint is a port published by the demand's stack.
//
// It has no URL on purpose: the URL is `<service>--<demand>.<domain>` (spec §5),
// and the ingress domain is installation policy, not a fact about the executor.
// If each adapter built the URL, the same naming rule would exist in two places
// and would diverge the first day the domain changed.
type SandboxEndpoint struct {
	Name  string
	Port  int32
	State string // running | stopped
}

type SandboxStatus struct {
	// Phase and Tier are what the executor IS delivering right now — not what
	// was asked for. It is that distinction that makes "declared, never
	// presumed" verifiable after provisioning, and not only at the moment of it.
	Phase     SandboxPhase
	Tier      IsolationTier
	Endpoints []SandboxEndpoint
}

// LogQuery selects what comes out of Tail. Service names a process INSIDE the
// sandbox (a pod's container, a compose service); empty = the main process.
type LogQuery struct {
	Service   string
	TailLines int  // 0 = everything the executor still holds
	Follow    bool // false = return what already exists and stop
}

// LogLine is a raw line from the executor. Classifying its origin (app, test,
// infra) is NOT here: that is a convention of what runs inside the sandbox, and
// it lives in the domain, where it changes in one place.
type LogLine struct {
	Service string
	// Stream is stdout or stderr — when the executor separates the two.
	// Kubernetes does not (it merges everything into the container log) and
	// always returns "stdout"; Docker separates them. That is why the contract
	// suite promises nothing about this field: depending on it means depending
	// on the adapter.
	Stream string
	Text   string
	At     time.Time
}

// ───────────────────────── command execution ─────────────────────────

// ExecRequest is ONE command to run inside an ACTIVE sandbox.
//
// Note what is NOT here, because the absence is the design:
//
//   - ENVIRONMENT VARIABLES. Docker accepts `Env` on `/exec/create`;
//     Kubernetes' `pods/exec` accepts nothing beyond the command. Honouring it
//     on k8s would mean prefixing `env K=V …` onto the argv — and a value in
//     argv is visible in the `ps` of any process in the sandbox, which is where
//     agent code runs. A capability that does not map stays out (ADR-0001), and
//     in this case staying out is also the safe choice: **exec has no field
//     through which a credential could arrive**. The ONE credential a sandbox
//     holds — the token to the project's root repository (ADR-0028 §3, the
//     execution spec §5) — enters at provisioning as a projected file, never
//     through exec, never as environ. It is the agent's own workbench key, not
//     a third party's; a third party's credential has no path in here at all;
//
//   - WORKING DIRECTORY. Same asymmetry: Docker has `WorkingDir`, k8s does not.
//     Instead of emulating it, both adapters fix the CONTAINER's working
//     directory to SandboxWorkspacePath at provisioning time, and exec inherits
//     it. That turns it into a uniform guarantee (14) instead of a field only
//     one adapter honours;
//
//   - STDIN, TTY and RESIZE. That is a SESSION, not a command: it is the
//     developer's terminal, which is dop-app's PTY (spec §5), and it stays
//     outside this port. What came in is the execution of ONE command, without
//     interaction, with an exit code — which is exactly what the agent's tool
//     loop needs.
type ExecRequest struct {
	// Command is argv. There is NO implicit shell: whoever wants a pipeline
	// passes ["sh","-c","…"] and the shell shows up in the audit trail instead
	// of hiding inside the adapter.
	Command []string
	// TimeoutSeconds is THIS command's ceiling. Zero uses DefaultExecTimeout.
	// Hitting it is not a port error — see guarantee 17.
	TimeoutSeconds int
	// MaxOutputBytes is the ceiling of EACH stream (stdout and stderr,
	// separately). Zero uses DefaultExecMaxOutputBytes. Tool output becomes
	// model context, and context is money (ADR-0011): an uncapped `cat` of a
	// 200 MB log is not a memory problem, it is an invoice.
	MaxOutputBytes int
}

// ExecResult is what the command produced. Note there is no error field:
// failure of the COMMAND is a result, not a failure of the port (guarantee 15).
type ExecResult struct {
	// ExitCode is the process's code. -1 means there was NO code: the process
	// did not finish (TimedOut) or the executor could not tell. Zero would
	// assert success, which is a different thing.
	ExitCode int
	Stdout   string
	Stderr   string
	// Truncated: one of the streams hit MaxOutputBytes. Whoever assembles the
	// result for the model MUST say so — an agent that draws conclusions from
	// silently cut output draws the wrong ones.
	Truncated bool
	// TimedOut: the command did not finish within the deadline. What already
	// came out is delivered: a command that hung after printing the essentials
	// still informs.
	TimedOut bool
}

const (
	// DefaultExecTimeout is a command's deadline when the caller does not pick
	// one. It lives in the PORT, and not in each adapter, because a per-adapter
	// default would give the same command different deadlines depending on where
	// the sandbox came up — the very divergence the contract suite exists to
	// catch.
	DefaultExecTimeout = 2 * time.Minute
	// DefaultExecMaxOutputBytes is the per-stream output ceiling. 64 KiB is on
	// the order of 16 thousand tokens: it fits in a turn without dominating it.
	DefaultExecMaxOutputBytes = 64 << 10
)

// SandboxLauncher is the executor where a demand executes: a microVM or a
// container holding the agent, the workspace and an inner Docker (execution spec
// §1).
//
// Two things live inside a sandbox, and the whole port turns on the difference
// between them: the EXECUTION, ephemeral and cheap to recreate, and the
// WORKSPACE, which is the demand's work and does not get recreated. Suspending
// tears down the first and preserves the second; destroying takes both and does
// not come back.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//
//  1. Launch delivers the REQUESTED tier or fails. Status.Tier always equals
//     Spec.Tier when the error is nil — silent degradation is forbidden, and a
//     tier the executor does not offer becomes KindPrecondition with a message
//     saying what is missing;
//  2. a refused tier leaves NO trace: after the refusal, Describe returns
//     KindNotFound. A refusal that provisions half is worse than no refusal;
//  3. SupportedTiers answers what THIS executor offers now — that is what lets
//     the domain refuse before writing state. It never returns an empty list
//     with no error: a executor offering no tier at all is an unavailable
//     executor (KindUnavailable);
//  4. Launch is IDEMPOTENT by SandboxHandle.ID: relaunching the same spec
//     returns the existing sandbox instead of creating a second one. Without
//     that, a network retry would duplicate a microVM — and the bill arrives at
//     the end of the month;
//  5. Suspend tears down the execution and PRESERVES everything under
//     SandboxWorkspacePath. Nothing OUTSIDE that path is promised: the k8s
//     adapter deletes the whole pod on suspension and only the PVC survives,
//     while Docker keeps the container's writable layer. Promising what only one
//     delivers would be the abstraction leaking;
//  6. Resume recreates the execution OVER the existing workspace and returns the
//     active phase. After Resume, what was in the workspace is still there;
//  7. Suspend and Resume are idempotent: suspending a suspended sandbox and
//     resuming an active one neither error nor change anything;
//  8. Destroy is IRREVERSIBLE and idempotent: it takes execution and workspace,
//     and destroying what does not exist returns nil. After it, Describe returns
//     KindNotFound and Resume REFUSES — there is no way back through the port;
//  9. Describe, Suspend and Resume of a nonexistent sandbox return KindNotFound.
//     Only Destroy treats absence as success, because only there is absence the
//     desired outcome;
//  10. sandboxes coexist without interference: an operation on one never alters
//     or reveals the other, even with the same image and the same command
//     (spec §1);
//  11. Tail delivers the sandbox's lines and DIES WITH the caller: with the
//     context cancelled, it returns without error and without leaving a live
//     goroutine. With Follow=false, it returns at the end of what exists;
//  12. an error from emit interrupts Tail and propagates — that is how the
//     server finds out the client is gone;
//  18. a Spec.Repository sandbox has the project's root repository CLONED at
//     SandboxDocumentsPath when it comes up: a file committed before the launch
//     is readable, byte for byte. With no repository the path is EMPTY — the
//     image creates the directory, and an empty directory says "no shelf" as
//     clearly as an absent one;
//  19. the shelf is WRITABLE, and SHARED: a commit pushed from one sandbox is
//     visible to a sandbox of the same project launched afterwards. It is what
//     "collaborated between agents" means (ADR-0028);
//  20. the shelf PERSISTS: what a sandbox pushed is still there after its own
//     Suspend/Resume, and after its Destroy — the repository outlives the
//     sandbox, it is the project's;
//  21. the token opens EXACTLY ONE repository: a sandbox of project X cannot
//     clone, fetch or push project Y's, even holding a valid token of its own;
//  22. the token is a FILE at SandboxTokenPath, not an environment variable:
//     `env` inside the sandbox does not contain it;
//  23. the mirror's credential — the user's remote — is never in a sandbox: no
//     file, no variable, nothing. The platform mirrors; the sandbox does not.
//
// ── EXEC: why it CAME INTO the port (and what stays out) ─────────────────────
//
// Until the tool loop shipped, this port declared that exec stayed out: "k8s
// requires a connection upgrade (SPDY/WebSocket) with its own stream semantics;
// Docker uses HTTP hijacking". That is true and remains true — but it is a
// statement about TRANSPORT, and transport is exactly what an adapter exists to
// absorb. This house's rule is a different one: *what stays out is what cannot
// be delivered by ALL adapters*. Running a command inside the sandbox and
// returning its output and exit code is deliverable by both — k8s through
// `pods/exec` over WebSocket (channels 1/2/3: stdout, stderr, status with the
// exit code), Docker through `/exec/create` + `/exec/start` with the same
// multiplexed stream Tail already unpacks. The right criterion rejects the
// exclusion.
//
// The exclusion was also expensive: without exec in the port, the agent TALKS
// and does not ACT. The alternative path — a process inside the sandbox exposing
// an API for the core to call — is worse at both ends: it would require the
// sandbox to be reachable (spec §5 says the opposite: the agent speaks outward)
// and it would require a credential INSIDE the sandbox to authenticate that
// call, which is precisely what the sandbox must not carry.
//
// What stays out is the SESSION: stdin, TTY, resize, bidirectional streaming.
// That is the developer's terminal, that is dop-app's PTY (spec §5), and that
// really does have semantics which do not map. What came in is the narrow
// slice: one command, no interaction, with a deadline, an output ceiling and an
// exit code.
//
// Exec's guarantees, also verified in BOTH adapters:
//
//  13. Exec runs INSIDE the requested sandbox: the command sees the workspace
//     under SandboxWorkspacePath and the same filesystem as the current
//     execution;
//  14. the command starts in SandboxWorkspacePath. It is not a request field
//     (see ExecRequest): it is the container's working directory, fixed at
//     provisioning time by both adapters;
//  15. A NON-ZERO EXIT CODE IS NOT A PORT ERROR. It comes back in
//     ExecResult.ExitCode with a nil error. This is the guarantee that holds up
//     the agent's tool loop: the model has to SEE that the command failed in
//     order to fix it, and a transport error in its place would erase the
//     difference between "the test failed" and "the executor went down";
//  16. stdout and stderr arrive SEPARATED. Unlike Tail — where k8s merges the
//     two into the container log and the port promises nothing — here both
//     executors genuinely separate them: channels 1 and 2 on k8s, frames 1 and
//     2 on Docker;
//  17. output is CAPPED and the deadline is RESPECTED, and neither is an error:
//     Truncated and TimedOut are result fields. A command that dumps megabytes
//     is cut; a command that hangs is abandoned with what already came out;
//  18. Exec on a nonexistent sandbox is KindNotFound and on a SUSPENDED sandbox
//     is KindPrecondition — never a made-up exit code. A executor with no
//     execution runs no command, and saying that is different from saying the
//     command failed.
//
// OUT of the port, on purpose:
//
//   - an INTERACTIVE SESSION in the sandbox (stdin, TTY, resize). See above: it
//     is dop-app's PTY, and bidirectional stream semantics do not map;
//   - microVM SNAPSHOT/restore. Kata's support is limited and the design does
//     not depend on it (spec §3): it would come in as a capability the domain
//     would eventually assume exists;
//   - cpu/memory LIMITS. They do not map between a Docker cgroup and a namespace
//     ResourceQuota with a LimitRange without becoming the lowest common
//     denominator of whichever vendor inspired the port first.
type SandboxLauncher interface {
	SupportedTiers(ctx context.Context) ([]IsolationTier, error)
	Launch(ctx context.Context, spec SandboxSpec) (*SandboxStatus, error)
	Suspend(ctx context.Context, h SandboxHandle) error
	Resume(ctx context.Context, spec SandboxSpec) (*SandboxStatus, error)
	Destroy(ctx context.Context, h SandboxHandle) error
	Describe(ctx context.Context, h SandboxHandle) (*SandboxStatus, error)
	Tail(ctx context.Context, h SandboxHandle, q LogQuery, emit func(LogLine) error) error
	// Exec runs ONE command inside the sandbox and waits for it to finish.
	//
	// The error is reserved for a EXECUTOR failure (nonexistent sandbox,
	// suspended sandbox, cluster down). Everything the command did — including
	// failing — comes back in ExecResult.
	Exec(ctx context.Context, h SandboxHandle, req ExecRequest) (*ExecResult, error)
}

// ───────────────────────── VerificationRunner ─────────────────────────

// RunnerDependency is a third party the application needs while it is verified:
// a database, a cache, a broker. Always a PUBLISHED image, pulled and never
// built (ADR-0030 §1).
//
// It is reachable at `localhost:<port>`, on EVERY executor: on Kubernetes it is
// a container of the same pod, and on Docker it joins the runner's network
// namespace. The same address on both is the point — an application configured
// for one executor and broken on the other would be compose's translation
// problem coming back under another name. Name is what the run calls it in an
// error message, not an address.
type RunnerDependency struct {
	Name  string
	Image string
	Port  int32
	Env   map[string]string
	// Ready is an optional shell command, run IN THE RUNNER, that has to succeed
	// before the application starts. Empty means readiness is the TCP port
	// accepting a connection.
	Ready string
}

// RunnerRepository is the customer's code: where to clone it from, with what,
// and at which commit.
//
// The three travel together because none of them is useful alone — and because
// the commit is what makes the evidence worth anything (ADR-0007: evidence that
// does not say which code it ran on is not evidence).
type RunnerRepository struct {
	CloneURL string
	Token    string
	Commit   string
}

// RunnerStepKind is what a step IS, which is what decides how its failure reads.
// A build that did not compile is not a test failure, and reporting it as one
// sends whoever reads the evidence to the wrong place.
type RunnerStepKind string

const (
	StepSetup RunnerStepKind = "setup" // clone, checkout, dependencies up
	StepBuild RunnerStepKind = "build"
	StepStart RunnerStepKind = "start"
	StepCheck RunnerStepKind = "check"
)

// RunnerStep is one command in the sequence. Command is a SHELL line, run by the
// runner image's shell: it is what the project wrote in its manifest, and
// splitting it into argv here would break every pipe and glob a build command
// legitimately uses.
type RunnerStep struct {
	Name    string // "build", "aaa", "e2e" — what shows up in the evidence
	Kind    RunnerStepKind
	Command string
}

// RunnerStepResult is what a step DID. Note that a non-zero exit code is not an
// error of the port, for the same reason it is not one in Exec: the domain has
// to be able to tell "the tests failed" from "the executor went down".
type RunnerStepResult struct {
	Name      string
	Kind      RunnerStepKind
	ExitCode  int
	StartedAt time.Time
	EndedAt   time.Time
}

func (r RunnerStepResult) Failed() bool { return r.ExitCode != 0 }

type RunnerPhase string

const (
	RunnerPending RunnerPhase = "pending" // the environment is coming up
	RunnerRunning RunnerPhase = "running" // the sequence is executing
	// RunnerHolding is a DEV SESSION: every step ran, there were no checks, and
	// the application is up waiting for a person (verification-runner spec §4).
	// It is a phase and not a flag because what ends it is different — a
	// deadline or a person, never the absence of work.
	RunnerHolding   RunnerPhase = "holding"
	RunnerSucceeded RunnerPhase = "succeeded"
	RunnerFailed    RunnerPhase = "failed"
)

// RunnerHandle identifies an already created run. As with the sandbox, the ID
// belongs to the domain: the adapter never invents identity.
//
// Namespace is the ACCOUNT's space, not the run's — and that difference is the
// cache. The account's cache volume lives in this space, and a volume cannot be
// mounted across namespaces, so a space per run would give every run a cold
// cache: guarantee 3 would be unimplementable on Kubernetes while quietly
// passing on Docker. Runs of one account therefore share the space and are told
// apart by ID.
type RunnerHandle struct {
	ID        string
	Namespace string
}

// RunnerSpec is everything the executor needs to materialize a run.
//
// What is NOT here is the same list as the sandbox's — no kubeconfig, no socket,
// no compose file — plus one more: there is no image OF THE PROJECT. The runner
// image is ours and carries the toolchains; the project arrives as a commit and
// is built inside it. Nothing is ever pushed to a registry (ADR-0030).
type RunnerSpec struct {
	RunnerHandle
	AccountID    string
	DemandID     string
	Image        string // the PLATFORM's runner image, with the toolchains
	Repository   RunnerRepository
	Dependencies []RunnerDependency
	// Steps is the sequence, in order. A spec with no StepCheck is a dev
	// session: the run holds after StepStart instead of finishing.
	Steps []RunnerStep
	// AppPort is the port the application answers on, published for a dev
	// session. Zero means the run starts no application.
	AppPort int32
	Env     map[string]string
	// CachePaths are paths inside the runner backed by the ACCOUNT's cache
	// volume. Never global: a cache shared between accounts is a side channel.
	CachePaths []string
}

// RunnerEndpoint is the port a dev session published. As with the sandbox, no
// URL: `<service>--<demand>.<domain>` is installation policy, and building it in
// two places is how the naming rule diverges.
type RunnerEndpoint struct {
	Port  int32
	State string // running | stopped
}

type RunnerStatus struct {
	Phase RunnerPhase
	Steps []RunnerStepResult
	// FailedStep names the step that stopped the sequence, empty if none. It is
	// a name and not an index because the evidence quotes it.
	FailedStep string
	Endpoint   *RunnerEndpoint
}

// VerificationRunner is where a verification runs, and where an application runs
// at all (ADR-0030, `verification-runner.md`).
//
// It is NOT the sandbox and the difference is the whole point. The sandbox is
// the bench: a dirty working tree with whatever the agent installed along the
// way. A runner starts from nothing and builds a COMMIT, which is what makes its
// green a statement about the code instead of a statement about the agent's
// afternoon.
//
// The application exists in exactly two windows: during a verification, and
// while a developer asked to look at it. There is no third — a demand does not
// keep an application running.
//
// ── How the sequence executes, and why it is not driven from here ────────────
//
// The steps run as the runner container's MAIN PROCESS, from a script the
// adapter generates, and each result comes back as a marked line on stdout that
// Status parses out of the log. The obvious alternative — the core driving step
// after step over exec — was rejected for one reason: it puts the run's state in
// a goroutine, and a core that restarts mid-verification would leave a container
// running with nobody to read it. Here the container IS the run; the core can
// die and come back and still find out what happened.
//
// The marker carries a nonce derived from the handle, so a build that prints
// something shaped like a result cannot forge one.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//
//  1. a run builds the COMMIT it was given, not the branch's tip: the same
//     commit twice produces the same tree;
//  2. a runner starts from NOTHING — a file a previous run wrote is not there.
//     It is the guarantee that separates a runner from the bench;
//  3. the CACHE is there: a second run of the same account finds under
//     CachePaths what the first one left;
//  4. a failing step STOPS the sequence, and Status names it in FailedStep. The
//     steps after it did not run and do not appear as passed;
//  5. a non-zero exit code is NOT a port error: it comes back in
//     RunnerStepResult.ExitCode with Status returning nil. A transport error in
//     its place would erase the difference between "the tests failed" and "the
//     cluster went down";
//  6. dependencies are READY before the start step, and one that never becomes
//     ready fails the run with a message naming it — never a mysterious
//     connection refused in the application's log;
//  7. Logs streams the run's output and DIES WITH the caller: with the context
//     cancelled it returns without error and leaves no live goroutine;
//  8. Destroy is IRREVERSIBLE and idempotent: destroying what does not exist
//     returns nil, and after it Status returns KindNotFound and the endpoint
//     answers nothing;
//  9. runs coexist without interference: two runs of the same commit, at the
//     same time, neither see nor alter each other — including two runs of the
//     SAME account, which share a space and a cache volume;
//  10. a spec with NO check steps starts and HOLDS in RunnerHolding — that is a
//     dev session. What ends it is Destroy, never the absence of work.
//
// OUT of the port, on purpose:
//
//   - the DEADLINE of a dev session. It is a policy decision (how long is a
//     session worth paying for), it is the same on every executor, and putting
//     it here would give two adapters two chances to disagree about a clock;
//   - the ADDRESS. RunnerEndpoint carries a port; the URL is built where the
//     ingress domain is known, exactly as for the sandbox;
//   - BUILDING AND PUSHING AN IMAGE of the project. It is not missing, it is
//     refused: the sequence this port exists to remove is `build → push → pull`
//     (ADR-0030 §1);
//   - cpu/memory LIMITS, for the same reason the sandbox refuses them.
//
// A LIMIT worth knowing before it surprises somebody: the account's cache is one
// ReadWriteOnce volume, so two concurrent runs of one account have to land on
// the same node. On a single-node cluster that is free; on several, the second
// run waits for a schedulable node. Making it ReadWriteMany would need a storage
// class most installations do not have — the same wall that killed the shared
// volume in ADR-0028.
type VerificationRunner interface {
	// Start creates the environment and begins the sequence. It returns as soon
	// as the run exists — a verification takes minutes, and a call that blocks
	// for minutes is a call that times out somewhere else.
	Start(ctx context.Context, spec RunnerSpec) (*RunnerStatus, error)
	// Status is the run's progress. The error is reserved for a EXECUTOR
	// failure; everything the run did, including failing, is in RunnerStatus.
	Status(ctx context.Context, h RunnerHandle) (*RunnerStatus, error)
	Logs(ctx context.Context, h RunnerHandle, q LogQuery, emit func(LogLine) error) error
	Destroy(ctx context.Context, h RunnerHandle) error
}

// ───────────────────────── Mailer ─────────────────────────

// Mail is the INTENT to notify someone — never the notification artifact.
//
// Note what is NOT here: subject, body, HTML, `template_id`. ADR-0025 decided
// that the template index, its resolution and its rendering live in the
// ADAPTER, and this struct is what remains once that leaves: "THIS happened, to
// THIS address, with THIS data". Rendering in the domain would look cleaner and
// would be worse — the platform could never use a provider template (losing its
// editor, versioning and localization) and the port would start carrying a blob
// of HTML, which is a rendering artifact, not an intent.
type Mail struct {
	// AccountID is the account on whose behalf the notice goes out. It reaches
	// the adapter because one day the sender will be per account (a verified
	// domain), not in order to filter.
	AccountID string
	// Kind is the notification's LOGICAL TYPE, in the domain's vocabulary
	// ("invite", "attention_digest"). It is a string, and not the named type
	// from the notification package, because `notification` imports `ports` (for
	// the clock) and the way back would be an import cycle.
	Kind string
	// To is ONE address. Fan-out is the rule's decision, not the channel's: a
	// `[]string` here would make the adapter choose between one message with
	// everybody in copy — which leaks the addresses to each other — and N
	// messages, which is what the caller already knows how to do.
	To     string
	ToName string
	// Data is the template's data, free-form. The adapter decides what to do
	// with it: SendGrid sends it as `dynamic_template_data`, SMTP renders
	// locally. A missing key is NOT an error (guarantee 8).
	Data map[string]any
}

// MailState is the outcome of a send, in the vocabulary the sibling project has
// already proven useful in operation.
type MailState string

const (
	// MailSent: the provider accepted the message.
	MailSent MailState = "sent"
	// MailSentLocal: a REHEARSAL. There was no credential, the adapter printed
	// instead of sending and nobody received anything. It is a state of its own,
	// and not `sent`, because the difference between "we notified" and "we
	// pretended to notify" cannot depend on whoever reads the log remembering
	// which environment that ran in.
	MailSentLocal MailState = "sent_local"
)

// MailReceipt is the proof of send. No body, no HTML, no provider response:
// only what the caller needs in order to RECORD what happened.
type MailReceipt struct {
	State MailState
	// Provider identifies who served it ("sendgrid", "smtp"). It serves
	// operations: "it never arrived" is a different investigation depending on
	// who sent it.
	Provider string
	// Reference is the provider's id when there is one (SendGrid's
	// `X-Message-Id`). Empty is NORMAL — SMTP has nothing to give back — and
	// that is why nothing in the domain may depend on this field.
	Reference string
}

// Mailer is the email CHANNEL. The trigger — deciding what to notify and to
// whom — belongs to the domain (internal/domain/notification); only the dispatch
// happens here.
//
// The port is per CHANNEL, and not one port for everything, because channels do
// not share a shape: email has a subject, HTML and attachments; push has a
// title, a badge and a deep link; SMS has 160 characters and no formatting. A
// single port would carry the union of all of it — with most fields never used —
// or the lowest common denominator, losing what each channel does well. `Pusher`
// and `SMSer` are born when there is push and SMS (ADR-0025).
//
// Guarantees verified by the contract suite, in EVERY adapter:
//
//  1. COMPLETE RESOLUTION: the adapter resolves EVERY type the domain can emit.
//     It is this port's most important guarantee, and the only one whose
//     violation is invisible: a type that exists in the policy and has no
//     template at the provider makes the event happen, the consumer run, and
//     NOBODY receive anything. A type outside the adapter's index is
//     KindNotFound, in Send and in Resolve — never a generic message, never a
//     silent success;
//
//  2. Resolve answers the same thing Send would resolve, with NO I/O and
//     WITHOUT sending. It exists so the contract suite (and the composition
//     root, at boot) can ask "do you know how to build this?" without mailing
//     anyone. The two answers AGREE: a type Resolve accepts, Send does not
//     refuse for want of a template, and vice versa;
//
//  3. LOCAL REHEARSAL: with no credential configured, the adapter does NOT talk
//     to the provider — it prints the message and returns MailSentLocal. It is
//     the sibling project's development mode, and it happens AFTER template
//     resolution, never before: a rehearsal that skipped resolution would hide
//     exactly the defect of guarantee 1 in every environment without a key,
//     which is where the suite runs;
//
//  4. the SECRET does not get out: an API key and an SMTP password never appear
//     in an error, in a log, in the adapter's String() or in a `%+v` of it. It
//     is not a promise of discipline — the credential is captured in a CLOSURE,
//     not held in a field, because fmt reads unexported fields by reflection and
//     cannot call their String();
//
//  5. an empty recipient, one without "@", or an empty kind are KindInvalid
//     decided with NO I/O: a caller with no address must not cost a round trip
//     to the provider;
//
//  6. with a nil error, State is ALWAYS MailSent or MailSentLocal — never
//     empty, never another value. An empty state would become a record that
//     does not say whether anyone received anything;
//
//  7. error translation is the house's: provider down, network, deadline
//     exceeded and unreadable response are KindUnavailable; a refused credential
//     is KindUnauthorized; content or an address refused by the provider is
//     KindInvalid. Confusing the first two groups sends the team hunting the
//     defect in the wrong place;
//
//  8. Data is OPTIONAL and free-form. A missing key is NOT an error — the
//     template decides what to do with the gap, and dropping an invite because a
//     cosmetic field did not arrive would trade a cosmetic problem for an access
//     blocker;
//
//  9. Send writes NOTHING and has no side effect beyond the send itself:
//     recording what was dispatched belongs to the caller (ADR-0025), and an
//     adapter that also recorded would have two responsibilities, one of them
//     impossible to test without a database;
//
//  10. safe for concurrent use.
//
// OUT of the port, on purpose:
//
//   - SUBJECT and BODY. They are rendering artifacts; see Mail;
//   - ATTACHMENTS. SendGrid accepts base64 in the JSON body, SMTP requires MIME
//     multipart, and no platform notice needs an attachment today. A capability
//     that does not map and that nobody uses does not get in;
//   - SCHEDULING (SendGrid's `send_at`). It does not exist in SMTP, and
//     scheduling is policy — the digest delay is a domain decision, tunable, and
//     it cannot depend on which provider served the call;
//   - open and click TRACKING, and later status lookups. It is asymmetric across
//     providers and would bring marketing vocabulary into a port that only needs
//     to notify people.
type Mailer interface {
	Send(ctx context.Context, m Mail) (*MailReceipt, error)
	Resolve(ctx context.Context, kind string) error
}

// ───────────────────────── SMSer ─────────────────────────

// SMS is ONE text message. Note what is not here, next to Mail: no subject, no
// HTML, no attachment, no template data. SMS has 160 characters and no
// formatting, and a port that carried e-mail's fields would be a port with most
// of its fields always empty.
//
// The text arrives READY. It is the opposite of Mail, and the difference is not
// an inconsistency — it is what the two channels are. E-mail has a template at
// the provider, with an editor and versioning, and that is why the port speaks
// intent; SMS has one line, the same in every provider, and inventing a
// template index for one line would be ceremony with no editor to preserve.
type SMS struct {
	// AccountID is the account on whose behalf it goes out. It reaches the
	// adapter because the sender may one day be per account, not to filter.
	AccountID string
	// To is ONE destination in E.164 ("+5511999999999"). One, for Mail's same
	// reason: fan-out is the caller's decision.
	To string
	// Text is what the person reads. The adapter does not render, does not
	// prefix and does not append — whoever sends is who knows what the message
	// says.
	Text string
}

// SMSState mirrors MailState, with the same vocabulary, so that operations
// reads both the same way.
type SMSState string

const (
	SMSSent SMSState = "sent"
	// SMSSentLocal is a REHEARSAL: no credential, the adapter printed instead of
	// sending. Local has no SMS gateway, and the difference between "we sent"
	// and "we pretended to" cannot depend on whoever reads the log remembering
	// the environment.
	SMSSentLocal SMSState = "sent_local"
)

type SMSReceipt struct {
	State SMSState
	// Provider identifies who served it ("twilio", "zenvia"): "it never
	// arrived" is a different investigation depending on the carrier.
	Provider string
	// Reference is the provider's id when there is one. Empty is normal, and
	// nothing in the domain may depend on it.
	Reference string
}

// SMSer is the SMS CHANNEL — the port ADR-0025 left foreseen and ADR-0027
// brought into being, when the second factor made SMS exist.
//
// It is a channel and not a notifier: what decides that a message goes out is
// the domain. And unlike Mailer, its caller is NOT the Notifier — a
// second-factor code is a CHALLENGE the person is waiting for, not a
// notification that interrupts them, so it does not pass through the policy
// table, the digest or the delay (ADR-0027 §3).
//
// Guarantees verified by the contract suite, in EVERY adapter:
//
//  1. an empty destination, one not in E.164 (no leading "+", non-digits, fewer
//     than 8 or more than 15 digits) and an empty text are KindInvalid decided
//     with NO I/O: a caller with a broken number must not cost a round trip, and
//     with SMS it must not cost money either;
//
//  2. LOCAL REHEARSAL: with no credential configured, the adapter does NOT talk
//     to the provider — it prints the message and returns SMSSentLocal. It is
//     the local environment's only mode: there is no SMS emulator, and the path
//     is exercised even though the delivery is not (P-35);
//
//  3. the SECRET does not get out: the token, the auth string and the account
//     SID never appear in an error, in a log, in the adapter's String() or in a
//     `%+v` of it. Captured in a CLOSURE, not held in a field — fmt reads
//     unexported fields by reflection;
//
//  4. with a nil error, State is ALWAYS SMSSent or SMSSentLocal, never empty;
//
//  5. error translation is the house's: provider down, network, deadline
//     exceeded and unreadable response are KindUnavailable; a refused credential
//     is KindUnauthorized; a number or text refused by the provider is
//     KindInvalid. The distinction decides whether the code is retried or the
//     person is told to fix the number;
//
//  6. Send writes NOTHING and has no side effect beyond the send: recording
//     belongs to the caller;
//
//  7. the TEXT does not reach the log. It carries the one-time code, and a code
//     in a log is a live credential at rest for as long as it is valid. The
//     rehearsal of guarantee 2 is the ONE exception, and it exists precisely
//     because there is no gateway locally — it prints where there is nobody to
//     receive;
//
//  8. safe for concurrent use.
//
// OUT of the port, on purpose:
//
//   - DELIVERY STATUS (Twilio's callbacks, Zenvia's webhooks). It is
//     asymmetric, it arrives minutes later through another channel, and nothing
//     in the second factor waits for it: what proves the code arrived is the
//     person typing it;
//   - the SENDER (a long code, a short code, an alphanumeric ID). It is
//     installation configuration, per provider and per country, not something
//     the domain chooses per message;
//   - scheduling, and message parts/concatenation. A code fits in one message.
type SMSer interface {
	Send(ctx context.Context, m SMS) (*SMSReceipt, error)
}

// ───────────────────────── Clock and IDs ─────────────────────────
// Small, but real: this is what makes the domain deterministic in tests.

// Clock is the domain's ONLY source of "now".
//
// Guarantees verified by the contract suite, in EVERY adapter:
//  1. Now never returns the zero instant;
//  2. Now always returns UTC — an instant with no defined zone is an ambiguity
//     that turns into a comparison bug when the process and the database
//     disagree about TZ;
//  3. Now does not go backwards between successive calls;
//  4. safe for concurrent use.
//
// The domain receives the port and NEVER calls time.Now() internally, not even
// as a fallback for a nil clock: the fallback switches the port off without
// anyone noticing and hands the test back the wall-clock dependency the port
// exists to remove. A service that needs time REQUIRES the clock in its
// constructor.
type Clock interface{ Now() time.Time }

type IDGenerator interface{ NewID() string }

// ───────────────────────── ProjectRepository ─────────────────────────

// ProjectRepository is the home of a project's knowledge: a git repository the
// platform hosts, born with the project (ADR-0028).
//
// It is a port for the usual reason — two adapters, one contract suite — and
// for a specific one: WHERE the repositories live (a directory on the core's
// disk, a service of its own, one day a managed git) is a deployment fact, and
// the domain only ever needs five verbs.
//
// Guarantees verified by the contract suite, in EVERY adapter:
//
//  1. Ensure is IDEMPOTENT by project: the second call returns the same clone
//     URL and touches nothing. A project has one root repository, ever;
//  2. a repository is born with a first commit holding the layout's manifest,
//     so a clone is never empty — an empty clone tells the agent "this project
//     knows nothing", which is a different statement from "this project is
//     new";
//  3. IssueToken returns a credential that clones, fetches and pushes THAT
//     project's repository over the clone URL — and NOTHING else: another
//     project's repository refuses it (guarantee 21 of the sandbox port hangs
//     off this one);
//  4. a token EXPIRES: after its TTL the same operations are refused. A demand's
//     token is a demand's, not a standing key;
//  5. Commit writes files on the platform's behalf and returns the commit: a
//     Read after it returns the content, and a clone after it contains it.
//     Attribution follows ADR-0003, and it is the caller's to state;
//  6. Read of a path that does not exist is KindNotFound; Read never invents;
//  7. a push through the clone URL reaches the platform: OnPush is called with
//     the project, the ref and the commits, AFTER the refs are updated — it is
//     how a push becomes an event (ADR-0006), and how the manifest gets
//     regenerated;
//  8. a mirror set with SetMirror receives what is pushed, and the mirror's
//     credential is resolved by the ADAPTER from the SecretStore — it never
//     appears in a clone URL, a token or a sandbox.
type ProjectRepository interface {
	Ensure(ctx context.Context, projectID string) (RepositoryInfo, error)
	IssueToken(ctx context.Context, projectID, demandID string, ttl time.Duration) (string, error)
	Commit(ctx context.Context, projectID string, c RepositoryCommit) (string, error)
	Read(ctx context.Context, projectID, path string) ([]byte, error)
	SetMirror(ctx context.Context, projectID, remoteURL string, credential SecretRef) error
	// OnPush registers the callback of guarantee 7. One per process; the
	// adapter calls it synchronously after the refs are updated.
	OnPush(fn func(ctx context.Context, p Push))
}

// RepositoryInfo is what Ensure answers.
type RepositoryInfo struct {
	ProjectID string
	// CloneURL is what a sandbox clones — reachable from inside the execution
	// executor. It is not secret.
	CloneURL string
}

// RepositoryFile is one file of a platform-side commit.
type RepositoryFile struct {
	Path    string
	Content []byte
	// Delete marks a removal: the file leaves the tree in this commit.
	Delete bool
}

// RepositoryCommit is a write on the platform's behalf. Author and committer
// follow ADR-0003: the author is the person for whom the work is done, the
// committer is who wrote it down — here, the platform.
type RepositoryCommit struct {
	Files          []RepositoryFile
	Message        string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
}

// Push is what OnPush receives: enough to make an event, no more. The commits
// are ids; whoever needs the diff reads the repository.
type Push struct {
	ProjectID string
	// DemandID is the demand whose token pushed — empty for a platform commit.
	DemandID string
	Ref      string
	Before   string
	After    string
}
