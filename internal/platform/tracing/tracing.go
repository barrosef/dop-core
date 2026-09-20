// Package tracing is the core's OpenTelemetry setup (ADR-0024 §4, phase 1).
//
// OpenTelemetry is the port; the vendor is the exporter. Three backends:
// none (the default — no export, no cost), OTLP over gRPC (Jaeger in the local
// environment, any collector elsewhere) and Cloud Trace on GCP. The rest of
// the code base never names a vendor: it asks for a tracer and reads a span
// from the context.
package tracing

import (
	"context"
	"fmt"
	"time"

	gcptrace "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config is what the composition root hands in; it is a subset of
// platform/config, restated so this package depends on nothing of ours.
type Config struct {
	// Backend: "" | "otlp" | "gcp".
	Backend string
	// OTLPEndpoint is host:port for the otlp backend (insecure gRPC — a
	// collector on the same network; TLS is the collector's job).
	OTLPEndpoint string
	// GCPProject for the gcp backend; empty resolves from the environment.
	GCPProject string
	// SampleRatio in [0,1]; 1 samples everything. Parent-based: a request
	// sampled upstream stays sampled here whatever the ratio.
	SampleRatio float64
	// Service and Mode name the resource: dop-core, and serve|worker|…
	Service string
	Mode    string
	Version string
}

const instrumentation = "github.com/barrosef/dop-core"

// Setup installs the global tracer provider and the W3C propagator. It
// returns the shutdown that flushes what is buffered; the caller defers it.
// With no backend it installs only the propagator, so the trace context still
// crosses the process even when nothing is exported.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	var exporter sdktrace.SpanExporter
	switch cfg.Backend {
	case "", "none":
		return func(context.Context) error { return nil }, nil
	case "otlp":
		if cfg.OTLPEndpoint == "" {
			return nil, fmt.Errorf("TRACE_BACKEND=otlp needs OTEL_EXPORTER_OTLP_ENDPOINT")
		}
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithTimeout(5*time.Second))
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		exporter = exp
	case "gcp":
		exp, err := gcptrace.New(gcptrace.WithProjectID(cfg.GCPProject))
		if err != nil {
			return nil, fmt.Errorf("cloud trace exporter: %w", err)
		}
		exporter = exp
	default:
		return nil, fmt.Errorf("unknown TRACE_BACKEND %q (use none, otlp or gcp)", cfg.Backend)
	}

	// Schemaless on purpose: resource.Default() carries the SDK's semconv
	// schema URL and Merge refuses two different ones. Pinning ours to the
	// SDK's version would break on every SDK upgrade; naming no schema never
	// does, and the attribute keys are the stable part anyway.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(cfg.Service),
		semconv.ServiceVersion(cfg.Version),
		attribute.String("dop.mode", cfg.Mode),
	))
	if err != nil {
		return nil, err
	}
	ratio := cfg.SampleRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Tracer is the one tracer the code base uses.
func Tracer() trace.Tracer { return otel.Tracer(instrumentation) }

// carrier is a one-key text map: the W3C traceparent, as it travels inside
// an event envelope or a NATS header.
type carrier map[string]string

func (c carrier) Get(k string) string { return c[k] }
func (c carrier) Set(k, v string)     { c[k] = v }
func (c carrier) Keys() []string      { return []string{"traceparent", "tracestate"} }

// TraceParent renders the current span as a W3C traceparent header value, or
// "" when there is no span. It is what the outbox writes into the envelope.
func TraceParent(ctx context.Context) string {
	c := carrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c["traceparent"]
}

// WithTraceParent continues a trace from a traceparent value: the context it
// returns has the remote span as parent. An empty or malformed value returns
// the context untouched.
func WithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier{"traceparent": traceparent})
}

// IDs returns the active span's trace and span ids for a log line, or empty
// strings when there is no span.
func IDs(ctx context.Context) (traceID, spanID string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}
