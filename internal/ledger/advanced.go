package ledger

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Advanced financial capabilities (docs/architecture/ADVANCED_FINANCIAL_MODEL.md;
// ADR-0020).
//
// Every code and kind declared in this file exists for a capability that runs
// in PAPER, SANDBOX or DEVNET mode only. Three rules make that a property of
// the books rather than of the callers:
//
//  1. Every simulated-only kind (IsSimulatedOnly) may touch only ledger
//     accounts in value domain SIMULATED. The application refuses anything
//     else before inserting, and migration 00731 refuses it again at COMMIT.
//  2. HOUSE and FACILITY accounts can only ever exist in value domain
//     SIMULATED (a CHECK on ledger_accounts), so the house book and every
//     platform-operated facility -- stable issuer, lending pool, insurance
//     fund, clearing house, bridge, prediction market -- cannot hold real
//     value in this schema version at all.
//  3. Every simulated-only posting balances per party, not only per asset:
//     for each (owner_type, owner_id, asset) the debits equal the credits.
//     A party's books therefore never gain value that the same party's books
//     do not explain, and value moving between parties is always an explicit
//     pair such as TRANSFER_OUT / TRANSFER_IN.

// Additional owner types.
const (
	// OwnerHouse is a house (principal / treasury) book. owner_id is the
	// house_accounts row. House and customer value never share an account:
	// they are different owner types with disjoint codes.
	OwnerHouse OwnerType = "HOUSE"
	// OwnerFacility is a platform-operated economic facility with segregated
	// books. owner_id is the facilities row, and the facility's kind decides
	// which codes it may hold.
	OwnerFacility OwnerType = "FACILITY"
)

// AllOwnerTypes returns every ledger owner type in declaration order.
func AllOwnerTypes() []OwnerType {
	return []OwnerType{OwnerCustomer, OwnerPlatform, OwnerHouse, OwnerFacility}
}

// FacilityKind is the kind of a platform-operated facility. A FACILITY ledger
// account's code must belong to the facility's kind; the database checks the
// pair when the account is created.
type FacilityKind string

// Facility kinds.
const (
	FacilityStableIssuer     FacilityKind = "STABLE_ISSUER"
	FacilityLendingPool      FacilityKind = "LENDING_POOL"
	FacilityInsuranceFund    FacilityKind = "INSURANCE_FUND"
	FacilityClearingHouse    FacilityKind = "CLEARING_HOUSE"
	FacilityBridge           FacilityKind = "BRIDGE"
	FacilityPredictionMarket FacilityKind = "PREDICTION_MARKET"
)

var allFacilityKinds = []FacilityKind{
	FacilityStableIssuer, FacilityLendingPool, FacilityInsuranceFund,
	FacilityClearingHouse, FacilityBridge, FacilityPredictionMarket,
}

// AllFacilityKinds returns every facility kind in declaration order (a copy).
func AllFacilityKinds() []FacilityKind { return append([]FacilityKind(nil), allFacilityKinds...) }

// Valid reports whether k is a declared facility kind.
func (k FacilityKind) Valid() bool {
	for _, x := range allFacilityKinds {
		if x == k {
			return true
		}
	}
	return false
}

// Customer codes for the advanced capabilities.
const (
	// CodeTransferOut is value a customer sent to another customer.
	// DEBIT-normal, never negative.
	CodeTransferOut Code = "TRANSFER_OUT"
	// CodeTransferIn is value a customer received from another customer.
	// CREDIT-normal, never negative.
	CodeTransferIn Code = "TRANSFER_IN"
	// CodeFeesSpread is the disclosed spread a customer paid. DEBIT-normal.
	CodeFeesSpread Code = "FEES_SPREAD"
	// CodeFeesPerformance is a crystallized performance fee. DEBIT-normal.
	CodeFeesPerformance Code = "FEES_PERFORMANCE"
	// CodeMarginCollateral is collateral posted to a margin account: still
	// the customer's, no longer freely spendable. DEBIT-normal.
	CodeMarginCollateral Code = "MARGIN_COLLATERAL"
	// CodeMarginLoan is borrowed principal plus capitalized interest.
	// CREDIT-normal, never negative.
	CodeMarginLoan Code = "MARGIN_LOAN"
	// CodeInterestExpense is margin interest accrued. DEBIT-normal.
	CodeInterestExpense Code = "INTEREST_EXPENSE"
	// CodeDerivativesMargin is initial margin locked for derivative
	// positions. DEBIT-normal.
	CodeDerivativesMargin Code = "DERIVATIVES_MARGIN"
	// CodeRealizedPnL is realized profit (credit) and loss (debit) from
	// variation margin, funding, option settlement and liquidation.
	// CREDIT-normal and bidirectional: a customer can be net down.
	CodeRealizedPnL Code = "REALIZED_PNL"
	// CodeOptionPremiumPaid is premium paid for long options still open.
	// DEBIT-normal.
	CodeOptionPremiumPaid Code = "OPTION_PREMIUM_PAID"
	// CodeOptionPremiumReceived is premium received for options written and
	// still open. CREDIT-normal.
	CodeOptionPremiumReceived Code = "OPTION_PREMIUM_RECEIVED"
	// CodeUnsettledReceivable is equity trade proceeds or shares bought that
	// have traded but not settled. DEBIT-normal.
	CodeUnsettledReceivable Code = "UNSETTLED_RECEIVABLE"
	// CodeBridgeInFlight is value whose source leg of a bridge is final and
	// whose destination leg is not. DEBIT-normal.
	CodeBridgeInFlight Code = "BRIDGE_IN_FLIGHT"
	// CodeCorporateActions offsets share and cash adjustments from splits,
	// reverse splits and cash-in-lieu. CREDIT-normal and bidirectional.
	CodeCorporateActions Code = "CORPORATE_ACTIONS"
	// CodeDividends is dividend income received. CREDIT-normal.
	CodeDividends Code = "DIVIDENDS"
	// CodeWithdrawn is value that left the customer to an external
	// (simulated) destination. DEBIT-normal.
	CodeWithdrawn Code = "WITHDRAWN"
)

// Platform codes for the advanced capabilities.
const (
	// CodeSpreadRevenue is disclosed spread revenue, booked separately from
	// explicit fee revenue. CREDIT-normal.
	CodeSpreadRevenue Code = "SPREAD_REVENUE"
	// CodePerformanceFeeAccrued is performance fees accrued but not yet
	// crystallized. DEBIT-normal.
	CodePerformanceFeeAccrued Code = "PERFORMANCE_FEE_ACCRUED"
	// CodePerformanceFeeDeferred is the deferred-revenue offset of an
	// accrual. CREDIT-normal.
	CodePerformanceFeeDeferred Code = "PERFORMANCE_FEE_DEFERRED"
	// CodePerformanceFeeRevenue is crystallized performance fee revenue.
	// CREDIT-normal.
	CodePerformanceFeeRevenue Code = "PERFORMANCE_FEE_REVENUE"
)

// House (principal / treasury) codes.
const (
	CodeHouseInventory      Code = "HOUSE_INVENTORY"
	CodeHouseCapital        Code = "HOUSE_CAPITAL"
	CodeHouseTradingOutflow Code = "HOUSE_TRADING_OUTFLOW"
	CodeHouseTradingInflow  Code = "HOUSE_TRADING_INFLOW"
	CodeHouseFees           Code = "HOUSE_FEES"
)

// Facility codes, grouped by the facility kind that may hold them.
const (
	// Stable issuer.
	CodeStableReserve        Code = "STABLE_RESERVE"
	CodeStableReserveFunding Code = "STABLE_RESERVE_FUNDING"
	CodeStableIssued         Code = "STABLE_ISSUED"
	CodeStableOutstanding    Code = "STABLE_OUTSTANDING"

	// Lending pool.
	CodePoolCash       Code = "POOL_CASH"
	CodePoolCapital    Code = "POOL_CAPITAL"
	CodeLoanReceivable Code = "LOAN_RECEIVABLE"
	CodeInterestIncome Code = "INTEREST_INCOME"

	// Insurance fund.
	CodeInsuranceFundCash    Code = "INSURANCE_FUND_CASH"
	CodeInsuranceFundCapital Code = "INSURANCE_FUND_CAPITAL"
	CodeDeficitReceivable    Code = "DEFICIT_RECEIVABLE"

	// Clearing house (derivatives).
	CodeClearingCash    Code = "CLEARING_CASH"
	CodeClearingCapital Code = "CLEARING_CAPITAL"
	CodeClearingResult  Code = "CLEARING_RESULT"

	// Bridge.
	CodeBridgeLocked      Code = "BRIDGE_LOCKED"
	CodeBridgeLockFunding Code = "BRIDGE_LOCK_FUNDING"
	CodeBridgeMinted      Code = "BRIDGE_MINTED"
	CodeBridgeOutstanding Code = "BRIDGE_OUTSTANDING"

	// Prediction market.
	CodePredictionEscrow            Code = "PREDICTION_ESCROW"
	CodePredictionCollateral        Code = "PREDICTION_COLLATERAL"
	CodePredictionSharesIssued      Code = "PREDICTION_SHARES_ISSUED"
	CodePredictionSharesOutstanding Code = "PREDICTION_SHARES_OUTSTANDING"
)

// advancedCodes is the advanced half of the chart, in chart order. It is merged
// into codeRegistry and allCodes by types.go so the chart stays one table.
var advancedCodes = []struct {
	code     Code
	info     codeInfo
	facility FacilityKind
}{
	{CodeTransferOut, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeTransferIn, codeInfo{OwnerCustomer, Credit, false}, ""},
	{CodeFeesSpread, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeFeesPerformance, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeMarginCollateral, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeMarginLoan, codeInfo{OwnerCustomer, Credit, false}, ""},
	{CodeInterestExpense, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeDerivativesMargin, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeRealizedPnL, codeInfo{OwnerCustomer, Credit, true}, ""},
	{CodeOptionPremiumPaid, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeOptionPremiumReceived, codeInfo{OwnerCustomer, Credit, false}, ""},
	{CodeUnsettledReceivable, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeBridgeInFlight, codeInfo{OwnerCustomer, Debit, false}, ""},
	{CodeCorporateActions, codeInfo{OwnerCustomer, Credit, true}, ""},
	{CodeDividends, codeInfo{OwnerCustomer, Credit, false}, ""},
	{CodeWithdrawn, codeInfo{OwnerCustomer, Debit, false}, ""},

	{CodeSpreadRevenue, codeInfo{OwnerPlatform, Credit, false}, ""},
	{CodePerformanceFeeAccrued, codeInfo{OwnerPlatform, Debit, false}, ""},
	{CodePerformanceFeeDeferred, codeInfo{OwnerPlatform, Credit, false}, ""},
	{CodePerformanceFeeRevenue, codeInfo{OwnerPlatform, Credit, false}, ""},

	{CodeHouseInventory, codeInfo{OwnerHouse, Debit, false}, ""},
	{CodeHouseCapital, codeInfo{OwnerHouse, Credit, false}, ""},
	{CodeHouseTradingOutflow, codeInfo{OwnerHouse, Debit, false}, ""},
	{CodeHouseTradingInflow, codeInfo{OwnerHouse, Credit, false}, ""},
	{CodeHouseFees, codeInfo{OwnerHouse, Debit, false}, ""},

	{CodeStableReserve, codeInfo{OwnerFacility, Debit, false}, FacilityStableIssuer},
	{CodeStableReserveFunding, codeInfo{OwnerFacility, Credit, false}, FacilityStableIssuer},
	{CodeStableIssued, codeInfo{OwnerFacility, Debit, false}, FacilityStableIssuer},
	{CodeStableOutstanding, codeInfo{OwnerFacility, Credit, false}, FacilityStableIssuer},

	{CodePoolCash, codeInfo{OwnerFacility, Debit, false}, FacilityLendingPool},
	{CodePoolCapital, codeInfo{OwnerFacility, Credit, false}, FacilityLendingPool},
	{CodeLoanReceivable, codeInfo{OwnerFacility, Debit, false}, FacilityLendingPool},
	{CodeInterestIncome, codeInfo{OwnerFacility, Credit, false}, FacilityLendingPool},

	{CodeInsuranceFundCash, codeInfo{OwnerFacility, Debit, false}, FacilityInsuranceFund},
	{CodeInsuranceFundCapital, codeInfo{OwnerFacility, Credit, false}, FacilityInsuranceFund},
	{CodeDeficitReceivable, codeInfo{OwnerFacility, Debit, false}, FacilityInsuranceFund},

	{CodeClearingCash, codeInfo{OwnerFacility, Debit, false}, FacilityClearingHouse},
	{CodeClearingCapital, codeInfo{OwnerFacility, Credit, false}, FacilityClearingHouse},
	{CodeClearingResult, codeInfo{OwnerFacility, Credit, true}, FacilityClearingHouse},

	{CodeBridgeLocked, codeInfo{OwnerFacility, Debit, false}, FacilityBridge},
	{CodeBridgeLockFunding, codeInfo{OwnerFacility, Credit, false}, FacilityBridge},
	{CodeBridgeMinted, codeInfo{OwnerFacility, Debit, false}, FacilityBridge},
	{CodeBridgeOutstanding, codeInfo{OwnerFacility, Credit, false}, FacilityBridge},

	{CodePredictionEscrow, codeInfo{OwnerFacility, Debit, false}, FacilityPredictionMarket},
	{CodePredictionCollateral, codeInfo{OwnerFacility, Credit, false}, FacilityPredictionMarket},
	{CodePredictionSharesIssued, codeInfo{OwnerFacility, Debit, false}, FacilityPredictionMarket},
	{CodePredictionSharesOutstanding, codeInfo{OwnerFacility, Credit, false}, FacilityPredictionMarket},
}

// facilityOfCode maps every FACILITY code to the facility kind that may hold
// it. It is derived from advancedCodes and never mutated after init.
var facilityOfCode = func() map[Code]FacilityKind {
	m := make(map[Code]FacilityKind)
	for _, c := range advancedCodes {
		if c.facility != "" {
			m[c.code] = c.facility
		}
	}
	return m
}()

// FacilityKind returns the facility kind that may hold a FACILITY code, or
// the empty kind for every other code.
func (c Code) FacilityKind() FacilityKind { return facilityOfCode[c] }

// Advanced journal kinds. Every one of them is simulated-only.
const (
	KindSandboxFunding                Kind = "SANDBOX_FUNDING"
	KindFacilityFunding               Kind = "FACILITY_FUNDING"
	KindHouseFunding                  Kind = "HOUSE_FUNDING"
	KindP2PTransfer                   Kind = "P2P_TRANSFER"
	KindSimTrade                      Kind = "SIM_TRADE"
	KindEquitySettlement              Kind = "EQUITY_SETTLEMENT"
	KindCorporateAction               Kind = "CORPORATE_ACTION"
	KindMarginCollateralMove          Kind = "MARGIN_COLLATERAL_MOVE"
	KindMarginBorrow                  Kind = "MARGIN_BORROW"
	KindMarginRepay                   Kind = "MARGIN_REPAY"
	KindMarginInterest                Kind = "MARGIN_INTEREST"
	KindLiquidation                   Kind = "LIQUIDATION"
	KindDeficitCoverage               Kind = "DEFICIT_COVERAGE"
	KindDerivativeMarginMove          Kind = "DERIVATIVE_MARGIN_MOVE"
	KindVariationMargin               Kind = "VARIATION_MARGIN"
	KindFundingPayment                Kind = "FUNDING_PAYMENT"
	KindOptionPremium                 Kind = "OPTION_PREMIUM"
	KindOptionSettlement              Kind = "OPTION_SETTLEMENT"
	KindFutureSettlement              Kind = "FUTURE_SETTLEMENT"
	KindStableIssuance                Kind = "STABLE_ISSUANCE"
	KindStableRedemption              Kind = "STABLE_REDEMPTION"
	KindBridgeSourceFinalized         Kind = "BRIDGE_SOURCE_FINALIZED"
	KindBridgeDestinationFinalized    Kind = "BRIDGE_DESTINATION_FINALIZED"
	KindBridgeRefund                  Kind = "BRIDGE_REFUND"
	KindPredictionSetIssuance         Kind = "PREDICTION_SET_ISSUANCE"
	KindPredictionSetRedemption       Kind = "PREDICTION_SET_REDEMPTION"
	KindPredictionPayout              Kind = "PREDICTION_PAYOUT"
	KindPredictionRefund              Kind = "PREDICTION_REFUND"
	KindPerformanceFeeAccrual         Kind = "PERFORMANCE_FEE_ACCRUAL"
	KindPerformanceFeeCrystallization Kind = "PERFORMANCE_FEE_CRYSTALLIZATION"
	KindSandboxWithdrawal             Kind = "SANDBOX_WITHDRAWAL"
)

var advancedKinds = []Kind{
	KindSandboxFunding, KindFacilityFunding, KindHouseFunding, KindP2PTransfer, KindSimTrade,
	KindEquitySettlement, KindCorporateAction,
	KindMarginCollateralMove, KindMarginBorrow, KindMarginRepay, KindMarginInterest,
	KindLiquidation, KindDeficitCoverage,
	KindDerivativeMarginMove, KindVariationMargin, KindFundingPayment,
	KindOptionPremium, KindOptionSettlement, KindFutureSettlement,
	KindStableIssuance, KindStableRedemption,
	KindBridgeSourceFinalized, KindBridgeDestinationFinalized, KindBridgeRefund,
	KindPredictionSetIssuance, KindPredictionSetRedemption, KindPredictionPayout, KindPredictionRefund,
	KindPerformanceFeeAccrual, KindPerformanceFeeCrystallization,
	KindSandboxWithdrawal,
}

// AdvancedKinds returns every simulated-only kind in declaration order (a copy).
func AdvancedKinds() []Kind { return append([]Kind(nil), advancedKinds...) }

// IsSimulatedOnly reports whether k may touch only SIMULATED-domain accounts
// and must balance per party.
func (k Kind) IsSimulatedOnly() bool {
	for _, x := range advancedKinds {
		if x == k {
			return true
		}
	}
	return false
}

// partyNet is one party's per-asset totals inside a posting.
type partyNet struct {
	debits  money.Quantity
	credits money.Quantity
}

// partyKey identifies one party's holding of one asset.
type partyKey struct {
	owner OwnerType
	id    string
	asset string
}

// checkPerPartyBalance is rule 3 of this file: a simulated-only posting must
// balance for every (owner_type, owner_id, asset), so no party's books gain
// value their own entries do not explain.
func checkPerPartyBalance(p Posting) error {
	if !p.Kind.IsSimulatedOnly() {
		return nil
	}
	acc := make(map[partyKey]*partyNet, len(p.Entries))
	order := make([]partyKey, 0, len(p.Entries))
	for _, e := range p.Entries {
		r := e.Account.normalized()
		k := partyKey{r.OwnerType, r.OwnerID, r.AssetID.String()}
		pn, ok := acc[k]
		if !ok {
			pn = &partyNet{}
			acc[k] = pn
			order = append(order, k)
		}
		switch e.Side {
		case Debit:
			pn.debits = pn.debits.Add(e.Quantity)
		case Credit:
			pn.credits = pn.credits.Add(e.Quantity)
		}
	}
	for _, k := range order {
		pn := acc[k]
		if pn.debits.Cmp(pn.credits) != 0 {
			return errs.Newf(errs.CodeLedgerUnbalanced,
				"%s postings must balance per party: %s %s asset %s has debits %s and credits %s",
				p.Kind, k.owner, k.id, k.asset, pn.debits, pn.credits).
				WithField("owner_type", string(k.owner)).
				WithField("owner_id", k.id).
				WithField("asset_id", k.asset)
		}
	}
	return nil
}
