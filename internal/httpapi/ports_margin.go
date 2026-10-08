package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
)

// MarginPort serves margin, leverage, borrowing and liquidation of simulated
// positions (internal/margin; ADR-0020). Every figure behind it is value
// domain SIMULATED and, unless a field says otherwise, in base units of the
// account's numeraire (the sandbox dollar).
//
// The margin service decides everything: every command runs one service
// operation in its own transaction, and the reads present the service's own
// view of an account (margin.Service.State evaluated by margin.Evaluate under
// the service's policy) or project the margin tables read-only, because the
// margin package exposes no listing of calls, cases, steps or stress runs.
type MarginPort interface {
	// Account returns a customer account's margin account evaluated at the
	// instant of the read, with its open margin call and latest case. It
	// changes nothing; NOT_FOUND when the account has no margin account.
	Account(ctx context.Context, account accounts.AccountID) (MarginAccountView, error)
	Calls(ctx context.Context, account accounts.AccountID, cursor string, limit int) (MarginCallPage, error)
	Cases(ctx context.Context, account accounts.AccountID, cursor string, limit int) (MarginCasePage, error)
	// Case returns one liquidation case with its steps; the caller scopes it
	// by the case's own account.
	Case(ctx context.Context, id margin.CaseID) (MarginCaseView, error)
	StressRuns(ctx context.Context, account accounts.AccountID, cursor string, limit int) (MarginStressRunPage, error)
	// StressRun returns one stored stress run with its report; the caller
	// scopes it by the run's own account.
	StressRun(ctx context.Context, id margin.StressRunID) (MarginStressRunView, error)

	Open(ctx context.Context, r MarginOpen) (margin.Account, error)
	Close(ctx context.Context, account accounts.AccountID, by econ.Actor) (margin.Account, error)
	PostCollateral(ctx context.Context, r margin.CollateralRequest) (margin.CollateralResult, error)
	ReleaseCollateral(ctx context.Context, r margin.CollateralRequest) (margin.CollateralResult, error)
	Borrow(ctx context.Context, r margin.LoanRequest) (margin.LoanResult, error)
	Repay(ctx context.Context, r margin.LoanRequest) (margin.LoanResult, error)
	LeveragedBuy(ctx context.Context, r margin.SpotRequest) (margin.SpotResult, error)
	RunStress(ctx context.Context, account accounts.AccountID) (MarginStressRunView, error)

	SeedFacilities(ctx context.Context, r MarginSeed) (MarginFacilitiesView, error)
	MarkToMarket(ctx context.Context, limit int) (MarginMarkView, error)
	// Queue lists the accounts a liquidation queue run considers: MARGIN_CALL
	// or LIQUIDATING under the service's policy, each with its live
	// evaluation.
	Queue(ctx context.Context, limit int) ([]MarginQueueEntry, error)
	RunQueue(ctx context.Context, limit int) (MarginQueueRunView, error)
	Liquidate(ctx context.Context, r margin.LiquidateRequest) (MarginLiquidationView, error)
	CoverDeficit(ctx context.Context, id margin.CaseID) (MarginDeficitView, error)
	// AllCalls pages margin calls across every account; an empty status lists
	// every status.
	AllCalls(ctx context.Context, status margin.CallStatus, cursor string, limit int) (MarginCallPage, error)

	// Deficit reads an account's latest liquidation case, the deficit the
	// insurance fund covered and what was repaid of it. NOT_FOUND when the
	// account has no margin account.
	Deficit(ctx context.Context, account accounts.AccountID) (MarginDeficitStatusView, error)
	// RepayDeficit repays a covered deficit to the insurance fund
	// (margin.Service.RepayDeficit); the owner's own write.
	RepayDeficit(ctx context.Context, r margin.DeficitRepaymentRequest) (MarginDeficitRepaymentView, error)
	// Reviews lists the cases in MANUAL_REVIEW, the longest waiting first,
	// with every resolution of each.
	Reviews(ctx context.Context, limit int) ([]MarginReviewView, error)
	// ProposeResolution and DecideResolution are the two operators of a
	// MANUAL_REVIEW exit (margin.Service.ProposeCaseResolution and
	// DecideCaseResolution).
	ProposeResolution(ctx context.Context, r margin.ProposeResolutionRequest) (margin.Resolution, error)
	DecideResolution(ctx context.Context, r margin.DecideResolutionRequest) (MarginResolutionDecisionView, error)
}

// MarginDeficitStatusView is an account's latest liquidation case and the
// deficit repayments made against it. Margin and Facts are nil when the
// account has no margin account, Case when it never had a case; Covered is
// nil unless that case is BANKRUPT with a deficit the insurance fund covered
// -- the only deficit margin.Service.RepayDeficit accepts a repayment of.
type MarginDeficitStatusView struct {
	AccountID  accounts.AccountID
	Margin     *margin.Account
	Facts      *MarginFacts
	Case       *MarginCaseView
	Covered    *MarginCoveredDeficit
	Repayments []MarginDeficitRepaymentRecord
}

// MarginCoveredDeficit is a covered deficit, what was repaid of it and what
// is still owed, in base units of the numeraire.
type MarginCoveredDeficit struct {
	Deficit     money.Quantity
	Repaid      money.Quantity
	Outstanding money.Quantity
}

// MarginDeficitRepaymentRecord is one margin_deficit_repayments row.
type MarginDeficitRepaymentRecord struct {
	ID                   margin.RepaymentID
	Amount               money.Quantity
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	IntentID             econ.IntentID
	RepaidAt             time.Time
}

// MarginDeficitRepaymentView is one repayment with the account facts its
// quantities are read in.
type MarginDeficitRepaymentView struct {
	Result margin.DeficitRepaymentResult
	Facts  MarginFacts
}

// MarginReviewView is a case in MANUAL_REVIEW: the transition that put it
// in the review it is in now and every resolution proposed for it.
type MarginReviewView struct {
	Case                      MarginCaseView
	CurrentReviewTransitionID string
	EnteredReviewAt           time.Time
	Resolutions               []margin.Resolution
}

// MarginResolutionDecisionView is a decided resolution and the case after it.
type MarginResolutionDecisionView struct {
	Result margin.ResolutionResult
	Case   MarginCaseView
}

// MarginFacts are the account facts a margin record needs to be read: whose
// it is and what unit its quantities are in.
type MarginFacts struct {
	AccountID         accounts.AccountID
	NumeraireAssetID  assets.AssetID
	NumeraireDecimals uint8
}

// MarginAccountView is a margin account and the engine's evaluation of it,
// taken from one consistent snapshot. State is what the evaluation saw: the
// marks of the holdings and the open loans.
type MarginAccountView struct {
	Account    margin.Account
	State      margin.AccountState
	Evaluation margin.Evaluation
	OpenCall   *MarginCallView
	LatestCase *MarginCaseView
}

// MarginCallView is a margin call with its account facts.
type MarginCallView struct {
	Call  margin.Call
	Facts MarginFacts
}

// MarginCallPage is one page of margin calls, newest first.
type MarginCallPage struct {
	Items      []MarginCallView
	NextCursor string
}

// MarginCaseView is a liquidation case with its account facts. Steps are
// present when the case was read by its own id.
type MarginCaseView struct {
	Case  margin.Case
	Facts MarginFacts
	Steps []margin.Step
}

// MarginCasePage is one page of liquidation cases, newest first.
type MarginCasePage struct {
	Items      []MarginCaseView
	NextCursor string
}

// MarginStressRunView is a stored stress run. Report.Results is empty in a
// page, which lists summaries only.
type MarginStressRunView struct {
	ID              margin.StressRunID
	MarginAccountID margin.AccountID
	Facts           MarginFacts
	RunAt           time.Time
	Report          margin.StressReport
}

// MarginStressRunPage is one page of stress runs, newest first.
type MarginStressRunPage struct {
	Items      []MarginStressRunView
	NextCursor string
}

// MarginOpen opens a margin account. The numeraire is not the caller's to
// choose: the adapter passes the sandbox dollar.
type MarginOpen struct {
	AccountID       accounts.AccountID
	MarginMode      margin.MarginMode
	Mode            gates.Mode
	IsolatedAssetID *assets.AssetID
	Actor           econ.Actor
}

// MarginSeed funds the lending pool and the insurance fund in the sandbox
// dollar, exactly once per key.
type MarginSeed struct {
	PoolAmount money.Quantity
	FundAmount money.Quantity
	Key        string
	Actor      econ.Actor
}

// MarginFacilitiesView is the two facilities margin accounts are wired to
// and the sandbox dollars each holds (unlent, undrawn) after a seeding.
type MarginFacilitiesView struct {
	AssetID       assets.AssetID
	AssetDecimals uint8
	Pool          econ.Facility
	Fund          econ.Facility
	PoolAvailable money.Quantity
	FundAvailable money.Quantity
}

// MarginMarkView is one mark-to-market run. A failure names no account: the
// margin service reports it as an error only.
type MarginMarkView struct {
	Evaluated []margin.EvaluateResult
	Failures  []error
}

// MarginQueueEntry is one account of the liquidation queue. Evaluation is nil
// when the engine could not evaluate the account; EvaluationErr says why.
type MarginQueueEntry struct {
	Account       margin.Account
	Facts         MarginFacts
	State         *margin.AccountState
	Evaluation    *margin.Evaluation
	EvaluationErr error
	OpenCall      *MarginCallView
	LatestCase    *MarginCaseView
}

// MarginLiquidationView is one liquidation result with the account facts its
// case and step are read in.
type MarginLiquidationView struct {
	Result margin.LiquidationResult
	Facts  MarginFacts
}

// MarginQueueRunView is what one liquidation queue run did.
type MarginQueueRunView struct {
	Results  []MarginLiquidationView
	Failures []margin.QueueFailure
}

// MarginDeficitView is a deficit coverage and the case after it.
type MarginDeficitView struct {
	Result margin.DeficitResult
	Case   MarginCaseView
}
