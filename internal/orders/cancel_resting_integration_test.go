//go:build integration

package orders

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_CancelRestingCancelsEveryWorkingOrderAndReleasesItsFunds:
// what a prediction market needs at close, void and settlement
// (predmarket.OutcomeBooks): every working order on the instrument's book is
// cancelled as a book command and its reservation released; a second call
// cancels nothing; an agent may not cancel in bulk and a reason is required.
func TestIntegration_CancelRestingCancelsEveryWorkingOrderAndReleasesItsFunds(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	seller, buyer := f.account(), f.account()
	f.fund(seller, m.base.ID, 10_000_000)
	f.fund(buyer, m.quote.ID, 1_000_000_000)
	ask := f.submit(f.limit(m, seller, SideSell, "101.00", units(2)))
	bid := f.submit(f.limit(m, buyer, SideBuy, "99.00", units(2)))
	require.Equal(t, StatusOpen, ask.Order.Status)
	require.Equal(t, StatusOpen, bid.Order.Status)

	cancel := func(by econ.Actor, reason string) (int, error) {
		var n int
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			n, err = f.svc.CancelResting(ctx, tx, []instruments.InstrumentID{m.inst, instruments.NewInstrumentID()}, by, reason)
			return err
		})
		return n, err
	}
	_, err := cancel(econ.Actor{Type: security.ActorAgent, ID: "agent-1"}, "the market closed")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "an agent never cancels in bulk")
	_, err = cancel(systemActor, " ")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a reason is required")

	n, err := cancel(systemActor, "the market closed")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both working orders; the instrument with no book is skipped")
	for _, oid := range []OrderID{ask.Order.ID, bid.Order.ID} {
		o, err := f.svc.Order(f.ctx, testDB, oid)
		require.NoError(t, err)
		assert.Equal(t, StatusCancelled, o.Status)
		var status string
		require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status FROM asset_reservations WHERE id = $1`, o.ReservationID).Scan(&status))
		assert.Equal(t, "RELEASED", status)
	}
	again, err := cancel(systemActor, "the market closed")
	require.NoError(t, err)
	assert.Zero(t, again, "nothing is left to cancel")

	proof, err := f.svc.VerifyJournal(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	assert.Equal(t, proof.StateHash, proof.FullReplayHash, "the cancels are journaled commands that replay")
}
