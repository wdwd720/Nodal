//go:build integration

package derivatives

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// recordingVenue is an execution adapter that answers with the venue it
// wraps -- or, when skew is set, a price that many ticks beyond it -- and
// records every request it was asked.
type recordingVenue struct {
	inner DerivativesExecutionAdapter
	skew  int64

	mu   sync.Mutex
	seen []ExecutionRequest
}

func (v *recordingVenue) Name() string              { return v.inner.Name() }
func (v *recordingVenue) Mode() config.ProviderMode { return v.inner.Mode() }

func (v *recordingVenue) Execute(ctx context.Context, req ExecutionRequest) (Execution, error) {
	v.mu.Lock()
	v.seen = append(v.seen, req)
	v.mu.Unlock()
	ex, err := v.inner.Execute(ctx, req)
	if err != nil || v.skew == 0 {
		return ex, err
	}
	ex.Price = ex.Price.Add(req.Spec.Tick.Mul(money.QuantityFromInt64(v.skew)))
	return ex, nil
}

func (v *recordingVenue) requests() []ExecutionRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]ExecutionRequest(nil), v.seen...)
}

// TestIntegration_AnOrderFillsExactlyWhereTheExecutionAdapterSays: a service
// wired through NewExecutionAdapter asks its adapter once per order -- with
// the contract's terms, the customer's side and size, the mark and the
// policy's disclosed slippage -- and the fill records exactly the adapter's
// answer: its price, its slippage and its source.
func TestIntegration_AnOrderFillsExactlyWhereTheExecutionAdapterSays(t *testing.T) {
	paper, err := NewExecutionAdapter(config.EnvTest, ExecutionModeFor(config.EnvTest), PaperVenueConfig{Name: "paper-itest"})
	require.NoError(t, err)
	venue := &recordingVenue{inner: paper}
	f := newFixtureWith(t, func(c *Config) { c.Execution = venue })
	fut := f.future(start.Add(30 * 24 * time.Hour))
	a := f.account()
	f.fund(a, usdUnits(100_000))
	m := f.mark(fut, 65_000, 65_000)

	open := f.open(a, fut, Long, 10)
	require.NotNil(t, open.Fill)
	asked := venue.requests()
	require.Len(t, asked, 1, "one question per order")
	assert.Equal(t, Buy, asked[0].Side)
	assert.Equal(t, "10", asked[0].Quantity.String())
	assert.Equal(t, m.ID, asked[0].Mark.ID)
	assert.Equal(t, f.svc.Policy().PaperSlippageBPS, asked[0].MaxSlippageBPS)
	answer, err := paper.Execute(f.ctx, asked[0])
	require.NoError(t, err)
	assert.True(t, answer.Price.Equal(open.Fill.PriceMantissa), "the fill is the adapter's price")
	assert.Equal(t, answer.SlippageBPS, open.Fill.SlippageBPS)
	assert.Equal(t, SourcePaper, open.Fill.Source)

	closed := f.closeAll(a, fut)
	require.NotNil(t, closed.Fill)
	asked = venue.requests()
	require.Len(t, asked, 2)
	assert.Equal(t, Sell, asked[1].Side, "a long closes with a sell")
	answer, err = paper.Execute(f.ctx, asked[1])
	require.NoError(t, err)
	assert.True(t, answer.Price.Equal(closed.Fill.PriceMantissa))
}

// TestIntegration_AnExecutionThatBreaksTheContractIsNeverPriced: an adapter
// that answers beyond the disclosed slippage is refused before the risk
// kernel, a reservation or a fill sees its price; the order's transaction
// rolls back and leaves nothing.
func TestIntegration_AnExecutionThatBreaksTheContractIsNeverPriced(t *testing.T) {
	paper, err := NewExecutionAdapter(config.EnvTest, config.ProviderModeSandbox, PaperVenueConfig{Name: "paper-skewed"})
	require.NoError(t, err)
	f := newFixtureWith(t, func(c *Config) { c.Execution = &recordingVenue{inner: paper, skew: 1} })
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	fillsBefore := f.count(`SELECT count(*) FROM derivative_fills WHERE instrument_id = $1`, perp)
	intentsBefore := f.count(`SELECT count(*) FROM economic_intents WHERE instrument_id = $1`, perp.String())

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.OpenPosition(ctx, tx, OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "broke the execution contract")
	assert.Equal(t, fillsBefore, f.count(`SELECT count(*) FROM derivative_fills WHERE instrument_id = $1`, perp))
	assert.Equal(t, intentsBefore, f.count(`SELECT count(*) FROM economic_intents WHERE instrument_id = $1`, perp.String()))
}
