package transfers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// book is an in-memory ledger keyed by (owner, code), balances in
// normal-side terms exactly as ledger_balances keeps them.
type book map[string]money.Quantity

func (b book) apply(entries []ledger.Entry) {
	for _, e := range entries {
		k := e.Account.OwnerID + "/" + string(e.Account.Code)
		delta := e.Quantity
		if e.Side != e.Account.Code.NormalSide() {
			delta = delta.Neg()
		}
		b[k] = b[k].Add(delta)
	}
}

func (b book) get(a accounts.AccountID, c ledger.Code) money.Quantity {
	return b[a.String()+"/"+string(c)]
}

// perPartyBalanced reports whether, for each owner, debits equal credits.
func perPartyBalanced(entries []ledger.Entry) bool {
	net := map[string]money.Quantity{}
	for _, e := range entries {
		if e.Side == ledger.Debit {
			net[e.Account.OwnerID] = net[e.Account.OwnerID].Add(e.Quantity)
		} else {
			net[e.Account.OwnerID] = net[e.Account.OwnerID].Sub(e.Quantity)
		}
	}
	for _, v := range net {
		if !v.IsZero() {
			return false
		}
	}
	return true
}

func TestPostings_TheTemplatesBalancePerPartyAndMirrorEachOther(t *testing.T) {
	a, b := accounts.NewAccountID(), accounts.NewAccountID()
	asset := assets.NewAssetID()
	q := money.QuantityFromInt64(42)
	fwd, rev := TransferEntries(a, b, asset, q), ReversalEntries(a, b, asset, q)
	assert.True(t, perPartyBalanced(fwd))
	assert.True(t, perPartyBalanced(rev))
	require.Len(t, rev, len(fwd))
	// Every reversal leg is a transfer leg on the other side.
	for _, r := range rev {
		found := false
		for _, f := range fwd {
			if f.Account == r.Account && f.Side == r.Side.Opposite() && f.Quantity.Equal(r.Quantity) {
				found = true
			}
		}
		assert.True(t, found, "%v has no mirror", r)
	}

	tid := NewTransferID()
	journal := ledger.NewTransactionID()
	tr := Transfer{ID: tid, SenderAccountID: a, RecipientAccountID: b, AssetID: asset, Quantity: q, Memo: "secret memo",
		JournalTransactionID: &journal}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	p := transferPosting(tr, now)
	require.NoError(t, p.Validate(), "the ledger's own validation, per-party balance included")
	assert.Equal(t, ledger.KindP2PTransfer, p.Kind)
	assert.NotContains(t, p.Description, "secret")
	assert.Empty(t, p.Metadata)
	content, err := ledger.CanonicalContent(p)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "secret", "the memo never reaches the content hash")

	r := reversalPosting(tr, "FRAUD_CONFIRMED", "corr", now)
	require.NoError(t, r.Validate())
	assert.Equal(t, ledger.KindCompensation, r.Kind)
	require.NotNil(t, r.ReversalOf)
	assert.Equal(t, journal, *r.ReversalOf)
	assert.Equal(t, "FRAUD_CONFIRMED", r.ReasonCode())
	assert.True(t, perPartyBalanced(r.Entries), "a COMPENSATION is not checked per party by the database, so the template must be")
}

// TestProp_PostingTemplatesConserveCustomerWallets is the model of the
// transfer ledger: random fundings, transfers (only within the sender's
// WALLET, as the reservation guarantees) and reversals (only within the
// recipient's WALLET) among N parties. After every step Σ WALLET equals Σ
// funding, every party satisfies WALLET + TRANSFER_OUT − TRANSFER_IN −
// CAPITAL = 0, and no account is negative.
func TestProp_PostingTemplatesConserveCustomerWallets(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(2, 6).Draw(rt, "parties")
		parties := make([]accounts.AccountID, n)
		for i := range parties {
			parties[i] = accounts.NewAccountID()
		}
		asset := assets.NewAssetID()
		b := book{}
		total := money.Quantity{}
		type done struct {
			from, to accounts.AccountID
			q        money.Quantity
		}
		var completed []done
		for range rapid.IntRange(1, 80).Draw(rt, "steps") {
			switch rapid.IntRange(0, 2).Draw(rt, "op") {
			case 0:
				who := parties[rapid.IntRange(0, n-1).Draw(rt, "who")]
				q := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000).Draw(rt, "funding"))
				b.apply([]ledger.Entry{
					{Account: ledger.CustomerAccount(who, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
					{Account: ledger.CustomerAccount(who, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: q},
				})
				total = total.Add(q)
			case 1:
				i := rapid.IntRange(0, n-1).Draw(rt, "from")
				j := (i + 1 + rapid.IntRange(0, n-2).Draw(rt, "to")) % n
				q := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000).Draw(rt, "amount"))
				if b.get(parties[i], ledger.CodeWallet).Cmp(q) < 0 {
					continue
				}
				b.apply(TransferEntries(parties[i], parties[j], asset, q))
				completed = append(completed, done{parties[i], parties[j], q})
			case 2:
				if len(completed) == 0 {
					continue
				}
				k := rapid.IntRange(0, len(completed)-1).Draw(rt, "which")
				d := completed[k]
				if b.get(d.to, ledger.CodeWallet).Cmp(d.q) < 0 {
					continue
				}
				b.apply(ReversalEntries(d.from, d.to, asset, d.q))
				completed = append(completed[:k], completed[k+1:]...)
			}
			sum := money.Quantity{}
			for _, p := range parties {
				w := b.get(p, ledger.CodeWallet)
				sum = sum.Add(w)
				residue := w.Add(b.get(p, ledger.CodeTransferOut)).Sub(b.get(p, ledger.CodeTransferIn)).Sub(b.get(p, ledger.CodeCapital))
				if !residue.IsZero() {
					rt.Fatalf("party identity broken by %s", residue)
				}
				for _, c := range []ledger.Code{ledger.CodeWallet, ledger.CodeCapital, ledger.CodeTransferOut, ledger.CodeTransferIn} {
					if b.get(p, c).IsNegative() {
						rt.Fatalf("%s went negative", c)
					}
				}
			}
			if !sum.Equal(total) {
				rt.Fatalf("Σ WALLET %s, funding %s", sum, total)
			}
		}
	})
}
