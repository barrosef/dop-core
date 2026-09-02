// A SecretStore adapter over Kubernetes Secrets.
//
// Used in the local environment (k3d) and in self-hosted clusters. There is no
// official GCP Secret Manager emulator, so this adapter is exercised EVERY DAY —
// which is exactly ADR-0001's two-adapter discipline.
//
// A detail that avoids a silent bug: it reads through the Kubernetes API, NEVER
// through a mounted volume. A volume is eventually consistent (the kubelet syncs
// on the order of a minute) and would violate the read-after-write guarantee.
package secretstore

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type K8s struct {
	client    *http.Client
	apiServer string
	token     string
	namespace string
}

type K8sConfig struct {
	APIServer string
	Token     string
	Namespace string
	Client    *http.Client
}

// serviceAccountCA is where the kubelet mounts the cluster's CA in every pod.
const serviceAccountCA = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

func NewK8s(cfg K8sConfig) *K8s {
	c := cfg.Client
	if c == nil {
		c = defaultClient()
	}
	return &K8s{client: c, apiServer: strings.TrimRight(cfg.APIServer, "/"), token: cfg.Token, namespace: cfg.Namespace}
}

// defaultClient trusts the cluster's CA BESIDES the public ones.
//
// The apiserver's certificate is signed by the cluster's own CA, which is in no
// bundle: with the default pool every call dies in "x509: certificate signed by
// unknown authority". The failure is treacherous because nothing at boot touches
// the API — the pod comes up green and only breaks on the FIRST credential
// written, far from the cause.
//
// Outside the cluster the file does not exist and we fall back to the system
// pool, which is the right thing for an apiserver with a public certificate and
// for the GCP adapter.
func defaultClient() *http.Client {
	c := &http.Client{Timeout: 10 * time.Second}
	pem, err := os.ReadFile(serviceAccountCA)
	if err != nil {
		return c
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return c
	}
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return c
}

// secretName maps the logical reference to a valid Secret name.
//
// Isolation between accounts lives in the NAME (the port's guarantee 5), and
// that is why it ends in a fingerprint of the RAW tuple.
//
// Without it there was a real collision: `sanitize` emits `-`, the same
// character that separated the fields, so {account:"a-b", kind:"c"} and
// {account:"a", kind:"b-c"} produced the SAME Secret — `dop-a-b-c-d` for both.
// One account would read the other's secret. Changing the separator does not fix
// it: `sanitize` turns any character outside the alphabet into the separator,
// whatever it is. Only hashing the original tuple makes the guarantee a
// property, and not a bet on the identifiers' shape.
//
// The readable prefix stays because `kubectl get secret` without it is
// unreadable.
func (k *K8s) secretName(ref ports.SecretRef) string {
	return fmt.Sprintf("dop-%s-%s-%s-%s",
		sanitize(ref.AccountID), sanitize(ref.Kind), sanitize(ref.OwnerID),
		fingerprintK8s(ref))
}

// fingerprintK8s tells apart tuples sanitize would confuse. Each field's LENGTH
// goes into the hash: without it, {"ab",""} and {"a","b"} would collide again,
// now by concatenation.
func fingerprintK8s(r ports.SecretRef) string {
	h := sha256.New()
	for _, s := range []string{r.AccountID, r.Kind, r.OwnerID} {
		fmt.Fprintf(h, "%d:", len(s))
		_, _ = h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

const secretKey = "value"

func (k *K8s) url(name string) string {
	return fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", k.apiServer, k.namespace, name)
}

func (k *K8s) do(ctx context.Context, method, url string, body []byte) (int, []byte, error) {
	var rdr *strings.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, nil, errs.Wrap(errs.KindUnavailable, err, "failed to talk to the Kubernetes API")
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, buf, nil
}

func (k *K8s) Put(ctx context.Context, ref ports.SecretRef, v ports.SecretValue) error {
	name := k.secretName(ref)
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "dop-core",
				"dop.dev/account":              sanitize(ref.AccountID),
			},
		},
		"type": "Opaque",
		"data": map[string]string{secretKey: b64(v)},
	})
	// Replace if it exists (guarantee 4), create if not.
	code, _, err := k.do(ctx, http.MethodPut, k.url(name), body)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		code, _, err = k.do(ctx, http.MethodPost,
			fmt.Sprintf("%s/api/v1/namespaces/%s/secrets", k.apiServer, k.namespace), body)
		if err != nil {
			return err
		}
	}
	if code >= 300 {
		return errs.Internal("Kubernetes refused the secret write (HTTP %d)", code)
	}
	return nil
}

func (k *K8s) Get(ctx context.Context, ref ports.SecretRef) (ports.SecretValue, error) {
	code, body, err := k.do(ctx, http.MethodGet, k.url(k.secretName(ref)), nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil // guarantee 2: absent returns nil, not an error
	}
	if code >= 300 {
		return nil, errs.Internal("Kubernetes refused the secret read (HTTP %d)", code)
	}
	var out struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable response from the Kubernetes API")
	}
	raw, ok := out.Data[secretKey]
	if !ok {
		return nil, nil
	}
	return unb64(raw)
}

func (k *K8s) Delete(ctx context.Context, ref ports.SecretRef) error {
	code, _, err := k.do(ctx, http.MethodDelete, k.url(k.secretName(ref)), nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound { // guarantee 3: idempotent
		return errs.Internal("Kubernetes refused the secret deletion (HTTP %d)", code)
	}
	return nil
}

func (k *K8s) Exists(ctx context.Context, ref ports.SecretRef) (bool, error) {
	v, err := k.Get(ctx, ref)
	return v != nil, err
}

var _ ports.SecretStore = (*K8s)(nil)
