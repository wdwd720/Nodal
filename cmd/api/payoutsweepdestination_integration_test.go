//go:build integration

package main

// A payout whose destination its holder removed is not submitted, and it does
// not stop the pass (F-263).
//
// payout.Service.Submit now refuses a destination that is not Usable(), inside
// the transaction that would claim the request. That refusal is the whole point
// -- a person who removes a destination because it was compromised has not
// stopped the value already reserved for it -- and it arrives at the sweep as
// INVALID_STATE_TRANSITION, which the pass already treats as "not this pass's
// problem" and skips.
//
// What this drives is the half that matters operationally: one stranded request
// must not abandon every later one. Each is an independent person's money.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider/payoutsandbox"
	"github.com/nodal/controlplane/internal/security"
)

func TestIntegration_ASweepSkipsAPayoutWhoseDestinationWasRemovedAndSubmitsTheRest(t *testing.T) {
	f := newPayoutSweepFixture(t)
	ctx := context.Background()

	// The request that will be stranded, reserved FIRST so it is the first the
	// pass reaches: a sweep that abandoned the rest at the first refusal would
	// pass a version of this test where the order ran the other way.
	stranded := f.reserve(t, 400)
	removed := f.destination

	// A second destination the holder keeps, and a request against it.
	var kept payout.DestinationID
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			dest, derr := f.svc.CreateDestination(ctx, tx, payout.Destination{
				AccountID: f.account, Kind: payout.DestinationBank,
				Provider: payoutsandbox.Name, ProviderReference: sandboxHandle(),
				DisplayLabel: "Second bank", Currency: "USD", Country: "US",
			})
			if derr != nil {
				return derr
			}
			if _, derr = f.svc.SetDestinationStatus(ctx, tx, dest.ID, payout.DestinationVerified); derr != nil {
				return derr
			}
			kept = dest.ID
			return nil
		}))
	f.destination = kept
	ordinary := f.reserve(t, 400)

	// The holder removes the first destination, which is the step-up-protected
	// act §25 calls high-risk and which somebody performs when a destination is
	// compromised.
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, removed, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType: security.ActorUser, ActorID: f.account.String(),
					Reason:     "this destination was compromised",
					OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}))

	// And the removal says what it stranded, which is what the DELETE route
	// renders as open_payout_ids.
	open, oerr := f.svc.OpenRequestsForDestination(ctx, f.db, removed)
	require.NoError(t, oerr)
	assert.Equal(t, []payout.RequestID{stranded.ID}, open,
		"the holder is told which request is now stuck, or their money goes quiet")

	f.clk.Advance(time.Second)
	f.sweep(t)

	assert.Equal(t, payout.StateVerified, f.stateOf(t, stranded.ID),
		"F-263: the sweep handed a provider the token the holder had just taken away")
	assert.Equal(t, payout.StateProviderPending, f.stateOf(t, ordinary.ID),
		"one stranded request must not abandon every later one; each is a different person's money")

	// The stranded request keeps its value reserved and no idempotency key was
	// committed for it, so nothing is in flight anywhere.
	var reserved, key string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT reserved_quantity::text, coalesce(provider_idempotency_key,'')
		   FROM payout_requests WHERE id = $1`, stranded.ID).Scan(&reserved, &key))
	assert.Equal(t, "400", reserved, "the value stays reserved; the refusal is not a cancellation")
	assert.Empty(t, key, "nothing was claimed, so there is nothing to reconcile")

	var calls int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM payout_provider_events WHERE request_id = $1`, stranded.ID).Scan(&calls))
	assert.Zero(t, calls, "the provider was never told about a destination that cannot receive value")

	// Repeating the pass is not a retry loop that eventually gets through.
	f.clk.Advance(time.Minute)
	f.sweep(t)
	assert.Equal(t, payout.StateVerified, f.stateOf(t, stranded.ID))

	// The customer-visible remedy ends it, and the value comes back.
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, cerr := f.svc.Cancel(ctx, tx, stranded.ID, "the destination it named was removed")
			return cerr
		}))
	assert.Equal(t, payout.StateRejected, f.stateOf(t, stranded.ID))
	open, oerr = f.svc.OpenRequestsForDestination(ctx, f.db, removed)
	require.NoError(t, oerr)
	assert.Empty(t, open)
}
