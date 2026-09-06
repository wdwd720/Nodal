package execution

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// goldenTransitions is an independent spelling of the PART 47 table. Any
// change to OrderTransitions must be a visible diff here too.
var goldenTransitions = map[OrderStatus]map[OrderStatus]bool{
	OrderCreated:         set(OrderValidated, OrderRejected, OrderExpired),
	OrderValidated:       set(OrderCapitalReserved, OrderRejected, OrderExpired),
	OrderCapitalReserved: set(OrderPlanned, OrderRejected, OrderExpired),
	OrderPlanned:         set(OrderSubmitting, OrderRejected, OrderExpired, OrderCancelRequested),
	OrderSubmitting:      set(OrderSubmitted, OrderSubmissionUnknown, OrderRejected, OrderExpired, OrderCancelRequested),
	OrderSubmitted: set(OrderAcknowledged, OrderPartiallyFilled, OrderFilled, OrderExpired, OrderFailedFinal,
		OrderSubmissionUnknown, OrderReconciliationRequired, OrderCancelRequested),
	OrderAcknowledged:    set(OrderPartiallyFilled, OrderFilled, OrderExpired, OrderFailedFinal, OrderReconciliationRequired, OrderCancelRequested),
	OrderPartiallyFilled: set(OrderFilled, OrderSettling, OrderExpired, OrderFailedFinal, OrderReconciliationRequired, OrderCancelRequested),
	OrderFilled:          set(OrderSettling, OrderReconciliationRequired),
	OrderSettling:        set(OrderSettled, OrderReconciliationRequired),
	OrderSettled:         set(),
	OrderRejected:        set(),
	OrderExpired:         set(),
	OrderCancelRequested: set(OrderCancelled, OrderPartiallyFilled, OrderFilled, OrderExpired, OrderSubmissionUnknown, OrderReconciliationRequired),
	OrderCancelled:       set(),
	OrderSubmissionUnknown: set(OrderSubmitting, OrderSubmitted, OrderAcknowledged, OrderPartiallyFilled, OrderFilled,
		OrderExpired, OrderFailedFinal, OrderReconciliationRequired),
	OrderReconciliationRequired: set(OrderSubmitted, OrderAcknowledged, OrderPartiallyFilled, OrderFilled, OrderSettling,
		OrderExpired, OrderFailedFinal, OrderCancelled),
	OrderFailedFinal: set(),
}

func set(s ...OrderStatus) map[OrderStatus]bool {
	m := map[OrderStatus]bool{}
	for _, x := range s {
		m[x] = true
	}
	return m
}

func TestOrderTransitions_Exhaustive(t *testing.T) {
	t.Parallel()
	all := AllOrderStatuses()
	require.Len(t, all, 18)
	require.Len(t, OrderTransitions, 18)
	require.Len(t, goldenTransitions, 18)
	for _, from := range all {
		for _, to := range all {
			want := goldenTransitions[from][to]
			require.Equal(t, want, CanTransition(from, to), "%s -> %s", from, to)
		}
		require.False(t, CanTransition(from, from), "self transition %s", from)
	}
	// No transition names an undeclared status.
	for from, tos := range OrderTransitions {
		require.True(t, from.Valid())
		for _, to := range tos {
			require.True(t, to.Valid(), "%s -> %s", from, to)
		}
	}
}

func TestOrderTransitions_TerminalsAndReachability(t *testing.T) {
	t.Parallel()
	terminals := map[OrderStatus]bool{OrderSettled: true, OrderRejected: true, OrderExpired: true, OrderCancelled: true, OrderFailedFinal: true}
	for _, s := range AllOrderStatuses() {
		require.Equal(t, terminals[s], s.Terminal(), "%s terminal", s)
		require.Equal(t, terminals[s], len(OrderTransitions[s]) == 0, "%s has outgoing transitions iff not terminal", s)
	}
	// Every status is reachable from CREATED.
	reach := map[OrderStatus]bool{OrderCreated: true}
	queue := []OrderStatus{OrderCreated}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range OrderTransitions[cur] {
			if !reach[next] {
				reach[next] = true
				queue = append(queue, next)
			}
		}
	}
	for _, s := range AllOrderStatuses() {
		require.True(t, reach[s], "%s unreachable from CREATED", s)
	}
	// Every non-terminal status can reach a terminal one.
	for _, s := range AllOrderStatuses() {
		if s.Terminal() {
			continue
		}
		seen := map[OrderStatus]bool{s: true}
		q := []OrderStatus{s}
		found := false
		for len(q) > 0 && !found {
			cur := q[0]
			q = q[1:]
			for _, next := range OrderTransitions[cur] {
				if next.Terminal() {
					found = true
					break
				}
				if !seen[next] {
					seen[next] = true
					q = append(q, next)
				}
			}
		}
		require.True(t, found, "%s cannot reach a terminal status", s)
	}
}

func TestOrderStatus_HappyPathIsLinear(t *testing.T) {
	t.Parallel()
	path := []OrderStatus{
		OrderCreated, OrderValidated, OrderCapitalReserved, OrderPlanned, OrderSubmitting, OrderSubmitted,
		OrderAcknowledged, OrderPartiallyFilled, OrderFilled, OrderSettling, OrderSettled,
	}
	for i := 1; i < len(path); i++ {
		require.True(t, CanTransition(path[i-1], path[i]), "%s -> %s", path[i-1], path[i])
	}
}

func TestOrderStatus_AcceptsFill(t *testing.T) {
	t.Parallel()
	for _, s := range AllOrderStatuses() {
		if s.AcceptsFill() {
			require.True(t, CanTransition(s, OrderFilled) || s == OrderPartiallyFilled, "%s accepts fills but cannot become FILLED", s)
			require.False(t, s.Terminal())
		}
	}
	require.True(t, OrderCancelRequested.AcceptsFill(), "PART 227: a fill while cancel is requested is accepted")
	require.False(t, OrderCancelled.AcceptsFill())
	require.False(t, OrderPlanned.AcceptsFill(), "nothing was submitted yet")
}

func TestNextStatusAfterFill(t *testing.T) {
	t.Parallel()
	q := money.QuantityFromInt64
	next, err := NextStatusAfterFill(OrderSubmitted, q(50), q(100))
	require.NoError(t, err)
	require.Equal(t, OrderPartiallyFilled, next)
	next, err = NextStatusAfterFill(OrderPartiallyFilled, q(100), q(100))
	require.NoError(t, err)
	require.Equal(t, OrderFilled, next)
	next, err = NextStatusAfterFill(OrderPartiallyFilled, q(60), q(100))
	require.NoError(t, err)
	require.Equal(t, OrderPartiallyFilled, next, "further partial fill keeps PARTIALLY_FILLED")
	next, err = NextStatusAfterFill(OrderCancelRequested, q(100), q(100))
	require.NoError(t, err)
	require.Equal(t, OrderFilled, next, "fill wins over a cancel request")
	_, err = NextStatusAfterFill(OrderSubmitted, q(101), q(100))
	require.True(t, errs.HasCode(err, errs.CodeReconciliationRequired), "overfill is a reconciliation matter")
	_, err = NextStatusAfterFill(OrderCancelled, q(100), q(100))
	require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition))
	_, err = NextStatusAfterFill(OrderSettled, q(1), q(100))
	require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition))
}

func TestCancelSemantics_Pure(t *testing.T) {
	t.Parallel()
	// A cancel request is a state, not a cancellation.
	require.True(t, CanTransition(OrderSubmitted, OrderCancelRequested))
	require.False(t, CanTransition(OrderSubmitted, OrderCancelled), "CANCELLED only from CANCEL_REQUESTED (external confirmation)")
	require.True(t, CanTransition(OrderCancelRequested, OrderCancelled))
	// Once a fill has moved the order on, the cancel can no longer be confirmed.
	require.True(t, CanTransition(OrderCancelRequested, OrderFilled))
	require.False(t, CanTransition(OrderFilled, OrderCancelled))
	require.False(t, CanTransition(OrderPartiallyFilled, OrderCancelled))
}

func TestOrderValidate(t *testing.T) {
	t.Parallel()
	o := Order{}
	err := o.Validate()
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed))
	e, _ := errs.As(err)
	require.Contains(t, e.Fields, "id")
	require.Contains(t, e.Fields, "side")
	require.Contains(t, e.Fields, "input_quantity")
}
