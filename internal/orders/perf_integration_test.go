//go:build integration

package orders

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

type throughput struct {
	orders, refused, trades int
	elapsed                 time.Duration
	p50, p95, p99           time.Duration
}

func (r throughput) String() string {
	perSec := int64(r.orders) * int64(time.Second) / max(int64(r.elapsed), 1)
	return fmt.Sprintf("%d orders (%d refused) in %s: %d orders/sec; latency p50 %s p95 %s p99 %s; %d trades",
		r.orders, r.refused, r.elapsed.Round(time.Millisecond), perSec, r.p50.Round(time.Millisecond),
		r.p95.Round(time.Millisecond), r.p99.Round(time.Millisecond), r.trades)
}

// drive submits workers x each orders to one book, one goroutine per funded
// account, through submit, and measures every submission.
func (f *fixture) drive(m spotMarket, accts []accounts.AccountID, each int, submit func(OrderRequest) error) throughput {
	f.t.Helper()
	var mu sync.Mutex
	var latencies []time.Duration
	var unexpected []error
	refused := 0
	var wg sync.WaitGroup
	start := time.Now()
	for w, acct := range accts {
		wg.Add(1)
		go func(w int, acct accounts.AccountID) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				side, cents := SideBuy, 9_995+(w*31+i*17)%10
				if (w+i)%2 == 0 {
					side, cents = SideSell, 9_996+(w*13+i*7)%10
				}
				r := f.limit(m, acct, side, fmt.Sprintf("%d.%02d", cents/100, cents%100), money.QuantityFromInt64(int64(1+(w+i)%5)*10_000))
				t0 := time.Now()
				err := submit(r)
				d := time.Since(t0)
				mu.Lock()
				latencies = append(latencies, d)
				if err != nil {
					refused++
					if !isBusinessRefusal(err) {
						unexpected = append(unexpected, err)
					}
				}
				mu.Unlock()
			}
		}(w, acct)
	}
	wg.Wait()
	elapsed := time.Since(start)
	for _, err := range unexpected {
		require.NoError(f.t, err)
	}
	slices.Sort(latencies)
	pct := func(p int) time.Duration { return latencies[(len(latencies)-1)*p/100] }
	return throughput{orders: len(latencies), refused: refused, elapsed: elapsed, p50: pct(50), p95: pct(95), p99: pct(99),
		trades: f.count(`SELECT count(*) FROM book_trades WHERE instrument_id = $1`, m.inst)}
}

// isBusinessRefusal reports whether err is a recorded refusal rather than a
// failure of the system.
func isBusinessRefusal(err error) bool {
	if ReasonOf(err) != "" {
		return true
	}
	switch errs.CodeOf(err) {
	case errs.CodeRiskRejected, errs.CodeInsufficientBuyingPower:
		return true
	}
	return false
}

// TestIntegration_ThroughputOfTheServiceOnOneBook measures orders through the
// whole service -- intent, settlement compiler, capability, Risk Kernel V2,
// capital reservation, effect, book lock, engine, journal, trades, ledger,
// evidence -- on one book from sixteen concurrent accounts, once through the
// staged pipeline (one transaction per stage) and once through the atomic
// single-transaction Submit, and then proves both journals replay to their
// live state. The engine alone is measured by internal/matching's
// TestLoad_100kOrders and BenchmarkMatchingMix.
func TestIntegration_ThroughputOfTheServiceOnOneBook(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput measurement skipped in -short mode")
	}
	f := newFixture(t)
	const workers, each = 16, 150

	staged := f.spotMarket(marketOpts{makerBPS: 5, takerBPS: 10, snapshotInterval: 256})
	res := f.drive(staged, f.fundedAccounts(staged, workers), each, func(r OrderRequest) error {
		_, err := f.pipe.Submit(f.ctx, r)
		return err
	})
	t.Logf("staged pipeline: %s", res)
	require.GreaterOrEqual(t, res.orders, 2_000)

	atomic := f.spotMarket(marketOpts{makerBPS: 5, takerBPS: 10, snapshotInterval: 256})
	res2 := f.drive(atomic, f.fundedAccounts(atomic, workers), each, func(r OrderRequest) error {
		var out StageOutcome
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.svc.submitAtomic(ctx, tx, r)
			return err
		})
		if err != nil {
			return err
		}
		return out.Rejection
	})
	t.Logf("single-transaction submit: %s", res2)

	for _, m := range []spotMarket{staged, atomic} {
		start := time.Now()
		proof, err := f.svc.VerifyJournal(f.ctx, testDB, m.inst)
		require.NoError(t, err)
		live, err := f.svc.GetBook(f.ctx, testDB, m.inst)
		require.NoError(t, err)
		require.Equal(t, live.StateHash, proof.FullReplayHash)
		require.Equal(t, live.StateHash, proof.SnapshotReplayHash)
		t.Logf("journal of %d commands, %d events, %d trades replayed from the database in %s",
			proof.Commands, proof.Events, proof.Trades, time.Since(start).Round(time.Millisecond))
		f.requireNoCrossedBook(m)
	}
}
