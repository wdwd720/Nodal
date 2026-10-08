package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

var (
	testMarginUSD   = assets.NewAssetID()
	testMarginSOL   = assets.NewAssetID()
	testMarginAcct  = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac01")
	testMarginLot   = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac02")
	testMarginCase  = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac03")
	testMarginRun   = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac04")
	testMarginCall  = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac05")
	testMarginStep  = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac06")
	testMarginMove  = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac07")
	testMarginJrnl  = ledger.NewTransactionID()
	testMarginJrnl2 = ledger.NewTransactionID()
	testMarginJrnl3 = ledger.NewTransactionID()
)

// parseInto decodes a fixed canonical id into a typed one; the margin package
// mints its ids only when it persists a row.
func parseInto[T interface{ UnmarshalText([]byte) error }](t T, v id.ID[id.Any]) T {
	if err := t.UnmarshalText([]byte(v.String())); err != nil {
		panic(err)
	}
	return t
}

func mAccountID() margin.AccountID {
	var v margin.AccountID
	parseInto(&v, testMarginAcct)
	return v
}

func mCaseID() margin.CaseID {
	var v margin.CaseID
	parseInto(&v, testMarginCase)
	return v
}

func mRunID() margin.StressRunID {
	var v margin.StressRunID
	parseInto(&v, testMarginRun)
	return v
}

func mCallID() margin.CallID {
	var v margin.CallID
	parseInto(&v, testMarginCall)
	return v
}

func mStepID() margin.StepID {
	var v margin.StepID
	parseInto(&v, testMarginStep)
	return v
}

func mMoveID() margin.MoveID {
	var v margin.MoveID
	parseInto(&v, testMarginMove)
	return v
}

func mLotID() margin.LotID {
	var v margin.LotID
	parseInto(&v, testMarginLot)
	return v
}

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

// fixedMarginState is the margin engine's fixed account: 1,000 paper dollars
// and 20 SOL at 100 in collateral against a 2,000 dollar loan accrued one day
// ago at the policy's 8%.
func fixedMarginState(t *testing.T) margin.AccountState {
	t.Helper()
	mark, err := money.NewPrice(q(100), 0, testMarginUSD.String(), "test-oracle", testNow)
	require.NoError(t, err)
	return margin.AccountState{
		AccountID: testAccountID.String(), Mode: margin.ModeCross, NumeraireAssetID: testMarginUSD.String(), NumeraireDecimals: 6,
		Holdings: []margin.Holding{
			{AssetID: testMarginUSD.String(), Decimals: 6, Quantity: q(1_000_000_000), Numeraire: true},
			{AssetID: testMarginSOL.String(), Decimals: 9, Quantity: q(20_000_000_000), Mark: &mark,
				Profile: margin.AssetProfile{Volatility: margin.VolLow, Liquidity: margin.LiqDeep, Venue: margin.VenueTier1, RiskWeightBPS: 10_000}},
		},
		Loans: []margin.Loan{{LotID: mLotID().String(), Principal: q(2_000_000_000), Interest: q(0), RateBPS: 800,
			AccruedThrough: testNow.Add(-24 * time.Hour)}},
		Derivatives: []margin.DerivativeExposure{},
		Now:         testNow,
	}
}

func fixedMarginEvaluation(t *testing.T) margin.Evaluation {
	t.Helper()
	ev, err := margin.Evaluate(margin.DefaultPolicy(), fixedMarginState(t))
	require.NoError(t, err)
	return ev
}

func fixedMarginFacts(acct accounts.AccountID) MarginFacts {
	return MarginFacts{AccountID: acct, NumeraireAssetID: testMarginUSD, NumeraireDecimals: 6}
}

// fakeMargin is the margin port. It counts the calls of every method, so a
// test can prove a replay never reached the service, and records what each
// call was scoped to.
type fakeMargin struct {
	stubErr
	mu         sync.Mutex
	calls      map[string]int
	lastAcct   accounts.AccountID
	lastLimit  int
	lastCursor string
	lastStatus margin.CallStatus
	lastOpen   MarginOpen
	lastColl   margin.CollateralRequest
	lastLoan   margin.LoanRequest
	lastSpot   margin.SpotRequest
	lastLiq    margin.LiquidateRequest
	lastSeed   MarginSeed
	lastActor  econ.Actor

	view       MarginAccountView
	call       MarginCallView
	caseView   MarginCaseView
	run        MarginStressRunView
	account    margin.Account
	collateral margin.CollateralResult
	loan       margin.LoanResult
	spot       margin.SpotResult
	facilities MarginFacilitiesView
	mark       MarginMarkView
	queue      []MarginQueueEntry
	queueRun   MarginQueueRunView
	liq        MarginLiquidationView
	deficit    MarginDeficitView
	next       string

	// The covered deficit and the MANUAL_REVIEW dual control.
	deficitStatus MarginDeficitStatusView
	repayment     MarginDeficitRepaymentView
	reviews       []MarginReviewView
	resolution    margin.Resolution
	decided       MarginResolutionDecisionView
	lastRepay     margin.DeficitRepaymentRequest
	lastPropose   margin.ProposeResolutionRequest
	lastDecide    margin.DecideResolutionRequest
}

func (f *fakeMargin) hit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *fakeMargin) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeMargin) scoped(acct accounts.AccountID, cursor string, limit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAcct, f.lastCursor, f.lastLimit = acct, cursor, limit
}

func (f *fakeMargin) Account(_ context.Context, acct accounts.AccountID) (MarginAccountView, error) {
	f.hit("Account")
	f.scoped(acct, "", 0)
	return f.view, f.fail()
}

func (f *fakeMargin) Calls(_ context.Context, acct accounts.AccountID, cursor string, limit int) (MarginCallPage, error) {
	f.hit("Calls")
	f.scoped(acct, cursor, limit)
	return MarginCallPage{Items: []MarginCallView{f.call}, NextCursor: f.next}, f.fail()
}

func (f *fakeMargin) Cases(_ context.Context, acct accounts.AccountID, cursor string, limit int) (MarginCasePage, error) {
	f.hit("Cases")
	f.scoped(acct, cursor, limit)
	return MarginCasePage{Items: []MarginCaseView{f.caseView}, NextCursor: f.next}, f.fail()
}

func (f *fakeMargin) Case(_ context.Context, cid margin.CaseID) (MarginCaseView, error) {
	f.hit("Case")
	if cid != f.caseView.Case.ID {
		return MarginCaseView{}, errs.New(errs.CodeNotFound, "liquidation case not found")
	}
	return f.caseView, f.fail()
}

func (f *fakeMargin) StressRuns(_ context.Context, acct accounts.AccountID, cursor string, limit int) (MarginStressRunPage, error) {
	f.hit("StressRuns")
	f.scoped(acct, cursor, limit)
	summary := f.run
	summary.Report.Results = nil
	return MarginStressRunPage{Items: []MarginStressRunView{summary}, NextCursor: f.next}, f.fail()
}

func (f *fakeMargin) StressRun(_ context.Context, rid margin.StressRunID) (MarginStressRunView, error) {
	f.hit("StressRun")
	if rid != f.run.ID {
		return MarginStressRunView{}, errs.New(errs.CodeNotFound, "stress run not found")
	}
	return f.run, f.fail()
}

func (f *fakeMargin) Open(_ context.Context, r MarginOpen) (margin.Account, error) {
	f.hit("Open")
	f.mu.Lock()
	f.lastOpen = r
	f.mu.Unlock()
	return f.account, f.fail()
}

func (f *fakeMargin) Close(_ context.Context, acct accounts.AccountID, by econ.Actor) (margin.Account, error) {
	f.hit("Close")
	f.mu.Lock()
	f.lastAcct, f.lastActor = acct, by
	f.mu.Unlock()
	closed := f.account
	closed.Status = margin.StatusClosed
	return closed, f.fail()
}

func (f *fakeMargin) PostCollateral(_ context.Context, r margin.CollateralRequest) (margin.CollateralResult, error) {
	f.hit("PostCollateral")
	f.mu.Lock()
	f.lastColl = r
	f.mu.Unlock()
	return f.collateral, f.fail()
}

func (f *fakeMargin) ReleaseCollateral(_ context.Context, r margin.CollateralRequest) (margin.CollateralResult, error) {
	f.hit("ReleaseCollateral")
	f.mu.Lock()
	f.lastColl = r
	f.mu.Unlock()
	return f.collateral, f.fail()
}

func (f *fakeMargin) Borrow(_ context.Context, r margin.LoanRequest) (margin.LoanResult, error) {
	f.hit("Borrow")
	f.mu.Lock()
	f.lastLoan = r
	f.mu.Unlock()
	return f.loan, f.fail()
}

func (f *fakeMargin) Repay(_ context.Context, r margin.LoanRequest) (margin.LoanResult, error) {
	f.hit("Repay")
	f.mu.Lock()
	f.lastLoan = r
	f.mu.Unlock()
	return f.loan, f.fail()
}

func (f *fakeMargin) LeveragedBuy(_ context.Context, r margin.SpotRequest) (margin.SpotResult, error) {
	f.hit("LeveragedBuy")
	f.mu.Lock()
	f.lastSpot = r
	f.mu.Unlock()
	return f.spot, f.fail()
}

func (f *fakeMargin) RunStress(_ context.Context, acct accounts.AccountID) (MarginStressRunView, error) {
	f.hit("RunStress")
	f.scoped(acct, "", 0)
	return f.run, f.fail()
}

func (f *fakeMargin) SeedFacilities(_ context.Context, r MarginSeed) (MarginFacilitiesView, error) {
	f.hit("SeedFacilities")
	f.mu.Lock()
	f.lastSeed = r
	f.mu.Unlock()
	return f.facilities, f.fail()
}

func (f *fakeMargin) MarkToMarket(_ context.Context, limit int) (MarginMarkView, error) {
	f.hit("MarkToMarket")
	f.scoped(accounts.AccountID{}, "", limit)
	return f.mark, f.fail()
}

func (f *fakeMargin) Queue(_ context.Context, limit int) ([]MarginQueueEntry, error) {
	f.hit("Queue")
	f.scoped(accounts.AccountID{}, "", limit)
	return f.queue, f.fail()
}

func (f *fakeMargin) RunQueue(_ context.Context, limit int) (MarginQueueRunView, error) {
	f.hit("RunQueue")
	f.scoped(accounts.AccountID{}, "", limit)
	return f.queueRun, f.fail()
}

func (f *fakeMargin) Liquidate(_ context.Context, r margin.LiquidateRequest) (MarginLiquidationView, error) {
	f.hit("Liquidate")
	f.mu.Lock()
	f.lastLiq = r
	f.mu.Unlock()
	return f.liq, f.fail()
}

func (f *fakeMargin) CoverDeficit(_ context.Context, cid margin.CaseID) (MarginDeficitView, error) {
	f.hit("CoverDeficit")
	if cid != f.deficit.Result.CaseID {
		return MarginDeficitView{}, errs.New(errs.CodeNotFound, "liquidation case not found")
	}
	return f.deficit, f.fail()
}

func (f *fakeMargin) AllCalls(_ context.Context, status margin.CallStatus, cursor string, limit int) (MarginCallPage, error) {
	f.hit("AllCalls")
	f.mu.Lock()
	f.lastStatus, f.lastCursor, f.lastLimit = status, cursor, limit
	f.mu.Unlock()
	return MarginCallPage{Items: []MarginCallView{f.call}, NextCursor: f.next}, f.fail()
}

func (f *fakeMargin) Deficit(_ context.Context, acct accounts.AccountID) (MarginDeficitStatusView, error) {
	f.hit("Deficit")
	f.mu.Lock()
	f.lastAcct = acct
	f.mu.Unlock()
	return f.deficitStatus, f.fail()
}

func (f *fakeMargin) RepayDeficit(_ context.Context, r margin.DeficitRepaymentRequest) (MarginDeficitRepaymentView, error) {
	f.hit("RepayDeficit")
	f.mu.Lock()
	f.lastRepay = r
	f.mu.Unlock()
	if err := f.fail(); err != nil {
		return MarginDeficitRepaymentView{}, err
	}
	return f.repayment, nil
}

func (f *fakeMargin) Reviews(_ context.Context, limit int) ([]MarginReviewView, error) {
	f.hit("Reviews")
	f.mu.Lock()
	f.lastLimit = limit
	f.mu.Unlock()
	return f.reviews, f.fail()
}

func (f *fakeMargin) ProposeResolution(_ context.Context, r margin.ProposeResolutionRequest) (margin.Resolution, error) {
	f.hit("ProposeResolution")
	f.mu.Lock()
	f.lastPropose = r
	f.mu.Unlock()
	if err := f.fail(); err != nil {
		return margin.Resolution{}, err
	}
	return f.resolution, nil
}

func (f *fakeMargin) DecideResolution(_ context.Context, r margin.DecideResolutionRequest) (MarginResolutionDecisionView, error) {
	f.hit("DecideResolution")
	f.mu.Lock()
	f.lastDecide = r
	f.mu.Unlock()
	if err := f.fail(); err != nil {
		return MarginResolutionDecisionView{}, err
	}
	return f.decided, nil
}

// newFakeMargin holds realistic values for testAccountID: the engine's own
// evaluation of the fixed account, an open margin call, a bankrupt case with
// one executed sale, and a stored stress run computed by the engine.
func newFakeMargin(t *testing.T) *fakeMargin {
	t.Helper()
	st := fixedMarginState(t)
	ev := fixedMarginEvaluation(t)
	facts := fixedMarginFacts(testAccountID)
	acct := margin.Account{
		ID: mAccountID(), AccountID: testAccountID, MarginMode: margin.ModeCross, Mode: gates.ModePaper, Status: margin.StatusNormal,
		PolicyVersion: margin.DefaultPolicy().Version, NumeraireAssetID: testMarginUSD, LendingPoolID: econ.NewFacilityID(),
		InsuranceFundID: econ.NewFacilityID(), OpenedBy: econ.Actor{Type: security.ActorUser, ID: testUserID.String()}, OpenedAt: testNow,
	}
	resolved := testNow.Add(time.Hour)
	call := margin.Call{ID: mCallID(), MarginAccountID: acct.ID, Status: margin.CallOpen, IssuedAt: testNow,
		EquityUnits: q(138_000_000), MaintenanceUnits: q(152_000_000), DeficiencyUnits: q(14_000_000), EvaluationHash: "eh"}
	deficit := q(211_981_234)
	cover := ledger.NewTransactionID()
	closed := testNow.Add(2 * time.Hour)
	mark, err := money.NewPrice(q(40), 0, testMarginUSD.String(), "test-oracle", testNow)
	require.NoError(t, err)
	exec := money.Price{Mantissa: q(399_000), Scale: 4, QuoteAsset: testMarginUSD.String()}
	step := margin.Step{
		ID: mStepID(), CaseID: mCaseID(), StepNo: 1, Action: margin.ActionSell, AssetID: testMarginSOL, Quantity: q(20_000_000_000),
		Mark: &mark, ExecutionPrice: &exec, SlippageBPS: 25, SlippageCapBPS: 500, Proceeds: q(798_000_000), Fee: q(7_980_000),
		Repaid: q(790_020_000), Excess: q(0), TradeJournalID: &testMarginJrnl, RepayJournalID: &testMarginJrnl2,
		EquityBefore: q(-201_000_000), EquityAfter: q(-212_000_000), PlanHash: "ph", ExecutedAt: testNow,
	}
	kase := margin.Case{ID: mCaseID(), MarginAccountID: acct.ID, MarginCallID: call.ID, Status: margin.CaseBankrupt,
		PolicyVersion: acct.PolicyVersion, Target: margin.TargetMaintenancePlusBuffer, MaxSteps: 10, StepsExecuted: 2, OpenedAt: testNow,
		ClosedAt: &closed, LastReason: "nothing is left to sell or apply and liabilities remain", DeficitUnits: &deficit,
		CoverageJournalID: &cover, CoveredAt: &resolved}
	report, err := margin.RunStress(margin.DefaultPolicy(), st)
	require.NoError(t, err)
	lot := mLotID()
	iid := econ.NewIntentID()
	eff := econ.NewEffectID()
	return &fakeMargin{
		calls: map[string]int{},
		view: MarginAccountView{Account: acct, State: st, Evaluation: ev,
			OpenCall:   &MarginCallView{Call: call, Facts: facts},
			LatestCase: &MarginCaseView{Case: kase, Facts: facts}},
		call:     MarginCallView{Call: call, Facts: facts},
		caseView: MarginCaseView{Case: kase, Facts: facts, Steps: []margin.Step{step}},
		run:      MarginStressRunView{ID: mRunID(), MarginAccountID: acct.ID, Facts: facts, RunAt: testNow, Report: report},
		account:  acct,
		collateral: margin.CollateralResult{MarginAccountID: acct.ID, MoveID: mMoveID(), JournalTransactionID: testMarginJrnl,
			EffectID: eff, ReservationID: econ.NewIntentID().String(), Status: margin.StatusNormal, Evaluation: ev},
		loan: margin.LoanResult{MarginAccountID: acct.ID, IntentID: iid, LotID: &lot, JournalTransactionID: testMarginJrnl, EffectID: eff,
			Allocations: []margin.LoanAllocation{{LotID: lot.String(), Interest: q(438_357), Principal: q(99_561_643)}},
			Status:      margin.StatusNormal, Evaluation: ev},
		spot: margin.SpotResult{MarginAccountID: acct.ID, IntentID: iid, LotID: &lot, Mark: mark,
			ExecutionPrice: money.Price{Mantissa: q(400_400), Scale: 4, QuoteAsset: testMarginUSD.String(), Source: "test-oracle", At: testNow},
			SlippageBPS:    10, Cost: q(800_800_001), BorrowJournal: testMarginJrnl, TradeJournal: testMarginJrnl2, CollateralJrnl: testMarginJrnl3,
			ReservationID: econ.NewIntentID().String(), Status: margin.StatusNormal, Evaluation: ev},
		facilities: MarginFacilitiesView{AssetID: testMarginUSD, AssetDecimals: 6,
			Pool:          econ.Facility{ID: acct.LendingPoolID, Kind: ledger.FacilityLendingPool, Name: "pool", Status: econ.FacilityActive},
			Fund:          econ.Facility{ID: acct.InsuranceFundID, Kind: ledger.FacilityInsuranceFund, Name: "fund", Status: econ.FacilityActive},
			PoolAvailable: q(1_000_000_000_000), FundAvailable: q(100_000_000_000)},
		mark: MarginMarkView{
			Evaluated: []margin.EvaluateResult{{MarginAccountID: acct.ID, AccountID: testAccountID, Previous: margin.StatusWarning,
				Status: margin.StatusMarginCall, Evaluation: ev, Call: &call}},
			Failures: []error{errs.New(errs.CodeReconciliationRequired, "margin: the borrow lots disagree with the ledger"),
				errs.Wrap(context.DeadlineExceeded, errs.CodeInternal, "pq: secret driver detail")},
		},
		queue: []MarginQueueEntry{{Account: acct, Facts: facts, State: &st, Evaluation: &ev,
			OpenCall: &MarginCallView{Call: call, Facts: facts}, LatestCase: &MarginCaseView{Case: kase, Facts: facts}},
			{Account: acct, Facts: facts, EvaluationErr: errs.Wrap(context.Canceled, errs.CodeInternal, "pq: another secret")}},
		queueRun: MarginQueueRunView{
			Results:  []MarginLiquidationView{{Result: margin.LiquidationResult{MarginAccountID: acct.ID, Eligible: true, Case: &kase, AccountStatus: margin.StatusLiquidating, Decision: margin.DecisionSellAll, Reason: "sell everything", Step: &step}, Facts: facts}},
			Failures: []margin.QueueFailure{{AccountID: testOtherAcct, Err: errs.New(errs.CodeReconciliationRequired, "lots disagree")}},
		},
		liq: MarginLiquidationView{Result: margin.LiquidationResult{MarginAccountID: acct.ID, Eligible: true, Case: &kase,
			AccountStatus: margin.StatusLiquidating, Decision: margin.DecisionSellAll, Reason: "no partial liquidation restores the target",
			Step: &step}, Facts: facts},
		deficit: MarginDeficitView{Result: margin.DeficitResult{CaseID: mCaseID(), Deficit: deficit, JournalTransactionID: cover},
			Case: MarginCaseView{Case: kase, Facts: facts}},
	}
}

func newMarginHarness(t *testing.T) (*harness, *fakeMargin) {
	t.Helper()
	h := newHarness(t)
	fm := newFakeMargin(t)
	ports := h.ports.ports()
	ports.Margin = fm
	h.server.opts.Ports = ports
	return h, fm
}

func strangerPrincipal() security.Principal {
	p := customerPrincipal()
	p.AccountIDs = []string{testOtherAcct.String()}
	return p
}

// --- tenant scoping ------------------------------------------------------------

func TestMarginAccountReads_AreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/advanced/margin/account", "/v1/advanced/margin/calls", "/v1/advanced/margin/cases",
		"/v1/advanced/margin/stress-runs"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarginHarness(t)
			res := h.do(http.MethodGet, path+"?account_id="+testAccountID.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, fm.lastAcct)
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
			assert.Equal(t, testOtherAcct, fm.lastAcct)
		})
	}
}

// A record fetched by its own id is scoped by the account stored on it, and a
// stranger's record answers exactly what an absent one answers.
func TestMarginRecordsByID_AreScopedByTheirOwnAccountAndMaskedAsNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ path, absent string }{
		{"/v1/advanced/margin/cases/" + mCaseID().String(), "/v1/advanced/margin/cases/" + testOrderID.String()},
		{"/v1/advanced/margin/stress-runs/" + mRunID().String(), "/v1/advanced/margin/stress-runs/" + testOrderID.String()},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			h, _ := newMarginHarness(t)
			res := h.do(http.MethodGet, tc.path, nil)
			require.Equal(t, http.StatusOK, res.Code, "the owner reads it; body=%s", res.Body.String())
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

			stranger := strangerPrincipal()
			h.as(&stranger)
			foreign := h.do(http.MethodGet, tc.path, nil)
			require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
			fp := foreign.problem()
			assert.Equal(t, errs.CodeNotFound, fp.Code)

			absent := h.do(http.MethodGet, tc.absent, nil)
			require.Equal(t, http.StatusNotFound, absent.Code, "body=%s", absent.Body.String())
			ap := absent.problem()
			assert.Equal(t, fp.Code, ap.Code)
			assert.Equal(t, fp.Detail, ap.Detail, "a foreign and an absent record must read alike")
			assert.Equal(t, len(fp.Fields), len(ap.Fields))

			op := operatorPrincipal()
			h.as(&op)
			require.Equal(t, http.StatusOK, h.do(http.MethodGet, tc.path, nil).Code)
		})
	}
}

// Every customer command is the owner's alone: an operator holding
// account:read_any (and every customer permission) still cannot act on a
// customer's margin account through the customer surface.
func TestMarginCustomerCommands_UseOwnershipOnly(t *testing.T) {
	t.Parallel()
	for _, m := range marginCustomerMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarginHarness(t)
			op := operatorPrincipal()
			h.as(&op)
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "own-only-"+m.name)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Zero(t, fm.count(m.counter))

			stranger := strangerPrincipal()
			h.as(&stranger)
			res = h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "own-only-2-"+m.name)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Zero(t, fm.count(m.counter))
		})
	}
}

// The operator surface is closed to customers: their role holds no
// sandbox:operate.
func TestMarginOperatorRoutes_RefuseACustomer(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	for _, m := range marginOperatorMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "customer-op-"+m.name)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Zero(t, fm.count(m.counter), m.name)
	}
	for _, path := range []string{"/v1/advanced/margin/ops/queue", "/v1/advanced/margin/ops/calls"} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", path, res.Body.String())
	}
}

// Value-moving operator actions demand a recent strong authentication.
func TestMarginValueMovingOperatorActions_RequireStepUp(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	stale := operatorPrincipal()
	stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
	h.as(&stale)
	for _, m := range marginOperatorMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stale-op-"+m.name)
		if m.stepUp {
			require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code, m.name)
			assert.Zero(t, fm.count(m.counter), m.name)
			continue
		}
		require.Equal(t, http.StatusOK, res.Code, "%s moves no value and needs no step-up; body=%s", m.name, res.Body.String())
	}
}

// --- idempotency -------------------------------------------------------------------

type marginMutation struct {
	name    string
	path    string
	altPath string
	body    any
	altBody any
	counter string
	stepUp  bool
}

func marginCustomerMutations() []marginMutation {
	acct := testAccountID.String()
	return []marginMutation{
		{name: "open", path: "/v1/advanced/margin/account", counter: "Open",
			body:    map[string]any{"account_id": acct, "margin_mode": "CROSS", "mode": "PAPER"},
			altBody: map[string]any{"account_id": acct, "margin_mode": "CROSS", "mode": "SANDBOX"}},
		{name: "close", path: "/v1/advanced/margin/account/close", counter: "Close",
			body: map[string]any{"account_id": acct}, altBody: map[string]any{"account_id": acct, "x": 1}},
		{name: "post", path: "/v1/advanced/margin/collateral/post", counter: "PostCollateral",
			body:    map[string]any{"account_id": acct, "asset_id": testMarginUSD.String(), "quantity": "1000000000"},
			altBody: map[string]any{"account_id": acct, "asset_id": testMarginUSD.String(), "quantity": "1000000001"}},
		{name: "release", path: "/v1/advanced/margin/collateral/release", counter: "ReleaseCollateral",
			body:    map[string]any{"account_id": acct, "asset_id": testMarginUSD.String(), "quantity": "5"},
			altBody: map[string]any{"account_id": acct, "asset_id": testMarginUSD.String(), "quantity": "6"}},
		{name: "borrow", path: "/v1/advanced/margin/borrow", counter: "Borrow",
			body: map[string]any{"account_id": acct, "amount": "100000000"}, altBody: map[string]any{"account_id": acct, "amount": "100000001"}},
		{name: "repay", path: "/v1/advanced/margin/repay", counter: "Repay",
			body: map[string]any{"account_id": acct, "amount": "100000000"}, altBody: map[string]any{"account_id": acct, "amount": "7"}},
		{name: "leveraged-buy", path: "/v1/advanced/margin/leveraged-buys", counter: "LeveragedBuy",
			body:    map[string]any{"account_id": acct, "asset_id": testMarginSOL.String(), "quantity": "8000000000"},
			altBody: map[string]any{"account_id": acct, "asset_id": testMarginSOL.String(), "quantity": "8000000001"}},
		{name: "stress", path: "/v1/advanced/margin/stress-runs", counter: "RunStress",
			body: map[string]any{"account_id": acct}, altBody: map[string]any{"account_id": acct, "x": 2}},
	}
}

func marginOperatorMutations() []marginMutation {
	acct := testAccountID.String()
	return []marginMutation{
		{name: "seed", path: "/v1/advanced/margin/ops/facilities/seed", counter: "SeedFacilities", stepUp: true,
			body:    map[string]any{"pool_amount": "1000000000000", "fund_amount": "100000000000"},
			altBody: map[string]any{"pool_amount": "1000000000001", "fund_amount": "100000000000"}},
		{name: "mark-to-market", path: "/v1/advanced/margin/ops/mark-to-market", counter: "MarkToMarket",
			body: map[string]any{"limit": 10}, altBody: map[string]any{"limit": 11}},
		{name: "queue-run", path: "/v1/advanced/margin/ops/queue/run", counter: "RunQueue", stepUp: true,
			body: map[string]any{"limit": 5}, altBody: map[string]any{"limit": 6}},
		{name: "liquidate", path: "/v1/advanced/margin/ops/liquidations", counter: "Liquidate", stepUp: true,
			body: map[string]any{"account_id": acct, "expected_step": 1}, altBody: map[string]any{"account_id": acct, "expected_step": 2}},
		{name: "cover-deficit", path: "/v1/advanced/margin/ops/cases/" + mCaseID().String() + "/cover-deficit", counter: "CoverDeficit",
			stepUp: true, altPath: "/v1/advanced/margin/ops/cases/" + testOrderID.String() + "/cover-deficit"},
		{name: "ops-stress", path: "/v1/advanced/margin/ops/stress-runs", counter: "RunStress",
			body: map[string]any{"account_id": acct}, altBody: map[string]any{"account_id": testOtherAcct.String()}},
	}
}

// Every margin command replays: the same key and body answer the same
// response without a second call to the service, and the same key with a
// different request is refused.
func TestMarginCommands_ReplayAndRefuseKeyReuse(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, m marginMutation, p security.Principal) {
		h, fm := newMarginHarness(t)
		h.as(&p)
		key := "replay-" + m.name + "-000"
		first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
		require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
		require.Equal(t, 1, fm.count(m.counter))
		assert.Equal(t, "SIMULATED", first.raw()["value_domain"])

		second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
		require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())
		assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers the first response")
		assert.Equal(t, 1, fm.count(m.counter), "a replay never reaches the service")

		path, body := m.path, m.altBody
		if m.altPath != "" {
			path, body = m.altPath, m.body
		}
		reuse := h.do(http.MethodPost, path, body, "Idempotency-Key", key)
		require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
		assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
		assert.Equal(t, 1, fm.count(m.counter))
	}
	for _, m := range marginCustomerMutations() {
		t.Run("customer/"+m.name, func(t *testing.T) {
			t.Parallel()
			run(t, m, customerPrincipal())
		})
	}
	for _, m := range marginOperatorMutations() {
		t.Run("operator/"+m.name, func(t *testing.T) {
			t.Parallel()
			run(t, m, operatorPrincipal())
		})
	}
}

// A refusal is a recorded conclusion: its code and reason codes reach the
// wire unchanged, and replaying the key repeats it without asking again.
func TestMarginRefusals_KeepTheirCodeAndReplay(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	fm.err = errs.New(errs.CodeRiskRejected, "risk kernel rejected BORROW").WithField("reason_codes", []string{"RISK_MARGIN_INITIAL"})
	body := map[string]any{"account_id": testAccountID.String(), "amount": "100000000"}
	res := h.do(http.MethodPost, "/v1/advanced/margin/borrow", body, "Idempotency-Key", "refused-borrow-01")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeRiskRejected, p.Code)
	assert.Equal(t, []any{"RISK_MARGIN_INITIAL"}, p.Fields["reason_codes"])

	fm.err = nil
	again := h.do(http.MethodPost, "/v1/advanced/margin/borrow", body, "Idempotency-Key", "refused-borrow-01")
	require.Equal(t, http.StatusUnprocessableEntity, again.Code, "body=%s", again.Body.String())
	assert.Equal(t, errs.CodeRiskRejected, again.problem().Code)
	assert.Equal(t, 1, fm.count("Borrow"))

	fm.err = errs.New(errs.CodeInvalidStateTransition, "margin: a MARGIN_CALL account takes no new leverage")
	res = h.do(http.MethodPost, "/v1/advanced/margin/leveraged-buys", map[string]any{"account_id": testAccountID.String(),
		"asset_id": testMarginSOL.String(), "quantity": "1"}, "Idempotency-Key", "refused-buy-0001")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)
}

// --- validation -----------------------------------------------------------------------

func TestMarginRequests_AreValidatedBeforeTheService(t *testing.T) {
	t.Parallel()
	acct := testAccountID.String()
	cases := []struct {
		name, method, path string
		body               any
		field              string
		operator           bool
	}{
		{"unknown margin mode", http.MethodPost, "/v1/advanced/margin/account",
			map[string]any{"account_id": acct, "margin_mode": "LEVERED", "mode": "PAPER"}, "margin_mode", false},
		{"unknown mode", http.MethodPost, "/v1/advanced/margin/account",
			map[string]any{"account_id": acct, "margin_mode": "CROSS", "mode": "ON_CHAIN"}, "mode", false},
		{"nil isolated asset", http.MethodPost, "/v1/advanced/margin/account",
			map[string]any{"account_id": acct, "margin_mode": "ISOLATED", "mode": "PAPER", "isolated_asset_id": nilUUIDText}, "isolated_asset_id", false},
		{"fractional quantity", http.MethodPost, "/v1/advanced/margin/collateral/post",
			map[string]any{"account_id": acct, "asset_id": testMarginUSD.String(), "quantity": "1.5"}, "quantity", false},
		{"nil asset", http.MethodPost, "/v1/advanced/margin/collateral/release",
			map[string]any{"account_id": acct, "asset_id": nilUUIDText, "quantity": "1"}, "asset_id", false},
		{"amount not a number", http.MethodPost, "/v1/advanced/margin/borrow",
			map[string]any{"account_id": acct, "amount": "abc"}, "amount", false},
		{"amount in dollars", http.MethodPost, "/v1/advanced/margin/repay",
			map[string]any{"account_id": acct, "amount": "10.00"}, "amount", false},
		{"buy quantity empty", http.MethodPost, "/v1/advanced/margin/leveraged-buys",
			map[string]any{"account_id": acct, "asset_id": testMarginSOL.String(), "quantity": ""}, "quantity", false},
		{"seed amount", http.MethodPost, "/v1/advanced/margin/ops/facilities/seed",
			map[string]any{"pool_amount": "1e9", "fund_amount": "1"}, "pool_amount", true},
		{"negative step", http.MethodPost, "/v1/advanced/margin/ops/liquidations",
			map[string]any{"account_id": acct, "expected_step": -1}, "expected_step", true},
		{"nil account", http.MethodPost, "/v1/advanced/margin/ops/liquidations",
			map[string]any{"account_id": nilUUIDText}, "account_id", true},
		{"zero limit", http.MethodPost, "/v1/advanced/margin/ops/queue/run", map[string]any{"limit": 0}, "limit", true},
		{"unknown call status", http.MethodGet, "/v1/advanced/margin/ops/calls?status=PENDING", nil, "status", true},
		{"nil case id", http.MethodGet, "/v1/advanced/margin/cases/" + nilUUIDText, nil, "marginCaseId", false},
		{"nil stress run id", http.MethodGet, "/v1/advanced/margin/stress-runs/" + nilUUIDText, nil, "marginStressRunId", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarginHarness(t)
			if tc.operator {
				op := operatorPrincipal()
				h.as(&op)
			}
			res := h.do(tc.method, tc.path, tc.body, "Idempotency-Key", "validate-key-0001")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeValidationFailed, p.Code)
			assert.Equal(t, tc.field, p.Fields["field"])
			fm.mu.Lock()
			defer fm.mu.Unlock()
			assert.Empty(t, fm.calls, "a malformed request never reaches the service")
		})
	}
}

const nilUUIDText = "00000000-0000-0000-0000-000000000000"

// The handler passes the service the scoped account, the caller as the
// requester and actor, the Idempotency-Key, and the sandbox-dollar numeraire
// is not the caller's to choose.
func TestMarginCommands_PassTheScopedRequestToTheService(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	res := h.do(http.MethodPost, "/v1/advanced/margin/account", map[string]any{"account_id": testAccountID.String(),
		"margin_mode": "ISOLATED", "mode": "SANDBOX", "isolated_asset_id": testMarginSOL.String()}, "Idempotency-Key", "open-isolated-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, MarginOpen{AccountID: testAccountID, MarginMode: margin.ModeIsolated, Mode: gates.ModeSandbox,
		IsolatedAssetID: &testMarginSOL, Actor: econ.Actor{Type: security.ActorUser, ID: testUserID.String()}}, fm.lastOpen)

	res = h.do(http.MethodPost, "/v1/advanced/margin/borrow", map[string]any{"account_id": testAccountID.String(), "amount": "99561643"},
		"Idempotency-Key", "borrow-scoped-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, fm.lastLoan.AccountID)
	assert.Equal(t, "99561643", fm.lastLoan.Amount.String())
	assert.Equal(t, "borrow-scoped-01", fm.lastLoan.IdempotencyKey)
	assert.Equal(t, margin.Requester{Type: security.ActorUser, ID: testUserID.String()}, fm.lastLoan.Requester)

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodPost, "/v1/advanced/margin/ops/liquidations", map[string]any{"account_id": testOtherAcct.String()},
		"Idempotency-Key", "liquidate-any-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, margin.LiquidateRequest{AccountID: testOtherAcct, ExpectedStep: 0}, fm.lastLiq,
		"an operator liquidates any account; absent expected_step is whatever step is next")

	res = h.do(http.MethodPost, "/v1/advanced/margin/ops/facilities/seed", map[string]any{"pool_amount": "5", "fund_amount": "0"},
		"Idempotency-Key", "seed-key-000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "seed-key-000001", fm.lastSeed.Key, "the Idempotency-Key makes the funding exactly once")
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: testUserID.String()}, fm.lastSeed.Actor)

	res = h.do(http.MethodGet, "/v1/advanced/margin/ops/calls?status=OPEN&limit=7", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, margin.CallOpen, fm.lastStatus)
	assert.Equal(t, 7, fm.lastLimit)
}

// --- exact figures ------------------------------------------------------------------------

// The account read renders the engine's own evaluation, exactly: quantities
// as integer strings of numeraire base units, the engine's conservative cent
// rendering as two-decimal strings, basis points as integers.
func TestMarginAccount_RendersTheEnginesFiguresExactly(t *testing.T) {
	t.Parallel()
	h, _ := newMarginHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/margin/account?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "CROSS", raw["margin_mode"])
	assert.Equal(t, "PAPER", raw["mode"])
	assert.Equal(t, testMarginUSD.String(), raw["numeraire_asset_id"])

	ev := raw["evaluation"].(map[string]any)
	// 1,000 + 20 × 100 × (1 − 25% haircut) − 2,000 − one day of 8% on 2,000.
	assert.Equal(t, "2500000000", ev["eligible_collateral"])
	assert.Equal(t, "2000000000", ev["borrow_liabilities"])
	assert.Equal(t, "438357", ev["accrued_interest"], "ceil(2e9 × 800 × 1 day / (10000 × 365 days))")
	assert.Equal(t, "499561643", ev["equity"])
	assert.Equal(t, "400000000", ev["initial_margin"])
	assert.Equal(t, "200000000", ev["maintenance_margin"])
	assert.Equal(t, "250000000", ev["warning_threshold"])
	assert.Equal(t, "299561643", ev["liquidation_buffer"])
	assert.Equal(t, "99561643", ev["margin_available"])
	assert.Equal(t, float64(40036), ev["effective_leverage_bps"], "ceil(2e9 × 10000 / 499561643)")
	assert.Equal(t, float64(50000), ev["leverage_cap_bps"])
	assert.Equal(t, float64(6), ev["numeraire_decimals"])
	assert.Equal(t, "NORMAL", ev["health"])
	assert.Equal(t, true, ev["prices_fresh"])
	assert.NotContains(t, ev, "liquidation_price", "the engine reports no liquidation price and none is invented")

	usdBlock := ev["usd"].(map[string]any)
	assert.Equal(t, "499.56", usdBlock["equity"], "equity rounds down")
	assert.Equal(t, "0.44", usdBlock["accrued_interest"], "liabilities round up")
	assert.Equal(t, "299.56", usdBlock["liquidation_buffer"])
	assert.Equal(t, "99.56", usdBlock["margin_available"])
	assert.Equal(t, "400.00", usdBlock["initial_margin"])

	holdings := ev["holdings"].([]any)
	require.Len(t, holdings, 2)
	var sol map[string]any
	for _, h := range holdings {
		if h.(map[string]any)["asset_id"] == testMarginSOL.String() {
			sol = h.(map[string]any)
		}
	}
	require.NotNil(t, sol)
	assert.Equal(t, "20000000000", sol["quantity"])
	assert.Equal(t, "2000000000", sol["market_value"])
	assert.Equal(t, "1500000000", sol["collateral_value"])
	hc := sol["haircut"].(map[string]any)
	assert.Equal(t, float64(1000), hc["volatility_bps"])
	assert.Equal(t, float64(1500), hc["concentration_bps"])
	assert.Equal(t, float64(2500), hc["total_bps"])
	mark := sol["mark"].(map[string]any)
	assert.Equal(t, "100", mark["mantissa"])
	assert.Equal(t, float64(0), mark["scale"])

	loans := raw["loans"].([]any)
	require.Len(t, loans, 1)
	assert.Equal(t, "2000000000", loans[0].(map[string]any)["principal"])
	assert.Equal(t, float64(800), loans[0].(map[string]any)["rate_bps"])

	call := raw["open_call"].(map[string]any)
	assert.Equal(t, "14000000", call["deficiency"])
	assert.Equal(t, "OPEN", call["status"])
	kase := raw["latest_case"].(map[string]any)
	assert.Equal(t, "BANKRUPT", kase["status"])
	assert.Equal(t, "211981234", kase["deficit"])
	assert.Equal(t, testAccountID.String(), kase["account_id"])
}

// A result the service replayed carries no evaluation (it computed none), and
// none is shown; a replayed buy shows no price it did not recompute.
func TestMarginCommands_AServiceReplayShowsNoInventedFigures(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	fm.loan.Replayed, fm.loan.Evaluation = true, margin.Evaluation{}
	res := h.do(http.MethodPost, "/v1/advanced/margin/borrow", map[string]any{"account_id": testAccountID.String(), "amount": "1"},
		"Idempotency-Key", "svc-replay-borrow")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, true, raw["replayed"])
	assert.NotContains(t, raw, "evaluation")

	fm.spot.Replayed, fm.spot.Evaluation = true, margin.Evaluation{}
	res = h.do(http.MethodPost, "/v1/advanced/margin/leveraged-buys", map[string]any{"account_id": testAccountID.String(),
		"asset_id": testMarginSOL.String(), "quantity": "1"}, "Idempotency-Key", "svc-replay-buy01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, true, raw["replayed"])
	assert.Equal(t, "800800001", raw["cost"])
	for _, k := range []string{"evaluation", "mark", "execution_price", "slippage_bps"} {
		assert.NotContains(t, raw, k)
	}
}

func TestMarginCommandResults_AreExactAndSimulated(t *testing.T) {
	t.Parallel()
	h, _ := newMarginHarness(t)
	res := h.do(http.MethodPost, "/v1/advanced/margin/repay", map[string]any{"account_id": testAccountID.String(), "amount": "100000000"},
		"Idempotency-Key", "exact-repay-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	alloc := raw["allocations"].([]any)[0].(map[string]any)
	assert.Equal(t, "438357", alloc["interest"], "interest is repaid first")
	assert.Equal(t, "99561643", alloc["principal"])
	assert.Equal(t, "499561643", raw["evaluation"].(map[string]any)["equity"])

	res = h.do(http.MethodPost, "/v1/advanced/margin/leveraged-buys", map[string]any{"account_id": testAccountID.String(),
		"asset_id": testMarginSOL.String(), "quantity": "8000000000"}, "Idempotency-Key", "exact-buy-000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, "800800001", raw["cost"])
	assert.Equal(t, float64(10), raw["slippage_bps"])
	exec := raw["execution_price"].(map[string]any)
	assert.Equal(t, "400400", exec["mantissa"])
	assert.Equal(t, float64(4), exec["scale"])

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodPost, "/v1/advanced/margin/ops/facilities/seed", map[string]any{"pool_amount": "1", "fund_amount": "1"},
		"Idempotency-Key", "exact-seed-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, "1000000000000", raw["lending_pool"].(map[string]any)["available"])
	assert.Equal(t, "LENDING_POOL", raw["lending_pool"].(map[string]any)["kind"])
	assert.Equal(t, "100000000000", raw["insurance_fund"].(map[string]any)["available"])

	res = h.do(http.MethodPost, "/v1/advanced/margin/ops/liquidations", map[string]any{"account_id": testAccountID.String()},
		"Idempotency-Key", "exact-liq-000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, "SELL_ALL", raw["decision"])
	step := raw["step"].(map[string]any)
	assert.Equal(t, "798000000", step["proceeds"])
	assert.Equal(t, "7980000", step["fee"])
	assert.Equal(t, "790020000", step["repaid"])
	assert.Equal(t, "-212000000", step["equity_after"], "a negative quantity keeps its sign")
	assert.Equal(t, "399000", step["execution_price"].(map[string]any)["mantissa"])

	res = h.do(http.MethodPost, "/v1/advanced/margin/ops/cases/"+mCaseID().String()+"/cover-deficit", nil,
		"Idempotency-Key", "exact-cover-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, "211981234", raw["deficit"])
	assert.Equal(t, "BANKRUPT", raw["case"].(map[string]any)["status"])
}

func TestMarginReads_RenderStepsStressAndQueueExactly(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/margin/cases/"+mCaseID().String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	steps := raw["steps"].([]any)
	require.Len(t, steps, 1)
	assert.Equal(t, "SELL", steps[0].(map[string]any)["action"])
	assert.Equal(t, "40", steps[0].(map[string]any)["mark"].(map[string]any)["mantissa"])

	res = h.do(http.MethodGet, "/v1/advanced/margin/stress-runs/"+mRunID().String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	results := raw["results"].([]any)
	require.Len(t, results, len(margin.AllStressScenarios()))
	byScenario := map[string]map[string]any{}
	for _, r := range results {
		byScenario[r.(map[string]any)["scenario"].(string)] = r.(map[string]any)
	}
	gap := byScenario["GAP_DOWN_50"]
	// The engine's own figures for the fixed account at half the mark.
	want := fm.run.Report.Results[3]
	require.Equal(t, margin.StressGapDown50, want.Scenario)
	assert.Equal(t, want.EquityAfter.String(), gap["equity_after"])
	assert.Equal(t, want.DeficitUSD.String(), gap["deficit_usd"])
	assert.Equal(t, string(want.Outcome), gap["outcome"])

	res = h.do(http.MethodGet, "/v1/advanced/margin/stress-runs?account_id="+testAccountID.String()+"&limit=3", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 3, fm.lastLimit)
	item := res.raw()["items"].([]any)[0].(map[string]any)
	assert.NotContains(t, item, "results", "a page lists summaries")
	assert.Equal(t, fm.run.Report.Hash, item["report_hash"])

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/margin/ops/queue?limit=500", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, maxPageLimit, fm.lastLimit, "the limit is clamped")
	items := res.raw()["items"].([]any)
	require.Len(t, items, 2)
	first := items[0].(map[string]any)
	assert.Equal(t, "299561643", first["evaluation"].(map[string]any)["liquidation_buffer"])
	assert.Equal(t, "OPEN", first["open_call"].(map[string]any)["status"])
	failed := items[1].(map[string]any)
	assert.NotContains(t, failed, "evaluation")
	ee := failed["evaluation_error"].(map[string]any)
	assert.Equal(t, "INTERNAL", ee["code"])
	assert.NotContains(t, ee["detail"], "secret", "an internal failure is redacted")
}

func TestMarginOperatorBatches_ReportEveryFailureRedacted(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, "/v1/advanced/margin/ops/mark-to-market", map[string]any{"limit": 3}, "Idempotency-Key", "mtm-failures-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 3, fm.lastLimit)
	raw := res.raw()
	evaluated := raw["evaluated"].([]any)
	require.Len(t, evaluated, 1)
	e := evaluated[0].(map[string]any)
	assert.Equal(t, "WARNING", e["previous_status"])
	assert.Equal(t, "MARGIN_CALL", e["status"])
	assert.Equal(t, testAccountID.String(), e["call"].(map[string]any)["account_id"])
	failures := raw["failures"].([]any)
	require.Len(t, failures, 2)
	assert.Equal(t, "RECONCILIATION_REQUIRED", failures[0].(map[string]any)["code"])
	assert.Equal(t, "INTERNAL", failures[1].(map[string]any)["code"])
	assert.NotContains(t, failures[1].(map[string]any)["detail"], "secret")

	res = h.do(http.MethodPost, "/v1/advanced/margin/ops/queue/run", nil, "Idempotency-Key", "queue-run-nobody")
	require.Equal(t, http.StatusOK, res.Code, "an absent body runs the service's own bound; body=%s", res.Body.String())
	assert.Equal(t, 0, fm.lastLimit)
	raw = res.raw()
	assert.Equal(t, testOtherAcct.String(), raw["failures"].([]any)[0].(map[string]any)["account_id"])
	assert.Equal(t, "SELL_ALL", raw["results"].([]any)[0].(map[string]any)["decision"])
}

// A deployment without the margin service says so on every margin route.
func TestMarginRoutes_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	acct := testAccountID.String()
	for _, tc := range []struct {
		method, path string
		body         any
		operator     bool
	}{
		{http.MethodGet, "/v1/advanced/margin/account?account_id=" + acct, nil, false},
		{http.MethodGet, "/v1/advanced/margin/calls?account_id=" + acct, nil, false},
		{http.MethodGet, "/v1/advanced/margin/cases?account_id=" + acct, nil, false},
		{http.MethodGet, "/v1/advanced/margin/cases/" + mCaseID().String(), nil, false},
		{http.MethodGet, "/v1/advanced/margin/stress-runs?account_id=" + acct, nil, false},
		{http.MethodGet, "/v1/advanced/margin/stress-runs/" + mRunID().String(), nil, false},
		{http.MethodGet, "/v1/advanced/margin/ops/queue", nil, true},
		{http.MethodGet, "/v1/advanced/margin/ops/calls", nil, true},
	} {
		if tc.operator {
			h.as(&op)
		} else {
			p := customerPrincipal()
			h.as(&p)
		}
		res := h.do(tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", tc.path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, tc.path)
	}
	p := customerPrincipal()
	for _, m := range marginCustomerMutations() {
		h.as(&p)
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "unwired-"+m.name)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
	for _, m := range marginOperatorMutations() {
		h.as(&op)
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "unwired-"+m.name)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
}

// Pagination: the cursor reaches the port verbatim and next_cursor is
// rendered nullable, like every other page.
func TestMarginPages_CarryTheCursor(t *testing.T) {
	t.Parallel()
	h, fm := newMarginHarness(t)
	fm.next = "cursor-2"
	res := h.do(http.MethodGet, "/v1/advanced/margin/calls?account_id="+testAccountID.String()+"&cursor=cursor-1&limit=5000", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "cursor-1", fm.lastCursor)
	assert.Equal(t, maxPageLimit, fm.lastLimit)
	assert.Equal(t, "cursor-2", res.raw()["next_cursor"])

	fm.next = ""
	res = h.do(http.MethodGet, "/v1/advanced/margin/cases?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Contains(t, raw, "next_cursor")
	assert.Nil(t, raw["next_cursor"])
	assert.Equal(t, defaultPageLimit, fm.lastLimit)
}

// --- covered deficits and MANUAL_REVIEW under dual control ---------------------------

var (
	testMarginResolution = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac08")
	testMarginRepayment  = id.MustParse[id.Any]("0193b2e0-0000-7000-8000-00000000ac09")
	testMarginReview     = "0193b2e0-0000-7000-8000-00000000ac0a"
	// testMarginApprover is a second operator, distinct from testUserID.
	testMarginApprover = "0193b2e0-0000-7000-8000-00000000ac0b"
)

func mResolutionID() margin.ResolutionID {
	var v margin.ResolutionID
	parseInto(&v, testMarginResolution)
	return v
}

func mRepaymentID() margin.RepaymentID {
	var v margin.RepaymentID
	parseInto(&v, testMarginRepayment)
	return v
}

// newMarginDualControlHarness extends the margin fake with a covered deficit
// of 211.981234 sandbox dollars of which 11.981234 is repaid, and a case in
// MANUAL_REVIEW with one pending resolution.
func newMarginDualControlHarness(t *testing.T) (*harness, *fakeMargin) {
	t.Helper()
	h, fm := newMarginHarness(t)
	facts := fm.caseView.Facts
	kase := fm.caseView.Case
	repaidAt := testNow.Add(3 * time.Hour)
	marginAcct := fm.account
	fm.deficitStatus = MarginDeficitStatusView{
		AccountID: testAccountID, Margin: &marginAcct, Facts: &facts, Case: &MarginCaseView{Case: kase, Facts: facts},
		Covered: &MarginCoveredDeficit{Deficit: q(211_981_234), Repaid: q(11_981_234), Outstanding: q(200_000_000)},
		Repayments: []MarginDeficitRepaymentRecord{{ID: mRepaymentID(), Amount: q(11_981_234), JournalTransactionID: testMarginJrnl3,
			EffectID: econ.NewEffectID(), IntentID: econ.NewIntentID(), RepaidAt: repaidAt}},
	}
	fm.repayment = MarginDeficitRepaymentView{Facts: facts, Result: margin.DeficitRepaymentResult{
		MarginAccountID: fm.account.ID, CaseID: kase.ID, RepaymentID: mRepaymentID(), IntentID: econ.NewIntentID(),
		JournalTransactionID: testMarginJrnl3, EffectID: econ.NewEffectID(), Repaid: q(100_000_000), Outstanding: q(100_000_000),
	}}
	review := kase
	review.Status, review.DeficitUnits, review.CoverageJournalID, review.CoveredAt, review.ClosedAt = margin.CaseManualReview, nil, nil, nil, nil
	fm.resolution = margin.Resolution{
		ID: mResolutionID(), CaseID: kase.ID, ReviewTransitionID: testMarginReview, Outcome: margin.ResolveResume,
		Reason: "the venue is back; finish the liquidation", ProposedBy: econ.Actor{Type: security.ActorOperator, ID: testUserID.String()},
		ProposedAt: testNow,
	}
	fm.reviews = []MarginReviewView{{Case: MarginCaseView{Case: review, Facts: facts}, CurrentReviewTransitionID: testMarginReview,
		EnteredReviewAt: testNow.Add(-time.Hour), Resolutions: []margin.Resolution{fm.resolution}}}
	approved := fm.resolution
	decision := margin.ResolutionApproved
	decidedAt := testNow.Add(time.Minute)
	approved.Decision, approved.DecidedBy = &decision, &econ.Actor{Type: security.ActorOperator, ID: testMarginApprover}
	approved.DecidedAt, approved.DecisionReason = &decidedAt, "agreed"
	resumed := review
	resumed.Status = margin.CaseInProgress
	fm.decided = MarginResolutionDecisionView{Case: MarginCaseView{Case: resumed, Facts: facts}, Result: margin.ResolutionResult{
		Resolution: approved, Case: resumed, AccountStatus: margin.StatusLiquidating,
		Waived: []margin.Accrual{{LotID: mLotID(), Period: margin.Period{Start: testNow.Add(-24 * time.Hour), End: testNow}, Amount: q(0), Waived: true}},
	}}
	return h, fm
}

func TestMarginDeficit_TheReadIsScopedAndExact(t *testing.T) {
	t.Parallel()
	path := "/v1/advanced/margin/deficit?account_id=" + testAccountID.String()
	h, fm := newMarginDualControlHarness(t)
	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, fm.lastAcct)
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "211981234", raw["deficit"], "exact base units of the numeraire")
	assert.Equal(t, "11981234", raw["repaid"])
	assert.Equal(t, "200000000", raw["outstanding"])
	assert.EqualValues(t, 6, raw["numeraire_decimals"])
	repayments, ok := raw["repayments"].([]any)
	require.True(t, ok)
	require.Len(t, repayments, 1)
	assert.Equal(t, "11981234", repayments[0].(map[string]any)["amount"])

	// No covered deficit: the case is shown, the deficit figures are absent.
	fm.deficitStatus.Covered = nil
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.NotContains(t, raw, "outstanding")
	assert.NotContains(t, raw, "repaid")

	stranger := strangerPrincipal()
	h.as(&stranger)
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 2, fm.count("Deficit"), "a foreign account never reaches the service")

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "an operator reads any account; body=%s", res.Body.String())

	// No margin account: nothing is owed, and nothing about a margin account is invented.
	fm.deficitStatus = MarginDeficitStatusView{AccountID: testAccountID, Repayments: []MarginDeficitRepaymentRecord{}}
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, testAccountID.String(), raw["account_id"])
	for _, absent := range []string{"margin_account_id", "numeraire_asset_id", "numeraire_decimals", "case", "deficit"} {
		assert.NotContains(t, raw, absent)
	}
}

func TestMarginDeficit_RepaymentIsTheOwnersExactAndReplayed(t *testing.T) {
	t.Parallel()
	h, fm := newMarginDualControlHarness(t)
	body := map[string]any{"account_id": testAccountID.String(), "amount": "100000000"}
	res := h.do(http.MethodPost, "/v1/advanced/margin/deficit/repayments", body, "Idempotency-Key", "deficit-key-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Equal(t, 1, fm.count("RepayDeficit"))
	assert.Equal(t, testAccountID, fm.lastRepay.AccountID)
	assert.Equal(t, "100000000", fm.lastRepay.Amount.String())
	assert.Equal(t, "deficit-key-00001", fm.lastRepay.IdempotencyKey, "the service's own replay key is the client's key")
	assert.Equal(t, security.ActorUser, fm.lastRepay.Requester.Type)
	assert.Equal(t, testUserID.String(), fm.lastRepay.Requester.ID)
	raw := res.raw()
	assert.Equal(t, "100000000", raw["repaid"])
	assert.Equal(t, "100000000", raw["outstanding"])
	assert.Equal(t, mRepaymentID().String(), raw["repayment_id"])
	assert.Equal(t, "SIMULATED", raw["value_domain"])

	again := h.do(http.MethodPost, "/v1/advanced/margin/deficit/repayments", body, "Idempotency-Key", "deficit-key-00001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, fm.count("RepayDeficit"), "a replay must not reach the service")

	body["amount"] = "100000001"
	res = h.do(http.MethodPost, "/v1/advanced/margin/deficit/repayments", body, "Idempotency-Key", "deficit-key-00001")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)

	for name, amount := range map[string]any{"decimal": "1.5", "float": "1e6", "negative sign only": "-"} {
		bad := map[string]any{"account_id": testAccountID.String(), "amount": amount}
		res = h.do(http.MethodPost, "/v1/advanced/margin/deficit/repayments", bad, "Idempotency-Key", "deficit-bad-"+name[:3])
		require.Equal(t, http.StatusBadRequest, res.Code, "%s body=%s", name, res.Body.String())
	}
	assert.Equal(t, 1, fm.count("RepayDeficit"))

	// Ownership only: neither a stranger nor an operator repays for an account.
	for name, p := range map[string]security.Principal{"stranger": strangerPrincipal(), "operator": operatorPrincipal()} {
		h.as(&p)
		res = h.do(http.MethodPost, "/v1/advanced/margin/deficit/repayments",
			map[string]any{"account_id": testAccountID.String(), "amount": "1"}, "Idempotency-Key", "deficit-own-"+name)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", name, res.Body.String())
	}
	assert.Equal(t, 1, fm.count("RepayDeficit"))

	// The service's refusals keep their codes.
	customer := customerPrincipal()
	h.as(&customer)
	fm.err = errs.New(errs.CodeInvalidStateTransition, "margin: the account has no covered deficit to repay")
	res = h.do(http.MethodPost, "/v1/advanced/margin/deficit/repayments",
		map[string]any{"account_id": testAccountID.String(), "amount": "1"}, "Idempotency-Key", "deficit-key-00002")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)
}

func TestMarginReviews_TheQueueIsAnOperatorRead(t *testing.T) {
	t.Parallel()
	h, fm := newMarginDualControlHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/margin/ops/reviews", nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Zero(t, fm.count("Reviews"))

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/margin/ops/reviews?limit=5", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 5, fm.lastLimit)
	var out api.MarginReviewQueue
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	require.Len(t, out.Items, 1)
	item := out.Items[0]
	assert.Equal(t, api.MarginCaseStatus("MANUAL_REVIEW"), item.Case.Status)
	assert.Equal(t, testMarginReview, item.CurrentReviewTransitionId.String())
	require.Len(t, item.Resolutions, 1)
	assert.True(t, item.Resolutions[0].Pending, "no decision yet: it awaits a second operator")
	assert.Equal(t, api.MarginResolutionResume, item.Resolutions[0].Outcome)
	assert.Nil(t, item.Resolutions[0].Decision)
}

func TestMarginResolutions_EachStepIsAnOperatorsUnderStepUp(t *testing.T) {
	t.Parallel()
	propose := "/v1/advanced/margin/ops/cases/" + mCaseID().String() + "/resolutions"
	decide := "/v1/advanced/margin/ops/resolutions/" + mResolutionID().String() + "/decision"
	for name, call := range map[string]struct {
		path string
		body map[string]any
	}{
		"propose": {propose, map[string]any{"outcome": "RESUME", "reason": "the venue is back"}},
		"decide":  {decide, map[string]any{"decision": "APPROVED", "reason": "agreed"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarginDualControlHarness(t)
			res := h.do(http.MethodPost, call.path, call.body, "Idempotency-Key", "resolution-key-"+name)
			require.Equal(t, http.StatusForbidden, res.Code, "a customer; body=%s", res.Body.String())
			stale := operatorPrincipal()
			stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
			h.as(&stale)
			res = h.do(http.MethodPost, call.path, call.body, "Idempotency-Key", "resolution-key-"+name+"-2")
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
			assert.Zero(t, fm.count("ProposeResolution")+fm.count("DecideResolution"))
		})
	}
}

func TestMarginResolutions_AProposalNamesItsOperatorAndIsReplayed(t *testing.T) {
	t.Parallel()
	h, fm := newMarginDualControlHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	path := "/v1/advanced/margin/ops/cases/" + mCaseID().String() + "/resolutions"
	body := map[string]any{"outcome": "RESUME", "reason": "the venue is back; finish the liquidation"}
	res := h.do(http.MethodPost, path, body, "Idempotency-Key", "propose-res-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Equal(t, 1, fm.count("ProposeResolution"))
	assert.Equal(t, mCaseID(), fm.lastPropose.CaseID)
	assert.Equal(t, margin.ResolveResume, fm.lastPropose.Outcome)
	assert.Equal(t, "the venue is back; finish the liquidation", fm.lastPropose.Reason)
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: testUserID.String()}, fm.lastPropose.Proposer)
	raw := res.raw()
	assert.Equal(t, true, raw["pending"])
	assert.Equal(t, "RESUME", raw["outcome"])
	assert.Equal(t, testMarginReview, raw["review_transition_id"])

	again := h.do(http.MethodPost, path, body, "Idempotency-Key", "propose-res-00001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, fm.count("ProposeResolution"), "a replay must not reach the service")

	res = h.do(http.MethodPost, path, map[string]any{"outcome": "FORGIVE", "reason": "x"}, "Idempotency-Key", "propose-res-00002")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodPost, "/v1/advanced/margin/ops/cases/not-a-uuid/resolutions", body, "Idempotency-Key", "propose-res-00003")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, fm.count("ProposeResolution"))

	fm.err = errs.New(errs.CodeInvalidStateTransition, "margin: only a case in MANUAL_REVIEW is resolved")
	res = h.do(http.MethodPost, path, body, "Idempotency-Key", "propose-res-00004")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)
}

func TestMarginResolutions_ADecisionCarriesTheCaseAfterItAndRefusalsKeepTheirCodes(t *testing.T) {
	t.Parallel()
	h, fm := newMarginDualControlHarness(t)
	approver := operatorPrincipal()
	approver.SubjectID = testMarginApprover
	h.as(&approver)
	path := "/v1/advanced/margin/ops/resolutions/" + mResolutionID().String() + "/decision"
	res := h.do(http.MethodPost, path, map[string]any{"decision": "APPROVED", "reason": "agreed"}, "Idempotency-Key", "decide-res-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, mResolutionID(), fm.lastDecide.ResolutionID)
	assert.Equal(t, margin.ResolutionApproved, fm.lastDecide.Decision)
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: testMarginApprover}, fm.lastDecide.Decider)
	var out api.MarginResolutionDecisionResult
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.False(t, out.Resolution.Pending)
	require.NotNil(t, out.Resolution.Decision)
	assert.Equal(t, api.MarginResolutionApproved, *out.Resolution.Decision)
	require.NotNil(t, out.Resolution.DecidedBy)
	assert.Equal(t, testMarginApprover, *out.Resolution.DecidedBy)
	assert.Equal(t, api.MarginCaseStatus("IN_PROGRESS"), out.Case.Status)
	assert.Equal(t, api.MarginAccountStatus("LIQUIDATING"), out.AccountStatus)
	require.Len(t, out.Waived, 1)
	assert.Equal(t, "0", out.Waived[0].Amount, "a waived period charges nothing")
	assert.True(t, out.Waived[0].Waived)

	again := h.do(http.MethodPost, path, map[string]any{"decision": "APPROVED", "reason": "agreed"}, "Idempotency-Key", "decide-res-00001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.Equal(t, 1, fm.count("DecideResolution"), "a replay must not reach the service")

	res = h.do(http.MethodPost, path, map[string]any{"decision": "MAYBE", "reason": "x"}, "Idempotency-Key", "decide-res-00002")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())

	for name, refusal := range map[string]error{
		"self approval":   errs.Wrap(security.ErrSelfApproval, errs.CodeForbidden, "margin: the operator who proposed a resolution cannot approve it"),
		"already decided": errs.New(errs.CodeConflict, "margin: resolution was already REJECTED"),
		"unknown":         errs.New(errs.CodeNotFound, "case resolution not found"),
	} {
		fm.err = refusal
		res = h.do(http.MethodPost, path, map[string]any{"decision": "APPROVED", "reason": "agreed"}, "Idempotency-Key", "decide-ref-"+name[:4])
		require.Equal(t, errs.HTTPStatus(errs.CodeOf(refusal)), res.Code, "%s body=%s", name, res.Body.String())
		assert.Equal(t, errs.CodeOf(refusal), res.problem().Code, name)
	}
}

func TestMarginDualControl_UnwiredMarginIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	customer := customerPrincipal()
	for _, c := range []struct {
		method, path string
		body         any
		as           *security.Principal
	}{
		{http.MethodGet, "/v1/advanced/margin/deficit?account_id=" + testAccountID.String(), nil, &customer},
		{http.MethodPost, "/v1/advanced/margin/deficit/repayments", map[string]any{"account_id": testAccountID.String(), "amount": "1"}, &customer},
		{http.MethodGet, "/v1/advanced/margin/ops/reviews", nil, &op},
		{http.MethodPost, "/v1/advanced/margin/ops/cases/" + mCaseID().String() + "/resolutions", map[string]any{"outcome": "RESUME", "reason": "x"}, &op},
		{http.MethodPost, "/v1/advanced/margin/ops/resolutions/" + mResolutionID().String() + "/decision", map[string]any{"decision": "REJECTED", "reason": "x"}, &op},
	} {
		h.as(c.as)
		res := h.do(c.method, c.path, c.body, "Idempotency-Key", "unwired-margin-0001")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s %s body=%s", c.method, c.path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
	}
}
