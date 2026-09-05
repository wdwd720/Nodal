package observability

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/nodal/controlplane/internal/config"
)

// Setup errors.
var (
	ErrInsecureOTLPInProd = errors.New("observability: insecure OTLP export is not allowed in PROD")
	ErrServiceNameEmpty   = errors.New("observability: service name is required")
	ErrBadSampleRatio     = errors.New("observability: trace sample ratio must be a decimal in [0,1]")
)

// Resource attribute keys.
const (
	resServiceName        = "service.name"
	resServiceVersion     = "service.version"
	resDeploymentEnv      = "deployment.environment"
	resDeploymentEnvName  = "deployment.environment.name"
	unspecifiedVersionStr = "unspecified"
)

// Setup installs the global OpenTelemetry providers and W3C propagation.
//
// With an OTLP endpoint it builds OTLP/gRPC trace and metric exporters (TLS
// unless cfg.OTLPInsecure, which is refused in PROD), a parent-based
// TraceIDRatio sampler from cfg.TraceSampleRatio, a batching span processor
// and a periodic metric reader at cfg.MetricsInterval (SDK default when
// zero). Without an endpoint it installs no-op providers. Either way the
// returned shutdown flushes and releases everything and must be called on
// exit.
func Setup(ctx context.Context, cfg config.TelemetryConfig, service, version string, env config.Environment) (func(context.Context) error, error) {
	if strings.TrimSpace(service) == "" {
		return nil, ErrServiceNameEmpty
	}
	if version == "" {
		version = unspecifiedVersionStr
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if cfg.OTLPEndpoint == "" {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		return func(context.Context) error { return nil }, nil
	}
	if cfg.OTLPInsecure && env == config.EnvProd {
		return nil, ErrInsecureOTLPInProd
	}
	ratio, err := parseSampleRatio(cfg.TraceSampleRatio)
	if err != nil {
		return nil, err
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String(resServiceName, service),
		attribute.String(resServiceVersion, version),
		attribute.String(resDeploymentEnv, string(env)),
		attribute.String(resDeploymentEnvName, string(env)),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: resource: %w", err)
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.OTLPInsecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	traceExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("observability: trace exporter: %w", err)
	}
	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = traceExp.Shutdown(ctx)
		return nil, fmt.Errorf("observability: metric exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithBatcher(traceExp),
	)
	var readerOpts []sdkmetric.PeriodicReaderOption
	if cfg.MetricsInterval > 0 {
		readerOpts = append(readerOpts, sdkmetric.WithInterval(cfg.MetricsInterval))
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, readerOpts...)),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return shutdown, nil
}

// parseSampleRatio is the single place a sampling probability becomes a
// float64. The value is a telemetry parameter, never a financial quantity.
// Empty means sample everything.
func parseSampleRatio(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 1 || f != f {
		return 0, fmt.Errorf("%w: %q", ErrBadSampleRatio, s)
	}
	return f, nil
}
