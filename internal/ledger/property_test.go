package ledger

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Generators.

// genPositiveInt64Quantity draws a positive quantity that fits int64 with
// headroom, so sums of a few of them stay exact in the generator arithmetic.
func genPositiveInt64Quantity(t *rapid.T, label string) money.Quantity {
	return money.QuantityFromInt64(rapid.Int64Range(1, 1e17).Draw(t, label))
}

// splitQuantity splits total (>= 1) into 1..3 positive parts.
func splitQuantity(t *rapid.T, total money.Quantity) []money.Quantity {
	n, err := total.Int64()
	if err != nil {
		t.Fatalf("total does not fit int64: %v", err)
	}
	parts := rapid.IntRange(1, 3).Draw(t, "parts")
	if int64(parts) > n {
		parts = 1
	}
	out := make([]money.Quantity, 0, parts)
	remaining := n
	for i := 0; i < parts-1; i++ {
		// leave at least 1 for every remaining part
		maxPart := remaining - int64(parts-1-i)
		p := rapid.Int64Range(1, maxPart).Draw(t, "part")
		out = append(out, money.QuantityFromInt64(p))
		remaining -= p
	}
	out = append(out, money.QuantityFromInt64(remaining))
	return out
}

func shuffleEntries(t *rapid.T, entries []Entry) []Entry {
	out := append([]Entry(nil), entries...)
	for i := len(out) - 1; i > 0; i-- {
		j := rapid.IntRange(0, i).Draw(t, "swap")
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func genAccountRef(t *rapid.T, asset assets.AssetID) AccountRef {
	code := rapid.SampledFrom(AllCodes()).Draw(t, "code")
	if code.OwnerType() == OwnerPlatform {
		return PlatformAccount(code, asset)
	}
	return CustomerAccount(testAccount, code, asset)
}

// genBalancedEntries draws a random multi-asset balanced entry set: per
// asset, 1..4 debits whose total is split into 1..3 credits.
func genBalancedEntries(t *rapid.T, pool []assets.AssetID) []Entry {
	nAssets := rapid.IntRange(1, len(pool)).Draw(t, "assets")
	var entries []Entry
	for a := 0; a < nAssets; a++ {
		asset := pool[a]
		var total money.Quantity
		for i := rapid.IntRange(1, 4).Draw(t, "debits"); i > 0; i-- {
			qty := genPositiveInt64Quantity(t, "debit")
			entries = append(entries, Entry{Account: genAccountRef(t, asset), Side: Debit, Quantity: qty})
			total = total.Add(qty)
		}
		for _, part := range splitQuantity(t, total) {
			entries = append(entries, Entry{Account: genAccountRef(t, asset), Side: Credit, Quantity: part})
		}
	}
	return shuffleEntries(t, entries)
}

// Properties.

// TestProp_PostingBalanced: any balanced multi-asset entry set validates, and
// any single-entry mutation (quantity, side, asset, removal) invalidates it
// with LEDGER_UNBALANCED (FINANCIAL_MODEL §8).
func TestProp_PostingBalanced(t *testing.T) {
	pool := []assets.AssetID{assets.NewAssetID(), assets.NewAssetID(), assets.NewAssetID(), assets.NewAssetID()}
	rapid.Check(t, func(rt *rapid.T) {
		entries := genBalancedEntries(rt, pool)
		p := Posting{
			Kind:           KindSeed,
			IdempotencyKey: "seed:prop",
			Reference:      FinancialEventReference{Type: "seed", ID: "prop"},
			EffectiveAt:    testEffective,
			Entries:        entries,
		}
		if err := p.Validate(); err != nil {
			rt.Fatalf("balanced posting rejected: %v", err)
		}
		for _, im := range assetImbalances(p.Entries) {
			if !im.net.IsZero() {
				rt.Fatalf("generator produced imbalance for %s: %s", im.asset, im.net)
			}
		}

		mutated := append([]Entry(nil), entries...)
		idx := rapid.IntRange(0, len(mutated)-1).Draw(rt, "idx")
		switch rapid.SampledFrom([]string{"increment", "flip", "asset", "drop"}).Draw(rt, "mutation") {
		case "increment":
			mutated[idx].Quantity = mutated[idx].Quantity.Add(money.QuantityFromInt64(1))
		case "flip":
			mutated[idx].Side = mutated[idx].Side.Opposite()
		case "asset":
			mutated[idx].Account.AssetID = assets.NewAssetID()
		case "drop":
			mutated = append(mutated[:idx], mutated[idx+1:]...)
		}
		p.Entries = mutated
		err := p.Validate()
		if err == nil {
			rt.Fatalf("mutated posting accepted")
		}
		if errs.CodeOf(err) != errs.CodeLedgerUnbalanced {
			rt.Fatalf("mutated posting failed with %v, want LEDGER_UNBALANCED", err)
		}
	})
}

// TestProp_SwapPostingBalanced: SwapPosting balances every asset for random
// quantities, fee assets (possibly equal to the traded assets) and zero fees,
// and the WALLET credit of the out asset carries every leg paid in it.
func TestProp_SwapPostingBalanced(t *testing.T) {
	pool := []assets.AssetID{assets.NewAssetID(), assets.NewAssetID(), assets.NewAssetID()}
	rapid.Check(t, func(rt *rapid.T) {
		outIdx := rapid.IntRange(0, 2).Draw(rt, "out")
		inIdx := rapid.IntRange(0, 2).Filter(func(i int) bool { return i != outIdx }).Draw(rt, "in")
		in := SwapInputs{
			AccountID:   testAccount,
			FillID:      "f1",
			OutAsset:    pool[outIdx],
			OutQuantity: genPositiveInt64Quantity(rt, "outQty"),
			InAsset:     pool[inIdx],
			InQuantity:  genPositiveInt64Quantity(rt, "inQty"),
			EffectiveAt: testEffective,
		}
		if rapid.Bool().Draw(rt, "networkFee") {
			in.NetworkFeeAsset = pool[rapid.IntRange(0, 2).Draw(rt, "nfAsset")]
			in.NetworkFeeQuantity = genPositiveInt64Quantity(rt, "nfQty")
		}
		if rapid.Bool().Draw(rt, "platformFee") {
			in.PlatformFeeAsset = pool[rapid.IntRange(0, 2).Draw(rt, "pfAsset")]
			in.PlatformFeeQuantity = genPositiveInt64Quantity(rt, "pfQty")
		}
		if rapid.Bool().Draw(rt, "usd") {
			u := money.USDFromMinor(rapid.Int64Range(0, 1e12).Draw(rt, "outUSD"))
			in.OutUSD = &u
			v := money.USDFromMinor(rapid.Int64Range(0, 1e12).Draw(rt, "pfUSD"))
			in.PlatformFeeUSD = &v
		}
		p, err := SwapPosting(in)
		if err != nil {
			rt.Fatalf("SwapPosting: %v", err)
		}
		if err := p.Validate(); err != nil {
			rt.Fatalf("swap posting invalid: %v", err)
		}
		for _, im := range assetImbalances(p.Entries) {
			if !im.net.IsZero() {
				rt.Fatalf("asset %s unbalanced: %s", im.asset, im.net)
			}
		}
		// 4 trade legs + 4 platform-fee legs + 2 network-fee legs, fewer when
		// same-account, same-side legs merge.
		if len(p.Entries) > 10 || len(p.Entries) < 4 {
			rt.Fatalf("unexpected entry count %d", len(p.Entries))
		}
		wantWalletOutCredit := in.OutQuantity
		if in.PlatformFeeQuantity.IsPositive() && in.PlatformFeeAsset == in.OutAsset {
			wantWalletOutCredit = wantWalletOutCredit.Add(in.PlatformFeeQuantity)
		}
		if in.NetworkFeeQuantity.IsPositive() && in.NetworkFeeAsset == in.OutAsset {
			wantWalletOutCredit = wantWalletOutCredit.Add(in.NetworkFeeQuantity)
		}
		wallet := CustomerAccount(testAccount, CodeWallet, in.OutAsset)
		var found bool
		for _, e := range p.Entries {
			if e.Account.key() == wallet.key() && e.Side == Credit {
				if found {
					rt.Fatalf("WALLET:OUT credit appears twice; legs were not merged")
				}
				found = true
				if !e.Quantity.Equal(wantWalletOutCredit) {
					rt.Fatalf("WALLET:OUT credit %s, want %s", e.Quantity, wantWalletOutCredit)
				}
			}
		}
		if !found {
			rt.Fatalf("no WALLET:OUT credit entry")
		}
	})
}

// TestProp_FundingReversalDeficit: for any 0 <= walletBalance < amount the
// two-transaction pattern leaves WALLET at zero (never negative), CAPITAL at
// −amount and DEFICIT at +shortfall; for walletBalance >= amount a single
// FUNDING_REVERSAL suffices and no deficit exists.
func TestProp_FundingReversalDeficit(t *testing.T) {
	wallet := CustomerAccount(testAccount, CodeWallet, testUSDC)
	capital := CustomerAccount(testAccount, CodeCapital, testUSDC)
	deficit := CustomerAccount(testAccount, CodeDeficit, testUSDC)
	in := FundingReversalInputs{AccountID: testAccount, DepositID: "d1", AssetID: testUSDC, EffectiveAt: testEffective, Reason: "chargeback"}

	rapid.Check(t, func(rt *rapid.T) {
		wb := rapid.Int64Range(0, 1e17).Draw(rt, "walletBalance")
		amount := rapid.Int64Range(wb+1, 2e17).Draw(rt, "amount")
		walletBalance := money.QuantityFromInt64(wb)
		amountQ := money.QuantityFromInt64(amount)
		shortfall := amountQ.Sub(walletBalance)

		ps, err := FundingReversalPostings(in, walletBalance, amountQ)
		if err != nil {
			rt.Fatalf("FundingReversalPostings: %v", err)
		}
		for _, p := range ps {
			if err := p.Validate(); err != nil {
				rt.Fatalf("posting %s invalid: %v", p.Kind, err)
			}
		}
		wantCount := 1
		if wb > 0 {
			wantCount = 2
		}
		if len(ps) != wantCount {
			rt.Fatalf("got %d postings, want %d", len(ps), wantCount)
		}
		if ps[len(ps)-1].Kind != KindFundingReversalDeficit {
			rt.Fatalf("last posting kind %s, want FUNDING_REVERSAL_DEFICIT", ps[len(ps)-1].Kind)
		}
		if wb > 0 && ps[0].Kind != KindFundingReversal {
			rt.Fatalf("first posting kind %s, want FUNDING_REVERSAL", ps[0].Kind)
		}

		// Start from the wallet holding walletBalance and apply the postings.
		net := applyPostings(ps...)
		walletAfter := walletBalance.Add(balanceOf(net, wallet))
		if walletAfter.IsNegative() {
			rt.Fatalf("WALLET went negative: %s", walletAfter)
		}
		if !walletAfter.IsZero() {
			rt.Fatalf("WALLET after reversal %s, want 0", walletAfter)
		}
		if !balanceOf(net, capital).Equal(amountQ.Neg()) {
			rt.Fatalf("CAPITAL delta %s, want %s", balanceOf(net, capital), amountQ.Neg())
		}
		if !balanceOf(net, deficit).Equal(shortfall) {
			rt.Fatalf("DEFICIT delta %s, want %s", balanceOf(net, deficit), shortfall)
		}
		// Per-asset balance identity: Σ debit-normal deltas == Σ credit-normal deltas.
		var dr, cr money.Quantity
		for _, ref := range []AccountRef{wallet, capital, deficit} {
			if ref.Code.NormalSide() == Debit {
				dr = dr.Add(balanceOf(net, ref))
			} else {
				cr = cr.Add(balanceOf(net, ref))
			}
		}
		if !dr.Equal(cr) {
			rt.Fatalf("balance identity broken: debit-normal %s, credit-normal %s", dr, cr)
		}
	})

	rapid.Check(t, func(rt *rapid.T) {
		amount := rapid.Int64Range(1, 1e17).Draw(rt, "amount")
		wb := rapid.Int64Range(amount, 2e17).Draw(rt, "walletBalance")
		ps, err := FundingReversalPostings(in, money.QuantityFromInt64(wb), money.QuantityFromInt64(amount))
		if err != nil {
			rt.Fatalf("FundingReversalPostings: %v", err)
		}
		if len(ps) != 1 || ps[0].Kind != KindFundingReversal {
			rt.Fatalf("full coverage must yield one FUNDING_REVERSAL, got %d", len(ps))
		}
		net := applyPostings(ps...)
		if !balanceOf(net, wallet).Equal(money.QuantityFromInt64(-amount)) || !balanceOf(net, capital).Equal(money.QuantityFromInt64(-amount)) {
			rt.Fatalf("full reversal deltas wallet %s capital %s", balanceOf(net, wallet), balanceOf(net, capital))
		}
		if !balanceOf(net, deficit).IsZero() {
			rt.Fatalf("deficit must be zero, got %s", balanceOf(net, deficit))
		}
	})
}

// TestProp_ContentHashOrderInvariant: shuffling entries never changes the
// hash; changing any entry quantity always does.
func TestProp_ContentHashOrderInvariant(t *testing.T) {
	pool := []assets.AssetID{assets.NewAssetID(), assets.NewAssetID()}
	rapid.Check(t, func(rt *rapid.T) {
		entries := genBalancedEntries(rt, pool)
		p := Posting{Kind: KindSeed, IdempotencyKey: "seed:hash", Reference: FinancialEventReference{Type: "seed", ID: "hash"}, EffectiveAt: testEffective, Entries: entries}
		want, err := ContentHash(p)
		if err != nil {
			rt.Fatalf("hash: %v", err)
		}
		p.Entries = shuffleEntries(rt, entries)
		got, err := ContentHash(p)
		if err != nil {
			rt.Fatalf("hash: %v", err)
		}
		if string(got) != string(want) {
			rt.Fatalf("hash changed under reorder")
		}
		idx := rapid.IntRange(0, len(p.Entries)-1).Draw(rt, "idx")
		p.Entries[idx].Quantity = p.Entries[idx].Quantity.Add(money.QuantityFromInt64(1))
		changed, err := ContentHash(p)
		if err != nil {
			rt.Fatalf("hash: %v", err)
		}
		if string(changed) == string(want) {
			rt.Fatalf("hash unchanged after quantity mutation")
		}
	})
}
