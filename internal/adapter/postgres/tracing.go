package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/tracing"
)

// NewPool opens the pool with the query tracer installed (ADR-0024 §4): every
// statement becomes a child span of whatever request ran it. The pool is the
// only place a connection is configured, so the tracer cannot be forgotten
// by a repository.
func NewPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errs.Wrap(errs.KindInvalid, err, "invalid DATABASE_URL")
	}
	cfg.ConnConfig.Tracer = queryTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to open the Postgres pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errs.Wrap(errs.KindUnavailable, err, "Postgres unreachable")
	}
	return pool, nil
}

// queryTracer is pgx's QueryTracer over OpenTelemetry: a span per statement,
// named by its first word, carrying the statement text truncated. Arguments
// are never recorded — a credential's value passes through SetCredential's
// statement, and a span is a log.
type queryTracer struct{}

const statementMax = 240

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := data.SQL
	if len(sql) > statementMax {
		sql = sql[:statementMax] + "…"
	}
	ctx, _ = tracing.Tracer().Start(ctx, "pg "+firstWord(data.SQL),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", sql),
		))
	return ctx
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, data.Err.Error())
	}
	span.End()
}

func firstWord(sql string) string {
	i := 0
	for i < len(sql) && (sql[i] == ' ' || sql[i] == '\n' || sql[i] == '\t') {
		i++
	}
	j := i
	for j < len(sql) && sql[j] != ' ' && sql[j] != '\n' && sql[j] != '\t' && sql[j] != '(' {
		j++
	}
	if j-i > 16 {
		j = i + 16
	}
	return sql[i:j]
}
