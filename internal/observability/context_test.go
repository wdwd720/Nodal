package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/nodal/controlplane/internal/config"
)

func TestContextIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assert.Equal(t, "", RequestID(ctx))
	assert.Equal(t, "", CorrelationID(ctx))
	assert.Equal(t, "", IntentID(ctx))
	assert.Equal(t, "", OrderID(ctx))
	assert.Equal(t, "", WorkflowID(ctx))

	ctx = WithRequestID(ctx, "req-1")
	ctx = WithCorrelationID(ctx, "corr-1")
	ctx = WithIntentID(ctx, "int-1")
	ctx = WithOrderID(ctx, "ord-1")
	ctx = WithWorkflowID(ctx, "wf-1")
	assert.Equal(t, "req-1", RequestID(ctx))
	assert.Equal(t, "corr-1", CorrelationID(ctx))
	assert.Equal(t, "int-1", IntentID(ctx))
	assert.Equal(t, "ord-1", OrderID(ctx))
	assert.Equal(t, "wf-1", WorkflowID(ctx))

	// Keys are distinct: overriding one leaves the others intact.
	ctx2 := WithRequestID(ctx, "req-2")
	assert.Equal(t, "req-2", RequestID(ctx2))
	assert.Equal(t, "corr-1", CorrelationID(ctx2))
	assert.Equal(t, "req-1", RequestID(ctx), "contexts are immutable")

	var nilCtx context.Context
	assert.Equal(t, "", RequestID(nilCtx))
	assert.Nil(t, ContextAttrs(nilCtx))
}

func spanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
}

func attrMap(attrs []slog.Attr) map[string]string {
	m := map[string]string{}
	for _, a := range attrs {
		m[a.Key] = a.Value.String()
	}
	return m
}

func TestContextAttrs(t *testing.T) {
	t.Parallel()
	assert.Empty(t, ContextAttrs(context.Background()), "nothing set, nothing emitted")

	ctx := WithWorkflowID(WithOrderID(WithIntentID(WithCorrelationID(WithRequestID(context.Background(), "r"), "c"), "i"), "o"), "w")
	ctx = trace.ContextWithSpanContext(ctx, spanContext())
	got := attrMap(ContextAttrs(ctx))
	assert.Equal(t, map[string]string{
		AttrRequestID:     "r",
		AttrCorrelationID: "c",
		AttrIntentID:      "i",
		AttrOrderID:       "o",
		AttrWorkflowID:    "w",
		AttrTraceID:       "0102030405060708090a0b0c0d0e0f10",
		AttrSpanID:        "0102030405060708",
	}, got)

	partial := attrMap(ContextAttrs(WithOrderID(context.Background(), "only")))
	assert.Equal(t, map[string]string{AttrOrderID: "only"}, partial)
}

func TestContextHandler_InjectsAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewLogger(config.EnvTest, &buf)
	ctx := trace.ContextWithSpanContext(WithIntentID(WithRequestID(context.Background(), "req-9"), "intent-9"), spanContext())
	l.InfoContext(ctx, "hello", "k", "v")
	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m), buf.String())
	assert.Equal(t, "req-9", m[AttrRequestID])
	assert.Equal(t, "intent-9", m[AttrIntentID])
	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", m[AttrTraceID])
	assert.Equal(t, "0102030405060708", m[AttrSpanID])
	assert.Equal(t, "v", m["k"])
	assert.NotContains(t, m, AttrOrderID)

	buf.Reset()
	l.Info("no context")
	m = nil // json.Unmarshal merges into an existing map
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.NotContains(t, m, AttrRequestID)
	assert.NotContains(t, m, AttrTraceID)

	// Derived loggers keep injecting.
	buf.Reset()
	l.With("static", 1).WithGroup("g").InfoContext(ctx, "derived", "x", "y")
	m = nil
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m), buf.String())
	assert.Equal(t, float64(1), m["static"])
	g := m["g"].(map[string]any)
	assert.Equal(t, "y", g["x"])
	assert.Equal(t, "req-9", g[AttrRequestID], "attrs land at the current group level")
}

func TestLoggerFrom(t *testing.T) {
	// Not parallel: exercises slog.Default as the fallback.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	stored := NewLogger(config.EnvTest, &buf)
	ctx := WithLogger(context.Background(), stored)
	assert.Same(t, stored, LoggerFrom(ctx))

	var nilLogger *slog.Logger
	assert.NotNil(t, LoggerFrom(WithLogger(context.Background(), nilLogger)))

	var def bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&def, nil)))
	fb := LoggerFrom(context.Background())
	require.NotNil(t, fb)
	fb.InfoContext(WithRequestID(context.Background(), "fallback-req"), "x", "password", sensitive)
	assert.Contains(t, def.String(), RedactedMarker, "the fallback redacts")
	assert.NotContains(t, def.String(), sensitive)
	assert.Contains(t, def.String(), "fallback-req", "the fallback injects context attrs")

	var nilCtx context.Context
	assert.NotNil(t, LoggerFrom(nilCtx))
}
