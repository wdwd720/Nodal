package stableasset

import (
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

// reserveModel is the reserve book of one stable asset, reduced to what the
// admission rule reads, so the rule can be exercised over millions of states.
type reserveModel struct {
	a              PlatformStableAsset
	cash, tbill    EligibleReserve
	issued         money.Quantity
	cashBal        money.Quantity
	tbillBal       money.Quantity
	pending        []pendingMint
	cashPrice      money.Price
	tbillPrice     money.Price
	tbillPriceSeen bool
}

type pendingMint struct{ q, r money.Quantity }

func (m *reserveModel) pricing() pricing {
	p := pricing{prices: map[assets.AssetID]*money.Price{}}
	cp := capAtPar(m.cashPrice)
	p.prices[m.cash.AssetID] = &cp
	if m.tbillPriceSeen {
		tp := m.tbillPrice
		p.prices[m.tbill.AssetID] = &tp
	}
	return p
}

func (m *reserveModel) ledger() ReserveLedger {
	p := m.pricing()
	rl := ReserveLedger{IssuedSupply: m.issued,
		CashEquivalentReserve: []ReserveHolding{p.holding(m.cash, m.cashBal)},
		TokenizedReserve:      []ReserveHolding{p.holding(m.tbill, m.tbillBal)}}
	var receivable money.Quantity
	for _, pm := range m.pending {
		rl.PendingMint = rl.PendingMint.Add(pm.q)
		receivable = receivable.Add(pm.r)
	}
	rl.Receivable = []ReserveHolding{p.holding(m.cash, receivable)}
	rl.SupplyValue = unitsValue(rl.IssuedSupply, m.a.Decimals).times(m.a.Peg)
	rl.EligibleReserve = sumEligible(rl.CashEquivalentReserve).Add(sumEligible(rl.TokenizedReserve))
	return rl
}

// projectedCovered: issued and pending supply against the reserve with every
// pending mint's payment counted.
func (m *reserveModel) projectedCovered() bool {
	rl := m.ledger()
	supply := unitsValue(rl.IssuedSupply.Add(rl.PendingMint), m.a.Decimals).times(m.a.Peg)
	return rl.EligibleReserve.Add(sumEligible(rl.Receivable)).Cmp(supply) >= 0
}

func newReserveModel(t *rapid.T) *reserveModel {
	cash := EligibleReserve{AssetID: assets.NewAssetID(), Kind: ReserveCashEquivalent,
		FactorBPS: money.BPS(rapid.Int64Range(5_000, 10_000).Draw(t, "cash_factor")), AcceptedForMint: true,
		Decimals: uint8(rapid.IntRange(2, 9).Draw(t, "cash_decimals"))}
	tbill := EligibleReserve{AssetID: assets.NewAssetID(), Kind: ReserveTokenized,
		FactorBPS: money.BPS(rapid.Int64Range(0, 10_000).Draw(t, "tbill_factor")), Decimals: 6}
	a := PlatformStableAsset{Decimals: uint8(rapid.IntRange(0, 9).Draw(t, "stable_decimals")),
		IssuanceCap: qty(rapid.Int64Range(1, 1_000_000_000).Draw(t, "cap")),
		Peg:         price(rapid.Int64Range(90, 110).Draw(t, "peg"), 2),
		Reserves:    []EligibleReserve{cash, tbill}}
	return &reserveModel{a: a, cash: cash, tbill: tbill, cashPrice: price(1, 0), tbillPrice: price(1, 0), tbillPriceSeen: true}
}

// TestProp_SupplyNeverExceedsEligibleReserve: over random sequences of mint
// requests, finalizations, failures, burns, reserve price moves (stale
// prices included) and tokenized top-ups, the admission rule never admits a
// mint that would leave issued plus pending supply above the verified
// eligible reserve or above the cap (and admits exactly when both hold), and
// a finalization or a redemption never breaks coverage that held before it.
// Price moves and stale prices can only shrink the eligible reserve, which
// makes the rule refuse; they never let it admit more.
func TestProp_SupplyNeverExceedsEligibleReserve(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := newReserveModel(rt)
		steps := rapid.IntRange(1, 60).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			switch rapid.IntRange(0, 6).Draw(rt, "op") {
			case 0, 1: // a mint request
				q := qty(rapid.Int64Range(1, 50_000_000).Draw(rt, "mint"))
				cp := capAtPar(m.cashPrice)
				r, err := requiredReserve(q, m.a.Decimals, m.a.Peg, m.cash.Decimals, cp, m.cash.FactorBPS)
				require.NoError(rt, err)
				payment := m.pricing().holding(m.cash, r)
				reason := admitMint(m.ledger(), m.a, q, payment)
				// Recompute the rule independently: admitted exactly when the
				// cap and the projected coverage both hold afterwards.
				rl := m.ledger()
				after := m.issued.Add(rl.PendingMint).Add(q)
				need := unitsValue(after, m.a.Decimals).times(m.a.Peg)
				have := rl.EligibleReserve.Add(sumEligible(rl.Receivable)).Add(payment.Eligible)
				require.Equal(rt, after.Cmp(m.a.IssuanceCap) <= 0 && need.Cmp(have) <= 0, reason == "", "reason %q", reason)
				if reason != "" {
					continue
				}
				m.pending = append(m.pending, pendingMint{q: q, r: r})
				require.True(rt, m.projectedCovered(), "an admitted mint left supply above the eligible reserve")
				require.True(rt, m.issued.Add(m.ledger().PendingMint).Cmp(m.a.IssuanceCap) <= 0, "an admitted mint broke the cap")
			case 2: // the oldest pending mint finalizes and posts
				if len(m.pending) == 0 {
					continue
				}
				before := m.projectedCovered()
				pm := m.pending[0]
				m.pending = m.pending[1:]
				m.issued, m.cashBal = m.issued.Add(pm.q), m.cashBal.Add(pm.r)
				if before {
					require.True(rt, m.projectedCovered(), "finalizing a pending mint broke coverage")
				}
			case 3: // the newest pending mint fails and releases
				if len(m.pending) > 0 {
					m.pending = m.pending[:len(m.pending)-1]
				}
			case 4: // a burn redeems at par
				if !m.issued.IsPositive() {
					continue
				}
				q := qty(rapid.Int64Range(1, 50_000_000).Draw(rt, "burn")).Min(m.issued)
				payout, err := parPayout(q, m.a.Decimals, m.a.Peg, m.cash.Decimals)
				require.NoError(rt, err)
				if payout.Cmp(m.cashBal) > 0 || !payout.IsPositive() {
					continue
				}
				before := m.ledger().Covered()
				m.issued, m.cashBal = m.issued.Sub(q), m.cashBal.Sub(payout)
				if before {
					require.True(rt, m.ledger().Covered(), "a redemption broke coverage")
				}
			case 5: // a reserve price moves, or the tokenized price goes stale
				m.cashPrice = price(rapid.Int64Range(50, 150).Draw(rt, "cash_price"), 2)
				m.tbillPrice = price(rapid.Int64Range(0, 150).Draw(rt, "tbill_price"), 2)
				m.tbillPriceSeen = rapid.Bool().Draw(rt, "tbill_price_fresh")
			case 6: // the operator over-collateralises with tokenized reserve
				m.tbillBal = m.tbillBal.Add(qty(rapid.Int64Range(1, 10_000_000).Draw(rt, "tbill_funding")))
			}
		}
	})
}
