package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Docker runs a verification as containers on the host's daemon: one for the
// runner, one per declared dependency, all on a network of the run's own.
//
// The network is what makes the dependencies reachable BY NAME, which is the
// same address the Kubernetes adapter gives them through a shared `localhost`.
// The application's configuration does not learn which executor it is on.
type Docker struct {
	client  *http.Client
	stream  *http.Client
	apiVer  string
	timeout time.Duration
	// CacheRoot is where the accounts' cache volumes live. It is a directory on
	// the host and not a named volume so that the same path is mountable by a
	// runner and inspectable by whoever operates the machine.
	cacheRoot string
}

type DockerConfig struct {
	Socket     string
	APIVersion string
	Timeout    time.Duration
	CacheRoot  string
}

const DefaultDockerSocket = "/var/run/docker.sock"

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
	root := cfg.CacheRoot
	if root == "" {
		root = "/var/lib/dop/cache"
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	return &Docker{
		client:    &http.Client{Transport: &http.Transport{DialContext: dial}, Timeout: to},
		stream:    &http.Client{Transport: &http.Transport{DialContext: dial}},
		apiVer:    ver,
		timeout:   to,
		cacheRoot: root,
	}
}

var _ ports.VerificationRunner = (*Docker)(nil)

// Names carry the RUN's id, not the account's space: several runs of one
// account exist at the same time, and Docker has one flat name space.
func (d *Docker) runnerName(h ports.RunnerHandle) string { return "dop-run-" + h.ID }
func (d *Docker) depName(h ports.RunnerHandle, dep string) string {
	return "dop-run-" + h.ID + "-dep-" + dep
}

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

func (d *Docker) ensureImage(ctx context.Context, image string) error {
	// RAW, not escaped: PathEscape turns the slashes of a registry-qualified
	// name into %2F and Docker stops finding the local image.
	code, _, err := d.do(ctx, http.MethodGet, "/images/"+image+"/json", nil)
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return nil
	}
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
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fail(resp.StatusCode, out, "downloading image "+image)
	}
	return nil
}

// ── Start ────────────────────────────────────────────────────────────────────

func (d *Docker) Start(ctx context.Context, spec ports.RunnerSpec) (*ports.RunnerStatus, error) {
	if err := validate(spec); err != nil {
		return nil, err
	}
	// Idempotent by handle: a network retry must not raise a second run, and
	// the bill for a duplicated environment arrives at the end of the month.
	if st, err := d.Status(ctx, spec.RunnerHandle); err == nil {
		return st, nil
	} else if errs.KindOf(err) != errs.KindNotFound {
		return nil, err
	}

	// The RUNNER comes up first, and the dependencies join its network
	// namespace afterwards — that is what puts them on `localhost`, the same
	// address a pod gives them. The order is not a preference: a container
	// cannot join a namespace that does not exist yet.
	//
	// Nothing races here: the script's first act after cloning is to wait for
	// each declared port, so the runner simply waits while the dependencies come
	// up beside it.
	if err := d.startRunner(ctx, spec); err != nil {
		_ = d.Destroy(ctx, spec.RunnerHandle)
		return nil, err
	}
	for _, dep := range spec.Dependencies {
		if err := d.startDependency(ctx, spec, dep); err != nil {
			// A refusal that leaves half an environment behind is worse than no
			// refusal: the next Start would find debris and call it a run.
			_ = d.Destroy(ctx, spec.RunnerHandle)
			return nil, err
		}
	}
	return &ports.RunnerStatus{Phase: ports.RunnerPending}, nil
}

func (d *Docker) startDependency(ctx context.Context, spec ports.RunnerSpec, dep ports.RunnerDependency) error {
	if err := d.ensureImage(ctx, dep.Image); err != nil {
		return err
	}
	name := d.depName(spec.RunnerHandle, dep.Name)
	cfg := map[string]any{
		"Image":  dep.Image,
		"Env":    envList(dep.Env),
		"Labels": labels(spec.RunnerHandle),
		"HostConfig": map[string]any{
			// The runner's own namespace: the dependency answers at
			// 127.0.0.1:<port>, exactly as it does inside a pod.
			"NetworkMode":   "container:" + d.runnerName(spec.RunnerHandle),
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}
	if err := d.create(ctx, name, cfg, "creating dependency "+dep.Name); err != nil {
		return err
	}
	return d.start(ctx, name, "starting dependency "+dep.Name)
}

func (d *Docker) startRunner(ctx context.Context, spec ports.RunnerSpec) error {
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return err
	}
	env := envList(spec.Env)
	if spec.Repository.Token != "" {
		env = append(env, "DOP_GIT_TOKEN="+spec.Repository.Token)
	}
	binds, err := d.cacheBinds(spec)
	if err != nil {
		return err
	}
	exposed := map[string]any{}
	bindings := map[string]any{}
	if spec.AppPort > 0 {
		key := fmt.Sprintf("%d/tcp", spec.AppPort)
		exposed[key] = map[string]any{}
		// Port 0 asks the daemon for a free one: two runs of the same commit
		// coexist, and a fixed host port would make the second fail to bind.
		bindings[key] = []map[string]string{{"HostPort": "0"}}
	}
	cfg := map[string]any{
		"Image":        spec.Image,
		"Entrypoint":   []string{},
		"Cmd":          []string{"sh", "-c", Script(spec)},
		"Env":          env,
		"Labels":       labels(spec.RunnerHandle),
		"ExposedPorts": exposed,
		"HostConfig": map[string]any{
			"Binds":         binds,
			"PortBindings":  bindings,
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}
	name := d.runnerName(spec.RunnerHandle)
	if err := d.create(ctx, name, cfg, "creating the runner"); err != nil {
		return err
	}
	return d.start(ctx, name, "starting the runner")
}

// cacheBinds mounts the ACCOUNT's cache at each declared path.
//
// Per account and never global: a cache shared between accounts is a side
// channel, and the fact that it is only build artefacts today does not make it
// safe tomorrow.
func (d *Docker) cacheBinds(spec ports.RunnerSpec) ([]string, error) {
	var binds []string
	for i, p := range spec.CachePaths {
		host := fmt.Sprintf("%s/%s/%d", d.cacheRoot, spec.AccountID, i)
		if err := os.MkdirAll(host, 0o777); err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "failed to prepare the account's cache")
		}
		binds = append(binds, host+":"+p)
	}
	return binds, nil
}

func (d *Docker) create(ctx context.Context, name string, cfg map[string]any, what string) error {
	code, body, err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), cfg)
	if err != nil {
		return err
	}
	if code == http.StatusCreated || code == http.StatusConflict {
		return nil
	}
	return fail(code, body, what)
}

func (d *Docker) start(ctx context.Context, name, what string) error {
	code, body, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil)
	if err != nil {
		return err
	}
	if code == http.StatusNoContent || code == http.StatusNotModified {
		return nil
	}
	return fail(code, body, what)
}

// ── Status ───────────────────────────────────────────────────────────────────

func (d *Docker) Status(ctx context.Context, h ports.RunnerHandle) (*ports.RunnerStatus, error) {
	name := d.runnerName(h)
	code, body, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, errs.NotFound("run %s does not exist", h.ID)
	}
	if code != http.StatusOK {
		return nil, fail(code, body, "reading the run")
	}
	var insp struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(body, &insp); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from Docker")
	}

	logs, err := d.readLogs(ctx, name)
	if err != nil {
		return nil, err
	}
	st := ParseLog(h, logs, insp.State.Running)
	if st.Phase == ports.RunnerHolding {
		for _, bindings := range insp.NetworkSettings.Ports {
			for _, b := range bindings {
				var p int32
				_, _ = fmt.Sscanf(b.HostPort, "%d", &p)
				if p > 0 {
					st.Endpoint = &ports.RunnerEndpoint{Port: p, State: "running"}
				}
			}
		}
	}
	return st, nil
}

func (d *Docker) readLogs(ctx context.Context, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/"+d.apiVer+"/containers/"+url.PathEscape(name)+"/logs?stdout=1&stderr=1", nil)
	if err != nil {
		return "", err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return "", errs.Wrap(errs.KindUnavailable, err, "failed to read the run's log")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "truncated log")
	}
	return string(undocker(raw)), nil
}

// undocker strips Docker's 8-byte stream header from each frame. Without it the
// marker would be preceded by control bytes and no line would ever be found.
func undocker(raw []byte) []byte {
	var out []byte
	for len(raw) >= 8 {
		n := int(raw[4])<<24 | int(raw[5])<<16 | int(raw[6])<<8 | int(raw[7])
		if raw[0] > 2 || n < 0 || n > len(raw)-8 {
			// Not a framed stream (a TTY container): take it as it is.
			return raw
		}
		out = append(out, raw[8:8+n]...)
		raw = raw[8+n:]
	}
	return out
}

// ── Logs ─────────────────────────────────────────────────────────────────────

func (d *Docker) Logs(ctx context.Context, h ports.RunnerHandle, q ports.LogQuery, emit func(ports.LogLine) error) error {
	name := d.runnerName(h)
	if _, err := d.Status(ctx, h); err != nil {
		return err
	}
	u := fmt.Sprintf("http://docker/%s/containers/%s/logs?stdout=1&stderr=1&timestamps=1&follow=%d",
		d.apiVer, url.PathEscape(name), boolInt(q.Follow))
	if q.TailLines > 0 {
		u += fmt.Sprintf("&tail=%d", q.TailLines)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errs.Wrap(errs.KindUnavailable, err, "failed to follow the run's log")
	}
	defer resp.Body.Close()

	buf := make([]byte, 32*1024)
	var pending []byte
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, undocker(buf[:n])...)
			for {
				i := strings.IndexByte(string(pending), '\n')
				if i < 0 {
					break
				}
				line := string(pending[:i])
				pending = pending[i+1:]
				ts, text := splitTimestamp(line)
				if err := emit(ports.LogLine{At: ts, Text: text}); err != nil {
					return err
				}
			}
		}
		if rerr != nil {
			if ctx.Err() != nil || rerr == io.EOF {
				return nil
			}
			return errs.Wrap(errs.KindUnavailable, rerr, "the run's log was interrupted")
		}
	}
}

func splitTimestamp(raw string) (time.Time, string) {
	i := strings.IndexByte(raw, ' ')
	if i < 0 {
		return time.Time{}, raw
	}
	ts, err := time.Parse(time.RFC3339Nano, raw[:i])
	if err != nil {
		return time.Time{}, raw
	}
	return ts, raw[i+1:]
}

// ── Destroy ──────────────────────────────────────────────────────────────────

// Destroy takes the runner, the dependencies and the network. It is idempotent:
// destroying what does not exist is success, because absence is the outcome
// asked for.
func (d *Docker) Destroy(ctx context.Context, h ports.RunnerHandle) error {
	code, body, err := d.do(ctx, http.MethodGet,
		"/containers/json?all=1&filters="+url.QueryEscape(`{"label":["dop.run=`+h.ID+`"]}`), nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fail(code, body, "listing the run's containers")
	}
	var list []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return errs.Wrap(errs.KindInternal, err, "unreadable response from Docker")
	}
	for _, c := range list {
		code, body, err := d.do(ctx, http.MethodDelete, "/containers/"+c.ID+"?force=1&v=1", nil)
		if err != nil {
			return err
		}
		if code != http.StatusNoContent && code != http.StatusNotFound {
			return fail(code, body, "removing a container of the run")
		}
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func labels(h ports.RunnerHandle) map[string]string {
	return map[string]string{"dop.run": h.ID, "dop.managed-by": "dop-core"}
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func validate(spec ports.RunnerSpec) error {
	if strings.TrimSpace(spec.ID) == "" || strings.TrimSpace(spec.Namespace) == "" {
		return errs.Invalid("a run with no identity")
	}
	if strings.TrimSpace(spec.Image) == "" {
		return errs.Invalid("a run with no runner image")
	}
	if strings.TrimSpace(spec.Repository.CloneURL) == "" || strings.TrimSpace(spec.Repository.Commit) == "" {
		return errs.Invalid(
			"a run with no repository or no commit: a verification that does not say which code it ran on is not evidence")
	}
	return nil
}
