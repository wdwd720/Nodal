//go:build integration

package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/security"
)

var testAppURL = os.Getenv("CP_TEST_DATABASE_URL")

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	if testAppURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; provision one with `go run ./scripts/testdb -name reconciliation -export`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "reconciliation-itest", MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// capitalEmitter records the events capital.Service emits. It does not write
// to the outbox: capital.TopicReservationLocked is not in the internal/event
// topic registry, so a real Enqueue would fail (reported to the integrator).
// Capital's own suite covers its outbox behavior; here the events only need
// to be observable.
type capitalEmitter struct {
	mu     sync.Mutex
	topics []string
}

func (c *capitalEmitter) Emit(_ context.Context, _ pgx.Tx, topic string, _ any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topics = append(c.topics, topic)
	return nil
}

// noSubmitAdapter is an execution adapter that fails the test if anything
// tries to submit. PART 48: the recovery path must never send a duplicate.
type noSubmitAdapter struct {
	t       *testing.T
	name    string
	status  execution.ExecutionStatus
	err     error
	submits int
}

func (a *noSubmitAdapter) Name() string { return a.name }

func (a *noSubmitAdapter) Quote(context.Context, execution.QuoteRequest) (execution.QuoteSnapshot, error) {
	a.t.Fatal("reconciliation must not request quotes")
	return execution.QuoteSnapshot{}, nil
}

func (a *noSubmitAdapter) ValidateQuote(context.Context, execution.QuoteSnapshot) error { return nil }

func (a *noSubmitAdapter) Build(context.Context, execution.BuildRequest) (execution.UnsignedAction, error) {
	a.t.Fatal("reconciliation must not build transactions")
	return execution.UnsignedAction{}, nil
}

func (a *noSubmitAdapter) Submit(context.Context, execution.SignedSubmission) (execution.SubmissionResult, error) {
	a.submits++
	a.t.Fatal("PART 48 violated: the recovery path submitted a transaction")
	return execution.SubmissionResult{}, nil
}

func (a *noSubmitAdapter) Status(context.Context, execution.ExternalReference) (execution.ExecutionStatus, error) {
	return a.status, a.err
}

func (a *noSubmitAdapter) Cancel(context.Context, execution.ExternalReference) error { return nil }

func (a *noSubmitAdapter) Reconcile(context.Context, execution.ReconcileScope) ([]execution.ExternalExecutionEvent, error) {
	return nil, nil
}

// fixture is one funded account with a SOL/USDC instrument, a wallet, a
// capital reservation, an order and an attempt: everything the PART 48/49
// recovery path touches.
type fixture struct {
	t   *testing.T
	ctx context.Context
	d   *db.DB
	clk *clock.Fake

	records     *Repository
	engine      *Engine
	metrics     *Metrics
	orders      *execution.Repository
	attempts    *execution.AttemptRepository
	ledgerSvc   *ledger.Service
	lots        *positions.Engine
	capitalS    *capital.Service
	capitalEmit *capitalEmitter
	adminSvc    *admin.Service

	sim       *chaintest.Chain
	primary   *chaintest.Fake
	secondary *chaintest.Fake
	adapter   *noSubmitAdapter

	userID     string
	account    accounts.AccountID
	usdc, sol  assets.Asset
	instrument instruments.Instrument
	listing    instruments.VenueListing
	walletID   string
	walletAddr string
	intentID   string
	planID     string
	quoteID    string
	suffix     string
}

const venueName = "jupiter"

func newFixture(t *testing.T, d *db.DB) *fixture {
	t.Helper()
	ctx := t.Context()
	clk := clock.NewFake(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	f := &fixture{t: t, ctx: ctx, d: d, clk: clk}

	outbox := event.NewOutbox(clk)
	writer := audit.NewWriter()
	f.records = NewRepository(clk, outbox, writer)
	f.orders = execution.NewRepository(clk, outbox, writer)
	f.attempts = execution.NewAttemptRepository(clk, outbox, writer)
	f.ledgerSvc = ledger.NewService(clk, "itest")
	f.ledgerSvc.AllowSeedPostings()
	f.lots = positions.NewEngine()
	f.capitalEmit = &capitalEmitter{}
	f.capitalS = capital.NewService(clk, f.capitalEmit)
	f.adminSvc = admin.NewService(clk, writer)
	f.metrics = NoopMetrics()

	f.sim = chaintest.NewChain(clk)
	f.primary = f.sim.NewObserver("helius", 0)
	f.secondary = f.sim.NewObserver("rpc-fallback", 0)
	f.adapter = &noSubmitAdapter{t: t, name: venueName, status: execution.ExecutionStatus{State: execution.ExternalNotFound}}

	suffix := id.New[id.Any]().String()
	f.suffix = suffix[len(suffix)-12:]

	arepo := accounts.NewRepository()
	u, err := arepo.CreateUser(ctx, d, "itest", "sub-"+suffix, nil)
	require.NoError(t, err)
	f.userID = u.ID.String()
	a, err := arepo.CreateAccount(ctx, d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	f.account = a.ID

	areg := assets.NewRepository()
	f.usdc, err = areg.Create(ctx, d, assets.Asset{
		Chain: "solana-devnet", MintAddress: "usdc-" + f.suffix, Kind: assets.KindSPLToken, Symbol: "USDC",
		Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD",
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	f.sol, err = areg.Create(ctx, d, assets.Asset{
		Chain: "solana-devnet", MintAddress: "sol-" + f.suffix, Kind: assets.KindSPLToken, Symbol: "SOL",
		Name: "Solana", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)

	irepo := instruments.NewRepository()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := irepo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: f.sol.ID, Quote: f.usdc.ID, Settlement: f.usdc.ID, CanonicalName: "SOL/USDC-" + f.suffix,
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: clk.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		f.instrument = inst
		venue, err := irepo.CreateVenue(ctx, tx, instruments.Venue{
			Code: "JUP-" + f.suffix, Name: "Jupiter", Kind: instruments.VenueDEXAggregator,
			Chain: "solana-devnet", Status: instruments.VenueActive,
		})
		if err != nil {
			return err
		}
		listing, err := irepo.CreateListing(ctx, tx, instruments.VenueListing{
			VenueID: venue.ID, InstrumentID: inst.ID, VenueNativeID: "SOL-USDC", Network: "solana-devnet",
			BaseMint: f.sol.MintAddress, QuoteMint: f.usdc.MintAddress, BasePrecision: 9, QuotePrecision: 6,
			MinNotionalQuote: money.QuantityFromInt64(1_000_000), Status: instruments.VenueActive,
		})
		if err != nil {
			return err
		}
		f.listing = listing
		return nil
	}))

	hash := sha256.Sum256([]byte(suffix))
	f.intentID = id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor,
			constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash)
		VALUES ($1,$2,'USER',$3,'ACQUIRE_NOTIONAL',$4,5000,'{}',now(),$5,$6,'LIVE','PLANNED',$7)`,
		f.intentID, f.account, f.userID, f.instrument.ID, "idem-"+f.intentID, "corr-"+f.suffix, hash[:])
	require.NoError(t, err)

	f.planID = id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO execution_plans (id, intent_id, version, planner_version, status, hard_constraints, plan_hash)
		VALUES ($1,$2,1,'itest','APPROVED','{}',$3)`, f.planID, f.intentID, hash[:])
	require.NoError(t, err)

	f.quoteID = id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO quotes (id, intent_id, provider, instrument_id, venue_listing_id, side, input_asset_id, input_quantity,
			output_asset_id, expected_output, minimum_output, effective_price_mantissa, effective_price_scale,
			price_impact_bps, slippage_bps, received_at, expires_at, route_hash, raw_response_hash)
		VALUES ($1,$2,$3,$4,$5,'BUY',$6,49500000,$7,250000000,240000000,150,0,5,50,now(),now() + interval '30 seconds',$8,$8)`,
		f.quoteID, f.intentID, venueName, f.instrument.ID, f.listing.ID, f.usdc.ID, f.sol.ID, hash[:])
	require.NoError(t, err)

	f.walletID = id.New[id.Any]().String()
	f.walletAddr = "Wa11et" + f.suffix
	_, err = d.Exec(ctx, `INSERT INTO wallets (id, account_id, provider, provider_wallet_id, chain, address, kind, status)
		VALUES ($1,$2,'privy',$3,'solana-devnet',$4,'EMBEDDED_DELEGATED','ACTIVE')`,
		f.walletID, f.account, "pw-"+f.walletID, f.walletAddr)
	require.NoError(t, err)

	f.engine = f.newEngine(Config{})
	return f
}

// newEngine builds the engine with the fixture's defaults, overridden by any
// non-zero field of cfg.
func (f *fixture) newEngine(cfg Config) *Engine {
	f.t.Helper()
	base := Config{
		DB: f.d, Clock: f.clk, Records: f.records, Policy: DefaultPolicy(),
		Orders: f.orders, Attempts: f.attempts, Ledger: f.ledgerSvc, Positions: f.lots,
		Reservations: f.capitalS, Approvals: f.adminSvc, Assets: assets.NewRepository(),
		Wallets:     NewSQLWallets(),
		Observers:   Observers{Primary: f.primary, Secondary: f.secondary, Policy: chain.DefaultPolicy()},
		Adapters:    Adapters{venueName: f.adapter},
		NativeAsset: f.sol.ID,
		Metrics:     f.metrics,
	}
	if cfg.Policy.Version != "" {
		base.Policy = cfg.Policy
	}
	if cfg.Observers.Primary != nil || cfg.Observers.Secondary != nil {
		base.Observers = cfg.Observers
	}
	if cfg.Metrics != nil {
		base.Metrics = cfg.Metrics
	}
	if cfg.Adapters != nil {
		base.Adapters = cfg.Adapters
	}
	e, err := NewEngine(base)
	require.NoError(f.t, err)
	return e
}

// fund posts a SEED transaction crediting the customer's WALLET with qty of
// asset, and opens the matching position lot so lots and ledger agree from
// the start.
func (f *fixture) fund(asset assets.Asset, qty money.Quantity, usdMinor int64) {
	f.t.Helper()
	require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		res, err := f.ledgerSvc.Post(ctx, tx, ledger.Posting{
			Kind:           ledger.KindSeed,
			IdempotencyKey: "seed:" + f.account.String() + ":" + asset.ID.String(),
			Reference:      ledger.FinancialEventReference{Type: "seed", ID: f.account.String()},
			EffectiveAt:    f.clk.Now(),
			Description:    "itest seed",
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(f.account, ledger.CodeWallet, asset.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(f.account, ledger.CodeCapital, asset.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		if err != nil {
			return err
		}
		_, err = f.lots.Acquire(ctx, tx, positions.AcquireLot{
			AccountID: f.account, AssetID: asset.ID, Quantity: qty, AcquiredAt: f.clk.Now(),
			Cost: money.USDFromMinor(usdMinor), BasisSource: "funding", ValuationSource: "itest",
			AcquisitionRef: positions.Ref{Type: "seed", ID: f.account.String()},
			JournalTxID:    res.TransactionID.Untyped(),
		})
		return err
	}))
}

// reserve creates an ACTIVE capital reservation of qty for the intent.
func (f *fixture) reserve(qty money.Quantity, usdMinor int64) capital.Reservation {
	f.t.Helper()
	var r capital.Reservation
	require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.capitalS.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: f.account.String(), AssetID: f.usdc.ID, Quantity: qty, USDMinor: usdMinor,
			IntentID: f.intentID, ActorType: security.ActorUser, ActorID: f.userID,
			IdempotencyKey: "res-" + f.intentID, TTL: time.Hour, Reason: "itest",
		})
		return err
	}))
	return r
}

// submittingOrder drives an order to SUBMITTING with a locked reservation and
// an attempt in SUBMITTING carrying a signature: exactly the state a process
// is in at the instant it crashes mid-submit.
func (f *fixture) submittingOrder(res capital.Reservation, inputQty, minOutput money.Quantity, sig string, lastValidBlockHeight int64) (execution.Order, execution.Attempt) {
	f.t.Helper()
	var (
		ord execution.Order
		att execution.Attempt
	)
	require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ord, err = f.orders.Create(ctx, tx, execution.Order{
			ID: execution.NewOrderID(), IntentID: f.intentID, PlanID: f.planID, AccountID: f.account,
			InstrumentID: f.instrument.ID.String(), VenueListingID: f.listing.ID.String(),
			Side: execution.SideBuy, Mode: execution.ModeLive,
			InputAssetID: f.usdc.ID, InputQuantity: inputQty,
			OutputAssetID: f.sol.ID, MinOutputQuantity: minOutput,
			ReservationID: res.ID.String(), QuoteID: f.quoteID, CorrelationID: "corr-" + f.suffix,
		})
		return err
	}))
	for _, to := range []execution.OrderStatus{
		execution.OrderValidated, execution.OrderCapitalReserved, execution.OrderPlanned, execution.OrderSubmitting,
	} {
		require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ord, err = f.orders.Transition(ctx, tx, ord.ID, to, execution.TransitionEvidence{Reason: "itest"})
			return err
		}))
	}
	require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return f.capitalS.LockForOrder(ctx, tx, res.ID, ord.ID.String())
	}))
	require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		att, err = f.attempts.Create(ctx, tx, execution.Attempt{
			ID: execution.NewAttemptID(), OrderID: ord.ID, PlanID: f.planID, WalletID: f.walletID,
			Provider: venueName, QuoteID: f.quoteID, CorrelationID: ord.CorrelationID,
			RecentBlockhash: "bh-" + f.suffix, LastValidBlockHeight: &lastValidBlockHeight,
		})
		return err
	}))
	for _, to := range []execution.AttemptStatus{
		execution.AttemptInspected, execution.AttemptSigningRequested, execution.AttemptSigned, execution.AttemptSubmitting,
	} {
		status := to
		require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			att, err = f.attempts.Update(ctx, tx, att.ID, execution.AttemptPatch{Status: &status, Reason: "itest"})
			return err
		}))
	}
	require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		att, err = f.attempts.SetSignature(ctx, tx, att.ID, sig, []byte("signed-"+sig))
		return err
	}))
	return ord, att
}

// land publishes the swap on the simulated chain: the external truth the
// crashed process never got to see.
func (f *fixture) land(sig string, inQty, outQty, fee money.Quantity) chain.TxObservation {
	f.t.Helper()
	obs := f.sim.Swap(f.walletAddr, f.usdc.MintAddress, f.sol.MintAddress, inQty, outQty, fee)
	obs.Signature = sig
	landed := f.sim.Land(obs)
	f.sim.Advance(64)
	return landed
}

func (f *fixture) balance(code ledger.Code, asset assets.AssetID) money.Quantity {
	f.t.Helper()
	q, err := f.ledgerSvc.Balance(f.ctx, f.d, ledger.CustomerAccount(f.account, code, asset))
	require.NoError(f.t, err)
	return q
}

func (f *fixture) fills(orderID execution.OrderID) []execution.Fill {
	f.t.Helper()
	out, err := f.orders.ListFills(f.ctx, f.d, orderID)
	require.NoError(f.t, err)
	return out
}

func (f *fixture) countRows(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.d.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

func (f *fixture) reservation(rid capital.ReservationID) capital.Reservation {
	f.t.Helper()
	r, err := f.capitalS.Get(f.ctx, f.d, rid)
	require.NoError(f.t, err)
	return r
}

func (f *fixture) reservedTotal() money.Quantity {
	f.t.Helper()
	av, err := f.capitalS.Availability(f.ctx, f.d, f.account.String(), f.usdc.ID, f.clk.Now())
	require.NoError(f.t, err)
	return av.Reserved
}

// operatorCtx returns a context whose principal is a step-up-fresh operator
// with the given roles.
func (f *fixture) operatorCtx(subject string, roles ...security.Role) context.Context {
	return security.WithPrincipal(f.ctx, security.Principal{
		SubjectID: subject, ActorType: security.ActorOperator, Roles: roles,
		SessionID: "sess-" + subject, AuthTime: f.clk.Now().Add(-time.Minute), AMR: []string{"mfa"},
	})
}

// breakGlassCtx returns a context whose principal holds a live BREAK_GLASS
// elevation: the only way to hold reconciliation:approve. The elevation is
// measured against the wall clock because security.Require is.
func (f *fixture) breakGlassCtx(subject string) context.Context {
	// The elevation must be live against both clocks: admin.Service judges it
	// with the injected (fake) clock, security.Require with the wall clock.
	until := f.clk.Now().Add(time.Hour)
	if wall := time.Now().UTC().Add(time.Hour); wall.After(until) {
		until = wall
	}
	return security.WithPrincipal(f.ctx, security.Principal{
		SubjectID: subject, ActorType: security.ActorOperator,
		Roles:     []security.Role{security.RoleAdmin, security.RoleBreakGlass},
		SessionID: "sess-" + subject, AuthTime: f.clk.Now().Add(-time.Minute), AMR: []string{"mfa"},
		BreakGlassUntil: &until,
	})
}

// agentCtx returns an AGENT principal: the one actor that may never resolve
// anything.
func (f *fixture) agentCtx(agentID string) context.Context {
	return security.WithPrincipal(f.ctx, security.Principal{
		SubjectID: agentID, ActorType: security.ActorAgent,
		AccountIDs: []string{f.account.String()}, AuthTime: f.clk.Now(),
	})
}

// newOperator inserts a user row so an operator subject id is a real user.
func (f *fixture) newOperator(label string) string {
	f.t.Helper()
	u, err := accounts.NewRepository().CreateUser(f.ctx, f.d, "itest", "op-"+label+"-"+id.New[id.Any]().String(), nil)
	require.NoError(f.t, err)
	return u.ID.String()
}

func (f *fixture) transitions(recordID RecordID) []Transition {
	f.t.Helper()
	out, err := f.records.ListTransitions(f.ctx, f.d, recordID)
	require.NoError(f.t, err)
	return out
}

// attemptTransitions returns the to_status values of an attempt's outbox
// events, oldest first. Attempts have no transitions table; the outbox is the
// record of every status change.
func (f *fixture) attemptTransitions(attemptID execution.AttemptID) []string {
	f.t.Helper()
	rows, err := f.d.Query(f.ctx, `SELECT payload->>'to' FROM outbox_events
		WHERE aggregate_type = 'execution_attempt' AND aggregate_id = $1 ORDER BY recorded_at, id`, attemptID.String())
	require.NoError(f.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s *string
		require.NoError(f.t, rows.Scan(&s))
		if s != nil {
			out = append(out, *s)
		}
	}
	require.NoError(f.t, rows.Err())
	return out
}

// orderTransitions returns an order's to_status values, oldest first.
func (f *fixture) orderTransitions(orderID execution.OrderID) []string {
	f.t.Helper()
	trs, err := f.orders.ListTransitions(f.ctx, f.d, orderID)
	require.NoError(f.t, err)
	out := make([]string, 0, len(trs))
	for _, t := range trs {
		out = append(out, string(t.To))
	}
	return out
}

// auditActions returns the audit actions written about a record, oldest first.
func (f *fixture) auditActions(recordID RecordID) []string {
	f.t.Helper()
	rows, err := f.d.Query(f.ctx, `SELECT action FROM audit_events
		WHERE resource_type = 'reconciliation_record' AND resource_id = $1 ORDER BY stream_seq`, recordID.String())
	require.NoError(f.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(f.t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(f.t, rows.Err())
	return out
}

// auditPayload returns the payload of the newest audit event with the given
// action about a record.
func (f *fixture) auditPayload(recordID RecordID, action string) map[string]any {
	f.t.Helper()
	var body []byte
	require.NoError(f.t, f.d.QueryRow(f.ctx, `SELECT payload FROM audit_events
		WHERE resource_type = 'reconciliation_record' AND resource_id = $1 AND action = $2
		ORDER BY stream_seq DESC LIMIT 1`, recordID.String(), action).Scan(&body))
	out := map[string]any{}
	require.NoError(f.t, json.Unmarshal(body, &out))
	return out
}

// requireAuditChainIntact verifies the account's hash-chained audit stream.
func (f *fixture) requireAuditChainIntact() {
	f.t.Helper()
	rep, err := audit.NewVerifier().VerifyStream(f.ctx, f.d, audit.AccountStream(f.account.String()))
	require.NoError(f.t, err)
	require.True(f.t, rep.OK, "audit chain broken: %s", rep.Reason)
	require.Positive(f.t, rep.Events)
}

// reserveWithKey is reserve with an explicit idempotency key, so a test can
// hold two independent reservations on one account.
func (f *fixture) reserveWithKey(qty money.Quantity, usdMinor int64, key string) capital.Reservation {
	f.t.Helper()
	var r capital.Reservation
	require.NoError(f.t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.capitalS.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: f.account.String(), AssetID: f.usdc.ID, Quantity: qty, USDMinor: usdMinor,
			IntentID: f.intentID, ActorType: security.ActorUser, ActorID: f.userID,
			IdempotencyKey: key, TTL: time.Hour, Reason: "itest",
		})
		return err
	}))
	return r
}

// seedChainBalances gives the simulated wallet enough of both assets that a
// simulated swap cannot drive a balance below zero (which a real chain never
// does, and which the observations table forbids).
func (f *fixture) seedChainBalances() {
	f.t.Helper()
	for _, b := range []chain.BalanceObservation{
		{
			Owner: f.walletAddr, Mint: f.usdc.MintAddress, TokenAccount: "ata-usdc-" + f.suffix,
			Amount: money.QuantityFromInt64(fundedUSDC), Decimals: 6, DecimalsKnown: true,
		},
		{
			Owner: f.walletAddr, Mint: f.sol.MintAddress, TokenAccount: "ata-sol-" + f.suffix,
			Amount: money.QuantityFromInt64(1_000_000_000), Decimals: 9, DecimalsKnown: true,
		},
	} {
		f.sim.SetBalance(b)
	}
}
