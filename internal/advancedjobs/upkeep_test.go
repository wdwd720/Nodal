package advancedjobs

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/stableasset"
)

// TestSettleReport_EveryRequestAPassCouldNotSettleIsAFailureOfItsOwn: a
// stable-asset pass becomes a job report in which each request the pass
// could not act on is a failure of its own unit, named by what it is, and
// redacted; everything it did act on is counted.
func TestSettleReport_EveryRequestAPassCouldNotSettleIsAFailureOfItsOwn(t *testing.T) {
	t.Parallel()
	refusal := errs.New(errs.CodeReconciliationRequired, "the request's effect is UNKNOWN")
	internal := errors.New("pq: relation stable_mint_requests is on fire")
	rep := settleReport(stableasset.SettleReport{
		Examined: 7, Issued: 2, Redeemed: 1, Queued: 1, Failed: 1, Waiting: 1,
		Failures: []stableasset.RequestFailure{
			{Operation: stableasset.UpkeepSettleMint, RequestID: "m1", Attempts: 1, RetryAfter: time.Unix(60, 0), Err: refusal},
			{Operation: stableasset.UpkeepSettleBurn, RequestID: "b1", Attempts: 3, Err: internal},
			{Operation: stableasset.UpkeepPayRedemption, RequestID: "r1", Attempts: 1, Err: refusal},
		},
	})
	assert.Equal(t, 7, rep.Examined)
	assert.Equal(t, 5, rep.Changed, "issued, redeemed, queued and failed requests changed")
	assert.Equal(t, 1, rep.Deferred)
	require.Len(t, rep.Failures, 3)
	assert.Equal(t, "mint m1", rep.Failures[0].Unit)
	assert.Equal(t, string(errs.CodeReconciliationRequired), rep.Failures[0].Code)
	assert.Equal(t, "burn b1", rep.Failures[1].Unit)
	assert.Equal(t, string(errs.CodeInternal), rep.Failures[1].Code)
	assert.NotContains(t, rep.Failures[1].Reason, "on fire", "an internal error's text goes only to the log")
	assert.Equal(t, internal, rep.Failures[1].Err())
	assert.Equal(t, "redemption r1", rep.Failures[2].Unit)
	assert.False(t, rep.OK())
}

// TestFailPending_ASubjectIsReportedUnderTheUnitTheExpiryPassAlwaysUsed: a
// delegated subject a pass could not handle is a failure of "swap <id>" or
// "withdrawal <id>", whichever pass failed it.
func TestFailPending_ASubjectIsReportedUnderTheUnitTheExpiryPassAlwaysUsed(t *testing.T) {
	t.Parallel()
	var rep Report
	failPending(&rep, []delegation.PendingFailure{
		{Kind: delegation.SubjectSwap, SubjectID: "s1", Operation: delegation.UpkeepExpireSwap, Err: errors.New("x")},
		{Kind: delegation.SubjectWithdrawal, SubjectID: "w1", Operation: delegation.UpkeepObserveWithdrawal, Err: errors.New("y")},
	})
	require.Len(t, rep.Failures, 2)
	assert.Equal(t, "swap s1", rep.Failures[0].Unit)
	assert.Equal(t, "withdrawal w1", rep.Failures[1].Unit)
}
