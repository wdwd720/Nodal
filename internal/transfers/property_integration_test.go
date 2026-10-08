//go:build integration

package transfers

import (
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestProp_TransfersAndReversalsConserveCustomerWallets drives random
// sequences of transfers and reversals among 2..4 customers through the real
// service and database, and after every step checks:
//
//   - Σ WALLET over the customers equals Σ funding (constant: a transfer or a
//     reversal changes ownership, never the total);
//   - for every customer, WALLET + TRANSFER_OUT − TRANSFER_IN − CAPITAL = 0;
//   - no WALLET is negative;
//
// and at the end that Reconcile finds nothing.
func TestProp_TransfersAndReversalsConserveCustomerWallets(t *testing.T) {
	f := newFixture(t)
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(2, 4).Draw(rt, "customers")
		cs := make([]customer, n)
		all := make([]accounts.AccountID, n)
		total := money.Quantity{}
		for i := range cs {
			cs[i] = f.customer()
			all[i] = cs[i].account
			amount := rapid.Int64Range(0, 5_000_000).Draw(rt, "funding")
			if amount > 0 {
				f.fund(cs[i].account, amount)
				total = total.Add(money.QuantityFromInt64(amount))
			}
		}
		var completed []Transfer
		steps := rapid.IntRange(1, 8).Draw(rt, "steps")
		for range steps {
			if len(completed) > 0 && rapid.Bool().Draw(rt, "reverse") {
				i := rapid.IntRange(0, len(completed)-1).Draw(rt, "which")
				_, err := f.reverse(operator(), ReverseRequest{TransferID: completed[i].ID, ReasonCode: "PROPERTY_TEST", Reason: "random reversal"})
				switch {
				case err == nil:
					completed = append(completed[:i], completed[i+1:]...)
				case errs.CodeOf(err) == errs.CodeConflict:
					// The recipient spent the funds: refused, nothing changed.
				default:
					rt.Fatalf("reverse: %v", err)
				}
			} else {
				from := rapid.IntRange(0, n-1).Draw(rt, "from")
				to := (from + 1 + rapid.IntRange(0, n-2).Draw(rt, "to")) % n
				amount := rapid.Int64Range(1, 3_000_000).Draw(rt, "amount")
				res, err := f.send(cs[from].p, f.req(cs[from], cs[to], amount))
				switch {
				case err == nil:
					completed = append(completed, res.Transfer)
				case errs.CodeOf(err) == errs.CodeInsufficientBuyingPower:
				default:
					rt.Fatalf("send: %v", err)
				}
			}
			if got := f.walletSum(all...); !got.Equal(total) {
				rt.Fatalf("Σ WALLET is %s, funding was %s", got, total)
			}
			for _, a := range all {
				w := f.balance(a, ledger.CodeWallet)
				lhs := w.Add(f.balance(a, ledger.CodeTransferOut)).Sub(f.balance(a, ledger.CodeTransferIn)).Sub(f.balance(a, ledger.CodeCapital))
				if !lhs.IsZero() || w.IsNegative() {
					rt.Fatalf("party %s: WALLET %s, identity residue %s", a, w, lhs)
				}
			}
		}
	})
	rep, err := f.svc.Reconcile(f.ctx, testDB, f.asset.ID)
	require.NoError(t, err)
	require.True(t, rep.OK(), "%+v", rep.Discrepancies)
}
