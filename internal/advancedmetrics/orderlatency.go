package advancedmetrics

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/orders"
)

// Outcomes of a timed order command, as the attribute outcome of
// advanced_order_latency and advanced_cancel_latency.
const (
	// OutcomeOK: the command was carried out.
	OutcomeOK = "OK"
	// OutcomeRefused: the command was refused -- a refusal the book recorded
	// or one of the request -- the class of error a client is answered with
	// a 4xx (errs.HTTPStatus).
	OutcomeRefused = "REFUSED"
	// OutcomeFailed: the command could not be carried out (an internal or
	// transient failure, a 5xx).
	OutcomeFailed = "FAILED"
)

// OrderLatency records advanced_order_latency and advanced_cancel_latency.
// It is the order book pipeline's observer (orders.WithObserver): the latency
// of a request belongs to the request, so it is recorded on the request's
// path, once per customer Submit or Cancel the pipeline runs, by the process
// that ran it, in milliseconds (the command's monotonic duration, rounded
// toward zero), with the command's outcome.
type OrderLatency struct {
	order, cancel metric.Int64Histogram
}

var _ orders.Observer = (*OrderLatency)(nil)

// NewOrderLatency records into the order and cancel latency instruments of m.
func NewOrderLatency(m *observability.AdvancedMetrics) (*OrderLatency, error) {
	if m == nil || m.OrderLatency == nil || m.CancelLatency == nil {
		return nil, errs.New(errs.CodeInternal, "advancedmetrics: order latency needs the advanced instruments")
	}
	return &OrderLatency{order: m.OrderLatency, cancel: m.CancelLatency}, nil
}

// ObserveCommand implements orders.Observer. A command the pipeline does not
// time (a replace) is not recorded.
func (l *OrderLatency) ObserveCommand(ctx context.Context, t orders.CommandTiming) {
	var h metric.Int64Histogram
	switch t.Kind {
	case orders.CommandNewOrder:
		h = l.order
	case orders.CommandCancel:
		h = l.cancel
	default:
		return
	}
	ms := t.Elapsed.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	h.Record(ctx, ms, observability.WithSafeAttrs(attribute.String("outcome", CommandOutcome(t.Err))))
}

// CommandOutcome classifies what a command returned.
func CommandOutcome(err error) string {
	switch {
	case err == nil:
		return OutcomeOK
	case errs.HTTPStatus(errs.CodeOf(err)) < http.StatusInternalServerError:
		return OutcomeRefused
	}
	return OutcomeFailed
}
