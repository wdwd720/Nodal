//go:build integration

package stableasset

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// TestIntegration_ABurnCannotExceedTheBalance: a burn is bounded by the
// holder's ledger balance (the capital reservation and the risk kernel) and
// by its balance on chain, and concurrent burns cannot both spend it.
func TestIntegration_ABurnCannotExceedTheBalance(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)
	f.mustMint(c, a, 1_000_000)

	_, err := f.burn(c, a, 1_000_001)
	require.Error(t, err)
	assert.Zero(t, f.count(`SELECT count(*) FROM stable_burn_requests WHERE account_id = $1`, c.id), "nothing was signed or submitted")

	res, err := f.burn(c, a, 600_000)
	require.NoError(t, err)
	require.Equal(t, BurnRedeemed, res.Request.Status, "failure %q", res.Request.FailureCode)
	assert.Equal(t, "400000", f.wallet(c, a.AssetID))
	assert.Equal(t, "4600000", f.wallet(c, f.cash), "redeemed at par into the cash reserve asset")
	assert.Equal(t, uint64(400_000), f.chainBalance(c, a))
	assert.Equal(t, uint64(400_000), f.chainSupply(a))
	assert.Equal(t, "400000", f.balance(a.account(ledger.CodeStableReserve, f.cash)))

	_, err = f.burn(c, a, 400_001)
	require.Error(t, err)

	// The chain holds less than the books say: the chain balance bounds the
	// burn even though the ledger would allow it.
	f.outOfBandBurn(c, a, 100_000)
	_, err = f.burn(c, a, 350_000)
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, RefusalChainBalanceInsufficient, e.Fields["reason"])

	// Two concurrent burns of a balance that covers only one.
	var wg sync.WaitGroup
	errsSeen := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.burn(c, a, 200_000)
			errsSeen <- err
		}()
	}
	wg.Wait()
	close(errsSeen)
	ok := 0
	for err := range errsSeen {
		if err == nil {
			ok++
		}
	}
	assert.Equal(t, 1, ok, "exactly one of two competing burns proceeds")
	assert.Equal(t, uint64(100_000), f.chainBalance(c, a))
}

// TestIntegration_ARedemptionIsTheExactInverseOfIssuance: minting then
// burning everything returns the customer's reserve and leaves the issuer
// with no reserve and no supply, on the books and on chain.
func TestIntegration_ARedemptionIsTheExactInverseOfIssuance(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 3_000_000)
	f.mustMint(c, a, 2_000_000)
	res, err := f.burn(c, a, 2_000_000)
	require.NoError(t, err)
	require.Equal(t, BurnRedeemed, res.Request.Status)

	assert.Equal(t, "3000000", f.wallet(c, f.cash))
	assert.Equal(t, "0", f.wallet(c, a.AssetID))
	assert.Equal(t, "0", f.balance(a.account(ledger.CodeStableReserve, f.cash)))
	assert.Equal(t, "0", f.balance(a.account(ledger.CodeStableReserveFunding, f.cash)))
	assert.Equal(t, "0", f.balance(a.account(ledger.CodeStableOutstanding, a.AssetID)))
	assert.Equal(t, "0", f.balance(a.account(ledger.CodeStableIssued, a.AssetID)))
	assert.Zero(t, f.chainSupply(a))
	var kind string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT kind FROM journal_transactions WHERE id = $1`, res.Request.JournalTransactionID).Scan(&kind))
	assert.Equal(t, string(ledger.KindStableRedemption), kind)
	assert.True(t, f.reconcile(a).Matched())
}

// TestIntegration_AQueuedRedemptionIsBurnedFirstAndPaidLater: in QUEUED mode
// a final burn waits for its payout; meanwhile the books and the chain still
// reconcile, because the queued burn is the honest difference.
func TestIntegration_AQueuedRedemptionIsBurnedFirstAndPaidLater(t *testing.T) {
	f := newFixture(t)
	a := f.register(func(d *Definition) { d.RedemptionMode = RedemptionQueued })
	c := f.customer()
	f.fund(c, f.cash, 1_000_000)
	f.mustMint(c, a, 1_000_000)

	res, err := f.burn(c, a, 400_000)
	require.NoError(t, err)
	require.Equal(t, BurnRedemptionQueued, res.Request.Status)
	assert.Equal(t, uint64(600_000), f.chainSupply(a), "burned on chain")
	assert.Equal(t, "1000000", f.balance(a.account(ledger.CodeStableOutstanding, a.AssetID)), "not yet redeemed on the books")
	r := f.reconcile(a)
	assert.True(t, r.Matched(), "%+v", r)
	assert.Equal(t, "400000", r.LandedUnpostedBurns.String())
	rl := f.reserveLedger(a)
	assert.Equal(t, "400000", sumQuantity(rl.PendingRedemption, f.cash).String())

	rep, err := f.svc.ProcessRedemptions(f.ctx, a.ID, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Redeemed)
	assert.Equal(t, "600000", f.balance(a.account(ledger.CodeStableOutstanding, a.AssetID)))
	assert.Equal(t, "400000", f.wallet(c, f.cash))
	assert.True(t, f.reconcile(a).Matched())
}

// TestIntegration_ReconciliationDetectsAnOutOfBandMint: units minted on chain
// outside the platform -- a rogue use of the mint authority -- are caught by
// the supply reconciliation, which opens a material STABLE_RESERVE record
// that blocks new risk and trips the circuit breaker.
func TestIntegration_ReconciliationDetectsAnOutOfBandMint(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 2_000_000)
	f.mustMint(c, a, 1_000_000)
	require.True(t, f.reconcile(a).Matched())

	f.outOfBandMint(a, addr("somewhere"), 250_000)
	r := f.reconcile(a)
	assert.False(t, r.SupplyMatches)
	assert.Equal(t, "1250000", r.ChainSupply.String())
	assert.Equal(t, "1000000", r.ExpectedChainSupply.String())
	require.NotEmpty(t, r.RecordID)
	rec, err := f.recon.Get(f.ctx, testDB, reconciliationRecordID(t, r.RecordID))
	require.NoError(t, err)
	assert.Equal(t, reconciliation.KindStableReserve, rec.Kind)
	assert.Equal(t, reconciliation.StatusMismatch, rec.Status)
	assert.True(t, rec.Material)
	assert.True(t, rec.BlocksNewRisk)
	assert.Equal(t, StatusPaused, f.asset(a).Status, "the breaker paused issuance")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM stable_peg_alerts WHERE stable_asset_id = $1 AND kind = 'SUPPLY_MISMATCH' AND breaker_tripped`, a.ID))

	// A second pass refreshes the same record instead of opening another.
	again := f.reconcile(a)
	assert.Equal(t, r.RecordID, again.RecordID)

	_, err = f.mint(c, a, 1_000)
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, RefusalAssetNotActive, e.Fields["reason"])
}

func reconciliationRecordID(t *testing.T, s string) reconciliation.RecordID {
	t.Helper()
	rid, err := reconciliation.ParseRecordID(s)
	require.NoError(t, err)
	return rid
}

// TestIntegration_AReserveShortfallFreezesIssuance: when the cash reserve's
// price falls, admission refuses new supply at once; the monitor trips the
// breaker (PAUSED, bound transition with its reason), raises the alert and
// the blocking reconciliation record; nothing mints until the price recovers,
// an operator resumes and the record is resolved.
func TestIntegration_AReserveShortfallFreezesIssuance(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)
	f.mustMint(c, a, 1_000_000)

	f.clk.Advance(time.Minute)
	f.price(f.cash, 900_000, 6) // the reserve dollar trades at 0.90
	_, err := f.mint(c, a, 1_000)
	require.Error(t, err, "admission refuses supply the reserve cannot cover")
	e, _ := errs.As(err)
	assert.Equal(t, RefusalReserveShortfall, e.Fields["reason"])

	report := f.monitor(a)
	assert.Contains(t, report.Alerts, AlertReserveShortfall)
	assert.True(t, report.BreakerTripped)
	assert.Equal(t, StatusPaused, report.Status)
	assert.Equal(t, money.BPS(9_000), report.Reserve.CoverageBPS)
	var reason string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT reason FROM stable_asset_transitions WHERE stable_asset_id = $1 AND to_status = 'PAUSED'`, a.ID).Scan(&reason))
	assert.Equal(t, "circuit breaker: RESERVE_SHORTFALL", reason)
	require.NotEmpty(t, report.Supply.RecordID)

	_, err = f.mint(c, a, 1_000)
	require.Error(t, err)
	_, err = f.burn(c, a, 1_000)
	require.Error(t, err, "a paused asset freezes burns too")

	f.clk.Advance(time.Minute)
	f.price(f.cash, 1_000_000, 6)
	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Transition(ctx, tx, a.ID, StatusActive, breakerActor, "the breaker may not resume")
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Transition(ctx, tx, a.ID, StatusActive, operator, "reserve price recovered; resuming")
		return err
	}))

	_, err = f.mint(c, a, 1_000)
	require.Error(t, err, "the unresolved reserve mismatch still blocks new risk")
	e, _ = errs.As(err)
	assert.Contains(t, e.Fields["reason_codes"], "RISK_CIRCUIT_BREAKER")

	f.resolve(reconciliationRecordID(t, report.Supply.RecordID))
	f.mustMint(c, a, 1_000)
}

// resolve walks a reconciliation record to RESOLVED_MANUAL as an operator
// would (one status change per transaction).
func (f *fixture) resolve(rid reconciliation.RecordID) {
	f.t.Helper()
	actor := reconciliation.Actor{Type: operator.Type, ID: operator.ID}
	reason := "price restored; reviewed and resolved"
	steps := []struct {
		to reconciliation.Status
		ev reconciliation.TransitionEvidence
	}{
		{reconciliation.StatusInvestigating, reconciliation.TransitionEvidence{Actor: actor, Reason: reason}},
		{reconciliation.StatusResolvedManual, reconciliation.TransitionEvidence{Actor: actor, Reason: reason, EvidenceRef: "incident:itest",
			Patch: &reconciliation.ResolutionPatch{ResolvedByActorType: operator.Type, ResolvedByActorID: operator.ID,
				Reason: reason, EvidenceRef: "incident:itest"}}},
	}
	for _, s := range steps {
		require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.recon.Transition(ctx, tx, rid, s.to, s.ev)
			return err
		}))
	}
}

// TestIntegration_APegDeviationTripsTheBreaker: the market price drifting
// beyond the threshold pauses the asset; a stale price is a warning only.
func TestIntegration_APegDeviationTripsTheBreaker(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 2_000_000)
	f.mustMint(c, a, 1_000_000)

	f.clk.Advance(2 * time.Hour) // every price is now older than the hour the asset accepts
	f.price(f.cash, 1_000_000, 6)
	f.price(f.tbill, 1_000_000, 6)
	report := f.monitor(a)
	assert.True(t, report.PriceStale)
	assert.Contains(t, report.Alerts, AlertPriceStale)
	assert.False(t, report.BreakerTripped, "a stale market price warns; it does not pause")
	assert.Equal(t, StatusActive, report.Status)

	f.clk.Advance(time.Minute)
	f.price(a.AssetID, 980_000, 6)
	report = f.monitor(a)
	assert.Equal(t, money.BPS(200), report.DeviationBPS)
	assert.Contains(t, report.Alerts, AlertPriceDeviation)
	assert.True(t, report.BreakerTripped)
	assert.Equal(t, StatusPaused, f.asset(a).Status)
}

// TestIntegration_ConcurrentMintsPreserveTheInvariants: competing mints
// against a cap admit exactly what fits; afterwards supply, reserve, wallets
// and chain all agree and nothing leaked.
func TestIntegration_ConcurrentMintsPreserveTheInvariants(t *testing.T) {
	f := newFixture(t)
	a := f.register(func(d *Definition) { d.IssuanceCap = money.QuantityFromInt64(2_000_000) })
	customers := make([]customer, 8)
	for i := range customers {
		customers[i] = f.customer()
		f.fund(customers[i], f.cash, 1_000_000)
	}
	var wg sync.WaitGroup
	results := make(chan error, len(customers))
	for _, c := range customers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.mint(c, a, 400_000)
			if err == nil && res.Request.Status != MintIssued {
				err = errs.New(errs.CodeInternal, "mint ended "+string(res.Request.Status))
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	issued := 0
	for err := range results {
		switch {
		case err == nil:
			issued++
		default:
			e, _ := errs.As(err)
			require.NotNil(t, e, "%v", err)
			assert.Equal(t, RefusalCapExceeded, e.Fields["reason"], "%v", err)
		}
	}
	assert.Equal(t, 5, issued, "exactly five 400k mints fit under a 2M cap")
	assert.Equal(t, uint64(2_000_000), f.chainSupply(a))
	assert.Equal(t, "2000000", f.balance(a.account(ledger.CodeStableOutstanding, a.AssetID)))
	assert.Equal(t, "2000000", f.balance(a.account(ledger.CodeStableReserve, f.cash)))
	r := f.reconcile(a)
	assert.True(t, r.Matched(), "%+v", r)
	rl := f.reserveLedger(a)
	assert.True(t, rl.PendingMint.IsZero())
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM asset_reservations WHERE asset_id = $1 AND status = 'ACTIVE'`, f.cash), "no reservation leaked")
}

// TestIntegration_SnapshotsAreSignedHashLinkedAndVerifiable: reserve
// snapshots are archived as evidence, chained by hash and signed; the chain
// verifies from the stored rows and the public key alone, and the database
// refuses a snapshot that does not extend the chain (SV007).
func TestIntegration_SnapshotsAreSignedHashLinkedAndVerifiable(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 3_000_000)
	take := func() SignedSnapshot {
		var sn SignedSnapshot
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			sn, err = f.svc.TakeSnapshot(ctx, tx, a.ID)
			return err
		}))
		return sn
	}
	first := take()
	f.mustMint(c, a, 1_000_000)
	f.clk.Advance(time.Minute)
	second := take()
	assert.Equal(t, "1000000", second.Snapshot.LedgerSupply)
	assert.Equal(t, "1000000", second.Snapshot.ChainSupply)
	assert.Equal(t, SnapshotDisclaimer, second.Snapshot.Disclaimer)
	assert.NotEmpty(t, second.EvidenceRef)

	stored, err := f.svc.Snapshots(f.ctx, testDB, a.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	require.NoError(t, VerifySnapshotChain(stored, f.snaps.PublicKey()))
	assert.Equal(t, first.Hash, stored[0].Hash)
	require.Error(t, VerifySnapshotChain([]SignedSnapshot{stored[1], stored[0]}, f.snaps.PublicKey()))

	_, err = testDB.Exec(f.ctx, `INSERT INTO stable_reserve_snapshots
		(id, stable_asset_id, seq, body, body_hash, prev_hash, signature, signer_public_key, evidence_ref, taken_at)
		VALUES ($1,$2,3,'{}',$3,$3,$4,$5,'x',now())`, uuid.New(), a.ID, make([]byte, 32), make([]byte, 64), make([]byte, 32))
	require.Error(t, err)
	assert.Equal(t, "SV007", db.SQLState(err), "%v", err)
	_, err = testDB.Exec(f.ctx, `UPDATE stable_reserve_snapshots SET body = '{}' WHERE stable_asset_id = $1`, a.ID)
	require.Error(t, err, "snapshots are append-only")
}

// TestIntegration_TheRegistryIsFrozenAndItsStatusBound: around Go, the
// registry's identity cannot change, its status moves only along legal,
// licensed edges, and it can only name a consistent SIMULATED asset.
func TestIntegration_TheRegistryIsFrozenAndItsStatusBound(t *testing.T) {
	f := newFixture(t)
	def := f.definition()
	a, err := f.svc.Register(f.ctx, def, operator)
	require.NoError(t, err)

	// Registering the same definition again is idempotent; a different one
	// under the same name is a conflict.
	again, err := f.svc.Register(f.ctx, def, operator)
	require.NoError(t, err)
	assert.Equal(t, a.ID, again.ID)
	changed := def
	changed.IssuanceCap = money.QuantityFromInt64(42)
	_, err = f.svc.Register(f.ctx, changed, operator)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	_, err = testDB.Exec(f.ctx, `UPDATE stable_assets SET issuance_cap = issuance_cap * 2 WHERE id = $1`, a.ID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err), "the application role may change status only")
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE stable_assets SET issuance_cap = issuance_cap * 2 WHERE id = $1`, a.ID)
	require.Error(t, err)
	assert.Equal(t, "SV002", db.SQLState(err))

	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stable_assets SET status = 'PAUSED' WHERE id = $1`, a.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err), "a status change needs its transition row")

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Transition(ctx, tx, a.ID, StatusRetired, operator, "wind down")
		return err
	}))
	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO stable_asset_transitions (id, stable_asset_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'RETIRED','ACTIVE','OPERATOR','x','bypass')`, uuid.New(), a.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE stable_assets SET status = 'ACTIVE' WHERE id = $1`, a.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "SV001", db.SQLState(err))

	// A registry row naming an asset that is not SIMULATED on its chain.
	_, err = testDB.Exec(f.ctx, `INSERT INTO stable_assets (id, name, asset_id, facility_id, network, chain_id, devnet_mint,
		decimals, status, issuance_cap, mint_authority_mode, mint_authority, burn_authority_mode, redemption_mode,
		peg_quote_asset_id, peg_mantissa, peg_scale, oracle_source, max_price_age_ms, peg_deviation_threshold_bps,
		attestable, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,'n','sbx-solana',$5,6,'ACTIVE',1,'PLATFORM_SIGNER',$5,'HOLDER_SIGNED','INSTANT',$6,1,0,'o',1,1,true,'SYSTEM','x')`,
		uuid.New(), "bad-"+uuid.NewString()[:8], f.cash, a.FacilityID, addr("x").String(), f.quote)
	require.Error(t, err)
	assert.Equal(t, "SV006", db.SQLState(err), "%v", err)
}
