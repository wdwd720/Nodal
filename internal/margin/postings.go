package margin

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// The posting templates of ADVANCED_FINANCIAL_MODEL §5 this package uses.
// Each builder returns a posting that balances per asset and per party, and
// uses only a simulated-only journal kind; the ledger refuses anything else
// before inserting and the database refuses it again at COMMIT (SM001,
// SM002).

// PostingMeta is what every posting carries besides its entries: the
// ledger idempotency key, the financial event reference and the times.
type PostingMeta struct {
	Key           string
	RefType       string
	RefID         string
	At            time.Time
	CorrelationID string
	Description   string
}

func (m PostingMeta) posting(kind ledger.Kind, entries []ledger.Entry) ledger.Posting {
	return ledger.Posting{
		Kind: kind, IdempotencyKey: m.Key,
		Reference:   ledger.FinancialEventReference{Type: m.RefType, ID: m.RefID},
		EffectiveAt: m.At, Description: m.Description, CorrelationID: m.CorrelationID, Entries: entries,
	}
}

func dr(ref ledger.AccountRef, q money.Quantity) ledger.Entry {
	return ledger.Entry{Account: ref, Side: ledger.Debit, Quantity: q}
}

func cr(ref ledger.AccountRef, q money.Quantity) ledger.Entry {
	return ledger.Entry{Account: ref, Side: ledger.Credit, Quantity: q}
}

func positive(q money.Quantity, what string) error {
	if !q.IsPositive() {
		return errs.Newf(errs.CodeValidationFailed, "margin: %s must be positive", what)
	}
	return nil
}

// CollateralMovePosting is MARGIN_COLLATERAL_MOVE: posting is Dr
// MARGIN_COLLATERAL / Cr WALLET; a release is the reverse.
func CollateralMovePosting(m PostingMeta, acct accounts.AccountID, asset assets.AssetID, qty money.Quantity, dir MoveDirection) (ledger.Posting, error) {
	if err := positive(qty, "a collateral move"); err != nil {
		return ledger.Posting{}, err
	}
	coll := ledger.CustomerAccount(acct, ledger.CodeMarginCollateral, asset)
	wallet := ledger.CustomerAccount(acct, ledger.CodeWallet, asset)
	switch dir {
	case MovePost:
		return m.posting(ledger.KindMarginCollateralMove, []ledger.Entry{dr(coll, qty), cr(wallet, qty)}), nil
	case MoveRelease:
		return m.posting(ledger.KindMarginCollateralMove, []ledger.Entry{dr(wallet, qty), cr(coll, qty)}), nil
	}
	return ledger.Posting{}, errs.Newf(errs.CodeValidationFailed, "margin: unknown collateral move direction %q", dir)
}

// BorrowPosting is MARGIN_BORROW: C: Dr WALLET / Cr MARGIN_LOAN; F(pool): Dr
// LOAN_RECEIVABLE / Cr POOL_CASH.
func BorrowPosting(m PostingMeta, acct accounts.AccountID, pool econ.Facility, asset assets.AssetID, qty money.Quantity) (ledger.Posting, error) {
	if err := positive(qty, "a borrow"); err != nil {
		return ledger.Posting{}, err
	}
	return m.posting(ledger.KindMarginBorrow, []ledger.Entry{
		dr(ledger.CustomerAccount(acct, ledger.CodeWallet, asset), qty),
		cr(ledger.CustomerAccount(acct, ledger.CodeMarginLoan, asset), qty),
		dr(pool.Account(ledger.CodeLoanReceivable, asset), qty),
		cr(pool.Account(ledger.CodePoolCash, asset), qty),
	}), nil
}

// RepayPosting is MARGIN_REPAY: C: Dr MARGIN_LOAN / Cr WALLET; F(pool): Dr
// POOL_CASH / Cr LOAN_RECEIVABLE.
func RepayPosting(m PostingMeta, acct accounts.AccountID, pool econ.Facility, asset assets.AssetID, qty money.Quantity) (ledger.Posting, error) {
	if err := positive(qty, "a repayment"); err != nil {
		return ledger.Posting{}, err
	}
	return m.posting(ledger.KindMarginRepay, []ledger.Entry{
		dr(ledger.CustomerAccount(acct, ledger.CodeMarginLoan, asset), qty),
		cr(ledger.CustomerAccount(acct, ledger.CodeWallet, asset), qty),
		dr(pool.Account(ledger.CodePoolCash, asset), qty),
		cr(pool.Account(ledger.CodeLoanReceivable, asset), qty),
	}), nil
}

// InterestPosting is MARGIN_INTEREST: C: Dr INTEREST_EXPENSE / Cr
// MARGIN_LOAN; F(pool): Dr LOAN_RECEIVABLE / Cr INTEREST_INCOME. Interest is
// capitalized into the loan.
func InterestPosting(m PostingMeta, acct accounts.AccountID, pool econ.Facility, asset assets.AssetID, qty money.Quantity) (ledger.Posting, error) {
	if err := positive(qty, "an interest accrual"); err != nil {
		return ledger.Posting{}, err
	}
	return m.posting(ledger.KindMarginInterest, []ledger.Entry{
		dr(ledger.CustomerAccount(acct, ledger.CodeInterestExpense, asset), qty),
		cr(ledger.CustomerAccount(acct, ledger.CodeMarginLoan, asset), qty),
		dr(pool.Account(ledger.CodeLoanReceivable, asset), qty),
		cr(pool.Account(ledger.CodeInterestIncome, asset), qty),
	}), nil
}

// SpotBuyPosting is SIM_TRADE for a paper buy: the buyer gives cost of the
// quote asset and receives qty of the base asset, each leg balanced within
// the buyer's own books.
func SpotBuyPosting(m PostingMeta, acct accounts.AccountID, quote assets.AssetID, cost money.Quantity, base assets.AssetID, qty money.Quantity) (ledger.Posting, error) {
	if err := positive(cost, "a trade cost"); err != nil {
		return ledger.Posting{}, err
	}
	if err := positive(qty, "a trade quantity"); err != nil {
		return ledger.Posting{}, err
	}
	return m.posting(ledger.KindSimTrade, []ledger.Entry{
		cr(ledger.CustomerAccount(acct, ledger.CodeWallet, quote), cost),
		dr(ledger.CustomerAccount(acct, ledger.CodeTradingOutflow, quote), cost),
		dr(ledger.CustomerAccount(acct, ledger.CodeWallet, base), qty),
		cr(ledger.CustomerAccount(acct, ledger.CodeTradingInflow, base), qty),
	}), nil
}

// LiquidationPosting is LIQUIDATION: the forced, reduce-only sale posted like
// a SIM_TRADE. The sold quantity leaves MARGIN_COLLATERAL (where the
// position is held) for TRADING_OUTFLOW, the proceeds arrive in WALLET from
// TRADING_INFLOW, and the liquidation fee leaves WALLET as FEES_PLATFORM and
// arrives in the insurance fund (Dr INSURANCE_FUND_CASH / Cr
// INSURANCE_FUND_CAPITAL). The repayment and any excess are their own
// MARGIN_REPAY and MARGIN_COLLATERAL_MOVE postings.
func LiquidationPosting(m PostingMeta, acct accounts.AccountID, fund econ.Facility, base assets.AssetID, qty money.Quantity,
	quote assets.AssetID, proceeds, fee money.Quantity) (ledger.Posting, error) {
	if err := positive(qty, "a liquidation quantity"); err != nil {
		return ledger.Posting{}, err
	}
	if proceeds.IsNegative() || fee.IsNegative() || fee.Cmp(proceeds) > 0 {
		return ledger.Posting{}, errs.New(errs.CodeValidationFailed, "margin: a liquidation fee is a part of the proceeds")
	}
	entries := []ledger.Entry{
		cr(ledger.CustomerAccount(acct, ledger.CodeMarginCollateral, base), qty),
		dr(ledger.CustomerAccount(acct, ledger.CodeTradingOutflow, base), qty),
	}
	if proceeds.IsPositive() {
		entries = append(entries,
			dr(ledger.CustomerAccount(acct, ledger.CodeWallet, quote), proceeds),
			cr(ledger.CustomerAccount(acct, ledger.CodeTradingInflow, quote), proceeds))
	}
	if fee.IsPositive() {
		entries = append(entries,
			dr(ledger.CustomerAccount(acct, ledger.CodeFeesPlatform, quote), fee),
			cr(ledger.CustomerAccount(acct, ledger.CodeWallet, quote), fee),
			dr(fund.Account(ledger.CodeInsuranceFundCash, quote), fee),
			cr(fund.Account(ledger.CodeInsuranceFundCapital, quote), fee))
	}
	return m.posting(ledger.KindLiquidation, entries), nil
}

// DeficitCoveragePosting is DEFICIT_COVERAGE: C: Dr MARGIN_LOAN / Cr DEFICIT;
// F(pool): Dr POOL_CASH / Cr LOAN_RECEIVABLE; F(insurance): Dr
// DEFICIT_RECEIVABLE / Cr INSURANCE_FUND_CASH. The pool is made whole, the
// fund pays and holds the claim, the customer owes the deficit.
func DeficitCoveragePosting(m PostingMeta, acct accounts.AccountID, pool, fund econ.Facility, asset assets.AssetID, qty money.Quantity) (ledger.Posting, error) {
	if err := positive(qty, "a deficit"); err != nil {
		return ledger.Posting{}, err
	}
	return m.posting(ledger.KindDeficitCoverage, []ledger.Entry{
		dr(ledger.CustomerAccount(acct, ledger.CodeMarginLoan, asset), qty),
		cr(ledger.CustomerAccount(acct, ledger.CodeDeficit, asset), qty),
		dr(pool.Account(ledger.CodePoolCash, asset), qty),
		cr(pool.Account(ledger.CodeLoanReceivable, asset), qty),
		dr(fund.Account(ledger.CodeDeficitReceivable, asset), qty),
		cr(fund.Account(ledger.CodeInsuranceFundCash, asset), qty),
	}), nil
}

// FacilityFundingPosting is FACILITY_FUNDING: F: Dr <cash code> / Cr
// <capital code> -- POOL_CASH / POOL_CAPITAL for the lending pool,
// INSURANCE_FUND_CASH / INSURANCE_FUND_CAPITAL for the insurance fund.
func FacilityFundingPosting(m PostingMeta, f econ.Facility, asset assets.AssetID, qty money.Quantity) (ledger.Posting, error) {
	if err := positive(qty, "a facility funding"); err != nil {
		return ledger.Posting{}, err
	}
	var cash, capital ledger.Code
	switch f.Kind {
	case ledger.FacilityLendingPool:
		cash, capital = ledger.CodePoolCash, ledger.CodePoolCapital
	case ledger.FacilityInsuranceFund:
		cash, capital = ledger.CodeInsuranceFundCash, ledger.CodeInsuranceFundCapital
	default:
		return ledger.Posting{}, errs.Newf(errs.CodeValidationFailed, "margin: a %s facility is not funded here", f.Kind)
	}
	return m.posting(ledger.KindFacilityFunding, []ledger.Entry{dr(f.Account(cash, asset), qty), cr(f.Account(capital, asset), qty)}), nil
}
