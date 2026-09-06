package observability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/nodal/controlplane/internal/config"
)

// The Setup tests mutate the OpenTelemetry globals and therefore never run in
// parallel; each restores no-op providers before returning.
func resetGlobals(t *testing.T) {
	t.Helper()
	shutdown, err := Setup(context.Background(), config.TelemetryConfig{}, "reset", "", config.EnvTest)
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
}

func TestSetup_NoOpWithoutEndpoint(t *testing.T) {
	t.Cleanup(func() { resetGlobals(t) })
	shutdown, err := Setup(context.Background(), config.TelemetryConfig{}, "svc", "1.2.3", config.EnvProd)
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	assert.False(t, span.SpanContext().IsValid(), "no-op tracer produces no real spans")
	span.End()
	assert.False(t, span.IsRecording())

	c, err := otel.Meter("test").Int64Counter("x")
	require.NoError(t, err)
	c.Add(context.Background(), 1) // must not panic

	assert.Contains(t, otel.GetTextMapPropagator().Fields(), "traceparent", "W3C propagation is installed even in no-op mode")
	assert.Contains(t, otel.GetTextMapPropagator().Fields(), "baggage")
	assert.NoError(t, shutdown(context.Background()))
	assert.NoError(t, shutdown(context.Background()), "idempotent")
}

func TestSetup_FailsClosed(t *testing.T) {
	t.Cleanup(func() { resetGlobals(t) })
	ctx := context.Background()

	_, err := Setup(ctx, config.TelemetryConfig{}, "  ", "v", config.EnvTest)
	assert.ErrorIs(t, err, ErrServiceNameEmpty)

	_, err = Setup(ctx, config.TelemetryConfig{OTLPEndpoint: "127.0.0.1:1", OTLPInsecure: true, TraceSampleRatio: "1"}, "svc", "v", config.EnvProd)
	assert.ErrorIs(t, err, ErrInsecureOTLPInProd)

	for _, bad := range []string{"abc", "2", "-0.5", "NaN", "Inf"} {
		_, err = Setup(ctx, config.TelemetryConfig{OTLPEndpoint: "127.0.0.1:1", OTLPInsecure: true, TraceSampleRatio: bad}, "svc", "v", config.EnvTest)
		assert.ErrorIs(t, err, ErrBadSampleRatio, bad)
	}
}

func TestSetup_ConfiguredProvidersAndShutdown(t *testing.T) {
	t.Cleanup(func() { resetGlobals(t) })
	cfg := config.TelemetryConfig{
		OTLPEndpoint:     "127.0.0.1:1", // nothing listens; export failures must not block shutdown
		OTLPInsecure:     true,
		TraceSampleRatio: "1",
		MetricsInterval:  time.Hour,
	}
	shutdown, err := Setup(context.Background(), cfg, "svc", "v9", config.EnvStaging)
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	ctx, span := otel.Tracer("test").Start(context.Background(), "op")
	assert.True(t, span.SpanContext().IsValid(), "SDK tracer installed")
	assert.True(t, span.SpanContext().IsSampled(), "ratio 1 samples everything")
	assert.Equal(t, span.SpanContext().TraceID().String(), attrMap(ContextAttrs(ctx))[AttrTraceID])
	span.End()

	c, err := otel.Meter("test").Int64Counter("x")
	require.NoError(t, err)
	c.Add(ctx, 1)

	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_ = shutdown(sctx) // an unreachable collector may surface an export error; it must return promptly
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestSetup_TLSByDefaultOutsideInsecure(t *testing.T) {
	t.Cleanup(func() { resetGlobals(t) })
	// OTLPInsecure=false in PROD is the only accepted PROD shape; the exporter
	// is created lazily so no connection is attempted here.
	shutdown, err := Setup(context.Background(), config.TelemetryConfig{OTLPEndpoint: "127.0.0.1:1", TraceSampleRatio: "0.5"}, "svc", "v", config.EnvProd)
	require.NoError(t, err)
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = shutdown(sctx)
}

func TestParseSampleRatio(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		want float64
		ok   bool
	}{
		"":      {1, true},
		"1":     {1, true},
		"0":     {0, true},
		"0.25":  {0.25, true},
		" 0.5 ": {0.5, true}, //nolint:gocritic // mapKey: intentional whitespace; parseSampleRatio trims its input
		"1.5":   {0, false},
		"-1":    {0, false},
		"NaN":   {0, false},
		"abc":   {0, false},
	}
	for in, tc := range cases {
		got, err := parseSampleRatio(in)
		if tc.ok {
			require.NoError(t, err, in)
			assert.Equal(t, tc.want, got, in)
		} else {
			assert.ErrorIs(t, err, ErrBadSampleRatio, in)
		}
	}
}
