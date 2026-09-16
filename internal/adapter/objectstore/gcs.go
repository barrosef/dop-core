// An ObjectStore adapter over the Google Cloud Storage API.
//
// It serves production (GCS/Firebase Storage) AND the local environment (the
// Firebase emulator) — the same protocol, a different endpoint. That is why
// MinIO left the design (ADR-0020): local and production share the semantics,
// signed URLs included.
//
// The variable bridge, the trap that costs dearly: the Cloud Storage SDK reads
// STORAGE_EMULATOR_HOST; the Firebase CLI exposes
// FIREBASE_STORAGE_EMULATOR_HOST. It is resolved at boot — without that, a local
// upload goes to the REAL bucket.
package objectstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

type GCS struct {
	client   *http.Client
	endpoint string // empty = production
	token    string
}

type GCSConfig struct {
	Endpoint string
	Token    string
	Client   *http.Client
}

// ResolveEmulatorHost bridges the two variables. Call it at boot.
func ResolveEmulatorHost() string {
	if h := os.Getenv("STORAGE_EMULATOR_HOST"); h != "" {
		return normalizeHost(h)
	}
	if h := os.Getenv("FIREBASE_STORAGE_EMULATOR_HOST"); h != "" {
		full := normalizeHost(h)
		_ = os.Setenv("STORAGE_EMULATOR_HOST", full)
		return full
	}
	return ""
}

func normalizeHost(h string) string {
	if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		return strings.TrimRight(h, "/")
	}
	return "http://" + strings.TrimRight(h, "/")
}

func NewGCS(cfg GCSConfig) *GCS {
	c := cfg.Client
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	ep := cfg.Endpoint
	if ep == "" {
		ep = ResolveEmulatorHost()
	}
	if ep == "" {
		ep = "https://storage.googleapis.com"
	}
	// normalizeHost here too, and not only in ResolveEmulatorHost: Firebase's
	// convention is host:port WITHOUT a scheme, and that is the shape that
	// arrives through configuration (wire.go reads STORAGE_EMULATOR_HOST
	// straight into cfg.Endpoint). Without this, url.Parse fails with "first
	// path segment cannot contain colon" — and only on the FIRST write, with the
	// process green until then.
	return &GCS{client: c, endpoint: normalizeHost(ep), token: cfg.Token}
}

func (g *GCS) auth(req *http.Request) {
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
}

func (g *GCS) Put(ctx context.Context, ref ports.ObjectRef, content []byte, contentType string) error {
	u := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(content)))
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	g.auth(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to write the object")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errs.Internal("storage refused the write (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func (g *GCS) Get(ctx context.Context, ref ports.ObjectRef) ([]byte, error) {
	u := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	g.auth(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to read the object")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errs.NotFound("object %s/%s", ref.Bucket, ref.Key)
	}
	if resp.StatusCode >= 300 {
		return nil, errs.Internal("storage refused the read (HTTP %d)", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (g *GCS) Delete(ctx context.Context, ref ports.ObjectRef) error {
	u := fmt.Sprintf("%s/storage/v1/b/%s/o/%s",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key))
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	g.auth(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to remove the object")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return errs.Internal("storage refused the removal (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func (g *GCS) Stat(ctx context.Context, ref ports.ObjectRef) (*ports.ObjectMeta, error) {
	u := fmt.Sprintf("%s/storage/v1/b/%s/o/%s",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	g.auth(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to stat the object")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errs.NotFound("object %s/%s", ref.Bucket, ref.Key)
	}
	var out struct {
		Size        string `json:"size"`
		ContentType string `json:"contentType"`
		Updated     string `json:"updated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable metadata")
	}
	m := &ports.ObjectMeta{ContentType: out.ContentType}
	fmt.Sscanf(out.Size, "%d", &m.Size)
	if t, err := time.Parse(time.RFC3339, out.Updated); err == nil {
		m.UpdatedAt = t
	}
	return m, nil
}

// SignedPutURL / SignedGetURL: against the emulator there is no signature — it
// returns the direct URL, which is the correct behaviour locally. In production,
// the V4 signature requires a service-account credential (to be implemented once
// Terraform provisions the SA).
func (g *GCS) SignedPutURL(_ context.Context, ref ports.ObjectRef, _ time.Duration) (string, error) {
	return fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key)), nil
}

func (g *GCS) SignedGetURL(_ context.Context, ref ports.ObjectRef, _ time.Duration) (string, error) {
	return fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key)), nil
}

var _ ports.ObjectStore = (*GCS)(nil)
