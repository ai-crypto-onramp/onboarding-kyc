package internal

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// tracerState holds the currently-installed OTel tracer provider and a
// shutdown function. It is safe to call installTracer multiple times; the
// first call wins and subsequent calls are no-ops (returns the existing
// shutdown).
var (
	tracerMu     sync.Mutex
	tracerShutdown func(context.Context) error
)

// installTracer configures a global OTel tracer provider. If
// OTEL_EXPORTER_OTLP_ENDPOINT is set, an OTLP HTTP exporter is used; otherwise
// a no-op provider is installed (so spans are recorded but never exported).
// The returned shutdown function flushes and stops the provider and MUST be
// called on process exit.
func installTracer(ctx context.Context, logger *slog.Logger) (shutdown func(context.Context) error, err error) {
	tracerMu.Lock()
	defer tracerMu.Unlock()
	if tracerShutdown != nil {
		return tracerShutdown, nil
	}
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	var tp *sdktrace.TracerProvider
	if endpoint == "" {
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithResource(newResource()),
		)
		logger.Info("otel tracer: no exporter configured, using no-op provider")
	} else {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
		if os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") != "" {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, expErr := otlptracehttp.New(ctx, opts...)
		if expErr != nil {
			return nil, fmt.Errorf("otlp exporter: %w", expErr)
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(newResource()),
		)
		logger.Info("otel tracer: OTLP exporter configured", "endpoint", endpoint)
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	tracerShutdown = tp.Shutdown
	return tp.Shutdown, nil
}

func newResource() *resource.Resource {
	r, _ := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("onboarding-kyc"),
		),
	)
	return r
}

// tracer returns the global tracer named for this service.
func tracer() trace.Tracer {
	return otel.Tracer("onboarding-kyc")
}

// spanMiddleware wraps each HTTP request in a server span, propagating the
// trace context from incoming headers. It records the HTTP method, route, and
// status code as span attributes.
func spanMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		prop := otel.GetTextMapPropagator()
		ctx = prop.Extract(ctx, propagation.HeaderCarrier(r.Header))
		spanName := r.Method + " " + r.URL.Path
		ctx, span := tracer().Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))
		span.SetAttributes(semconv.HTTPResponseStatusCode(rw.status))
		if rw.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rw.status))
		}
	})
}

// startSpan is a small helper for creating child spans in service code.
func startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return tracer().Start(ctx, name, opts...)
}

// spanFromCtx returns the active span from the context if any, else nil.
func spanFromCtx(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// recordSpanError records an error on the active span (if any) and sets its
// status to Error. Safe to call with a nil error.
func recordSpanError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if span := spanFromCtx(ctx); span != nil && span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// shutdownTracer flushes and stops the global tracer provider, if installed.
// Safe to call when no provider is configured.
func shutdownTracer(ctx context.Context) error {
	tracerMu.Lock()
	sd := tracerShutdown
	tracerShutdown = nil
	tracerMu.Unlock()
	if sd == nil {
		return nil
	}
	return sd(ctx)
}