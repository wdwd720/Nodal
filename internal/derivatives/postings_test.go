package derivatives

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

func posting(kind ledger.Kind, entries []ledger.Entry) ledger.Posting {
	return ledger.Posting{Kind: kind, IdempotencyKey: "test", Reference: ledger.FinancialEventReference{Type: "t", ID: "1"},
		EffectiveAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), Entries: entries}
}

// TestProp_EveryPostingTemplateBalancesPerParty validates every template this
// package posts with the ledger's own validator, which for simulated-only
// kinds requires each (owner, asset) to debit exactly what it credits -- the
// rule the database enforces again as SM002.
func TestProp_EveryPostingTemplateBalancesPerParty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		acct := accounts.NewAccountID()
		ch := econ.Facility{ID: econ.NewFacilityID(), Kind: ledger.FacilityClearingHouse}
		asset := assets.NewAssetID()
		amt := q(rapid.Int64Range(1, 1<<50).Draw(t, "amount"))
		alloc := q(rapid.Int64Range(-(1<<50), 1<<50).Draw(t, "alloc"))
		released := q(rapid.Int64Range(0, 1<<50).Draw(t, "released"))
		wallet := q(rapid.Int64Range(0, 1<<50).Draw(t, "wallet"))
		margin := q(rapid.Int64Range(0, 1<<50).Draw(t, "margin"))
		pay := splitPayment(amt, released, wallet, margin)
		if !pay.collected().Add(pay.Shortfall).Equal(amt) {
			t.Fatal("a payment does not account for the whole obligation")
		}
		templates := []struct {
			name    string
			kind    ledger.Kind
			entries []ledger.Entry
		}{
			{"margin post", ledger.KindDerivativeMarginMove, marginMoveEntries(acct, asset, amt, true)},
			{"margin release", ledger.KindDerivativeMarginMove, marginMoveEntries(acct, asset, amt, false)},
			{"variation received", ledger.KindVariationMargin, receiveEntries(acct, ch, asset, amt)},
			{"variation paid", ledger.KindVariationMargin, payEntries(acct, ch, asset, pay)},
			{"funding received", ledger.KindFundingPayment, receiveEntries(acct, ch, asset, amt)},
			{"funding paid", ledger.KindFundingPayment, payEntries(acct, ch, asset, pay)},
			{"future settlement paid", ledger.KindFutureSettlement, payEntries(acct, ch, asset, pay)},
			{"premium paid", ledger.KindOptionPremium, premiumPaidEntries(acct, ch, asset, amt)},
			{"premium received", ledger.KindOptionPremium, premiumReceivedEntries(acct, ch, asset, amt)},
			{"option closed by a holder", ledger.KindOptionPremium,
				append(receiveEntries(acct, ch, asset, amt), recognizePremiumEntries(acct, asset, alloc.Abs())...)},
			{"option settled for a writer", ledger.KindOptionSettlement,
				append(payEntries(acct, ch, asset, pay), recognizePremiumEntries(acct, asset, alloc.Abs().Neg())...)},
			{"clearing house capital", ledger.KindFacilityFunding, clearingFundingEntries(ch, asset, amt)},
		}
		for _, tpl := range templates {
			if len(tpl.entries) == 0 {
				continue // nothing collected: nothing posted
			}
			if err := posting(tpl.kind, tpl.entries).Validate(); err != nil {
				t.Fatalf("%s: %v", tpl.name, err)
			}
		}
	})
}

func TestPostingTemplates_NeverEmitAZeroEntry(t *testing.T) {
	acct := accounts.NewAccountID()
	ch := econ.Facility{ID: econ.NewFacilityID(), Kind: ledger.FacilityClearingHouse}
	asset := assets.NewAssetID()
	zero := money.Quantity{}
	assert.Empty(t, marginMoveEntries(acct, asset, zero, true))
	assert.Empty(t, receiveEntries(acct, ch, asset, zero))
	assert.Empty(t, payEntries(acct, ch, asset, payment{}))
	assert.Empty(t, premiumPaidEntries(acct, ch, asset, zero))
	assert.Empty(t, premiumReceivedEntries(acct, ch, asset, zero))
	assert.Empty(t, recognizePremiumEntries(acct, asset, zero))
	assert.Empty(t, clearingFundingEntries(ch, asset, zero))

	// A payment the available funds cover only partly takes the rest from
	// margin, then records a shortfall; nothing is ever taken that is not
	// there.
	p := splitPayment(q(100), q(0), q(30), q(50))
	assert.Equal(t, "30", p.FromWallet.String())
	assert.Equal(t, "50", p.FromMargin.String())
	assert.Equal(t, "20", p.Shortfall.String())
	p = splitPayment(q(100), q(0), q(-5), q(0))
	assert.True(t, p.FromWallet.IsZero())
	assert.Equal(t, "100", p.Shortfall.String())
	// The margin a reduction frees pays first, before it reaches the wallet.
	p = splitPayment(q(100), q(40), q(30), q(50))
	assert.Equal(t, "40", p.FromRelease.String())
	assert.Equal(t, "30", p.FromWallet.String())
	assert.Equal(t, "30", p.FromMargin.String())
	assert.True(t, p.Shortfall.IsZero())
	assert.Equal(t, "70", p.fromMarginAccount().String())
}

// TestProp_APaymentTakesEachSourceInOrderAndNoMoreThanItHas: an obligation is
// met from the freed margin, then the available funds, then the kept margin,
// each never beyond what it holds; what is left is the shortfall, and the
// parts add up to the obligation exactly.
func TestProp_APaymentTakesEachSourceInOrderAndNoMoreThanItHas(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amt := q(rapid.Int64Range(0, 1<<50).Draw(t, "amount"))
		released := q(rapid.Int64Range(0, 1<<50).Draw(t, "released"))
		available := q(rapid.Int64Range(-(1<<40), 1<<50).Draw(t, "available"))
		kept := q(rapid.Int64Range(0, 1<<50).Draw(t, "kept"))
		p := splitPayment(amt, released, available, kept)
		zero := money.Quantity{}
		for name, part := range map[string]money.Quantity{"release": p.FromRelease, "wallet": p.FromWallet, "margin": p.FromMargin, "shortfall": p.Shortfall} {
			if part.IsNegative() {
				t.Fatalf("%s part is negative: %s", name, part)
			}
		}
		if p.FromRelease.Cmp(released) > 0 || p.FromWallet.Cmp(available.Max(zero)) > 0 || p.FromMargin.Cmp(kept) > 0 {
			t.Fatalf("a source gave more than it holds: %+v", p)
		}
		if !p.collected().Add(p.Shortfall).Equal(amt) {
			t.Fatalf("the parts do not add up to the obligation: %+v of %s", p, amt)
		}
		if p.FromWallet.IsPositive() && !p.FromRelease.Equal(released) {
			t.Fatal("available funds were taken while freed margin was left")
		}
		if p.FromMargin.IsPositive() && !p.FromWallet.Equal(available.Max(zero)) {
			t.Fatal("kept margin was taken while available funds were left")
		}
		if p.Shortfall.IsPositive() && !p.FromMargin.Equal(kept) {
			t.Fatal("a shortfall was recorded while kept margin was left")
		}
	})
}
