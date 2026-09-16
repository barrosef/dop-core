package contract_test

// The tests that cross BOTH adapters.
//
// The suite (mailer.go) runs once per adapter and proves each one delivers the
// port. What it cannot say on its own is what only appears when you compare the
// two — and that is where ADR-0025's defect lives: one provider falls behind and
// nothing turns red.

import (
	"strings"
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/mailer"
	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

func errsKindOf(err error) errs.Kind { return errs.KindOf(err) }
func contains(s, sub string) bool    { return strings.Contains(s, sub) }

// EVERY adapter resolves EVERY kind — said in one go, with no double and no
// network.
//
// It is the suite's subtest 1's same assertion, written here so the failure
// message names WHICH provider fell behind. With the suite alone, whoever adds a
// kind sees "sendgrid failed" and "smtp failed" in two distant blocks, and the
// right conclusion — "the new kind has no template anywhere" — depends on the
// reader putting the two together.
func TestEveryAdapterResolvesEveryPolicyKind(t *testing.T) {
	kinds := notification.KindNames()
	if len(kinds) == 0 {
		t.Fatal("the policy declares no kind at all")
	}
	adaptadores := map[string]ports.Mailer{
		// In DRY RUN: no credential and no network. The port's guarantee 3
		// requires the resolution to happen anyway, and that is what lets this
		// test exist with no double at all.
		"sendgrid": mailer.NewSendGrid(mailer.SendGridConfig{}),
		"smtp":     mailer.NewSMTP(mailer.SMTPConfig{}),
	}
	for name, m := range adaptadores {
		for _, kind := range kinds {
			if err := m.Resolve(t.Context(), kind); err != nil {
				t.Errorf("the %q notice has no template in adapter %q: %v\n"+
					"For as long as that lasts, whoever has that provider wired "+
					"simply RECEIVES NOTHING — no error, no log, no metric.",
					kind, name, err)
			}
		}
	}
}

// The publishing script has to cover everything the SendGrid adapter promises
// to resolve.
//
// Without this test, the hole is the following: the index has the kind, the
// suite is green, the id goes into the configuration — and the template was
// never published, because the script's catalog did not include it. The send
// fails in production with "template not found", months after the suite
// approved.
func TestThePublishCatalogCoversTheSendGridIndex(t *testing.T) {
	catalogo, err := mailer.SendGridCatalog()
	if err != nil {
		t.Fatalf("invalid publishing catalog: %v", err)
	}
	vistos := map[string]bool{}
	for _, spec := range catalogo {
		if len(spec.HTML) == 0 {
			t.Errorf("the %q (%s) template is empty", spec.Kind, spec.File)
		}
		if strings.TrimSpace(spec.Subject) == "" {
			t.Errorf("the %q template has no subject to publish", spec.Kind)
		}
		if spec.Name == "" {
			t.Errorf("the %q template has no name — it is the name that makes publishing "+
				"idempotent; without it, every run creates a new template", spec.Kind)
		}
		vistos[spec.Kind] = true
	}
	for _, kind := range notification.KindNames() {
		if !vistos[kind] {
			t.Errorf("the %q notice is not in the publishing catalog: the template never "+
				"reaches SendGrid, and the send fails with 'template not found'", kind)
		}
	}
}

// Both templates of the SAME notice have to speak of the same data.
//
// It is the "two places for the same notice's template" ADR-0025 accepts as a
// cost. What it asks in return is that the suite stops one of them from falling
// behind — and "falling behind" is not only absence: it is also SMTP starting to
// show a field SendGrid ignores. This test does not compare HTML (they would be
// two different engines); it compares which VARIABLES each side consumes.
func TestBothTemplatesConsumeTheSameFields(t *testing.T) {
	catalogo, err := mailer.SendGridCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, spec := range catalogo {
		hbs := handlebarsFields(string(spec.HTML))
		gos := goFields(t, spec.File)
		for c := range gos {
			if !hbs[c] {
				t.Errorf("%s: the SMTP template uses %q and SendGrid's does not — whoever is "+
					"on SendGrid receives the notice without that information", spec.Kind, c)
			}
		}
		for c := range hbs {
			if !gos[c] {
				t.Errorf("%s: the SendGrid template uses %q and SMTP's does not — whoever is "+
					"self-hosted receives the notice without that information", spec.Kind, c)
			}
		}
	}
}

// handlebarsFields extracts the names used in a SendGrid template.
// Reconhece `{{campo}}`, `{{#if campo}}`, `{{#each campo}}` e `{{this.campo}}`.
func handlebarsFields(s string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range entreChaves(s) {
		raw = strings.TrimSpace(raw)
		raw = strings.TrimPrefix(raw, "#if ")
		raw = strings.TrimPrefix(raw, "#each ")
		if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "#") {
			continue
		}
		raw = strings.TrimPrefix(raw, "this.")
		if raw != "" && raw != "subject" {
			out[raw] = true
		}
	}
	return out
}

// goFields extracts the names used in an SMTP template. It recognizes
// `{{.field}}`, `{{if .field}}` and `{{range .field}}`; inside a `range`,
// `{{.field}}` is the item's field — and that is why the name, not the path, is
// what gets compared.
func goFields(t *testing.T, file string) map[string]bool {
	t.Helper()
	raw, err := mailer.SMTPTemplateSource(file)
	if err != nil {
		t.Fatalf("source do template SMTP %q: %v", file, err)
	}
	out := map[string]bool{}
	for _, exp := range entreChaves(raw) {
		exp = strings.TrimSpace(exp)
		for _, prefixo := range []string{"if ", "range ", "with ", "else if "} {
			exp = strings.TrimPrefix(exp, prefixo)
		}
		if exp == "end" || exp == "else" || !strings.HasPrefix(exp, ".") {
			continue
		}
		name := strings.TrimPrefix(exp, ".")
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func entreChaves(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			return out
		}
		s = s[i+2:]
		j := strings.Index(s, "}}")
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+2:]
	}
}

// Each template DECLARES the kind it serves, and the index has to agree.
//
// ── The hole this test closes, and how it was found ─────────────────────────
//
// After every expected break had failed as it should, the question left was what
// the suite did NOT catch. Two sabotages passed green:
//
//  1. swapping the FILES of `invite` and `attention_digest` in SMTP's index —
//     only a hand-written test for the digest complained, and a NEW KIND would
//     not have that test;
//  2. swapping the SUBJECTS of the two kinds in SendGrid's index — nothing
//     complained.
//
// They are the same family and it is the worst of them: the adapter resolves the
// RIGHT kind to the WRONG artifact. Guarantee 1 is still delivered — nobody is
// left without receiving anything — and the invited person receives a digest of
// pending items from an account they are not part of yet.
//
// The structural half (1) closes here, and closes GENERICALLY — it holds for
// every kind that comes later, with nobody having to remember: the file declares
// the kind in its header, and the index has to point at the file that declares
// that kind. The SUBJECT half (2) stays open and is in the report: two texts
// written by people, swapped between two lines of a table, are not machine
// distinguishable without a redundant declaration — and the right redundancy is
// the subject living INSIDE the template, which SendGrid does not allow while it
// keeps the subject separate from the body.
func TestEachTemplateDeclaresTheKindItServes(t *testing.T) {
	catalogo, err := mailer.SendGridCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, spec := range catalogo {
		marker := "dop-template: " + spec.Kind

		// SendGrid: the HTML the script is going to publish.
		if !contains(string(spec.HTML), marker) {
			t.Errorf("the SendGrid template of the %q notice (%s) does not declare %q: the index may "+
				"be pointing at the file of another kind, and whoever receives it will read "+
				"the wrong message", spec.Kind, spec.File, marker)
		}

		// SMTP: the file the index associates with the SAME kind.
		file, err := mailer.SMTPTemplateFile(spec.Kind)
		if err != nil {
			t.Errorf("the %q notice has no file in SMTP's index: %v", spec.Kind, err)
			continue
		}
		source, err := mailer.SMTPTemplateSource(file)
		if err != nil {
			t.Errorf("source de %q: %v", file, err)
			continue
		}
		if !contains(source, marker) {
			t.Errorf("SMTP's index associates the %q notice with file %q, and that file "+
				"declares it serves another kind — the message goes out with the right label "+
				"and the wrong content, which is worse than not going out", spec.Kind, file)
		}
	}
}
