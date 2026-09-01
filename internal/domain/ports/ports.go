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

type Event struct {
	ID          string
	AccountID   string
	Aggregate   string
	AggregateID string
	Type        string
	Payload     []byte
	OccurredAt  time.Time
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

// IsolationTier is the isolation level of the substrate where a demand runs.
//
// It is DECLARED, never presumed. Whoever provisions says which one they want;
// the launcher delivers EXACTLY that or refuses. There is no silent
// degradation: a sandbox that asked for a microVM and got a container still
// looks healthy, and the difference only shows up on the day of the incident.
// A Kata RuntimeClass is missing from most distributions (substrate spec §2 and
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

// SandboxHandle identifies an ALREADY provisioned sandbox. The ID belongs to the
// domain: the adapter never invents identity, it only stamps it on what it
// creates.
type SandboxHandle struct {
	ID        string
	Namespace string // dop-<short-id> — one per demand (spec §1)
}

// SandboxSpec is everything the substrate needs to materialize a sandbox. Note
// what is NOT here: no kubeconfig, no socket, no runtimeClassName, no internal
// registry image name, no cgroup limit. That is vendor vocabulary and it lives
// on the far side of the port.
type SandboxSpec struct {
	SandboxHandle
	AccountID string
	DemandID  string
	Tier      IsolationTier
	Image     string
	// An empty Command means the image's entrypoint. It exists so the substrate
	// can be exercised with a generic image in the contract suite — without it,
	// proving the guarantees would require a published devbox image, and the
	// suite would stop running on the laptop of whoever works on the adapter.
	Command []string
	Env     map[string]string
}

// SandboxPhase is what the SUBSTRATE sees. It is not the domain's state: there
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
// and the ingress domain is installation policy, not a fact about the substrate.
// If each adapter built the URL, the same naming rule would exist in two places
// and would diverge the first day the domain changed.
type SandboxEndpoint struct {
	Name  string
	Port  int32
	State string // running | stopped
}

type SandboxStatus struct {
	// Phase and Tier are what the substrate IS delivering right now — not what
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
	TailLines int  // 0 = everything the substrate still holds
	Follow    bool // false = return what already exists and stop
}

// LogLine is a raw line from the substrate. Classifying its origin (app, test,
// infra) is NOT here: that is a convention of what runs inside the sandbox, and
// it lives in the domain, where it changes in one place.
type LogLine struct {
	Service string
	// Stream is stdout or stderr — when the substrate separates the two.
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
//     in this case staying out is also the safe choice: **the port has no field
//     through which a credential could arrive**. It is not a promise of
//     discipline, it is the absence of a field;
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
	// did not finish (TimedOut) or the substrate could not tell. Zero would
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

// SandboxLauncher is the substrate where a demand executes: a microVM or a
// container holding the agent, the workspace and an inner Docker (substrate spec
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
//     tier the substrate does not offer becomes KindPrecondition with a message
//     saying what is missing;
//  2. a refused tier leaves NO trace: after the refusal, Describe returns
//     KindNotFound. A refusal that provisions half is worse than no refusal;
//  3. SupportedTiers answers what THIS substrate offers now — that is what lets
//     the domain refuse before writing state. It never returns an empty list
//     with no error: a substrate offering no tier at all is an unavailable
//     substrate (KindUnavailable);
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
//     server finds out the client is gone.
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
//     difference between "the test failed" and "the substrate went down";
//  16. stdout and stderr arrive SEPARATED. Unlike Tail — where k8s merges the
//     two into the container log and the port promises nothing — here both
//     substrates genuinely separate them: channels 1 and 2 on k8s, frames 1 and
//     2 on Docker;
//  17. output is CAPPED and the deadline is RESPECTED, and neither is an error:
//     Truncated and TimedOut are result fields. A command that dumps megabytes
//     is cut; a command that hangs is abandoned with what already came out;
//  18. Exec on a nonexistent sandbox is KindNotFound and on a SUSPENDED sandbox
//     is KindPrecondition — never a made-up exit code. A substrate with no
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
	// The error is reserved for a SUBSTRATE failure (nonexistent sandbox,
	// suspended sandbox, cluster down). Everything the command did — including
	// failing — comes back in ExecResult.
	Exec(ctx context.Context, h SandboxHandle, req ExecRequest) (*ExecResult, error)
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
