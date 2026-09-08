//go:build integration

package httpapi

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// TestIntegration_ConcurrentPurchasesDoNotStarveTheConnectionPool is F-27, and
// it is written against a pool of TWO on purpose.
//
// The defect: a capability check inside a financial transaction read the gate
// rows through the connection POOL. The caller already held a connection from
// that pool for its transaction, so every concurrent posting needed a SECOND
// connection while holding its first. With N concurrent postings and a pool of
// N, every connection was held by a transaction whose owner was waiting for a
// connection that would never be released. Nothing recovered until the
// statement timeout thirty seconds later.
//
// It was invisible to every existing test because every existing test supplies
// a MAP as the capability resolver. Only the production resolver reads the
// database, so only production could deadlock — and the internal marketplace
// could not commit a purchase at all (F-24, F-26), so nobody had ever run
// concurrent purchases against a real gate.
//
// A pool of two makes the failure certain rather than probable: with the bug,
// two concurrent purchases are enough. The deadline is short for the same
// reason — the broken version does not fail, it HANGS, so the test has to be
// what runs out of patience.
func TestIntegration_ConcurrentPurchasesDoNotStarveTheConnectionPool(t *testing.T) {
	appURL := testAppDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Two connections. One per concurrent purchase, and not one to spare.
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "pool-starvation-itest", MaxConns: 2, MinConns: 2})
	require.NoError(t, err)
	defer pool.Close()

	clk := clock.System()
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)

	// The REAL resolver, reading real gate rows. A map here would pass whatever
	// the code did, which is the whole reason this defect survived.
	resolver := dbCapabilityResolver{checker: checker, pool: pool}

	led := ledger.NewService(clk, "pool-starvation-itest")
	led.SetCapabilityResolver(resolver)
	credits := credit.NewService(led, clk)
	svc := commerce.NewService(led, credits, audit.NewWriter(), clk)
	svc.SetCapabilityResolver(resolver)

	// Two purchases at once, each in its own transaction, each asking the
	// resolver a question while it holds one.
	const concurrent = 2
	var (
		wg   sync.WaitGroup
		errs = make([]error, concurrent)
	)
	start := make(chan struct{})
	for i := range concurrent {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
				// The capability question, asked from inside the transaction.
				// This is the exact shape that deadlocked: with the bug it
				// reaches for a second connection and never gets one.
				_, qerr := resolver.ActiveConversionCapabilities(ctx, tx)
				return qerr
			})
		}(i)
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the capability check inside a transaction never returned: " +
			"every pool connection is held by a transaction waiting for another connection")
	}
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
}

// dbCapabilityResolver is the production shape: it answers from gate rows,
// through whichever querier the caller supplies.
type dbCapabilityResolver struct {
	checker *gates.Checker
	pool    db.Querier
}

var caps = []gates.Capability{
	gates.Capability(valuedomain.CapNativeMarketTrading),
	gates.Capability(valuedomain.CapPayoutReserve),
	gates.Marketplace,
}

func (r dbCapabilityResolver) ActiveConversionCapabilities(ctx context.Context, q db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return r.on(ctx, q)
}

func (r dbCapabilityResolver) ActiveCapabilities(ctx context.Context, q db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return r.on(ctx, q)
}

func (r dbCapabilityResolver) on(ctx context.Context, q db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	if q == nil {
		q = r.pool
	}
	verdicts, err := r.checker.ActiveSet(ctx, q, caps)
	if err != nil {
		return nil, err
	}
	out := make(map[valuedomain.CapabilityKey]bool, len(verdicts))
	for c, v := range verdicts {
		out[valuedomain.CapabilityKey(string(c))] = v.Active
	}
	return out, nil
}

// testAppDSN returns the integration database URL or skips.
func testAppDSN(t *testing.T) string {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set")
	}
	return url
}
