//go:build integration

package nativemarket

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The pool-record shortfall branch keeps the parents it found (F-276).
//
// `moveCredits`' Sell arm draws the pooled contributions a sale is paid out of
// and mints the seller's proceeds against them. When the record could not
// account for the whole draw-down it set `drawn = nil` -- one line under a
// comment saying the found parents still constrain the mint downwards. Dropping
// them is what made that comment false:
//
//   - a parentless derived lot is minted at the REVERSIBLE fallback, and
//     `settleDerivedCandidates` requires a parent row, so no direction of
//     SettleDerived can ever see it again. It is stranded REVERSIBLE for ever,
//     which is F-230's outcome restored on the one path F-230's fix did not
//     cover; and
//   - its origin floor is opened at its own origin, MARKET_TRADING_PROCEEDS,
//     which SandboxPolicy releases -- so the provenance of the value that DID
//     fund it is discarded in the permissive direction.
//
// # This is legacy data only on a fresh deployment
//
// `recordPoolSources` writes a row for every Credit that reaches a pool, in the
// same transaction as the buy that sent it, and `drawPoolSources` draws only
// against those rows. On a database migrated from 00809 onwards the record and
// the reserve move together and `covered` always equals what was asked for. The
// branch exists for a pool that traded before the record did -- and the finding
// is about what it does when it is reached, not about how often.
//
// The fixture reproduces that state the only way it can be reached: by reducing
// the pool's recorded contributions below the reserve they describe. `cp_app`
// holds `UPDATE (remaining)` on the table, which is the grant the draw-down
// itself uses, so this is a state the application role can produce.
func TestIntegration_APoolShortfallKeepsTheParentsItFound(t *testing.T) {
	f := newFixture(t)

	// A trader holding nothing but a promotional grant: the provenance the
	// branch must not discard, and the one goal §23 is about.
	trader := newAccount(t)
	grant := f.fundAs(trader, valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 40_000_000_000)

	buy, err := f.buy(trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	// The pool's record, made short of the reserve it describes. Halved rather
	// than emptied: the branch is about what happens when SOME of the draw-down
	// is accounted for, and an empty record takes the ordinary no-parents path.
	tag, err := testDB.Exec(f.ctx,
		`UPDATE native_market_credit_sources
		    SET remaining = remaining / 2
		  WHERE market_id = $1 AND remaining > 0`, f.market.ID)
	require.NoError(t, err)
	require.Positive(t, tag.RowsAffected(), "fixture check: the buy recorded what it paid into the pool")

	sell, err := f.sell(trader, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)
	require.True(t, sell.Fill.CreditsOut.IsPositive(), "fixture check: the sale returned Credits")

	lots, err := f.credits.Lots(f.ctx, testDB, trader)
	require.NoError(t, err)
	var proceeds *credit.Lot
	for i := range lots {
		if lots[i].Origin == valuedomain.OriginMarketTradingProceeds {
			proceeds = &lots[i]
			break
		}
	}
	require.NotNil(t, proceeds, "fixture check: the sale minted MARKET_TRADING_PROCEEDS")

	parents, err := f.credits.ParentsOf(f.ctx, testDB, proceeds.ID)
	require.NoError(t, err)
	require.NotEmpty(t, parents,
		"F-276: the shortfall branch discarded the parents it had found, so the proceeds name "+
			"nothing that funded them. SettleDerived's candidate query requires a parent row, so "+
			"this lot is outside every direction of the sweep for ever")
	assert.Equal(t, grant.ID, parents[0].LotID,
		"and the parent it names is the lot the pool's record does know about")

	assert.Equal(t, valuedomain.OriginPromotional, proceeds.OriginFloor,
		"F-276: proceeds funded in part by a promotional grant carry a MARKET_TRADING_PROCEEDS "+
			"floor, because the parents that established the provenance were thrown away")
	assert.Equal(t, []valuedomain.CreditOrigin{valuedomain.OriginPromotional}, proceeds.RootOrigins)
	assert.Equal(t, valuedomain.FinalityUnfunded, proceeds.Finality,
		"minted at the worst of the parents that were found, not at the REVERSIBLE fallback")

	ok, reasons := valuedomain.SandboxPolicy().Permits(valuedomain.PermitInput{
		Origin:      proceeds.Origin,
		OriginFloor: proceeds.OriginFloor,
		RootOrigins: proceeds.RootOrigins,
		Finality:    proceeds.Finality,
		Domain:      valuedomain.InternalCredit,
		Verified:    valuedomain.VerificationPayoutKYC,
		ActiveCaps:  map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		PolicyValid: true,
	})
	assert.False(t, ok, "a grant round-tripped through a market with an incomplete pool record is "+
		"still a grant; reasons=%v", reasons)

	// And the shortfall is recorded rather than inferred, beside the market it
	// happened in, so an operator reading a lot whose provenance looks thin can
	// find out why.
	var events int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_events
		  WHERE action = 'native_market.pool_provenance_shortfall' AND resource_id = $1`,
		f.market.ID.String()).Scan(&events))
	assert.Positive(t, events,
		"F-276: the sale drew more than the record could account for and said nothing")
}
