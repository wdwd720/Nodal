package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// Futures, perpetuals and options in PAPER and SANDBOX mode
// (internal/derivatives, ADR-0020). Every figure behind this port is value
// domain SIMULATED: amounts are base units of a contract's settlement asset
// and prices are integer mantissas at its price scale.

// DerivativesPort serves /advanced/derivatives. The service decides every
// rule; the port carries its answers and the read model of its tables.
type DerivativesPort interface {
	// Policy is the service's margin, funding and paper-fill policy.
	Policy(ctx context.Context) (derivatives.Policy, error)
	// Contracts lists derivative contracts ("" lists every derivative kind).
	Contracts(ctx context.Context, kind instruments.InstrumentType, limit int) ([]DerivativeContractView, error)
	// Contract is one derivative contract; NOT_FOUND for any other instrument.
	Contract(ctx context.Context, id instruments.InstrumentID) (DerivativeContractView, error)
	// Marks pages a contract's marks, newest observation first.
	Marks(ctx context.Context, id instruments.InstrumentID, cursor string, limit int) ([]derivatives.Mark, string, error)
	// Analytics is an option's reference analytics at its latest mark.
	Analytics(ctx context.Context, r derivatives.AnalyticsRequest) (DerivativeAnalyticsView, error)

	// Positions pages an account's positions ("" status lists every status)
	// with the service's valuation of the account's open book.
	Positions(ctx context.Context, account accounts.AccountID, status derivatives.PositionStatus, cursor string, limit int) (DerivativePositionPageView, error)
	// Position is one position, valued when it is open. The caller scopes it
	// by the position's own account.
	Position(ctx context.Context, id derivatives.PositionID) (DerivativePositionView, error)
	// PositionAccount is the account a position belongs to, so a command on
	// it can be scoped before it runs. NOT_FOUND when there is no such
	// position.
	PositionAccount(ctx context.Context, id derivatives.PositionID) (accounts.AccountID, error)
	// Flows pages an account's register of clearing-house flows, newest first.
	Flows(ctx context.Context, account accounts.AccountID, filter DerivativeFlowFilter, cursor string, limit int) ([]DerivativeFlowView, string, error)
	// FundingAccruals pages the funding accrued to an account's positions,
	// newest interval first; a non-nil position narrows it to that position.
	FundingAccruals(ctx context.Context, account accounts.AccountID, position *derivatives.PositionID, cursor string, limit int) ([]DerivativeAccrualView, string, error)

	// Open, Close and Exercise run the service's economic operation. A
	// refusal the service records is committed and returned as the
	// service's error (derivatives.AsRefusal recognises it).
	Open(ctx context.Context, c DerivativeOpenCommand) (DerivativeTradeView, error)
	Close(ctx context.Context, c DerivativeReduceCommand) (DerivativeTradeView, error)
	Exercise(ctx context.Context, c DerivativeReduceCommand) (DerivativeTradeView, error)

	// The operator side of the sandbox.
	RecordMark(ctx context.Context, c DerivativeMarkCommand) (derivatives.Mark, error)
	RunVariationMargin(ctx context.Context, c DerivativeRunCommand) (DerivativeRunView, error)
	RunFunding(ctx context.Context, c DerivativeRunCommand) (DerivativeRunView, error)
	SettleExpiry(ctx context.Context, c DerivativeSettleCommand) (DerivativeSettlementView, error)
	FundClearingHouse(ctx context.Context, c DerivativeClearingFundCommand) (DerivativeClearingFundView, error)
	ReconcileClearing(ctx context.Context, asset assets.AssetID) (DerivativeClearingReportView, error)
}

// DerivativeAssetView names the asset amounts are counted in, so base units
// can be read as the decimal they denote.
type DerivativeAssetView struct {
	ID       assets.AssetID
	Symbol   string
	Decimals uint8
}

// DerivativeSettlementRecord is a contract's final settlement.
type DerivativeSettlementRecord struct {
	ID            derivatives.SettlementID
	Kind          derivatives.SettlementKind
	PriceMantissa money.Quantity
	PriceScale    int32
	PriceSource   string
	Positions     int
	SettledAt     time.Time
}

// DerivativeContractView is a derivative instrument with the terms in force,
// the asset it settles in, its latest mark and its final settlement.
type DerivativeContractView struct {
	Instrument instruments.Instrument
	Current    instruments.Version
	Settlement DerivativeAssetView
	// LatestMark is nil when the contract has never been marked.
	LatestMark *derivatives.Mark
	// Settled is nil until the contract has been finally settled.
	Settled *DerivativeSettlementRecord
}

// DerivativePositionView is a position with what a reader needs to read its
// figures, and the service's valuation of it.
type DerivativePositionView struct {
	Position   derivatives.Position
	Symbol     string
	Kind       instruments.InstrumentType
	PriceScale int32
	Settlement DerivativeAssetView
	// Computed is false when this answer did not value the position (a trade
	// result). When it is true, an OPEN position carries exactly one of
	// Valuation (the service valued the account's book) and Refusal (it could
	// not, and said why).
	Computed  bool
	Valuation *derivatives.PositionRisk
	Refusal   *errs.Error
}

// DerivativePositionPageView is a page of positions and the valuation of the
// account's whole open book: exactly one of Book and BookRefusal is set.
type DerivativePositionPageView struct {
	Items       []DerivativePositionView
	Next        string
	Book        *derivatives.RiskContribution
	Quote       DerivativeAssetView
	BookRefusal *errs.Error
}

// DerivativeFlowFilter narrows the flow register.
type DerivativeFlowFilter struct {
	PositionID *derivatives.PositionID
	// Kind "" is every kind.
	Kind derivatives.CashFlowKind
}

// DerivativeFlowView is one row of the clearing-house flow register.
type DerivativeFlowView struct {
	Flow       derivatives.CashFlow
	Symbol     string
	Settlement DerivativeAssetView
}

// DerivativeAccrualView is one position's funding for one interval, with the
// rate the interval was charged at.
type DerivativeAccrualView struct {
	ID             derivatives.AccrualID
	FundingRateID  derivatives.FundingRateID
	PositionID     derivatives.PositionID
	InstrumentID   instruments.InstrumentID
	Symbol         string
	IntervalStart  time.Time
	IntervalEnd    time.Time
	Quantity       money.Quantity
	Direction      derivatives.FundingDirection
	Amount         money.Quantity
	RateE8         int64
	PremiumIndexE8 int64
	Clamped        bool
	PerContract    money.Quantity
	MarkMantissa   money.Quantity
	PriceScale     int32
	FlowID         *derivatives.CashFlowID
	Settlement     DerivativeAssetView
}

// DerivativeAnalyticsView is an option's reference analytics with the mark
// and the request inputs the model used.
type DerivativeAnalyticsView struct {
	Analytics derivatives.Analytics
	Mark      derivatives.Mark
	Inputs    derivatives.AnalyticsRequest
}

// DerivativeTradeView is the outcome of an open, close or exercise.
type DerivativeTradeView struct {
	Outcome derivatives.Outcome
	// Position is the outcome's position, made readable (never valued).
	Position *DerivativePositionView
}

// DerivativeOpenCommand opens or adds to a position.
type DerivativeOpenCommand struct {
	AccountID      accounts.AccountID
	InstrumentID   instruments.InstrumentID
	Side           derivatives.Side
	Quantity       money.Quantity
	Mode           gates.Mode
	IdempotencyKey string
	CorrelationID  string
}

// DerivativeReduceCommand closes (or exercises) contracts of one position.
// AccountID is the position's own account, already scoped to the caller.
type DerivativeReduceCommand struct {
	PositionID     derivatives.PositionID
	AccountID      accounts.AccountID
	Quantity       *money.Quantity
	Mode           gates.Mode
	IdempotencyKey string
	CorrelationID  string
}

// DerivativePriceInput is a price exactly as the client wrote it: a mantissa
// at a scale. The service rescales it, exactly or not at all.
type DerivativePriceInput struct {
	Mantissa money.Quantity
	Scale    int32
}

// DerivativeMarkCommand records a sandbox mark.
type DerivativeMarkCommand struct {
	InstrumentID instruments.InstrumentID
	Mark         DerivativePriceInput
	Index        *DerivativePriceInput
	Source       string
	// ObservedAt zero means now.
	ObservedAt time.Time
}

// DerivativeRunCommand names one interval of one contract.
type DerivativeRunCommand struct {
	InstrumentID  instruments.InstrumentID
	IntervalStart time.Time
	CorrelationID string
}

// DerivativeRunView is what a variation-margin or funding run did.
type DerivativeRunView struct {
	InstrumentID instruments.InstrumentID
	Result       derivatives.RunResult
	Settlement   DerivativeAssetView
}

// DerivativeSettleCommand settles one expired contract.
type DerivativeSettleCommand struct {
	InstrumentID  instruments.InstrumentID
	Price         *DerivativePriceInput
	PriceSource   string
	CorrelationID string
}

// DerivativeSettlementView is a final settlement.
type DerivativeSettlementView struct {
	InstrumentID instruments.InstrumentID
	Result       derivatives.SettlementResult
	PriceScale   int32
	Settlement   DerivativeAssetView
}

// DerivativeClearingFundCommand capitalizes the clearing house. A zero
// AssetID is the sandbox dollar. IdempotencyKey is the operator's own key.
type DerivativeClearingFundCommand struct {
	AssetID        assets.AssetID
	Quantity       money.Quantity
	IdempotencyKey string
}

// DerivativeClearingFundView is what funding the clearing house posted.
type DerivativeClearingFundView struct {
	Asset    DerivativeAssetView
	Quantity money.Quantity
	Journals []ledger.TransactionID
}

// DerivativeClearingReportView is the CLEARING reconciliation of one asset.
type DerivativeClearingReportView struct {
	Report derivatives.ClearingReport
	Asset  DerivativeAssetView
}
