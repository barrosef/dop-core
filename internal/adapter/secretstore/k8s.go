// Adaptador de SecretStore sobre Secrets do Kubernetes.
//
// Usado no ambiente local (k3d) e em clusters self-hosted. Não existe emulador
// oficial do GCP Secret Manager, então este adaptador é exercitado TODO DIA —
// o que é exatamente a disciplina de dois adaptadores da ADR-0001.
//
// Detalhe que evita um bug silencioso: lê pela API do Kubernetes, NUNCA por
// volume montado. Volume é eventualmente consistente (o kubelet sincroniza em
// torno de um minuto) e violaria a garantia de leitura-após-escrita.
package secretstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

// serviceAccountCA é onde o kubelet monta a CA do cluster em todo pod.
const serviceAccountCA = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

func NewK8s(cfg K8sConfig) *K8s {
	c := cfg.Client
	if c == nil {
		c = defaultClient()
	}
	return &K8s{client: c, apiServer: strings.TrimRight(cfg.APIServer, "/"), token: cfg.Token, namespace: cfg.Namespace}
}

// defaultClient confia na CA do cluster ALÉM das públicas.
//
// O certificado do apiserver é assinado pela CA do próprio cluster, que não
// está em bundle nenhum: com o pool padrão toda chamada morre em
// "x509: certificate signed by unknown authority". A falha é traiçoeira
// porque nada no boot toca a API — o pod sobe verde e só quebra na PRIMEIRA
// credencial gravada, longe da causa.
//
// Fora do cluster o arquivo não existe e caímos no pool do sistema, que é o
// certo para apiserver com certificado público e para o adaptador do GCP.
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

// secretName mapeia a referência lógica para um nome de Secret válido.
// O isolamento entre contas está no nome — referência da conta A jamais
// resolve segredo da conta B (garantia 5 do contrato).
func (k *K8s) secretName(ref ports.SecretRef) string {
	return fmt.Sprintf("dop-%s-%s-%s",
		sanitize(ref.AccountID), sanitize(ref.Kind), sanitize(ref.OwnerID))
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
		return 0, nil, errs.Wrap(errs.KindUnavailable, err, "falha ao falar com a API do Kubernetes")
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
	// Substitui se existir (garantia 4), cria se não.
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
		return errs.Internal("Kubernetes recusou a escrita do segredo (HTTP %d)", code)
	}
	return nil
}

func (k *K8s) Get(ctx context.Context, ref ports.SecretRef) (ports.SecretValue, error) {
	code, body, err := k.do(ctx, http.MethodGet, k.url(k.secretName(ref)), nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil // garantia 2: ausente devolve nil, não erro
	}
	if code >= 300 {
		return nil, errs.Internal("Kubernetes recusou a leitura do segredo (HTTP %d)", code)
	}
	var out struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "resposta ilegível da API do Kubernetes")
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
	if code >= 300 && code != http.StatusNotFound { // garantia 3: idempotente
		return errs.Internal("Kubernetes recusou a remoção do segredo (HTTP %d)", code)
	}
	return nil
}

func (k *K8s) Exists(ctx context.Context, ref ports.SecretRef) (bool, error) {
	v, err := k.Get(ctx, ref)
	return v != nil, err
}

var _ ports.SecretStore = (*K8s)(nil)
