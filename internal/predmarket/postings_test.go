package predmarket

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

func testMarket(n int) Market {
	m := Market{
		ID: NewMarketID(), Symbol: "TEST", FacilityID: econ.NewFacilityID(), CollateralAssetID: assets.NewAssetID(),
		UnitQuantity: ShareUnit(),
	}
	for i := 0; i < n; i++ {
		m.Outcomes = append(m.Outcomes, Outcome{Index: i, Symbol: string(rune('A' + i)), ShareAssetID: assets.NewAssetID()})
	}
	return m
}

func meta(key string) postingMeta {
	return postingMeta{Key: key, Reference: ledger.FinancialEventReference{Type: "test", ID: key}, At: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
}

// deltas applies a posting to empty books: the signed change of every
// account's normal-side balance.
func deltas(p ledger.Posting) map[ledger.AccountRef]money.Quantity {
	out := map[ledger.AccountRef]money.Quantity{}
	for _, e := range p.Entries {
		d := e.Quantity
		if e.Side != e.Account.Code.NormalSide() {
			d = d.Neg()
		}
		out[e.Account] = out[e.Account].Add(d)
	}
	return out
}

func TestPostings_FollowTheTemplates(t *testing.T) {
	m := testMarket(2)
	a := accounts.NewAccountID()
	f := m.Facility()
	cash := m.CollateralAssetID
	qty := q(3_000_000)

	iss, err := IssuancePosting(m, a, qty, meta("iss"))
	require.NoError(t, err)
	assert.Equal(t, ledger.KindPredictionSetIssuance, iss.Kind)
	d := deltas(iss)
	assert.Equal(t, "-3000000", d[ledger.CustomerAccount(a, ledger.CodeWallet, cash)].String())
	assert.Equal(t, "3000000", d[ledger.CustomerAccount(a, ledger.CodeTradingOutflow, cash)].String())
	assert.Equal(t, "3000000", d[f.Account(ledger.CodePredictionEscrow, cash)].String())
	assert.Equal(t, "3000000", d[f.Account(ledger.CodePredictionCollateral, cash)].String())
	for _, o := range m.Outcomes {
		assert.Equal(t, "3000000", d[ledger.CustomerAccount(a, ledger.CodeWallet, o.ShareAssetID)].String())
		assert.Equal(t, "3000000", d[ledger.CustomerAccount(a, ledger.CodeTradingInflow, o.ShareAssetID)].String())
		assert.Equal(t, "3000000", d[f.Account(ledger.CodePredictionSharesIssued, o.ShareAssetID)].String())
		assert.Equal(t, "3000000", d[f.Account(ledger.CodePredictionSharesOutstanding, o.ShareAssetID)].String())
	}

	red, err := RedemptionPosting(m, a, qty, meta("red"))
	require.NoError(t, err)
	assert.Equal(t, ledger.KindPredictionSetRedemption, red.Kind)
	d = deltas(red)
	assert.Equal(t, "3000000", d[ledger.CustomerAccount(a, ledger.CodeWallet, cash)].String())
	assert.Equal(t, "-3000000", d[f.Account(ledger.CodePredictionEscrow, cash)].String())
	for _, o := range m.Outcomes {
		assert.Equal(t, "-3000000", d[ledger.CustomerAccount(a, ledger.CodeWallet, o.ShareAssetID)].String())
		assert.Equal(t, "-3000000", d[f.Account(ledger.CodePredictionSharesOutstanding, o.ShareAssetID)].String())
	}

	pay, err := SettlementPosting(m, SettlementPayout, PlanLine{AccountID: a, Shares: []money.Quantity{q(2_000_000), q(1_000_000)}, Amount: q(2_000_000)}, meta("pay"))
	require.NoError(t, err)
	assert.Equal(t, ledger.KindPredictionPayout, pay.Kind)
	d = deltas(pay)
	assert.Equal(t, "2000000", d[ledger.CustomerAccount(a, ledger.CodeWallet, cash)].String())
	assert.Equal(t, "-2000000", d[f.Account(ledger.CodePredictionEscrow, cash)].String())
	assert.Equal(t, "-2000000", d[ledger.CustomerAccount(a, ledger.CodeWallet, m.Outcomes[0].ShareAssetID)].String())
	assert.Equal(t, "-1000000", d[ledger.CustomerAccount(a, ledger.CodeWallet, m.Outcomes[1].ShareAssetID)].String())

	loser, err := SettlementPosting(m, SettlementRefund, PlanLine{AccountID: a, Shares: []money.Quantity{q(0), q(1)}, Amount: q(0)}, meta("loser"))
	require.NoError(t, err, "a holding that is paid nothing still has its shares retired")
	assert.Equal(t, ledger.KindPredictionRefund, loser.Kind)
	assert.Len(t, loser.Entries, 4)

	_, err = IssuancePosting(m, a, q(1_500_000), meta("frac"))
	require.Error(t, err, "issuance is in whole sets")
	_, err = SettlementPosting(m, "BONUS", PlanLine{AccountID: a, Shares: []money.Quantity{q(1), q(0)}}, meta("x"))
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestProp_PostingsBalancePerPartyAndKeepTheEscrowIdentity: every posting the
// builders produce validates (balanced per asset AND per party, the rule the
// database enforces as SM002), and its effect on the facility's books keeps
// escrow = collateral = shares outstanding(i) = shares issued(i) moving in
// lockstep with the customer side.
func TestProp_PostingsBalancePerPartyAndKeepTheEscrowIdentity(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(MinOutcomes, MaxOutcomes).Draw(rt, "outcomes")
		m := testMarket(n)
		a := accounts.NewAccountID()
		f := m.Facility()
		sets := rapid.Int64Range(1, 1_000_000).Draw(rt, "sets")
		qty := q(sets * shareUnitBaseUnits)

		check := func(p ledger.Posting, escrowDelta money.Quantity, shareDelta []money.Quantity) {
			if err := p.Validate(); err != nil {
				rt.Fatalf("%s does not validate: %v", p.Kind, err)
			}
			if len(p.Entries) > ledger.MaxEntries {
				rt.Fatalf("%s has %d entries", p.Kind, len(p.Entries))
			}
			d := deltas(p)
			if d[f.Account(ledger.CodePredictionEscrow, m.CollateralAssetID)].Cmp(escrowDelta) != 0 ||
				d[f.Account(ledger.CodePredictionCollateral, m.CollateralAssetID)].Cmp(escrowDelta) != 0 {
				rt.Fatalf("%s moved the escrow by %s, want %s", p.Kind, d[f.Account(ledger.CodePredictionEscrow, m.CollateralAssetID)], escrowDelta)
			}
			for i, o := range m.Outcomes {
				out := d[f.Account(ledger.CodePredictionSharesOutstanding, o.ShareAssetID)]
				iss := d[f.Account(ledger.CodePredictionSharesIssued, o.ShareAssetID)]
				held := d[ledger.CustomerAccount(a, ledger.CodeWallet, o.ShareAssetID)]
				if out.Cmp(shareDelta[i]) != 0 || iss.Cmp(shareDelta[i]) != 0 || held.Cmp(shareDelta[i]) != 0 {
					rt.Fatalf("%s outcome %d: outstanding %s issued %s held %s, want %s", p.Kind, i, out, iss, held, shareDelta[i])
				}
			}
		}

		all := func(v money.Quantity) []money.Quantity {
			out := make([]money.Quantity, n)
			for i := range out {
				out[i] = v
			}
			return out
		}
		iss, err := IssuancePosting(m, a, qty, meta("i"))
		if err != nil {
			rt.Fatalf("issuance: %v", err)
		}
		check(iss, qty, all(qty))
		red, err := RedemptionPosting(m, a, qty, meta("r"))
		if err != nil {
			rt.Fatalf("redemption: %v", err)
		}
		check(red, qty.Neg(), all(qty.Neg()))

		line := PlanLine{AccountID: a, Shares: make([]money.Quantity, n)}
		retired := make([]money.Quantity, n)
		for i := range line.Shares {
			line.Shares[i] = q(rapid.Int64Range(0, 5_000_000).Draw(rt, "held"))
			retired[i] = line.Shares[i].Neg()
		}
		line.Shares[0] = line.Shares[0].Add(q(1)) // at least one share is retired
		retired[0] = line.Shares[0].Neg()
		line.Amount = q(rapid.Int64Range(0, 5_000_000).Draw(rt, "paid"))
		kind := rapid.SampledFrom([]SettlementKind{SettlementPayout, SettlementRefund}).Draw(rt, "kind")
		st, err := SettlementPosting(m, kind, line, meta("s"))
		if err != nil {
			rt.Fatalf("settlement: %v", err)
		}
		check(st, line.Amount.Neg(), retired)
	})
}
