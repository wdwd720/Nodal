//go:build integration && chaos

package chaos

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// TestChaos_ClockJumpNeverDuplicatesOrResurrects is the "no duplicate
// financial effect and no silent divergence" invariant against a clock that
// moves the wrong way.
//
// A host clock jumping is not exotic: NTP steps it, a VM resumes from a
// snapshot with a stale clock, a container starts before time sync. The
// system injects clock.Clock everywhere precisely so this is testable, and
// D-033 found the real version of this defect in `scripts/seed` — an
// idempotent posting that carried the wall clock in its content, so every run
// produced different content under the same key and the ledger correctly
// refused it.
//
// Two properties, both about not trusting the clock:
//
//  1. A replay across a clock jump is ABSORBED, not doubled — provided the
//     caller does not put the wall clock into idempotent content. When it
//     does, the ledger must REFUSE it loudly rather than post twice; the
//     negative control proves that is what happens.
//  2. A terminal state is terminal. Once a reservation has EXPIRED, moving
//     the clock backwards must not resurrect it, and its capital must not
//     silently come back.
func TestChaos_ClockJumpNeverDuplicatesOrResurrects(t *testing.T) {
	requireEnv(t)

	w := newFinancialWorld(t, 10_000_000)
	ctx := w.ctx
	trustTheClock := chaosBreak(t, "clock_jump_trusted")

	// --- 1. a replay across a clock jump -------------------------------------

	// The correct pattern: the financial fact's effective instant is pinned
	// to the fact, never read from the wall clock at replay time.
	pinned := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	depositID := "chaos-clock-" + chaosToken()
	build := func() ledger.Posting {
		id := depositID
		if trustTheClock {
			// NEGATIVE CONTROL: the deposit id — and therefore the
			// idempotency KEY derived from it — moves with the wall clock, so
			// every replay looks like a brand new deposit and the money
			// really does post twice.
			//
			// Note what is NOT a usable control here: putting the clock in
			// the posting's CONTENT while keeping the key fixed. The ledger
			// refuses that as INVALID_IDEMPOTENCY_REUSE, so the invariant
			// still holds and the test passes either way. That version of
			// this control was written first and passed; it proved nothing,
			// and it is recorded here so nobody rediscovers it.
			id = depositID + "-" + w.clk.Now().Format(time.RFC3339Nano)
		}
		p, err := ledger.FundingSettledPosting(ledger.FundingInputs{
			AccountID: w.account, DepositID: id, AssetID: w.usdc,
			Quantity: money.QuantityFromInt64(2_000_000), EffectiveAt: pinned,
		})
		require.NoError(t, err)
		return p
	}

	postingKey := build().IdempotencyKey
	first, err := w.ledger.PostInTx(ctx, testDB, build())
	require.NoError(t, err, "the first posting must succeed")
	require.False(t, first.Existing)

	balanceAfterFirst := w.walletBalance(t)

	for _, jump := range []time.Duration{
		48 * time.Hour,        // forward: an NTP step, or a snapshot resumed late
		-96 * time.Hour,       // backward: the case that makes a naive replay look new
		-365 * 24 * time.Hour, // a badly wrong clock
	} {
		w.clk.Advance(jump)
		res, err := w.ledger.PostInTx(ctx, testDB, build())
		if err != nil {
			// The ledger refusing is the CORRECT outcome for a caller whose
			// idempotent content moved. It must never be a silent second
			// posting, and it must name the reuse rather than fail opaquely.
			assert.Equalf(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err),
				"a replay after a %s clock jump failed with %s; the only acceptable failure here is a named idempotency reuse",
				jump, errs.CodeOf(err))
			continue
		}
		assert.Truef(t, res.Existing,
			"a replay after a %s clock jump was treated as a NEW posting; the clock decided whether money moved", jump)
		assert.Equalf(t, first.TransactionID, res.TransactionID,
			"a replay after a %s clock jump produced a different transaction", jump)
	}

	assert.Equal(t, balanceAfterFirst.String(), w.walletBalance(t).String(),
		"the wallet balance moved across clock jumps that replayed one deposit")
	assert.Equal(t, 1, countPostingsByKey(t, postingKey),
		"the deposit posted more than once across clock jumps")
	requireNoDrift(t, w, "after the clock jumps")

	// --- 2. an expired reservation stays expired -----------------------------
	w.clk.Set(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	var reservation capital.Reservation
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		r, err := w.capital.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: w.account.String(), AssetID: w.usdc,
			Quantity: money.QuantityFromInt64(1_000_000), USDMinor: 100,
			ActorType: security.ActorUser, ActorID: "chaos-user",
			IdempotencyKey: "chaos-clock-res-" + chaosToken(), TTL: time.Hour, Reason: "clock",
		})
		reservation = r
		return err
	}))
	require.Equal(t, capital.ReservationActive, reservation.Status)
	heldWhileActive := w.activeReservations(t)
	require.NotEmpty(t, heldWhileActive)

	// Move past the TTL and expire it, exactly as the sweeper would.
	w.clk.Advance(2 * time.Hour)
	var expired []capital.ReservationID
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ids, err := w.capital.ExpireDue(ctx, tx, w.clk.Now(), 100)
		expired = ids
		return err
	}))
	require.Contains(t, expired, reservation.ID, "the reservation past its TTL was not expired")

	// Now the clock jumps BACKWARDS to before the reservation was even made.
	w.clk.Advance(-24 * time.Hour)
	after, err := w.capital.Get(ctx, testDB, reservation.ID)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationExpired, after.Status,
		"a backwards clock jump changed a terminal reservation state")

	// Re-running the sweeper with the rewound clock must not undo anything,
	// and the expired reservation must not reappear as active capital.
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.capital.ExpireDue(ctx, tx, w.clk.Now(), 100)
		return err
	}))
	still, err := w.capital.Get(ctx, testDB, reservation.ID)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationExpired, still.Status)
	for _, r := range w.activeReservations(t) {
		assert.NotEqual(t, reservation.ID, r.ID,
			"an EXPIRED reservation is holding capital again after the clock was rewound")
	}

	// And the terminal state must refuse a transition, whatever the clock says.
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.capital.Consume(ctx, tx, reservation.ID, money.QuantityFromInt64(1), 1, "")
		return err
	})
	require.Error(t, err, "an EXPIRED reservation was consumed after a backwards clock jump")
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err),
		"consuming an expired reservation must be refused as an invalid transition, got %s: %v", errs.CodeOf(err), err)
	requireNoDrift(t, w, "after the reservation clock jump")
}
