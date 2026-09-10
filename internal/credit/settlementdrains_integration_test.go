//go:build integration

package credit

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capacity"
)

// The money-at-risk ceiling drains, and settlement is what drains it (F-90).
//
// internal/capacity counts CAPTURED and REVERSIBLE as money at risk and
// deliberately excludes SETTLED, on the stated grounds that counting SETTLED
// "would turn the ceiling into a lifetime cumulative cap that can only ever
// rise, so the tier would end up refusing every purchase forever -- an outage,
// not a ceiling."
//
// That reasoning is only true while something settles. The only exits from
// REVERSIBLE are SettleDue and a won dispute, and SettleDue's only caller was
// cmd/reconciliation-worker -- which the launch tier does not deploy. So the
// exclusion bought nothing: the sum was monotonically non-decreasing and the
// deployment would have refused every purchase with AT_CAPACITY, permanently,
// at $2,000 of lifetime sales.
//
// This is the property the fix has to have, stated as arithmetic rather than
// as wiring: a payment past its reversibility window stops counting against
// the ceiling once it is settled, and one inside the window still counts.
func TestIntegration_SettlementDrainsTheMoneyAtRiskCeiling(t *testing.T) {
	f := newPurchaseFixture(t)
	guard, err := capacity.NewGuard(capacity.Budget{MaxAtRiskMinor: 1_000_000}, f.clk.Now)
	require.NoError(t, err)

	atRisk := func() int64 {
		t.Helper()
		var r capacity.Reading
		require.NoError(t, f.tx(func(tx pgx.Tx) error {
			var merr error
			r, merr = guard.Measure(f.ctx, tx)
			return merr
		}))
		return r.AtRiskMinor
	}

	before := atRisk()
	p := f.start(t, "drains", 10000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 10000))

	funded := atRisk()
	require.Equal(t, before+10000, funded,
		"a captured, reversible payment must count against the ceiling")

	// Still inside the window: settlement finds nothing and the ceiling does
	// not move. Without this half, the assertion below could be satisfied by a
	// sweep that settles everything the moment it is captured.
	var n int
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var serr error
		n, serr = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 10)
		return serr
	}))
	require.Zero(t, n)
	assert.Equal(t, funded, atRisk(), "money inside its reversibility window still counts")

	// Past the window. reversible_at is backdated rather than the clock
	// advanced, because SettleDue compares the database's column against the
	// database's now().
	_, err = testDB.Exec(f.ctx,
		`UPDATE credit_fundings SET reversible_at = now() - interval '31 days' WHERE id = $1`,
		p.Funding.ID)
	require.NoError(t, err)

	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var serr error
		n, serr = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 10)
		return serr
	}))
	require.Equal(t, 1, n)

	assert.Equal(t, before, atRisk(),
		"settled money still counted against the ceiling, so the ceiling can only ever rise")
}
