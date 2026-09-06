//go:build integration

package settlement_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/settlement/settlementtest"
)

var testAppURL = os.Getenv("CP_TEST_DATABASE_URL")

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	if testAppURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test (provision one with `go run ./scripts/testdb -name execution`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "settlement-itest", MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// dbWorld is a settlementtest.World whose instrument, listing, assets,
// account, intent and wallet exist in the database, so plans and orders can
// be persisted through the real repositories.
type dbWorld struct {
	*settlementtest.World
	d *db.DB
}

func newDBWorld(t *testing.T, d *db.DB) *dbWorld {
	t.Helper()
	ctx := t.Context()
	w := settlementtest.NewWorld()
	arepo := accounts.NewRepository()
	u, err := arepo.CreateUser(ctx, d, "itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := arepo.CreateAccount(ctx, d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	w.AccountID = a.ID
	suffix := id.New[id.Any]().String()
	suffix = suffix[len(suffix)-12:] // the random tail; the head is a millisecond timestamp shared across fixtures
	usdc := w.USDC
	usdc.ID, usdc.MintAddress = assets.AssetID{}, "usdc-"+suffix
	usdc, err = assets.NewRepository().Create(ctx, d, usdc)
	require.NoError(t, err)
	sol := w.SOL
	sol.ID, sol.MintAddress, sol.Kind = assets.AssetID{}, "sol-"+suffix, assets.KindSPLToken
	sol, err = assets.NewRepository().Create(ctx, d, sol)
	require.NoError(t, err)
	w.USDC, w.SOL = usdc, sol
	w.Adapter.NetworkFeeAsset = sol.ID
	irepo := instruments.NewRepository()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := irepo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: sol.ID, Quote: usdc.ID, Settlement: usdc.ID, CanonicalName: "SOL/USDC-" + suffix, RiskClass: assets.RiskMajor,
			Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		w.Instrument = inst
		venue, err := irepo.CreateVenue(ctx, tx, instruments.Venue{Code: "JUP-" + suffix, Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-devnet", Status: instruments.VenueActive})
		if err != nil {
			return err
		}
		w.Venue = venue
		listing, err := irepo.CreateListing(ctx, tx, instruments.VenueListing{
			VenueID: venue.ID, InstrumentID: inst.ID, VenueNativeID: "SOL-USDC", Network: "solana-devnet", BaseMint: sol.MintAddress, QuoteMint: usdc.MintAddress,
			BasePrecision: 9, QuotePrecision: 6, MinNotionalQuote: money.QuantityFromInt64(1_000_000), Status: instruments.VenueActive,
		})
		if err != nil {
			return err
		}
		w.Listing = listing
		return nil
	}))
	w.WalletAddress = "wallet-" + suffix
	w.Chain.SetBalance(w.WalletAddress, usdc.MintAddress, money.QuantityFromInt64(1_000_000_000))
	w.Chain.SetBalance(w.WalletAddress, sol.MintAddress, money.QuantityFromInt64(5_000_000_000))
	_, err = d.Exec(ctx, `INSERT INTO wallets (id, account_id, provider, provider_wallet_id, chain, address, kind, status)
		VALUES ($1,$2,'privy',$3,'solana-devnet',$4,'EMBEDDED_DELEGATED','ACTIVE')`, w.WalletID, w.AccountID, "pw-"+w.WalletID, w.WalletAddress)
	require.NoError(t, err)
	hash := sha256.Sum256([]byte(w.IntentID))
	_, err = d.Exec(ctx, `INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor, constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash)
		VALUES ($1,$2,'USER','user-1','ACQUIRE_NOTIONAL',$3,10000,'{}',now(),$4,$5,'LIVE','PLANNED',$6)`,
		w.IntentID, w.AccountID, w.Instrument.ID, "idem-"+w.IntentID, "corr-"+w.IntentID[:8], hash[:])
	require.NoError(t, err)
	return &dbWorld{World: w, d: d}
}

func TestIntegration_PlanRepository(t *testing.T) {
	d := openTestDB(t)
	w := newDBWorld(t, d)
	ctx := t.Context()
	repo := settlement.NewPlanRepository(w.Clock, audit.NewWriter())
	plan, err := w.Planner.Plan(w.Input())
	require.NoError(t, err)
	plan.AccountID = w.AccountID.String()

	var stored settlement.Plan
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = repo.Create(ctx, tx, plan)
		return err
	}))
	require.Equal(t, plan.ID, stored.ID)
	require.Equal(t, settlement.PlanDraft, stored.Status)
	require.Equal(t, plan.Hash, stored.Hash)
	require.Len(t, stored.Steps, 17)
	require.Equal(t, w.AccountID.String(), stored.AccountID, "account resolved through the intent")
	ok, err := settlement.VerifyHash(stored)
	require.NoError(t, err)
	require.True(t, ok, "the persisted plan hashes to the same value as the planned one")
	wantHC, err := audit.CanonicalJSON(plan.HardConstraints)
	require.NoError(t, err)
	gotHC, err := audit.CanonicalJSON(stored.HardConstraints)
	require.NoError(t, err)
	require.JSONEq(t, string(wantHC), string(gotHC))
	for i, s := range stored.Steps {
		require.Equal(t, plan.Steps[i].ID, s.ID)
		require.Equal(t, plan.Steps[i].Type, s.Type)
		require.ElementsMatch(t, plan.Steps[i].DependsOn, s.DependsOn)
		require.Equal(t, plan.Steps[i].Timeout, s.Timeout)
		require.JSONEq(t, string(plan.Steps[i].EvidenceInputs), string(s.EvidenceInputs))
	}

	// The same (intent, version) cannot be planned twice.
	dup := plan
	dup.ID = settlement.NewPlanID()
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Create(ctx, tx, dup)
		return err
	})
	require.True(t, errs.HasCode(err, errs.CodeConflict), "%v", err)

	// Approve freezes the plan.
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		p, err := repo.Approve(ctx, tx, plan.ID, "OPERATOR", "op-1", "approved for execution")
		if err != nil {
			return err
		}
		require.Equal(t, settlement.PlanApproved, p.Status)
		require.NotNil(t, p.ApprovedAt)
		return nil
	}))
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Approve(ctx, tx, plan.ID, "OPERATOR", "op-1", "again")
		return err
	})
	require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition))

	// The database trigger refuses any change to a frozen field.
	for _, stmt := range []string{
		`UPDATE execution_plans SET hard_constraints = '{"side":"SELL"}' WHERE id = $1`,
		`UPDATE execution_plans SET plan_hash = '\x00' WHERE id = $1`,
		`UPDATE execution_plans SET selected_venue_listing_id = NULL WHERE id = $1`,
		`UPDATE execution_plans SET dry_run = true WHERE id = $1`,
		`UPDATE execution_plans SET approved_at = now() WHERE id = $1`,
		`UPDATE execution_plans SET version = 7 WHERE id = $1`,
		`DELETE FROM execution_plans WHERE id = $1`,
	} {
		err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, plan.ID)
			return err
		})
		require.Error(t, err, stmt)
		require.True(t, db.SQLState(err) == "LG003" || db.IsInsufficientPrivilege(err), "%s: %v", stmt, err)
	}
	// Lifecycle columns still move.
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.SetStatus(ctx, tx, plan.ID, settlement.PlanApproved, settlement.PlanExecuting, "started")
		return err
	}))
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.SetStatus(ctx, tx, plan.ID, settlement.PlanApproved, settlement.PlanExecuting, "stale from")
		return err
	})
	require.True(t, errs.HasCode(err, errs.CodeConflict), "compare-and-set on the status")

	// Step state machine with compare-and-set and attempt counting.
	first := stored.Steps[0]
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		s, err := repo.MarkStep(ctx, tx, first.ID, settlement.StepPending, settlement.StepRunning, settlement.StepOutcome{})
		if err != nil {
			return err
		}
		require.Equal(t, int32(1), s.Attempts)
		require.NotNil(t, s.StartedAt)
		s, err = repo.MarkStep(ctx, tx, first.ID, settlement.StepRunning, settlement.StepSucceeded, settlement.StepOutcome{EvidenceOutput: map[string]any{"eligible": true}})
		if err != nil {
			return err
		}
		require.Equal(t, settlement.StepSucceeded, s.State)
		require.JSONEq(t, `{"eligible":true}`, string(s.EvidenceOutput))
		require.NotNil(t, s.FinishedAt)
		return nil
	}))
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.MarkStep(ctx, tx, first.ID, settlement.StepRunning, settlement.StepFailed, settlement.StepOutcome{})
		return err
	})
	require.True(t, errs.HasCode(err, errs.CodeConflict))
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return repo.ResetSteps(ctx, tx, plan.ID, 0)
	}))
	got, err := repo.Get(ctx, d, plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.StepPending, got.Steps[0].State)
	require.Empty(t, got.Steps[0].EvidenceOutput)
	require.Equal(t, int32(1), got.Steps[0].Attempts, "attempt history survives a reset")

	// Supersede: replanning creates version 2 and marks version 1 SUPERSEDED.
	next, err := w.Planner.Plan(w.Input())
	require.NoError(t, err)
	next.Version = 2
	next.AccountID = w.AccountID.String()
	next.Hash, err = settlement.ComputeHash(next)
	require.NoError(t, err)
	wrong := next
	wrong.Version = 3
	wrong.Hash, _ = settlement.ComputeHash(wrong)
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Supersede(ctx, tx, plan.ID, wrong, "OPERATOR", "op-1", "replan")
		return err
	})
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed), "version must be current + 1: %v", err)
	var v2 settlement.Plan
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v2, err = repo.Supersede(ctx, tx, plan.ID, next, "OPERATOR", "op-1", "replan")
		return err
	}))
	require.Equal(t, int32(2), v2.Version)
	require.Equal(t, settlement.PlanDraft, v2.Status)
	old, err := repo.Get(ctx, d, plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanSuperseded, old.Status)
	require.NotNil(t, old.FinishedAt)
	latest, err := repo.LatestForIntent(ctx, d, w.IntentID)
	require.NoError(t, err)
	require.Equal(t, v2.ID, latest.ID)

	// NO_VALID_PLAN is a persisted outcome too.
	nvp := settlement.Plan{
		ID: settlement.NewPlanID(), IntentID: w.IntentID, Version: 3, PlannerVersion: settlement.PlannerVersion, Status: settlement.PlanNoValidPlan,
		NoPlanReasonCodes: []string{settlement.ReasonRiskRejected}, AccountID: w.AccountID.String(),
	}
	nvp.Hash, err = settlement.ComputeHash(nvp)
	require.NoError(t, err)
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Create(ctx, tx, nvp)
		return err
	}))
	got, err = repo.Get(ctx, d, nvp.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanNoValidPlan, got.Status)
	require.Equal(t, []string{settlement.ReasonRiskRejected}, got.NoPlanReasonCodes)
	require.Empty(t, got.Steps)

	var auditRows int
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'execution_plan' AND resource_id = $1`, plan.ID.String()).Scan(&auditRows))
	require.GreaterOrEqual(t, auditRows, 4, "created, approved, status, superseded")
}

// sqlQuotes persists quote snapshots into the quotes table for the FK the
// order carries (the integrator wires internal/quote's repository).
type sqlQuotes struct{}

func (sqlQuotes) Save(ctx context.Context, tx pgx.Tx, q execution.QuoteSnapshot) (string, error) {
	qid := id.New[id.Any]().String()
	route := q.RouteSummary
	if len(route) == 0 {
		route = json.RawMessage("[]")
	}
	_, err := tx.Exec(ctx, `INSERT INTO quotes (id, intent_id, provider, provider_request_id, instrument_id, venue_listing_id, side, input_asset_id, input_quantity, output_asset_id,
			expected_output, minimum_output, effective_price_mantissa, effective_price_scale, price_impact_bps, slippage_bps, est_network_cost, est_network_cost_asset_id,
			est_venue_fee, est_venue_fee_asset_id, platform_fee, platform_fee_asset_id, platform_fee_bps, fee_policy_version, received_at, expires_at, route_hash, route_summary, raw_response_ref, raw_response_hash)
		VALUES ($1,$2::uuid,$3,$4,$5::uuid,$6::uuid,$7,$8,$9::numeric,$10,$11::numeric,$12::numeric,$13::numeric,$14,$15,$16,$17::numeric,$18,$19::numeric,$20,$21::numeric,$22,$23,$24,$25,$26,$27,$28,$29,$30)`,
		qid, q.IntentID, q.Provider, q.ProviderRequestID, q.InstrumentID, q.VenueListingID, string(q.Side), q.InputAsset, q.InputQuantity.String(), q.OutputAsset,
		q.ExpectedOutput.String(), q.MinimumOutput.String(), q.EffectivePrice.Mantissa.String(), q.EffectivePrice.Scale, q.PriceImpactBPS, q.SlippageBPS,
		q.EstNetworkCost.String(), nullAsset(q.EstNetworkAsset), q.EstVenueFee.String(), nullAsset(q.EstVenueFeeAsset), q.PlatformFee.String(), nullAsset(q.PlatformFeeAsset),
		q.PlatformFeeBPS, q.FeePolicyVersion, q.ReceivedAt, q.ExpiresAt, q.RouteHash, route, q.RawResponseRef, q.RawResponseHash)
	return qid, err
}

func nullAsset(a assets.AssetID) *assets.AssetID {
	if a.IsZero() {
		return nil
	}
	return &a
}

// sqlIntents reads the executor's IntentRef from trade_intents and wallets.
type sqlIntents struct{ w *dbWorld }

func (r sqlIntents) Intent(ctx context.Context, q db.Querier, intentID string) (settlement.IntentRef, error) {
	ref := r.w.IntentRef()
	var accountID string
	if err := q.QueryRow(ctx, `SELECT account_id::text FROM trade_intents WHERE id = $1::uuid`, intentID).Scan(&accountID); err != nil {
		return settlement.IntentRef{}, err
	}
	ref.AccountID = accountID
	return ref, nil
}

// capitalEmitter forwards capital events onto the outbox for registered
// topics; capital.reservation.locked has no registered topic yet, so it is
// recorded as an audit-visible no-op here (the integrator's adapter decides).
type capitalEmitter struct {
	outbox *event.Outbox
	clk    clock.Clock
}

func (e capitalEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	if _, ok := event.Lookup(event.Topic(topic)); !ok {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	agg := ""
	if ev, ok := payload.(capital.ReservationEvent); ok {
		agg = ev.ReservationID
	}
	if agg == "" {
		return nil
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID: event.NewEventID().String(), Type: topic, SchemaVersion: event.Topic(topic).Version(), Source: "capital",
		AggregateType: event.AggregateReservation, AggregateID: agg, OccurredAt: e.clk.Now(), Payload: body,
	})
}

func seedWalletBalance(t *testing.T, d *db.DB, account accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		wallet := ledgerAccount(t, tx, account, asset, "WALLET", "DEBIT")
		cap := ledgerAccount(t, tx, account, asset, "CAPITAL", "CREDIT")
		txID := id.New[id.Any]()
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1,'SEED',$2,'seed',$3,now(),'SYSTEM','settlement-itest',$4)`, txID, "seed:"+txID.String(), txID.String(), []byte{0}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,0,$3,$4,'DEBIT',$5)`,
			id.New[id.Any](), txID, wallet, asset, qty); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,1,$3,$4,'CREDIT',$5)`,
			id.New[id.Any](), txID, cap, asset, qty)
		return err
	}))
}

func ledgerAccount(t *testing.T, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID, code, side string) id.ID[id.Any] {
	t.Helper()
	ctx := t.Context()
	_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
		VALUES ($1,'CUSTOMER',$2,$3,$4,$5) ON CONFLICT (owner_type, owner_id, code, asset_id) DO NOTHING`, id.New[id.Any](), account, code, asset, side)
	require.NoError(t, err)
	var out id.ID[id.Any]
	require.NoError(t, tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE owner_type='CUSTOMER' AND owner_id=$1 AND code=$2 AND asset_id=$3`, account, code, asset).Scan(&out))
	return out
}

// TestIntegration_ExecutorOnRepositories runs the executor with the real
// plan, order, attempt, capital, ledger and position repositories on the
// isolated database, the fake adapter and chain standing in for the venue.
func TestIntegration_ExecutorOnRepositories(t *testing.T) {
	d := openTestDB(t)
	w := newDBWorld(t, d)
	ctx := t.Context()
	seedWalletBalance(t, d, w.AccountID, w.USDC.ID, money.QuantityFromInt64(1_000_000_000))
	seedWalletBalance(t, d, w.AccountID, w.SOL.ID, money.QuantityFromInt64(1_000_000_000))
	outbox := event.NewOutbox(w.Clock)
	aud := audit.NewWriter()
	plans := settlement.NewPlanRepository(w.Clock, aud)
	orders := execution.NewRepository(w.Clock, outbox, aud)
	attempts := execution.NewAttemptRepository(w.Clock, outbox, aud)
	capitalSvc := capital.NewService(w.Clock, capitalEmitter{outbox: outbox, clk: w.Clock})
	ledgerSvc := ledger.NewService(w.Clock, "itest")
	lots := positions.NewEngine()

	plan, err := w.Planner.Plan(w.Input())
	require.NoError(t, err)
	plan.AccountID = w.AccountID.String()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := plans.Create(ctx, tx, plan); err != nil {
			return err
		}
		_, err := plans.Approve(ctx, tx, plan.ID, "OPERATOR", "op-1", "approved")
		return err
	}))

	deps := w.Deps()
	deps.DB, deps.Plans, deps.Orders, deps.Attempts = d, plans, orders, attempts
	deps.Quotes, deps.Intents = sqlQuotes{}, sqlIntents{w: w}
	deps.Capital, deps.Ledger, deps.Positions, deps.Audit = capitalSvc, ledgerSvc, lots, aud
	w.Signer.OmitDecisionID = true // the fake signer writes no signing_decisions row
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	res, err := ex.Run(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)

	// Exactly one economic effect, all the way down to the real tables.
	require.Equal(t, 1, w.Chain.Landed())
	stored, err := plans.Get(ctx, d, plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, stored.Status)
	for _, s := range stored.Steps {
		require.Equal(t, settlement.StepSucceeded, s.State, "step %s: %s", s.Type, s.LastError)
	}
	oid, err := execution.ParseOrderID(res.OrderID)
	require.NoError(t, err)
	o, err := orders.Get(ctx, d, oid)
	require.NoError(t, err)
	require.Equal(t, execution.OrderSettled, o.Status)
	require.True(t, o.FilledInputQuantity.Equal(o.InputQuantity))
	fills, err := orders.ListFills(ctx, d, oid)
	require.NoError(t, err)
	require.Len(t, fills, 1)
	require.True(t, fills[0].Posted())
	require.True(t, fills[0].PositionApplied())
	var journalRows int
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM journal_transactions WHERE reference_type = 'fill' AND reference_id = $1`, fills[0].ID.String()).Scan(&journalRows))
	require.Equal(t, 1, journalRows)
	holdings, err := lots.Holdings(ctx, d, w.AccountID)
	require.NoError(t, err)
	var solOpen money.Quantity
	for _, h := range holdings {
		if h.AssetID == w.SOL.ID {
			solOpen = h.Quantity
		}
	}
	require.True(t, solOpen.IsPositive(), "the buy opened a SOL lot")
	rid, err := capital.ParseReservationID(o.ReservationID)
	require.NoError(t, err)
	reservation, err := capitalSvc.Get(ctx, d, rid)
	require.NoError(t, err)
	require.Equal(t, capital.ReservationConsumed, reservation.Status)
	require.Equal(t, o.ID.String(), reservation.LockedByOrderID)
	// Drift verifiers are global; other suites on the shared test database
	// seed raw reservation rows, so only this account's rows are judged.
	drift, err := capital.VerifyReservationTotals(ctx, d)
	require.NoError(t, err)
	for _, dr := range drift {
		require.NotEqual(t, w.AccountID, dr.AccountID, "reservation totals drift: %+v", dr)
	}
	ledgerDrift, err := ledger.VerifyBalances(ctx, d)
	require.NoError(t, err)
	for _, dr := range ledgerDrift {
		require.NotEqual(t, w.AccountID.String(), dr.Account.OwnerID, "ledger drift: %+v", dr)
	}
	atts, err := attempts.ListForOrder(ctx, d, oid)
	require.NoError(t, err)
	require.Len(t, atts, 1)
	require.Equal(t, execution.AttemptConfirmed, atts[0].Status)
	require.NotEmpty(t, atts[0].TxSignature)
	// The audit chain of the account verifies end to end.
	report, err := audit.NewVerifier().VerifyStream(ctx, d, audit.AccountStream(w.AccountID.String()))
	require.NoError(t, err)
	require.True(t, report.OK, report.Reason)
	require.Greater(t, report.Events, 20)
}
