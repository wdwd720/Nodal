//go:build integration

package capital

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// PART 23 — concurrency torture test.
//
// Account with 10,000 USDC available; 100 concurrent agents each reserve
// 500 USDC; exactly 20 succeed, 80 fail with INSUFFICIENT_BUYING_POWER,
// reserved == 10,000, available == 0, nothing negative, no duplicate row.
// The iteration is repeated with randomized start jitter CP_TORTURE_ITERATIONS
// times (default 25 locally; CI runs 1000) under both transaction modes:
//
//   - read committed + row locks (db.InTx), the production path;
//   - SERIALIZABLE (db.Serializable), which turns every lock wait into a
//     40001 retry. Under 100-way contention db.Serializable's fixed budget
//     of 5 retries is exhausted for a share of callers; the harness then
//     resubmits exactly as an API client would after a 409/503, and reports
//     the resubmission count. The invariant is judged on definitive
//     outcomes only and must hold regardless of how many resubmissions it
//     took.
//
// The same test runs at envelope level: the wallet is over-funded and a
// $10,000 envelope is the binding constraint.

const (
	tortureGoroutines     = 100
	tortureUnitMinor      = 500_00           // $500.00 per reservation
	tortureBudgetMinor    = 10_000_00        // $10,000.00 envelope
	tortureBudgetUnits    = 10_000 * oneUSDC // 10,000 USDC wallet
	tortureExpectedWins   = 20
	tortureDefaultIters   = 25
	tortureIterationsEnv  = "CP_TORTURE_ITERATIONS"
	tortureMaxStartJitter = 3 * time.Millisecond
)

func tortureIterations(t *testing.T) int {
	t.Helper()
	if v := os.Getenv(tortureIterationsEnv); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err, tortureIterationsEnv)
		require.Positive(t, n, tortureIterationsEnv)
		return n
	}
	return tortureDefaultIters
}

type txMode struct {
	name string
	run  func(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error
}

func tortureModes() []txMode {
	return []txMode{
		{"read_committed_row_lock", func(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
			return testDB.InTx(ctx, db.TxOptions{MaxRetries: 3}, fn)
		}},
		{"serializable", testDB.Serializable},
	}
}

type tortureResult struct {
	successes    int64
	insufficient int64
	unexpected   int64
	resubmits    int64
	firstErr     error
}

func TestTorture_ReservationsNeverOversubscribe(t *testing.T) {
	requireEnv(t)
	iters := tortureIterations(t)
	for _, level := range []string{"asset", "envelope"} {
		for _, mode := range tortureModes() {
			t.Run(level+"/"+mode.name, func(t *testing.T) {
				var resubmits int64
				start := time.Now()
				for i := 0; i < iters; i++ {
					resubmits += runTortureIteration(t, mode, level, i)
				}
				t.Logf("%s/%s: %d iterations x %d concurrent reservations of $500 against $10,000 -> %d/%d every time; %d resubmissions after retry exhaustion; %s",
					level, mode.name, iters, tortureGoroutines, tortureExpectedWins, tortureGoroutines, resubmits, time.Since(start).Round(time.Millisecond))
			})
		}
	}
}

func runTortureIteration(t *testing.T, mode txMode, level string, iteration int) int64 {
	t.Helper()
	f := newFixture(t)
	var envID *EnvelopeID
	if level == "envelope" {
		f.seedWallet(f.accountID, qty(100*tortureBudgetUnits)) // wallet is not the constraint
		env := f.createEnvelope(f.accountID, tortureBudgetMinor)
		envID = &env.ID
	} else {
		f.seedWallet(f.accountID, qty(tortureBudgetUnits))
	}

	var res tortureResult
	var firstErrOnce sync.Once
	var wg sync.WaitGroup
	release := make(chan struct{})
	for g := 0; g < tortureGoroutines; g++ {
		req := f.request(f.accountID, usdcUnits, tortureUnitMinor, envID)
		rng := rand.New(rand.NewPCG(uint64(iteration), uint64(g))) //nolint:gosec // reproducible jitter, not security
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			time.Sleep(time.Duration(rng.Int64N(int64(tortureMaxStartJitter))))
			for {
				err := mode.run(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
					_, err := f.svc.Reserve(ctx, tx, req)
					return err
				})
				if errors.Is(err, db.ErrRetriesExhausted) || db.IsRetryable(err) {
					atomic.AddInt64(&res.resubmits, 1)
					time.Sleep(time.Duration(rng.Int64N(int64(2 * time.Millisecond))))
					continue
				}
				switch {
				case err == nil:
					atomic.AddInt64(&res.successes, 1)
				case errs.HasCode(err, errs.CodeInsufficientBuyingPower):
					atomic.AddInt64(&res.insufficient, 1)
				default:
					atomic.AddInt64(&res.unexpected, 1)
					firstErrOnce.Do(func() { res.firstErr = err })
				}
				return
			}
		}()
	}
	close(release)
	wg.Wait()

	label := level + "/" + mode.name + " iteration " + strconv.Itoa(iteration)
	require.Zerof(t, res.unexpected, "%s: unexpected errors, first: %v", label, res.firstErr)
	require.EqualValuesf(t, tortureExpectedWins, res.successes, "%s: successes", label)
	require.EqualValuesf(t, tortureGoroutines-tortureExpectedWins, res.insufficient, "%s: insufficient", label)

	// Database truth.
	var rows, active, distinctKeys int64
	var sumQty money.Quantity
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'ACTIVE'), count(DISTINCT idempotency_key), coalesce(sum(quantity), 0)::text
		FROM asset_reservations WHERE account_id = $1 AND asset_id = $2`, f.accountID, f.assetID).Scan(&rows, &active, &distinctKeys, &sumQty))
	assert.EqualValuesf(t, tortureExpectedWins, rows, "%s: reservation rows (no duplicates)", label)
	assert.EqualValuesf(t, tortureExpectedWins, active, "%s: active rows", label)
	assert.EqualValuesf(t, tortureExpectedWins, distinctKeys, "%s: distinct idempotency keys", label)
	assert.Equalf(t, qty(tortureExpectedWins*usdcUnits).String(), sumQty.String(), "%s: sum of reserved quantity", label)

	avail := f.availability(f.accountID)
	assert.Equalf(t, qty(tortureExpectedWins*usdcUnits).String(), avail.Reserved.String(), "%s: totals.reserved", label)
	assert.Falsef(t, avail.Reserved.IsNegative(), "%s: negative reserved", label)
	if level == "asset" {
		assert.Truef(t, avail.Available.IsZero(), "%s: available should be zero, got %s", label, avail.Available)
		assert.Equalf(t, qty(tortureBudgetUnits).String(), avail.WalletBalance.String(), "%s: wallet balance untouched", label)
	} else {
		e := f.envelope(*envID)
		assertUSD(t, 0, e.Available, label+": envelope available")
		assertUSD(t, tortureBudgetMinor, e.Reserved, label+": envelope reserved")
		assertUSD(t, 0, e.Deployed, label+": envelope deployed")
		assertUSD(t, tortureBudgetMinor, e.Allocation, label+": envelope allocation")
		assertConserved(t, e)
	}
	f.assertNoDrift()
	return atomic.LoadInt64(&res.resubmits)
}

// Under contention the envelope row and the totals row are locked in a fixed
// order, and Consume / Release / Expire race against Reserve without ever
// breaking the conservation identities.
func TestTorture_MixedOperationsConserveCapital(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(10_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 5_000_00)
	other := f.newAccount()
	f.seedWallet(other, qty(3_000*oneUSDC))

	const workers = 40
	const opsPerWorker = 25
	var wg sync.WaitGroup
	var unexpected int64
	var firstErr error
	var once sync.Once
	var mu sync.Mutex
	var known []ReservationID
	pick := func(rng *rand.Rand) (ReservationID, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(known) == 0 {
			return ReservationID{}, false
		}
		return known[rng.IntN(len(known))], true
	}
	remember := func(rid ReservationID) {
		mu.Lock()
		defer mu.Unlock()
		known = append(known, rid)
	}
	accept := func(err error, codes ...errs.Code) {
		if err == nil || errors.Is(err, db.ErrRetriesExhausted) || db.IsRetryable(err) {
			return
		}
		for _, c := range codes {
			if errs.HasCode(err, c) {
				return
			}
		}
		atomic.AddInt64(&unexpected, 1)
		once.Do(func() { firstErr = err })
	}
	for w := 0; w < workers; w++ {
		rng := rand.New(rand.NewPCG(7, uint64(w))) //nolint:gosec // reproducible, not security
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				account := f.accountID
				var eid *EnvelopeID
				if rng.IntN(3) == 0 {
					account = other
				} else if rng.IntN(2) == 0 {
					eid = &env.ID
				}
				switch rng.IntN(6) {
				case 0, 1, 2:
					req := f.request(account, rng.Int64N(800*oneUSDC)+1, rng.Int64N(400_00)+1, eid)
					req.TTL = time.Duration(rng.Int64N(int64(time.Minute))) + time.Second
					r, err := f.reserve(req)
					accept(err, errs.CodeInsufficientBuyingPower)
					if err == nil {
						remember(r.ID)
					}
				case 3:
					if rid, ok := pick(rng); ok {
						_, err := f.consume(rid, rng.Int64N(100*oneUSDC), rng.Int64N(50_00), "", rng.IntN(2) == 0)
						accept(err, errs.CodeInvalidStateTransition, errs.CodeValidationFailed, errs.CodeConflict)
					}
				case 4:
					if rid, ok := pick(rng); ok {
						_, err := f.release(rid, "torture")
						accept(err, errs.CodeInvalidStateTransition)
					}
				case 5:
					err := testDB.InTx(f.ctx, db.TxOptions{MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
						_, err := f.svc.ExpireDue(ctx, tx, f.clk.Now().Add(time.Duration(rng.Int64N(int64(2*time.Minute)))), 5)
						return err
					})
					accept(err)
				}
			}
		}()
	}
	wg.Wait()
	require.Zero(t, unexpected, "first unexpected error: %v", firstErr)

	for _, account := range []accounts.AccountID{f.accountID, other} {
		a := f.availability(account)
		assert.False(t, a.Reserved.IsNegative())
		assert.LessOrEqual(t, a.Reserved.Cmp(a.WalletBalance), 0, "never oversubscribed")
	}
	assertConserved(t, f.envelope(env.ID))
	f.assertNoDrift()
}
