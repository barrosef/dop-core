package verification

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

const k8sServiceAccountCA = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

// Names inside the ACCOUNT's namespace. The cache is fixed — it is the account's
// and it is shared — while everything belonging to one run carries the run's id,
// because several runs of one account live here at the same time.
const (
	runContainer  = "runner"
	runCachePVC   = "cache"
	runTokenKey   = "token"
	runTokenMount = "/etc/dop"
)

// podName and tokenName are per RUN. k8s object names are DNS labels, and a run
// id is hex, so the only thing needed is a prefix and a ceiling.
func podName(h ports.RunnerHandle) string   { return "runner-" + labelValue(h.ID) }
func tokenName(h ports.RunnerHandle) string { return "git-token-" + labelValue(h.ID) }

// K8s runs a verification as ONE pod: the runner container plus one container
// per declared dependency.
//
// One pod and not several is the decision that makes the dependencies free: the
// containers share a network namespace, so the application reaches its database
// at `localhost:5432` with nothing to resolve. A compose file's networking
// exists to make `backend` resolve; a pod removes the question instead of
// answering it.
type K8s struct {
	client            *http.Client
	stream            *http.Client
	apiServer         string
	token             string
	cacheSize         string
	storageClass      string
	platformNamespace string
}

type K8sConfig struct {
	APIServer    string
	Token        string
	CacheSize    string
	StorageClass string
	Client       *http.Client
	Timeout      time.Duration
	// PlatformNamespace is where the core and the projects' git server run. It
	// is here for the same reason as in the sandbox adapter: empty disables the
	// NetworkPolicy, which is what the contract suite wants on a cluster it does
	// not own.
	PlatformNamespace string
}

func NewK8s(cfg K8sConfig) *K8s {
	c := cfg.Client
	if c == nil {
		to := cfg.Timeout
		if to <= 0 {
			to = 30 * time.Second
		}
		c = &http.Client{Timeout: to, Transport: &http.Transport{TLSClientConfig: k8sTLS()}}
	}
	size := cfg.CacheSize
	if size == "" {
		size = "10Gi"
	}
	return &K8s{
		client:            c,
		stream:            &http.Client{Transport: &http.Transport{TLSClientConfig: k8sTLS()}},
		apiServer:         strings.TrimRight(cfg.APIServer, "/"),
		token:             cfg.Token,
		cacheSize:         size,
		storageClass:      cfg.StorageClass,
		platformNamespace: cfg.PlatformNamespace,
	}
}

var _ ports.VerificationRunner = (*K8s)(nil)

func k8sTLS() *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if pem, err := os.ReadFile(k8sServiceAccountCA); err == nil {
		pool.AppendCertsFromPEM(pem)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func (k *K8s) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errs.Wrap(errs.KindInternal, err, "request unreadable for Kubernetes")
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, k.apiServer+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, nil, errs.Wrap(errs.KindUnavailable, err, "failed to talk to the Kubernetes API")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, errs.Wrap(errs.KindInternal, err, "truncated response from Kubernetes")
	}
	return resp.StatusCode, out, nil
}

func k8sFail(code int, body []byte, what string) error {
	var st struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &st)
	msg := strings.TrimSpace(st.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	switch code {
	case http.StatusNotFound:
		return errs.NotFound("%s", what)
	case http.StatusConflict:
		return errs.New(errs.KindAlreadyExists, "%s: %s", what, msg)
	case http.StatusForbidden, http.StatusUnauthorized:
		return errs.Permission("Kubernetes denied %s: %s", what, msg)
	}
	return errs.Internal("Kubernetes refused %s (HTTP %d): %s", what, code, msg)
}

// ── Start ────────────────────────────────────────────────────────────────────

func (k *K8s) Start(ctx context.Context, spec ports.RunnerSpec) (*ports.RunnerStatus, error) {
	if err := validate(spec); err != nil {
		return nil, err
	}
	if st, err := k.Status(ctx, spec.RunnerHandle); err == nil {
		return st, nil
	} else if errs.KindOf(err) != errs.KindNotFound {
		return nil, err
	}
	if err := k.ensureNamespace(ctx, spec); err != nil {
		return nil, err
	}
	if err := k.ensureToken(ctx, spec); err != nil {
		_ = k.Destroy(ctx, spec.RunnerHandle)
		return nil, err
	}
	if len(spec.CachePaths) > 0 {
		if err := k.ensureCache(ctx, spec); err != nil {
			_ = k.Destroy(ctx, spec.RunnerHandle)
			return nil, err
		}
	}
	if err := k.ensurePod(ctx, spec); err != nil {
		_ = k.Destroy(ctx, spec.RunnerHandle)
		return nil, err
	}
	return &ports.RunnerStatus{Phase: ports.RunnerPending}, nil
}

func (k *K8s) ensureNamespace(ctx context.Context, spec ports.RunnerSpec) error {
	code, body, err := k.do(ctx, http.MethodPost, "/api/v1/namespaces", map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": spec.Namespace, "labels": k8sLabels(spec)},
	})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "creating the run's namespace")
	}
	return nil
}

// ensureCache claims the ACCOUNT's cache volume.
//
// The claim carries the account in its name and the namespace is the run's, so
// what makes two runs of one account share a cache is the underlying volume's
// binding — never a path built from the demand. A cache keyed by demand would
// be a cache that is always cold, which is the same as no cache at all.
func (k *K8s) ensureCache(ctx context.Context, spec ports.RunnerSpec) error {
	claim := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]string{"storage": k.cacheSize}},
	}
	if k.storageClass != "" {
		claim["storageClassName"] = k.storageClass
	}
	code, body, err := k.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+spec.Namespace+"/persistentvolumeclaims", map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{
				"name":   runCachePVC,
				"labels": k8sLabels(spec),
				// The account is an annotation and not part of the name: it is
				// what an operator reads to know whose cache this is, and what a
				// future reclaim policy will select on.
				"annotations": map[string]string{"dop.account": spec.AccountID},
			},
			"spec": claim,
		})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "claiming the account's cache")
	}
	return nil
}

func (k *K8s) ensureToken(ctx context.Context, spec ports.RunnerSpec) error {
	if spec.Repository.Token == "" {
		return nil
	}
	code, body, err := k.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+spec.Namespace+"/secrets", map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": tokenName(spec.RunnerHandle), "labels": k8sLabels(spec)},
			"stringData": map[string]string{runTokenKey: spec.Repository.Token},
		})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "delivering the run's credential")
	}
	return nil
}

func (k *K8s) ensurePod(ctx context.Context, spec ports.RunnerSpec) error {
	env := k8sEnv(spec.Env)
	var mounts []any
	var volumes []any
	if spec.Repository.Token != "" {
		// A projected file, not a variable: a build that dumps `env` into its
		// log must not carry the credential with it.
		volumes = append(volumes, map[string]any{
			"name": "git-token", "secret": map[string]any{"secretName": tokenName(spec.RunnerHandle)},
		})
		mounts = append(mounts, map[string]any{
			"name": "git-token", "mountPath": runTokenMount, "readOnly": true,
		})
		env = append(env, map[string]any{
			"name": "DOP_GIT_TOKEN_FILE", "value": runTokenMount + "/" + runTokenKey,
		})
	}
	for i, p := range spec.CachePaths {
		volumes = append(volumes, map[string]any{
			"name":                  runCachePVC,
			"persistentVolumeClaim": map[string]any{"claimName": runCachePVC},
		})
		mounts = append(mounts, map[string]any{
			"name": runCachePVC, "mountPath": p, "subPath": fmt.Sprintf("%d", i),
		})
		if i == 0 {
			continue
		}
		// One volume, many mounts: repeating the volume would be rejected by the
		// apiserver as a duplicate name.
		volumes = volumes[:len(volumes)-1]
	}

	containers := []any{map[string]any{
		"name":  runContainer,
		"image": spec.Image,
		// `command`, not `args`: here we DO mean to replace the image's
		// entrypoint — the run script is the container's whole reason to exist.
		// (The sandbox needs the opposite, and getting it backwards there cost a
		// silently empty workspace.)
		"command":      []string{"sh", "-c", Script(spec)},
		"env":          env,
		"volumeMounts": mounts,
	}}
	for _, d := range spec.Dependencies {
		containers = append(containers, map[string]any{
			"name":  "dep-" + d.Name,
			"image": d.Image,
			"env":   k8sEnv(d.Env),
		})
	}

	code, body, err := k.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+spec.Namespace+"/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name": podName(spec.RunnerHandle), "labels": k8sLabels(spec),
				// The application's port travels ON THE POD because Status only
				// receives a handle. Rebuilding it from the log would mean
				// parsing the run's own output for a fact we already knew when
				// we created it.
				"annotations": map[string]string{"dop.app-port": fmt.Sprintf("%d", spec.AppPort)},
			},
			"spec": map[string]any{
				// Never: the sequence decides what happens on a failure, and a
				// kubelet restarting the runner would replay a verification from
				// the top with half its evidence already emitted.
				"restartPolicy":                "Never",
				"automountServiceAccountToken": false,
				"containers":                   containers,
				"volumes":                      volumes,
			},
		})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "creating the run's pod")
	}
	return nil
}

// ── Status ───────────────────────────────────────────────────────────────────

func (k *K8s) Status(ctx context.Context, h ports.RunnerHandle) (*ports.RunnerStatus, error) {
	code, body, err := k.do(ctx, http.MethodGet,
		"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName(h), nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, errs.NotFound("run %s does not exist", h.ID)
	}
	if code >= 300 {
		return nil, k8sFail(code, body, "reading the run")
	}
	var pod struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Running *struct{} `json:"running"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &pod); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from Kubernetes")
	}
	alive := false
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == runContainer && cs.State.Running != nil {
			alive = true
		}
	}
	// A pod that has not scheduled yet has no log, and asking for one answers
	// 400. Pending is the honest answer, and it is what Start already returned.
	if pod.Status.Phase == "Pending" {
		return &ports.RunnerStatus{Phase: ports.RunnerPending}, nil
	}
	logs, err := k.readLogs(ctx, h)
	if err != nil {
		return nil, err
	}
	st := ParseLog(h, logs, alive)
	if st.Phase == ports.RunnerHolding {
		// The port is the pod's; the URL belongs to whoever knows the ingress
		// domain.
		var port int32
		_, _ = fmt.Sscanf(pod.Metadata.Annotations["dop.app-port"], "%d", &port)
		st.Endpoint = &ports.RunnerEndpoint{Port: port, State: "running"}
	}
	return st, nil
}

func (k *K8s) readLogs(ctx context.Context, h ports.RunnerHandle) (string, error) {
	code, body, err := k.do(ctx, http.MethodGet,
		"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName(h)+"/log?container="+runContainer, nil)
	if err != nil {
		return "", err
	}
	if code == http.StatusBadRequest || code == http.StatusNotFound {
		return "", nil // the container has not started: no log yet, and that is not an error
	}
	if code >= 300 {
		return "", k8sFail(code, body, "reading the run's log")
	}
	return string(body), nil
}

// ── Logs ─────────────────────────────────────────────────────────────────────

func (k *K8s) Logs(ctx context.Context, h ports.RunnerHandle, q ports.LogQuery, emit func(ports.LogLine) error) error {
	if _, err := k.Status(ctx, h); err != nil {
		return err
	}
	v := url.Values{}
	v.Set("container", runContainer)
	v.Set("timestamps", "true")
	if q.Follow {
		v.Set("follow", "true")
	}
	if q.TailLines > 0 {
		v.Set("tailLines", fmt.Sprintf("%d", q.TailLines))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		k.apiServer+"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName(h)+"/log?"+v.Encode(), nil)
	if err != nil {
		return err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	resp, err := k.stream.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errs.Wrap(errs.KindUnavailable, err, "failed to follow the run's log")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusBadRequest {
			return nil // no log yet
		}
		return k8sFail(resp.StatusCode, out, "reading the run's log")
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		at, text := splitTimestamp(sc.Text())
		if err := emit(ports.LogLine{Service: runContainer, Stream: "stdout", Text: text, At: at}); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "the run's log was interrupted")
	}
	return nil
}

// ── Destroy ──────────────────────────────────────────────────────────────────

// Destroy removes THIS run's pod and credential, and leaves the namespace and
// the account's cache standing.
//
// Deleting the namespace would be simpler and would take the cache with it —
// the account's, shared with runs that have nothing to do with this one. The
// guarantee is that the RUN is irreversibly gone, not that the account's space
// is.
//
// Idempotent: an absent pod is the outcome asked for.
func (k *K8s) Destroy(ctx context.Context, h ports.RunnerHandle) error {
	for _, path := range []string{
		"/api/v1/namespaces/" + h.Namespace + "/pods/" + podName(h) + "?gracePeriodSeconds=0",
		"/api/v1/namespaces/" + h.Namespace + "/secrets/" + tokenName(h),
	} {
		code, body, err := k.do(ctx, http.MethodDelete, path, nil)
		if err != nil {
			return err
		}
		if code >= 300 && code != http.StatusNotFound && code != http.StatusConflict {
			return k8sFail(code, body, "removing the run")
		}
	}
	// The pod terminates asynchronously. Waiting is what makes Destroy mean "it
	// is gone" to the caller that immediately asks Status — the guarantee is
	// irreversibility, not a queued intention.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		code, _, err := k.do(ctx, http.MethodGet,
			"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName(h), nil)
		if err != nil {
			return err
		}
		if code == http.StatusNotFound {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errs.New(errs.KindUnavailable, "the run's pod did not finish terminating")
}

// ── helpers ──────────────────────────────────────────────────────────────────

func k8sLabels(spec ports.RunnerSpec) map[string]string {
	return map[string]string{
		"dop.run":        labelValue(spec.ID),
		"dop.account":    labelValue(spec.AccountID),
		"dop.demand":     labelValue(spec.DemandID),
		"dop.managed-by": "dop-core",
	}
}

// labelValue keeps a value inside what Kubernetes accepts. An id that is not a
// valid label is not a reason to fail a run — it is a reason to store what fits.
func labelValue(s string) string {
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
}

func k8sEnv(m map[string]string) []any {
	out := make([]any, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]any{"name": k, "value": v})
	}
	return out
}
