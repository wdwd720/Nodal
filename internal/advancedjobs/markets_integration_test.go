//go:build integration

package advancedjobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/transfers"
)

// market defines and opens a binary SANDBOX market in the sandbox dollar,
// closing after closeIn.
func (w *world) market(closeIn time.Duration) predmarket.Market {
	w.t.Helper()
	creator := w.operator("market-creator")
	spec := predmarket.MarketSpec{
		Symbol:   "AJ-" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10]),
		Question: "Will the advanced worker close this sandbox market on time?",
		Outcomes: []string{"YES", "NO"}, CollateralAssetID: w.usd.ID, Mode: gates.ModeSandbox,
		OpenAt: w.clk.Now(), CloseAt: w.clk.Now().Add(closeIn), DisputeWindow: 0,
		OracleSource: w.oracle.Source(), OraclePublicKey: w.oracle.PublicKey(),
	}
	var m predmarket.Market
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if m, _, err = w.predictions.CreateMarket(ctx, tx, spec, creator); err != nil {
			return err
		}
		m, err = w.predictions.Open(ctx, tx, m.ID, creator)
		return err
	}))
	require.Equal(w.t, predmarket.StatusOpen, m.Status)
	return m
}

func (w *world) marketStatus(id predmarket.MarketID) predmarket.MarketStatus {
	w.t.Helper()
	m, err := w.predictions.Get(w.ctx, testDB, id)
	require.NoError(w.t, err)
	return m.Status
}

// TestIntegration_ADuePredictionMarketCloses: the close-due job leaves an
// open market alone until its close time, then closes it -- outcome
// instruments HALTED, the market instrument CLOSE_ONLY -- and a second run
// does nothing more.
func TestIntegration_ADuePredictionMarketCloses(t *testing.T) {
	w := newWorld(t)
	m := w.market(time.Hour)
	job := NewPredictionClose(testDB, w.predictions, Settings{})

	run(t, w.ctx, job)
	assert.Equal(t, predmarket.StatusOpen, w.marketStatus(m.ID), "not due before its close time")

	w.clk.Advance(61 * time.Minute)
	rep := run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, predmarket.StatusClosed, w.marketStatus(m.ID))
	for _, o := range m.Outcomes {
		ins, err := w.instr.Get(w.ctx, testDB, o.InstrumentID)
		require.NoError(t, err)
		assert.Equal(t, assets.StatusHalted, ins.Status, "no trading in an outcome after close")
	}
	ins, err := w.instr.Get(w.ctx, testDB, m.InstrumentID)
	require.NoError(t, err)
	assert.Equal(t, assets.StatusCloseOnly, ins.Status)

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM prediction_market_transitions WHERE market_id = $1 AND to_status = 'CLOSED'`, m.ID),
		"closed once")
	var actor string
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT actor_type || ':' || actor_id FROM prediction_market_transitions
		WHERE market_id = $1 AND to_status = 'CLOSED'`, m.ID).Scan(&actor))
	assert.Equal(t, "SYSTEM:"+WorkerActorID, actor, "the scheduler closed it, as itself")
}

// TestIntegration_EscrowReconciliationRecordsEachMarketsStanding: the escrow
// job reconciles an open market's escrow against its shares, records a match,
// and running it again changes no book and opens no mismatch.
func TestIntegration_EscrowReconciliationRecordsEachMarketsStanding(t *testing.T) {
	w := newWorld(t)
	m := w.market(time.Hour)
	job := NewPredictionEscrow(testDB, w.predictions, Settings{})
	for i := 0; i < 2; i++ {
		rep := run(t, w.ctx, job)
		assert.False(t, failedUnit(rep, m.ID.String()), "%+v", rep.Failures)
	}
	assert.Positive(t, w.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'PREDICTION_ESCROW'
		AND scope_id = $1 AND status = 'MATCHED'`, m.ID.String()))
	assert.Zero(t, w.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'PREDICTION_ESCROW'
		AND scope_id = $1 AND status <> 'MATCHED'`, m.ID.String()), "an untouched escrow never mismatches")
}

// spotBook opens a PAPER internal book for a fresh simulated asset against
// the sandbox dollar (scenario D's terms) and returns the asset.
func (w *world) spotBook() (assets.Asset, instruments.InstrumentID) {
	w.t.Helper()
	eth := w.simAsset("AJETH", 8, assets.RiskMajor)
	var inst instruments.InstrumentID
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ins, err := w.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: eth.ID, Quote: w.usd.ID, Settlement: w.usd.ID, CanonicalName: eth.Symbol + "/SBX-USD",
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: w.clk.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		inst = ins.ID
		base, quote := eth.ID, w.usd.ID
		terms := instruments.Terms{
			Kind: instruments.TypeSpotPair, Symbol: eth.Symbol + "/SBX-USD", VenueCode: "NODAL-SIM", BaseAssetID: &base,
			QuoteAssetID: &quote, SettlementAssetID: quote, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
			TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(10_000), MinQuantity: money.QuantityFromInt64(10_000),
			QuantityDecimals: eth.Decimals, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7",
			OracleMaxAgeMS: 30_000, MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.Transferable,
		}
		zero := func(v string) fees.Schedule {
			s := fees.ZeroSchedule(w.usd.ID, 6)
			s.Version = v
			return s
		}
		_, err = w.orders.OpenBook(ctx, tx, orders.OpenBookRequest{
			InstrumentID: ins.ID, Mode: gates.ModePaper, SpotTerms: &terms,
			MakerSchedule: zero("advancedjobs-maker"), TakerSchedule: zero("advancedjobs-taker"),
			SnapshotInterval: 2, Actor: w.system,
		})
		return err
	}))
	w.mark(eth.ID, "2500.00")
	return eth, inst
}

func (w *world) limitOrder(c customer, inst instruments.InstrumentID, side orders.Side, price string, qty money.Quantity) orders.OrderRequest {
	w.t.Helper()
	p, err := money.PriceFromDecimalString(price, w.usd.ID.String(), "order", w.clk.Now())
	require.NoError(w.t, err)
	return orders.OrderRequest{
		InstrumentID: inst, AccountID: c.account, Side: side, Type: orders.TypeLimit, TimeInForce: orders.GTC,
		Price: &p, Quantity: qty, IdempotencyKey: "advancedjobs-" + uuid.NewString(), ActorType: security.ActorUser, ActorID: c.user.String(),
	}
}

// TestIntegration_ATamperedBookJournalProducesAReconciliationRecord: the
// book-proof job proves a book whose journal replays, opens a material
// reconciliation record when a journalled event's bytes are changed behind the
// service's back, and refreshes that one record -- never a second -- when it
// runs again.
func TestIntegration_ATamperedBookJournalProducesAReconciliationRecord(t *testing.T) {
	w := newWorld(t)
	eth, inst := w.spotBook()
	maker := w.customer("maker")
	w.fund(maker.account, eth.ID, units(3, eth.Decimals))
	sell, err := w.pipeline.Submit(w.ctx, w.limitOrder(maker, inst, orders.SideSell, "2500.00", units(2, eth.Decimals)))
	require.NoError(t, err)
	require.Equal(t, orders.StatusOpen, sell.Order.Status)
	book, err := w.orders.GetBook(w.ctx, testDB, inst)
	require.NoError(t, err)
	job := NewOrdersBookProof(testDB, w.orders, w.recon, Settings{})
	records := func() int {
		return w.count(`SELECT count(*) FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2`, ScopeOrderBook, book.ID.String())
	}

	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, book.ID.String()), "%+v", rep.Failures)
	assert.Zero(t, records(), "a journal that replays is not a finding")

	// Change one journalled event's bytes as the table owner, with the
	// append-only trigger switched off for the length of the edit -- the
	// bypass the database otherwise refuses even to its owner.
	owner, err := pgx.Connect(w.ctx, testMigrateURL)
	require.NoError(t, err)
	defer func() { _ = owner.Close(context.Background()) }()
	tx, err := owner.Begin(w.ctx)
	require.NoError(t, err)
	_, err = tx.Exec(w.ctx, `ALTER TABLE book_events DISABLE TRIGGER book_events_immutable`)
	require.NoError(t, err)
	tag, err := tx.Exec(w.ctx, `UPDATE book_events SET event = event || '\x00'::bytea
		WHERE book_id = $1 AND seq = (SELECT min(seq) FROM book_events WHERE book_id = $1)`, book.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected())
	_, err = tx.Exec(w.ctx, `ALTER TABLE book_events ENABLE TRIGGER book_events_immutable`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(w.ctx))

	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, book.ID.String()), "a finding is recorded, not reported as a failure: %+v", rep.Failures)
	assert.GreaterOrEqual(t, rep.Changed, 1)
	require.Equal(t, 1, records())
	var kind, status string
	var material bool
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT kind, status, material FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2`,
		ScopeOrderBook, book.ID.String()).Scan(&kind, &status, &material))
	assert.Equal(t, string(reconciliation.KindLedgerInternal), kind)
	assert.Equal(t, string(reconciliation.StatusMismatch), status)
	assert.True(t, material)

	run(t, w.ctx, job)
	assert.Equal(t, 1, records(), "a second pass refreshes the open record instead of opening another")
}

// TestIntegration_ATerminalOrdersIntentIsCompletedOnce: an order filled by a
// submission that stopped before its Complete stage leaves its intent
// EXECUTING; the complete-terminal job completes it, once.
func TestIntegration_ATerminalOrdersIntentIsCompletedOnce(t *testing.T) {
	w := newWorld(t)
	eth, inst := w.spotBook()
	maker, taker := w.customer("maker"), w.customer("taker")
	w.fund(maker.account, eth.ID, units(3, eth.Decimals))
	w.fund(taker.account, w.usd.ID, w.usdUnits(10_000))
	_, err := w.pipeline.Submit(w.ctx, w.limitOrder(maker, inst, orders.SideSell, "2500.00", units(1, eth.Decimals)))
	require.NoError(t, err)

	// The taker's submission, stage by stage, stopping before Complete.
	req := w.limitOrder(taker, inst, orders.SideBuy, "2500.00", units(1, eth.Decimals))
	var out orders.StageOutcome
	stage := func(fn func(ctx context.Context, tx pgx.Tx) (orders.StageOutcome, error)) {
		t.Helper()
		require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = fn(ctx, tx)
			return err
		}))
	}
	stage(func(ctx context.Context, tx pgx.Tx) (orders.StageOutcome, error) {
		return w.orders.Accept(ctx, tx, req)
	})
	oid := out.Order.ID
	stage(func(ctx context.Context, tx pgx.Tx) (orders.StageOutcome, error) {
		return w.orders.Approve(ctx, tx, oid)
	})
	stage(func(ctx context.Context, tx pgx.Tx) (orders.StageOutcome, error) {
		return w.orders.Reserve(ctx, tx, oid)
	})
	stage(func(ctx context.Context, tx pgx.Tx) (orders.StageOutcome, error) {
		return w.orders.Execute(ctx, tx, oid)
	})
	require.Equal(t, orders.StatusFilled, out.Order.Status)
	require.Equal(t, econ.StatusExecuting, out.Intent.Status, "the crash left the intent executing")
	intentID := out.Intent.ID

	job := NewOrdersCompleteTerminal(testDB, w.orders, Settings{})
	rep := run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	in, err := w.intents.Get(w.ctx, testDB, intentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusCompleted, in.Status)

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM economic_intent_transitions WHERE intent_id = $1 AND to_status = 'COMPLETED'`, intentID),
		"completed once")
}

// TestIntegration_TransfersReconciliationRecordsADiscrepancyOnce: a
// P2P_TRANSFER posted around the transfers service is a SEV1 the subsystem
// reports; the job records it once as a material reconciliation record and
// refreshes that record on the next pass.
func TestIntegration_TransfersReconciliationRecordsADiscrepancyOnce(t *testing.T) {
	w := newWorld(t)
	job := NewTransfersReconcile(testDB, w.transfers, w.recon, Settings{})
	records := func() int {
		return w.count(`SELECT count(*) FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2`, ScopeTransfers, ScopeTransfersID)
	}
	before := records()
	rep := run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)

	gold := w.simAsset("AJGOLD", 6, assets.RiskStandard)
	alice, bob := w.customer("alice"), w.customer("bob")
	w.fund(alice.account, gold.ID, units(10, 6))
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindP2PTransfer, IdempotencyKey: "advancedjobs-orphan:" + ref,
			Reference: ledger.FinancialEventReference{Type: "p2p_transfer", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: transfers.TransferEntries(alice.account, bob.account, gold.ID, units(1, 6)),
		})
		return err
	}))

	rep = run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, 1, rep.Changed)
	require.Equal(t, before+1, records())
	var status, kind string
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT kind, status FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2
		ORDER BY opened_at DESC LIMIT 1`, ScopeTransfers, ScopeTransfersID).Scan(&kind, &status))
	assert.Equal(t, string(reconciliation.KindLedgerInternal), kind)
	assert.Equal(t, string(reconciliation.StatusMismatch), status)

	run(t, w.ctx, job)
	assert.Equal(t, before+1, records(), "the open record is refreshed, not duplicated")
}
