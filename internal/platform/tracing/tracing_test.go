package tracing_test

import (
	"context"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/platform/tracing"
	"go.opentelemetry.io/otel/trace"
)

// The one guarantee the rest of the code base relies on: a traceparent
// rendered on one side is the parent on the other — with no exporter at all,
// because the propagator is installed regardless of the backend.
func TestTraceParentRoundTripsWithoutAnExporter(t *testing.T) {
	ctx := context.Background()
	if _, err := tracing.Setup(ctx, tracing.Config{}); err != nil {
		t.Fatal(err)
	}
	if tracing.TraceParent(ctx) != "" {
		t.Fatal("no span, no traceparent")
	}
	remote := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	child := tracing.WithTraceParent(ctx, remote)
	sc := trace.SpanContextFromContext(child)
	if !sc.IsValid() || sc.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("the remote trace should be the parent, got %+v", sc)
	}
	if got := tracing.TraceParent(child); got != remote {
		t.Errorf("round trip: got %q, want %q", got, remote)
	}
	traceID, spanID := tracing.IDs(child)
	if traceID != "0af7651916cd43dd8448eb211c80319c" || spanID != "b7ad6b7169203331" {
		t.Errorf("IDs() = %q, %q", traceID, spanID)
	}
	// A malformed value leaves the context untouched instead of failing.
	if trace.SpanContextFromContext(tracing.WithTraceParent(ctx, "garbage")).IsValid() {
		t.Error("garbage must not produce a span context")
	}
}

func TestUnknownBackendIsRefusedAtBoot(t *testing.T) {
	if _, err := tracing.Setup(context.Background(), tracing.Config{Backend: "datadog"}); err == nil {
		t.Error("an unknown backend must be a boot error, not a silent no-op")
	}
}

// The exporter path is exercised for real, against nothing listening: the
// OTLP exporter connects lazily, so Setup must succeed — and this is where a
// resource or sampler misconfiguration surfaces before a deploy does.
func TestOTLPSetupBuildsAProvider(t *testing.T) {
	shutdown, err := tracing.Setup(context.Background(), tracing.Config{
		Backend: "otlp", OTLPEndpoint: "127.0.0.1:1", SampleRatio: 0.5,
		Service: "dop-core", Mode: "test", Version: "test",
	})
	if err != nil {
		t.Fatalf("otlp setup: %v", err)
	}
	ctx, span := tracing.Tracer().Start(context.Background(), "probe")
	if id, _ := tracing.IDs(ctx); id == "" {
		t.Error("with a provider installed, a span must have ids")
	}
	span.End()
	flush, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(flush) // the export fails (nothing listens); shutdown must still return
}
