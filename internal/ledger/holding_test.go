package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notHolding is every code that is not a holding, with the reason. Together
// with holdingCodes it must cover the whole chart: a new code is classified
// on purpose or this test fails.
var notHolding = map[Code]string{
	CodeCapital:                     "source of funded value",
	CodeTradingOutflow:              "flow: value given in a trade",
	CodeTradingInflow:               "source: value received in a trade",
	CodeFeesNetwork:                 "flow: fee paid",
	CodeFeesVenue:                   "flow: fee paid",
	CodeFeesPlatform:                "flow: fee paid (the platform's PLATFORM_FEE_RECEIVABLE holds it)",
	CodeDeficit:                     "liability",
	CodeReconciliationAdjustment:    "adjustment source",
	CodePlatformFeeRevenue:          "revenue",
	CodePlatformAdjustment:          "adjustment source",
	CodeCreditIssuance:              "source of issued credits",
	CodePayoutClearing:              "clearing liability",
	CodePayoutSettled:               "flow: value paid out of the ledger",
	CodeTransferOut:                 "flow: value sent",
	CodeTransferIn:                  "source: value received",
	CodeFeesSpread:                  "flow: spread paid",
	CodeFeesPerformance:             "flow: performance fee paid",
	CodeMarginLoan:                  "liability",
	CodeInterestExpense:             "flow: interest charged",
	CodeRealizedPnL:                 "result",
	CodeOptionPremiumPaid:           "flow: premium paid",
	CodeOptionPremiumReceived:       "source: premium received",
	CodeBridgeInFlight:              "mirror: the bridge facility holds the units as BRIDGE_LOCKED",
	CodeCorporateActions:            "source/sink of corporate action adjustments",
	CodeDividends:                   "source: dividend income",
	CodeWithdrawn:                   "flow: value withdrawn from the sandbox",
	CodeSpreadRevenue:               "revenue",
	CodePerformanceFeeAccrued:       "accrual: nothing collected yet",
	CodePerformanceFeeDeferred:      "accrual",
	CodePerformanceFeeRevenue:       "revenue",
	CodeHouseCapital:                "source of house value",
	CodeHouseTradingOutflow:         "flow: value given in a trade",
	CodeHouseTradingInflow:          "source: value received in a trade",
	CodeHouseFees:                   "flow: fees paid",
	CodeStableReserveFunding:        "source of the reserve",
	CodeStableIssued:                "mirror of outstanding supply, held by the holders",
	CodeStableOutstanding:           "liability: supply outstanding",
	CodePoolCapital:                 "source of the pool's value",
	CodeLoanReceivable:              "claim: the borrower holds the lent value",
	CodeInterestIncome:              "revenue",
	CodeInsuranceFundCapital:        "source of the fund's value",
	CodeDeficitReceivable:           "claim: nothing held until repaid",
	CodeClearingCapital:             "source of the clearing house's value",
	CodeClearingResult:              "result",
	CodeBridgeLockFunding:           "source of the locked value",
	CodeBridgeMinted:                "mirror of the twins outstanding, held by the holders",
	CodeBridgeOutstanding:           "liability: twins outstanding",
	CodePredictionCollateral:        "source of the escrowed collateral",
	CodePredictionSharesIssued:      "mirror of shares outstanding, held by the holders",
	CodePredictionSharesOutstanding: "liability: shares outstanding",
}

func TestHoldingCodes_EveryCodeIsClassifiedOnPurpose(t *testing.T) {
	for _, c := range AllCodes() {
		_, not := notHolding[c]
		require.True(t, c.HoldsValue() != not, "%s must be exactly one of holding or not holding", c)
	}
	for c := range notHolding {
		assert.True(t, c.Valid(), "%s is not in the chart", c)
	}
	assert.Len(t, HoldingCodes(), len(holdingCodes))
}

func TestHoldingCodes_AHoldingIsAlwaysDebitNormalAndNeverNegative(t *testing.T) {
	for _, c := range HoldingCodes() {
		assert.Equal(t, Debit, c.NormalSide(), "%s", c)
		assert.False(t, c.AllowsNegative(), "%s", c)
	}
}
