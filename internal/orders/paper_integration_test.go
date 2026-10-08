//go:build integration

package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

func paperSchedule(m spotMarket) fees.Schedule {
	s := fees.ZeroSchedule(m.quote.ID, 6)
	s.Version = "paper-itest-v1"
	s.SpreadBPS = 25
	s.PercentageFeeBPS = 10
	s.FixedFee = money.QuantityFromInt64(1_000)
	s.VenuePassThroughBPS = 5
	return s
}

// paperOperator publishes the paper venue's schedules in these tests, as an
// operator does through the fee ledger.
var paperOperator = econ.Actor{Type: security.ActorOperator, ID: "orders-itest-operator"}

// publishPaper publishes an instrument's paper-venue schedule in the fee
// ledger, effective now (or from a later instant).
func (f *fixture) publishPaper(inst instruments.InstrumentID, s fees.Schedule, from time.Time) feeledger.PublishedSchedule {
	f.t.Helper()
	var p feeledger.PublishedSchedule
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = f.fees.PublishSchedule(ctx, tx, feeledger.PublishRequest{Key: PaperScheduleKey(PaperScheduleKeyPrefix, inst),
			Schedule: s, EffectiveFrom: from, Reason: "orders itest paper schedule", Actor: paperOperator})
		return err
	}))
	return p
}

// paperVenue publishes each schedule as its instrument's paper schedule in
// force and returns the venue.
func (f *fixture) paperVenue(schedules map[instruments.InstrumentID]fees.Schedule) *PaperVenue {
	f.t.Helper()
	for inst, s := range schedules {
		f.publishPaper(inst, s, time.Time{})
	}
	v, err := NewPaperVenue(PaperConfig{
		Clock: f.clk, Ledger: f.ledger, Capital: f.capital, Intents: f.intents, Effects: f.effects, Authorizer: f.auth,
		Risk: f.risk, RiskPolicy: risk.DefaultAdvancedPolicy(), Router: f.router, Instruments: f.instr, Assets: f.assets,
		Prices: f.prices, KillSwitches: f.kills, FeeLedger: f.fees,
	})
	require.NoError(f.t, err)
	return v
}

func paperRequest(m spotMarket, acct accounts.AccountID, side Side, qty money.Quantity) PaperOrderRequest {
	return PaperOrderRequest{InstrumentID: m.inst, AccountID: acct, Side: side, Quantity: qty,
		IdempotencyKey: "paper-" + uuid.NewString(), ActorType: security.ActorUser, ActorID: acct.String()}
}

// TestIntegration_PaperVenueFillsAtTheMarkWithADisclosedSpread: a market order
// fills at once against the mark; the customer pays the reference notional,
// the disclosed spread and the disclosed fees -- each posted to its own
// account -- and nothing else; the fill verifies; a replay is the same fill.
func TestIntegration_PaperVenueFillsAtTheMarkWithADisclosedSpread(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{m.inst: paperSchedule(m)})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000)
	f.fund(x, m.base.ID, 5_000_000)

	out, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideBuy, units(2)))
	require.NoError(t, err)
	require.NotNil(t, out.Fill)
	fill := *out.Fill
	assert.Equal(t, econ.StatusCompleted, out.Intent.Status)
	assert.Equal(t, VenueLabelPaper, fill.VenueLabel)
	// 2 units at the 100.00 mark: notional 200; spread 25 bps = 0.50;
	// percentage fee 10 bps = 0.20 plus a fixed 0.001; venue pass-through
	// 5 bps = 0.10. The customer pays 200.801 and nothing else.
	assert.Equal(t, "200000000", fill.NotionalAtReference.String())
	assert.Equal(t, "500000", fill.SpreadAmount.String())
	assert.Equal(t, "201000", fill.PlatformFeeAmount.String())
	assert.Equal(t, "100000", fill.VenueFeeAmount.String())
	assert.Equal(t, "200801000", fill.TotalQuote.String())
	assert.Equal(t, "799199000", f.wallet(x, m.quote.ID).String())
	assert.Equal(t, "7000000", f.wallet(x, m.base.ID).String())
	assert.Equal(t, "500000", f.balance(ledger.CustomerAccount(x, ledger.CodeFeesSpread, m.quote.ID)).String())
	assert.Equal(t, "500000", f.balance(ledger.PlatformAccount(ledger.CodeSpreadRevenue, m.quote.ID)).String())
	assert.Equal(t, "201000", f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, m.quote.ID)).String())
	assert.Equal(t, "100000", f.balance(ledger.CustomerAccount(x, ledger.CodeFeesVenue, m.quote.ID)).String())
	require.NoError(t, v.VerifyFill(f.ctx, testDB, out.Intent.ID))
	f.requireArchived(fill.EvidenceRef, fill.EvidenceSHA256)

	// A sell credits the notional less the disclosed spread and fees, exactly
	// what the schedule quotes.
	sell, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideSell, units(1)))
	require.NoError(t, err)
	mark, err := money.PriceFromDecimalString("100.00", m.quote.ID.String(), "orders-itest", f.clk.Now())
	require.NoError(t, err)
	q, err := paperSchedule(m).Quote(fees.QuoteInput{Side: fees.SideSell, BaseQty: units(1), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: mark})
	require.NoError(t, err)
	assert.Equal(t, q.TotalCredit.String(), sell.Fill.TotalQuote.String())
	require.NoError(t, v.VerifyFill(f.ctx, testDB, sell.Intent.ID))

	// Executing the same intent again returns the first fill.
	in, err := f.intents.Get(f.ctx, testDB, out.Intent.ID)
	require.NoError(t, err)
	var again PaperFill
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		again, err = v.ExecuteIntent(ctx, tx, in)
		return err
	}))
	assert.True(t, again.Replayed)
	assert.Equal(t, fill.ID, again.ID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "orders:paper:"+out.Intent.ID.String()))
}

// TestIntegration_PaperDisclosureTamperFailsVerification proves the two audit
// claims of the spread engine: every unit of spread charged is discoverable
// from the stored record (it recomputes from the schedule and the disclosure
// alone, and equals what the journal posted), and there is no undisclosed
// spread path (a disclosure altered in any way fails verification; the
// record cannot be edited; the internal book can charge no spread at all; and
// all spread revenue in the ledger is accounted for by stored disclosures).
func TestIntegration_PaperDisclosureTamperFailsVerification(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{m.inst: paperSchedule(m)})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000)
	out, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideBuy, units(3)))
	require.NoError(t, err)
	stored, err := v.Fill(f.ctx, testDB, out.Intent.ID)
	require.NoError(t, err)
	d, err := VerifyFillRecord(stored.Schedule, stored.DisclosureBytes, stored.DisclosureHash)
	require.NoError(t, err)
	assert.Equal(t, stored.SpreadAmount.String(), d.Spread.Amount.String(), "the charged spread is the disclosed spread")

	hashOf := func(b []byte) string {
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])
	}
	tamper := func(edit func(*fees.Disclosure)) []byte {
		var td fees.Disclosure
		require.NoError(t, json.Unmarshal(stored.DisclosureBytes, &td))
		edit(&td)
		b, err := td.Canonical()
		require.NoError(t, err)
		return b
	}
	for name, edit := range map[string]func(*fees.Disclosure){
		"one more unit of spread":      func(d *fees.Disclosure) { d.Spread.Amount = d.Spread.Amount.Add(money.QuantityFromInt64(1)) },
		"a smaller stated spread rate": func(d *fees.Disclosure) { d.Spread.BPS = 20 },
		"a fee line removed":           func(d *fees.Disclosure) { d.Fees.Lines = d.Fees.Lines[:2] },
		"a different total":            func(d *fees.Disclosure) { d.TotalDebit = d.TotalDebit.Sub(money.QuantityFromInt64(1)) },
		"another reference price": func(d *fees.Disclosure) {
			d.ReferencePrice.Mantissa = d.ReferencePrice.Mantissa.Add(money.QuantityFromInt64(1))
		},
	} {
		forged := tamper(edit)
		_, err := VerifyFillRecord(stored.Schedule, forged, hashOf(forged))
		require.Error(t, err, "%s with a matching hash", name)
		assert.Equal(t, ReasonPaperDisclosureMismatch, ReasonOf(err), name)
		_, err = VerifyFillRecord(stored.Schedule, forged, stored.DisclosureHash)
		require.Error(t, err, "%s with the original hash", name)
	}
	// A schedule swapped for a cheaper-looking one does not verify either.
	other := paperSchedule(m)
	other.SpreadBPS = 5
	otherBytes, err := other.Canonical()
	require.NoError(t, err)
	_, err = VerifyFillRecord(otherBytes, stored.DisclosureBytes, stored.DisclosureHash)
	require.Error(t, err)

	// The stored record cannot be edited, by the application or around it.
	_, err = testDB.Exec(f.ctx, `UPDATE paper_fills SET spread_amount = 0 WHERE intent_id = $1`, out.Intent.ID)
	require.Error(t, err)
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE paper_fills SET disclosure = $2 WHERE intent_id = $1`, out.Intent.ID,
		tamper(func(d *fees.Disclosure) { d.Spread.Amount = money.QuantityFromInt64(0) }))
	require.Error(t, err, "history is append-only even for the owner")

	// The internal book cannot charge a spread: a schedule with one is refused.
	spready := bookSchedule(m.quote.ID, 10)
	spready.SpreadBPS = 1
	m2 := f.spotMarket(marketOpts{})
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.OpenBook(ctx, tx, OpenBookRequest{InstrumentID: m2.inst, Mode: gates.ModePaper, MakerSchedule: spready,
			TakerSchedule: spready, Actor: systemActor})
		require.Error(t, err)
		return nil
	}))
	// Internal-book trades post no spread at all, and every unit of spread
	// revenue in the ledger is a stored paper disclosure's.
	accts := f.fundedAccounts(m, 2)
	f.submit(f.limit(m, accts[0], SideSell, "100.00", units(1)))
	f.submit(f.limit(m, accts[1], SideBuy, "100.00", units(1)))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM journal_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id
		JOIN book_trades t ON t.journal_transaction_id = e.transaction_id
		WHERE t.instrument_id = $1 AND a.code IN ('FEES_SPREAD','SPREAD_REVENUE')`, m.inst))
	var disclosed string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(spread_amount), 0)::text FROM paper_fills WHERE quote_asset_id = $1`,
		m.quote.ID).Scan(&disclosed))
	assert.Equal(t, disclosed, f.balance(ledger.PlatformAccount(ledger.CodeSpreadRevenue, m.quote.ID)).String())
}

// TestIntegration_PaperVenueRefusesAStaleMarkAndNeverPosts: with no fresh
// mark a buy is refused by the risk kernel and a sell by the venue itself;
// both refusals are recorded on the intent and nothing is reserved or posted.
func TestIntegration_PaperVenueRefusesAStaleMarkAndNeverPosts(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{m.inst: paperSchedule(m)})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000)
	f.fund(x, m.base.ID, 5_000_000)
	f.clk.Advance(2 * time.Minute)
	journals := f.count(`SELECT count(*) FROM journal_transactions`)

	buy, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideBuy, units(1)))
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	assert.Equal(t, econ.StatusRejected, buy.Intent.Status)
	sell, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideSell, units(1)))
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
	assert.Equal(t, econ.StatusRejected, sell.Intent.Status)
	assert.Equal(t, string(errs.CodeStaleMarketData), sell.Intent.RejectionCode)
	assert.Equal(t, journals, f.count(`SELECT count(*) FROM journal_transactions`), "nothing posted")
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM paper_fills WHERE account_id = $1`, x))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, x))
}

// TestIntegration_PaperIntentExecutorFillsAnIntentItDoesNotOwn: another
// subsystem's intent (a strategy marketplace paper-follow), routed and risk
// approved by its owner, is filled through IntentExecutor; an intent that is
// not approved, has been altered, or would execute beyond its protection
// price is refused before anything is written.
func TestIntegration_PaperIntentExecutorFillsAnIntentItDoesNotOwn(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	var exec IntentExecutor = f.paperVenue(map[instruments.InstrumentID]fees.Schedule{m.inst: paperSchedule(m)})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000)
	ownIntent := func(terms string, approve bool) econ.Intent {
		qty := units(1)
		base := m.base.ID
		var in econ.Intent
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			in, _, err = f.intents.Submit(ctx, tx, econ.Intent{AccountID: x, Kind: econ.KindAcquireExposure, Mode: gates.ModePaper,
				Capability: gates.StrategyMarketplace, ActorType: security.ActorUser, ActorID: "follower", StrategyRef: "strategy-1",
				InstrumentID: m.inst.String(), AssetID: &base, Quantity: &qty, Terms: json.RawMessage(terms),
				IdempotencyKey: "follow-" + uuid.NewString()})
			return err
		}))
		steps := []econ.IntentStatus{econ.StatusRouted}
		if approve {
			steps = append(steps, econ.StatusRiskApproved)
		}
		for _, s := range steps {
			require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				var err error
				in, err = f.intents.Transition(ctx, tx, in.ID, s, systemActor, "the owner's stage", "")
				return err
			}))
		}
		return in
	}
	run := func(in econ.Intent) (PaperFill, error) {
		var fill PaperFill
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			fill, err = exec.ExecuteIntent(ctx, tx, in)
			return err
		})
		return fill, err
	}
	fill, err := run(ownIntent(`{"strategy":"momentum"}`, true))
	require.NoError(t, err)
	assert.Equal(t, units(1).String(), fill.BaseQuantity.String())
	assert.True(t, fill.Disclosure.Input.StrategyOriginated, "a strategy intent is priced as strategy-originated")

	_, err = run(ownIntent(`{}`, false))
	assert.Equal(t, ReasonPaperIntentNotApproved, ReasonOf(err))
	altered := ownIntent(`{}`, true)
	altered.ContentHash = make([]byte, 32)
	_, err = run(altered)
	assert.Equal(t, ReasonPaperIntentShape, ReasonOf(err))
	// 100.00 x 1.0025 = 100.25 is beyond a 100.10 protection price.
	_, err = run(ownIntent(`{"paper_protection_price":{"mantissa":"10010","scale":2}}`, true))
	assert.Equal(t, ReasonPaperProtection, ReasonOf(err))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM paper_fills WHERE account_id = $1`, x))
}
