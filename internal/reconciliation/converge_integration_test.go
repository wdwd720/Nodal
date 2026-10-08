//go:build integration

package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
)

// TestIntegration_PeriodicMatchesConvergeOnOneMatchedRecord: N scheduled
// passes that match leave one MATCHED record for their kind and scope instead
// of one per pass; an operator's FULL check is always kept as evidence of
// itself (and stands for the periodic passes after it); and after a mismatch
// is resolved, the first clean periodic pass is recorded and the later ones
// converge on it.
func TestIntegration_PeriodicMatchesConvergeOnOneMatchedRecord(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	repo := NewRepository(clk, event.NewOutbox(clk), audit.NewWriter())
	scope := id.New[id.Any]().String()
	req := func(mode Mode, status Status) OpenRequest {
		return OpenRequest{Kind: KindBookJournal, Mode: mode, ScopeType: "order_book", ScopeID: scope,
			Expected: map[string]string{"claim": "the journal replays to the book"}, Observed: map[string]string{"verified": "true"},
			Status: status, Material: status == StatusMismatch, Actor: SystemActor(), Reason: "converge itest"}
	}
	records := func() int {
		var n int
		require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM reconciliation_records WHERE kind = 'BOOK_JOURNAL' AND scope_id = $1`,
			scope).Scan(&n))
		return n
	}
	match := func(mode Mode) (Record, bool) {
		t.Helper()
		clk.Advance(time.Hour)
		var rec Record
		var opened bool
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, opened, err = repo.RecordMatch(ctx, tx, req(mode, StatusMatched))
			return err
		}))
		require.Equal(t, StatusMatched, rec.Status)
		return rec, opened
	}

	first, opened := match(ModePeriodic)
	require.True(t, opened, "the first matched pass is recorded")
	for i := 0; i < 5; i++ {
		again, opened := match(ModePeriodic)
		assert.False(t, opened, "pass %d", i+2)
		assert.Equal(t, first.ID, again.ID, "pass %d stands on the first record", i+2)
	}
	assert.Equal(t, 1, records(), "six periodic passes leave one MATCHED record")
	stored, err := repo.Get(ctx, d.Pool(), first.ID)
	require.NoError(t, err)
	assert.True(t, first.UpdatedAt.Equal(stored.UpdatedAt), "a matched record is never rewritten")

	full, opened := match(ModeFull)
	assert.True(t, opened, "an operator's FULL check is evidence of itself")
	assert.NotEqual(t, first.ID, full.ID)
	again, opened := match(ModePeriodic)
	assert.False(t, opened)
	assert.Equal(t, full.ID, again.ID, "the standing FULL match stands for the periodic pass after it")
	assert.Equal(t, 2, records())

	clk.Advance(time.Hour)
	var mismatch Record
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		mismatch, err = repo.Open(ctx, tx, req(ModePeriodic, StatusMismatch))
		return err
	}))
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Transition(ctx, tx, mismatch.ID, StatusResolvedAutomatic, TransitionEvidence{
			Actor: SystemActor(), Reason: "converge itest: the difference disappeared on re-observation"})
		return err
	}))
	clean, opened := match(ModePeriodic)
	assert.True(t, opened, "the first clean pass after a resolved mismatch is recorded")
	for i := 0; i < 3; i++ {
		again, opened := match(ModePeriodic)
		assert.False(t, opened)
		assert.Equal(t, clean.ID, again.ID)
	}
	assert.Equal(t, 4, records(), "matched, full, the resolved mismatch and the first clean pass after it")

	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := repo.RecordMatch(ctx, tx, req(ModePeriodic, StatusMismatch))
		return err
	})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "RecordMatch records matches only")
}
