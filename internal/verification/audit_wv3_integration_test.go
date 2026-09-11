//go:build integration

package verification_test

// Reproduction for the THIRD round of the withdrawal-verification audit
// (goal §54). Nothing here changes product code.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// atomicProvider counts the calls it was handed, safely under concurrency.
type atomicProvider struct {
	*contractedProvider
	gets atomic.Int64
}

func (p *atomicProvider) Get(ctx context.Context, ref string) (verification.Result, error) {
	p.gets.Add(1)
	return p.contractedProvider.Get(ctx, ref)
}

// ---------------------------------------------------------------------------
// F-wv3-4 — the poll interval is a read-then-write with nothing between, so
// concurrent polls of one session all pass it and all call the provider.
//
// verification.Service.Poll reads session.ProviderPolledAt, compares it with
// the clock, and only then commits MarkProviderPolled in a transaction of its
// own. Every request that reads the row before that commit lands sees the OLD
// timestamp and proceeds. D-133's stated purpose is that "one signed-in person
// cannot make this deployment call an identity provider six hundred times a
// minute"; the General rate-limit class is 600/minute per principal, and a
// browser that fires them concurrently rather than in series gets one provider
// call per concurrent request.
//
// The existing TestIntegration_APollInsideTheMinimumIntervalCallsNobody polls
// fifty times IN SERIES, which is the case the read-then-write does handle.
// ---------------------------------------------------------------------------

func TestAuditWV3_ConcurrentPollsOfOneSessionShareTheInterval(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	userID, accountID := newAccount(t)

	inner := &contractedProvider{ref: "prov-ref-wv3-concurrent-" + accountID.String()}
	p := &atomicProvider{contractedProvider: inner}
	// A fixed clock, as the shipped interval test uses: every call below is at
	// one instant, which is what a person holding down refresh produces.
	svc := newConcurrentService(t, p, now)

	_, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)
	open, found, oerr := verification.NewRepository().OpenSession(ctx, testDB, userID)
	require.NoError(t, oerr)
	require.True(t, found)

	// The provider has not decided: the state this route is polled in.
	p.result = verification.Result{
		ProviderRef: inner.ref,
		Status:      verification.SessionProcessing,
		RawStatus:   "processing",
	}

	// Twenty tabs, or one client with a connection pool, inside one interval.
	const concurrent = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	errsSeen := make([]error, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, perr := svc.Poll(ctx, testDB, accountID, open.ID)
			errsSeen[i] = perr
		}(i)
	}
	close(start)
	wg.Wait()
	for i, perr := range errsSeen {
		require.NoError(t, perr, "poll %d failed", i)
	}

	assert.LessOrEqual(t, p.gets.Load(), int64(1),
		"F-wv3-4: %d concurrent polls of one session made %d calls to the identity provider. "+
			"D-133 bounds provider calls per session with a read of provider_polled_at "+
			"followed by a separate committed write, and nothing makes the pair atomic, so "+
			"every request that reads before the write commits passes the interval",
		concurrent, p.gets.Load())
}

// newConcurrentService mirrors newCountingService with the atomic provider.
func newConcurrentService(t *testing.T, p *atomicProvider, now time.Time) *verification.Service {
	t.Helper()
	reg := verification.NewRegistry(false)
	require.NoError(t, reg.Register(p))
	svc, err := verification.NewService(verification.Deps{
		Repo:        verification.NewRepository(),
		Compliance:  compliance.NewRepository(audit.NewWriter()),
		Providers:   reg,
		Clock:       fixedClock(now),
		Environment: "STAGING",
		SandboxTier: false,
	})
	require.NoError(t, err)
	return svc
}
