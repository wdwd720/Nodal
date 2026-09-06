//go:build integration

package capital

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// acceptCodes fails the property unless err is nil or carries one of the
// expected codes. Retry exhaustion never happens sequentially; it is a
// failure here.
func acceptCodes(rt *rapid.T, op string, err error, codes ...errs.Code) {
	rt.Helper()
	if err == nil {
		return
	}
	for _, c := range codes {
		if errs.HasCode(err, c) {
			return
		}
	}
	rt.Fatalf("%s: unexpected error %v (allowed %v)", op, err, codes)
}

// TestProp_CapitalConserved drives a random sequence of reserve / consume /
// release / lock / expire / pnl / undeploy / hold operations across random
// accounts and envelopes and checks after the sequence that:
//
//   - available + reserved + deployed == allocation for every envelope and
//     reserved == Σ active reservation budget (VerifyEnvelopeBudgets);
//   - totals.reserved == Σ(quantity − consumed) of ACTIVE reservations for
//     every (account, asset) (VerifyReservationTotals);
//   - no reserved quantity exceeds the wallet balance (never oversubscribed);
//   - nothing is negative (also enforced by CHECK constraints: a violation
//     would have surfaced as an unexpected error).
func TestProp_CapitalConserved(t *testing.T) {
	requireEnv(t)
	rapid.Check(t, func(rt *rapid.T) {
		f := newFixture(rt)
		accountsN := rapid.IntRange(1, 2).Draw(rt, "accounts")
		accts := []accounts.AccountID{f.accountID}
		for len(accts) < accountsN {
			accts = append(accts, f.newAccount())
		}
		balances := map[accounts.AccountID]int64{}
		envelopes := map[accounts.AccountID]EnvelopeID{}
		for _, a := range accts {
			bal := rapid.Int64Range(1, 5_000*oneUSDC).Draw(rt, "balance")
			balances[a] = bal
			f.seedWallet(a, qty(bal))
			alloc := rapid.Int64Range(0, 2_000_00).Draw(rt, "allocation")
			envelopes[a] = f.createEnvelope(a, alloc, func(e *Envelope) {
				e.MaxDailyLoss = money.USDFromMinor(rapid.Int64Range(0, 500_00).Draw(rt, "max_daily_loss"))
				e.MaxDrawdown = money.USDFromMinor(rapid.Int64Range(0, 1_000_00).Draw(rt, "max_drawdown"))
			}).ID
		}

		var known []ReservationID
		pickRes := func() (ReservationID, bool) {
			if len(known) == 0 {
				return ReservationID{}, false
			}
			return known[rapid.IntRange(0, len(known)-1).Draw(rt, "reservation")], true
		}
		ops := rapid.IntRange(1, 30).Draw(rt, "ops")
		for i := 0; i < ops; i++ {
			account := accts[rapid.IntRange(0, len(accts)-1).Draw(rt, "account")]
			eid := envelopes[account]
			switch rapid.SampledFrom([]string{"reserve", "reserve", "reserve", "consume", "consume_final", "release", "lock", "expire", "pnl", "undeploy", "hold"}).Draw(rt, "op") {
			case "reserve":
				var env *EnvelopeID
				if rapid.Bool().Draw(rt, "with_envelope") {
					env = &eid
				}
				req := f.request(account, rapid.Int64Range(1, balances[account]).Draw(rt, "quantity"), rapid.Int64Range(0, 2_000_00).Draw(rt, "usd"), env)
				req.TTL = time.Duration(rapid.Int64Range(1, 300).Draw(rt, "ttl_seconds")) * time.Second
				r, err := f.reserve(req)
				acceptCodes(rt, "reserve", err, errs.CodeInsufficientBuyingPower)
				if err == nil {
					known = append(known, r.ID)
				}
			case "consume", "consume_final":
				rid, ok := pickRes()
				if !ok {
					continue
				}
				final := rapid.Bool().Draw(rt, "final")
				_, err := f.consume(rid, rapid.Int64Range(0, 1_000*oneUSDC).Draw(rt, "consume_qty"), rapid.Int64Range(0, 500_00).Draw(rt, "consume_usd"), uuid.NewString(), final)
				acceptCodes(rt, "consume", err, errs.CodeInvalidStateTransition, errs.CodeValidationFailed, errs.CodeConflict)
			case "release":
				rid, ok := pickRes()
				if !ok {
					continue
				}
				_, err := f.release(rid, "property")
				acceptCodes(rt, "release", err, errs.CodeInvalidStateTransition)
			case "lock":
				rid, ok := pickRes()
				if !ok {
					continue
				}
				acceptCodes(rt, "lock", f.lock(rid, uuid.NewString()), errs.CodeInvalidStateTransition, errs.CodeConflict)
			case "expire":
				f.clk.Advance(time.Duration(rapid.Int64Range(0, 400).Draw(rt, "advance_seconds")) * time.Second)
				f.expireDue(rapid.IntRange(1, 10).Draw(rt, "expire_limit"))
			case "pnl":
				_, err := f.applyPnL(eid, rapid.Int64Range(-300_00, 300_00).Draw(rt, "pnl"))
				acceptCodes(rt, "pnl", err)
			case "undeploy":
				err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
					_, err := f.svc.Undeploy(ctx, tx, eid, money.USDFromMinor(rapid.Int64Range(1, 200_00).Draw(rt, "undeploy")))
					return err
				})
				acceptCodes(rt, "undeploy", err, errs.CodeValidationFailed)
			case "hold":
				var exp *time.Time
				if rapid.Bool().Draw(rt, "hold_expires") {
					e := f.clk.Now().Add(time.Duration(rapid.Int64Range(1, 300).Draw(rt, "hold_ttl")) * time.Second)
					exp = &e
				}
				err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
					_, err := f.svc.PlaceHold(ctx, tx, WithdrawalHold{AccountID: account, AssetID: f.assetID, Quantity: qty(rapid.Int64Range(1, balances[account]).Draw(rt, "hold_qty")), Reason: "property", ExpiresAt: exp})
					return err
				})
				acceptCodes(rt, "hold", err)
			}
		}

		f.assertNoDrift()
		for _, a := range accts {
			av := f.availability(a)
			if av.Reserved.IsNegative() {
				rt.Fatalf("negative reserved for %s: %s", a, av.Reserved)
			}
			if av.Reserved.Cmp(av.WalletBalance) > 0 {
				rt.Fatalf("oversubscribed %s: reserved %s > balance %s", a, av.Reserved, av.WalletBalance)
			}
			e := f.envelope(envelopes[a])
			sum, err := e.Available.Add(e.Reserved)
			if err != nil {
				rt.Fatalf("sum: %v", err)
			}
			sum, err = sum.Add(e.Deployed)
			if err != nil {
				rt.Fatalf("sum: %v", err)
			}
			if !sum.Equal(e.Allocation) {
				rt.Fatalf("envelope %s: available %s + reserved %s + deployed %s != allocation %s", e.ID, e.Available, e.Reserved, e.Deployed, e.Allocation)
			}
			if e.Available.IsNegative() || e.Reserved.IsNegative() || e.Deployed.IsNegative() || e.RealizedLoss.IsNegative() || e.DailyLoss.IsNegative() || e.CurrentDrawdown.IsNegative() {
				rt.Fatalf("envelope %s has a negative counter: %+v", e.ID, e)
			}
		}
	})
}

// TestProp_ReservationsNeverOversubscribe: for a random balance and a random
// set of concurrent reservations of random sizes, the sum of the successful
// quantities never exceeds the balance, every failure is
// INSUFFICIENT_BUYING_POWER, and the totals row equals the successful sum
// (FINANCIAL_MODEL §8: no reservation exceeds available at commit).
func TestProp_ReservationsNeverOversubscribe(t *testing.T) {
	requireEnv(t)
	rapid.Check(t, func(rt *rapid.T) {
		f := newFixture(rt)
		balance := rapid.Int64Range(1, 1_000*oneUSDC).Draw(rt, "balance")
		f.seedWallet(f.accountID, qty(balance))
		n := rapid.IntRange(2, 30).Draw(rt, "concurrent")
		reqs := make([]ReserveRequest, n)
		for i := range reqs {
			reqs[i] = f.request(f.accountID, rapid.Int64Range(1, balance).Draw(rt, "quantity"), 0, nil)
		}
		results := make([]error, n)
		won := make([]bool, n)
		var wg sync.WaitGroup
		for i := range reqs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for {
					err := testDB.InTx(f.ctx, db.TxOptions{MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
						_, err := f.svc.Reserve(ctx, tx, reqs[i])
						return err
					})
					if errors.Is(err, db.ErrRetriesExhausted) || db.IsRetryable(err) {
						continue
					}
					results[i] = err
					won[i] = err == nil
					return
				}
			}(i)
		}
		wg.Wait()
		var sum int64
		for i, err := range results {
			if err != nil && !errs.HasCode(err, errs.CodeInsufficientBuyingPower) {
				rt.Fatalf("request %d: unexpected error %v", i, err)
			}
			if won[i] {
				q, _ := reqs[i].Quantity.Int64()
				sum += q
			}
		}
		if sum > balance {
			rt.Fatalf("oversubscribed: %d reserved of %d", sum, balance)
		}
		av := f.availability(f.accountID)
		if av.Reserved.String() != qty(sum).String() {
			rt.Fatalf("totals %s != successful sum %d", av.Reserved, sum)
		}
		// Every loser asked for more than what was left after the winners
		// committed is not guaranteed (order-dependent), but at least one
		// request must have succeeded whenever any request fit the balance.
		fits := false
		for _, r := range reqs {
			if q, _ := r.Quantity.Int64(); q <= balance {
				fits = true
			}
		}
		if fits && sum == 0 {
			rt.Fatalf("no reservation succeeded although at least one fit the balance %d", balance)
		}
		f.assertNoDrift()
	})
}
