//go:build integration

package signing_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	signingv1 "github.com/nodal/controlplane/internal/gen/proto/controlplane/signing/v1"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/signing"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/signing/signingtest"
	"github.com/nodal/controlplane/internal/wallet"
	"github.com/nodal/controlplane/internal/wallet/wallettest"
)

// Run against an isolated database: eval "$(go run ./scripts/testdb -name signing -export)".
const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "signing integration: connect:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "signing integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "signing integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "signing-itest", MaxConns: 16})
	if err != nil {
		fmt.Fprintln(os.Stderr, "signing integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

// seed is a complete approved chain for one swap attempt.
type seed struct {
	userID, accountID, inputAssetID, outputAssetID, venueID, exposureID, instrumentID, listingID string
	intentID, riskID, quoteID, planID, reservationID, walletID, orderID, attemptID               string
	providerWalletID                                                                             string
	planHash                                                                                     []byte
	raw                                                                                          []byte
	swap                                                                                         *signingtest.Swap
}

type seedOptions struct {
	native        bool
	version       inspect.Version
	planStatus    string
	riskDecision  string
	walletStatus  string
	delegated     bool
	reservation   string
	attemptStatus string
	quoteExpired  bool
	simulationOK  *bool
	txHashOnRow   bool
}

func defaultSeedOptions() seedOptions {
	ok := true
	return seedOptions{version: inspect.VersionV0, planStatus: "APPROVED", riskDecision: "ALLOW", walletStatus: "ACTIVE", delegated: true, reservation: "ACTIVE", attemptStatus: "INSPECTED", simulationOK: &ok, txHashOnRow: true}
}

func exec(ctx context.Context, t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := testDB.Exec(ctx, sql, args...)
	require.NoError(t, err, sql)
}

func newID() string { return id.New[id.Any]().String() }

// tail returns the random suffix of a UUIDv7 (the prefix is a timestamp shared
// by ids minted in the same millisecond).
func tail(uuid string) string { return uuid[len(uuid)-12:] }

// seedChain inserts users → accounts → assets → instrument/listing → intent →
// risk decision → quote → plan → reservation → wallet → order → attempt.
func seedChain(t *testing.T, fake *wallettest.Fake, opts seedOptions) *seed {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	s := &seed{}
	var swap *signingtest.Swap
	if opts.native {
		swap = signingtest.NewNativeInputSwap()
	} else {
		swap = signingtest.NewSwap()
	}
	// The wallet key comes from the fake provider so the fake can really sign.
	s.providerWalletID = fake.ProviderWalletID(newID(), "solana-mainnet")
	swap.WalletKey = fake.KeyFor(s.providerWalletID)
	swap.Derive()
	s.swap = swap
	var txHash [32]byte
	s.planHash = swap.PlanHash

	s.userID, s.accountID = newID(), newID()
	exec(ctx, t, `INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1,'test',$2,'ACTIVE')`, s.userID, "sub-"+s.userID)
	exec(ctx, t, `INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1,$2,'CUSTOMER','ACTIVE')`, s.accountID, s.userID)

	// Every seed gets its own mints so the (chain, mint_address) uniqueness of
	// the asset registry never collides between tests; the inspector derives
	// ATAs from the stored mint, so the stored value must be exact. The native
	// asset is a single shared row ('native').
	if !opts.native {
		swap.InputMint = signingtest.PubkeyFromSeed("mint-in-" + newID())
	}
	swap.OutputMint = signingtest.PubkeyFromSeed("mint-out-" + newID())
	swap.Derive()
	s.raw = swap.Golden(opts.version)
	txHash = sha256.Sum256(s.raw)
	inputKind, inputMint := "SPL_TOKEN", swap.InputMint.String()
	if opts.native {
		inputKind, inputMint = "NATIVE", "native"
	}
	require.NoError(t, testDB.QueryRow(ctx, `INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, risk_class, status)
		VALUES ($1,'solana-mainnet',$2,$3,$4,$4,6,'SETTLEMENT','ACTIVE')
		ON CONFLICT (chain, mint_address) DO UPDATE SET name = EXCLUDED.name RETURNING id::text`,
		newID(), inputMint, inputKind, "IN"+tail(newID())).Scan(&s.inputAssetID))
	require.NoError(t, testDB.QueryRow(ctx, `INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, risk_class, status)
		VALUES ($1,'solana-mainnet',$2,'SPL_TOKEN',$3,$3,5,'STANDARD','ACTIVE') RETURNING id::text`,
		newID(), swap.OutputMint.String(), "OUT"+tail(newID())).Scan(&s.outputAssetID))

	s.venueID, s.exposureID, s.instrumentID, s.listingID = newID(), newID(), newID(), newID()
	exec(ctx, t, `INSERT INTO venues (id, code, name, kind, chain, status) VALUES ($1,$2,'Jupiter','DEX_AGGREGATOR','solana-mainnet','ACTIVE')`, s.venueID, "JUP-"+tail(s.venueID))
	exec(ctx, t, `INSERT INTO economic_exposures (id, kind, description, underlying_asset_id) VALUES ($1,'ASSET_PRICE','out price',$2)`, s.exposureID, s.outputAssetID)
	exec(ctx, t, `INSERT INTO instruments (id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id, settlement_asset_id, risk_class, status, active_from)
		VALUES ($1,'SPOT_PAIR',$2,$3,$4,$5,$5,'STANDARD','ACTIVE',$6)`, s.instrumentID, "OUT/IN-"+tail(s.instrumentID), s.exposureID, s.outputAssetID, s.inputAssetID, now.Add(-time.Hour))
	exec(ctx, t, `INSERT INTO venue_listings (id, venue_id, instrument_id, venue_native_id, network, base_precision, quote_precision, min_notional_quote, status)
		VALUES ($1,$2,$3,'jup:out/in','solana-mainnet',5,6,0,'ACTIVE')`, s.listingID, s.venueID, s.instrumentID)

	s.intentID, s.riskID, s.quoteID, s.planID, s.reservationID, s.walletID, s.orderID, s.attemptID = newID(), newID(), newID(), newID(), newID(), newID(), newID(), newID()
	contentHash := sha256.Sum256([]byte("intent:" + s.intentID))
	exec(ctx, t, `INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor, constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash)
		VALUES ($1,$2,'USER',$3,'ACQUIRE_NOTIONAL',$4,2500,'{}',$5,$6,'corr','LIVE','PLANNED',$7)`, s.intentID, s.accountID, s.userID, s.instrumentID, now, "idem-"+s.intentID, contentHash[:])
	constraints, _ := json.Marshal(map[string]any{"constraints": map[string]any{"max_slippage_bps": 100}})
	policyHash := sha256.Sum256([]byte("policy"))
	routeHash := sha256.Sum256([]byte("route"))
	exec(ctx, t, `INSERT INTO risk_decisions (id, intent_id, account_id, stage, policy_version, policy_hash, account_snapshot, market_snapshot, decision, reason_codes, resulting_constraints, evaluator_version, evaluated_at)
		VALUES ($1,$2,$3,'FINAL','risk/1',$4,'{}','{}',$5,'{}',$6,'eval/1',$7)`, s.riskID, s.intentID, s.accountID, policyHash[:], opts.riskDecision, constraints, now)
	expires := now.Add(2 * time.Minute)
	if opts.quoteExpired {
		expires = now.Add(-time.Minute)
	}
	exec(ctx, t, `INSERT INTO quotes (id, intent_id, provider, instrument_id, venue_listing_id, side, input_asset_id, input_quantity, output_asset_id, expected_output, minimum_output,
			effective_price_mantissa, effective_price_scale, price_impact_bps, slippage_bps, received_at, expires_at, route_hash, raw_response_hash)
		VALUES ($1,$2,'jupiter',$3,$4,'BUY',$5,$6,$7,$8,$9,1,0,5,$10,$11,$12,$13,$13)`,
		s.quoteID, s.intentID, s.instrumentID, s.listingID, s.inputAssetID, fmt.Sprint(swap.InAmount), s.outputAssetID,
		fmt.Sprint(swap.QuotedOut), fmt.Sprint(swap.MinOutput), int(swap.SlippageBPS), now, expires, routeHash[:])
	hard, _ := json.Marshal(map[string]any{
		"max_input_quantity": fmt.Sprint(swap.MaxInputDebit), "min_output_quantity": fmt.Sprint(swap.MinOutput),
		"max_slippage_bps": 100, "max_compute_units": 1_400_000,
		"allowed_program_ids": []string{swap.DEXProgram.String()},
	})
	var approvedAt *time.Time
	if opts.planStatus == "APPROVED" {
		approvedAt = &now
	}
	exec(ctx, t, `INSERT INTO execution_plans (id, intent_id, version, planner_version, status, hard_constraints, quote_id, risk_decision_id, plan_hash, dry_run, approved_at)
		VALUES ($1,$2,1,'planner/1',$3,$4,$5,$6,$7,false,$8)`, s.planID, s.intentID, opts.planStatus, hard, s.quoteID, s.riskID, s.planHash, approvedAt)
	exec(ctx, t, `INSERT INTO asset_reservations (id, account_id, asset_id, intent_id, actor_type, actor_id, quantity, usd_minor, status, idempotency_key, expires_at)
		VALUES ($1,$2,$3,$4,'USER',$5,$6,2500,$7,$8,$9)`, s.reservationID, s.accountID, s.inputAssetID, s.intentID, s.userID, fmt.Sprint(swap.MaxInputDebit), opts.reservation, "res-"+s.reservationID, now.Add(time.Hour))
	exec(ctx, t, `UPDATE trade_intents SET reservation_id = $2, risk_decision_id = $3, plan_id = $4 WHERE id = $1`, s.intentID, s.reservationID, s.riskID, s.planID)
	var delegatedAt *time.Time
	if opts.delegated {
		delegatedAt = &now
	}
	exec(ctx, t, `INSERT INTO wallets (id, account_id, provider, provider_wallet_id, chain, address, kind, status, delegation_verified_at)
		VALUES ($1,$2,'fake',$3,'solana-mainnet',$4,'EMBEDDED_DELEGATED',$5,$6)`, s.walletID, s.accountID, s.providerWalletID, swap.Wallet.String(), opts.walletStatus, delegatedAt)
	exec(ctx, t, `INSERT INTO orders (id, intent_id, plan_id, account_id, instrument_id, venue_listing_id, side, mode, status, input_asset_id, input_quantity, output_asset_id, min_output_quantity, reservation_id, quote_id, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,'BUY','LIVE','PLANNED',$7,$8,$9,$10,$11,$12,'corr')`,
		s.orderID, s.intentID, s.planID, s.accountID, s.instrumentID, s.listingID, s.inputAssetID, fmt.Sprint(swap.InAmount), s.outputAssetID, fmt.Sprint(swap.MinOutput), s.reservationID, s.quoteID)
	var rowHash []byte
	if opts.txHashOnRow {
		rowHash = txHash[:]
	}
	exec(ctx, t, `INSERT INTO execution_attempts (id, order_id, plan_id, attempt_no, wallet_id, provider, quote_id, unsigned_tx_hash, recent_blockhash, last_valid_block_height, simulation_ref, simulation_ok, status, correlation_id)
		VALUES ($1,$2,$3,1,$4,'jupiter',$5,$6,$7,$8,'sim://1',$9,$10,'corr')`,
		s.attemptID, s.orderID, s.planID, s.walletID, s.quoteID, rowHash, swap.Blockhash.String(), int64(swap.LastValidBlockHeight), opts.simulationOK, opts.attemptStatus)
	return s
}

func (s *seed) request() signing.Request {
	h := sha256.Sum256(s.raw)
	return signing.Request{
		AttemptID: s.attemptID, PlanID: s.planID, IntentID: s.intentID, RiskDecisionID: s.riskID, WalletID: s.walletID,
		UnsignedTx: s.raw, ExpectedTxHash: h[:], LookupTables: s.swap.LookupTables(), SimulationRef: "sim://1", CorrelationID: "corr-" + tail(s.attemptID),
	}
}

func newService(t *testing.T, fake *wallettest.Fake, chain *fakeChain) *signing.PGService {
	t.Helper()
	svc, err := signing.New(context.Background(), signing.Deps{
		DB: testDB, Audit: audit.NewWriterWithBuildVersion("itest"), Clock: clock.System(), Wallets: fake, Signer: fake,
		BlockHeights: chain, Simulations: chain, LookupTables: nil, Env: config.EnvTest, ServiceName: "execution-worker-itest",
		Policy: signing.DefaultPolicy(),
	})
	require.NoError(t, err)
	return svc
}

func chainFor(s *seed) *fakeChain {
	return &fakeChain{height: s.swap.CurrentBlockHeight, sim: s.swap.Simulation(s.raw), tables: s.swap.LookupTables()}
}

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func TestIntegration_SignApprovesGoldenAndNeverSignsTwice(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	s := seedChain(t, fake, defaultSeedOptions())
	svc := newService(t, fake, chainFor(s))

	d, signed, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	require.True(t, d.Approved, "reason codes: %v checks: %+v", d.ReasonCodes, d.Checks)
	assert.False(t, d.Replayed)
	assert.Len(t, d.Checks, 16)
	assert.NotEmpty(t, signed)
	assert.NotEmpty(t, d.ProviderSignRef)
	require.NoError(t, fake.Verify(s.providerWalletID, signed), "the fake really signed with the wallet key")
	assert.Equal(t, 1, fake.SignCalls())

	// Replay: same decision, same bytes, no second provider call.
	d2, signed2, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	assert.True(t, d2.Replayed)
	assert.Equal(t, d.ID, d2.ID)
	assert.Equal(t, signed, signed2)
	assert.Equal(t, 1, fake.SignCalls(), "never signs twice")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_decisions WHERE attempt_id = $1`, s.attemptID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_results WHERE attempt_id = $1`, s.attemptID))
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM security_events WHERE detail->>'attempt_id' = $1`, s.attemptID))
	assert.Equal(t, 2, countRows(t, `SELECT count(*) FROM audit_events WHERE resource_id = $1`, d.ID), "decision + signed events")

	got, err := svc.GetDecision(ctx, d.ID)
	require.NoError(t, err)
	assert.True(t, got.Approved)
	assert.Equal(t, d.ProviderSignRef, got.ProviderSignRef)
	assert.Equal(t, inspect.InspectorVersion, got.InspectorVersion)

	// Concurrent replays still yield one provider call.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Sign(ctx, s.request())
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, fake.SignCalls())

	// The immutable decision cannot be altered by the app role.
	_, err = testDB.Exec(ctx, `UPDATE signing_decisions SET decision = 'REJECTED' WHERE id = $1`, d.ID)
	require.Error(t, err)
}

func TestIntegration_SignRejectsMutatedTransaction(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	s := seedChain(t, fake, defaultSeedOptions())
	// Mutate: an extra System transfer from the wallet to a foreign account.
	ixs := append(s.swap.GoldenInstructions(), solana.NewInstruction(signingtest.SystemProgram, solana.AccountMetaSlice{
		solana.NewAccountMeta(s.swap.Wallet, true, true), solana.NewAccountMeta(s.swap.Foreign, true, false),
	}, signingtest.SystemTransferData(1_000)))
	s.raw = s.swap.MustBuild(inspect.VersionV0, ixs)
	// The attempt row carries the golden hash; the mutated bytes also fail TX_HASH_MISMATCH,
	// so point the row at the mutated bytes to exercise the inspector path.
	h := sha256.Sum256(s.raw)
	exec(ctx, t, `UPDATE execution_attempts SET unsigned_tx_hash = $2 WHERE id = $1`, s.attemptID, h[:])
	svc := newService(t, fake, chainFor(s))

	d, signed, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	assert.False(t, d.Approved)
	assert.Nil(t, signed)
	assert.NotEmpty(t, d.ReasonCodes)
	assert.Equal(t, 0, fake.SignCalls(), "rejected transactions never reach the provider")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM security_events WHERE kind = 'signing_rejection' AND detail->>'attempt_id' = $1`, s.attemptID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM audit_events WHERE action = 'signing.decision.rejected' AND resource_id = $1`, d.ID))

	// Replaying a rejection never re-inspects into an approval.
	d2, signed2, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	assert.True(t, d2.Replayed)
	assert.False(t, d2.Approved)
	assert.Nil(t, signed2)
	assert.Equal(t, 0, fake.SignCalls())
}

func TestIntegration_LinkageFailuresReject(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	f := false
	cases := map[string]struct {
		mutate func(o *seedOptions)
		reqMut func(s *seed, r *signing.Request)
		want   string
	}{
		"plan not approved":         {mutate: func(o *seedOptions) { o.planStatus = "DRAFT" }, want: signing.ReasonPlanNotApproved},
		"risk rejected":             {mutate: func(o *seedOptions) { o.riskDecision = "REJECT" }, want: signing.ReasonRiskNotAllowed},
		"wallet suspended":          {mutate: func(o *seedOptions) { o.walletStatus = "SUSPENDED" }, want: signing.ReasonWalletInactive},
		"delegation not verified":   {mutate: func(o *seedOptions) { o.delegated = false }, want: signing.ReasonDelegationNotVerified},
		"reservation released":      {mutate: func(o *seedOptions) { o.reservation = "RELEASED" }, want: signing.ReasonReservationInactive},
		"attempt already signed":    {mutate: func(o *seedOptions) { o.attemptStatus = "SIGNED" }, want: signing.ReasonAttemptState},
		"quote expired":             {mutate: func(o *seedOptions) { o.quoteExpired = true }, want: signing.ReasonQuoteExpired},
		"simulation failed on row":  {mutate: func(o *seedOptions) { o.simulationOK = &f }, want: inspect.ReasonSimulationFailed},
		"no simulation on row":      {mutate: func(o *seedOptions) { o.simulationOK = nil }, want: inspect.ReasonSimulationMissing},
		"caller hash mismatch":      {reqMut: func(s *seed, r *signing.Request) { r.ExpectedTxHash = make([]byte, 32) }, want: signing.ReasonTxHashMismatch},
		"caller plan hash mismatch": {reqMut: func(s *seed, r *signing.Request) { r.ClaimedPlanHash = make([]byte, 32) }, want: signing.ReasonCallerExpectation},
		"lookup tables withheld":    {reqMut: func(s *seed, r *signing.Request) { r.LookupTables = nil }, want: inspect.ReasonLookupTableUnresolved},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake, err := wallettest.New(config.EnvTest)
			require.NoError(t, err)
			opts := defaultSeedOptions()
			if tc.mutate != nil {
				tc.mutate(&opts)
			}
			s := seedChain(t, fake, opts)
			svc := newService(t, fake, chainFor(s))
			req := s.request()
			if tc.reqMut != nil {
				tc.reqMut(s, &req)
			}
			d, signed, err := svc.Sign(ctx, req)
			require.NoError(t, err)
			assert.False(t, d.Approved)
			assert.Nil(t, signed)
			assert.Contains(t, d.ReasonCodes, tc.want, "codes %v", d.ReasonCodes)
			assert.Equal(t, 0, fake.SignCalls())
		})
	}
	t.Run("mismatched identifiers reject", func(t *testing.T) {
		fake, err := wallettest.New(config.EnvTest)
		require.NoError(t, err)
		s := seedChain(t, fake, defaultSeedOptions())
		other := seedChain(t, fake, defaultSeedOptions())
		svc := newService(t, fake, chainFor(s))
		req := s.request()
		req.PlanID = other.planID
		req.IntentID = other.intentID
		req.RiskDecisionID = other.riskID
		d, _, err := svc.Sign(ctx, req)
		require.NoError(t, err)
		assert.False(t, d.Approved)
		assert.Contains(t, d.ReasonCodes, signing.ReasonAttemptLinkage)
	})
	t.Run("unknown attempt is an error, not a decision", func(t *testing.T) {
		fake, err := wallettest.New(config.EnvTest)
		require.NoError(t, err)
		s := seedChain(t, fake, defaultSeedOptions())
		svc := newService(t, fake, chainFor(s))
		req := s.request()
		req.AttemptID = newID()
		_, _, err = svc.Sign(ctx, req)
		require.Error(t, err)
		assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	})
}

func TestIntegration_ProviderFailureThenRecovery(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	s := seedChain(t, fake, defaultSeedOptions())
	svc := newService(t, fake, chainFor(s))

	fake.FailWith(errs.New(errs.CodeProviderUnavailable, "privy down"))
	d, signed, err := svc.Sign(ctx, s.request())
	require.Error(t, err)
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	assert.True(t, d.Approved, "the decision is recorded even though signing failed")
	assert.Nil(t, signed)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_decisions WHERE attempt_id = $1`, s.attemptID))
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM signing_results WHERE attempt_id = $1`, s.attemptID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM audit_events WHERE action = 'signing.provider_failed' AND resource_id = $1`, d.ID))

	// Recovery: the same attempt completes signing under the same decision with the same idempotency key.
	fake.FailWith(nil)
	d2, signed2, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	assert.Equal(t, d.ID, d2.ID)
	assert.True(t, d2.Replayed)
	assert.NotEmpty(t, signed2)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_decisions WHERE attempt_id = $1`, s.attemptID), "no second decision")
	calls := fake.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, calls[0].IdempotencyKey, calls[1].IdempotencyKey)
	assert.Equal(t, s.attemptID, calls[1].IdempotencyKey)

	// Provider timeout honors the context.
	s2 := seedChain(t, fake, defaultSeedOptions())
	svc2 := newService(t, fake, chainFor(s2))
	fake.Delay(2 * time.Second)
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, _, err = svc2.Sign(tctx, s2.request())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errs.CodeOf(err) == errs.CodeProviderUnavailable, "got %v", err)
}

func TestIntegration_NativeInputAndGRPCAdapter(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	opts := defaultSeedOptions()
	opts.native = true
	opts.version = inspect.VersionLegacy
	s := seedChain(t, fake, opts)
	svc := newService(t, fake, chainFor(s))
	client := signing.NewInProcessClient(signing.NewServer(svc))
	req := s.request()
	resp, err := client.Sign(ctx, wireRequest(req))
	require.NoError(t, err)
	require.True(t, resp.GetApproved(), "%v", resp.GetReasonCodes())
	assert.NotEmpty(t, resp.GetSignedTransaction())
	got, err := client.GetDecision(ctx, wireGet(resp.GetDecisionId()))
	require.NoError(t, err)
	assert.Equal(t, resp.GetDecisionId(), got.GetDecision().GetDecisionId())
	assert.Equal(t, resp.GetProviderSignRef(), got.GetDecision().GetProviderSignRef())
}

func TestIntegration_WalletRepositoryAndTransitionBinding(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	s := seedChain(t, fake, defaultSeedOptions())
	repo := wallet.NewRepository()
	wid, err := wallet.ParseWalletID(s.walletID)
	require.NoError(t, err)

	w, err := repo.Get(ctx, testDB, wid)
	require.NoError(t, err)
	assert.Equal(t, wallet.StatusActive, w.Status)
	assert.True(t, w.DelegationVerified())

	// A bare status update is refused at COMMIT (migration 00615 binding).
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE wallets SET status = 'SUSPENDED' WHERE id = $1`, wid)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))

	// Agent actors and empty reasons are refused before any SQL.
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Transition(ctx, tx, wid, wallet.StatusChange{To: wallet.StatusSuspended, ActorType: "AGENT", ActorID: "a", Reason: "x"}, time.Now())
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Transition writes the row and the transition together.
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		w, err := repo.Transition(ctx, tx, wid, wallet.StatusChange{To: wallet.StatusSuspended, ActorType: "OPERATOR", ActorID: s.userID, Reason: "compliance hold", CorrelationID: "c1"}, time.Now())
		if err != nil {
			return err
		}
		assert.Equal(t, wallet.StatusSuspended, w.Status)
		return nil
	}))
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM wallet_status_transitions WHERE wallet_id = $1 AND to_status = 'SUSPENDED'`, wid))

	// Illegal transition and terminal REVOKED.
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Transition(ctx, tx, wid, wallet.StatusChange{To: wallet.StatusRevoked, ActorType: "OPERATOR", ActorID: s.userID, Reason: "user revoked delegation"}, time.Now())
		return err
	}))
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Transition(ctx, tx, wid, wallet.StatusChange{To: wallet.StatusActive, ActorType: "OPERATOR", ActorID: s.userID, Reason: "oops"}, time.Now())
		return err
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// A revoked wallet can never sign.
	svc := newService(t, fake, chainFor(s))
	d, signed, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	assert.False(t, d.Approved)
	assert.Nil(t, signed)
	assert.Contains(t, d.ReasonCodes, signing.ReasonWalletInactive)

	// Create + MarkDelegationVerified round-trip.
	acct := w.AccountID
	created, err := repo.Create(ctx, testDB, wallet.Wallet{
		AccountID: acct, Provider: "fake", ProviderWalletID: "fake-" + tail(newID()), Chain: "solana-devnet",
		Address: signingtest.PubkeyFromSeed("addr-" + newID()).String(), Kind: wallet.KindEmbeddedDelegated, Status: wallet.StatusActive,
	})
	require.NoError(t, err)
	assert.False(t, created.DelegationVerified())
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		st, err := fake.VerifyDelegation(ctx, created.ProviderWalletID)
		if err != nil {
			return err
		}
		capability, err := fake.Capabilities(ctx)
		if err != nil {
			return err
		}
		updated, err := repo.MarkDelegationVerified(ctx, tx, created.ID, st, capability, time.Now())
		if err != nil {
			return err
		}
		assert.True(t, updated.DelegationVerified())
		var caps map[string]any
		require.NoError(t, json.Unmarshal(updated.Capabilities, &caps))
		assert.Equal(t, "VERIFIED", caps["delegated_signing"])
		assert.Equal(t, "CODE_COMPLETE", caps["verification_label"])
		return nil
	}))
	list, err := repo.ListByAccount(ctx, testDB, acct.String())
	require.NoError(t, err)
	assert.Len(t, list, 2)
	_, err = repo.Create(ctx, testDB, created)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
}

// wireRequest converts a domain request into its gRPC form (test helper).
func wireRequest(r signing.Request) *signingv1.SignRequest {
	out := &signingv1.SignRequest{
		AttemptId: r.AttemptID, PlanId: r.PlanID, IntentId: r.IntentID, RiskDecisionId: r.RiskDecisionID, WalletId: r.WalletID,
		UnsignedTransaction: r.UnsignedTx, ExpectedTransactionHash: r.ExpectedTxHash, SimulationRef: r.SimulationRef, CorrelationId: r.CorrelationID,
	}
	for addr, keys := range r.LookupTables {
		out.LookupTables = append(out.LookupTables, &signingv1.LookupTable{Address: addr, AccountKeys: keys})
	}
	return out
}

func wireGet(decisionID string) *signingv1.GetDecisionRequest {
	return &signingv1.GetDecisionRequest{DecisionId: decisionID}
}
