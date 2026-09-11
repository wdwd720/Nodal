//go:build integration

package payout_test

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
	"github.com/nodal/controlplane/internal/valuedomain"
)

// A conversion request cannot be created underneath an account closure (F-249).
//
// D-102 gave the closure decision the three financial blockers it needed, and
// read them as an aggregate. An aggregate read sees a snapshot, so under READ
// COMMITTED a Create committing between that read and the closure's own commit
// was invisible to both: the closure saw no open request, the payout saw an
// ACTIVE account, and the reservation landed on an account CLOSED a moment
// later -- value held out of the balance of somebody who can no longer sign in
// to cancel it, recoverable only by an operator.
//
// Both sides now name the same row. profile.Repository.LockHoldings takes FOR
// UPDATE on the person's accounts before it reads the blockers, and
// guardWithdraw takes FOR SHARE on the account before it reads the status. This
// drives the first half of that -- the payout waiting -- because that is the
// half that lives in this package, and it drives it against the real lock
// rather than against a comment.
func TestIntegration_ACreateWaitsForAnAccountLockTheClosureWouldHold(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	// The control: with nobody holding the row, the same request goes through.
	_, _, err := f.create(100, f.input())
	require.NoError(t, err)

	// Somebody else's transaction, holding the account row the way an EFFECT
	// decision holds it.
	holder, err := testDB.Pool().Begin(f.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(f.ctx) }()
	_, err = holder.Exec(f.ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, f.account)
	require.NoError(t, err)

	// A Create that would otherwise succeed, given a second to get past the
	// guard. It does not: it waits on the lock and the statement times out,
	// which is the observable form of "this cannot commit underneath a
	// closure".
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, terr := tx.Exec(ctx, `SET LOCAL statement_timeout = '1s'`); terr != nil {
				return terr
			}
			dest := f.destination
			quote, qerr := f.quoteThrough(ctx, tx, 100)
			if qerr != nil {
				return qerr
			}
			_, _, cerr := f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(100),
				ProviderTerms:      f.terms(),
				Environment:        "TEST",
				DisclosureAccepted: true,
				IdempotencyKey:     "closure-race-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.input())
			return cerr
		})
	require.Error(t, err,
		"F-249: a conversion request committed while a closure held the account row, so the "+
			"reservation landed on an account that was CLOSED a moment later")
	assert.Equal(t, "57014", db.SQLState(err),
		"the refusal must be the lock wait timing out, not something else: %v", err)

	// And nothing was written by the attempt.
	var n int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM payout_requests WHERE account_id = $1 AND idempotency_key LIKE 'closure-race-%'`,
		f.account).Scan(&n))
	assert.Zero(t, n)
}
