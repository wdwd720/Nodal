package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// The portfolio (advanced goal §26) is one read of everything an account
// holds in the SIMULATED domain. Most of it is other subsystems' own reads,
// which the handler composes exactly as their own routes answer them.
// PortfolioPort serves only what no subsystem's read answers on its own: the
// account's balances asset by asset, with what is reserved of them and why
// and what each is worth in the sandbox dollar, and what stands in the way of
// new risk on the account.

// PortfolioPort reads an account's portfolio snapshot.
type PortfolioPort interface {
	// Snapshot reads, from one consistent database snapshot, the account's
	// SIMULATED balances, the unresolved reconciliation records and the
	// economic effects of UNKNOWN outcome on it, and the prediction markets
	// it holds shares in or is owed a held payout by. It changes nothing.
	// NOT_FOUND when the account does not exist.
	Snapshot(ctx context.Context, account accounts.AccountID) (PortfolioSnapshot, error)
}

// PortfolioSnapshot is what one PortfolioPort.Snapshot read.
type PortfolioSnapshot struct {
	AccountID     accounts.AccountID
	AccountStatus accounts.Status
	AsOf          time.Time
	// Quote is the sandbox dollar every value is stated in. When this
	// deployment has not provisioned it, QuoteProvisioned is false, Quote
	// carries only its well-known identity and nothing is valued.
	Quote            assets.Asset
	QuoteProvisioned bool
	// MaxMarkAge is how old a mark may be and still value an asset: the bound
	// buying power V2 uses.
	MaxMarkAge time.Duration
	// Balances are every SIMULATED asset the account holds, by symbol, then
	// chain, then asset id.
	Balances []PortfolioBalance
	// BlocksNewRisk is reconciliation's own answer for the account.
	BlocksNewRisk bool
	// Reconciliation holds the unresolved records on the account, oldest
	// first, at most portfolioListBound of them.
	Reconciliation          []reconciliation.Record
	ReconciliationTruncated bool
	// UnknownEffects holds the account's economic effects whose outcome is
	// UNKNOWN, oldest first, at most portfolioListBound of them.
	UnknownEffects          []econ.Effect
	UnknownEffectsTruncated bool
	// PredictionMarkets are the markets the account holds outcome shares in or
	// is owed an unclaimed held payout by, in id order, at most
	// portfolioListBound of them.
	PredictionMarkets          []predmarket.MarketID
	PredictionMarketsTruncated bool
}

// PortfolioBalance is one SIMULATED asset an account holds, every quantity in
// base units of the asset.
type PortfolioBalance struct {
	Asset assets.Asset
	// Wallet is the WALLET ledger balance.
	Wallet money.Quantity
	// Reserved, WithdrawalHolds and Available are the capital service's
	// availability of the asset (capital.Service.Availability): what ACTIVE
	// reservations hold back, what withdrawal holds keep, and the wallet less
	// both, floored at zero.
	Reserved        money.Quantity
	WithdrawalHolds money.Quantity
	Available       money.Quantity
	// Reservations break Reserved down by purpose; they sum to it.
	Reservations []PortfolioReservation
	// Elsewhere are the non-zero balances held outside the wallet
	// (portfolioElsewhereCodes), Owed the non-zero balances owed
	// (portfolioOwedCodes).
	Elsewhere []PortfolioLedgerBalance
	Owed      []PortfolioLedgerBalance
	// Total is Wallet plus every Elsewhere balance.
	Total     money.Quantity
	Valuation PortfolioValuation
}

// PortfolioPurpose is what a capital reservation is for.
type PortfolioPurpose string

// Reservation purposes, in the order a balance lists them.
const (
	PortfolioPurposeOpenOrders       PortfolioPurpose = "OPEN_ORDERS"
	PortfolioPurposePendingTransfers PortfolioPurpose = "PENDING_TRANSFERS"
	PortfolioPurposeBridgeLocks      PortfolioPurpose = "BRIDGE_LOCKS"
	PortfolioPurposeStrategy         PortfolioPurpose = "STRATEGY"
	PortfolioPurposeWithdrawals      PortfolioPurpose = "WITHDRAWALS"
	PortfolioPurposeOther            PortfolioPurpose = "OTHER"
)

// PortfolioReservation is the ACTIVE reservations of one asset that serve one
// purpose under one capability.
type PortfolioReservation struct {
	Purpose PortfolioPurpose
	// Capability is the capability of the economic intent the reservations
	// serve; empty for a trade intent and for a reservation that names none.
	Capability string
	// Quantity is what they still hold back: reserved less consumed.
	Quantity money.Quantity
	Count    int
}

// PortfolioLedgerBalance is one non-zero ledger balance of an asset.
type PortfolioLedgerBalance struct {
	Code     ledger.Code
	Quantity money.Quantity
}

// PortfolioValuationStatus says how an asset is valued in the sandbox dollar.
type PortfolioValuationStatus string

// Valuation statuses.
const (
	// PortfolioValuationPar is the sandbox dollar itself, at face.
	PortfolioValuationPar PortfolioValuationStatus = "PAR"
	// PortfolioValuationMarked is valued at a mark no older than MaxMarkAge.
	PortfolioValuationMarked PortfolioValuationStatus = "MARKED"
	// PortfolioValuationUnvalued has no such mark, or nothing to value in.
	PortfolioValuationUnvalued PortfolioValuationStatus = "UNVALUED"
)

// PortfolioValuation is one asset's value in base units of the sandbox
// dollar.
type PortfolioValuation struct {
	Status PortfolioValuationStatus
	// Mark is the mark a MARKED asset is valued at.
	Mark *money.Price
	// Value is the asset's Total at the mark (or at par), rounded down; nil
	// when UNVALUED.
	Value *money.Quantity
	// OwedValue is what is owed in the asset at the same mark, rounded up; nil
	// when nothing is owed or the asset is UNVALUED.
	OwedValue *money.Quantity
	// Problem says why an asset is UNVALUED.
	Problem *errs.Error
}
