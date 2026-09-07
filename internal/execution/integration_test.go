//go:build integration

package execution

import (
	"context"
	"crypto/sha256"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var testAppURL = os.Getenv("CP_TEST_DATABASE_URL")

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	if testAppURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test (provision one with `go run ./scripts/testdb -name execution`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "execution-itest", MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// fixture is one account with a SOL/USDC instrument, a listing, an intent, a
// plan row, a reservation, a quote and a wallet: everything an order's
// foreign keys need.
type fixture struct {
	d           *db.DB
	clk         *clock.Fake
	repo        *Repository
	attempts    *AttemptRepository
	account     accounts.AccountID
	usdc, sol   assets.Asset
	instrument  instruments.Instrument
	listing     instruments.VenueListing
	intentID    string
	planID      string
	reservation string
	quoteID     string
	walletID    string
}

func newFixture(t *testing.T, d *db.DB) *fixture {
	t.Helper()
	ctx := t.Context()
	f := &fixture{d: d, clk: clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))}
	f.repo = NewRepository(f.clk, event.NewOutbox(f.clk), audit.NewWriter())
	f.attempts = NewAttemptRepository(f.clk, event.NewOutbox(f.clk), audit.NewWriter())

	arepo := accounts.NewRepository()
	u, err := arepo.CreateUser(ctx, d, "itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := arepo.CreateAccount(ctx, d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	f.account = a.ID
	suffix := id.New[id.Any]().String()
	suffix = suffix[len(suffix)-12:] // the random tail; the head is a millisecond timestamp shared across fixtures
	f.usdc, err = assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: "solana-devnet", MintAddress: "usdc-" + suffix, Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USD Coin", Decimals: 6,
		IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	f.sol, err = assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: "solana-devnet", MintAddress: "sol-" + suffix, Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "SOL", Name: "Solana", Decimals: 9,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	irepo := instruments.NewRepository()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := irepo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: f.sol.ID, Quote: f.usdc.ID, Settlement: f.usdc.ID, CanonicalName: "SOL/USDC-" + suffix, RiskClass: assets.RiskMajor,
			Status: assets.StatusActive, ActiveFrom: f.clk.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		f.instrument = inst
		venue, err := irepo.CreateVenue(ctx, tx, instruments.Venue{Code: "JUP-" + suffix, Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-devnet", Status: instruments.VenueActive})
		if err != nil {
			return err
		}
		listing, err := irepo.CreateListing(ctx, tx, instruments.VenueListing{
			VenueID: venue.ID, InstrumentID: inst.ID, VenueNativeID: "SOL-USDC", Network: "solana-devnet", BaseMint: f.sol.MintAddress, QuoteMint: f.usdc.MintAddress,
			BasePrecision: 9, QuotePrecision: 6, MinNotionalQuote: money.QuantityFromInt64(1_000_000), Status: instruments.VenueActive,
		})
		if err != nil {
			return err
		}
		f.listing = listing
		return nil
	}))
	f.intentID = id.New[id.Any]().String()
	hash := sha256.Sum256([]byte(f.intentID))
	_, err = d.Exec(ctx, `INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor, constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash)
		VALUES ($1,$2,'USER','user-1','ACQUIRE_NOTIONAL',$3,10000,'{}',now(),$4,$5,'LIVE','PLANNED',$6)`,
		f.intentID, f.account, f.instrument.ID, "idem-"+f.intentID, "corr-"+f.intentID[:8], hash[:])
	require.NoError(t, err)
	f.planID = id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO execution_plans (id, intent_id, version, planner_version, status, hard_constraints, plan_hash)
		VALUES ($1,$2,1,'test',  'APPROVED','{}',$3)`, f.planID, f.intentID, hash[:])
	require.NoError(t, err)
	f.reservation = id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO asset_reservations (id, account_id, asset_id, intent_id, actor_type, actor_id, quantity, usd_minor, status, idempotency_key, expires_at)
		VALUES ($1,$2,$3,$4,'USER','user-1',100000000,10000,'ACTIVE',$5,now() + interval '1 hour')`,
		f.reservation, f.account, f.usdc.ID, f.intentID, "res-"+f.reservation)
	require.NoError(t, err)
	f.quoteID = id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO quotes (id, intent_id, provider, instrument_id, venue_listing_id, side, input_asset_id, input_quantity, output_asset_id, expected_output, minimum_output,
			effective_price_mantissa, effective_price_scale, price_impact_bps, slippage_bps, received_at, expires_at, route_hash, raw_response_hash)
		VALUES ($1,$2,'jupiter',$3,$4,'BUY',$5,100000000,$6,666666666,660000000,150,0,5,50,now(),now() + interval '30 seconds',$7,$7)`,
		f.quoteID, f.intentID, f.instrument.ID, f.listing.ID, f.usdc.ID, f.sol.ID, hash[:])
	require.NoError(t, err)
	f.walletID = id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO wallets (id, account_id, provider, provider_wallet_id, chain, address, kind, status)
		VALUES ($1,$2,'privy',$3,'solana-devnet',$4,'EMBEDDED_DELEGATED','ACTIVE')`, f.walletID, f.account, "pw-"+f.walletID, "addr-"+f.walletID)
	require.NoError(t, err)
	return f
}

func (f *fixture) newOrder() Order {
	return Order{
		ID: NewOrderID(), IntentID: f.intentID, PlanID: f.planID, AccountID: f.account, InstrumentID: f.instrument.ID.String(), VenueListingID: f.listing.ID.String(),
		Side: SideBuy, Mode: ModeLive, InputAssetID: f.usdc.ID, InputQuantity: money.QuantityFromInt64(100_000_000), OutputAssetID: f.sol.ID,
		MinOutputQuantity: money.QuantityFromInt64(660_000_000), ReservationID: f.reservation, QuoteID: f.quoteID, CorrelationID: "corr-" + f.intentID[:8],
	}
}

func (f *fixture) createOrder(t *testing.T) Order {
	t.Helper()
	var o Order
	require.NoError(t, f.d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		o, err = f.repo.Create(ctx, tx, f.newOrder())
		return err
	}))
	return o
}

func (f *fixture) transition(t *testing.T, o OrderID, to OrderStatus, ev TransitionEvidence) (Order, error) {
	t.Helper()
	var out Order
	err := f.d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.repo.Transition(ctx, tx, o, to, ev)
		return err
	})
	return out, err
}

func (f *fixture) submittedOrder(t *testing.T) Order {
	t.Helper()
	o := f.createOrder(t)
	for _, to := range []OrderStatus{OrderValidated, OrderCapitalReserved, OrderPlanned, OrderSubmitting, OrderSubmitted} {
		var err error
		o, err = f.transition(t, o.ID, to, TransitionEvidence{Reason: "test"})
		require.NoError(t, err)
	}
	return o
}

func (f *fixture) fill(o Order, ext string, in, out int64) Fill {
	return Fill{
		ID: NewFillID(), OrderID: o.ID, AccountID: f.account, Venue: "JUPITER", ExternalFillID: ext + "-" + o.ID.String(), TxSignature: "sig-" + ext + "-" + o.ID.String(),
		InputAssetID: f.usdc.ID, InputQuantity: money.QuantityFromInt64(in), OutputAssetID: f.sol.ID, OutputQuantity: money.QuantityFromInt64(out),
		NetworkFeeQuantity: money.QuantityFromInt64(5000), NetworkFeeAssetID: f.sol.ID,
		EffectivePriceMantissa: money.QuantityFromInt64(150), EffectivePriceScale: 0, Source: FillFromProvider, Finality: FinalityConfirmed, ObservedAt: f.clk.Now(),
	}
}

func (f *fixture) recordFill(t *testing.T, fl Fill) (Fill, error) {
	t.Helper()
	var out Fill
	err := f.d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.repo.RecordFill(ctx, tx, fl)
		return err
	})
	return out, err
}

func TestIntegration_OrderLifecycle(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	ctx := t.Context()
	o := f.createOrder(t)
	require.Equal(t, OrderCreated, o.Status)

	_, err := f.transition(t, o.ID, OrderSubmitted, TransitionEvidence{})
	require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition), "illegal transitions fail")

	_, err = f.transition(t, o.ID, OrderRejected, TransitionEvidence{Reason: "no code"})
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed), "REJECTED needs a rejection code")

	for _, to := range []OrderStatus{OrderValidated, OrderCapitalReserved, OrderPlanned, OrderSubmitting, OrderSubmitted} {
		o, err = f.transition(t, o.ID, to, TransitionEvidence{Reason: "test"})
		require.NoError(t, err)
	}
	trs, err := f.repo.ListTransitions(ctx, d, o.ID)
	require.NoError(t, err)
	require.Len(t, trs, 5)
	require.Equal(t, OrderCreated, trs[0].From)
	require.Equal(t, OrderSubmitted, trs[4].To)

	// A status change without its transition row is refused at COMMIT by the
	// binding trigger of migration 00603.
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE orders SET status = 'ACKNOWLEDGED' WHERE id = $1`, o.ID)
		return err
	})
	require.Error(t, err)
	require.Equal(t, "AU001", db.SQLState(err))
	got, err := f.repo.Get(ctx, d, o.ID)
	require.NoError(t, err)
	require.Equal(t, OrderSubmitted, got.Status)

	// Outbox and audit rows exist for the transitions.
	var outboxRows, auditRows int
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE topic = 'order.transitioned' AND aggregate_id = $1`, o.ID.String()).Scan(&outboxRows))
	require.Equal(t, 6, outboxRows, "creation + five transitions")
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'order' AND resource_id = $1`, o.ID.String()).Scan(&auditRows))
	require.Equal(t, 6, auditRows)

	byIntent, err := f.repo.GetByIntent(ctx, d, f.intentID)
	require.NoError(t, err)
	require.Equal(t, o.ID, byIntent.ID)
	byPlan, err := f.repo.GetByPlan(ctx, d, f.planID)
	require.NoError(t, err)
	require.Equal(t, o.ID, byPlan.ID)

	// One order per intent.
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.Create(ctx, tx, f.newOrder())
		return err
	})
	require.True(t, errs.HasCode(err, errs.CodeConflict))
}

func TestIntegration_RecordFill_PartialThenFilled(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	o := f.submittedOrder(t)
	f1, err := f.recordFill(t, f.fill(o, "ext-1", 40_000_000, 260_000_000))
	require.NoError(t, err)
	require.False(t, f1.Existing)
	got, err := f.repo.Get(t.Context(), d, o.ID)
	require.NoError(t, err)
	require.Equal(t, OrderPartiallyFilled, got.Status)
	require.Equal(t, "40000000", got.FilledInputQuantity.String())

	f2, err := f.recordFill(t, f.fill(o, "ext-2", 60_000_000, 400_000_000))
	require.NoError(t, err)
	require.False(t, f2.Existing)
	got, err = f.repo.Get(t.Context(), d, o.ID)
	require.NoError(t, err)
	require.Equal(t, OrderFilled, got.Status)
	require.Equal(t, "100000000", got.FilledInputQuantity.String())
	require.Equal(t, "660000000", got.FilledOutputQuantity.String())

	// Overfill is refused as a reconciliation matter.
	_, err = f.recordFill(t, f.fill(o, "ext-3", 1, 1))
	require.True(t, errs.HasCode(err, errs.CodeReconciliationRequired), "%v", err)
	fills, err := f.repo.ListFills(t.Context(), d, o.ID)
	require.NoError(t, err)
	require.Len(t, fills, 2)

	var fillEvents int
	require.NoError(t, d.QueryRow(t.Context(), `SELECT count(*) FROM outbox_events WHERE topic = 'fill.observed' AND aggregate_id = $1`, o.ID.String()).Scan(&fillEvents))
	require.Equal(t, 2, fillEvents)
}

// TestProp_Integration_FillIdempotent delivers the same fill N times, some
// concurrently, and requires one row and one economic effect.
func TestProp_Integration_FillIdempotent(t *testing.T) {
	d := openTestDB(t)
	rapid.Check(t, func(rt *rapid.T) {
		f := newFixture(t, d)
		o := f.submittedOrder(t)
		n := rapid.IntRange(2, 12).Draw(rt, "deliveries")
		parallel := rapid.Bool().Draw(rt, "parallel")
		fl := f.fill(o, "ext-dup", 100_000_000, 666_666_666)
		results := make([]Fill, n)
		errsOut := make([]error, n)
		if parallel {
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					fx := fl
					fx.ID = NewFillID() // a redelivery carries a fresh internal id but the same external id
					results[i], errsOut[i] = f.recordFill(t, fx)
				}(i)
			}
			wg.Wait()
		} else {
			for i := 0; i < n; i++ {
				fx := fl
				fx.ID = NewFillID()
				results[i], errsOut[i] = f.recordFill(t, fx)
			}
		}
		fresh := 0
		for i := range results {
			require.NoError(rt, errsOut[i])
			if !results[i].Existing {
				fresh++
			}
		}
		require.Equal(rt, 1, fresh, "exactly one delivery was new")
		fills, err := f.repo.ListFills(t.Context(), d, o.ID)
		require.NoError(rt, err)
		require.Len(rt, fills, 1)
		got, err := f.repo.Get(t.Context(), d, o.ID)
		require.NoError(rt, err)
		require.Equal(rt, OrderFilled, got.Status)
		require.Equal(rt, "100000000", got.FilledInputQuantity.String(), "cumulative quantity counted once")
		var events int
		require.NoError(rt, d.QueryRow(t.Context(), `SELECT count(*) FROM outbox_events WHERE topic = 'fill.observed' AND aggregate_id = $1`, o.ID.String()).Scan(&events))
		require.Equal(rt, 1, events, "one fill event")
	})
}

func TestIntegration_FillMarkers_SetOnce(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	ctx := t.Context()
	o := f.submittedOrder(t)
	fl, err := f.recordFill(t, f.fill(o, "ext-post", 100_000_000, 666_666_666))
	require.NoError(t, err)
	txA := seedJournalTx(t, d, f)
	txB := seedJournalTx(t, d, f)
	mark := func(txID string) error {
		return d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error { return f.repo.MarkFillPosted(ctx, tx, fl.ID, txID) })
	}
	require.NoError(t, mark(txA))
	require.NoError(t, mark(txA), "same value is a no-op")
	err = mark(txB)
	require.True(t, errs.HasCode(err, errs.CodeFillImmutable), "%v", err)
	got, err := f.repo.GetFill(ctx, d, fl.ID)
	require.NoError(t, err)
	require.Equal(t, txA, got.JournalTransactionID)

	// The trigger is the second line of defense against raw writes.
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE fills SET journal_transaction_id = $2 WHERE id = $1`, fl.ID, txB)
		return err
	})
	require.Equal(t, "LG003", db.SQLState(err))
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE fills SET input_quantity = 1 WHERE id = $1`, fl.ID)
		return err
	})
	require.Equal(t, "LG003", db.SQLState(err), "economic fields are immutable")

	at := f.clk.Now()
	apply := func() error {
		return d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error { return f.repo.MarkPositionApplied(ctx, tx, fl.ID, at) })
	}
	require.NoError(t, apply())
	require.NoError(t, apply(), "idempotent")
	got, err = f.repo.GetFill(ctx, d, fl.ID)
	require.NoError(t, err)
	require.NotNil(t, got.PositionAppliedAt)
}

func TestIntegration_CancelRace(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()

	t.Run("fill wins over cancel request", func(t *testing.T) {
		f := newFixture(t, d)
		o := f.submittedOrder(t)
		var err error
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			o, err = f.repo.RequestCancel(ctx, tx, o.ID, TransitionEvidence{ActorType: "USER", ActorID: "user-1"})
			return err
		}))
		require.Equal(t, OrderCancelRequested, o.Status)
		_, err = f.recordFill(t, f.fill(o, "ext-race", 100_000_000, 666_666_666))
		require.NoError(t, err)
		got, err := f.repo.Get(ctx, d, o.ID)
		require.NoError(t, err)
		require.Equal(t, OrderFilled, got.Status, "the fill wins")
		err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.ConfirmCancelled(ctx, tx, o.ID, CancelConfirmation{Source: FillFromProvider, ExternalRef: "cancel-1", ConfirmedAt: f.clk.Now()})
			return err
		})
		require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition), "a filled order can no longer be cancelled: %v", err)
	})

	t.Run("cancel needs external confirmation", func(t *testing.T) {
		f := newFixture(t, d)
		o := f.submittedOrder(t)
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.RequestCancel(ctx, tx, o.ID, TransitionEvidence{})
			return err
		}))
		err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.ConfirmCancelled(ctx, tx, o.ID, CancelConfirmation{ConfirmedAt: f.clk.Now()})
			return err
		})
		require.True(t, errs.HasCode(err, errs.CodeValidationFailed), "%v", err)
		got, err := f.repo.Get(ctx, d, o.ID)
		require.NoError(t, err)
		require.Equal(t, OrderCancelRequested, got.Status, "a request alone never cancels")
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.ConfirmCancelled(ctx, tx, o.ID, CancelConfirmation{Source: FillFromChainObserver, ExternalRef: "expired-blockhash", ConfirmedAt: f.clk.Now()})
			return err
		}))
		got, err = f.repo.Get(ctx, d, o.ID)
		require.NoError(t, err)
		require.Equal(t, OrderCancelled, got.Status)
		require.NotNil(t, got.TerminalAt)
		_, err = f.recordFill(t, f.fill(o, "ext-late", 1, 1))
		require.True(t, errs.HasCode(err, errs.CodeReconciliationRequired), "a fill on a cancelled order is a reconciliation matter")
	})

	t.Run("cancel cannot be confirmed without a request", func(t *testing.T) {
		f := newFixture(t, d)
		o := f.submittedOrder(t)
		err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.ConfirmCancelled(ctx, tx, o.ID, CancelConfirmation{Source: FillFromProvider, ExternalRef: "x", ConfirmedAt: f.clk.Now()})
			return err
		})
		require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition))
	})
}

func TestIntegration_Attempts(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	ctx := t.Context()
	o := f.submittedOrder(t)
	newAttempt := func() Attempt {
		return Attempt{ID: NewAttemptID(), OrderID: o.ID, PlanID: f.planID, WalletID: f.walletID, Provider: "jupiter", QuoteID: f.quoteID, UnsignedTxHash: []byte{1, 2}, CorrelationID: "c"}
	}
	create := func() Attempt {
		var a Attempt
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			a, err = f.attempts.Create(ctx, tx, newAttempt())
			return err
		}))
		return a
	}
	a1 := create()
	a2 := create()
	require.Equal(t, int32(1), a1.AttemptNo)
	require.Equal(t, int32(2), a2.AttemptNo)
	require.Equal(t, AttemptBuilt, a1.Status)

	update := func(id AttemptID, p AttemptPatch) (Attempt, error) {
		var a Attempt
		err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			a, err = f.attempts.Update(ctx, tx, id, p)
			return err
		})
		return a, err
	}
	status := func(s AttemptStatus) *AttemptStatus { return &s }
	_, err := update(a1.ID, AttemptPatch{Status: status(AttemptSigned)})
	require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition), "BUILT -> SIGNED skips inspection")
	a1, err = update(a1.ID, AttemptPatch{Status: status(AttemptInspected), InspectionResult: []byte(`[{"check":"FEE_PAYER","ok":true}]`)})
	require.NoError(t, err)
	a1, err = update(a1.ID, AttemptPatch{Status: status(AttemptSigningRequested)})
	require.NoError(t, err)

	setSig := func(id AttemptID, sig string) error {
		return d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.attempts.SetSignature(ctx, tx, id, sig, []byte{9})
			return err
		})
	}
	sig := "sig-" + id.New[id.Any]().String()
	require.NoError(t, setSig(a1.ID, sig))
	require.NoError(t, setSig(a1.ID, sig), "repeat with the same signature is a no-op")
	require.True(t, errs.HasCode(setSig(a1.ID, "other"), errs.CodeConflict))
	require.True(t, errs.HasCode(setSig(a2.ID, sig), errs.CodeConflict), "a signature belongs to exactly one attempt")
	found, err := f.attempts.FindBySignature(ctx, d, sig)
	require.NoError(t, err)
	require.Equal(t, a1.ID, found.ID)

	for _, s := range []AttemptStatus{AttemptSigned, AttemptSubmitting, AttemptSubmitted} {
		a1, err = update(a1.ID, AttemptPatch{Status: status(s)})
		require.NoError(t, err)
	}
	rec, err := f.attempts.ListRecoverable(ctx, d, 100)
	require.NoError(t, err)
	var ids []AttemptID
	for _, a := range rec {
		ids = append(ids, a.ID)
	}
	require.Contains(t, ids, a1.ID)
	require.NotContains(t, ids, a2.ID, "BUILT attempts are not recoverable")

	a1, err = update(a1.ID, AttemptPatch{Status: status(AttemptSubmissionUnknown)})
	require.NoError(t, err)
	a1, err = update(a1.ID, AttemptPatch{Status: status(AttemptAdopted)})
	require.NoError(t, err)
	now := f.clk.Now()
	fin := FinalityConfirmed
	a1, err = update(a1.ID, AttemptPatch{Status: status(AttemptConfirmed), Finality: &fin, ConfirmedAt: &now})
	require.NoError(t, err)
	require.Equal(t, FinalityConfirmed, a1.Finality)
	list, err := f.attempts.ListForOrder(ctx, d, o.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	var events int
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE topic = 'execution.attempt.transitioned' AND aggregate_id = $1`, a1.ID.String()).Scan(&events))
	require.GreaterOrEqual(t, events, 8)
}

// seedJournalTx posts a balanced SEED transaction (Dr WALLET / Cr CAPITAL of
// one USDC unit) so a fill can reference a real journal transaction.
func seedJournalTx(t *testing.T, d *db.DB, f *fixture) string {
	t.Helper()
	ctx := t.Context()
	txID := id.New[id.Any]()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		account := func(code, side string) id.ID[id.Any] {
			_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
				VALUES ($1,'CUSTOMER',$2,$3,$4,$5) ON CONFLICT (owner_type, owner_id, code, asset_id) DO NOTHING`, id.New[id.Any](), f.account, code, f.usdc.ID, side)
			require.NoError(t, err)
			var out id.ID[id.Any]
			require.NoError(t, tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE owner_type='CUSTOMER' AND owner_id=$1 AND code=$2 AND asset_id=$3`, f.account, code, f.usdc.ID).Scan(&out))
			return out
		}
		wallet, capitalAcct := account("WALLET", "DEBIT"), account("CAPITAL", "CREDIT")
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1,'SEED',$2,'seed',$3,now(),'SYSTEM','execution-itest',$4)`, txID, "seed:"+txID.String(), txID.String(), []byte{0}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,0,$3,$4,'DEBIT',1)`,
			id.New[id.Any](), txID, wallet, f.usdc.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,1,$3,$4,'CREDIT',1)`,
			id.New[id.Any](), txID, capitalAcct, f.usdc.ID)
		return err
	}))
	return txID.String()
}
