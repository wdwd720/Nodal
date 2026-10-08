package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/treasury"
)

// Fixed identifiers of the treasury handler tests.
var (
	trUSD     = assets.NewAssetID()
	trBase    = assets.NewAssetID()
	trInst    = instruments.NewInstrumentID()
	trHouse   = econ.NewHouseAccountID()
	trJournal = ledger.NewTransactionID()
	trOrderID = mustTrID[orders.OrderID]("0193b2e0-0000-7000-8000-0000000000a1")
	trBookID  = mustTrID[orders.BookID]("0193b2e0-0000-7000-8000-0000000000a2")
	trAction  = mustTrID[treasury.ActionID]("0193b2e0-0000-7000-8000-0000000000a3")
	trDecided = mustTrID[treasury.DecisionID]("0193b2e0-0000-7000-8000-0000000000a4")
	trFillID  = mustTrID[treasury.PaperFillID]("0193b2e0-0000-7000-8000-0000000000a5")
	trFunding = mustTrID[treasury.FundingID]("0193b2e0-0000-7000-8000-0000000000a6")
	trPolicy  = mustTrID[treasury.PolicyID]("0193b2e0-0000-7000-8000-0000000000a7")
	trSnap    = mustTrID[treasury.SnapshotID]("0193b2e0-0000-7000-8000-0000000000a8")
	trEventID = mustTrID[treasury.PositionEventID]("0193b2e0-0000-7000-8000-0000000000a9")
	trDisc    = mustTrID[treasury.DisclosureID]("0193b2e0-0000-7000-8000-0000000000aa")
	trTradeID = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-0000000000ab")
	trEffect  = econ.NewEffectID()
	trOp      = econ.Actor{Type: security.ActorOperator, ID: testUserID.String()}
)

// mustTrID parses a fixed canonical id into a typed one whose kind the
// treasury keeps to itself.
func mustTrID[T any, PT interface {
	*T
	UnmarshalText([]byte) error
}](s string) T {
	var v T
	if err := PT(&v).UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return v
}

func trQty(n int64) money.Quantity { return money.QuantityFromInt64(n) }

// trDisclosure is a real disclosure of the fee engine: 1 unit bought at a
// reference price of 100.00 under 25 bps of spread and a 5 bps venue fee.
func trDisclosure(t *testing.T) fees.Disclosure {
	t.Helper()
	s := fees.ZeroSchedule(trUSD, 6)
	s.Version = "treasury-paper/v2/itest"
	s.SpreadBPS, s.VenuePassThroughBPS = 25, 5
	ref, err := money.PriceFromDecimalString("100.00", trUSD.String(), "test-oracle", testNow)
	require.NoError(t, err)
	in := fees.QuoteInput{Side: fees.SideBuy, BaseQty: trQty(1_000_000), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: ref}
	q, err := s.Quote(in)
	require.NoError(t, err)
	require.NoError(t, fees.VerifyQuote(s, in, q))
	return q.Disclosure()
}

// fakeTreasury is the treasury port. It counts every call, so a test can
// prove a replay never reached the service, and records what each call was
// asked.
type fakeTreasury struct {
	stubErr
	mu       sync.Mutex
	calls    map[string]int
	last     map[string]any
	book     TreasuryBookView
	outcome  treasury.OrderOutcome
	paper    TreasuryPaperView
	report   TreasuryReportView
	balances TreasuryBalancesView
	events   TreasuryEventPage
	fills    TreasuryPaperFillPage
	decide   TreasuryDecisionPage
	trades   TreasuryHouseTradePage
	cross    treasury.CrossingDisclosure
	found    []treasury.Discrepancy
	snapshot treasury.Snapshot
	next     string
}

func (f *fakeTreasury) hit(name string, arg any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	f.last[name] = arg
}

func (f *fakeTreasury) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeTreasury) arg(name string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[name]
}

// known answers NOT_FOUND for any house book but the fixed one.
func (f *fakeTreasury) known(house econ.HouseAccountID) error {
	if house != trHouse {
		return errs.New(errs.CodeNotFound, "treasury: treasury book not found")
	}
	return f.fail()
}

func (f *fakeTreasury) Books(_ context.Context, cursor string, limit int) (TreasuryBookPage, error) {
	f.hit("Books", []any{cursor, limit})
	return TreasuryBookPage{Items: []TreasuryBookView{f.book}, NextCursor: f.next}, f.fail()
}

func (f *fakeTreasury) Book(_ context.Context, house econ.HouseAccountID) (TreasuryBookView, error) {
	f.hit("Book", house)
	return f.book, f.known(house)
}

func (f *fakeTreasury) Register(_ context.Context, r TreasuryRegister) (TreasuryBookView, error) {
	f.hit("Register", r)
	return f.book, f.fail()
}

func (f *fakeTreasury) SetSigningPolicy(_ context.Context, r TreasurySigningChange) (treasury.SigningPolicyVersion, error) {
	f.hit("SetSigningPolicy", r)
	return *f.book.Signing, f.known(r.House)
}

func (f *fakeTreasury) SetRiskPolicy(_ context.Context, r TreasuryRiskChange) (treasury.PolicyVersion, error) {
	f.hit("SetRiskPolicy", r)
	return *f.book.Risk, f.known(r.House)
}

func (f *fakeTreasury) Fund(_ context.Context, r treasury.FundRequest) (TreasuryFundingView, error) {
	f.hit("Fund", r)
	mark := money.Price{Mantissa: trQty(10_050), Scale: 2, QuoteAsset: trUSD.String(), Source: "test-oracle", At: testNow}
	return TreasuryFundingView{
		Funding: treasury.Funding{ID: trFunding, HouseAccountID: trHouse, Asset: trBase, Quantity: r.Quantity, CostBasis: trQty(100_500_001),
			Mark: &mark, EffectID: trEffect, JournalTransactionID: trJournal, EvidenceRef: "evidence://treasury/funding", FundedAt: testNow,
			Action: f.outcome.Action},
		Asset: TreasuryAsset{ID: trBase, Symbol: "SBX-SOL", Decimals: 6},
	}, f.known(r.HouseAccountID)
}

func (f *fakeTreasury) PlaceOrder(_ context.Context, r TreasuryOrderCommand) (treasury.OrderOutcome, error) {
	f.hit("PlaceOrder", r)
	return f.outcome, f.known(r.House)
}

func (f *fakeTreasury) CancelOrder(_ context.Context, r treasury.CancelRequest) (treasury.OrderOutcome, error) {
	f.hit("CancelOrder", r)
	return f.outcome, f.known(r.HouseAccountID)
}

func (f *fakeTreasury) Quote(_ context.Context, r TreasuryQuoteCommand) (treasury.QuoteOutcome, error) {
	f.hit("Quote", r)
	ask := f.outcome
	ask.Action.Outcome, ask.Action.RejectionCode = treasury.OutcomeRejected, string(errs.CodeRiskRejected)
	ask.Action.ReasonCodes = []string{risk.ReasonCircuitBreaker, treasury.ReasonInventoryCap}
	ask.Order, ask.Trades = nil, nil
	return treasury.QuoteOutcome{Bid: f.outcome, Ask: ask}, f.known(r.House)
}

func (f *fakeTreasury) Hedge(_ context.Context, r treasury.PaperRequest) (TreasuryPaperView, error) {
	f.hit("Hedge", r)
	return f.paper, f.known(r.HouseAccountID)
}

func (f *fakeTreasury) Convert(_ context.Context, r treasury.ConvertRequest) (TreasuryPaperView, error) {
	f.hit("Convert", r)
	return f.paper, f.known(r.HouseAccountID)
}

func (f *fakeTreasury) Rebalance(_ context.Context, r treasury.RebalanceRequest) (TreasuryPaperView, error) {
	f.hit("Rebalance", r)
	if r.Target.IsZero() {
		return TreasuryPaperView{NoOp: true}, f.known(r.HouseAccountID)
	}
	return f.paper, f.known(r.HouseAccountID)
}

func (f *fakeTreasury) ExecutePaper(_ context.Context, r treasury.PaperRequest) (TreasuryPaperView, error) {
	f.hit("ExecutePaper", r)
	return f.paper, f.known(r.HouseAccountID)
}

func (f *fakeTreasury) SetCrossingRule(_ context.Context, r treasury.CrossingRuleRequest) (treasury.CrossingDisclosure, error) {
	f.hit("SetCrossingRule", r)
	return f.cross, f.fail()
}

func (f *fakeTreasury) Sync(_ context.Context, house econ.HouseAccountID) (int, error) {
	f.hit("Sync", house)
	return 3, f.known(house)
}

func (f *fakeTreasury) MarkToMarket(_ context.Context, house econ.HouseAccountID, key string, actor econ.Actor) (TreasurySnapshotView, error) {
	f.hit("MarkToMarket", []any{house, key, actor})
	return TreasurySnapshotView{Snapshot: f.snapshot, Report: f.report}, f.known(house)
}

func (f *fakeTreasury) Report(_ context.Context, house econ.HouseAccountID) (TreasuryReportView, error) {
	f.hit("Report", house)
	return f.report, f.known(house)
}

func (f *fakeTreasury) Balances(_ context.Context, house econ.HouseAccountID) (TreasuryBalancesView, error) {
	f.hit("Balances", house)
	return f.balances, f.known(house)
}

func (f *fakeTreasury) Reconcile(_ context.Context, house econ.HouseAccountID) ([]treasury.Discrepancy, error) {
	f.hit("Reconcile", house)
	return f.found, f.known(house)
}

func (f *fakeTreasury) Events(_ context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryEventPage, error) {
	f.hit("Events", []any{house, cursor, limit})
	return f.events, f.known(house)
}

func (f *fakeTreasury) Actions(_ context.Context, house econ.HouseAccountID, limit int) ([]treasury.Action, error) {
	f.hit("Actions", []any{house, limit})
	return []treasury.Action{f.outcome.Action}, f.known(house)
}

func (f *fakeTreasury) PaperFills(_ context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryPaperFillPage, error) {
	f.hit("PaperFills", []any{house, cursor, limit})
	return f.fills, f.known(house)
}

func (f *fakeTreasury) RiskDecisions(_ context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryDecisionPage, error) {
	f.hit("RiskDecisions", []any{house, cursor, limit})
	return f.decide, f.known(house)
}

func (f *fakeTreasury) CrossingDisclosures(_ context.Context, inst instruments.InstrumentID) ([]treasury.CrossingDisclosure, error) {
	f.hit("CrossingDisclosures", inst)
	return []treasury.CrossingDisclosure{f.cross}, f.fail()
}

func (f *fakeTreasury) HouseTrades(_ context.Context, acct accounts.AccountID, cursor string, limit int) (TreasuryHouseTradePage, error) {
	f.hit("HouseTrades", acct)
	return f.trades, f.fail()
}

// newFakeTreasury holds one house book (market making, operators allowed, a
// risk policy), an applied house order with one trade, an applied paper fill
// with its real disclosure, and a report whose limits are in use.
func newFakeTreasury(t *testing.T) *fakeTreasury {
	t.Helper()
	signing := treasury.SigningPolicyVersion{ID: trPolicy, HouseAccountID: trHouse, Version: 1,
		Policy: treasury.SigningPolicy{ServiceActorID: "house-service-1", OperatorsAllowed: true}, Hash: "5ad1", Actor: trOp,
		Reason: "first policy", EffectiveAt: testNow}
	policy := treasury.PolicyVersion{ID: trPolicy, HouseAccountID: trHouse, Version: 2, Hash: "9c0e", Actor: trOp, Reason: "limits",
		EffectiveAt: testNow, Policy: treasury.Policy{
			Label: "mm-limits", ValuationAsset: trUSD, DailyLossLimit: trQty(1_000_000_000), MaxMarkAge: 30 * time.Second,
			MaxInventory:          []treasury.AssetLimit{{Asset: trBase, MaxQuantity: trQty(100_000_000)}},
			MaxInstrumentExposure: []treasury.InstrumentLimit{{Instrument: trInst, MaxExposure: trQty(5_000_000_000)}},
			PaperSpreadBPS:        25, PaperFeeBPS: 5,
		}}
	book := treasury.Book{HouseAccountID: trHouse, Name: "mm book", Strategy: econ.HouseMarketMaking, Class: treasury.ClassPrincipal,
		Status: econ.HouseActive, ValuationAsset: trUSD, Version: 7, RegisteredBy: trOp, RegisteredAt: testNow}
	riskVersion := 2
	action := treasury.Action{ID: trAction, HouseAccountID: trHouse, Kind: treasury.ActionPlaceOrder, Activity: econ.HouseMarketMaking,
		Outcome: treasury.OutcomeApplied, ReasonCodes: []string{}, Actor: trOp, SigningPolicyVersion: 1, RiskPolicyVersion: riskVersion,
		IdempotencyKey: "order-key-0001", RiskDecisionID: &trDecided, OrderID: &trOrderID, EvidenceRef: "evidence://treasury/action",
		EvidenceSHA256: []byte{0xde, 0xad}, OccurredAt: testNow}
	house := trHouse
	order := orders.OrderView{ID: trOrderID, BookID: trBookID, InstrumentID: trInst, OwnerType: orders.OwnerHouse, OwnerID: trHouse.String(),
		HouseAccountID: &house, Side: orders.SideBuy, Type: orders.TypeLimit, TimeInForce: orders.GTC, PostOnly: true, Mode: gates.ModePaper,
		Price: "99.95", Quantity: trQty(2_000_000), FilledQuantity: trQty(500_000), RemainingQuantity: trQty(1_500_000),
		CancelledQuantity: trQty(0), Status: orders.StatusPartiallyFilled, CreatedAt: testNow, UpdatedAt: testNow}
	tradePrice := money.Price{Mantissa: trQty(9_995), Scale: 2, QuoteAsset: trUSD.String(), Source: "internal-book", At: testNow}
	trade := orders.Trade{ID: trTradeID, BookID: trBookID, TradeSeq: 41, Price: tradePrice, BaseQuantity: trQty(500_000),
		QuoteNotional: trQty(49_975_000), MakerOrderID: trOrderID, TakerOrderID: trOrderID, MakerOwnerType: orders.OwnerHouse,
		TakerOwnerType: orders.OwnerCustomer, AggressorSide: orders.SideSell, MakerFee: trQty(0), TakerFee: trQty(49_975),
		CrossingRuleVersion: 3, JournalTransactionID: trJournal}
	invCap := trQty(100_000_000)
	decision := treasury.RiskDecision{ID: trDecided, Verdict: risk.Allow, ReasonCodes: []string{}, TreasuryCodes: []string{},
		DecisionHash: "d1", InputHash: "i1", Limits: treasury.LimitEvaluation{RiskIncreasing: true, Codes: []string{}, Breakers: []string{},
			LossMeasure: trQty(-12_345), DailyLossLimit: trQty(1_000_000_000), MissingMarks: []string{}, InventoryAfter: trQty(3_500_000),
			InventoryCap: &invCap}}
	d := trDisclosure(t)
	ref, err := money.PriceFromDecimalString("100.00", trUSD.String(), "test-oracle", testNow)
	require.NoError(t, err)
	fill := treasury.PaperFill{ID: trFillID, HouseAccountID: trHouse, Activity: econ.HouseHedging, VenueLabel: treasury.VenueLabelPaper,
		Side: orders.SideBuy, BaseAsset: trBase, QuoteAsset: trUSD, BaseQuantity: trQty(1_000_000), ReferencePrice: ref,
		NotionalAtReference: d.NotionalAtReference, SpreadBPS: d.Spread.BPS, SpreadAmount: d.Spread.Amount, FeeAmount: d.Fees.Total,
		TotalQuote: d.TotalDebit, ScheduleHash: d.ScheduleHash, Disclosure: d, DisclosureHash: "3f1d", EffectID: trEffect,
		JournalTransactionID: trJournal, EvidenceRef: "evidence://treasury/paper", FilledAt: testNow}
	paperAction := action
	paperAction.Kind, paperAction.Activity, paperAction.OrderID, paperAction.PaperFillID = treasury.ActionPaperFill, econ.HouseHedging, nil, &trFillID
	known := TreasuryAssets{trUSD: {ID: trUSD, Symbol: "SBX-USD", Decimals: 6}, trBase: {ID: trBase, Symbol: "SBX-SOL", Decimals: 6}}
	mark := treasury.MarkRecord{Mantissa: trQty(10_100), Scale: 2, Source: "test-oracle", At: testNow}
	marketValue, unrealized := trQty(353_500_000), trQty(3_499_999)
	rep := treasury.Report{Kind: treasury.ReportKind, HouseAccountID: trHouse.String(), ValuationAsset: trUSD.String(), At: testNow,
		TradingDay: "2026-09-06", ThroughEventSeq: 12,
		Assets: []treasury.AssetPnL{{Asset: trBase.String(), Quantity: trQty(3_500_000), Cost: trQty(350_000_001), Mark: &mark,
			MarketValue: &marketValue, Unrealized: &unrealized, RealizedTotal: trQty(-12_345), RealizedToday: trQty(-12_345)}},
		RealizedTotal: trQty(-12_345), RealizedToday: trQty(-12_345), UnrealizedTotal: trQty(3_499_999), OpenLosses: trQty(0),
		LossMeasure: trQty(-12_345), MissingMarks: []string{}, Rounding: map[string]string{"cost_released": "up", "market_value": "down"}}
	lossCap, lossUsed, usedInv, expCap, expUsed := trQty(1_000_000_000), trQty(-12_345), trQty(3_500_000), trQty(5_000_000_000), trQty(503_500_000)
	base, inst := trBase, trInst
	return &fakeTreasury{
		calls: map[string]int{}, last: map[string]any{},
		book:    TreasuryBookView{Book: book, ValuationAsset: known[trUSD], Signing: &signing, Risk: &policy},
		outcome: treasury.OrderOutcome{Action: action, Order: &order, Trades: []orders.Trade{trade}, Decision: &decision},
		paper:   TreasuryPaperView{Outcome: treasury.PaperOutcome{Action: paperAction, Fill: &fill, Decision: &decision}, Assets: known},
		report: TreasuryReportView{Report: rep, Valuation: known[trUSD], Assets: known, RiskPolicyVersion: 2, Limits: []TreasuryLimitUse{
			{Kind: TreasuryLimitKindDailyLoss, Cap: &lossCap, Used: &lossUsed, Codes: []string{}},
			{Kind: TreasuryLimitKindInventory, Asset: &base, Cap: &invCap, Used: &usedInv, Codes: []string{}},
			{Kind: TreasuryLimitKindInstrumentExposure, Instrument: &inst, Asset: &base, Cap: &expCap, Used: &expUsed,
				Codes: []string{treasury.ReasonInstrumentExposure}},
		}},
		balances: TreasuryBalancesView{House: trHouse, Assets: known, Items: []treasury.AssetBalance{{Asset: trBase, Inventory: trQty(3_500_000),
			Reserved: trQty(1_000_000), Available: trQty(2_500_000), Funded: trQty(0),
			InternalBook: treasury.VenueFlow{Bought: trQty(500_000), Sold: trQty(0)},
			PaperVenue:   treasury.VenueFlow{Bought: trQty(3_000_000), Sold: trQty(0)}, WorkingBuys: trQty(1_500_000), WorkingSells: trQty(1_000_000)}}},
		events: TreasuryEventPage{Assets: known, NextCursor: "next-events", Items: []treasury.PositionEvent{{ID: trEventID, HouseAccountID: trHouse,
			Seq: 12, Kind: treasury.EventBookFill, Asset: trBase, BookTradeID: &trTradeID, InstrumentID: &inst, JournalTransactionID: trJournal,
			OccurredAt: testNow, Movement: treasury.Movement{Direction: treasury.Increase, Quantity: trQty(500_000), Value: trQty(49_975_000),
				Fees: trQty(0), Before: treasury.Position{Quantity: trQty(3_000_000), Cost: trQty(300_025_001)},
				After: treasury.Position{Quantity: trQty(3_500_000), Cost: trQty(350_000_001)}, CostRemoved: trQty(0), Realized: trQty(0)}}}},
		fills: TreasuryPaperFillPage{Assets: known, Items: []treasury.PaperFill{fill}},
		decide: TreasuryDecisionPage{Items: []TreasuryDecisionRecord{{ID: trDecided, HouseAccountID: trHouse, IntentKind: "PROVIDE_LIQUIDITY",
			Instrument: &inst, Asset: trBase, RiskIncreasing: true, Stage: "PRE_TRADE", Verdict: "REJECT",
			ReasonCodes: []string{risk.ReasonCircuitBreaker}, TreasuryCodes: []string{treasury.ReasonInventoryCap},
			Limits: decision.Limits, InputHash: "i2", DecisionHash: "d2", RiskPolicyVersion: "risk-v2", EvaluatorVersion: "eval-1",
			TreasuryPolicyVersion: 2, TreasuryPolicyHash: strings.Repeat("a", 64), EvaluatedAt: testNow}}},
		trades: TreasuryHouseTradePage{Items: []TreasuryHouseTrade{{TradeID: trTradeID, BookID: trBookID, Instrument: trInst, TradeSeq: 41,
			House: trHouse, HouseSide: orders.SideBuy, AccountSide: orders.SideSell, Price: tradePrice, Base: known[trBase], Quote: known[trUSD],
			BaseQuantity: trQty(500_000), QuoteNotional: trQty(49_975_000), AccountFee: trQty(49_975), CrossingRuleVersion: 3,
			Disclosure: "The house book may trade against you here; this is simulated activity.", JournalTransaction: trJournal,
			TradedAt: testNow}}},
		cross: treasury.CrossingDisclosure{ID: trDisc, InstrumentID: trInst, BookID: trBookID, RuleVersion: 3, Allow: true,
			Disclosure: "The house book may trade against you here; this is simulated activity.", Reason: "sandbox market making",
			Actor: trOp, EvidenceRef: "evidence://treasury/crossing", EvidenceSHA256: []byte{0xbe, 0xef}, DisclosedAt: testNow},
		found:    []treasury.Discrepancy{{Asset: trBase, What: "position vs HOUSE_INVENTORY", Expected: trQty(3_500_000), Actual: trQty(3_499_999)}},
		snapshot: treasury.Snapshot{ID: trSnap, Report: rep, DocumentHash: "77aa", Action: action},
	}
}

func newTreasuryHarness(t *testing.T) (*harness, *fakeTreasury) {
	t.Helper()
	h := newHarness(t)
	ft := newFakeTreasury(t)
	ports := h.ports.ports()
	ports.Treasury = ft
	h.server.opts.Ports = ports
	op := operatorPrincipal()
	h.as(&op)
	return h, ft
}

func trBook() string { return "/v1/advanced/treasury/books/" + trHouse.String() }

// treasuryMutation is one treasury command with a body, a different body
// under the same key, the fake method it reaches and whether it needs a
// step-up.
type treasuryMutation struct {
	name    string
	path    string
	altPath string
	body    any
	altBody any
	counter string
	stepUp  bool
}

func treasuryMutations() []treasuryMutation {
	book := trBook()
	order := map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING", "side": "BUY", "order_type": "LIMIT",
		"time_in_force": "GTC", "price": "99.95", "quantity": "2000000", "post_only": true}
	altOrder := map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING", "side": "BUY", "order_type": "LIMIT",
		"time_in_force": "GTC", "price": "99.96", "quantity": "2000000", "post_only": true}
	limits := map[string]any{"label": "mm-limits", "daily_loss_limit": "1000000000", "max_mark_age_ms": 30000,
		"max_inventory":           []any{map[string]any{"asset_id": trBase.String(), "max_quantity": "100000000"}},
		"max_instrument_exposure": []any{map[string]any{"instrument_id": trInst.String(), "max_exposure": "5000000000"}},
		"paper_spread_bps":        25, "paper_fee_bps": 5, "reason": "limits"}
	altLimits := map[string]any{"label": "mm-limits-2", "daily_loss_limit": "1000000000", "max_mark_age_ms": 30000,
		"max_inventory": []any{}, "max_instrument_exposure": []any{}, "paper_spread_bps": 25, "paper_fee_bps": 5, "reason": "limits"}
	return []treasuryMutation{
		{name: "register", path: "/v1/advanced/treasury/books", counter: "Register", stepUp: true,
			body: map[string]any{"name": "mm book", "strategy": "MARKET_MAKING", "service_actor_id": "house-service-1",
				"operators_allowed": true, "reason": "open the sandbox market maker"},
			altBody: map[string]any{"name": "mm book 2", "strategy": "MARKET_MAKING", "service_actor_id": "house-service-1",
				"operators_allowed": true, "reason": "open the sandbox market maker"}},
		{name: "actor-policy", path: book + "/actor-policy", counter: "SetSigningPolicy", stepUp: true,
			body:    map[string]any{"service_actor_id": "house-service-2", "operators_allowed": false, "reason": "rotate the house service"},
			altBody: map[string]any{"service_actor_id": "house-service-3", "operators_allowed": false, "reason": "rotate the house service"}},
		{name: "limits", path: book + "/limits", counter: "SetRiskPolicy", stepUp: true, body: limits, altBody: altLimits},
		{name: "fund", path: book + "/fundings", counter: "Fund", stepUp: true,
			body:    map[string]any{"asset_id": trBase.String(), "quantity": "1000000", "reason": "seed"},
			altBody: map[string]any{"asset_id": trBase.String(), "quantity": "1000001", "reason": "seed"}},
		{name: "order", path: book + "/orders", counter: "PlaceOrder", stepUp: true, body: order, altBody: altOrder},
		{name: "cancel", path: book + "/orders/" + trOrderID.String() + "/cancel", counter: "CancelOrder",
			altPath: book + "/orders/" + testOrderID.String() + "/cancel"},
		{name: "quote", path: book + "/quotes", counter: "Quote", stepUp: true,
			body: map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING", "bid_price": "99.90", "bid_quantity": "1000000",
				"ask_price": "100.10", "ask_quantity": "1000000"},
			altBody: map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING", "bid_price": "99.80", "bid_quantity": "1000000",
				"ask_price": "100.10", "ask_quantity": "1000000"}},
		{name: "hedge", path: book + "/hedges", counter: "Hedge", stepUp: true,
			body:    map[string]any{"asset_id": trBase.String(), "side": "BUY", "quantity": "1000000"},
			altBody: map[string]any{"asset_id": trBase.String(), "side": "SELL", "quantity": "1000000"}},
		{name: "convert", path: book + "/conversions", counter: "Convert", stepUp: true,
			body:    map[string]any{"from_asset_id": trUSD.String(), "to_asset_id": trBase.String(), "quantity": "1000000"},
			altBody: map[string]any{"from_asset_id": trBase.String(), "to_asset_id": trUSD.String(), "quantity": "1000000"}},
		{name: "rebalance", path: book + "/rebalances", counter: "Rebalance", stepUp: true,
			body:    map[string]any{"asset_id": trBase.String(), "target": "2000000"},
			altBody: map[string]any{"asset_id": trBase.String(), "target": "2000001"}},
		{name: "paper", path: book + "/paper-fills", counter: "ExecutePaper", stepUp: true,
			body:    map[string]any{"activity": "HEDGING", "asset_id": trBase.String(), "side": "BUY", "quantity": "1000000", "mode": "SANDBOX"},
			altBody: map[string]any{"activity": "HEDGING", "asset_id": trBase.String(), "side": "BUY", "quantity": "1000000", "mode": "PAPER"}},
		{name: "crossing", path: "/v1/advanced/treasury/crossing-rules", counter: "SetCrossingRule", stepUp: true,
			body: map[string]any{"instrument_id": trInst.String(), "allow": true, "reason": "sandbox market making",
				"disclosure": "The house book may trade against you here; this is simulated activity."},
			altBody: map[string]any{"instrument_id": trInst.String(), "allow": false, "reason": "sandbox market making over",
				"disclosure": "The house book no longer trades against you here; this is simulated activity."}},
		{name: "sync", path: book + "/sync", counter: "Sync", altPath: "/v1/advanced/treasury/books/" + testOrderID.String() + "/sync"},
		{name: "mark", path: book + "/mark-to-market", counter: "MarkToMarket",
			altPath: "/v1/advanced/treasury/books/" + testOrderID.String() + "/mark-to-market"},
	}
}

// --- authorization ---------------------------------------------------------------

// The treasury is the operator's: a customer reaches none of its reads or
// commands, and the service is never asked.
func TestTreasuryRoutes_RefuseACustomer(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)
	c := customerPrincipal()
	h.as(&c)
	for _, path := range []string{"/v1/advanced/treasury/books", trBook(), trBook() + "/report", trBook() + "/balances",
		trBook() + "/reconciliation", trBook() + "/events", trBook() + "/actions", trBook() + "/paper-fills", trBook() + "/risk-decisions"} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	}
	for _, m := range treasuryMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "customer-tr-"+m.name)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Zero(t, ft.count(m.counter), m.name)
	}
}

// Everything that moves value or changes a policy or a crossing rule needs a
// recent strong authentication; a cancel, a sync and a mark do not.
func TestTreasuryCommands_RequireStepUpWhereValueOrPolicyMoves(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)
	stale := operatorPrincipal()
	stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
	h.as(&stale)
	for _, m := range treasuryMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stale-tr-"+m.name)
		if m.stepUp {
			require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code, m.name)
			assert.Zero(t, ft.count(m.counter), m.name)
			continue
		}
		require.Equal(t, http.StatusOK, res.Code, "%s needs no step-up; body=%s", m.name, res.Body.String())
	}
}

// The crossing disclosures are what customers are told: a customer reads
// them, and so does an operator.
func TestTreasuryCrossingDisclosures_AreReadableByCustomers(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)
	for _, p := range []security.Principal{customerPrincipal(), operatorPrincipal()} {
		h.as(&p)
		res := h.do(http.MethodGet, "/v1/advanced/treasury/crossing-disclosures?instrument_id="+trInst.String(), nil)
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		raw := res.raw()
		assert.Equal(t, "SIMULATED", raw["value_domain"])
		item := raw["items"].([]any)[0].(map[string]any)
		assert.Equal(t, "The house book may trade against you here; this is simulated activity.", item["disclosure"])
		assert.Equal(t, float64(3), item["rule_version"])
		assert.Equal(t, "beef", item["evidence_sha256"])
	}
	assert.Equal(t, trInst, ft.arg("CrossingDisclosures"))
}

// The house trades against an account are its owner's (or an operator's who
// may read any account).
func TestTreasuryHouseTrades_AreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)
	c := customerPrincipal()
	h.as(&c)
	res := h.do(http.MethodGet, "/v1/advanced/treasury/house-trades?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, ft.arg("HouseTrades"))
	item := res.raw()["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "SELL", item["account_side"])
	assert.Equal(t, "BUY", item["house_side"])
	assert.Equal(t, "49975", item["account_fee"])
	assert.Equal(t, "9995", item["price"].(map[string]any)["mantissa"])
	assert.Equal(t, float64(2), item["price"].(map[string]any)["scale"])
	assert.Equal(t, "The house book may trade against you here; this is simulated activity.", item["disclosure"])

	res = h.do(http.MethodGet, "/v1/advanced/treasury/house-trades?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, ft.count("HouseTrades"))

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/treasury/house-trades?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, ft.arg("HouseTrades"))
}

// --- reads -----------------------------------------------------------------------

func TestTreasuryReads_RenderTheServicesFiguresExactly(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)

	res := h.do(http.MethodGet, "/v1/advanced/treasury/books?limit=7", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	item := raw["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "PRINCIPAL", item["class"])
	assert.Equal(t, []any{"MARKET_MAKING", "HEDGING"}, item["permitted_activities"])
	assert.Equal(t, float64(6), item["valuation_asset"].(map[string]any)["decimals"])
	assert.Equal(t, []any{"", 7}, ft.arg("Books"))

	res = h.do(http.MethodGet, trBook(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, "NONE_SANDBOX", raw["signing_policy"].(map[string]any)["key_custody"])
	rp := raw["risk_policy"].(map[string]any)
	assert.Equal(t, "1000000000", rp["daily_loss_limit"])
	assert.Equal(t, float64(30000), rp["max_mark_age_ms"])
	assert.Equal(t, "100000000", rp["max_inventory"].([]any)[0].(map[string]any)["max_quantity"])

	res = h.do(http.MethodGet, trBook()+"/report", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "-12345", raw["loss_measure"])
	assert.Equal(t, "3499999", raw["unrealized_total"])
	line := raw["assets"].([]any)[0].(map[string]any)
	assert.Equal(t, "350000001", line["cost"])
	assert.Equal(t, "353500000", line["market_value"])
	assert.Equal(t, "SBX-SOL", line["asset"].(map[string]any)["symbol"])
	limits := raw["limits"].([]any)
	require.Len(t, limits, 3)
	assert.Equal(t, "DAILY_LOSS", limits[0].(map[string]any)["kind"])
	assert.Equal(t, "-12345", limits[0].(map[string]any)["used"])
	assert.Equal(t, "503500000", limits[2].(map[string]any)["used"])
	assert.Equal(t, []any{treasury.ReasonInstrumentExposure}, limits[2].(map[string]any)["codes"])
	assert.Equal(t, float64(2), raw["risk_policy_version"])

	res = h.do(http.MethodGet, trBook()+"/balances", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	bal := res.raw()["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "2500000", bal["available"])
	assert.Equal(t, "3000000", bal["paper_venue"].(map[string]any)["bought"])

	res = h.do(http.MethodGet, trBook()+"/reconciliation", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, false, raw["agrees"])
	assert.Equal(t, "3499999", raw["discrepancies"].([]any)[0].(map[string]any)["actual"])

	res = h.do(http.MethodGet, trBook()+"/events?cursor=abc&limit=5", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, "next-events", raw["next_cursor"])
	ev := raw["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "350000001", ev["cost_after"])
	assert.Equal(t, trTradeID.String(), ev["book_trade_id"])
	assert.Equal(t, []any{trHouse, "abc", 5}, ft.arg("Events"))

	res = h.do(http.MethodGet, trBook()+"/actions", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	act := res.raw()["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "dead", act["evidence_sha256"])
	assert.Equal(t, float64(2), act["risk_policy_version"])

	res = h.do(http.MethodGet, trBook()+"/paper-fills", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	fill := res.raw()["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "SIMULATED_PAPER_VENUE", fill["venue_label"])
	d := fill["disclosure"].(map[string]any)
	assert.Equal(t, "250000", d["spread"].(map[string]any)["amount"], "25 bps of 100.00 on one unit is exactly 0.25")
	assert.Equal(t, "100300000", d["total_debit"], "notional + spread + venue fee, exactly")
	assert.Equal(t, fill["total_quote"], d["total_debit"])

	res = h.do(http.MethodGet, trBook()+"/risk-decisions", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	dec := res.raw()["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "REJECT", dec["verdict"])
	assert.Equal(t, []any{treasury.ReasonInventoryCap}, dec["treasury_codes"])
	assert.Equal(t, "3500000", dec["limits"].(map[string]any)["inventory_after"])
}

// An unknown house book answers NOT_FOUND on every read, and a nil id is a
// validation error that never reaches the service.
func TestTreasuryReads_OfAnUnknownBookAreNotFound(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)
	unknown := "/v1/advanced/treasury/books/" + testOrderID.String()
	for _, suffix := range []string{"", "/report", "/balances", "/reconciliation", "/events", "/actions", "/paper-fills", "/risk-decisions"} {
		res := h.do(http.MethodGet, unknown+suffix, nil)
		require.Equal(t, http.StatusNotFound, res.Code, "%s body=%s", suffix, res.Body.String())
		assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	}
	before := len(ft.calls)
	res := h.do(http.MethodGet, "/v1/advanced/treasury/books/"+nilUUIDText, nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "houseBookId", res.problem().Fields["field"])
	assert.Equal(t, before, len(ft.calls))
}

// --- commands ------------------------------------------------------------------------

// Every treasury command replays: the same key and body answer the same
// response without a second call; the same key with another request is
// refused.
func TestTreasuryCommands_ReplayAndRefuseKeyReuse(t *testing.T) {
	t.Parallel()
	for _, m := range treasuryMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, ft := newTreasuryHarness(t)
			key := "replay-tr-" + m.name + "-0"
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
			require.Equal(t, 1, ft.count(m.counter))
			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())
			assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers the first response")
			assert.Equal(t, 1, ft.count(m.counter), "a replay never reaches the service")

			path, body := m.path, m.altBody
			if m.altPath != "" {
				path, body = m.altPath, m.body
			}
			reuse := h.do(http.MethodPost, path, body, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
			assert.Equal(t, 1, ft.count(m.counter))
		})
	}
}

// The handler hands the treasury exactly the request: the caller as the
// actor, the Idempotency-Key as the treasury's own key, quantities exact.
func TestTreasuryCommands_PassTheRequestToTheService(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)
	ms := map[string]treasuryMutation{}
	for _, m := range treasuryMutations() {
		ms[m.name] = m
	}
	res := h.do(http.MethodPost, ms["order"].path, ms["order"].body, "Idempotency-Key", "pass-order-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	got := ft.arg("PlaceOrder").(TreasuryOrderCommand)
	assert.Equal(t, trHouse, got.House)
	assert.Equal(t, trInst, got.Instrument)
	assert.Equal(t, "99.95", got.Price, "the price is passed exactly, as the decimal it was")
	assert.Equal(t, "2000000", got.Quantity.String())
	assert.True(t, got.PostOnly)
	assert.Equal(t, "pass-order-0001", got.IdempotencyKey)
	assert.Equal(t, trOp, got.Actor)
	out := res.raw()
	assert.Equal(t, "SIMULATED", out["value_domain"])
	assert.Equal(t, "APPLIED", out["action"].(map[string]any)["outcome"])
	assert.Equal(t, "1500000", out["order"].(map[string]any)["remaining_quantity"])
	assert.Equal(t, float64(3), out["trades"].([]any)[0].(map[string]any)["crossing_rule_version"])
	assert.Equal(t, "3500000", out["decision"].(map[string]any)["limits"].(map[string]any)["inventory_after"])

	res = h.do(http.MethodPost, ms["limits"].path, ms["limits"].body, "Idempotency-Key", "pass-limits-001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	pol := ft.arg("SetRiskPolicy").(TreasuryRiskChange)
	assert.Equal(t, 30*time.Second, pol.Policy.MaxMarkAge)
	assert.Equal(t, "1000000000", pol.Policy.DailyLossLimit.String())
	assert.Equal(t, money.BPS(25), pol.Policy.PaperSpreadBPS)
	require.Len(t, pol.Policy.MaxInventory, 1)
	assert.True(t, pol.Policy.ValuationAsset.IsZero(), "the valuation asset is the book's, stated by the adapter")

	res = h.do(http.MethodPost, ms["paper"].path, ms["paper"].body, "Idempotency-Key", "pass-paper-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	paper := ft.arg("ExecutePaper").(treasury.PaperRequest)
	assert.Equal(t, econ.HouseHedging, paper.Activity)
	assert.Equal(t, gates.ModeSandbox, paper.Mode)
	fill := res.raw()["fill"].(map[string]any)
	assert.Equal(t, "SBX-SOL", fill["base_asset"].(map[string]any)["symbol"])

	res = h.do(http.MethodPost, ms["hedge"].path, ms["hedge"].body, "Idempotency-Key", "pass-hedge-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, gates.Mode(""), ft.arg("Hedge").(treasury.PaperRequest).Mode, "an absent mode is the service's default")

	res = h.do(http.MethodPost, ms["register"].path, ms["register"].body, "Idempotency-Key", "pass-register-1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	reg := ft.arg("Register").(TreasuryRegister)
	assert.Nil(t, reg.ValuationAsset, "the sandbox dollar unless the operator names another")
	assert.True(t, reg.Signing.OperatorsAllowed)

	res = h.do(http.MethodPost, ms["mark"].path, nil, "Idempotency-Key", "pass-mark-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, []any{trHouse, "pass-mark-00001", trOp}, ft.arg("MarkToMarket"))
	assert.Equal(t, "77aa", res.raw()["document_hash"])
}

// A two-sided quote answers both legs, each with its own outcome: one leg can
// be refused while the other rests.
func TestTreasuryQuote_AnswersEachLegsOutcome(t *testing.T) {
	t.Parallel()
	h, _ := newTreasuryHarness(t)
	m := treasuryMutations()[6]
	require.Equal(t, "quote", m.name)
	res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "quote-legs-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "APPLIED", raw["bid"].(map[string]any)["action"].(map[string]any)["outcome"])
	ask := raw["ask"].(map[string]any)["action"].(map[string]any)
	assert.Equal(t, "REJECTED", ask["outcome"])
	assert.Equal(t, string(errs.CodeRiskRejected), ask["rejection_code"])
	assert.Equal(t, []any{risk.ReasonCircuitBreaker, treasury.ReasonInventoryCap}, ask["reason_codes"])
	assert.Nil(t, raw["ask"].(map[string]any)["order"])
}

// A rebalance already at its target fills and records nothing, and says so.
func TestTreasuryRebalance_AtItsTargetIsANoOp(t *testing.T) {
	t.Parallel()
	h, _ := newTreasuryHarness(t)
	res := h.do(http.MethodPost, trBook()+"/rebalances", map[string]any{"asset_id": trBase.String(), "target": "0"},
		"Idempotency-Key", "rebalance-noop-1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, true, raw["no_op"])
	assert.Nil(t, raw["action"])
	assert.Nil(t, raw["fill"])
	assert.Equal(t, "SIMULATED", raw["value_domain"])
}

// A refusal the treasury recorded reaches the wire with its code, its reason
// and its reason codes, and replaying the key repeats it without asking again.
func TestTreasuryRefusals_KeepTheirCodeAndReplay(t *testing.T) {
	t.Parallel()
	h, ft := newTreasuryHarness(t)
	ft.err = errs.New(errs.CodeRiskRejected, "treasury: PLACE_ORDER was refused").WithField("reason", treasury.ReasonInventoryCap).
		WithField("rejection_code", string(errs.CodeRiskRejected)).
		WithField("reason_codes", []string{risk.ReasonCircuitBreaker, treasury.ReasonInventoryCap}).
		WithField("action_id", trAction.String())
	m := treasuryMutations()[4]
	require.Equal(t, "order", m.name)
	res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "refused-order-01")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeRiskRejected, p.Code)
	assert.Equal(t, treasury.ReasonInventoryCap, p.Fields["reason"])
	assert.Equal(t, []any{risk.ReasonCircuitBreaker, treasury.ReasonInventoryCap}, p.Fields["reason_codes"])
	assert.Equal(t, trAction.String(), p.Fields["action_id"])

	ft.err = nil
	again := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "refused-order-01")
	require.Equal(t, http.StatusUnprocessableEntity, again.Code, "body=%s", again.Body.String())
	assert.Equal(t, errs.CodeRiskRejected, again.problem().Code)
	assert.Equal(t, 1, ft.count("PlaceOrder"))

	// An operator the book's signing policy does not let trade is refused
	// with the treasury's FORBIDDEN, and a kill switch keeps its own code.
	ft.err = errs.New(errs.CodeForbidden, "treasury: the house book's signing policy does not let operators trade for it").
		WithField("reason", treasury.ReasonNotAuthorized)
	res = h.do(http.MethodPost, trBook()+"/hedges", map[string]any{"asset_id": trBase.String(), "side": "BUY", "quantity": "1"},
		"Idempotency-Key", "refused-hedge-01")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, treasury.ReasonNotAuthorized, res.problem().Fields["reason"])
	ft.err = errs.New(errs.CodeKillSwitchActive, "kill switch active").WithField("reason", treasury.ReasonKillSwitch)
	res = h.do(http.MethodPost, trBook()+"/hedges", map[string]any{"asset_id": trBase.String(), "side": "BUY", "quantity": "2"},
		"Idempotency-Key", "refused-hedge-02")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeKillSwitchActive, res.problem().Code)
}

func TestTreasuryRequests_AreValidatedBeforeTheService(t *testing.T) {
	t.Parallel()
	book := trBook()
	cases := []struct {
		name, path string
		body       any
		field      string
	}{
		{"unknown strategy", "/v1/advanced/treasury/books", map[string]any{"name": "x", "strategy": "ARBITRAGE",
			"service_actor_id": "s", "operators_allowed": true, "reason": "r"}, "strategy"},
		{"blank name", "/v1/advanced/treasury/books", map[string]any{"name": "  ", "strategy": "HEDGING",
			"service_actor_id": "s", "operators_allowed": true, "reason": "r"}, "name"},
		{"nil valuation asset", "/v1/advanced/treasury/books", map[string]any{"name": "x", "strategy": "HEDGING",
			"valuation_asset_id": nilUUIDText, "service_actor_id": "s", "operators_allowed": true, "reason": "r"}, "valuation_asset_id"},
		{"fractional limit", book + "/limits", map[string]any{"label": "l", "daily_loss_limit": "10.5", "max_mark_age_ms": 1000,
			"max_inventory": []any{}, "max_instrument_exposure": []any{}, "paper_spread_bps": 0, "paper_fee_bps": 0, "reason": "r"}, "daily_loss_limit"},
		{"inventory asset", book + "/limits", map[string]any{"label": "l", "daily_loss_limit": "10", "max_mark_age_ms": 1000,
			"max_inventory": []any{map[string]any{"asset_id": nilUUIDText, "max_quantity": "1"}}, "max_instrument_exposure": []any{},
			"paper_spread_bps": 0, "paper_fee_bps": 0, "reason": "r"}, "max_inventory.asset_id"},
		{"funding in dollars", book + "/fundings", map[string]any{"asset_id": trUSD.String(), "quantity": "1.00", "reason": "r"}, "quantity"},
		{"funding mode", book + "/fundings", map[string]any{"asset_id": trUSD.String(), "quantity": "1", "mode": "ON_CHAIN", "reason": "r"}, "mode"},
		{"order price", book + "/orders", map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING", "side": "BUY",
			"order_type": "LIMIT", "time_in_force": "GTC", "price": "1e2", "quantity": "1"}, "price"},
		{"order side", book + "/orders", map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING", "side": "HOLD",
			"order_type": "LIMIT", "time_in_force": "GTC", "price": "100", "quantity": "1"}, "side"},
		{"order activity", book + "/orders", map[string]any{"instrument_id": trInst.String(), "activity": "SPECULATION", "side": "BUY",
			"order_type": "LIMIT", "time_in_force": "GTC", "price": "100", "quantity": "1"}, "activity"},
		{"order tif", book + "/orders", map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING", "side": "BUY",
			"order_type": "LIMIT", "time_in_force": "DAY", "price": "100", "quantity": "1"}, "time_in_force"},
		{"order instrument", book + "/orders", map[string]any{"instrument_id": nilUUIDText, "activity": "MARKET_MAKING", "side": "BUY",
			"order_type": "LIMIT", "time_in_force": "GTC", "price": "100", "quantity": "1"}, "instrument_id"},
		{"quote ask price", book + "/quotes", map[string]any{"instrument_id": trInst.String(), "activity": "MARKET_MAKING",
			"bid_price": "99", "bid_quantity": "1", "ask_price": "-1", "ask_quantity": "1"}, "ask_price"},
		{"hedge quantity", book + "/hedges", map[string]any{"asset_id": trBase.String(), "side": "BUY", "quantity": "one"}, "quantity"},
		{"conversion asset", book + "/conversions", map[string]any{"from_asset_id": nilUUIDText, "to_asset_id": trBase.String(),
			"quantity": "1"}, "from_asset_id"},
		{"rebalance target", book + "/rebalances", map[string]any{"asset_id": trBase.String(), "target": "1.5"}, "target"},
		{"paper activity", book + "/paper-fills", map[string]any{"activity": "YIELD", "asset_id": trBase.String(), "side": "BUY",
			"quantity": "1"}, "activity"},
		{"crossing instrument", "/v1/advanced/treasury/crossing-rules", map[string]any{"instrument_id": nilUUIDText, "allow": true,
			"disclosure": "This is simulated activity of the house book.", "reason": "r"}, "instrument_id"},
		{"cancel order id", book + "/orders/" + nilUUIDText + "/cancel", nil, "orderId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, ft := newTreasuryHarness(t)
			res := h.do(http.MethodPost, tc.path, tc.body, "Idempotency-Key", "validate-tr-0001")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeValidationFailed, p.Code)
			assert.Equal(t, tc.field, p.Fields["field"])
			ft.mu.Lock()
			defer ft.mu.Unlock()
			assert.Empty(t, ft.calls, "a malformed request never reaches the service")
		})
	}
}

// Without a treasury the routes say so; they never answer an empty success.
func TestTreasuryRoutes_WithoutAPortAreUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	for _, path := range []string{"/v1/advanced/treasury/books", trBook(), trBook() + "/report", trBook() + "/balances",
		trBook() + "/reconciliation", trBook() + "/events", trBook() + "/actions", trBook() + "/paper-fills", trBook() + "/risk-decisions",
		"/v1/advanced/treasury/crossing-disclosures?instrument_id=" + trInst.String(),
		"/v1/advanced/treasury/house-trades?account_id=" + testAccountID.String()} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, path)
	}
	for _, m := range treasuryMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "unwired-tr-"+m.name)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
}
