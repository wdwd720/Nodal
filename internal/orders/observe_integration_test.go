//go:build integration

package orders

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/security"
)

// recordingObserver keeps every timing the pipeline hands it.
type recordingObserver struct {
	mu   sync.Mutex
	seen []CommandTiming
}

func (r *recordingObserver) ObserveCommand(_ context.Context, t CommandTiming) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, t)
}

func (r *recordingObserver) snapshot() []CommandTiming {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CommandTiming(nil), r.seen...)
}

// TestIntegration_ThePipelineTimesEachSubmitAndCancelOnce: a pipeline built
// with an observer tells it about each Submit and each Cancel exactly once,
// after the command returned, with the command's kind, what it returned -- a
// refusal a stage recorded included -- and a duration that is never
// negative; a replace is not timed, and a pipeline built without an observer
// times nothing.
func TestIntegration_ThePipelineTimesEachSubmitAndCancelOnce(t *testing.T) {
	f := newFixture(t)
	obs := &recordingObserver{}
	pipe, err := NewPipeline(f.svc, testDB, WithObserver(obs))
	require.NoError(t, err)
	m := f.spotMarket(marketOpts{})
	seller, broke := f.account(), f.account()
	f.fund(seller, m.base.ID, 5_000_000)

	rest, err := pipe.Submit(f.ctx, f.limit(m, seller, SideSell, "101.00", units(1)))
	require.NoError(t, err)
	require.Equal(t, StatusOpen, rest.Order.Status)
	refused, refusal := pipe.Submit(f.ctx, f.limit(m, broke, SideBuy, "100.00", units(1)))
	require.Error(t, refusal, "the account cannot pay for it")
	require.Equal(t, StatusRejected, refused.Order.Status, "a refusal a stage recorded")
	require.Len(t, obs.snapshot(), 2)

	_, err = pipe.Replace(f.ctx, ReplaceRequest{
		OrderID: rest.Order.ID, AccountID: seller, NewPrice: *m.price(t, "102.00"),
		NewQuantity: units(1), IdempotencyKey: "itest-" + uuid.NewString(), ActorType: security.ActorUser, ActorID: seller.String(),
	})
	require.NoError(t, err)
	require.Len(t, obs.snapshot(), 2, "a replace is not timed")
	cancelled, err := pipe.Cancel(f.ctx, CancelRequest{
		OrderID: rest.Order.ID, AccountID: seller, ActorType: security.ActorUser,
		ActorID: seller.String(),
	})
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, cancelled.Order.Status)

	seen := obs.snapshot()
	require.Len(t, seen, 3)
	assert.Equal(t, CommandNewOrder, seen[0].Kind)
	require.NoError(t, seen[0].Err)
	assert.Equal(t, CommandNewOrder, seen[1].Kind)
	assert.Equal(t, refusal, seen[1].Err, "the observer sees what the caller got")
	assert.Equal(t, CommandCancel, seen[2].Kind)
	require.NoError(t, seen[2].Err)
	for _, s := range seen {
		assert.GreaterOrEqual(t, s.Elapsed, time.Duration(0))
	}

	plain, err := NewPipeline(f.svc, testDB, WithObserver(nil))
	require.NoError(t, err)
	_, err = plain.Submit(f.ctx, f.limit(m, seller, SideSell, "103.00", units(1)))
	require.NoError(t, err)
	require.Len(t, obs.snapshot(), 3, "another pipeline's commands are not this observer's")
}
