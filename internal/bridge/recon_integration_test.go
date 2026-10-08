//go:build integration

package bridge

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// TestIntegration_BridgeReconciliationDetectsAnOutOfBandLock: units locked
// into the escrow, or minted on the destination, without a booked leg make
// the route's books and chains disagree; the BRIDGE reconciliation records a
// material mismatch that blocks new risk and pauses the route, while a leg
// already in flight still completes.
func TestIntegration_BridgeReconciliationDetectsAnOutOfBandLock(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	f.mustBridge(r, c, 100_000)
	require.True(t, f.reconcile(r).Matched())
	inFlight, err := f.bridge(r, c, 50_000, gates.ModeDevnet)
	require.NoError(t, err)

	lockTx := f.outOfBandLock(r, c, 70_000)
	rec := f.reconcile(r)
	assert.False(t, rec.EscrowMatches)
	assert.True(t, rec.SupplyMatches)
	assert.True(t, rec.BooksConserve, "the books themselves still conserve")
	assert.Equal(t, "70000", rec.Escrow.Sub(rec.ExpectedEscrow).String())
	assert.True(t, rec.BreakerTripped)
	require.NotEmpty(t, rec.RecordID)
	assert.Equal(t, RoutePaused, f.routeNow(r).Status)
	recID, err := reconciliation.ParseRecordID(rec.RecordID)
	require.NoError(t, err)
	record, err := f.recon.Get(f.ctx, testDB, recID)
	require.NoError(t, err)
	assert.Equal(t, reconciliation.KindBridge, record.Kind)
	assert.Equal(t, reconciliation.StatusMismatch, record.Status)
	assert.True(t, record.Material && record.BlocksNewRisk)

	_, err = f.bridge(r, c, 1_000, gates.ModeSandbox)
	assert.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err), "a paused route takes no new legs")

	// The leg already in flight completes.
	f.produce(r.SourceChain, 1+itDepth)
	f.observe(inFlight.Leg.ID)
	f.produce(r.DestinationChain, 1+itDepth)
	done := f.observe(inFlight.Leg.ID)
	assert.Equal(t, StateReconciled, done.Status)
	assert.Equal(t, "150000", f.wallet(c, r.DestinationAssetID))

	// A mint nobody booked on the destination is caught too, on the same record.
	f.outOfBandRelease(r, lockTx)
	rec2 := f.reconcile(r)
	assert.False(t, rec2.SupplyMatches)
	assert.Equal(t, rec.RecordID, rec2.RecordID, "the open record is refreshed, not duplicated")
	assert.False(t, rec2.BreakerTripped, "already paused")

	// The breaker never resumes a route. An operator may, but the open record
	// still blocks new risk.
	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.TransitionRoute(ctx, tx, r.ID, RouteActive, breakerActor, "self-resume")
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.TransitionRoute(ctx, tx, r.ID, RouteActive, operator, "investigating")
		return err
	}))
	_, err = f.bridge(r, c, 1_000, gates.ModeSandbox)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err), "the open BRIDGE record is a circuit breaker")
}

// TestIntegration_TheDatabaseRefusesBridgeBookkeepingBypasses: with the
// service bypassed, the database refuses an illegal leg transition (BR001), a
// changed frozen or set-once column (BR002), a bridge journal that breaks the
// route's conservation identity (BR003), a route inconsistent with its assets
// (BR004), a bridge journal bound to no leg (BR005), a status change without
// its transition row (AU001), and any change to a release attempt.
func TestIntegration_TheDatabaseRefusesBridgeBookkeepingBypasses(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	leg := f.mustBridge(r, c, 100_000)
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	inTx := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return testDB.InTx(f.ctx, db.TxOptions{}, fn)
	}

	err = inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO bridge_leg_transitions (id, leg_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'RECONCILED','BRIDGE_REQUESTED','OPERATOR','x','bypass')`, uuid.New(), leg.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE bridge_legs SET status = 'BRIDGE_REQUESTED' WHERE id = $1`, leg.ID)
		return err
	})
	assert.Equal(t, "BR001", db.SQLState(err), "%v", err)

	_, err = testDB.Exec(f.ctx, `UPDATE bridge_legs SET quantity = quantity * 2 WHERE id = $1`, leg.ID)
	assert.Equal(t, "42501", db.SQLState(err), "the application role may not touch a leg's identity")
	_, err = owner.Exec(f.ctx, `UPDATE bridge_legs SET quantity = quantity * 2 WHERE id = $1`, leg.ID)
	assert.Equal(t, "BR002", db.SQLState(err), "%v", err)
	_, err = testDB.Exec(f.ctx, `UPDATE bridge_legs SET lock_tx_id = 'forged' WHERE id = $1`, leg.ID)
	assert.Equal(t, "BR002", db.SQLState(err), "a set-once column never changes: %v", err)
	_, err = owner.Exec(f.ctx, `UPDATE bridge_routes SET source_mint = 'x' WHERE id = $1`, r.ID)
	assert.Equal(t, "BR002", db.SQLState(err), "%v", err)

	err = inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bridge_routes SET status = 'PAUSED' WHERE id = $1`, r.ID)
		return err
	})
	assert.Equal(t, "AU001", db.SQLState(err), "a status change needs its transition row: %v", err)

	post := func(p ledger.Posting) error {
		p.IdempotencyKey = "bypass:" + uuid.NewString()
		return inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.ledger.Post(ctx, tx, p)
			return err
		})
	}
	// Half a BRIDGE_SOURCE_FINALIZED: the customer's units go in flight with
	// nothing locked on the facility.
	half := sourceFinalizedPosting(r, leg, f.clk.Now())
	half.Entries = half.Entries[:2]
	err = post(half)
	assert.Equal(t, "BR003", db.SQLState(err), "%v", err)
	unbound := sourceFinalizedPosting(r, leg, f.clk.Now())
	unbound.Reference = ledger.FinancialEventReference{Type: "not_a_bridge_leg", ID: uuid.NewString()}
	err = post(unbound)
	assert.Equal(t, "BR005", db.SQLState(err), "%v", err)
	resized := leg
	resized.Quantity = devnet.QuantityOf(7)
	err = post(sourceFinalizedPosting(r, resized, f.clk.Now()))
	assert.Equal(t, "BR005", db.SQLState(err), "a journal must move exactly its leg's quantity: %v", err)

	// A route whose source is a SIMULATED asset that lives on no devnet chain.
	_, err = testDB.Exec(f.ctx, `INSERT INTO bridge_routes (id, name, facility_id, network, source_chain, source_asset_id,
		source_mint, destination_chain, destination_asset_id, destination_mint, decimals, attester, unit_price_mantissa,
		unit_price_scale, max_release_attempts, status, created_by_actor_type, created_by_actor_id)
		SELECT $1, $4, facility_id, network, source_chain, $3, source_mint, destination_chain,
		       destination_asset_id, destination_mint, decimals, attester, 1, 0, 1, 'ACTIVE', 'SYSTEM', 'x'
		  FROM bridge_routes WHERE id = $2`, uuid.New(), r.ID, f.sandboxAsset(), "bad-"+uuid.NewString()[:8])
	assert.Equal(t, "BR004", db.SQLState(err), "%v", err)

	atts := f.attempts(leg.ID, AttemptRelease)
	_, err = testDB.Exec(f.ctx, `UPDATE bridge_release_attempts SET raw_tx = '\x00' WHERE tx_id = $1`, string(atts[0].TxID))
	assert.Equal(t, "42501", db.SQLState(err))
	_, err = owner.Exec(f.ctx, `DELETE FROM bridge_release_attempts WHERE tx_id = $1`, string(atts[0].TxID))
	require.Error(t, err, "release attempts are append-only")

	assert.Equal(t, StateReconciled, f.leg(leg.ID).Status)
	assert.True(t, f.reconcile(r).Matched(), "nothing above reached the books")
}

// sandboxAsset creates a SIMULATED asset on the sandbox chain: not a devnet
// asset, so no route may name it. Its symbol is unique to the test.
func (f *fixture) sandboxAsset() assets.AssetID {
	f.t.Helper()
	sym := "BX" + uuid.NewString()[:6]
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: sym, Name: sym + " (sandbox test asset)", Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a.ID
}

// TestIntegration_ConcurrentLegsKeepTheBooksConserved: many customers bridging
// at once on one route all complete, each exactly once, and the route's books
// and chains agree afterwards.
func TestIntegration_ConcurrentLegsKeepTheBooksConserved(t *testing.T) {
	f := newFixture(t)
	r := f.route
	const n = 6
	cs := make([]customer, n)
	for i := range cs {
		cs[i] = f.customer()
		f.fund(r, cs[i], 1_000_000)
	}
	var wg sync.WaitGroup
	errsCh := make(chan error, 2*n)
	for i := range cs {
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func(c customer, q uint64) {
				defer wg.Done()
				res, err := f.bridge(r, c, q, gates.ModeSandbox)
				if err == nil && res.Leg.Status != StateReconciled {
					err = errs.Newf(errs.CodeInternal, "leg ended %s (%s)", res.Leg.Status, res.Leg.FailureCode)
				}
				errsCh <- err
			}(cs[i], uint64(100_000*(j+1)))
		}
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
	for _, c := range cs {
		assert.Equal(t, "700000", f.wallet(c, r.SourceAssetID))
		assert.Equal(t, "300000", f.wallet(c, r.DestinationAssetID))
		assert.Equal(t, "0", f.inFlight(r, c))
		assert.Equal(t, uint64(300_000), f.chainBalance(r.DestinationChain, c.holder, r.DestinationMint))
	}
	assert.Equal(t, "1800000", f.facility(r, ledger.CodeBridgeLocked, r.SourceAssetID))
	assert.Equal(t, "1800000", f.facility(r, ledger.CodeBridgeOutstanding, r.DestinationAssetID))
	assert.Equal(t, uint64(1_800_000), f.supply(r.DestinationChain, r.DestinationMint))
	rec := f.reconcile(r)
	assert.True(t, rec.Matched(), "%+v", rec)
	assert.Equal(t, RouteActive, f.routeNow(r).Status)
}

// TestIntegration_ConcurrentDuplicatesSettleOnce: the same request sent by
// many goroutines at once is one leg; many drivers observing one leg at once
// relay one release and post each journal once.
func TestIntegration_ConcurrentDuplicatesSettleOnce(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	req := f.request(r, c, 200_000, "same-"+uuid.NewString(), gates.ModeDevnet)
	const n = 8
	legs := make(chan LegID, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.svc.Bridge(c.ctx, req)
			if err == nil {
				legs <- res.Leg.ID
			}
		}()
	}
	wg.Wait()
	close(legs)
	var lid LegID
	got := 0
	for id := range legs {
		if got > 0 {
			require.Equal(t, lid, id, "one request, one leg")
		}
		lid, got = id, got+1
	}
	require.Equal(t, n, got)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM bridge_legs WHERE account_id = $1`, c.id))
	leg := f.leg(lid)
	assert.Equal(t, 1, f.submissions(leg.LockTxID))

	drive := func() {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := f.svc.Observe(f.ctx, lid)
				assert.NoError(t, err)
			}()
		}
		wg.Wait()
	}
	f.produce(r.SourceChain, 1+itDepth)
	drive()
	require.Len(t, f.attempts(lid, AttemptRelease), 1, "one release relayed, whoever drove it")
	f.produce(r.DestinationChain, 1+itDepth)
	drive()
	leg = f.leg(lid)
	assert.Equal(t, StateReconciled, leg.Status)
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeSourceFinalized))
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeDestinationFinalized))
	assert.Equal(t, 1, f.submissions(f.attempts(lid, AttemptRelease)[0].TxID))
	assert.Equal(t, "200000", f.wallet(c, r.DestinationAssetID))
	assert.Equal(t, uint64(200_000), f.supply(r.DestinationChain, r.DestinationMint))
	assert.True(t, f.reconcile(r).Matched())
}

// TestProp_IntegrationNoDoubleCounting: over random sequences of legs that
// complete, fail before finality, are refunded after a destination that never
// released, or are left in flight at a random stage -- through the database,
// the ledger and the devnet host -- a customer's value is always counted
// exactly once across the source wallet, the bridge and the destination
// wallet, nothing not final is available or eligible, and the route's books
// and chains agree.
func TestProp_IntegrationNoDoubleCounting(t *testing.T) {
	f := newFixture(t)
	r := f.route
	seen := map[string]int{}
	defer func() {
		for _, k := range []string{"completed", "expired", "refunded", "in-flight"} {
			assert.Positive(t, seen[k], "the property must have exercised %s legs", k)
		}
	}()
	rapid.Check(t, func(rt *rapid.T) {
		c := f.customer()
		const funded = 1_000_000
		f.fund(r, c, funded)
		steps := rapid.IntRange(1, 4).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			free, err := money.ParseQuantity(f.available(c, r.SourceAssetID))
			require.NoError(rt, err)
			n, err := free.Int64()
			require.NoError(rt, err)
			if n == 0 {
				break
			}
			q := uint64(rapid.Int64Range(1, n).Draw(rt, "q")) // #nosec G115 -- positive by construction
			switch rapid.IntRange(0, 3).Draw(rt, "op") {
			case 0:
				res, err := f.bridge(r, c, q, gates.ModeSandbox)
				require.NoError(rt, err)
				require.Equal(rt, StateReconciled, res.Leg.Status)
				seen["completed"]++
			case 1:
				require.NoError(rt, f.host.DropNext(f.ctx, r.SourceChain, 1))
				res, err := f.bridge(r, c, q, gates.ModeSandbox)
				require.NoError(rt, err)
				require.Equal(rt, StateFailed, res.Leg.Status)
				seen["expired"]++
			case 2:
				require.NoError(rt, f.host.SetMintFrozen(f.ctx, r.DestinationChain, r.DestinationMint, true))
				res, err := f.bridge(r, c, q, gates.ModeSandbox)
				require.NoError(rt, f.host.SetMintFrozen(f.ctx, r.DestinationChain, r.DestinationMint, false))
				require.NoError(rt, err)
				require.Equal(rt, StateManualReview, res.Leg.Status)
				f.noPhantomCapital(r, c, funded, "in review")
				out, err := f.svc.Refund(f.ctx, res.Leg.ID, operator, "destination frozen")
				require.NoError(rt, err)
				require.Equal(rt, StateFailed, out.Leg.Status)
				seen["refunded"]++
			case 3:
				res, err := f.bridge(r, c, q, gates.ModeDevnet)
				require.NoError(rt, err)
				f.produce(r.SourceChain, uint64(rapid.IntRange(0, 1+itDepth).Draw(rt, "src_blocks"))) // #nosec G115 -- small
				f.observe(res.Leg.ID)
				f.produce(r.DestinationChain, uint64(rapid.IntRange(0, 1+itDepth).Draw(rt, "dst_blocks"))) // #nosec G115 -- small
				f.observe(res.Leg.ID)
				seen["in-flight"]++
			}
			f.noPhantomCapital(r, c, funded, "step")
			var rec RouteReconciliation
			require.NoError(rt, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				rec, err = f.svc.ReconcileRoute(ctx, tx, r.ID, reconciliation.ModeEventDriven, operator)
				return err
			}))
			require.True(rt, rec.Matched(), "%+v", rec)
		}
	})
}

// TestIntegration_ARequestedLegCarriesItsLock: the leg register refuses a leg
// past RESERVED that does not carry the lock transaction it was requested
// with (bridge_legs_requested_has_lock), even written directly by the owner.
func TestIntegration_ARequestedLegCarriesItsLock(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	intentID := uuid.New()
	_, err = owner.Exec(f.ctx, `INSERT INTO economic_intents (id, account_id, kind, mode, capability, actor_type, actor_id,
		content_hash, idempotency_key, status) VALUES ($1, $2, 'BRIDGE_VALUE', 'SANDBOX', 'CROSS_CHAIN_BUYING_POWER', 'USER', 'u',
		sha256('lockless'::bytea), $3, 'RECEIVED')`, intentID, c.id, "lockless-"+uuid.NewString())
	require.NoError(t, err)
	_, err = owner.Exec(f.ctx, `INSERT INTO bridge_legs (id, route_id, account_id, intent_id, mode, quantity, source_owner,
		destination_owner, lock_nonce, plan, plan_hash, status) VALUES ($1, $2, $3, $4, 'SANDBOX', 1, 'a', 'b', 987654321,
		'{}', sha256('plan'::bytea), 'BRIDGE_REQUESTED')`, uuid.New(), f.route.ID, c.id, intentID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge_legs_requested_has_lock", "%v", err)
}
