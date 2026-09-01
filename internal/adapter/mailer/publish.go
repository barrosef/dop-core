// Publicação dos templates versionados no SendGrid.
//
// Os arquivos são do REPOSITÓRIO (templates/sendgrid/), e é um script
// idempotente que os leva ao fornecedor — o mesmo desenho do projeto irmão, e
// pelo mesmo motivo: template que só existe dentro do editor do fornecedor não
// tem histórico, não passa por revisão, não volta atrás e não sobrevive à troca
// de conta.
//
// A lista do que publicar é DERIVADA do índice do adaptador (`sendgridIndex`).
// Uma segunda lista aqui seria mais um lugar para esquecer de atualizar — e o
// esquecimento seria silencioso, que é exatamente o que a ADR-0025 manda
// impedir.
package mailer

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

//go:embed templates/sendgrid/*.html
var sendgridFiles embed.FS

// TemplateSpec é um template a publicar, já com o HTML carregado.
type TemplateSpec struct {
	Kind    string
	Name    string
	Subject string
	File    string
	HTML    []byte
}

// SendGridCatalog devolve o que este adaptador precisa que exista no
// fornecedor, derivado do índice. Exportada porque quem publica é um script,
// que roda fora do processo — ver publish_templates.go.
func SendGridCatalog() ([]TemplateSpec, error) {
	kinds := chaves(sendgridIndex)
	out := make([]TemplateSpec, 0, len(kinds))
	for _, kind := range kinds {
		spec := sendgridIndex[kind]
		html, err := sendgridFiles.ReadFile("templates/sendgrid/" + spec.File)
		if err != nil {
			// Índice apontando para arquivo que não existe é o mesmo silêncio
			// da garantia 1, um passo antes: o template nunca seria publicado, e
			// o envio falharia meses depois com "template not found".
			return nil, errs.Precondition(
				"o índice do SendGrid cita %q para o aviso %q, e o arquivo não foi embutido",
				spec.File, kind)
		}
		out = append(out, TemplateSpec{
			Kind: kind, Name: spec.Name, Subject: spec.Subject,
			File: spec.File, HTML: html,
		})
	}
	return out, nil
}

// PublishResult é o que o script imprime: o id a configurar.
type PublishResult struct {
	Kind       string
	Name       string
	TemplateID string
	Created    bool
}

// PublishConfig é o mínimo para publicar. A chave usada aqui NÃO é a de envio:
// publicar exige escopo de administração de templates, e dar esse escopo ao
// processo que manda e-mail seria dar ao worker o poder de reescrever o que
// todo mundo recebe.
type PublishConfig struct {
	APIKey  string
	BaseURL string
	Client  httpDoer
	Timeout time.Duration
}

// PublishSendGridTemplates cria o que falta e versiona o que já existe.
//
// IDEMPOTENTE por NOME: rodar duas vezes não cria dois templates. O SendGrid não
// tem "upsert", então a idempotência é nossa — listar, casar por nome, criar só
// o que faltou. A VERSÃO, essa é sempre nova: é assim que o fornecedor guarda
// histórico, e é o que permite voltar atrás pelo editor dele quando alguém
// publicar um HTML quebrado.
func PublishSendGridTemplates(ctx context.Context, cfg PublishConfig) ([]PublishResult, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errs.Invalid("publicação de template exige chave de administração do SendGrid")
	}
	catalogo, err := SendGridCatalog()
	if err != nil {
		return nil, err
	}
	c := &publicador{
		base:    strings.TrimRight(naoVazio(cfg.BaseURL, "https://api.sendgrid.com"), "/"),
		redigir: redactor(cfg.APIKey),
	}
	c.http = cfg.Client
	if c.http == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		c.http = &http.Client{Timeout: t}
	}
	chave := cfg.APIKey
	c.autorizar = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+chave) }

	existentes, err := c.listar(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]PublishResult, 0, len(catalogo))
	for _, spec := range catalogo {
		id, criado := existentes[spec.Name], false
		if id == "" {
			id, err = c.criar(ctx, spec.Name)
			if err != nil {
				return out, err
			}
			criado = true
		}
		if err := c.versionar(ctx, id, spec); err != nil {
			return out, err
		}
		out = append(out, PublishResult{
			Kind: spec.Kind, Name: spec.Name, TemplateID: id, Created: criado,
		})
	}
	return out, nil
}

type publicador struct {
	base      string
	http      httpDoer
	autorizar func(*http.Request)
	redigir   func(string) string
}

func (c *publicador) listar(ctx context.Context) (map[string]string, error) {
	var resp struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
		Templates []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"templates"`
	}
	if err := c.call(ctx, http.MethodGet,
		"/v3/templates?generations=dynamic&page_size=200", nil, &resp); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, t := range resp.Result {
		out[t.Name] = t.ID
	}
	// O SendGrid já respondeu nas duas formas conforme a versão da API; aceitar
	// as duas é mais barato do que descobrir a diferença em produção.
	for _, t := range resp.Templates {
		out[t.Name] = t.ID
	}
	return out, nil
}

func (c *publicador) criar(ctx context.Context, nome string) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	err := c.call(ctx, http.MethodPost, "/v3/templates",
		map[string]any{"name": nome, "generation": "dynamic"}, &resp)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", errs.New(errs.KindUnavailable,
			"o SendGrid criou o template %q sem devolver id", nome)
	}
	return resp.ID, nil
}

func (c *publicador) versionar(ctx context.Context, id string, spec TemplateSpec) error {
	// O nome da versão carrega o INSTANTE, e não um contador: contador exigiria
	// ler o estado remoto para saber em quanto está, e duas publicações
	// simultâneas produziriam duas "v3".
	return c.call(ctx, http.MethodPost, "/v3/templates/"+id+"/versions", map[string]any{
		"name":         "dop-" + time.Now().UTC().Format("20060102-150405"),
		"subject":      spec.Subject,
		"html_content": string(spec.HTML),
		"active":       1,
		"editor":       "code",
	}, nil)
}

func (c *publicador) call(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return errs.Wrap(errs.KindInternal, err, "pedido ilegível para o SendGrid")
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return errs.Wrap(errs.KindInternal, err, "requisição inválida para o SendGrid")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.autorizar(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return errs.New(errs.KindUnavailable, "falha ao falar com o SendGrid: %s",
			c.redigir(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return errs.New(errs.KindUnauthorized,
				"o SendGrid recusou a chave de administração de templates (HTTP %d)", resp.StatusCode)
		}
		return errs.New(errs.KindUnavailable, "o SendGrid recusou %s %s (HTTP %d): %s",
			method, path, resp.StatusCode, c.redigir(explicarSG(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errs.New(errs.KindUnavailable, "resposta ilegível do SendGrid em %s: %s",
			path, c.redigir(err.Error()))
	}
	return nil
}

// FormatPublishResults é o que o script imprime — as linhas de configuração
// prontas para copiar. Sai daqui, e não do script, para poder ser testado.
func FormatPublishResults(rs []PublishResult) string {
	linhas := make([]string, 0, len(rs))
	for _, r := range rs {
		acao := "atualizado"
		if r.Created {
			acao = "criado"
		}
		linhas = append(linhas, fmt.Sprintf("SENDGRID_TEMPLATE_%s=%s  # %s (%s)",
			strings.ToUpper(r.Kind), r.TemplateID, r.Name, acao))
	}
	sort.Strings(linhas)
	return strings.Join(linhas, "\n")
}
