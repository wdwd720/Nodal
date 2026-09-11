//go:build integration

package payout_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// A payout takes exactly the units its decision evaluated (D-136, F-270).
//
// The two auditor reproductions drive one refused dimension each: a REVERSIBLE
// lot of the approved origin, and a promotional-floored lot of the approved
// origin. This is the mixed case, which is the one a real account is in --
// several lots of several kinds, one of them approved -- and it is the case the
// origin filter could never get right, because every lot in it is of an origin
// the decision approved.
//
// Three lots, in consumption order (ConsumptionRank puts PURCHASED before
// MARKET_TRADING_PROCEEDS, and created_at breaks the tie inside an origin):
//
//  1. a REVERSIBLE purchase -- an origin SandboxPolicy releases, at a finality
//     PayoutEligible() refuses. Minted FIRST, so it sorts first among the
//     purchases.
//  2. a SETTLED purchase -- the only lot of the three the policy releases.
//  3. proceeds whose provenance bottoms out in a promotional grant -- an origin
//     SandboxPolicy releases, with a floor and a root set it does not.
//
// The decision approves exactly one of them. Exactly that one must be drawn,
// and the allocation must record what it was.
func TestIntegration_AMixedBalanceDrawsOnlyTheLotTheDecisionApproved(t *testing.T) {
	f := newAuditFixture(t)

	reversible := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 300_000_000)
	settled := f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 300_000_000)
	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 300_000_000)
	laundered := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: grant.ID, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalityUnfunded,
		}})

	require.Equal(t, valuedomain.OriginPurchased, floorOf(t, settled.ID))
	require.Equal(t, valuedomain.OriginPromotional, floorOf(t, laundered.ID),
		"fixture check: a grant round-tripped through a market floors at the grant")

	eligible, total, err := f.credits.EligibleLots(f.ctx, testDB, credit.BalanceRequest{
		AccountID:  f.account,
		Policy:     valuedomain.SandboxPolicy(),
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:        f.clk.Now(),
	})
	require.NoError(t, err)
	require.Len(t, eligible, 1, "fixture check: exactly one of the four lots is payout-eligible")
	require.Equal(t, settled.ID, eligible[0].ID, "fixture check: it is the SETTLED purchase")
	require.Equal(t, "300000000", total.String())

	req := f.createPayout(t, 300_000_000, "reserve-by-lot-mixed")
	require.Equal(t, payout.StateVerified, req.State)
	require.Equal(t, "300000000", req.ReservedQuantity.String())

	got := allocatedLots(t, req.ID)
	assert.Len(t, got, 1, "one approved lot, one allocation")
	assert.Equal(t, map[credit.LotID]string{settled.ID: "300000000"}, got,
		"D-136: the payout drew a lot the decision did not evaluate. Every lot here is of an "+
			"origin the decision approved, so an origin filter cannot tell them apart: one is "+
			"inside its dispute window and one is a promotional grant that has been traded")
	assert.NotContains(t, got, reversible.ID)
	assert.NotContains(t, got, laundered.ID)
	assert.NotContains(t, got, grant.ID)

	// And the record says what left, as what it was: origin AND floor.
	var origin, floor string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT origin, origin_floor FROM payout_allocations WHERE request_id = $1`, req.ID).
		Scan(&origin, &floor))
	assert.Equal(t, string(valuedomain.OriginPurchased), origin)
	assert.Equal(t, string(valuedomain.OriginPurchased), floor,
		"D-136: the allocation records the origin and not what funded it, so a settled payout "+
			"cannot be told from one drawn on value a grant funded")

	// The provenance read model reports one slice, with both.
	slices, perr := f.svc.Provenance(f.ctx, testDB, req.ID)
	require.NoError(t, perr)
	require.Len(t, slices, 1)
	assert.Equal(t, valuedomain.OriginPurchased, slices[0].Origin)
	assert.Equal(t, valuedomain.OriginPurchased, slices[0].OriginFloor)
	assert.Equal(t, "300000000", slices[0].Quantity.String())
}

// Two lots of ONE origin and ONE finality, differing only in what funded them,
// fold into TWO provenance slices rather than one.
//
// It is the reporting half of D-136. A payout drawn on trading proceeds out of
// a settled purchase and one drawn on trading proceeds out of a promotional
// grant would both have read "MARKET_TRADING_PROCEEDS: 600" -- the same
// collapse that let the reservation take the wrong lot, one surface along.
func TestIntegration_ProvenanceReportsTwoFloorsOfOneOriginSeparately(t *testing.T) {
	f := newAuditFixture(t)

	purchase := f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 300_000_000)
	earning := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 300_000_000)
	fromPurchase := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: purchase.ID, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalitySettled,
		}})
	fromEarning := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: earning.ID, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalitySettled,
		}})
	require.Equal(t, valuedomain.OriginPurchased, floorOf(t, fromPurchase.ID))
	require.Equal(t, valuedomain.OriginCreatorEarning, floorOf(t, fromEarning.ID))

	// 1200 Credits: everything, so both proceeds lots are drawn.
	req := f.createPayout(t, 1_200_000_000, "provenance-two-floors")
	require.Equal(t, payout.StateVerified, req.State)

	slices, err := f.svc.Provenance(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	proceeds := map[valuedomain.CreditOrigin]string{}
	for _, s := range slices {
		if s.Origin != valuedomain.OriginMarketTradingProceeds {
			continue
		}
		_, seen := proceeds[s.OriginFloor]
		require.False(t, seen, "two slices of one (origin, floor) were reported separately")
		proceeds[s.OriginFloor] = s.Quantity.String()
	}
	assert.Equal(t, map[valuedomain.CreditOrigin]string{
		valuedomain.OriginPurchased:      "300000000",
		valuedomain.OriginCreatorEarning: "300000000",
	}, proceeds,
		"D-136: the two provenances were summed into one line of MARKET_TRADING_PROCEEDS, so a "+
			"reader cannot tell what actually left")
}
