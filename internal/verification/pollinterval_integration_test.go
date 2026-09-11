//go:build integration

package verification_test

// A poll inside the minimum interval answers from the record and calls nobody
// (D-133).
//
// GET /v1/me/verification/sessions/{id} called verification.Provider.Get on
// every request whose session was not terminal, in the General rate-limit class
// -- 600 a minute per principal on the deployment, 6000 under the browser
// suite. One signed-in person could make this deployment call an identity
// provider six hundred times a minute, against a contract whose pricing and
// rate limits are the provider's.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// countingProvider is contractedProvider with a tally of the calls it was
// actually handed, which is the quantity this decision is about.
type countingProvider struct {
	*contractedProvider
	gets int
}

func (p *countingProvider) Get(ctx context.Context, ref string) (verification.Result, error) {
	p.gets++
	return p.contractedProvider.Get(ctx, ref)
}

func TestIntegration_APollInsideTheMinimumIntervalCallsNobody(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	userID, accountID := newAccount(t)

	inner := &contractedProvider{ref: "prov-ref-pollinterval-" + accountID.String()}
	p := &countingProvider{contractedProvider: inner}
	// The clock is fixed, so every call below happens at the same instant as
	// far as the service is concerned. That is the worst case for the interval
	// and the exact case a person holding down refresh produces.
	svc := newCountingService(t, p, now)

	_, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)
	open, found, oerr := verification.NewRepository().OpenSession(ctx, testDB, userID)
	require.NoError(t, oerr)
	require.True(t, found)

	// The provider has not decided yet, which is the state this route is polled
	// in: the person is watching a spinner.
	p.result = verification.Result{
		ProviderRef: inner.ref,
		Status:      verification.SessionProcessing,
		RawStatus:   "processing",
	}

	first, err := svc.Poll(ctx, testDB, accountID, open.ID)
	require.NoError(t, err)
	assert.Equal(t, verification.SessionProcessing, first.Status)
	require.Equal(t, 1, p.gets, "the first poll has nothing recorded to answer from and must call")

	for i := 0; i < 50; i++ {
		again, perr := svc.Poll(ctx, testDB, accountID, open.ID)
		require.NoError(t, perr)
		assert.Equal(t, verification.SessionProcessing, again.Status,
			"a poll inside the interval still answers, with the status the last call recorded")
	}
	assert.Equal(t, 1, p.gets,
		"D-133: fifty polls made fifty provider calls; the interval is per SESSION, so two tabs "+
			"or two people share it")

	// And the record says when the call happened, which is what makes the
	// interval a property of the session rather than of whoever is asking.
	reread, rerr := verification.NewRepository().Session(ctx, testDB, open.ID)
	require.NoError(t, rerr)
	require.NotNil(t, reread.ProviderPolledAt)
	assert.WithinDuration(t, now, reread.ProviderPolledAt.UTC(), time.Minute)

	// Past the interval, the provider is asked again, and the new answer is
	// ingested rather than the cached one returned.
	later := newCountingService(t, p, now.Add(verification.PollMinimumInterval+time.Second))
	p.result = approvedResult(inner.ref, false)
	decided, err := later.Poll(ctx, testDB, accountID, open.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, p.gets, "past the interval the provider is asked")
	assert.Equal(t, verification.SessionApproved, decided.Status)

	// A terminal session is not polled at all, interval or no interval: there
	// is nothing left for a provider to say.
	final, err := later.Poll(ctx, testDB, accountID, open.ID)
	require.NoError(t, err)
	assert.Equal(t, verification.SessionApproved, final.Status)
	assert.Equal(t, 2, p.gets, "a decided session is never polled again")
}

// newCountingService is newContractedService with the counting wrapper, and
// with the clock it is given: the interval is read from the service's clock, so
// a test that needs to step past it constructs a second service rather than
// mutating a fixed one.
func newCountingService(t *testing.T, p *countingProvider, now time.Time) *verification.Service {
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
