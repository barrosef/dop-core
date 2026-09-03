// A SandboxLauncher adapter over the host's Docker.
//
// It is what makes developing the platform possible with no cluster (substrate
// spec §2) — and, by ADR-0001, it is the PROOF that the port is right: with a
// single adapter, the port would come out in Kubernetes's shape and nobody would
// notice.
//
// It speaks the Engine API over the unix socket, with net/http — no SDK. It is
// not preciousness: Docker's SDK drags the whole daemon's dependency tree into a
// binary that only needs seven HTTP calls.
package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// DefaultDockerSocket is where the daemon listens on Linux.
const DefaultDockerSocket = "/var/run/docker.sock"

type Docker struct {
	client *http.Client
	// stream has a zero Timeout: a log follow lasts as long as the client does,
	// and a timeout on the HTTP client would kill the tail midway with no useful
	// error at all.
	stream  *http.Client
	apiVer  string
	timeout time.Duration
}

type DockerConfig struct {
	Socket string
	// APIVersion pins the negotiated version. Without it the daemon uses the
	// most recent one it knows — and a host upgrade would change the response's
	// format under us.
	APIVersion string
	Timeout    time.Duration
}

func NewDocker(cfg DockerConfig) *Docker {
	socket := cfg.Socket
	if socket == "" {
		socket = DefaultDockerSocket
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" && strings.HasPrefix(v, "unix://") {
		socket = strings.TrimPrefix(v, "unix://")
	}
	ver := cfg.APIVersion
	if ver == "" {
		ver = "v1.43"
	}
	to := cfg.Timeout
	if to <= 0 {
		to = 30 * time.Second
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	return &Docker{
		client:  &http.Client{Transport: &http.Transport{DialContext: dial}, Timeout: to},
		stream:  &http.Client{Transport: &http.Transport{DialContext: dial}},
		apiVer:  ver,
		timeout: to,
	}
}

var _ ports.SandboxLauncher = (*Docker)(nil)

// ── names ────────────────────────────────────────────────────────────────────
//
// The demand's namespace becomes a name PREFIX in Docker, because Docker has no
// namespaces. It is the spec's same hierarchical identification, expressed in
// what this substrate offers — and that is why the labels travel along: the name
// is for finding, the label is for querying.

func (d *Docker) containerName(h ports.SandboxHandle) string { return h.Namespace + "-sandbox" }
func (d *Docker) volumeName(h ports.SandboxHandle) string    { return h.Namespace + "-workspace" }

// ── HTTP ─────────────────────────────────────────────────────────────────────

func (d *Docker) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errs.Wrap(errs.KindInternal, err, "request unreadable for Docker")
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/"+d.apiVer+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, errs.Wrap(errs.KindUnavailable, err, "failed to talk to Docker")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, errs.Wrap(errs.KindInternal, err, "truncated response from Docker")
	}
	return resp.StatusCode, out, nil
}

// fail turns Docker's error body into a domain error with the message the
// daemon gave. Swallowing that message is what turns "no such image" into
// "internal error" and burns an hour of investigation.
func fail(code int, body []byte, what string) error {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	switch code {
	case http.StatusNotFound:
		return errs.NotFound("%s", what)
	case http.StatusConflict:
		return errs.New(errs.KindAlreadyExists, "%s: %s", what, msg)
	}
	return errs.Internal("Docker refused %s (HTTP %d): %s", what, code, msg)
}

// ── tiers ────────────────────────────────────────────────────────────────────

// SupportedTiers asks the daemon which runtimes it has registered.
//
// It is the same reasoning as the k8s adapter's with RuntimeClass, on the other
// side of the port: the isolation level is a FACT of the host, verifiable, and
// not an assumption of the configuration. A host with no runsc and no kata
// offers exactly `namespace` — and saying so out loud is what lets the domain
// refuse instead of degrading.
func (d *Docker) SupportedTiers(ctx context.Context) ([]ports.IsolationTier, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/info", nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fail(code, body, "querying the daemon")
	}
	var info struct {
		Runtimes map[string]any `json:"Runtimes"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from Docker")
	}
	tiers := map[ports.IsolationTier]bool{ports.TierNamespace: true} // runc is always there
	for name := range info.Runtimes {
		switch {
		case strings.Contains(name, "kata"), strings.Contains(name, "firecracker"):
			tiers[ports.TierHardware] = true
		case strings.Contains(name, "runsc"), strings.Contains(name, "gvisor"),
			strings.Contains(name, "edera"):
			tiers[ports.TierKernelEmulated] = true
		}
	}
	out := make([]ports.IsolationTier, 0, len(tiers))
	for t := range tiers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// runtimeFor returns the Docker runtime that delivers the requested tier.
func (d *Docker) runtimeFor(ctx context.Context, tier ports.IsolationTier) (string, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/info", nil)
	if err != nil {
		return "", err
	}
	if code >= 300 {
		return "", fail(code, body, "querying the daemon")
	}
	var info struct {
		Runtimes       map[string]any `json:"Runtimes"`
		DefaultRuntime string         `json:"DefaultRuntime"`
	}
	_ = json.Unmarshal(body, &info)

	want := func(match ...string) string {
		names := make([]string, 0, len(info.Runtimes))
		for n := range info.Runtimes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, m := range match {
				if strings.Contains(n, m) {
					return n
				}
			}
		}
		return ""
	}

	switch tier {
	case ports.TierNamespace:
		if info.DefaultRuntime != "" {
			return info.DefaultRuntime, nil
		}
		return "runc", nil
	case ports.TierKernelEmulated:
		if r := want("runsc", "gvisor", "edera"); r != "" {
			return r, nil
		}
	case ports.TierHardware:
		if r := want("kata", "firecracker"); r != "" {
			return r, nil
		}
	}
	// The port's guarantee 1: a refusal with a message, never one level less.
	return "", errs.Precondition(
		"this host's Docker has no runtime for %q isolation — install and register "+
			"the corresponding runtime or ask for another level", tier)
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func (d *Docker) Launch(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	// The runtime is resolved BEFORE creating a volume or a container: a refused
	// tier must leave no trace (guarantee 2).
	runtime, err := d.runtimeFor(ctx, spec.Tier)
	if err != nil {
		return nil, err
	}

	// Relaunching the same spec returns what already exists (guarantee 4).
	if st, err := d.Describe(ctx, spec.SandboxHandle); err == nil {
		return st, nil
	} else if errs.KindOf(err) != errs.KindNotFound {
		return nil, err
	}

	if err := d.ensureVolume(ctx, spec); err != nil {
		return nil, err
	}
	if err := d.ensureSessionsVolume(ctx, spec); err != nil {
		return nil, err
	}
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return nil, err
	}
	if err := d.createContainer(ctx, spec, runtime); err != nil {
		return nil, err
	}
	// The token goes in with the container CREATED and not yet started: the
	// archive endpoint writes into a stopped container, and the entrypoint
	// reads the file before anything else runs.
	if err := d.putToken(ctx, spec); err != nil {
		return nil, err
	}
	if err := d.startContainer(ctx, spec.SandboxHandle); err != nil {
		return nil, err
	}
	// The collector comes up AFTER the agent's container: it follows a file that
	// does not exist until the tool starts writing it.
	if err := d.ensureCollector(ctx, spec); err != nil {
		return nil, err
	}
	return d.Describe(ctx, spec.SandboxHandle)
}

// putToken delivers the sandbox's one credential as a FILE (guarantee 22).
//
// Docker has no projected volumes; the archive endpoint is how the daemon
// writes a file into a container that has not started. The file belongs to
// the sandbox's user and is readable by nobody else — it is the agent's own
// key, and only the agent's.
// sessionsVolume is shared between the agent's container and the collector's:
// the tool writes there and the collector reads. On Docker there are no pods, so
// "beside" means a second container on the same named volume.
func (d *Docker) sessionsVolume(h ports.SandboxHandle) string { return h.Namespace + "-sessions" }

func (d *Docker) collectorName(h ports.SandboxHandle) string { return h.Namespace + "-collector" }

// ensureCollector raises the sidecar, or removes it when there is none.
//
// The key travels in the collector's OWN environment, and the agent's container
// has an environment of its own — a container does not read another's. It is a
// weaker separation than Kubernetes's projected Secret, and it is the strongest
// this substrate offers; the port promises the outcome, each adapter reaches it
// its own way.
func (d *Docker) ensureCollector(ctx context.Context, spec ports.SandboxSpec) error {
	name := d.collectorName(spec.SandboxHandle)
	if spec.Collector.Image == "" {
		code, body, err := d.do(ctx, http.MethodDelete, "/containers/"+name+"?force=true", nil)
		if err != nil {
			return err
		}
		if code >= 300 && code != http.StatusNotFound {
			return fail(code, body, "removing the collector")
		}
		return nil
	}
	if err := d.ensureImage(ctx, spec.Collector.Image); err != nil {
		return err
	}
	// Recreated on every launch: it is stateless, and its configuration —
	// the demand, the key — changes with the sandbox.
	if code, body, err := d.do(ctx, http.MethodDelete, "/containers/"+name+"?force=true", nil); err != nil {
		return err
	} else if code >= 300 && code != http.StatusNotFound {
		return fail(code, body, "recycling the collector")
	}

	env := []string{
		"COLLECTOR_SESSION_DIR=" + ports.SandboxSessionsPath,
		"COLLECTOR_ACCOUNT_ID=" + spec.Collector.AccountID,
		"COLLECTOR_DEMAND_ID=" + spec.Collector.DemandID,
		"COLLECTOR_PROJECT_ID=" + spec.Collector.ProjectID,
		"CORE_TARGET=" + spec.Collector.CoreTarget,
		"CALL_AUTH_KEY_COLLECTOR=" + spec.Collector.Key,
	}
	sort.Strings(env)
	body := map[string]any{
		"Image":  spec.Collector.Image,
		"Cmd":    []string{"collector"},
		"Env":    env,
		"Labels": labelsFor(spec),
		"HostConfig": map[string]any{
			// Read-only on the sessions: the collector follows, it does not write.
			"Binds":         []string{d.sessionsVolume(spec.SandboxHandle) + ":" + ports.SandboxSessionsPath + ":ro"},
			"CapDrop":       []string{"ALL"},
			"SecurityOpt":   []string{"no-new-privileges:true"},
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		},
	}
	code, resp, err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return fail(code, resp, "creating the collector")
	}
	if code, resp, err := d.do(ctx, http.MethodPost, "/containers/"+name+"/start", nil); err != nil {
		return err
	} else if code >= 300 && code != http.StatusNotModified {
		return fail(code, resp, "starting the collector")
	}
	return nil
}

func (d *Docker) putToken(ctx context.Context, spec ports.SandboxSpec) error {
	if spec.Repository.CloneURL == "" {
		return nil
	}
	dir := strings.TrimPrefix(path.Dir(ports.SandboxTokenPath), "/")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: sandboxUID, Gid: 0,
	}); err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to assemble the token")
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: strings.TrimPrefix(ports.SandboxTokenPath, "/"), Mode: 0o400,
		Uid: sandboxUID, Gid: 0, Size: int64(len(spec.Repository.Token)),
	}); err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to assemble the token")
	}
	if _, err := tw.Write([]byte(spec.Repository.Token)); err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to assemble the token")
	}
	if err := tw.Close(); err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to close the token")
	}
	target := "http://docker/" + d.apiVer + "/containers/" +
		d.containerName(spec.SandboxHandle) + "/archive?path=/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.client.Do(req)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to talk to Docker")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fail(resp.StatusCode, body, "writing the sandbox's token")
	}
	return nil
}

// sandboxUID is the devbox image's user. It is repeated here because the tar
// carries ownership, and a file the agent cannot read is a key nobody holds.
const sandboxUID = 1001

func (d *Docker) ensureSessionsVolume(ctx context.Context, spec ports.SandboxSpec) error {
	if spec.Collector.Image == "" {
		return nil
	}
	code, body, err := d.do(ctx, http.MethodPost, "/volumes/create", map[string]any{
		"Name":   d.sessionsVolume(spec.SandboxHandle),
		"Labels": labelsFor(spec),
	})
	if err != nil {
		return err
	}
	if code >= 300 {
		return fail(code, body, "creating the sessions volume")
	}
	return nil
}

func (d *Docker) ensureVolume(ctx context.Context, spec ports.SandboxSpec) error {
	code, body, err := d.do(ctx, http.MethodPost, "/volumes/create", map[string]any{
		"Name":   d.volumeName(spec.SandboxHandle),
		"Labels": labelsFor(spec),
	})
	if err != nil {
		return err
	}
	// Docker returns 201 both on creation and when the volume already exists.
	if code >= 300 {
		return fail(code, body, "creating the workspace")
	}
	return nil
}

// ensureImage pulls the image if it is not on the host.
//
// Without this, the first Launch on a clean machine fails with "no such image" —
// and the contract suite would depend on somebody having run docker pull first,
// which is the definition of a test that passes by accident.
func (d *Docker) ensureImage(ctx context.Context, image string) error {
	// The name goes in RAW: PathEscape would turn the slashes of a
	// registry-qualified name (`dop-registry:5000/dop/devbox:0.1.1`) into
	// `%2F`, Docker would not find the local image, and the adapter would try
	// to pull from a registry the host cannot resolve. Docker's own client
	// sends the name as is.
	code, _, err := d.do(ctx, http.MethodGet, "/images/"+image+"/json", nil)
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return nil
	}
	// The pull may take far longer than a normal call.
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(pullCtx, http.MethodPost,
		"http://docker/"+d.apiVer+"/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to pull image %s", image)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body) // the body is the progress; it has to be drained
	if resp.StatusCode >= 300 {
		return fail(resp.StatusCode, out, "downloading image "+image)
	}
	return nil
}

func (d *Docker) createContainer(ctx context.Context, spec ports.SandboxSpec, runtime string) error {
	env := make([]string, 0, len(spec.Env)+1)
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	if spec.Repository.CloneURL != "" {
		// Not secret: what is secret is the token, and that is a file.
		env = append(env, envProjectRepo+"="+spec.Repository.CloneURL)
	}
	if spec.Collector.Image != "" {
		env = append(env, envSessionDir+"="+ports.SandboxSessionsPath)
	}
	sort.Strings(env) // reproducible creation

	binds := []string{d.volumeName(spec.SandboxHandle) + ":" + ports.SandboxWorkspacePath}
	if spec.Collector.Image != "" {
		// The two containers share this one: the tool writes, the collector
		// reads.
		binds = append(binds, d.sessionsVolume(spec.SandboxHandle)+":"+ports.SandboxSessionsPath)
	}

	body := map[string]any{
		"Image":  spec.Image,
		"Env":    env,
		"Labels": labelsFor(spec),
		// The CONTAINER's working directory is the workspace, and this is where
		// the port's guarantee 14 comes from: k8s's `pods/exec` does not accept
		// a working directory, so instead of emulating it in the adapter both
		// pin the container's and let the exec inherit it. A tool's command
		// starts in the same place on both substrates.
		"WorkingDir": ports.SandboxWorkspacePath,
		// A false Tty keeps stdout and stderr SEPARATE in the log stream. With a
		// tty the two merge and LogLine.Stream would start lying.
		"Tty": false,
		"HostConfig": map[string]any{
			"Runtime": runtime,
			"Binds":   binds,
			// Defence in layers (spec §6). The agent reads untrusted content and
			// carries a credential: no capabilities at all, and no
			// re-escalation.
			"CapDrop":     []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges:true"},
			// An automatic restart would hide a sandbox dying in a loop: the
			// domain needs to SEE the state, not a container resurrecting.
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}
	if len(spec.Command) > 0 {
		body["Cmd"] = spec.Command
	}

	code, resp, err := d.do(ctx, http.MethodPost,
		"/containers/create?name="+url.QueryEscape(d.containerName(spec.SandboxHandle)), body)
	if err != nil {
		return err
	}
	if code == http.StatusConflict {
		return nil // it already exists: Launch is idempotent
	}
	if code >= 300 {
		return fail(code, resp, "creating the sandbox")
	}
	return nil
}

func (d *Docker) startContainer(ctx context.Context, h ports.SandboxHandle) error {
	code, body, err := d.do(ctx, http.MethodPost,
		"/containers/"+d.containerName(h)+"/start", nil)
	if err != nil {
		return err
	}
	// 304 = it was already running. That is a success, not an error.
	if code >= 300 && code != http.StatusNotModified {
		return fail(code, body, "starting the sandbox")
	}
	return nil
}

// Suspend stops execution and PRESERVES the workspace volume.
//
// Here lives the most instructive divergence between the two adapters: k8s
// deletes the pod and only the PVC survives; Docker stops the container and its
// writable layer stays standing. Both deliver guarantee 5 — what is under
// /workspace survives — and only that. If the port promised "the whole sandbox
// survives", this adapter would deliver and the other would not, and the
// contract suite would exist only to rubber-stamp a lie.
func (d *Docker) Suspend(ctx context.Context, h ports.SandboxHandle) error {
	if _, err := d.Describe(ctx, h); err != nil {
		return err // a nonexistent one returns NotFound (guarantee 9)
	}
	code, body, err := d.do(ctx, http.MethodPost, "/containers/"+d.containerName(h)+"/stop?t=10", nil)
	if err != nil {
		return err
	}
	// 304 = already stopped; 404 = the container is gone, but the workspace is there.
	if code >= 300 && code != http.StatusNotModified && code != http.StatusNotFound {
		return fail(code, body, "suspending the sandbox")
	}
	return nil
}

func (d *Docker) Resume(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	st, err := d.Describe(ctx, spec.SandboxHandle)
	if err != nil {
		return nil, err
	}
	if st.Phase == ports.PhaseActive {
		return st, nil // idempotent (guarantee 7)
	}
	// The container may have been removed with the volume intact; in that case
	// recreating is the way back to the existing workspace. The shelf is not
	// touched here: it is a clone in the container, and the entrypoint pulls
	// on every start — what the project learned while the sandbox slept is
	// there when it wakes (guarantee 20).
	if code, _, err := d.do(ctx, http.MethodGet, "/containers/"+d.containerName(spec.SandboxHandle)+"/json", nil); err != nil {
		return nil, err
	} else if code == http.StatusNotFound {
		runtime, err := d.runtimeFor(ctx, spec.Tier)
		if err != nil {
			return nil, err
		}
		if err := d.createContainer(ctx, spec, runtime); err != nil {
			return nil, err
		}
		if err := d.putToken(ctx, spec); err != nil {
			return nil, err
		}
	}
	if err := d.startContainer(ctx, spec.SandboxHandle); err != nil {
		return nil, err
	}
	return d.Describe(ctx, spec.SandboxHandle)
}

// Destroy takes execution AND workspace. It is irreversible by construction:
// once the volume is removed there is nothing to resume.
func (d *Docker) Destroy(ctx context.Context, h ports.SandboxHandle) error {
	// Everything this sandbox owns, in order, and absence is success everywhere
	// — destroying what is not there is the desired result (guarantee 8).
	for _, target := range []struct{ path, what string }{
		{"/containers/" + d.containerName(h) + "?force=1&v=0", "removing the sandbox"},
		// The collector goes with it: it exists only to follow this sandbox, and
		// one left running would keep tailing a file nobody writes any more.
		{"/containers/" + d.collectorName(h) + "?force=1&v=0", "removing the collector"},
		{"/volumes/" + d.sessionsVolume(h) + "?force=1", "removing the sessions"},
		{"/volumes/" + d.volumeName(h) + "?force=1", "removing the workspace"},
	} {
		code, body, err := d.do(ctx, http.MethodDelete, target.path, nil)
		if err != nil {
			return err
		}
		if code >= 300 && code != http.StatusNotFound {
			return fail(code, body, target.what)
		}
	}
	return nil
}

func (d *Docker) Describe(ctx context.Context, h ports.SandboxHandle) (*ports.SandboxStatus, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/containers/"+d.containerName(h)+"/json", nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		// No container but a workspace: suspended. Neither: it does not exist.
		// The tier comes from the VOLUME's label, which survives the suspension
		// — the same way the k8s adapter reads it from the namespace's label.
		// Without that, resuming a suspended sandbox would have no way to
		// re-check the declared isolation, and silent degradation would come in
		// through the back door.
		tier, ok, err := d.volumeTier(ctx, h)
		if err != nil {
			return nil, err
		}
		if ok {
			return &ports.SandboxStatus{Phase: ports.PhaseSuspended, Tier: tier}, nil
		}
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	if code >= 300 {
		return nil, fail(code, body, "reading the sandbox")
	}

	var insp struct {
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
		Config struct {
			Labels       map[string]string `json:"Labels"`
			ExposedPorts map[string]any    `json:"ExposedPorts"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &insp); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from Docker")
	}

	st := &ports.SandboxStatus{
		Tier:      ports.IsolationTier(insp.Config.Labels[labelTier]),
		Endpoints: endpointsFromPorts(insp.Config.ExposedPorts, insp.State.Running),
	}
	switch {
	case insp.State.Running:
		st.Phase = ports.PhaseActive
	case insp.State.Status == "created":
		st.Phase = ports.PhaseProvisioning
	default:
		st.Phase = ports.PhaseSuspended
	}
	return st, nil
}

func (d *Docker) volumeTier(ctx context.Context, h ports.SandboxHandle) (ports.IsolationTier, bool, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/volumes/"+d.volumeName(h), nil)
	if err != nil {
		return "", false, err
	}
	if code == http.StatusNotFound {
		return "", false, nil
	}
	if code >= 300 {
		return "", false, fail(code, body, "reading the workspace")
	}
	var vol struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &vol); err != nil {
		return "", false, errs.Wrap(errs.KindInternal, err, "unreadable response from Docker")
	}
	return ports.IsolationTier(vol.Labels[labelTier]), true, nil
}

// ── exec ─────────────────────────────────────────────────────────────────────

// Exec runs a command inside the sandbox's container (guarantees 13 to 18).
//
// There are THREE calls, and the third is the one many implementations forget:
// `/exec/create` builds the process, `/exec/start` returns the stream with the
// output, and `/exec/{id}/json` is the ONLY place the exit code appears. Reading
// only the stream would deliver the output of a failed command with an invented
// zero exit code — which is exactly the confusion guarantee 15 exists to
// prevent.
func (d *Docker) Exec(ctx context.Context, h ports.SandboxHandle, req ports.ExecRequest) (*ports.ExecResult, error) {
	if len(req.Command) == 0 {
		return nil, errs.Invalid("exec with no command")
	}
	// The phase BEFORE trying: Docker answers 409 for a stopped container, and
	// 409 is "already exists" in this adapter's error translator. Asking first
	// gives k8s's same answer — NotFound for a nonexistent one, Precondition for
	// a suspended one (guarantee 18) — instead of letting each substrate pick
	// its own.
	st, err := d.Describe(ctx, h)
	if err != nil {
		return nil, err
	}
	if st.Phase != ports.PhaseActive {
		return nil, errs.Precondition(
			"sandbox %s is in %q and does not run commands; resume it first", h.ID, st.Phase)
	}

	deadline, limit := execLimits(req)
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	code, body, err := d.do(runCtx, http.MethodPost, "/containers/"+d.containerName(h)+"/exec",
		map[string]any{
			"AttachStdout": true,
			"AttachStderr": true,
			// Stdin closed: this port's exec is a command, not a session. And a
			// false Tty is what KEEPS stdout and stderr separate in the stream
			// (guarantee 16) — with a tty the two merge.
			"AttachStdin": false,
			"Tty":         false,
			"Cmd":         req.Command,
		})
	if err != nil {
		return nil, err
	}
	if code == http.StatusConflict {
		return nil, errs.Precondition(
			"sandbox %s is not running; resume it before running commands", h.ID)
	}
	if code >= 300 {
		return nil, fail(code, body, "preparing the command in the sandbox")
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from Docker when creating the exec")
	}

	start, err := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "request unreadable for Docker")
	}
	httpReq, err := http.NewRequestWithContext(runCtx, http.MethodPost,
		"http://docker/"+d.apiVer+"/exec/"+created.ID+"/start", strings.NewReader(string(start)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// `stream` and not `client`: this call's deadline is the command's, and the
	// ordinary client's Timeout (30s) would cut every command longer than that
	// with nothing to explain it.
	resp, err := d.stream.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execution interrupted by the caller")
		}
		if runCtx.Err() != nil {
			// The deadline blew before any byte: it is still a RESULT.
			return d.execOutcome(ctx, created.ID, "", "", false, true)
		}
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to start the command in the sandbox")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusConflict {
			return nil, errs.Precondition(
				"sandbox %s is not running; resume it before running commands", h.ID)
		}
		return nil, fail(resp.StatusCode, out, "running the command in the sandbox")
	}

	stdout, stderr := &cappedBuffer{max: limit}, &cappedBuffer{max: limit}
	readErr := demuxRaw(resp.Body, stdout, stderr)

	if ctx.Err() != nil {
		return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execution interrupted by the caller")
	}
	timedOut := runCtx.Err() != nil
	if readErr != nil && !timedOut {
		return nil, errs.Wrap(errs.KindUnavailable, readErr, "the command's stream was interrupted")
	}
	return d.execOutcome(ctx, created.ID,
		stdout.String(), stderr.String(), stdout.truncated || stderr.truncated, timedOut)
}

// execOutcome queries the exit code and assembles the result.
//
// The context comes WITHOUT the command's deadline on purpose: when the command
// blew its deadline, its context is already dead, and using it here would lose
// exactly the information that the process is still running in there.
func (d *Docker) execOutcome(ctx context.Context, execID, stdout, stderr string,
	truncated, timedOut bool) (*ports.ExecResult, error) {

	res := &ports.ExecResult{
		ExitCode: -1, Stdout: stdout, Stderr: stderr, Truncated: truncated, TimedOut: timedOut,
	}
	insp, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	code, body, err := d.do(insp, http.MethodGet, "/exec/"+execID+"/json", nil)
	if err != nil || code >= 300 {
		// Without the exit code, -1 is the honest answer: zero would assert
		// success, and asserting success without knowing is the worst of the
		// three outcomes.
		return res, nil
	}
	var out struct {
		Running  bool `json:"Running"`
		ExitCode *int `json:"ExitCode"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return res, nil
	}
	if !out.Running && out.ExitCode != nil {
		res.ExitCode = *out.ExitCode
	}
	if out.Running {
		// The process stayed up: it is a blown deadline, even if the stream
		// ended earlier. Saying otherwise would give a hung command the face of
		// a command that finished with no output.
		res.TimedOut = true
	}
	return res, nil
}

// demuxRaw unpacks Docker's frames straight into two buffers.
//
// Separate from `demux` because the two reads want different things: Tail wants
// timestamped LINES, exec wants each stream's exact BYTES. Reusing the line one
// here would rebuild the output with breaks the command did not emit.
func demuxRaw(r io.Reader, stdout, stderr *cappedBuffer) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		target := stdout
		if header[0] == 2 {
			target = stderr
		}
		// It ALWAYS reads the whole frame, even after hitting the cap: stopping
		// the read would leave the daemon writing into a full pipe and the
		// process in there stuck. The cap cuts what is KEPT, not what is read.
		if _, err := io.CopyN(target, r, int64(size)); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
	}
}

// ── logs ─────────────────────────────────────────────────────────────────────

// Tail follows the sandbox's log and dies along with the caller.
//
// Docker's stream is MULTIPLEXED when there is no tty: each frame carries an
// 8-byte header with the stream and the size. Reading that as plain text would
// deliver the binary header glued to each frame's first line — a bug that goes
// unnoticed until somebody searches for an exact prefix in the log.
func (d *Docker) Tail(ctx context.Context, h ports.SandboxHandle, q ports.LogQuery, emit func(ports.LogLine) error) error {
	if q.Service != "" && q.Service != "sandbox" {
		// A Docker sandbox is ONE process; the demand's internal compose runs
		// inside it and is not visible from here. An unknown name is NotFound,
		// the same answer k8s gives for a container that does not exist in the
		// pod.
		return errs.NotFound("process %q in sandbox %s", q.Service, h.ID)
	}
	if _, err := d.Describe(ctx, h); err != nil {
		return err
	}

	path := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&timestamps=1&follow=%d",
		d.containerName(h), boolToInt(q.Follow))
	if q.TailLines > 0 {
		path += fmt.Sprintf("&tail=%d", q.TailLines)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/"+d.apiVer+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		if ctxEnded(ctx) {
			return nil
		}
		return errs.Wrap(errs.KindUnavailable, err, "failed to follow the logs")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		return fail(resp.StatusCode, out, "reading the logs")
	}

	return demux(ctx, resp.Body, "sandbox", emit)
}

// demux unpacks Docker's frames and delivers them line by line.
func demux(ctx context.Context, r io.Reader, service string, emit func(ports.LogLine) error) error {
	header := make([]byte, 8)
	for {
		if ctxEnded(ctx) {
			return nil
		}
		if _, err := io.ReadFull(r, header); err != nil {
			return streamEnd(ctx, err)
		}
		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return streamEnd(ctx, err)
		}
		for _, raw := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
			at, text := splitTimestamp(raw)
			if err := emit(ports.LogLine{Service: service, Stream: stream, Text: text, At: at}); err != nil {
				return err // an emit error goes up: it is how we learn the client is gone
			}
		}
	}
}

// streamEnd tells "it ended" from "it broke". End of stream and the client's
// cancellation are a normal close; the rest is a real failure.
func streamEnd(ctx context.Context, err error) error {
	if err == nil || ctxEnded(ctx) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return errs.Wrap(errs.KindUnavailable, err, "log stream interrupted")
}

func ctxEnded(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ═════════════════════════════════════════════════════════════════════════════
// Shared by BOTH adapters in this package.
//
// It lives here, and not in a third file, because there is little of it and
// because duplicating it between k8s.go and docker.go would open the door to the
// two diverging on exactly the conventions that have to be identical: the labels
// that identify whose sandbox it is, and the validation of what the port
// requires.
// ═════════════════════════════════════════════════════════════════════════════

// Hierarchical identification is a LABEL, not a name (substrate spec §1): the
// name carries only what has to be short and unique; the account, the demand and
// the tier stay queryable without parsing a string.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelAccount   = "dop.dev/account"
	labelDemand    = "dop.dev/demand"
	labelSandbox   = "dop.dev/sandbox"
	labelTier      = "dop.dev/tier"
)

func labelsFor(spec ports.SandboxSpec) map[string]string {
	return map[string]string{
		labelManagedBy: "dop-core",
		labelAccount:   labelValue(spec.AccountID),
		labelDemand:    labelValue(spec.DemandID),
		labelSandbox:   labelValue(spec.ID),
		labelTier:      string(spec.Tier),
	}
}

// labelValue reduces an id to the alphabet k8s accepts in a label (63
// characters, alphanumeric with - _ . in the middle). Docker would accept
// anything; using the stricter rule on both is what keeps the same query working
// on either side.
func labelValue(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-.")
	}
	return out
}

// validateSpec refuses what the port does not admit. The launcher NEVER invents
// an identity or an isolation level: the id, the namespace, the image and the
// tier come from the domain or the call is invalid.
func validateSpec(spec ports.SandboxSpec) error {
	if strings.TrimSpace(spec.ID) == "" || strings.TrimSpace(spec.Namespace) == "" {
		return errs.Invalid("sandbox with no identification: id and namespace are mandatory")
	}
	if strings.TrimSpace(spec.Image) == "" {
		return errs.Invalid("sandbox with no image")
	}
	if !ports.ValidIsolationTier(spec.Tier) {
		return errs.Invalid(
			"isolation level not declared — the substrate does not choose for you")
	}
	if spec.Repository.CloneURL != "" && strings.TrimSpace(spec.Repository.Token) == "" {
		return errs.Invalid("a repository with no token: the sandbox could not clone it")
	}
	return nil
}

// envProjectRepo is the variable the entrypoint reads to know what to clone.
// The token is NOT in the environment — see ports.SandboxTokenPath.
const envProjectRepo = "DOP_PROJECT_REPO"

// envSessionDir tells the agent's container where the collector is watching, so
// its entrypoint can leave the tool's own auth answer there.
const envSessionDir = "DOP_SESSION_DIR"

// endpointsFromPorts converts published ports into endpoints.
//
// With no published port, an EMPTY list — never an invented endpoint. It is the
// port's guarantee: an adapter that fabricates an endpoint makes the cockpit
// offer a link that does not open.
func endpointsFromPorts(exposed map[string]any, running bool) []ports.SandboxEndpoint {
	if len(exposed) == 0 {
		return nil
	}
	state := "stopped"
	if running {
		state = "running"
	}
	keys := make([]string, 0, len(exposed))
	for k := range exposed {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]ports.SandboxEndpoint, 0, len(keys))
	for _, k := range keys {
		num := k
		if i := strings.IndexByte(k, '/'); i > 0 {
			num = k[:i]
		}
		var p int32
		if _, err := fmt.Sscanf(num, "%d", &p); err != nil || p <= 0 {
			continue
		}
		// Docker does not name ports. The name comes from the number — and that
		// is why the contract suite does NOT promise an endpoint name: in k8s it
		// comes from the container port's `name`, here there is nowhere to take
		// it from.
		out = append(out, ports.SandboxEndpoint{Name: fmt.Sprintf("port-%d", p), Port: p, State: state})
	}
	return out
}

// execLimits resolves the deadline and the output cap from the request.
//
// The defaults are the PORT's and not each adapter's: a default per adapter
// would make the same command have different deadlines depending on where the
// sandbox came up, and the contract suite — which measures both with the same
// ruler — would have no way to assert anything about either.
func execLimits(req ports.ExecRequest) (time.Duration, int) {
	deadline := time.Duration(req.TimeoutSeconds) * time.Second
	if req.TimeoutSeconds <= 0 {
		deadline = ports.DefaultExecTimeout
	}
	limit := req.MaxOutputBytes
	if limit <= 0 {
		limit = ports.DefaultExecMaxOutputBytes
	}
	return deadline, limit
}

// cappedBuffer accumulates up to `max` bytes and RECORDS that it cut.
//
// It never returns an error from Write: what writes into it is a stream-reading
// loop, and interrupting the read because of the cap would leave the process on
// the other side stuck in a full pipe. The cap limits what is KEPT — the read
// goes on to the end, and that is what allows collecting the exit code
// afterwards.
type cappedBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if space := b.max - len(b.buf); space > 0 {
		if len(p) <= space {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:space]...)
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return string(b.buf) }

// splitTimestamp splits off the RFC3339 stamp both substrates prefix when
// timestamps are requested. A line with no stamp returns the zero instant — and
// it is the DOMAIN that decides what to do with that, not the adapter guessing
// time.Now().
func splitTimestamp(raw string) (time.Time, string) {
	sp := strings.IndexByte(raw, ' ')
	if sp <= 0 {
		return time.Time{}, raw
	}
	at, err := time.Parse(time.RFC3339Nano, raw[:sp])
	if err != nil {
		return time.Time{}, raw
	}
	return at.UTC(), raw[sp+1:]
}
