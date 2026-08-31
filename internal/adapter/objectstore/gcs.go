// Adaptador de ObjectStore sobre a API do Google Cloud Storage.
//
// Serve produção (GCS/Firebase Storage) E ambiente local (emulador do Firebase)
// — mesmo protocolo, endpoint diferente. É por isso que o MinIO saiu do desenho
// (ADR-0020): local e produção compartilham semântica, incluindo URL assinada.
//
// Ponte de variáveis, a armadilha que custa caro: o SDK do Cloud Storage lê
// STORAGE_EMULATOR_HOST; o Firebase CLI expõe FIREBASE_STORAGE_EMULATOR_HOST.
// Resolve-se no boot — sem isso, upload local vai para o bucket REAL.
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

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type GCS struct {
	client   *http.Client
	endpoint string // vazio = produção
	token    string
}

type GCSConfig struct {
	Endpoint string
	Token    string
	Client   *http.Client
}

// ResolveEmulatorHost faz a ponte entre as duas variáveis. Chamar no boot.
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
	return &GCS{client: c, endpoint: strings.TrimRight(ep, "/"), token: cfg.Token}
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
		return errs.Wrap(errs.KindUnavailable, err, "falha ao gravar objeto")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errs.Internal("armazenamento recusou a escrita (HTTP %d)", resp.StatusCode)
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
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao ler objeto")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errs.NotFound("objeto %s/%s", ref.Bucket, ref.Key)
	}
	if resp.StatusCode >= 300 {
		return nil, errs.Internal("armazenamento recusou a leitura (HTTP %d)", resp.StatusCode)
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
		return errs.Wrap(errs.KindUnavailable, err, "falha ao remover objeto")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return errs.Internal("armazenamento recusou a remoção (HTTP %d)", resp.StatusCode)
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
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao consultar objeto")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errs.NotFound("objeto %s/%s", ref.Bucket, ref.Key)
	}
	var out struct {
		Size        string `json:"size"`
		ContentType string `json:"contentType"`
		Updated     string `json:"updated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "metadados ilegíveis")
	}
	m := &ports.ObjectMeta{ContentType: out.ContentType}
	fmt.Sscanf(out.Size, "%d", &m.Size)
	if t, err := time.Parse(time.RFC3339, out.Updated); err == nil {
		m.UpdatedAt = t
	}
	return m, nil
}

// SignedPutURL / SignedGetURL: contra o emulador não há assinatura — devolve a
// URL direta, que é o comportamento correto localmente. Em produção, a
// assinatura V4 exige credencial de service account (a implementar quando o
// Terraform provisionar a SA).
func (g *GCS) SignedPutURL(_ context.Context, ref ports.ObjectRef, _ time.Duration) (string, error) {
	return fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key)), nil
}

func (g *GCS) SignedGetURL(_ context.Context, ref ports.ObjectRef, _ time.Duration) (string, error) {
	return fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media",
		g.endpoint, url.PathEscape(ref.Bucket), url.QueryEscape(ref.Key)), nil
}

var _ ports.ObjectStore = (*GCS)(nil)
