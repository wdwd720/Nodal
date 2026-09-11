package httpapi

// Reproductions for the THIRD round of the withdrawal-verification audit
// (goal §54). Unit half; the integration half is in internal/payout and
// internal/verification. Nothing here changes product code.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// ---------------------------------------------------------------------------
// F-wv3-3 — GET /v1/me/eligibility states two contradictory figures for the
// same money, and the one it makes the verdict from is the false one.
//
// `payout_eligible` comes from credit.Balances, which evaluates
// valuedomain.Policy.Permits PER LOT. `withdrawable_now` is the sum of the
// per-ORIGIN buckets, and foldHoldings collapses every lot of one origin into
// a single OriginHolding carrying the LEAST final finality and the MOST
// restricted origin floor in the bucket. One refused lot therefore zeroes every
// other lot of that origin -- including the ones payout.Engine.Evaluate would
// approve, and does approve: POST /v1/payouts reserves them.
//
// So the page reports `eligible: false`, `withdrawable_now: 0` and
// `payout_eligible: 200000000` in one payload, and the conversion request the
// page says is impossible succeeds.
//
// The conservative fold is deliberate and documented on eligibility's own type.
// What is not documented anywhere is that the two figures in one response, and
// the verdict derived from one of them, disagree with the commit path.
// ---------------------------------------------------------------------------

func TestAuditWV3_TheEligibilityPageAgreesWithItselfAboutWhatMayLeave(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	policy := valuedomain.SandboxPolicy()
	caps := map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true}

	qty := func(n int64) money.Quantity { return money.QuantityFromInt64(n) }

	// Two lots of ONE origin. Both are MARKET_TRADING_PROCEEDS; one was round
	// tripped out of a promotional grant and carries that floor (D-131), the
	// other out of a settled purchase.
	lots := []credit.Lot{
		{
			ID: credit.NewLotID(), Origin: valuedomain.OriginMarketTradingProceeds,
			OriginFloor: valuedomain.OriginPromotional,
			Finality:    valuedomain.FinalityUnfunded,
			Quantity:    qty(200_000_000), Remaining: qty(200_000_000),
			CreatedAt: now.Add(-48 * time.Hour),
		},
		{
			ID: credit.NewLotID(), Origin: valuedomain.OriginMarketTradingProceeds,
			OriginFloor: valuedomain.OriginPurchased,
			Finality:    valuedomain.FinalitySettled,
			Quantity:    qty(200_000_000), Remaining: qty(200_000_000),
			CreatedAt: now.Add(-24 * time.Hour),
		},
	}

	// What the payout engine decides, lot by lot: this is exactly
	// credit.EligibleLots' loop and exactly what POST /v1/payouts reserves.
	payoutEligible := money.Quantity{}
	for _, l := range lots {
		ok, _ := policy.Permits(valuedomain.PermitInput{
			Origin: l.Origin, OriginFloor: l.OriginFloor, Finality: l.Finality,
			Domain: valuedomain.InternalCredit, Verified: valuedomain.VerificationPayoutKYC,
			HeldDays: l.AgeDays(now), ActiveCaps: caps, PolicyValid: true,
		})
		if ok {
			payoutEligible = payoutEligible.Add(l.Remaining)
		}
	}
	require.Equal(t, "200000000", payoutEligible.String(),
		"fixture check: the purchased-floored proceeds are payout-eligible and the other lot is not")

	// What the page reports, through the product's own folding.
	gross := qty(400_000_000)
	out := eligibility.ExplainWithdrawal(eligibility.WithdrawalInput{
		Policy:      policy,
		Verified:    valuedomain.VerificationPayoutKYC,
		ActiveCaps:  caps,
		Holdings:    foldHoldings(lots, now),
		PolicyValid: true,

		Gross:          gross,
		Spendable:      gross,
		PayoutEligible: payoutEligible,

		JurisdictionSupported: true,
		ProviderAvailable:     true,
		DestinationConfigured: true,
		DisclosureAccepted:    true,
	})

	assert.Equal(t, out.PayoutEligible.String(), out.WithdrawableNow.String(),
		"F-wv3-3: one response says payout_eligible=%s and withdrawable_now=%s. "+
			"The first is credit.Balances' per-lot answer and is what POST /v1/payouts "+
			"will reserve; the second is the sum of the per-origin buckets, and "+
			"foldHoldings gave the whole MARKET_TRADING_PROCEEDS bucket the most "+
			"restricted floor in it",
		out.PayoutEligible.String(), out.WithdrawableNow.String())
	assert.True(t, out.Eligible,
		"F-wv3-3: the page reports eligible=false — the verdict the Withdraw control is "+
			"gated on — for an account whose conversion request the payout engine approves")
}
