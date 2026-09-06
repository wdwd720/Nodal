package ledger

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Shared fixtures for the unit tests. Ids are fresh per process; tests that
// need stable ids (the hash golden) parse fixed v7-shaped strings.
var (
	testAccount   = accounts.NewAccountID()
	testUSDC      = assets.NewAssetID()
	testSOL       = assets.NewAssetID()
	testEffective = time.Date(2026, 9, 5, 10, 0, 0, 1, time.UTC)
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func cust(code Code, asset assets.AssetID) AccountRef {
	return CustomerAccount(testAccount, code, asset)
}

// fundingPosting is a minimal valid posting: Dr WALLET n / Cr CAPITAL n.
func fundingPosting(n int64) Posting {
	return Posting{
		Kind:           KindFundingSettled,
		IdempotencyKey: "deposit:d1:settled",
		Reference:      FinancialEventReference{Type: "deposit", ID: "d1"},
		EffectiveAt:    testEffective,
		Entries: []Entry{
			{Account: cust(CodeWallet, testUSDC), Side: Debit, Quantity: q(n)},
			{Account: cust(CodeCapital, testUSDC), Side: Credit, Quantity: q(n)},
		},
	}
}

// applyPostings folds postings into normal-side balances keyed by
// AccountRef.key, exactly as the database trigger would.
func applyPostings(ps ...Posting) map[string]money.Quantity {
	out := map[string]money.Quantity{}
	for _, p := range ps {
		for _, e := range p.Entries {
			k := e.Account.key()
			if e.Side == e.Account.Code.NormalSide() {
				out[k] = out[k].Add(e.Quantity)
			} else {
				out[k] = out[k].Sub(e.Quantity)
			}
		}
	}
	return out
}

func balanceOf(m map[string]money.Quantity, ref AccountRef) money.Quantity { return m[ref.key()] }

// requireCode asserts err carries the given errs code.
func requireCode(t *testing.T, err error, code errs.Code, msgAndArgs ...any) {
	t.Helper()
	require.Error(t, err, msgAndArgs...)
	msg := "want " + string(code) + ", got: " + err.Error()
	if len(msgAndArgs) > 0 {
		msg += " (" + fmt.Sprint(msgAndArgs...) + ")"
	}
	require.Equal(t, code, errs.CodeOf(err), msg)
}
