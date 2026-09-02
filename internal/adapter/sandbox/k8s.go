// A SandboxLauncher adapter over Kubernetes.
//
// It is the product's orchestration surface in BOTH modes — SaaS on DOP's
// cluster and the client's infrastructure — changing the kubeconfig and the
// limits, not the implementation (substrate spec §2). One namespace per demand,
// one PVC with the workspace, one pod with the agent.
//
// It speaks through the cluster's API, with the service account's CA, in the
// same way and for the same reason as the SecretStore adapter: an object read
// through a mounted volume is eventually consistent, and here reading right
// after writing is the normal case — provisioning and describing happen in the
// same user request.
package sandbox

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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// k8sServiceAccountCA is where the kubelet mounts the cluster's CA in every pod.
const k8sServiceAccountCA = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

// Fixed names inside the demand's namespace. Fixed on purpose: the namespace
// already isolates, so the object needs no suffix — and a predictable name is
// what allows describing a sandbox knowing only the handle.
const (
	podName       = "sandbox"
	pvcName       = "workspace"
	containerName = "sandbox"
)

type K8s struct {
	client *http.Client
	stream *http.Client
	// tls is the SAME configuration as the HTTP transport's, kept because the
	// exec does not go through `http.Client`: it dials the socket by hand in
	// order to speak WebSocket (see websocket.go). Without keeping it, the exec
	// would trust only the public CAs and would fail against the certificate of
	// the cluster's own CA.
	tls       *tls.Config
	apiServer string
	token     string
	// workspaceSize is the size of the workspace's PVC. It is not on the port:
	// it is a deployment limit, and Docker has nothing to do with it.
	workspaceSize string
	storageClass  string
}

type K8sConfig struct {
	APIServer     string
	Token         string
	WorkspaceSize string
	StorageClass  string
	Client        *http.Client
	Timeout       time.Duration
}

func NewK8s(cfg K8sConfig) *K8s {
	c := cfg.Client
	if c == nil {
		to := cfg.Timeout
		if to <= 0 {
			to = 30 * time.Second
		}
		c = &http.Client{Timeout: to, Transport: k8sTransport()}
	}
	size := cfg.WorkspaceSize
	if size == "" {
		size = "10Gi"
	}
	return &K8s{
		client: c,
		// Zero Timeout: following a log lasts as long as the client does.
		stream:        &http.Client{Transport: k8sTransport()},
		tls:           k8sTLS(),
		apiServer:     strings.TrimRight(cfg.APIServer, "/"),
		token:         cfg.Token,
		workspaceSize: size,
		storageClass:  cfg.StorageClass,
	}
}

var _ ports.SandboxLauncher = (*K8s)(nil)

// k8sTransport trusts the cluster's CA BESIDES the public ones — the same trap
// as the SecretStore adapter's: the apiserver's certificate is signed by the
// cluster's own CA, which is in no bundle, and the pod comes up green only to
// break on the first real call.
func k8sTransport() *http.Transport {
	return &http.Transport{TLSClientConfig: k8sTLS()}
}

// k8sTLS exists separately because TWO paths need it: the `http.Client` of the
// ordinary calls and the hand-dialled socket of the exec. Duplicating the setup
// would guarantee that one of the two stopped trusting the cluster's CA on the
// first adjustment, and the failure would only appear on the first real call.
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

// k8sFail preserves the apiserver's message. It is what says "forbidden: cannot
// create resource pods" — the information that separates a five-minute fix from
// an afternoon of guessing.
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

// ── tiers ────────────────────────────────────────────────────────────────────

// runtimeClasses reads what the cluster offers. It is the spec's R-4 in one
// call: a Kata RuntimeClass is missing in most distributions, and the only
// acceptable answer to that is a refusal with a message.
func (k *K8s) runtimeClasses(ctx context.Context) ([]string, error) {
	code, body, err := k.do(ctx, http.MethodGet, "/apis/node.k8s.io/v1/runtimeclasses", nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		// We cannot answer "namespace only" here: that would assert the cluster
		// has no Kata when in truth we cannot look. Missing permission is an
		// installation problem and has to show up as one.
		return nil, errs.Permission(
			"no permission to list runtimeclasses.node.k8s.io — without it the " +
				"substrate cannot prove which isolation it offers")
	}
	if code == http.StatusNotFound {
		return nil, nil // a cluster with no RuntimeClass API: namespace isolation only
	}
	if code >= 300 {
		return nil, k8sFail(code, body, "listing runtimeclasses")
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from Kubernetes")
	}
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.Metadata.Name)
	}
	sort.Strings(out)
	return out, nil
}

func (k *K8s) SupportedTiers(ctx context.Context) ([]ports.IsolationTier, error) {
	classes, err := k.runtimeClasses(ctx)
	if err != nil {
		return nil, err
	}
	// A strict securityContext depends on no RuntimeClass at all: every cluster
	// delivers namespace isolation.
	tiers := []ports.IsolationTier{ports.TierNamespace}
	if matchClass(classes, "kata", "firecracker") != "" {
		tiers = append(tiers, ports.TierHardware)
	}
	if matchClass(classes, "gvisor", "runsc", "edera") != "" {
		tiers = append(tiers, ports.TierKernelEmulated)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] < tiers[j] })
	return tiers, nil
}

func matchClass(classes []string, want ...string) string {
	for _, c := range classes {
		for _, w := range want {
			if strings.Contains(strings.ToLower(c), w) {
				return c
			}
		}
	}
	return ""
}

// runtimeClassFor returns the RuntimeClass that DELIVERS the requested tier, or
// refuses. It never returns "the closest one": that is the line between declared
// isolation and presumed isolation.
func (k *K8s) runtimeClassFor(ctx context.Context, tier ports.IsolationTier) (string, error) {
	if tier == ports.TierNamespace {
		return "", nil // no RuntimeClass; the securityContext does the work
	}
	classes, err := k.runtimeClasses(ctx)
	if err != nil {
		return "", err
	}
	var found string
	switch tier {
	case ports.TierHardware:
		found = matchClass(classes, "kata", "firecracker")
	case ports.TierKernelEmulated:
		found = matchClass(classes, "gvisor", "runsc", "edera")
	}
	if found == "" {
		return "", errs.Precondition(
			"this cluster has no RuntimeClass for %q isolation — install the "+
				"corresponding runtime or ask for another level (substrate spec, R-4)", tier)
	}
	return found, nil
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func (k *K8s) Launch(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	// Resolved BEFORE creating the namespace: a refused tier leaves no trace.
	runtimeClass, err := k.runtimeClassFor(ctx, spec.Tier)
	if err != nil {
		return nil, err
	}

	if st, err := k.Describe(ctx, spec.SandboxHandle); err == nil {
		return st, nil // relaunching returns what exists (guarantee 4)
	} else if errs.KindOf(err) != errs.KindNotFound {
		return nil, err
	}

	if err := k.ensureNamespace(ctx, spec); err != nil {
		return nil, err
	}
	if err := k.ensureWorkspace(ctx, spec); err != nil {
		return nil, err
	}
	if err := k.ensurePod(ctx, spec, runtimeClass); err != nil {
		return nil, err
	}
	return k.Describe(ctx, spec.SandboxHandle)
}

func (k *K8s) ensureNamespace(ctx context.Context, spec ports.SandboxSpec) error {
	labels := labelsFor(spec)
	code, body, err := k.do(ctx, http.MethodPost, "/api/v1/namespaces", map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": spec.Namespace, "labels": labels},
	})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "creating the demand's namespace")
	}
	return nil
}

// ensureWorkspace creates the PVC. It is THE PVC that survives the suspension —
// the pod is disposable, the workspace is not.
func (k *K8s) ensureWorkspace(ctx context.Context, spec ports.SandboxSpec) error {
	claim := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]string{"storage": k.workspaceSize}},
	}
	if k.storageClass != "" {
		claim["storageClassName"] = k.storageClass
	}
	code, body, err := k.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+spec.Namespace+"/persistentvolumeclaims", map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{"name": pvcName, "labels": labelsFor(spec)},
			"spec":     claim,
		})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "creating the workspace")
	}
	return nil
}

func (k *K8s) ensurePod(ctx context.Context, spec ports.SandboxSpec, runtimeClass string) error {
	env := make([]map[string]string, 0, len(spec.Env))
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, map[string]string{"name": key, "value": spec.Env[key]})
	}

	container := map[string]any{
		"name":  containerName,
		"image": spec.Image,
		"env":   env,
		// `pods/exec` does NOT accept a working directory — only the command.
		// Pinning the container's is what makes the exec inherit it and what
		// makes the port's guarantee 14 the same on both substrates: a tool's
		// command starts in the workspace, here and in Docker.
		"workingDir": ports.SandboxWorkspacePath,
		"volumeMounts": []map[string]any{
			{"name": pvcName, "mountPath": ports.SandboxWorkspacePath},
		},
		// Defence in layers (spec §6): the agent reads untrusted content and
		// carries a credential. No capabilities, no privilege escalation.
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]any{"drop": []string{"ALL"}},
		},
	}
	if len(spec.Command) > 0 {
		container["command"] = spec.Command
	}

	podSpec := map[string]any{
		// Never: a sandbox dying in a loop has to SHOW UP as stopped. With an
		// automatic restart the domain would see "active" forever and the
		// developer would be staring at a terminal restarting on its own.
		"restartPolicy": "Never",
		"containers":    []any{container},
		"volumes": []map[string]any{
			{"name": pvcName, "persistentVolumeClaim": map[string]string{"claimName": pvcName}},
		},
		// An arbitrary NON-root user from the very first image: OKD and
		// OpenShift refuse root through SCC, and that is an image requirement,
		// not a deployment one (spec §2). fsGroup is what makes the workspace
		// writable for that user.
		"securityContext": map[string]any{
			"runAsNonRoot":   true,
			"runAsUser":      1000,
			"runAsGroup":     1000,
			"fsGroup":        1000,
			"seccompProfile": map[string]string{"type": "RuntimeDefault"},
		},
	}
	if runtimeClass != "" {
		podSpec["runtimeClassName"] = runtimeClass
	}

	code, body, err := k.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+spec.Namespace+"/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": podName, "labels": labelsFor(spec)},
			"spec":     podSpec,
		})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "creating the sandbox's pod")
	}
	return nil
}

// Suspend DELETES the pod and leaves the PVC.
//
// It is the divergence the contract suite made explicit: here the suspension
// takes everything that was not in the PVC — the pod's log included — while the
// Docker adapter merely stops the container and keeps the writable layer. That
// is why the port promises ONLY what is under SandboxWorkspacePath.
func (k *K8s) Suspend(ctx context.Context, h ports.SandboxHandle) error {
	if _, err := k.Describe(ctx, h); err != nil {
		return err
	}
	code, body, err := k.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName+"?gracePeriodSeconds=10", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return k8sFail(code, body, "suspending the sandbox")
	}
	return nil
}

// Resume recreates the pod ON TOP OF the existing PVC.
func (k *K8s) Resume(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	st, err := k.Describe(ctx, spec.SandboxHandle)
	if err != nil {
		return nil, err
	}
	if st.Phase == ports.PhaseActive {
		return st, nil
	}
	runtimeClass, err := k.runtimeClassFor(ctx, spec.Tier)
	if err != nil {
		return nil, err
	}
	// A stopped pod (Succeeded/Failed) does not "restart": it goes away and
	// comes back. Deleting it first is what makes Resume genuinely idempotent.
	if err := k.deletePodAndWait(ctx, spec.Namespace); err != nil {
		return nil, err
	}
	if err := k.ensurePod(ctx, spec, runtimeClass); err != nil {
		return nil, err
	}
	return k.Describe(ctx, spec.SandboxHandle)
}

// deletePodAndWait waits for the pod to really go away. Without the wait, the
// POST right after hits a 409 with the pod still terminating and Resume would
// fail every now and then — the worst kind of defect, the one that only shows up
// on other people's machines.
func (k *K8s) deletePodAndWait(ctx context.Context, ns string) error {
	code, body, err := k.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+ns+"/pods/"+podName+"?gracePeriodSeconds=0", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return k8sFail(code, body, "removing the previous pod")
	}
	for i := 0; i < 60; i++ {
		code, _, err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+podName, nil)
		if err != nil {
			return err
		}
		if code == http.StatusNotFound {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errs.New(errs.KindUnavailable, "the sandbox's previous pod did not terminate in time")
}

// Destroy deletes the WHOLE NAMESPACE: pod, PVC and everything the demand may
// have created inside it. It is what makes the destruction genuinely
// irreversible — deleting object by object would leave behind whatever nobody
// anticipated.
func (k *K8s) Destroy(ctx context.Context, h ports.SandboxHandle) error {
	code, body, err := k.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+h.Namespace, nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound && code != http.StatusConflict {
		return k8sFail(code, body, "destroying the sandbox")
	}
	return nil
}

func (k *K8s) Describe(ctx context.Context, h ports.SandboxHandle) (*ports.SandboxStatus, error) {
	nsTier, err := k.namespaceTier(ctx, h)
	if err != nil {
		return nil, err
	}

	code, body, err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+h.Namespace+"/pods/"+podName, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		// No pod, but a PVC: suspended. Neither: the namespace exists but it is
		// not a sandbox of ours — for the port, it does not exist.
		ok, err := k.workspaceExists(ctx, h)
		if err != nil {
			return nil, err
		}
		if ok {
			return &ports.SandboxStatus{Phase: ports.PhaseSuspended, Tier: nsTier}, nil
		}
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	if code >= 300 {
		return nil, k8sFail(code, body, "reading the sandbox")
	}

	var pod struct {
		Metadata struct {
			Labels            map[string]string `json:"labels"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Ports []struct {
					Name          string `json:"name"`
					ContainerPort int32  `json:"containerPort"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &pod); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from Kubernetes")
	}

	tier := ports.IsolationTier(pod.Metadata.Labels[labelTier])
	if tier == ports.TierUnspecified {
		tier = nsTier
	}
	st := &ports.SandboxStatus{Tier: tier}
	switch pod.Status.Phase {
	case "Running":
		st.Phase = ports.PhaseActive
	case "Pending":
		st.Phase = ports.PhaseProvisioning
	default: // Succeeded, Failed, Unknown — the execution ended, the PVC remains
		st.Phase = ports.PhaseSuspended
	}
	if pod.Metadata.DeletionTimestamp != nil {
		st.Phase = ports.PhaseSuspended
	}

	running := st.Phase == ports.PhaseActive
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			name := p.Name
			if name == "" {
				name = fmt.Sprintf("port-%d", p.ContainerPort)
			}
			state := "stopped"
			if running {
				state = "running"
			}
			st.Endpoints = append(st.Endpoints, ports.SandboxEndpoint{
				Name: name, Port: p.ContainerPort, State: state,
			})
		}
	}
	return st, nil
}

// namespaceTier reads the tier from the namespace's LABEL, which survives the
// suspension. An absent namespace — or one already Terminating — is a
// nonexistent sandbox: a namespace on its way out does not come back, and
// treating it as alive would make Destroy look like it did not work.
func (k *K8s) namespaceTier(ctx context.Context, h ports.SandboxHandle) (ports.IsolationTier, error) {
	code, body, err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+h.Namespace, nil)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", errs.NotFound("sandbox %s", h.ID)
	}
	if code >= 300 {
		return "", k8sFail(code, body, "reading the demand's namespace")
	}
	var ns struct {
		Metadata struct {
			Labels            map[string]string `json:"labels"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &ns); err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "unreadable response from Kubernetes")
	}
	if ns.Metadata.DeletionTimestamp != nil {
		return "", errs.NotFound("sandbox %s", h.ID)
	}
	return ports.IsolationTier(ns.Metadata.Labels[labelTier]), nil
}

func (k *K8s) workspaceExists(ctx context.Context, h ports.SandboxHandle) (bool, error) {
	code, body, err := k.do(ctx, http.MethodGet,
		"/api/v1/namespaces/"+h.Namespace+"/persistentvolumeclaims/"+pvcName, nil)
	if err != nil {
		return false, err
	}
	if code == http.StatusNotFound {
		return false, nil
	}
	if code >= 300 {
		return false, k8sFail(code, body, "reading the workspace")
	}
	return true, nil
}

// ── exec ─────────────────────────────────────────────────────────────────────

// wsExecProtocol is Kubernetes's channel subprotocol.
//
// `v4` and not `v5`: v5 adds only the closing of stdin, and this exec has no
// stdin. Asking for the higher version without needing it would trade
// compatibility with an older cluster for nothing.
const wsExecProtocol = "v4.channel.k8s.io"

// The subprotocol's channels. 0 (stdin) and 4 (resize) are not used here — which
// is exactly what separates "run a command" from "open a session".
const (
	channelStdout = 1
	channelStderr = 2
	channelStatus = 3
)

// Exec runs a command inside the sandbox's container (guarantees 13 to 18).
//
// The exit code is this adapter's delicate point. It does NOT come over HTTP and
// it does NOT come at the end of stdout: it comes on channel 3, as a
// `metav1.Status` in JSON, and only there. An exec that reads stdout/stderr and
// closes the connection works perfectly and returns the wrong exit code (zero)
// for every failed command — which is exactly the confusion guarantee 15 exists
// to prevent.
func (k *K8s) Exec(ctx context.Context, h ports.SandboxHandle, req ports.ExecRequest) (*ports.ExecResult, error) {
	if len(req.Command) == 0 {
		return nil, errs.Invalid("exec with no command")
	}
	st, err := k.Describe(ctx, h)
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

	v := url.Values{}
	v.Set("container", containerName)
	v.Set("stdout", "true")
	v.Set("stderr", "true")
	// stdin OFF: see websocket.go's header. A command, not a session.
	v.Set("stdin", "false")
	v.Set("tty", "false")
	for _, arg := range req.Command {
		v.Add("command", arg)
	}
	address := k.apiServer + "/api/v1/namespaces/" + h.Namespace + "/pods/" + podName +
		"/exec?" + v.Encode()

	header := http.Header{}
	if k.token != "" {
		header.Set("Authorization", "Bearer "+k.token)
	}
	ws, resp, err := wsDial(runCtx, address, header, wsExecProtocol, k.tls)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execution interrupted by the caller")
		}
		if runCtx.Err() != nil {
			// Not even the handshake fitted in the deadline. It is still a
			// RESULT: whoever asked for a short deadline needs to see "it did
			// not fit", not an infrastructure error sending them to investigate
			// the cluster.
			return &ports.ExecResult{ExitCode: -1, TimedOut: true}, nil
		}
		return nil, err
	}
	if ws == nil {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden &&
			strings.Contains(strings.ToLower(string(body)), "reject") {
			// `kubectl proxy` refuses `/pods/*/exec` by default. Its raw message
			// does not say what to do; this one does.
			return nil, errs.Permission(
				"the proxy in front of the API refused the exec path — run " +
					"`kubectl proxy --port=8001 --reject-paths='^$'` or point " +
					"K8S_API_SERVER straight at the apiserver")
		}
		return nil, k8sFail(resp.StatusCode, body, "running the command in the sandbox")
	}
	defer ws.Close()

	stdout, stderr := &cappedBuffer{max: limit}, &cappedBuffer{max: limit}
	var status []byte
	for {
		msg, err := ws.ReadMessage()
		if err != nil {
			break // EOF, a close, or a socket dropped by the deadline
		}
		if len(msg) == 0 {
			// k8s opens each channel with a message carrying only the channel
			// byte.
			continue
		}
		data := msg[1:]
		switch msg[0] {
		case channelStdout:
			_, _ = stdout.Write(data)
		case channelStderr:
			_, _ = stderr.Write(data)
		case channelStatus:
			status = append(status, data...)
		}
	}

	if ctx.Err() != nil {
		return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execution interrupted by the caller")
	}
	res := &ports.ExecResult{
		ExitCode:  -1,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
		TimedOut:  runCtx.Err() != nil,
	}
	if res.TimedOut {
		// With no status on channel 3, there was no termination: the process is
		// still in there. -1 and TimedOut say that; zero would assert success.
		return res, nil
	}
	applyExecStatus(res, status)
	return res, nil
}

// applyExecStatus reads the `metav1.Status` from channel 3.
//
// Three cases, and all three are a RESULT, never an error of the port:
//
//   - Success → code 0;
//   - Failure with an ExitCode cause → the code the process returned;
//   - Failure with no code cause (the classic "executable file not found") → -1,
//     and the substrate's message goes into stderr. It has to reach the MODEL,
//     which is who can fix the command; swallowing it would leave the agent with
//     an empty output and no clue.
func applyExecStatus(res *ports.ExecResult, raw []byte) {
	if len(raw) == 0 {
		return
	}
	var st struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	if st.Status == "Success" {
		res.ExitCode = 0
		return
	}
	for _, c := range st.Details.Causes {
		if c.Reason == "ExitCode" {
			if n, err := strconv.Atoi(strings.TrimSpace(c.Message)); err == nil {
				res.ExitCode = n
				return
			}
		}
	}
	if msg := strings.TrimSpace(st.Message); msg != "" {
		if res.Stderr != "" && !strings.HasSuffix(res.Stderr, "\n") {
			res.Stderr += "\n"
		}
		res.Stderr += msg
	}
}

// ── logs ─────────────────────────────────────────────────────────────────────

// Tail follows the pod's log and dies along with the caller.
//
// k8s delivers plain text, line by line, with an RFC3339 stamp when asked. There
// is no multiplexing: stdout and stderr arrive MERGED. It is the reason the port
// promises nothing about LogLine.Stream — Docker separates them, this one does
// not, and promising what only one delivers is the abstraction leaking.
func (k *K8s) Tail(ctx context.Context, h ports.SandboxHandle, q ports.LogQuery, emit func(ports.LogLine) error) error {
	if _, err := k.Describe(ctx, h); err != nil {
		return err
	}
	container := containerName
	if q.Service != "" {
		if q.Service != containerName {
			return errs.NotFound("process %q in sandbox %s", q.Service, h.ID)
		}
		container = q.Service
	}

	v := url.Values{}
	v.Set("container", container)
	v.Set("timestamps", "true")
	if q.Follow {
		v.Set("follow", "true")
	}
	if q.TailLines > 0 {
		v.Set("tailLines", fmt.Sprintf("%d", q.TailLines))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		k.apiServer+"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName+"/log?"+v.Encode(), nil)
	if err != nil {
		return err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	resp, err := k.stream.Do(req)
	if err != nil {
		if ctxEnded(ctx) {
			return nil
		}
		return errs.Wrap(errs.KindUnavailable, err, "failed to follow the logs")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		return k8sFail(resp.StatusCode, out, "reading the logs")
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if ctxEnded(ctx) {
			return nil
		}
		at, text := splitTimestamp(sc.Text())
		if err := emit(ports.LogLine{Service: containerName, Stream: "stdout", Text: text, At: at}); err != nil {
			return err
		}
	}
	return streamEnd(ctx, sc.Err())
}
