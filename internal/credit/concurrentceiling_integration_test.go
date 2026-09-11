//go:build integration

package credit

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capacity"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// A ceiling that concurrent purchases can each pass is not a ceiling (F-96).
//
// StartPurchase used to say, in a comment: "The guard reads inside this
// transaction, so two concurrent purchases cannot both be admitted against the
// same headroom." Under READ COMMITTED that is false. `AdmitAmount` sums
// paid_amount_minor over credit_fundings, and a concurrent uncommitted INSERT
// is invisible to that sum -- so N transactions measured the same headroom and
// all N were admitted. There is no unique or exclusion constraint on
// credit_fundings that could have caught it afterwards.
//
// With the shipped numbers the arithmetic was eight simultaneous purchases of
// the full ceiling against a pool of eight connections: $16,000 admitted
// against a $2,000 cap.
//
// The window was not narrow either. The transaction stayed open across the
// outbound provider call, so it was hundreds of milliseconds wide, which is the
// other half of what this change fixed.
func TestIntegration_ConcurrentPurchasesCannotAllPassOneCeiling(t *testing.T) {
	const (
		workers = 8
		each    = int64(10_000)
	)
	// The ceiling is a GLOBAL sum over credit_fundings and this suite shares
	// one database, so the headroom three purchases need has to be measured
	// rather than assumed. It was assumed, which made this test a function of
	// how much money every test declared before it had left in flight.
	f := newPurchaseFixtureWithCeiling(t, atRiskMinor(t)+3*each)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		refusals []error
		start    = make(chan struct{})
	)
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			_, err := f.svcP.StartPurchase(f.ctx, testDB, StartPurchaseRequest{
				AccountID: f.account, Amount: money.USDFromMinor(each),
				Currency: "USD", IdempotencyKey: f.keyPrefix + ":race-" + string(rune('a'+n)),
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				accepted++
				return
			}
			refusals = append(refusals, err)
		}(i)
	}
	close(start)
	wg.Wait()

	// Three fit under the ceiling and five do not. The exact count is the
	// point: "some were refused" would pass against a guard that refused at
	// random.
	assert.Equal(t, 3, accepted,
		"the ceiling admitted %d purchases of %d against a ceiling of %d", accepted, each, 3*each)
	require.Len(t, refusals, workers-3)
	for _, err := range refusals {
		assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err), "refused for the wrong reason: %v", err)
	}

	// And the database agrees with the count, so this is not a guard that
	// refused after writing. Scoped to this fixture's own account, which is
	// fresh, so the shared database's other rows cannot flatter it.
	var atRisk int64
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT coalesce(sum(paid_amount_minor), 0)::bigint FROM credit_fundings WHERE account_id = $1`,
		f.account).Scan(&atRisk))
	assert.Equal(t, 3*each, atRisk, "money at risk passed the ceiling it was measured against")
}

// atRiskMinor is what internal/capacity would measure right now, over the whole
// database, using the same state list the guard uses.
func atRiskMinor(t *testing.T) int64 {
	t.Helper()
	requireEnv(t)
	var total int64
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT coalesce(sum(paid_amount_minor), 0)::bigint FROM credit_fundings WHERE state = ANY($1)`,
		capacity.AtRiskFundingStates()).Scan(&total))
	return total
}
