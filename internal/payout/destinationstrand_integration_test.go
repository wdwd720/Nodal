//go:build integration

package payout_test

// The other half of F-263: refusing to submit to a destination that is no
// longer usable leaves value reserved, so the act that made it unusable has to
// say what it stranded.
//
// TestAuditWV2_DisablingADestinationStopsAPayoutReservedForIt (the auditor's
// reproduction) proves the refusal. This proves the answer.

import (
	"context"
	"testing"

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

func TestIntegration_DisablingADestinationNamesTheRequestsItStranded(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 4_000_000_000)
	dest := f.destination

	// Nothing points at it yet, and the answer is an empty list rather than an
	// error: a person who removes a destination they never used is told
	// nothing, not told something went wrong.
	open, err := f.svc.OpenRequestsForDestination(f.ctx, testDB, dest)
	require.NoError(t, err)
	assert.Empty(t, open)

	reserve := func(key string) payout.Request {
		t.Helper()
		quote, qerr := f.quote(500_000_000)
		require.NoError(t, qerr)
		var req payout.Request
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				var cerr error
				req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
					AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
					Quantity:           money.QuantityFromInt64(500_000_000),
					ProviderTerms:      f.terms(),
					Environment:        "TEST",
					DisclosureAccepted: true,
					IdempotencyKey:     key + "-" + uuid.NewString(),
					EffectiveAt:        f.clk.Now(),
				}, f.sandboxInput())
				return cerr
			}))
		require.Equal(t, payout.StateVerified, req.State)
		return req
	}

	held := reserve("strand-held")
	cancelled := reserve("strand-cancelled")
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, cerr := f.svc.Cancel(ctx, tx, cancelled.ID, "the holder changed their mind")
			return cerr
		}))

	// Both are the same account's and both name the same destination; only the
	// one that still holds value is stranded by a removal.
	open, err = f.svc.OpenRequestsForDestination(f.ctx, testDB, dest)
	require.NoError(t, err)
	assert.Equal(t, []payout.RequestID{held.ID}, open,
		"a cancelled request is terminal and nothing about it is stranded")

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, dest, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType: security.ActorUser, ActorID: f.account.String(),
					Reason:     "this destination was compromised",
					OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}))

	// The removal is not refused for having an open request. §25 makes removing
	// a destination the act of somebody whose destination is compromised, and a
	// removal an attacker can block by starting a payout is not a control.
	after, gerr := f.svc.Destination(f.ctx, testDB, dest)
	require.NoError(t, gerr)
	require.False(t, after.Status.Usable())

	open, err = f.svc.OpenRequestsForDestination(f.ctx, testDB, dest)
	require.NoError(t, err)
	require.Equal(t, []payout.RequestID{held.ID}, open,
		"the request the removal stranded is the one the holder has to be told about")

	// And it is still reserved, which is why being told matters: the value is
	// out of the balance until they cancel it.
	stranded, sgerr := f.svc.Get(f.ctx, testDB, held.ID)
	require.NoError(t, sgerr)
	assert.Equal(t, payout.StateVerified, stranded.State)
	assert.Equal(t, "500000000", stranded.ReservedQuantity.String())

	// The customer-visible remedy works on it, and afterwards nothing is
	// stranded and nothing is reserved.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, cerr := f.svc.Cancel(ctx, tx, held.ID, "the destination it named was removed")
			return cerr
		}))
	open, err = f.svc.OpenRequestsForDestination(f.ctx, testDB, dest)
	require.NoError(t, err)
	assert.Empty(t, open)
	returned, rerr := f.svc.Get(f.ctx, testDB, held.ID)
	require.NoError(t, rerr)
	assert.Equal(t, "0", returned.ReservedQuantity.String())
}
