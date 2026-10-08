//go:build integration

package transfers

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_ConcurrentTransfersCannotDoubleSpend: 50 goroutines, each in
// its own transaction, each sending a tenth of the sender's balance. Exactly
// ten succeed, the other forty are refused for funds, the sender ends at
// exactly zero, and Σ WALLET is what it was.
func TestIntegration_ConcurrentTransfersCannotDoubleSpend(t *testing.T) {
	f := newFixture(t)
	alice := f.customer()
	recipients := []customer{f.customer(), f.customer(), f.customer(), f.customer(), f.customer()}
	const balance, n = 10_000_000, 50
	f.fund(alice.account, balance)
	all := []accounts.AccountID{alice.account}
	for _, r := range recipients {
		all = append(all, r.account)
	}

	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := range n {
		req := f.req(alice, recipients[i%len(recipients)], balance/10)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.send(alice.p, req)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	succeeded, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errs.CodeOf(err) == errs.CodeInsufficientBuyingPower:
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 10, succeeded, "exactly balance / (balance/10) transfers fit")
	assert.Equal(t, n-10, refused)
	assert.True(t, f.balance(alice.account, ledger.CodeWallet).IsZero())
	assert.Equal(t, fmt.Sprint(balance), f.walletSum(all...).String(), "Σ WALLET is conserved")
	assert.Equal(t, 10, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1 AND status = 'COMPLETED'`, alice.account))
	assert.Equal(t, n-10, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1 AND status = 'REJECTED'`, alice.account))
	for _, a := range all {
		f.requirePartyIdentity(a)
	}
	f.requireReconciled()
}

// TestIntegration_TransfersAndOtherSpendersShareOneBudget: transfers serialize
// on the sender's transfer lock, but another subsystem spending the same
// wallet does not take it. The capital reservation is what both go through,
// so together they still cannot spend more than the wallet holds.
func TestIntegration_TransfersAndOtherSpendersShareOneBudget(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	const balance, n = 10_000_000, 40
	f.fund(alice.account, balance)

	var wg sync.WaitGroup
	type outcome struct {
		transfer bool
		err      error
	}
	results := make(chan outcome, n)
	for i := range n {
		wg.Add(1)
		if i%2 == 0 {
			req := f.req(alice, bob, balance/10)
			go func() {
				defer wg.Done()
				_, err := f.send(alice.p, req)
				results <- outcome{true, err}
			}()
			continue
		}
		go func() {
			defer wg.Done()
			err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.cap.Reserve(ctx, tx, reserveFor(alice, f.asset.ID, balance/10))
				return err
			})
			results <- outcome{false, err}
		}()
	}
	wg.Wait()
	close(results)
	transfers, reservations := 0, 0
	for r := range results {
		switch {
		case r.err == nil && r.transfer:
			transfers++
		case r.err == nil:
			reservations++
		case errs.CodeOf(r.err) == errs.CodeInsufficientBuyingPower:
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	assert.Equal(t, 10, transfers+reservations, "transfers and reservations together fit the wallet exactly")
	wallet := f.balance(alice.account, ledger.CodeWallet)
	assert.Equal(t, fmt.Sprint(balance-transfers*(balance/10)), wallet.String())
	avail, err := f.cap.Availability(f.ctx, testDB, alice.account.String(), f.asset.ID, f.clk.Now())
	require.NoError(t, err)
	assert.True(t, avail.Available.IsZero(), "nothing left: %s", avail.Available)
	assert.Equal(t, wallet.String(), avail.Reserved.String(), "what is left is exactly what the other spender holds")
	f.requirePartyIdentity(alice.account)
	f.requirePartyIdentity(bob.account)
	f.requireReconciled()
}

// TestIntegration_HundredConcurrentTransfersPreserveInvariants is the goal's
// §32 target: 100 concurrent transfer attempts among ten customers, random
// pairs and amounts, each in its own transaction. Every attempt either
// completes or is refused for funds; afterwards Σ WALLET equals Σ funding,
// every per-party identity holds, no balance is negative and Reconcile is
// clean. The wall-clock duration is reported.
func TestIntegration_HundredConcurrentTransfersPreserveInvariants(t *testing.T) {
	f := newFixture(t)
	const parties, attempts, funding = 10, 100, 50_000_000
	cs := make([]customer, parties)
	all := make([]accounts.AccountID, parties)
	for i := range cs {
		cs[i] = f.customer()
		all[i] = cs[i].account
		f.fund(cs[i].account, funding)
	}
	rng := rand.New(rand.NewPCG(32, 100))
	reqs := make([]SendRequest, attempts)
	senders := make([]customer, attempts)
	for i := range reqs {
		from := rng.IntN(parties)
		to := (from + 1 + rng.IntN(parties-1)) % parties
		senders[i] = cs[from]
		reqs[i] = f.req(cs[from], cs[to], 1+rng.Int64N(20_000_000))
	}

	var wg sync.WaitGroup
	errsCh := make(chan error, attempts)
	start := time.Now()
	for i := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.send(senders[i].p, reqs[i])
			errsCh <- err
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(errsCh)
	completed, refused := 0, 0
	for err := range errsCh {
		switch {
		case err == nil:
			completed++
		case errs.CodeOf(err) == errs.CodeInsufficientBuyingPower:
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, attempts, completed+refused)
	assert.Equal(t, money.QuantityFromInt64(parties*funding).String(), f.walletSum(all...).String(), "Σ WALLET is conserved")
	for _, a := range all {
		f.requirePartyIdentity(a)
	}
	rep := f.requireReconciled()
	require.Len(t, rep.Assets, 1)
	assert.True(t, rep.Assets[0].WalletDelta.IsZero())
	t.Logf("§32: %d concurrent transfer attempts (%d completed, %d refused for funds) in %s", attempts, completed, refused, elapsed)
	fmt.Printf("TRANSFERS_P32_MEASUREMENT attempts=%d completed=%d refused=%d elapsed=%s\n", attempts, completed, refused, elapsed)
}
