// Package logging entrega log estruturado em JSON para todos os modos do core.
//
// Formato idêntico ao do BFF (dop-api): mesmos nomes de campo, mesmo formato de
// tempo, mesma política de máscara. Um log agregado de plataforma só é útil se as
// duas pontas falarem a mesma língua.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// Campos canônicos — os mesmos no core e no BFF.
const (
	FieldRequestID  = "request_id"
	FieldAccountID  = "account_id"
	FieldActorID    = "actor_id"
	FieldActorKind  = "actor_kind"
	FieldComponent  = "component"
	FieldMode       = "mode"
	FieldDurationMs = "duration_ms"
	FieldError      = "error"
)

// Chaves cujo valor nunca é escrito em log — redação de segredos é requisito
// (F-10), não conveniência. Espelha a máscara automática do BFF.
var masked = map[string]bool{
	"password": true, "token": true, "secret": true, "authorization": true,
	"api_key": true, "private_key": true, "client_secret": true,
	"credential": true, "credential_ref": false, // a REFERÊNCIA pode aparecer
}

const redacted = "***"

// New devolve o logger raiz do processo.
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

// redact aplica a máscara e normaliza o timestamp para RFC3339 em UTC.
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

// Into guarda o logger no contexto para propagação por camada.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From recupera o logger do contexto; devolve o default se não houver.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
