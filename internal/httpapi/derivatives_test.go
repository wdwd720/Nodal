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
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// fixedID decodes a canonical UUIDv7 into any typed identifier, for the
// derivative ids the package mints only when it persists a row.
func fixedID[T any, PT interface {
	*T
	UnmarshalText([]byte) error
}](s string) T {
	var v T
	if err := PT(&v).UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return v
}

var (
	testDerivInstr    = instruments.NewInstrumentID()
	testDerivPosition = fixedID[derivatives.PositionID]("0193b2e0-0000-7000-8000-0000000000e1")
	testDerivClosed   = fixedID[derivatives.PositionID]("0193b2e0-0000-7000-8000-0000000000e2")
	testDerivMark     = fixedID[derivatives.MarkID]("0193b2e0-0000-7000-8000-0000000000e3")
	testDerivFill     = fixedID[derivatives.FillID]("0193b2e0-0000-7000-8000-0000000000e4")
	testDerivFlow     = fixedID[derivatives.CashFlowID]("0193b2e0-0000-7000-8000-0000000000e5")
	testDerivAccrual  = fixedID[derivatives.AccrualID]("0193b2e0-0000-7000-8000-0000000000e6")
	testDerivRate     = fixedID[derivatives.FundingRateID]("0193b2e0-0000-7000-8000-0000000000e7")
	testDerivSettle   = fixedID[derivatives.SettlementID]("0193b2e0-0000-7000-8000-0000000000e8")
	testDerivExercise = fixedID[derivatives.ExerciseID]("0193b2e0-0000-7000-8000-0000000000e9")
	testDerivJournal  = fixedID[ledger.TransactionID]("0193b2e0-0000-7000-8000-0000000000ea")
	testDerivRun      = "0193b2e0-0000-7000-8000-0000000000eb"
	testDerivHouse    = "0193b2e0-0000-7000-8000-0000000000ec"
	testDerivRecord   = "0193b2e0-0000-7000-8000-0000000000ed"
	testDerivIntent   = econ.NewIntentID()
)

// fakeDerivatives is the derivatives port. It counts every call and records
// what each was scoped to, so a test can prove the handler passed the scoped
// account and parsed values -- and that a replay never reached it.
type fakeDerivatives struct {
	mu    sync.Mutex
	err   error // returned by every method when set
	calls map[string]int

	lastAccount  accounts.AccountID
	lastLimit    int
	lastKind     instruments.InstrumentType
	lastStatus   derivatives.PositionStatus
	lastFilter   DerivativeFlowFilter
	lastAccrual  *derivatives.PositionID
	lastCursor   string
	lastAnalytic derivatives.AnalyticsRequest
	lastOpen     DerivativeOpenCommand
	lastReduce   DerivativeReduceCommand
	lastMark     DerivativeMarkCommand
	lastRun      DerivativeRunCommand
	lastSettle   DerivativeSettleCommand
	lastFund     DerivativeClearingFundCommand
	lastAsset    assets.AssetID

	policy     derivatives.Policy
	contract   DerivativeContractView
	marks      []derivatives.Mark
	analytics  DerivativeAnalyticsView
	page       DerivativePositionPageView
	position   DerivativePositionView
	owner      accounts.AccountID
	flows      []DerivativeFlowView
	accruals   []DerivativeAccrualView
	next       string
	trade      DerivativeTradeView
	tradeErr   error
	mark       derivatives.Mark
	run        DerivativeRunView
	settlement DerivativeSettlementView
	fund       DerivativeClearingFundView
	report     DerivativeClearingReportView
}

func (f *fakeDerivatives) called(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *fakeDerivatives) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeDerivatives) Policy(context.Context) (derivatives.Policy, error) {
	f.called("Policy")
	return f.policy, f.err
}

func (f *fakeDerivatives) Contracts(_ context.Context, kind instruments.InstrumentType, limit int) ([]DerivativeContractView, error) {
	f.called("Contracts")
	f.mu.Lock()
	f.lastKind, f.lastLimit = kind, limit
	f.mu.Unlock()
	return []DerivativeContractView{f.contract}, f.err
}

func (f *fakeDerivatives) Contract(context.Context, instruments.InstrumentID) (DerivativeContractView, error) {
	f.called("Contract")
	return f.contract, f.err
}

func (f *fakeDerivatives) Marks(_ context.Context, _ instruments.InstrumentID, cursor string, limit int) ([]derivatives.Mark, string, error) {
	f.called("Marks")
	f.mu.Lock()
	f.lastCursor, f.lastLimit = cursor, limit
	f.mu.Unlock()
	return f.marks, f.next, f.err
}

func (f *fakeDerivatives) Analytics(_ context.Context, r derivatives.AnalyticsRequest) (DerivativeAnalyticsView, error) {
	f.called("Analytics")
	f.mu.Lock()
	f.lastAnalytic = r
	f.mu.Unlock()
	v := f.analytics
	v.Inputs = r
	return v, f.err
}

func (f *fakeDerivatives) Positions(_ context.Context, account accounts.AccountID, status derivatives.PositionStatus, cursor string, limit int) (DerivativePositionPageView, error) {
	f.called("Positions")
	f.mu.Lock()
	f.lastAccount, f.lastStatus, f.lastCursor, f.lastLimit = account, status, cursor, limit
	f.mu.Unlock()
	return f.page, f.err
}

func (f *fakeDerivatives) Position(context.Context, derivatives.PositionID) (DerivativePositionView, error) {
	f.called("Position")
	return f.position, f.err
}

func (f *fakeDerivatives) PositionAccount(context.Context, derivatives.PositionID) (accounts.AccountID, error) {
	f.called("PositionAccount")
	return f.owner, f.err
}

func (f *fakeDerivatives) Flows(_ context.Context, account accounts.AccountID, filter DerivativeFlowFilter, cursor string, limit int) ([]DerivativeFlowView, string, error) {
	f.called("Flows")
	f.mu.Lock()
	f.lastAccount, f.lastFilter, f.lastCursor, f.lastLimit = account, filter, cursor, limit
	f.mu.Unlock()
	return f.flows, f.next, f.err
}

func (f *fakeDerivatives) FundingAccruals(_ context.Context, account accounts.AccountID, position *derivatives.PositionID, cursor string, limit int) ([]DerivativeAccrualView, string, error) {
	f.called("FundingAccruals")
	f.mu.Lock()
	f.lastAccount, f.lastAccrual, f.lastCursor, f.lastLimit = account, position, cursor, limit
	f.mu.Unlock()
	return f.accruals, f.next, f.err
}

func (f *fakeDerivatives) tradeAnswer() (DerivativeTradeView, error) {
	if f.err != nil {
		return DerivativeTradeView{}, f.err
	}
	return f.trade, f.tradeErr
}

func (f *fakeDerivatives) Open(_ context.Context, c DerivativeOpenCommand) (DerivativeTradeView, error) {
	f.called("Open")
	f.mu.Lock()
	f.lastOpen = c
	f.mu.Unlock()
	return f.tradeAnswer()
}

func (f *fakeDerivatives) Close(_ context.Context, c DerivativeReduceCommand) (DerivativeTradeView, error) {
	f.called("Close")
	f.mu.Lock()
	f.lastReduce = c
	f.mu.Unlock()
	return f.tradeAnswer()
}

func (f *fakeDerivatives) Exercise(_ context.Context, c DerivativeReduceCommand) (DerivativeTradeView, error) {
	f.called("Exercise")
	f.mu.Lock()
	f.lastReduce = c
	f.mu.Unlock()
	return f.tradeAnswer()
}

func (f *fakeDerivatives) RecordMark(_ context.Context, c DerivativeMarkCommand) (derivatives.Mark, error) {
	f.called("RecordMark")
	f.mu.Lock()
	f.lastMark = c
	f.mu.Unlock()
	return f.mark, f.err
}

func (f *fakeDerivatives) RunVariationMargin(_ context.Context, c DerivativeRunCommand) (DerivativeRunView, error) {
	f.called("RunVariationMargin")
	f.mu.Lock()
	f.lastRun = c
	f.mu.Unlock()
	return f.run, f.err
}

func (f *fakeDerivatives) RunFunding(_ context.Context, c DerivativeRunCommand) (DerivativeRunView, error) {
	f.called("RunFunding")
	f.mu.Lock()
	f.lastRun = c
	f.mu.Unlock()
	return f.run, f.err
}

func (f *fakeDerivatives) SettleExpiry(_ context.Context, c DerivativeSettleCommand) (DerivativeSettlementView, error) {
	f.called("SettleExpiry")
	f.mu.Lock()
	f.lastSettle = c
	f.mu.Unlock()
	return f.settlement, f.err
}

func (f *fakeDerivatives) FundClearingHouse(_ context.Context, c DerivativeClearingFundCommand) (DerivativeClearingFundView, error) {
	f.called("FundClearingHouse")
	f.mu.Lock()
	f.lastFund = c
	f.mu.Unlock()
	return f.fund, f.err
}

func (f *fakeDerivatives) ReconcileClearing(_ context.Context, a assets.AssetID) (DerivativeClearingReportView, error) {
	f.called("ReconcileClearing")
	f.mu.Lock()
	f.lastAsset = a
	f.mu.Unlock()
	return f.report, f.err
}

// newFakeDerivatives holds realistic values for a perpetual on a sandbox
// dollar: an open long of testAccountID valued by the service, a closed
// position, flows, an accrual, a completed open, runs and a settlement. Big
// figures exceed int64 so an exactness bug cannot hide.
func newFakeDerivatives() *fakeDerivatives {
	sbx := DerivativeAssetView{ID: testAssetID, Symbol: "SBX-USD", Decimals: 6}
	under := instruments.NewInstrumentID()
	terms := instruments.Terms{
		Kind: instruments.TypePerpetual, Symbol: "SBXBTC-PERP", VenueCode: "NODAL-SANDBOX", SettlementAssetID: testAssetID,
		UnderlyingInstrumentID: &under, ContractMultiplier: money.QuantityFromInt64(1_000_000), PriceScale: 2,
		TickSize: money.QuantityFromInt64(50), LotSize: money.QuantityFromInt64(1), MinQuantity: money.QuantityFromInt64(1),
		SettlementStyle: instruments.SettleCash, TradingCalendar: derivatives.Calendar24x7, JurisdictionTags: []string{"SANDBOX"},
		OracleMaxAgeMS: 60_000, OracleSources: []string{"operator"}, MarginModel: instruments.MarginLinear,
		CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.NonTransferable,
		FundingIntervalMS: 8 * 3600 * 1000,
	}
	ins := instruments.Instrument{
		ID: testDerivInstr, Type: instruments.TypePerpetual, CanonicalName: "SBXBTC-PERP", SettlementAssetID: testAssetID,
		RiskClass: assets.RiskSpeculative, Status: assets.StatusActive, ActiveFrom: testNow,
	}
	mark := derivatives.Mark{
		ID: testDerivMark, InstrumentID: testDerivInstr, MarkMantissa: money.QuantityFromInt64(6_500_050), MarkScale: 2,
		HasIndex: true, IndexMantissa: money.QuantityFromInt64(649_995_000), IndexScale: 4, IndexSource: "operator",
		Source: "operator", ObservedAt: testNow, RecordedAt: testNow,
	}
	big := qty("-123456789012345678901234")
	open := derivatives.Position{
		ID: testDerivPosition, AccountID: testAccountID, InstrumentID: testDerivInstr, SettlementAssetID: testAssetID,
		Status: derivatives.PositionOpen, Quantity: money.QuantityFromInt64(10), CostValue: qty("650065000000"),
		SettledValue: qty("650050000000"), RealizedPnL: qty("0"), FundingPnL: qty("-1500001"), MarginPosted: qty("65006500000"),
		Shortfall: qty("0"), Version: 3, OpenedAt: testNow,
	}
	closedAt := testNow.Add(time.Hour)
	closed := derivatives.Position{
		ID: testDerivClosed, AccountID: testAccountID, InstrumentID: testDerivInstr, SettlementAssetID: testAssetID,
		Status: derivatives.PositionClosed, Quantity: qty("0"), CostValue: qty("0"), SettledValue: qty("0"),
		RealizedPnL: qty("987654321098765432109876"), FundingPnL: qty("0"), MarginPosted: qty("0"), Shortfall: qty("0"),
		Version: 7, OpenedAt: testNow.Add(-time.Hour), ClosedAt: &closedAt,
	}
	valuation := derivatives.PositionRisk{
		PositionID: testDerivPosition.String(), InstrumentID: testDerivInstr.String(), Kind: "PERPETUAL",
		SettlementAssetID: testAssetID.String(), Quantity: money.QuantityFromInt64(10), MarkID: testDerivMark.String(),
		Mark: money.QuantityFromInt64(6_500_050), MarkObservedAt: testNow,
		Requirement: derivatives.Requirement{
			Notional: qty("650050000000"), Initial: qty("71505500000"), Maintenance: qty("32502500000"),
			ScenarioLoss: qty("0"), FundingBuffer: qty("1462612500"),
		},
		AverageEntryPrice: money.QuantityFromInt64(6_500_650), UnrealizedPnL: big, EquityAdjustment: qty("12345"),
	}
	view := func(p derivatives.Position) DerivativePositionView {
		return DerivativePositionView{Position: p, Symbol: "SBXBTC-PERP", Kind: instruments.TypePerpetual, PriceScale: 2, Settlement: sbx}
	}
	openView, closedView := view(open), view(closed)
	openView.Computed, openView.Valuation = true, &valuation
	closedView.Computed = true
	jid := testDerivJournal
	qtyTen := money.QuantityFromInt64(10)
	did := testDecisionID()
	markID := testDerivMark
	tradePos := view(open)
	return &fakeDerivatives{
		calls:    map[string]int{},
		policy:   derivatives.DefaultPolicy(),
		contract: DerivativeContractView{Instrument: ins, Current: instruments.Version{InstrumentID: testDerivInstr, Version: 2, Terms: terms, ContentHash: []byte{0xab, 0xcd}}, Settlement: sbx, LatestMark: &mark},
		marks:    []derivatives.Mark{mark},
		analytics: DerivativeAnalyticsView{
			Analytics: derivatives.Analytics{
				Label: derivatives.AnalyticsLabel, Model: derivatives.AnalyticsModel, InstrumentID: testDerivInstr.String(),
				Right: "CALL", Style: "AMERICAN", Regime: "NORMAL", Spot: "64999.5", Strike: "65000",
				TimeYears: "0.082191780821917808", Vol: "0.8", Price: "5944.123456789012345678", Delta: "0.546",
				Gamma: "0.000021", Theta: "-36210.5", ThetaPerDay: "-99.207", Vega: "7400.1", VegaPerPoint: "74.001",
				Intrinsic: "0", TimeValue: "65000.5", ImpliedVol: "", ImpliedVolReason: "PRICE_ABOVE_UPPER_BOUND", AsOf: testNow,
			},
			Mark: mark,
		},
		page: DerivativePositionPageView{
			Items: []DerivativePositionView{openView, closedView}, Next: "cursor-2", Quote: sbx,
			Book: &derivatives.RiskContribution{
				AccountID: testAccountID.String(), AsOf: testNow, PolicyVersion: "derivatives-sandbox-v1", PolicyHash: "ph",
				GrossNotional: usd("650050.00"), InitialMargin: usd("71505.50"), MaintenanceMargin: usd("32502.50"),
				UnrealizedPnL: usd("-0.15"), EquityAdjustment: usd("0.01"), MaxInitialBPS: 1000,
				Positions: []derivatives.PositionRisk{valuation},
			},
		},
		position: openView,
		owner:    testAccountID,
		flows: []DerivativeFlowView{{
			Flow: derivatives.CashFlow{
				ID: testDerivFlow, PositionID: testDerivPosition, AccountID: testAccountID, InstrumentID: testDerivInstr,
				SettlementAssetID: testAssetID, Kind: derivatives.FlowVariationMargin, Amount: qty("-150000000000000000000001"),
				Shortfall: qty("1"), JournalTransactionID: &jid, ReferenceType: "variation_margin_run", ReferenceID: testDerivRun,
				OccurredAt: testNow,
			},
			Symbol: "SBXBTC-PERP", Settlement: sbx,
		}},
		accruals: []DerivativeAccrualView{{
			ID: testDerivAccrual, FundingRateID: testDerivRate, PositionID: testDerivPosition, InstrumentID: testDerivInstr,
			Symbol: "SBXBTC-PERP", IntervalStart: testNow.Add(-8 * time.Hour), IntervalEnd: testNow, Quantity: money.QuantityFromInt64(10),
			Direction: derivatives.FundingPay, Amount: qty("975075000"), RateE8: 15_000, PremiumIndexE8: 15_000, Clamped: false,
			PerContract: qty("97507500"), MarkMantissa: money.QuantityFromInt64(6_500_050), PriceScale: 2, Settlement: sbx,
		}},
		trade: DerivativeTradeView{
			Outcome: derivatives.Outcome{
				Intent: econ.Intent{
					ID: testDerivIntent, AccountID: testAccountID, Kind: econ.KindOpenPosition, Mode: gates.ModePaper,
					Capability: gates.Derivatives, ActorType: security.ActorUser, ActorID: testUserID.String(),
					InstrumentID: testDerivInstr.String(), Quantity: &qtyTen, Terms: []byte(`{"side":"LONG"}`),
					ContentHash: []byte{0x0c, 0x0d}, Status: econ.StatusCompleted, CreatedAt: testNow, UpdatedAt: testNow,
				},
				DecisionID: &did,
				Fill: &derivatives.FillRecord{
					ID: testDerivFill, PositionID: testDerivPosition, AccountID: testAccountID, InstrumentID: testDerivInstr,
					Source: derivatives.SourcePaper, Side: derivatives.Buy, Quantity: qtyTen, PriceMantissa: money.QuantityFromInt64(6_500_700),
					PriceScale: 2, Value: qty("650070000000"), RealizedPnL: qty("0"), CashSettled: qty("0"), MarkID: &markID,
					SlippageBPS: 10, IntentID: testDerivIntent.String(), RiskDecisionID: did.String(), OccurredAt: testNow,
				},
				Position: &open,
				Journals: []ledger.TransactionID{jid},
			},
			Position: &tradePos,
		},
		mark: mark,
		run: DerivativeRunView{
			InstrumentID: testDerivInstr, Settlement: sbx,
			Result: derivatives.RunResult{
				ID: testDerivRun, IntervalStart: testNow.Add(-time.Hour), IntervalEnd: testNow, MarkID: testDerivMark, Positions: 2,
				Paid: qty("100000000000000000000003"), Received: qty("100000000000000000000002"), Shortfall: qty("1"),
				PremiumIndexE8: -20_000, RateE8: -20_000, PerContract: qty("130001000"), Journals: []ledger.TransactionID{jid},
			},
		},
		settlement: DerivativeSettlementView{
			InstrumentID: testDerivInstr, PriceScale: 2, Settlement: sbx,
			Result: derivatives.SettlementResult{
				ID: testDerivSettle, Kind: derivatives.SettleOption, Price: money.QuantityFromInt64(6_600_000), Positions: 3,
				Paid: qty("4000000"), Received: qty("4000000"), Shortfall: qty("0"), IntrinsicPerContract: qty("1000000"),
				Exercises: []derivatives.ExerciseID{testDerivExercise}, Journals: []ledger.TransactionID{jid},
			},
		},
		fund: DerivativeClearingFundView{Asset: sbx, Quantity: qty("10000000000000"), Journals: []ledger.TransactionID{jid}},
		report: DerivativeClearingReportView{
			Asset: sbx,
			Report: derivatives.ClearingReport{
				ClearingHouseID: testDerivHouse, AssetID: testAssetID.String(), ClearingCash: qty("10000000000005"),
				ClearingCapital: qty("10000000000000"), ClearingResult: qty("5"), RegisterNet: qty("-5"), RegisterShortfall: qty("0"),
				Flows: 4, ExpectedResult: qty("5"), Difference: qty("0"),
				NetOpenInterest: map[string]money.Quantity{
					"0193b2e0-0000-7000-8000-0000000000f2": qty("0"), "0193b2e0-0000-7000-8000-0000000000f1": qty("-3"),
				},
				InternallyMatched: false, Matched: true, RecordID: testDerivRecord,
			},
		},
	}
}

func newDerivativesHarness(t *testing.T) (*harness, *fakeDerivatives) {
	t.Helper()
	h := newHarness(t)
	fd := newFakeDerivatives()
	ports := h.ports.ports()
	ports.Derivatives = fd
	h.server.opts.Ports = ports
	return h, fd
}

// staleOperator is an operator whose strong authentication is too old for
// the operations that demand a recent one.
func staleOperator() security.Principal {
	p := operatorPrincipal()
	p.AuthTime = testNow.Add(-2 * time.Hour)
	return p
}

func strangerPrincipal() security.Principal {
	p := customerPrincipal()
	p.SubjectID = accounts.NewUserID().String()
	p.AccountIDs = []string{testOtherAcct.String()}
	return p
}

const derivBase = "/v1/advanced/derivatives"

func openBody() map[string]any {
	return map[string]any{
		"account_id": testAccountID.String(), "instrument_id": testDerivInstr.String(), "side": "LONG", "quantity": "10", "mode": "PAPER",
	}
}

// derivativeMutation is one command route with a valid body and a variation
// of it, and the port method it reaches.
type derivativeMutation struct {
	name     string
	path     string
	body     map[string]any
	changed  map[string]any
	method   string
	operator bool
	status   int
}

func derivativeMutations() []derivativeMutation {
	instr := derivBase + "/contracts/" + testDerivInstr.String()
	pos := derivBase + "/positions/" + testDerivPosition.String()
	return []derivativeMutation{
		{name: "open", path: derivBase + "/positions", body: openBody(), method: "Open", status: http.StatusCreated,
			changed: func() map[string]any { b := openBody(); b["quantity"] = "11"; return b }()},
		{name: "close", path: pos + "/close", body: map[string]any{"mode": "PAPER"}, method: "Close", status: http.StatusOK,
			changed: map[string]any{"mode": "PAPER", "quantity": "3"}},
		{name: "exercise", path: pos + "/exercise", body: map[string]any{"mode": "PAPER"}, method: "Exercise", status: http.StatusOK,
			changed: map[string]any{"mode": "SANDBOX"}},
		{name: "mark", path: instr + "/marks", body: map[string]any{"mark": "65000.50", "source": "operator"}, method: "RecordMark",
			operator: true, status: http.StatusOK, changed: map[string]any{"mark": "65000.00", "source": "operator"}},
		{name: "variation margin", path: instr + "/variation-margin-runs", body: map[string]any{"interval_start": "2026-09-06T11:00:00Z"},
			method: "RunVariationMargin", operator: true, status: http.StatusOK,
			changed: map[string]any{"interval_start": "2026-09-06T10:00:00Z"}},
		{name: "funding", path: instr + "/funding-runs", body: map[string]any{"interval_start": "2026-09-06T00:00:00Z"},
			method: "RunFunding", operator: true, status: http.StatusOK, changed: map[string]any{"interval_start": "2026-09-05T16:00:00Z"}},
		{name: "settlement", path: instr + "/settlement", body: map[string]any{}, method: "SettleExpiry", operator: true,
			status: http.StatusOK, changed: map[string]any{"price": "66000", "price_source": "operator"}},
		{name: "clearing funding", path: derivBase + "/clearing-house/funding", body: map[string]any{"quantity": "10000000000000"},
			method: "FundClearingHouse", operator: true, status: http.StatusOK, changed: map[string]any{"quantity": "1"}},
		{name: "reconciliation", path: derivBase + "/clearing-house/reconciliations", body: map[string]any{},
			method: "ReconcileClearing", operator: true, status: http.StatusOK,
			changed: map[string]any{"asset_id": testAssetID.String()}},
	}
}

// A deployment without the derivatives service says so on every route.
func TestDerivatives_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	instr := derivBase + "/contracts/" + testDerivInstr.String()
	reads := []string{
		derivBase + "/policy",
		derivBase + "/contracts",
		instr,
		instr + "/marks",
		instr + "/analytics?vol_bps=8000",
		derivBase + "/positions?account_id=" + testAccountID.String(),
		derivBase + "/positions/" + testDerivPosition.String(),
		derivBase + "/flows?account_id=" + testAccountID.String(),
		derivBase + "/funding-accruals?account_id=" + testAccountID.String(),
	}
	for _, path := range reads {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, path)
	}
	for _, m := range derivativeMutations() {
		if m.operator {
			op := operatorPrincipal()
			h.as(&op)
		} else {
			c := customerPrincipal()
			h.as(&c)
		}
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "unwired-key-"+strings.ReplaceAll(m.name, " ", "-"))
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
}

func TestDerivatives_AccountReadsAreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, path := range []string{derivBase + "/positions", derivBase + "/flows", derivBase + "/funding-accruals"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h, fd := newDerivativesHarness(t)
			res := h.do(http.MethodGet, path+"?account_id="+testAccountID.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, fd.lastAccount)
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
			assert.Equal(t, testOtherAcct, fd.lastAccount)
		})
	}
}

// A position fetched by its own id is scoped by the account stored on it: a
// stranger gets exactly the answer an absent id gets.
func TestDerivatives_PositionByIDIsNotFoundForAStranger(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	path := derivBase + "/positions/" + testDerivPosition.String()
	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "VALUED", res.raw()["valuation_status"])

	stranger := strangerPrincipal()
	h.as(&stranger)
	foreign := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
	fp := foreign.problem()

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads it; body=%s", res.Body.String())

	// The absent answer, as the service words it, is normalised to the same one.
	fd.err = errs.New(errs.CodeNotFound, "derivative position not found").WithField("position_id", testDerivPosition.String())
	h.as(&stranger)
	absent := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, absent.Code)
	ap := absent.problem()
	assert.Equal(t, ap.Detail, fp.Detail, "foreign and absent must read the same")
	assert.Equal(t, ap.Fields, fp.Fields, "foreign and absent must read the same")
}

// Closing and exercising act on a position only for its owner. Anyone else
// -- an operator included, as on every write -- is told it does not exist,
// and the command never reaches the service.
func TestDerivatives_CloseAndExerciseAreOwnerOnlyAndMasked(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"close", "exercise"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			h, fd := newDerivativesHarness(t)
			path := derivBase + "/positions/" + testDerivPosition.String() + "/" + verb
			method := map[string]string{"close": "Close", "exercise": "Exercise"}[verb]

			stranger := strangerPrincipal()
			h.as(&stranger)
			foreign := h.do(http.MethodPost, path, map[string]any{"mode": "PAPER"}, "Idempotency-Key", verb+"-foreign-01")
			require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())

			op := operatorPrincipal()
			h.as(&op)
			res := h.do(http.MethodPost, path, map[string]any{"mode": "PAPER"}, "Idempotency-Key", verb+"-operator-01")
			require.Equal(t, http.StatusNotFound, res.Code, "an operator may read it but not act on it; body=%s", res.Body.String())
			assert.Zero(t, fd.count(method), "nobody but the owner reached the service")

			fd.err = errs.New(errs.CodeNotFound, "derivative position not found")
			absent := h.do(http.MethodPost, path, map[string]any{"mode": "PAPER"}, "Idempotency-Key", verb+"-absent-01")
			require.Equal(t, http.StatusNotFound, absent.Code)
			assert.Equal(t, absent.problem().Fields, foreign.problem().Fields)
			assert.Equal(t, absent.problem().Detail, foreign.problem().Detail)
			fd.err = nil

			owner := customerPrincipal()
			h.as(&owner)
			res = h.do(http.MethodPost, path, map[string]any{"mode": "SANDBOX", "quantity": "4"}, "Idempotency-Key", verb+"-owner-0001")
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, 1, fd.count(method))
			assert.Equal(t, testDerivPosition, fd.lastReduce.PositionID)
			assert.Equal(t, testAccountID, fd.lastReduce.AccountID, "the command is scoped to the position's own account")
			require.NotNil(t, fd.lastReduce.Quantity)
			assert.Equal(t, "4", fd.lastReduce.Quantity.String())
			assert.Equal(t, gates.ModeSandbox, fd.lastReduce.Mode)
			assert.Equal(t, verb+"-owner-0001", fd.lastReduce.IdempotencyKey)
		})
	}
}

func TestDerivatives_PositionsRenderTheServicesValuationExactly(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	res := h.do(http.MethodGet, derivBase+"/positions?account_id="+testAccountID.String()+"&status=OPEN&limit=5000&cursor=c-1", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, derivatives.PositionOpen, fd.lastStatus)
	assert.Equal(t, maxPageLimit, fd.lastLimit, "the limit is clamped")
	assert.Equal(t, "c-1", fd.lastCursor)

	var page api.DerivativePositionPage
	res.json(&page)
	assert.Equal(t, api.SIMULATED, page.ValueDomain)
	next, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "cursor-2", next)

	require.Equal(t, api.DerivativeBookValued, page.BookStatus)
	require.NotNil(t, page.Book)
	assert.Nil(t, page.BookRefusal)
	assert.Equal(t, "650050.00", page.Book.GrossNotional)
	assert.Equal(t, "71505.50", page.Book.InitialMargin)
	assert.Equal(t, "-0.15", page.Book.UnrealizedPnl)
	assert.Equal(t, "SBX-USD", page.Book.QuoteAsset.Symbol, "the book names the asset it is valued in")
	assert.Equal(t, 1, page.Book.Positions)

	require.Len(t, page.Items, 2)
	open := page.Items[0]
	assert.Equal(t, api.DerivativeValuationValued, open.ValuationStatus)
	require.NotNil(t, open.Side)
	assert.Equal(t, api.DerivativeSideLong, *open.Side)
	assert.Equal(t, "10", open.Quantity)
	assert.Equal(t, "-1500001", open.FundingPnl)
	assert.Equal(t, "65006500000", open.MarginPosted)
	assert.Equal(t, 6, open.SettlementAsset.Decimals)
	require.NotNil(t, open.Valuation)
	assert.Equal(t, "-123456789012345678901234", open.Valuation.UnrealizedPnl, "beyond int64, exactly")
	assert.Equal(t, "6500650", open.Valuation.AverageEntryPriceMantissa)
	assert.Equal(t, "71505500000", open.Valuation.Requirement.Initial)
	assert.Equal(t, "32502500000", open.Valuation.Requirement.Maintenance)
	assert.Equal(t, "1462612500", open.Valuation.Requirement.FundingBuffer)
	assert.Equal(t, testDerivMark.String(), open.Valuation.MarkId.String())

	closed := page.Items[1]
	assert.Equal(t, api.DerivativeValuationNotApplicable, closed.ValuationStatus)
	assert.Nil(t, closed.Side, "a flat position has no side")
	assert.Nil(t, closed.Valuation)
	assert.Equal(t, "987654321098765432109876", closed.RealizedPnl)
	require.NotNil(t, closed.ClosedAt)

	// Figures are strings on the wire, never JSON numbers.
	item := res.raw()["items"].([]any)[0].(map[string]any)
	assert.IsType(t, "", item["quantity"])
	assert.IsType(t, "", item["valuation"].(map[string]any)["unrealized_pnl"])
}

// When the service refuses to value the book it says why, and no figure
// stands in for the one it could not compute.
func TestDerivatives_ARefusedBookIsShownNotInvented(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	refusal := errs.New(errs.CodeStaleMarketData, "derivatives: a position's mark is stale; its risk is unknown")
	fd.page.Book, fd.page.BookRefusal = nil, refusal
	fd.page.Items[0].Valuation, fd.page.Items[0].Refusal = nil, refusal

	res := h.do(http.MethodGet, derivBase+"/positions?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.DerivativePositionPage
	res.json(&page)
	require.Equal(t, api.DerivativeBookRefused, page.BookStatus)
	assert.Nil(t, page.Book)
	require.NotNil(t, page.BookRefusal)
	assert.Equal(t, "STALE_MARKET_DATA", page.BookRefusal.Code)
	assert.Equal(t, api.DerivativeValuationRefused, page.Items[0].ValuationStatus)
	assert.Nil(t, page.Items[0].Valuation)
	require.NotNil(t, page.Items[0].ValuationRefusal)
	assert.Equal(t, "STALE_MARKET_DATA", page.Items[0].ValuationRefusal.Code)
	assert.Equal(t, api.DerivativeValuationNotApplicable, page.Items[1].ValuationStatus)
	assert.NotContains(t, res.raw(), "book")
}

// An open position the answer says it valued, without a valuation or a
// refusal, is a fault, not a blank.
func TestDerivatives_AnOpenPositionNeitherValuedNorRefusedIsAFault(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	fd.position.Valuation = nil
	res := h.do(http.MethodGet, derivBase+"/positions/"+testDerivPosition.String(), nil)
	require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())

	fd.page.Book, fd.page.BookRefusal = nil, nil
	res = h.do(http.MethodGet, derivBase+"/positions?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())
}

func TestDerivatives_ReadsRefuseMalformedParameters(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	for _, path := range []string{
		derivBase + "/contracts?kind=SPOT_PAIR",
		derivBase + "/positions?account_id=" + testAccountID.String() + "&status=PENDING",
		derivBase + "/flows?account_id=" + testAccountID.String() + "&kind=DIVIDEND",
		derivBase + "/flows?account_id=" + testAccountID.String() + "&position_id=not-a-uuid",
		derivBase + "/contracts/" + testDerivInstr.String() + "/analytics",
		derivBase + "/positions/not-a-uuid",
	} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusBadRequest, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code, path)
	}
	assert.Zero(t, fd.count("Contracts")+fd.count("Positions")+fd.count("Flows")+fd.count("Analytics")+fd.count("Position"))
}

func TestDerivatives_ContractsCarryTheirTermsMarkAndSettlementExactly(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	res := h.do(http.MethodGet, derivBase+"/contracts?kind=PERPETUAL&limit=7", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, instruments.TypePerpetual, fd.lastKind)
	assert.Equal(t, 7, fd.lastLimit)
	var list api.DerivativeContractList
	res.json(&list)
	assert.Equal(t, api.SIMULATED, list.ValueDomain)
	require.Len(t, list.Items, 1)
	c := list.Items[0]
	assert.Equal(t, api.DerivativeKindPerpetual, c.Kind)
	assert.Equal(t, "abcd", c.TermsHash)
	assert.Equal(t, 2, c.TermsVersion)
	require.NotNil(t, c.Terms.FundingIntervalMs)
	assert.Equal(t, int64(8*3600*1000), *c.Terms.FundingIntervalMs)
	assert.Equal(t, "50", c.Terms.TickSize)
	assert.True(t, c.Marked)
	require.NotNil(t, c.LatestMark)
	assert.Equal(t, "6500050", c.LatestMark.MarkMantissa)
	assert.Equal(t, 2, c.LatestMark.PriceScale)
	require.NotNil(t, c.LatestMark.IndexMantissa)
	assert.Equal(t, "649995000", *c.LatestMark.IndexMantissa)
	assert.Equal(t, 4, *c.LatestMark.IndexScale)
	assert.False(t, c.Settled)
	assert.Nil(t, c.Settlement)
	assert.Equal(t, "SBX-USD", c.SettlementAsset.Symbol)

	// A never-marked, settled contract says both, explicitly.
	fd.contract.LatestMark = nil
	fd.contract.Settled = &DerivativeSettlementRecord{
		ID: testDerivSettle, Kind: derivatives.SettleFuture, PriceMantissa: qty("6600000"), PriceScale: 2,
		PriceSource: "mark:x", Positions: 2, SettledAt: testNow,
	}
	res = h.do(http.MethodGet, derivBase+"/contracts/"+testDerivInstr.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.DerivativeContract
	res.json(&detail)
	assert.False(t, detail.Marked)
	assert.Nil(t, detail.LatestMark)
	assert.True(t, detail.Settled)
	require.NotNil(t, detail.Settlement)
	assert.Equal(t, "6600000", detail.Settlement.PriceMantissa)
	assert.Equal(t, api.DerivativeSettlementFuture, detail.Settlement.Kind)

	res = h.do(http.MethodGet, derivBase+"/contracts/"+testDerivInstr.String()+"/marks?cursor=m-1&limit=3", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "m-1", fd.lastCursor)
	assert.Equal(t, 3, fd.lastLimit)
	var marks api.DerivativeMarkPage
	res.json(&marks)
	require.Len(t, marks.Items, 1)
	assert.Equal(t, "operator", marks.Items[0].Source)
	assert.True(t, marks.NextCursor.IsNull(), "no next page is said with null")
}

// The analytics carry the service's own label and the inputs the model used;
// every value is an exact decimal string.
func TestDerivatives_AnalyticsAreLabelledReferenceValuesWithTheirInputs(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	res := h.do(http.MethodGet, derivBase+"/contracts/"+testDerivInstr.String()+"/analytics?vol_bps=8000&dividend_yield_bps=-25", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, money.BPS(8000), fd.lastAnalytic.VolBPS)
	assert.Equal(t, money.BPS(0), fd.lastAnalytic.RateBPS, "an absent rate is zero")
	assert.Equal(t, money.BPS(-25), fd.lastAnalytic.DividendYieldBPS)
	var a api.DerivativeOptionAnalytics
	res.json(&a)
	assert.Equal(t, api.SIMULATED, a.ValueDomain)
	assert.Equal(t, derivatives.AnalyticsLabel, a.Label)
	assert.Contains(t, a.Label, "not a quote")
	assert.Equal(t, derivatives.AnalyticsModel, a.Model)
	assert.Equal(t, "5944.123456789012345678", a.Price)
	assert.Equal(t, "-99.207", a.ThetaPerDay)
	assert.Equal(t, int64(8000), a.Inputs.VolBps)
	assert.Equal(t, int64(0), a.Inputs.RateBps)
	assert.Equal(t, int64(-25), a.Inputs.DividendYieldBps)
	assert.Equal(t, "6500050", a.Mark.MarkMantissa, "the mark the model used")
	assert.Nil(t, a.ImpliedVol, "no implied volatility is invented when none was solved")
	require.NotNil(t, a.ImpliedVolReason)
	assert.Equal(t, "PRICE_ABOVE_UPPER_BOUND", *a.ImpliedVolReason)
}

func TestDerivatives_OpenIsOwnedValidatedAndExact(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	foreign := openBody()
	foreign["account_id"] = testOtherAcct.String()
	res := h.do(http.MethodPost, derivBase+"/positions", foreign, "Idempotency-Key", "open-foreign-001")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodPost, derivBase+"/positions", openBody(), "Idempotency-Key", "open-operator-01")
	require.Equal(t, http.StatusForbidden, res.Code, "an operator does not trade a customer's account; body=%s", res.Body.String())

	c := customerPrincipal()
	h.as(&c)
	for name, mutate := range map[string]func(map[string]any){
		"side":       func(b map[string]any) { b["side"] = "BUY" },
		"quantity":   func(b map[string]any) { b["quantity"] = "1.5" },
		"mode":       func(b map[string]any) { b["mode"] = "REAL" },
		"instrument": func(b map[string]any) { b["instrument_id"] = "00000000-0000-0000-0000-000000000000" },
	} {
		b := openBody()
		mutate(b)
		res := h.do(http.MethodPost, derivBase+"/positions", b, "Idempotency-Key", "open-bad-"+name)
		require.Equal(t, http.StatusBadRequest, res.Code, "%s body=%s", name, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code, name)
	}
	assert.Zero(t, fd.count("Open"), "nothing malformed or foreign reached the service")

	res = h.do(http.MethodPost, derivBase+"/positions", openBody(), "Idempotency-Key", "open-good-00001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, fd.lastOpen.AccountID)
	assert.Equal(t, testDerivInstr, fd.lastOpen.InstrumentID)
	assert.Equal(t, derivatives.Long, fd.lastOpen.Side)
	assert.Equal(t, "10", fd.lastOpen.Quantity.String())
	assert.Equal(t, gates.ModePaper, fd.lastOpen.Mode)
	assert.Equal(t, "open-good-00001", fd.lastOpen.IdempotencyKey, "the service is idempotent on the client's own key")

	var out api.DerivativeTradeResult
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.False(t, out.Replay)
	assert.Equal(t, "COMPLETED", out.Intent.Status)
	require.NotNil(t, out.Fill)
	assert.Equal(t, "6500700", out.Fill.PriceMantissa)
	assert.Equal(t, int64(10), out.Fill.SlippageBps)
	assert.Equal(t, "0", out.Fill.AmountSettled)
	require.NotNil(t, out.Position)
	assert.Equal(t, api.DerivativeValuationNotComputed, out.Position.ValuationStatus, "a trade result does not value the position")
	require.NotNil(t, out.RiskDecisionId)
	assert.Equal(t, testDecisionID().String(), out.RiskDecisionId.String())
	require.Len(t, out.JournalTransactionIds, 1)
}

// Every command is replayed under its Idempotency-Key, never re-executed,
// and a key reused for a different request is refused.
func TestDerivatives_EveryMutationIsReplayedNotReExecuted(t *testing.T) {
	t.Parallel()
	for _, m := range derivativeMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fd := newDerivativesHarness(t)
			if m.operator {
				op := operatorPrincipal()
				h.as(&op)
			}
			key := "replay-" + strings.ReplaceAll(m.name, " ", "-") + "-01"
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, m.status, first.Code, "body=%s", first.Body.String())
			assert.Equal(t, "SIMULATED", first.raw()["value_domain"])
			require.Equal(t, 1, fd.count(m.method))

			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Contains(t, []int{http.StatusOK, m.status}, second.Code, "body=%s", second.Body.String())
			assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers what the first call answered")
			assert.Equal(t, 1, fd.count(m.method), "a replay never reaches the service")

			changed := h.do(http.MethodPost, m.path, m.changed, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, changed.Code, "body=%s", changed.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, changed.problem().Code)
			assert.Equal(t, 1, fd.count(m.method))
		})
	}
}

// A refusal the service records reaches the client with its stable code and
// the fields that let it find the rejected intent, and is itself replayed.
func TestDerivatives_ARecordedRefusalKeepsItsCodeAndIsReplayed(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	fd.tradeErr = errs.New(errs.CodeRiskRejected, "derivatives: the risk kernel refused this order").
		WithField("intent_id", testDerivIntent.String()).WithField("reasons", []string{"RISK_ORACLE_STALE"}).
		WithField("risk_decision_id", testDecisionID().String())
	for i := 0; i < 2; i++ {
		res := h.do(http.MethodPost, derivBase+"/positions", openBody(), "Idempotency-Key", "open-refused-001")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeRiskRejected, p.Code)
		assert.Equal(t, testDerivIntent.String(), p.Fields["intent_id"])
		assert.Equal(t, []any{"RISK_ORACLE_STALE"}, p.Fields["reasons"])
		assert.Equal(t, testDecisionID().String(), p.Fields["risk_decision_id"])
	}
	assert.Equal(t, 1, fd.count("Open"), "the refusal is a conclusion: it is replayed, not re-run")
}

// The service's own replay of an intent is an answer, not a creation.
func TestDerivatives_AServiceReplayIsNotACreation(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	fd.trade.Outcome.Replay = true
	res := h.do(http.MethodPost, derivBase+"/positions", openBody(), "Idempotency-Key", "open-svc-replay1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["replay"])
}

// The operator side needs sandbox:operate, and a recent strong
// authentication wherever value moves (runs, settlement, capitalizing the
// clearing house) -- not to record a mark or reconcile.
func TestDerivatives_OperatorActionsNeedOperateAndStepUpWhereValueMoves(t *testing.T) {
	t.Parallel()
	stepUp := map[string]bool{"variation margin": true, "funding": true, "settlement": true, "clearing funding": true}
	for _, m := range derivativeMutations() {
		if !m.operator {
			continue
		}
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fd := newDerivativesHarness(t)
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "op-customer-0001")
			require.Equal(t, http.StatusForbidden, res.Code, "a customer is not an operator; body=%s", res.Body.String())

			stale := staleOperator()
			h.as(&stale)
			res = h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "op-stale-000001")
			if stepUp[m.name] {
				require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
				assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
				assert.Zero(t, fd.count(m.method))
			} else {
				require.Equal(t, m.status, res.Code, "no value moves; body=%s", res.Body.String())
			}
		})
	}
}

func TestDerivatives_PricesAreReadExactlyAsWritten(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	path := derivBase + "/contracts/" + testDerivInstr.String() + "/marks"
	res := h.do(http.MethodPost, path, map[string]any{
		"mark": "65000.50", "index": "64999.5000", "source": "operator", "observed_at": "2026-09-06T11:59:00Z",
	}, "Idempotency-Key", "mark-exact-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "6500050", fd.lastMark.Mark.Mantissa.String())
	assert.Equal(t, int32(2), fd.lastMark.Mark.Scale)
	require.NotNil(t, fd.lastMark.Index)
	assert.Equal(t, "649995000", fd.lastMark.Index.Mantissa.String(), "trailing zeros are kept: the scale is what was written")
	assert.Equal(t, int32(4), fd.lastMark.Index.Scale)
	assert.Equal(t, "operator", fd.lastMark.Source)
	assert.Equal(t, time.Date(2026, 9, 6, 11, 59, 0, 0, time.UTC), fd.lastMark.ObservedAt)
	assert.Equal(t, "6500050", res.raw()["mark_mantissa"])

	for i, bad := range []string{"1e5", "-1", "1.", ".5", "65,000", "", "1.0000000000000000000000000000000000000001"} {
		res := h.do(http.MethodPost, path, map[string]any{"mark": bad, "source": "operator"}, "Idempotency-Key", "mark-bad-0000"+string(rune('a'+i)))
		require.Equal(t, http.StatusBadRequest, res.Code, "%q body=%s", bad, res.Body.String())
	}
	assert.Equal(t, 1, fd.count("RecordMark"))

	res = h.do(http.MethodPost, derivBase+"/contracts/"+testDerivInstr.String()+"/settlement",
		map[string]any{"price": "66000", "price_source": "operator attestation"}, "Idempotency-Key", "settle-exact-001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.NotNil(t, fd.lastSettle.Price)
	assert.Equal(t, "66000", fd.lastSettle.Price.Mantissa.String())
	assert.Equal(t, int32(0), fd.lastSettle.Price.Scale)
	assert.Equal(t, "operator attestation", fd.lastSettle.PriceSource)
}

// A run reports the totals of the work it did, and nothing for a replay,
// which wrote nothing.
func TestDerivatives_RunsReportTotalsOnlyForTheRunThatDidTheWork(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	instr := derivBase + "/contracts/" + testDerivInstr.String()
	res := h.do(http.MethodPost, instr+"/variation-margin-runs", map[string]any{"interval_start": "2026-09-06T11:00:00Z"},
		"Idempotency-Key", "vm-run-000000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC), fd.lastRun.IntervalStart)
	var vm api.DerivativeRunResult
	res.json(&vm)
	assert.Equal(t, api.DerivativeRunVariationMargin, vm.Kind)
	require.NotNil(t, vm.Paid)
	assert.Equal(t, "100000000000000000000003", *vm.Paid)
	assert.Equal(t, "100000000000000000000002", *vm.Received)
	assert.Equal(t, "1", *vm.Shortfall)
	require.NotNil(t, vm.Positions)
	assert.Equal(t, 2, *vm.Positions)
	assert.Nil(t, vm.RateE8, "variation margin has no rate")

	fd.run.Result = derivatives.RunResult{ID: testDerivRun, Replay: true, IntervalStart: testNow.Add(-time.Hour), IntervalEnd: testNow, Positions: 2}
	res = h.do(http.MethodPost, instr+"/variation-margin-runs", map[string]any{"interval_start": "2026-09-06T11:00:00Z"},
		"Idempotency-Key", "vm-run-000000002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, true, raw["replay"])
	assert.NotContains(t, raw, "paid")
	assert.NotContains(t, raw, "received")
	assert.NotContains(t, raw, "shortfall")

	fd.run.Result = derivatives.RunResult{ID: testDerivRun, Replay: true, IntervalStart: testNow.Add(-8 * time.Hour), IntervalEnd: testNow,
		PremiumIndexE8: 900_000, RateE8: 750_000, Clamped: true, PerContract: qty("48750375")}
	res = h.do(http.MethodPost, instr+"/funding-runs", map[string]any{"interval_start": "2026-09-06T04:00:00Z"},
		"Idempotency-Key", "funding-run-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var fr api.DerivativeRunResult
	res.json(&fr)
	assert.Equal(t, api.DerivativeRunFunding, fr.Kind)
	require.NotNil(t, fr.RateE8)
	assert.Equal(t, int64(750_000), *fr.RateE8)
	assert.True(t, *fr.Clamped)
	assert.Equal(t, "48750375", *fr.PerContractAmount)
	assert.Nil(t, fr.Positions, "a funding replay does not say how many positions it covered")
	assert.Nil(t, fr.Paid)
}

func TestDerivatives_FlowsAccrualsSettlementAndTheClearingHouseAreExact(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	res := h.do(http.MethodGet, derivBase+"/flows?account_id="+testAccountID.String()+"&kind=FUNDING&position_id="+testDerivPosition.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, derivatives.FlowFunding, fd.lastFilter.Kind)
	require.NotNil(t, fd.lastFilter.PositionID)
	assert.Equal(t, testDerivPosition, *fd.lastFilter.PositionID)
	var flows api.DerivativeFlowPage
	res.json(&flows)
	require.Len(t, flows.Items, 1)
	assert.Equal(t, "-150000000000000000000001", flows.Items[0].Amount)
	assert.Equal(t, "1", flows.Items[0].Shortfall)
	assert.Equal(t, api.DerivativeFlowVariationMargin, flows.Items[0].Kind)
	require.NotNil(t, flows.Items[0].JournalTransactionId)

	res = h.do(http.MethodGet, derivBase+"/funding-accruals?account_id="+testAccountID.String()+"&position_id="+testDerivPosition.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.NotNil(t, fd.lastAccrual)
	var accruals api.DerivativeFundingAccrualPage
	res.json(&accruals)
	require.Len(t, accruals.Items, 1)
	a := accruals.Items[0]
	assert.Equal(t, api.DerivativeFundingPay, a.Direction)
	assert.Equal(t, "975075000", a.Amount)
	assert.Equal(t, int64(15_000), a.RateE8)
	assert.Equal(t, "97507500", a.PerContractAmount)
	assert.Nil(t, a.FlowId)

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodPost, derivBase+"/contracts/"+testDerivInstr.String()+"/settlement", map[string]any{}, "Idempotency-Key", "settle-shape-001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Nil(t, fd.lastSettle.Price, "without a price the service derives it")
	var st api.DerivativeSettlementResult
	res.json(&st)
	assert.Equal(t, api.DerivativeSettlementOption, st.Kind)
	require.NotNil(t, st.IntrinsicPerContract)
	assert.Equal(t, "1000000", *st.IntrinsicPerContract)
	require.Len(t, st.ExerciseIds, 1)

	res = h.do(http.MethodPost, derivBase+"/clearing-house/funding", map[string]any{"quantity": "10000000000000"}, "Idempotency-Key", "fund-shape-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.True(t, fd.lastFund.AssetID.IsZero(), "no asset named: the adapter funds the sandbox dollar")
	assert.Equal(t, "fund-shape-0001", fd.lastFund.IdempotencyKey)
	assert.Equal(t, "10000000000000", res.raw()["quantity"])

	res = h.do(http.MethodPost, derivBase+"/clearing-house/reconciliations", map[string]any{"asset_id": testAssetID.String()},
		"Idempotency-Key", "reconcile-shape1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAssetID, fd.lastAsset)
	var rep api.DerivativeClearingReport
	res.json(&rep)
	assert.Equal(t, "10000000000005", rep.Holdings)
	assert.Equal(t, "5", rep.Result)
	assert.Equal(t, "-5", rep.RegisterNet)
	assert.True(t, rep.Matched)
	require.Len(t, rep.NetOpenInterest, 2)
	assert.Equal(t, "0193b2e0-0000-7000-8000-0000000000f1", rep.NetOpenInterest[0].InstrumentId.String(), "sorted by instrument")
	assert.Equal(t, "-3", rep.NetOpenInterest[0].Quantity)
	require.NotNil(t, rep.RecordId)
}

func TestDerivatives_PolicyIsTheServicesOwn(t *testing.T) {
	t.Parallel()
	h, _ := newDerivativesHarness(t)
	res := h.do(http.MethodGet, derivBase+"/policy", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var p api.DerivativePolicy
	res.json(&p)
	want := derivatives.DefaultPolicy()
	assert.Equal(t, want.Version, p.Version)
	assert.Equal(t, want.Hash(), p.Hash)
	assert.Equal(t, int64(want.PaperSlippageBPS), p.PaperSlippageBps)
	assert.Equal(t, derivatives.FundingRateScale, p.FundingRateScale)
	require.Len(t, p.Linear, len(want.Linear))
	assert.Contains(t, p.OptionShocksBps, int64(-3000), "negative shocks are carried as they are")
}

// Port failures carry their code to the wire unchanged; business refusals
// are never 500.
func TestDerivatives_PortErrorsKeepTheirCode(t *testing.T) {
	t.Parallel()
	h, fd := newDerivativesHarness(t)
	fd.err = errs.New(errs.CodeStaleMarketData, "derivatives: analytics need a mark with an index")
	res := h.do(http.MethodGet, derivBase+"/contracts/"+testDerivInstr.String()+"/analytics?vol_bps=8000", nil)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeStaleMarketData, res.problem().Code)

	fd.err = errs.New(errs.CodeNotFound, "no derivative contract has this id")
	res = h.do(http.MethodGet, derivBase+"/contracts/"+testDerivInstr.String(), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
}

// The settlement compiler judges every derivative intent against the legal
// policy the deployment loads: cmd/api's legalRouterFor yields the
// conservative policy by default and the development policy only outside
// production-like environments. Both must route all three derivative intent
// kinds as SIMULATION -- for an account with no verification and no
// jurisdiction determination, which is every account here -- or no
// derivative could ever be traded. This proves it without loosening either.
func TestDerivatives_TheDeploymentLegalPoliciesRouteEveryDerivativeIntent(t *testing.T) {
	t.Parallel()
	for _, pol := range []legalrouter.Policy{legalrouter.ConservativePolicy(), legalrouter.DevelopmentPolicy()} {
		router, err := legalrouter.New(pol)
		require.NoError(t, err, pol.Version)
		for _, kind := range []econ.IntentKind{econ.KindOpenPosition, econ.KindClosePosition, econ.KindExerciseOption} {
			q := money.QuantityFromInt64(1)
			in := econ.Intent{
				ID: econ.NewIntentID(), AccountID: testAccountID, Kind: kind, Mode: gates.ModePaper, Capability: gates.Derivatives,
				ActorType: security.ActorUser, ActorID: testUserID.String(), InstrumentID: testDerivInstr.String(),
				AssetID: &testAssetID, Quantity: &q, IdempotencyKey: "route-" + string(kind), CorrelationID: "corr-route",
				Status: econ.StatusReceived, CreatedAt: testNow, UpdatedAt: testNow,
			}
			route, err := econ.Route(econ.RouteRequest{
				Intent:       in,
				Subject:      settlement.Subject{Type: settlement.SubjectInstrument, ID: testDerivInstr.String(), AssetID: testAssetID.String()},
				Verification: valuedomain.VerificationNone,
			}, router, testNow)
			require.NoError(t, err, "%s under %s", kind, pol.Version)
			assert.True(t, route.MayExecuteNow(), "%s under %s: %v", kind, pol.Version, route.Reasons)
			assert.Equal(t, legalrouter.ProductSimulation, route.Product, "%s under %s", kind, pol.Version)
		}
	}
}
