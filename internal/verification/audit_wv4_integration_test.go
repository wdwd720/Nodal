//go:build integration

package verification_test

// Reproductions for the FOURTH round of the withdrawal-verification audit
// (goal §54). The round is narrow: it audits the THIRD round's fix rather than
// the area. Nothing here changes product code.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// ---------------------------------------------------------------------------
// F-274's fix, and the half of it the third round did not measure: the LOSERS
// of the claim must not be answered with something staler than the interval.
//
// `ClaimProviderPoll` is one `UPDATE … WHERE … RETURNING`, so exactly one
// concurrent statement can take an interval and the rest answer from the record
// — which is what the interval has always meant them to do. The question this
// round asks is what "the record" is worth. `Poll` reads the session BEFORE the
// claim and returns that read to a loser, so a loser can answer with a status
// the winner has since replaced.
//
// Two properties are checked. First, that the staleness is bounded by the race
// and not by the interval: once the winner's Ingest has committed, a poll taken
// after it is answered with the new status even though the interval has not
// elapsed, because `Status.Terminal()` and the row read both precede the claim.
// Second, that a terminal session short-circuits before the claim, so a decided
// verification is never held behind an interval somebody else is holding.
// ---------------------------------------------------------------------------

func TestAuditWV4_TheLosersOfAPollClaimAreNotStalePastTheInterval(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	userID, accountID := newAccount(t)

	inner := &contractedProvider{ref: "prov-ref-wv4-stale-" + accountID.String()}
	p := &atomicProvider{contractedProvider: inner}
	svc := newConcurrentService(t, p, now)

	_, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)
	open, found, oerr := verification.NewRepository().OpenSession(ctx, testDB, userID)
	require.NoError(t, oerr)
	require.True(t, found)

	// The provider has decided, and the first poll to claim the interval will
	// learn it. A fixed clock means no poll after this one can claim again.
	p.result = verification.Result{
		ProviderRef: inner.ref,
		Status:      verification.SessionApproved,
		RawStatus:   "approved",
	}

	const concurrent = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	answers := make([]verification.SessionStatus, concurrent)
	errsSeen := make([]error, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, perr := svc.Poll(ctx, testDB, accountID, open.ID)
			answers[i], errsSeen[i] = s.Status, perr
		}(i)
	}
	close(start)
	wg.Wait()
	for i, perr := range errsSeen {
		require.NoError(t, perr, "poll %d failed", i)
	}
	require.LessOrEqual(t, p.gets.Load(), int64(1),
		"fixture check: F-274's fix still bounds the provider calls at one per interval")

	// The claim has been taken and cannot be taken again on this clock. Every
	// poll from here is a LOSER, and every one of them must carry the decision
	// the winner recorded rather than the status they were started with.
	for i := 0; i < 5; i++ {
		s, perr := svc.Poll(ctx, testDB, accountID, open.ID)
		require.NoError(t, perr)
		assert.Equal(t, verification.SessionApproved, s.Status,
			"F-274: a poll that loses the claim answers from the record, and the record is read "+
				"fresh on every call. Answering with anything but the winner's result would hold "+
				"a decided verification behind an interval somebody else is holding")
	}
	assert.LessOrEqual(t, p.gets.Load(), int64(1),
		"and none of them called the provider again")

	// What the winner itself answered with. At most one loser can have been
	// answered with a pre-claim read, and only inside the race window.
	approved := 0
	for _, s := range answers {
		if s == verification.SessionApproved {
			approved++
		}
	}
	t.Logf("%d of %d concurrent polls answered APPROVED; %d provider calls",
		approved, concurrent, p.gets.Load())
	assert.Positive(t, approved,
		"the winner at least must carry the decision it fetched")
}
