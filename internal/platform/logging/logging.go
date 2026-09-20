// Package logging provides structured JSON logging for every mode of the core.
//
// The format is identical to the BFF's (dop-api): same field names, same time
// format, same masking policy. An aggregated platform log is only useful if
// both ends speak the same language.
package logging

import (
	"context"
	"log/slog"

	"github.com/barrosef/dop-core/internal/platform/tracing"
	"os"
	"strings"
)

// Canonical fields — the same in the core and in the BFF.
const (
	FieldRequestID  = "request_id"
	FieldTraceID    = "trace_id"
	FieldSpanID     = "span_id"
	FieldAccountID  = "account_id"
	FieldActorID    = "actor_id"
	FieldActorKind  = "actor_kind"
	FieldComponent  = "component"
	FieldMode       = "mode"
	FieldDurationMs = "duration_ms"
	FieldError      = "error"
)

// Keys whose value is never written to the log — redacting secrets is a
// requirement (F-10), not a convenience. Mirrors the BFF's automatic masking.
var masked = map[string]bool{
	"password": true, "token": true, "secret": true, "authorization": true,
	"api_key": true, "private_key": true, "client_secret": true,
	"credential": true, "credential_ref": false, // the REFERENCE may appear
}

const redacted = "***"

// New returns the process's root logger.
func New(mode string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:       level(),
		ReplaceAttr: redact,
	})
	return slog.New(h).With(slog.String(FieldComponent, "dop-core"), slog.String(FieldMode, mode))
}

func level() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// redact applies the mask and normalizes the timestamp to RFC3339 in UTC.
func redact(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		a.Key = "ts"
		return a
	}
	if a.Key == slog.MessageKey {
		a.Key = "msg"
		return a
	}
	if m, ok := masked[strings.ToLower(a.Key)]; ok && m {
		return slog.String(a.Key, redacted)
	}
	return a
}

type ctxKey struct{}

// Into stores the logger in the context so it propagates across layers.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From retrieves the logger from the context; returns the default if absent.
//
// When a span is active, the logger it returns carries trace_id and span_id
// (ADR-0024 §4): every line written under a request joins the trace without
// the call site knowing a span exists.
func From(ctx context.Context) *slog.Logger {
	l, ok := ctx.Value(ctxKey{}).(*slog.Logger)
	if !ok {
		l = slog.Default()
	}
	if traceID, spanID := tracing.IDs(ctx); traceID != "" {
		return l.With(slog.String(FieldTraceID, traceID), slog.String(FieldSpanID, spanID))
	}
	return l
}
