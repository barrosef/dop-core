// A SecretStore adapter over Google Cloud Secret Manager.
//
// It is the port's second REAL adapter (ADR-0001): it serves the platform when
// it runs on GCP, while its k8s sibling serves the self-hosted cluster. The SAME
// set of contract tests runs against both — that, and not the intention, is what
// makes the swap possible.
//
// Like the k8s one, it talks to the service through the API, and uses the
// OFFICIAL library: a hand-written client would get the happy cases right and
// get exactly the ones that matter wrong (error code, retry, checksum, alias
// resolution).
//
// ─────────────────── PORT → SECRET MANAGER MAPPING ───────────────────
//
// The port has Put/Get/Delete/Exists over a FLAT SecretRef. Secret Manager has
// two layers: the SECRET (a container, with a replication policy and IAM) and
// the VERSIONS (the material, immutable, numbered). Versioning is OUT of the
// port on purpose (see ports.SecretStore) — so the adapter has to hide the
// second layer completely. The decision:
//
//	Put    → CreateSecret (ignoring AlreadyExists)
//	         + AddSecretVersion
//	         + confirmation BY NUMBER (strong)
//	         + waiting for the `latest` alias (eventual)
//	         + destruction of the previous versions.
//	Get    → AccessSecretVersion on ".../versions/latest".
//	Delete → DeleteSecret (takes the container and ALL the versions).
//	Exists → Get != nil.
//
// Why Get reads `latest` and not a named version: the port has nowhere to keep a
// version number — SecretRef is flat and the domain knows no version. `latest`
// is the only stable address derivable from the reference alone. The price of
// that is in the consistency section, further down, and it is the most important
// discovery in this file.
//
// Why Put destroys the previous versions. Guarantee 4 says Put REPLACES. In the
// k8s adapter that is literal: the old value stops existing. If the old versions
// stayed here, the same reference would keep resolving the OLD secret by version
// number — and "I rotated the leaked credential" would start meaning different
// things in each adapter. A port whose meaning depends on who implements it is
// not a port.
//
// Why Delete erases the SECRET and not just the version. The port requires
// Delete to be idempotent and absence to be a normal state. Destroying only the
// version would leave behind an empty container with its own IAM — state
// invisible through the port, which nobody cleans up and which comes back as
// "it exists but has no value".
//
// Isolation between accounts (guarantee 5) comes out of the NAME, as in k8s: see
// secretID.
//
// ───────── CONSISTENCY: GUARANTEE 1 IS NOT DELIVERABLE ON REAL GCP ─────────
//
// The port promises IMMEDIATE read-after-write. Google documents the opposite,
// in https://cloud.google.com/secret-manager/docs/reference/consistency :
//
//   - "adding a secret version and then immediately accessing that secret
//     version BY VERSION NUMBER is a strongly consistent operation";
//   - "This doesn't apply when you access a secret version using aliases or
//     latest";
//   - "Other operations within Secret Manager are eventually consistent",
//     and they converge "typically within minutes, but may take a few hours".
//
// That is: the only strongly consistent path is the one the port CANNOT use,
// because it requires carrying the version number — and the version is exactly
// what SecretRef does not have. A Get right after a Put can legitimately return
// (nil, nil) on real GCP, which through the port means "it does not exist": the
// just-written credential would show up as absent.
//
// This is NOT an implementation defect and it is not fixable inside this file.
// It is a pending architecture decision — either the port starts returning a
// version identifier from Put for the domain to keep (and Get reads by number,
// strongly), or guarantee 1 becomes "read-after-write within the process that
// wrote" and the domain has to tolerate transient absence.
//
// What this adapter does meanwhile, and why:
//
//  1. it confirms the write BY NUMBER (strong, always works) — proving the
//     material was accepted and came back byte for byte;
//  2. it WAITS for the `latest` alias to reach the new version, with a
//     configurable ceiling. Put does not return before that. If it does not
//     converge within the ceiling, it returns KindUnavailable saying exactly
//     that.
//
// Step 2 is expensive and may fail on real GCP. It is deliberate: a slow Put and
// an explicit error are preferable to a silent Get returning "it does not exist"
// for a credential that was just written. On the emulator it finishes on the
// first attempt — and that is why it must not be mistaken for proof that the
// guarantee holds in production.
//
// ─────────────── WHERE THE LOCAL EMULATOR IS MORE PERMISSIVE ───────────────
//
// The local environment uses a COMMUNITY emulator (Google publishes none; see
// P-17 in the ROADMAP). It is looser than GCP in ways that would make the suite
// pass here and BREAK in production. Each divergence below was verified against
// the running emulator, and each has a defence IN THIS FILE — the defence is
// what stops the local environment from hiding the production path:
//
//  1. REPLICATION. The emulator accepts CreateSecret WITHOUT the `replication`
//     field and invents "automatic". Google's REST reference marks the field as
//     "Required" (the .proto, newer, says "Optional" because of regional
//     secrets — the two disagree with each other). Defence: we send
//     Replication_Automatic ALWAYS, explicitly. We never depend on anybody's
//     default, least of all a default the vendor's own documentation is split
//     about.
//
//  2. NAME FORMAT. The emulator accepts ANY secretId — a dot, a space, a
//     slash, an uppercase letter, 300 characters, all answered 200. Real GCP
//     documents "maximum length of 255 characters ... letters, numerals, and
//     the hyphen (-) and underscore (_)". Defence: validateSecretID runs BEFORE
//     every call, and the name we build is born inside the alphabet.
//
//  3. VALUE SIZE. The emulator stored 128 KiB without complaining. Real GCP
//     documents 64 KiB per version. Defence: maxPayloadBytes, checked before
//     anything leaves the machine.
//
//  4. CHECKSUM. The emulator returns dataCrc32c = 0 ALWAYS (it does not compute
//     one) and ignores the checksum sent. Real GCP verifies it on write and
//     ALWAYS returns the value on read — generating one if the client did not
//     send it. Defence: we send the CRC (the real one validates it) and, on
//     read, we only verify when it comes back non-zero. That would leave an
//     asymmetry — integrity checked only in production — were it not for Put's
//     confirmation, which compares the BYTES read against the ones written and
//     holds in both environments.
//
//  5. THE "latest" ALIAS. In the emulator, `latest` FALLS BACK: with versions 3
//     (disabled) and 2 (destroyed), it serves version 1, and the absence
//     message is "No enabled versions found". On real GCP `latest` is "an alias
//     to the most recently CREATED SecretVersion", regardless of state: if it
//     is disabled or destroyed, the access fails. Only a partial defence is
//     possible: by the design above, this adapter's highest-numbered version is
//     ALWAYS enabled (Put destroys the previous ones, Delete takes everything),
//     so the two environments coincide. The divergence appears if somebody
//     disables a version FROM OUTSIDE — the console, Terraform, an incident
//     response. There, and only there, local returns the OLD value and
//     production returns absence. It is the point where the suite passes here
//     and may fail there.
//
//  6. PROPAGATION. The emulator is an in-memory table and announces "0ms". Real
//     GCP is eventually consistent for everything that is not access by number
//     — see the consistency section above. NO local test exercises that delay,
//     and it is where the port's only guarantee that does not hold in
//     production comes from.
//
//  7. QUOTAS. The emulator has none. Real GCP publishes, among others:
//     AddSecretVersion at 2 qps / 120 qpm PER SECRET; Destroy/Disable/Enable at
//     1 qps / 60 qpm PER VERSION; and, per project, 90,000 accesses/min but
//     only 600 reads/min and 600 WRITES/min. One Put of this adapter spends 3
//     to 5 of those operations (create + addVersion + access + list +
//     destroys), which puts the practical ceiling around 150 writes per minute
//     across the whole project. And the contract suite itself writes the SAME
//     reference several times in a row, which on real GCP hits the per-secret
//     limit. Defence: quota errors become KindUnavailable (retryable) — but
//     nothing here simulates the quota, and no local test will ever meet it.
//
//  8. IAM. The emulator has NO access control at all: any caller reads any
//     secret. In production the isolation by name is the FIRST barrier and the
//     service account's IAM policy is the second — and it is the second that no
//     local test exercises. What the suite proves locally about isolation
//     (guarantee 5) is only the half that lives in the name.
//
//  9. NAME REUSE AFTER DELETE. In the emulator, deleting and recreating with
//     the same name works immediately. On real GCP DeleteSecret is irreversible
//     and immediate, but the METADATA is eventually consistent: recreating
//     right after may hit AlreadyExists or create a secret that does not show
//     up yet. The contract suite does exactly that cycle.
//
//  10. DURABILITY. The emulator is pure memory: the /data the image declares
//     stays empty, there is no import/export flag and a restart erases
//     EVERYTHING — verified. Restarting the pod takes every credential of the
//     local environment with it. On real GCP the secret is durable and
//     replicated. No defence is possible here; the consequence is recorded in
//     dop-infra/docs/ambiente-local.md.
package secretstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"
	"strings"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// maxPayloadBytes is real GCP's documented ceiling (64 KiB per version).
// Checked HERE because the emulator accepts more — divergence 3.
const maxPayloadBytes = 64 * 1024

// defaultPropagation is how long Put waits for the `latest` alias to reach the
// just-written version. On the emulator the first attempt is enough. On real GCP
// this is the ceiling of the attempt to honour guarantee 1 — see the consistency
// section.
const defaultPropagation = 30 * time.Second

type GCP struct {
	client      *secretmanager.Client
	parent      string // projects/<id>
	propagation time.Duration
}

type GCPConfig struct {
	// ProjectID is the project that HOSTS the secrets. Mandatory: without it
	// the names would come out as "projects//secrets/..." and the failure would
	// appear far from the cause, on the first credential written.
	ProjectID string
	// Endpoint points at the EMULATOR ("host:port"). Empty = real GCP, with the
	// environment's default credential (ADC). Filled in = no authentication and
	// no TLS, which is what the emulator offers — and that is why filling it in
	// in production would be sending a secret in clear text to an arbitrary
	// address.
	//
	// There is no official emulator variable for Secret Manager (Google has
	// STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST and the like, but none here —
	// the official library reads none). This one is OURS.
	Endpoint string
	// Propagation is the ceiling of the read-after-write wait. Zero = default.
	Propagation time.Duration
}

// NewGCP opens the client. It returns an error because building the client
// resolves the credential (ADC) and may fail — and failing at BOOT is better
// than failing on the first credential written, with the process already calling
// itself healthy.
func NewGCP(ctx context.Context, cfg GCPConfig) (*GCP, error) {
	if strings.TrimSpace(cfg.ProjectID) == "" {
		return nil, errs.Invalid("SECRET_PROJECT is mandatory when SECRET_BACKEND=gcp")
	}
	var opts []option.ClientOption
	if cfg.Endpoint != "" {
		// The three options travel TOGETHER. WithEndpoint alone would make the
		// library keep looking for ADC and demanding TLS, and the error would
		// show up as "transport: authentication handshake failed" — which says
		// nothing about the real cause.
		opts = append(opts,
			option.WithEndpoint(cfg.Endpoint),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)
	}
	c, err := secretmanager.NewClient(ctx, opts...)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to open the Secret Manager client")
	}
	prop := cfg.Propagation
	if prop <= 0 {
		prop = defaultPropagation
	}
	return &GCP{client: c, parent: "projects/" + cfg.ProjectID, propagation: prop}, nil
}

// Close releases the gRPC connection. The port has no Close (neither k8s nor
// the in-memory one needs one); the composition root closes it, along with the
// rest.
func (g *GCP) Close() error { return g.client.Close() }

// ───────────────────────── name and isolation ─────────────────────────

// gcpSecretID is the alphabet real GCP accepts. The emulator accepts anything
// (divergence 2), so this expression is the only thing between an invalid name
// and a failure that would only show up in production.
var gcpSecretID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// compMax limits each readable piece of the name so the total fits in 255.
const compMax = 60

// secretID maps the logical reference to the secret's name. Isolation between
// accounts lives HERE — account A's reference never resolves account B's secret
// (guarantee 5).
//
// Two deliberate differences from the k8s sibling:
//
//   - the separator is "_", which sanitize NEVER produces. In k8s the separator
//     is "-", which sanitize produces all the time, and that is why the names
//     there are AMBIGUOUS: account "a-b" with kind "c" and account "a" with kind
//     "b-c" generate the SAME name. It is a latent cross-account leak, reported
//     separately;
//
//   - the name ends with a fingerprint of the RAW tuple. sanitize is lossy
//     ("a.b" and "a-b" become the same thing), so the readable part alone is not
//     enough. The fingerprint is what makes guarantee 5 a property of the CODE,
//     and not of the shape the identifiers happen to have today.
func (g *GCP) secretID(ref ports.SecretRef) string {
	return fmt.Sprintf("dop_%s_%s_%s_%s",
		clamp(sanitize(ref.AccountID)),
		clamp(sanitize(ref.Kind)),
		clamp(sanitize(ref.OwnerID)),
		fingerprint(ref))
}

func clamp(s string) string {
	if len(s) > compMax {
		return s[:compMax]
	}
	return s
}

// fingerprint tells apart tuples sanitize would confuse. Each field's LENGTH
// goes into the hash: without it, {"ab",""} and {"a","b"} would digest the same.
func fingerprint(r ports.SecretRef) string {
	h := sha256.New()
	for _, s := range []string{r.AccountID, r.Kind, r.OwnerID} {
		fmt.Fprintf(h, "%d:", len(s))
		_, _ = h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// validateSecretID never quotes the secret's value, only the name's shape.
func validateSecretID(id string) error {
	if !gcpSecretID.MatchString(id) {
		return errs.Invalid(
			"secret name outside the format Secret Manager accepts (%d characters)", len(id))
	}
	return nil
}

func (g *GCP) secretName(id string) string { return g.parent + "/secrets/" + id }

// ───────────────────────── the port's operations ─────────────────────────

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func (g *GCP) Put(ctx context.Context, ref ports.SecretRef, v ports.SecretValue) error {
	if len(v) > maxPayloadBytes {
		return errs.Invalid("secret larger than Secret Manager's limit (%d bytes; maximum %d)",
			len(v), maxPayloadBytes)
	}
	id := g.secretID(ref)
	if err := validateSecretID(id); err != nil {
		return err
	}

	// 1. the container. AlreadyExists is the NORMAL path of the second Put — it
	//    is not an error, it is the secret already existing. Replication goes
	//    explicitly (divergence 1).
	_, err := g.client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   g.parent,
		SecretId: id,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
			Labels: map[string]string{"managed-by": "dop-core"},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return wrapGCP(err, "failed to create the secret in Secret Manager")
	}

	// 2. the material. The CRC is checked by real GCP on write; the emulator
	//    ignores it (divergence 4).
	crc := int64(crc32.Checksum(v, crcTable))
	ver, err := g.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  g.secretName(id),
		Payload: &secretmanagerpb.SecretPayload{Data: v, DataCrc32C: &crc},
	})
	if err != nil {
		return wrapGCP(err, "failed to write the secret version")
	}
	n, err := versionNumber(ver.GetName())
	if err != nil {
		return err
	}

	// 3. STRONG confirmation, by number: it is the only access Google promises
	//    to be immediate. It proves the material was accepted and comes back
	//    identical.
	if err := g.confirmByNumber(ctx, ver.GetName(), v); err != nil {
		return err
	}

	// 4. EVENTUAL confirmation, through the path Get uses. See the consistency
	//    section: this is where guarantee 1 is honoured — or fails loudly.
	if err := g.awaitLatest(ctx, id, n); err != nil {
		return err
	}

	// 5. "Put REPLACES" (guarantee 4): the previous value stops being readable.
	return g.destroyOlder(ctx, id, n)
}

// confirmByNumber reads the just-created version by its FULL name and compares
// the bytes. It is the end-to-end integrity that does not depend on the checksum
// — which the emulator does not compute (divergence 4).
func (g *GCP) confirmByNumber(ctx context.Context, name string, v ports.SecretValue) error {
	resp, err := g.client.AccessSecretVersion(ctx,
		&secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return wrapGCP(err, "failed to confirm the just-written secret version")
	}
	if !bytes.Equal(resp.GetPayload().GetData(), v) {
		// Without quoting either value (guarantee 6).
		return errs.Internal("Secret Manager returned content different from what was written")
	}
	return nil
}

// awaitLatest waits for the `latest` alias to reach the written version.
//
// Without this, "write and return" would be read-after-write only on the
// emulator: on real GCP the alias is eventually consistent, and a Get right
// after the Put would return (nil, nil) — which through the port means "it does
// not exist". A just-written credential would show up as absent, in silence.
func (g *GCP) awaitLatest(ctx context.Context, id string, want int64) error {
	deadline := time.Now().Add(g.propagation)
	wait := 25 * time.Millisecond
	for {
		resp, err := g.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
			Name: g.secretName(id) + "/versions/latest",
		})
		switch {
		case err == nil:
			got, verr := versionNumber(resp.GetName())
			if verr != nil {
				return verr
			}
			// >= and not ==: another concurrent Put may already have gone
			// ahead, and in that case propagation has more than caught up.
			if got >= want {
				return nil
			}
		case status.Code(err) == codes.NotFound, status.Code(err) == codes.FailedPrecondition:
			// not propagated yet — exactly the case this wait covers
		default:
			return wrapGCP(err, "failed to confirm the secret's visibility")
		}

		if !time.Now().Before(deadline) {
			return errs.New(errs.KindUnavailable,
				"Secret Manager did not make version %d visible through `latest` within %s: "+
					"the write was accepted, but read-after-write was not confirmed",
				want, g.propagation)
		}
		select {
		case <-ctx.Done():
			return errs.Wrap(errs.KindUnavailable, ctx.Err(),
				"context ended before confirming the secret's visibility")
		case <-time.After(wait):
		}
		if wait < time.Second {
			wait *= 2
		}
	}
}

// destroyOlder erases the previous versions' material. It runs AFTER the
// confirmations: the new value stands first, only then the old one falls.
//
// The error GOES UP instead of being swallowed. If the destruction fails, the
// old value stays readable by version number and guarantee 4 was not delivered
// in full — silencing that would be the platform believing it rotated a
// credential that still works. The message says the new value is already active,
// so the operator knows repeating the Put is safe.
func (g *GCP) destroyOlder(ctx context.Context, id string, keep int64) error {
	it := g.client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{
		Parent: g.secretName(id),
	})
	var stale []string
	for {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return wrapGCP(err, "the secret's new value is already active, but the "+
				"previous versions could not be listed to be destroyed")
		}
		if v.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
			continue
		}
		n, err := versionNumber(v.GetName())
		if err != nil {
			return err
		}
		if n < keep {
			stale = append(stale, v.GetName())
		}
	}
	for _, name := range stale {
		_, err := g.client.DestroySecretVersion(ctx,
			&secretmanagerpb.DestroySecretVersionRequest{Name: name})
		// FailedPrecondition/NotFound = already destroyed, probably through a
		// race with another Put. That is the desired state anyway.
		if err != nil && status.Code(err) != codes.FailedPrecondition &&
			status.Code(err) != codes.NotFound {
			return wrapGCP(err, "the secret's new value is already active, but the "+
				"previous value was NOT destroyed and stays readable")
		}
	}
	return nil
}

func (g *GCP) Get(ctx context.Context, ref ports.SecretRef) (ports.SecretValue, error) {
	id := g.secretID(ref)
	if err := validateSecretID(id); err != nil {
		return nil, err
	}
	resp, err := g.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: g.secretName(id) + "/versions/latest",
	})
	switch {
	case err == nil:
	case status.Code(err) == codes.NotFound:
		// guarantee 2: absent returns nil, not an error. It covers both "the
		// secret does not exist" and "the secret exists and has no usable
		// version".
		return nil, nil
	case status.Code(err) == codes.FailedPrecondition:
		// `latest` exists but is disabled or destroyed (divergence 5). Through
		// the port there is no value, and that is the same as absence:
		// translating it into an error would make the caller treat "credential
		// not configured yet" as a system failure.
		return nil, nil
	default:
		return nil, wrapGCP(err, "failed to read the secret in Secret Manager")
	}

	data := resp.GetPayload().GetData()
	// It only verifies when the server reports the checksum: the emulator always
	// returns zero (divergence 4), and demanding it would break the local
	// environment over a defect that is not the adapter's.
	if c := resp.GetPayload().DataCrc32C; c != nil && *c != 0 {
		if uint32(*c) != crc32.Checksum(data, crcTable) {
			return nil, errs.Internal("secret corrupted in transit: checksum mismatch")
		}
	}
	if data == nil {
		// An empty value is a VALUE, not an absence: we return a non-nil slice
		// so Exists agrees with k8s and with the in-memory adapter.
		return ports.SecretValue{}, nil
	}
	return ports.SecretValue(data), nil
}

func (g *GCP) Delete(ctx context.Context, ref ports.SecretRef) error {
	id := g.secretID(ref)
	if err := validateSecretID(id); err != nil {
		return err
	}
	err := g.client.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: g.secretName(id)})
	if err != nil && status.Code(err) != codes.NotFound { // guarantee 3: idempotent
		return wrapGCP(err, "failed to delete the secret in Secret Manager")
	}
	return nil
}

func (g *GCP) Exists(ctx context.Context, ref ports.SecretRef) (bool, error) {
	v, err := g.Get(ctx, ref)
	return v != nil, err
}

// ───────────────────────── error translation ─────────────────────────

// versionNumber extracts the N from ".../versions/N". The name comes from the
// server; if it does not have that shape, some premise of this adapter stopped
// holding — and it is better to say so than to carry on with an invented
// number.
func versionNumber(name string) (int64, error) {
	i := strings.LastIndex(name, "/versions/")
	if i < 0 {
		return 0, errs.Internal("unexpected version name coming from Secret Manager")
	}
	n, err := strconv.ParseInt(name[i+len("/versions/"):], 10, 64)
	if err != nil {
		return 0, errs.Internal("unexpected version number coming from Secret Manager")
	}
	return n, nil
}

// wrapGCP translates the gRPC code into the domain's Kind.
//
// The server's original message does NOT come along: it carries the secret's
// full name, which reveals the account and the owner. The error goes up to the
// log (guarantee 6).
func wrapGCP(err error, msg string) error {
	// A blown deadline with the connection down does NOT arrive as a server
	// status: the library returns the CONTEXT's error, and status.Code() over it
	// answers Unknown — which would fall into the default and become
	// KindInternal. That is: "Secret Manager is down" would be reported as a
	// defect of ours, sending whoever investigates to look in the wrong place.
	// It is the same mistake the IdentityProvider port forbids in writing in its
	// guarantee 7.
	//
	// Found running the suite with the emulator UNREACHABLE — the path that
	// should SKIP the test, and which is only exercised when somebody exercises
	// it on purpose.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.KindUnavailable, err, "%s", msg)
	}
	var kind errs.Kind
	switch status.Code(err) {
	case codes.NotFound:
		kind = errs.KindNotFound
	case codes.AlreadyExists:
		kind = errs.KindAlreadyExists
	case codes.InvalidArgument:
		kind = errs.KindInvalid
	case codes.PermissionDenied:
		kind = errs.KindPermission
	case codes.Unauthenticated:
		kind = errs.KindUnauthorized
	case codes.FailedPrecondition:
		kind = errs.KindPrecondition
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.ResourceExhausted:
		// ResourceExhausted is QUOTA (divergence 7): temporary and retryable,
		// not the caller's defect.
		kind = errs.KindUnavailable
	default:
		kind = errs.KindInternal
	}
	return errs.New(kind, "%s (code %s)", msg, status.Code(err))
}

var _ ports.SecretStore = (*GCP)(nil)
