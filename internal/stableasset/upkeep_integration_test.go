//go:build integration

package stableasset

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// poisonRow makes the database refuse every update of one row of table --
// at once, or only at COMMIT -- until the test ends: an item that can never
// be processed. It needs the schema owner, so it connects with the migration
// role.
func poisonRow(t *testing.T, table, rowID string, atCommit bool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testMigrateURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	name := "itest_poison_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err = conn.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '`+rowID+`'::uuid THEN
				RAISE EXCEPTION 'itest poison: % refuses every change', NEW.id;
			END IF;
			RETURN NEW;
		END $$`)
	require.NoError(t, err)
	trigger := `CREATE TRIGGER ` + name + ` BEFORE UPDATE ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	if atCommit {
		trigger = `CREATE CONSTRAINT TRIGGER ` + name + ` AFTER UPDATE ON ` + table +
			` DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	}
	_, err = conn.Exec(ctx, trigger)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, testMigrateURL)
		if err != nil {
			t.Errorf("drop poison: %v", err)
			return
		}
		defer func() { _ = c.Close(ctx) }()
		if _, err := c.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON `+table); err != nil {
			t.Errorf("drop poison trigger: %v", err)
		}
		if _, err := c.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`); err != nil {
			t.Errorf("drop poison function: %v", err)
		}
	})
}

// unknownMint leaves a fresh customer's mint SUBMISSION_UNKNOWN: its
// submission timed out but landed.
func (f *fixture) unknownMint(a PlatformStableAsset) MintRecord {
	f.t.Helper()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)
	require.NoError(f.t, f.host.TimeoutButLand(f.ctx, a.Chain))
	res, err := f.mint(c, a, 1_000_000)
	require.Error(f.t, err)
	require.Equal(f.t, MintSubmissionUnknown, res.Request.Status)
	return res.Request
}

func (f *fixture) finalize(a PlatformStableAsset) {
	f.t.Helper()
	_, err := f.host.ProduceBlocks(f.ctx, a.Chain, 1+itDepth)
	require.NoError(f.t, err)
}

func (f *fixture) mintStatus(rid MintRequestID) MintStatus {
	f.t.Helper()
	rec, err := f.svc.GetMint(f.ctx, testDB, rid)
	require.NoError(f.t, err)
	return rec.Status
}

func requestFailure(rep SettleReport, request string) (RequestFailure, bool) {
	for _, f := range rep.Failures {
		if f.RequestID == request {
			return f, true
		}
	}
	return RequestFailure{}, false
}

// TestIntegration_AMintOfAnotherNetworkIsNeverObservedOrSettledHere: a mint
// of another devnet network -- its outcome unknown, or submitted and waiting
// -- is never taken by this network's ReconcileUnknown or ObservePending
// (this host would read it as NOT_FOUND and could fail it while it stands
// final on its own chain), is refused by SettleMint without anything
// changing, and is settled by its own network.
func TestIntegration_AMintOfAnotherNetworkIsNeverObservedOrSettledHere(t *testing.T) {
	here := newFixture(t)
	there := newFixture(t)
	require.NotEqual(t, here.network, there.network)
	a := there.register()
	unknown := there.unknownMint(a)
	c := there.customer()
	there.fund(c, there.cash, 5_000_000)
	submitted, err := there.svc.Mint(c.ctx, there.mintRequest(c, a, 1_000_000, "elsewhere-"+uuid.NewString(), gates.ModeDevnet))
	require.NoError(t, err)
	require.Equal(t, MintSubmitted, submitted.Request.Status)
	there.finalize(a)
	transitions := func(rid MintRequestID) int {
		return here.count(`SELECT count(*) FROM stable_mint_request_transitions WHERE request_id = $1`, rid)
	}
	unknownBefore, submittedBefore := transitions(unknown.ID), transitions(submitted.Request.ID)

	rep, err := here.svc.ReconcileUnknown(here.ctx, 500)
	require.NoError(t, err)
	assert.Zero(t, rep.Examined, "another network's unknown mint is never taken: %+v", rep)
	rep, err = here.svc.ObservePending(here.ctx, 500)
	require.NoError(t, err)
	assert.Zero(t, rep.Examined, "nor its submitted one: %+v", rep)
	err = testDB.InTx(here.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := here.svc.SettleMint(ctx, tx, unknown.ID)
		return err
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "settling it here is refused: %v", err)
	err = testDB.InTx(here.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := here.svc.ReconcileSupply(ctx, tx, a.ID, reconciliation.ModePeriodic, operator)
		return err
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "so is reconciling its asset against this host's chain")
	assert.Equal(t, MintSubmissionUnknown, there.mintStatus(unknown.ID))
	assert.Equal(t, MintSubmitted, there.mintStatus(submitted.Request.ID))
	assert.Equal(t, unknownBefore, transitions(unknown.ID), "never observed or settled here")
	assert.Equal(t, submittedBefore, transitions(submitted.Request.ID))

	rep, err = there.svc.ReconcileUnknown(there.ctx, 500)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Issued)
	rep, err = there.svc.ObservePending(there.ctx, 500)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Issued)
	assert.Equal(t, MintIssued, there.mintStatus(unknown.ID), "its own network settles it")
	assert.Equal(t, MintIssued, there.mintStatus(submitted.Request.ID))
}

// TestIntegration_AMintThatCannotSettleNeverStopsTheOthers: one unknown mint
// the database refuses to settle plus N good ones. In one pass every good
// mint is issued and the poison is rolled back alone and reported with its
// backoff; a second pass settles a new unknown mint without retrying the
// poison inside its backoff; once the backoff has passed it is tried again,
// after the never-failed ones, and reported again.
func TestIntegration_AMintThatCannotSettleNeverStopsTheOthers(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	var mints []MintRecord
	for i := 0; i < 4; i++ {
		mints = append(mints, f.unknownMint(a))
	}
	f.finalize(a)
	poison := mints[0]
	poisonRow(t, "stable_mint_requests", poison.ID.String(), false)

	rep, err := f.svc.ReconcileUnknown(f.ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Issued, "every good mint is issued in the one pass")
	require.Len(t, rep.Failures, 1, "%+v", rep.Failures)
	failed := rep.Failures[0]
	assert.Equal(t, poison.ID.String(), failed.RequestID)
	assert.Equal(t, UpkeepSettleMint, failed.Operation)
	assert.Equal(t, 1, failed.Attempts)
	assert.True(t, failed.RetryAfter.Equal(f.clk.Now().Add(time.Minute)))
	assert.Equal(t, MintSubmissionUnknown, f.mintStatus(poison.ID), "rolled back alone")
	for _, m := range mints[1:] {
		assert.Equal(t, MintIssued, f.mintStatus(m.ID))
	}

	fresh := f.unknownMint(a)
	f.finalize(a)
	rep, err = f.svc.ReconcileUnknown(f.ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Issued)
	assert.Empty(t, rep.Failures, "not retried inside its backoff")
	assert.Equal(t, MintIssued, f.mintStatus(fresh.ID))

	f.clk.Advance(2 * time.Minute)
	rep, err = f.svc.ReconcileUnknown(f.ctx, 100)
	require.NoError(t, err)
	failed, ok := requestFailure(rep, poison.ID.String())
	require.True(t, ok, "tried again once its backoff passed")
	assert.Equal(t, 2, failed.Attempts)
	assert.True(t, failed.RetryAfter.Equal(f.clk.Now().Add(2*time.Minute)), "the backoff doubles")
}

// TestIntegration_ARedemptionThatCannotBePaidNeverStopsTheOthers: one queued
// redemption the database refuses to pay plus N good ones of the same asset.
// In one pass every good one is paid and the poison is rolled back alone and
// reported; a second pass pays a new queued redemption without retrying the
// poison inside its backoff.
func TestIntegration_ARedemptionThatCannotBePaidNeverStopsTheOthers(t *testing.T) {
	f := newFixture(t)
	a := f.register(func(d *Definition) { d.RedemptionMode = RedemptionQueued })
	// Every holder is issued before anything is burned: the admission rule
	// refuses a mint while a burned redemption waits for its payout.
	var holders []customer
	for i := 0; i < 4; i++ {
		c := f.customer()
		f.fund(c, f.cash, 1_000_000)
		f.mustMint(c, a, 1_000_000)
		holders = append(holders, c)
	}
	queued := func(c customer) BurnRecord {
		res, err := f.burn(c, a, 400_000)
		require.NoError(t, err)
		require.Equal(t, BurnRedemptionQueued, res.Request.Status)
		return res.Request
	}
	burns := []BurnRecord{queued(holders[0]), queued(holders[1]), queued(holders[2])}
	poison := burns[0]
	poisonRow(t, "stable_burn_requests", poison.ID.String(), true)

	rep, err := f.svc.ProcessRedemptions(f.ctx, a.ID, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Redeemed, "every good redemption is paid in the one pass")
	failed, ok := requestFailure(rep, poison.ID.String())
	require.True(t, ok, "%+v", rep.Failures)
	assert.Equal(t, UpkeepPayRedemption, failed.Operation)
	assert.Equal(t, 1, failed.Attempts)
	got, err := f.svc.GetBurn(f.ctx, testDB, poison.ID)
	require.NoError(t, err)
	assert.Equal(t, BurnRedemptionQueued, got.Status, "rolled back alone")

	fresh := queued(holders[3])
	rep, err = f.svc.ProcessRedemptions(f.ctx, a.ID, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Redeemed)
	assert.Empty(t, rep.Failures, "not retried inside its backoff")
	got, err = f.svc.GetBurn(f.ctx, testDB, fresh.ID)
	require.NoError(t, err)
	assert.Equal(t, BurnRedeemed, got.Status)
}

// TestIntegration_PeriodicSupplyReconciliationsLeaveOneMatchedRecord: N
// scheduled supply reconciliations of an asset whose books agree leave one
// MATCHED STABLE_RESERVE record, not N; an operator's FULL check is recorded
// every time.
func TestIntegration_PeriodicSupplyReconciliationsLeaveOneMatchedRecord(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)
	f.mustMint(c, a, 1_000_000)
	records := func() int {
		return f.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'STABLE_RESERVE' AND scope_id = $1`, a.ID.String())
	}
	first := f.reconcile(a)
	require.True(t, first.Matched())
	for i := 0; i < 4; i++ {
		f.clk.Advance(time.Minute) // within the prices' freshness: still a match
		again := f.reconcile(a)
		require.True(t, again.Matched())
		assert.Equal(t, first.RecordID, again.RecordID)
	}
	assert.Equal(t, 1, records(), "five periodic passes leave one MATCHED record")
	var full SupplyReconciliation
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		full, err = f.svc.ReconcileSupply(ctx, tx, a.ID, reconciliation.ModeFull, operator)
		return err
	}))
	assert.NotEqual(t, first.RecordID, full.RecordID, "an operator's FULL check is evidence of itself")
	assert.Equal(t, 2, records())
}
