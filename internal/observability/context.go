package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota + 1
	keyCorrelationID
	keyIntentID
	keyOrderID
	keyWorkflowID
	keyLogger
)

// Attribute keys used for the identifiers carried in context. They are the
// same in logs and on spans (goal PART 131).
const (
	AttrRequestID     = "request_id"
	AttrCorrelationID = "correlation_id"
	AttrIntentID      = "intent_id"
	AttrOrderID       = "order_id"
	AttrWorkflowID    = "workflow_id"
	AttrTraceID       = "trace_id"
	AttrSpanID        = "span_id"
)

// WithRequestID returns ctx carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID returns the request id in ctx, or "".
func RequestID(ctx context.Context) string { return stringValue(ctx, keyRequestID) }

// WithCorrelationID returns ctx carrying the correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyCorrelationID, id)
}

// CorrelationID returns the correlation id in ctx, or "".
func CorrelationID(ctx context.Context) string { return stringValue(ctx, keyCorrelationID) }

// WithIntentID returns ctx carrying the intent id.
func WithIntentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyIntentID, id)
}

// IntentID returns the intent id in ctx, or "".
func IntentID(ctx context.Context) string { return stringValue(ctx, keyIntentID) }

// WithOrderID returns ctx carrying the order id.
func WithOrderID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyOrderID, id)
}

// OrderID returns the order id in ctx, or "".
func OrderID(ctx context.Context) string { return stringValue(ctx, keyOrderID) }

// WithWorkflowID returns ctx carrying the workflow id.
func WithWorkflowID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyWorkflowID, id)
}

// WorkflowID returns the workflow id in ctx, or "".
func WorkflowID(ctx context.Context) string { return stringValue(ctx, keyWorkflowID) }

func stringValue(ctx context.Context, k ctxKey) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(k).(string)
	return v
}

// ContextAttrs returns the identifiers present in ctx as slog attributes:
// request_id, correlation_id, intent_id, order_id, workflow_id, and
// trace_id/span_id when ctx carries a valid OpenTelemetry span. Absent
// values are omitted.
func ContextAttrs(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	var attrs []slog.Attr
	add := func(key string, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(key, v))
		}
	}
	add(AttrRequestID, RequestID(ctx))
	add(AttrCorrelationID, CorrelationID(ctx))
	add(AttrIntentID, IntentID(ctx))
	add(AttrOrderID, OrderID(ctx))
	add(AttrWorkflowID, WorkflowID(ctx))
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		add(AttrTraceID, sc.TraceID().String())
		add(AttrSpanID, sc.SpanID().String())
	}
	return attrs
}

// WithLogger returns ctx carrying l for LoggerFrom.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, l)
}

// LoggerFrom returns the logger stored by WithLogger. When none is stored it
// falls back to slog.Default wrapped in the redaction and context handlers,
// so a forgotten WithLogger can never produce an unredacted line. The
// returned logger is never nil.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(keyLogger).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.New(NewContextHandler(NewRedactHandler(slog.Default().Handler())))
}
