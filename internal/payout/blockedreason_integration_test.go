//go:build integration

package payout_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// A payout refused at submit says why, where its holder can read it (F-277).
//
// F-263 made Submit refuse a request whose destination has stopped being
// usable, inside the claim transaction and before the transition to SUBMITTED.
// That is the right shape: the transaction rolls back, the request stays
// VERIFIED with its value reserved, and moving it to FAILED would return the
// reservation on the strength of a fact the person can undo. What it left is a
// person with nothing to read. The request says VERIFIED, which reads as "on
// its way"; the refusal is an error the sweep logs for an operator; and the
// only customer-facing mention was a field on the DELETE response that disabled
// the destination, which is gone as soon as the page is.
//
// So the refusal is recorded on the request and the holder is told once.

// countingNotifier is what cmd/api wires a notification producer into.
type countingNotifier struct {
	notices []payout.BlockedNotice
}

func (n *countingNotifier) PayoutBlocked(_ context.Context, _ pgx.Tx, in payout.BlockedNotice) error {
	n.notices = append(n.notices, in)
	return nil
}

func TestIntegration_APayoutRefusedAtSubmitSaysWhyAndTellsItsHolder(t *testing.T) {
	f := newAuditFixture(t)
	notifier := &countingNotifier{}
	f.svc.SetNotifier(notifier)

	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)
	req := f.createPayout(t, 500_000_000, "blocked-reason")
	require.Equal(t, payout.StateVerified, req.State)
	require.Equal(t, "500000000", req.ReservedQuantity.String())
	require.Empty(t, req.BlockedReason, "fixture check: nothing is blocking it yet")

	// The holder removes the destination, which is what strands the request.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, f.destination, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType: security.ActorUser, ActorID: f.account.String(),
					Reason: "this destination was compromised", OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}))

	_, serr := f.svc.Submit(f.ctx, testDB, req.ID, f.provider.Name())
	require.Error(t, serr, "fixture check: F-263's refusal still fires")

	after, gerr := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, gerr)
	assert.Equal(t, payout.StateVerified, after.State,
		"the request stays reserved: failing it would return the reservation on a fact the person can undo")
	assert.Equal(t, "500000000", after.ReservedQuantity.String())
	assert.NotEmpty(t, after.BlockedReason,
		"F-277: the payout was refused, kept its holder's value reserved, and recorded no reason "+
			"anybody but an operator reading a log could see")
	assert.True(t, after.Blocked())
	assert.Contains(t, after.BlockedReason, "Cancel it",
		"the reason names the action, because a reason with no action is a dead end")
	assert.Empty(t, after.FailureReason,
		"and it is not a failure: failure_reason is what the notification follower renders as "+
			"'your withdrawal failed', which is not what happened")
	require.NotNil(t, after.BlockedAt)
	assert.WithinDuration(t, f.clk.Now().UTC(), after.BlockedAt.UTC(), time.Minute)

	require.Len(t, notifier.notices, 1,
		"F-277: the holder was never told; the only mention was on the response that removed the destination")
	assert.Equal(t, req.ID, notifier.notices[0].RequestID)
	assert.Equal(t, after.BlockedReason, notifier.notices[0].Reason)
	assert.Contains(t, notifier.notices[0].Occurrence, req.ID.String(),
		"the notice is keyed on the request so a re-read cannot duplicate it")

	// Every later sweep pass re-reads the same request and refuses it again.
	// The reason is unchanged, so nothing is rewritten and nobody is told twice:
	// a person whose destination was removed on Friday must not find four
	// thousand copies of this on Monday.
	for i := 0; i < 3; i++ {
		_, again := f.svc.Submit(f.ctx, testDB, req.ID, f.provider.Name())
		require.Error(t, again)
	}
	assert.Len(t, notifier.notices, 1, "F-277: the sweep told the holder once per pass")
	stable, gerr := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, gerr)
	assert.Equal(t, after.BlockedAt.UTC(), stable.BlockedAt.UTC(),
		"and the instant it was recorded did not move")

	// Cancelling is what releases the value, and it still works with a reason
	// recorded against the request.
	var cancelled payout.Request
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			cancelled, cerr = f.svc.Cancel(ctx, tx, req.ID, "the destination is gone")
			return cerr
		}))
	assert.Equal(t, payout.StateRejected, cancelled.State,
		"cancelling moves a payout to REJECTED and returns the exact lot slices it reserved")
	assert.Equal(t, "0", cancelled.ReservedQuantity.String())
	assert.NotEmpty(t, cancelled.BlockedReason,
		"the reason stays on the request, because why it could not be sent is part of its history")
}

// A deployment that has not wired a notifier still records the reason and still
// refuses. The notice is how somebody who is NOT looking at the page finds out;
// it is not what makes the refusal safe.
func TestIntegration_ABlockedPayoutRecordsItsReasonWithNoNotifierWired(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)

	quote, qerr := f.quote(400_000_000)
	require.NoError(t, qerr)
	dest := f.destination
	var req payout.Request
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(400_000_000),
				ProviderTerms:      f.terms(),
				Environment:        "TEST",
				DisclosureAccepted: true,
				IdempotencyKey:     "blocked-nonotifier-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return cerr
		}))
	require.Equal(t, payout.StateVerified, req.State)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, dest, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType: security.ActorUser, ActorID: f.account.String(),
					Reason: "removed", OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}))
	_, serr := f.svc.Submit(f.ctx, testDB, req.ID, f.provider.Name())
	require.Error(t, serr)

	after, gerr := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, gerr)
	assert.NotEmpty(t, after.BlockedReason)
	assert.Equal(t, payout.StateVerified, after.State)
}
