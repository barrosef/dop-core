//go:build ignore

// A script that publishes the email templates to SendGrid. Idempotent.
//
//	SENDGRID_TEMPLATES_API_KEY=SG.xxx \
//	  go run internal/adapter/mailer/publish_templates.go
//
// It prints the configuration lines (SENDGRID_TEMPLATE_<KIND>=d-…) ready to
// copy.
//
// It is a script, and not a MODE of the binary, on purpose: the dop-core image
// has four modes (ADR-0016) and all of them are long-running services.
// Publishing a template is a maintenance operation, runs with a DIFFERENT key —
// an administration one, not a sending one — and must not exist inside the
// process that sends email: giving the worker administration scope is giving it
// the power to rewrite what everybody receives.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/mailer"
)

func main() {
	key := os.Getenv("SENDGRID_TEMPLATES_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "set SENDGRID_TEMPLATES_API_KEY (the template ADMINISTRATION key)")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rs, err := mailer.PublishSendGridTemplates(ctx, mailer.PublishConfig{
		APIKey:  key,
		BaseURL: os.Getenv("SENDGRID_API"),
	})
	// It prints what ALREADY worked before dying: a failure on the third
	// template must not lose the ids of the first two, which are already created
	// at the provider and need to reach the configuration.
	if s := mailer.FormatPublishResults(rs); s != "" {
		fmt.Println(s)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "failure:", err)
		os.Exit(1)
	}
}
