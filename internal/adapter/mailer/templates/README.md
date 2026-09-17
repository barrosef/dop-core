# E-mail templates

Two directories, and the duplication is DELIBERATE — it is the negative
consequence ADR-0018 accepts in writing ("two places for the same notice's
template for as long as both adapters exist"). It is neither an accident nor
laziness: it is the price of the `Mailer` port not leaking a provider's
vocabulary.

| Directory | Who uses it | Syntax | How it reaches its destination |
|---|---|---|---|
| `smtp/` | the SMTP adapter | Go `html/template` (`{{.field}}`) | embedded in the binary (`go:embed`), rendered on send |
| `sendgrid/` | the SendGrid adapter | Handlebars (`{{field}}`) | published at the provider by the idempotent script |

The syntaxes differ because the engines differ — a single file would not serve
both. What keeps one from falling behind is not discipline: it is the contract
suite, which requires EVERY adapter to resolve EVERY kind
`notification.Kinds()` returns.

The template CONTENT is still in Portuguese. That is notification content, not
code: localizing it means one template per locale at the provider (and one per
locale under `smtp/`), and it is a pending item — see ADR-0018.

## Publishing to SendGrid

The script is idempotent — it creates what is missing and creates a new version
of what already exists:

```
SENDGRID_TEMPLATES_API_KEY=SG.xxx \
  go run internal/adapter/mailer/publish_templates.go
```

It prints the resulting `template_id`s, which go into the configuration
(`SENDGRID_TEMPLATE_<KIND>`). The list of what to publish is DERIVED from the
adapter's index — there is no second list to forget to update.
