// Publishing the versioned templates to SendGrid.
//
// The files come from the REPOSITORY (templates/sendgrid/), and an idempotent
// script takes them to the provider — the same design as the sibling project,
// and for the same reason: a template that only exists inside the provider's
// editor has no history, goes through no review, does not roll back and does not
// survive an account change.
//
// The list of what to publish is DERIVED from the adapter's index
// (`sendgridIndex`). A second list here would be one more place to forget to
// update — and the oversight would be silent, which is exactly what ADR-0025
// requires preventing.
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

	"github.com/barrosef/dop-core/internal/platform/errs"
)

//go:embed templates/sendgrid/*.html
var sendgridFiles embed.FS

// TemplateSpec is a template to publish, with its HTML already loaded.
type TemplateSpec struct {
	Kind    string
	Name    string
	Subject string
	File    string
	HTML    []byte
}

// SendGridCatalog returns what this adapter needs to exist at the provider,
// derived from the index. Exported because the publisher is a script, which runs
// outside the process — see publish_templates.go.
func SendGridCatalog() ([]TemplateSpec, error) {
	kinds := sortedKeys(sendgridIndex)
	out := make([]TemplateSpec, 0, len(kinds))
	for _, kind := range kinds {
		spec := sendgridIndex[kind]
		html, err := sendgridFiles.ReadFile("templates/sendgrid/" + spec.File)
		if err != nil {
			// An index pointing at a file that does not exist is guarantee 1's
			// same silence, one step earlier: the template would never be
			// published, and the send would fail months later with "template
			// not found".
			return nil, errs.Precondition(
				"SendGrid's index cites %q for the %q notice, and the file was not embedded",
				spec.File, kind)
		}
		out = append(out, TemplateSpec{
			Kind: kind, Name: spec.Name, Subject: spec.Subject,
			File: spec.File, HTML: html,
		})
	}
	return out, nil
}

// PublishResult is what the script prints: the id to configure.
type PublishResult struct {
	Kind       string
	Name       string
	TemplateID string
	Created    bool
}

// PublishConfig is the minimum to publish. The key used here is NOT the sending
// one: publishing requires template administration scope, and giving that scope
// to the process that sends email would give the worker the power to rewrite
// what everybody receives.
type PublishConfig struct {
	APIKey  string
	BaseURL string
	Client  httpDoer
	Timeout time.Duration
}

// PublishSendGridTemplates creates what is missing and versions what already
// exists.
//
// IDEMPOTENT by NAME: running it twice does not create two templates. SendGrid
// has no "upsert", so the idempotency is ours — list, match by name, create only
// what was missing. The VERSION, that one is always new: it is how the provider
// keeps history, and it is what allows rolling back through its editor when
// somebody publishes broken HTML.
func PublishSendGridTemplates(ctx context.Context, cfg PublishConfig) ([]PublishResult, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errs.Invalid("publishing a template requires SendGrid's administration key")
	}
	catalog, err := SendGridCatalog()
	if err != nil {
		return nil, err
	}
	c := &publisher{
		base:   strings.TrimRight(orDefault(cfg.BaseURL, "https://api.sendgrid.com"), "/"),
		redact: redactor(cfg.APIKey),
	}
	c.http = cfg.Client
	if c.http == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		c.http = &http.Client{Timeout: t}
	}
	key := cfg.APIKey
	c.authorize = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }

	existing, err := c.list(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]PublishResult, 0, len(catalog))
	for _, spec := range catalog {
		id, created := existing[spec.Name], false
		if id == "" {
			id, err = c.create(ctx, spec.Name)
			if err != nil {
				return out, err
			}
			created = true
		}
		if err := c.addVersion(ctx, id, spec); err != nil {
			return out, err
		}
		out = append(out, PublishResult{
			Kind: spec.Kind, Name: spec.Name, TemplateID: id, Created: created,
		})
	}
	return out, nil
}

type publisher struct {
	base      string
	http      httpDoer
	authorize func(*http.Request)
	redact    func(string) string
}

func (c *publisher) list(ctx context.Context) (map[string]string, error) {
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
	// SendGrid has answered in both shapes depending on the API version;
	// accepting both is cheaper than discovering the difference in production.
	for _, t := range resp.Templates {
		out[t.Name] = t.ID
	}
	return out, nil
}

func (c *publisher) create(ctx context.Context, name string) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	err := c.call(ctx, http.MethodPost, "/v3/templates",
		map[string]any{"name": name, "generation": "dynamic"}, &resp)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", errs.New(errs.KindUnavailable,
			"SendGrid created template %q without returning an id", name)
	}
	return resp.ID, nil
}

func (c *publisher) addVersion(ctx context.Context, id string, spec TemplateSpec) error {
	// The version's name carries the INSTANT, and not a counter: a counter would
	// require reading the remote state to know where it is, and two simultaneous
	// publications would produce two "v3".
	return c.call(ctx, http.MethodPost, "/v3/templates/"+id+"/versions", map[string]any{
		"name":         "dop-" + time.Now().UTC().Format("20060102-150405"),
		"subject":      spec.Subject,
		"html_content": string(spec.HTML),
		"active":       1,
		"editor":       "code",
	}, nil)
}

func (c *publisher) call(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return errs.Wrap(errs.KindInternal, err, "request unreadable for SendGrid")
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return errs.Wrap(errs.KindInternal, err, "invalid request for SendGrid")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return errs.New(errs.KindUnavailable, "failed to talk to SendGrid: %s",
			c.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return errs.New(errs.KindUnauthorized,
				"SendGrid refused the template administration key (HTTP %d)", resp.StatusCode)
		}
		return errs.New(errs.KindUnavailable, "SendGrid refused %s %s (HTTP %d): %s",
			method, path, resp.StatusCode, c.redact(explainSG(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errs.New(errs.KindUnavailable, "unreadable response from SendGrid at %s: %s",
			path, c.redact(err.Error()))
	}
	return nil
}

// FormatPublishResults is what the script prints — the configuration lines
// ready to copy. It lives here, and not in the script, so it can be tested.
func FormatPublishResults(rs []PublishResult) string {
	lines := make([]string, 0, len(rs))
	for _, r := range rs {
		action := "updated"
		if r.Created {
			action = "created"
		}
		lines = append(lines, fmt.Sprintf("SENDGRID_TEMPLATE_%s=%s  # %s (%s)",
			strings.ToUpper(r.Kind), r.TemplateID, r.Name, action))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
