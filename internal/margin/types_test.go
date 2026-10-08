package margin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTransitions_TablesAreStrict(t *testing.T) {
	for _, s := range AllAccountStatuses() {
		assert.False(t, CanTransitionAccount(s, s), "%s self transition", s)
	}
	for _, to := range AllAccountStatuses() {
		assert.False(t, CanTransitionAccount(StatusClosed, to), "CLOSED is terminal")
	}
	assert.False(t, CanTransitionAccount(StatusBankrupt, StatusNormal), "a bankrupt account does not quietly recover")
	assert.False(t, CanTransitionAccount(StatusMarginCall, StatusBankrupt), "bankruptcy comes out of a liquidation")
	assert.True(t, CanTransitionAccount(StatusLiquidating, StatusBankrupt))

	for _, s := range AllCaseStatuses() {
		assert.False(t, CanTransitionCase(s, s))
		if s.Terminal() {
			for _, to := range AllCaseStatuses() {
				assert.False(t, CanTransitionCase(s, to), "terminal %s -> %s", s, to)
			}
		}
	}
	for _, b := range BirthCaseStatuses() {
		assert.False(t, b.Terminal(), "a case is born open")
	}
	assert.True(t, CanTransitionCall(CallOpen, CallMet))
	assert.True(t, CanTransitionCall(CallOpen, CallLiquidated))
	assert.False(t, CanTransitionCall(CallMet, CallOpen))
	assert.False(t, CanTransitionCall(CallLiquidated, CallMet))
}

// TestTransitions_ManualReviewLeavesOnlyThroughAResolution (F1): MANUAL_REVIEW
// is not a dead end. It leaves for IN_PROGRESS (resumed) or BANKRUPT
// (declared) and for nothing else, and every exit needs a resolution;
// automation never moves it, and it carries closed_at like a finished case.
func TestTransitions_ManualReviewLeavesOnlyThroughAResolution(t *testing.T) {
	for _, to := range AllCaseStatuses() {
		want := to == CaseInProgress || to == CaseBankrupt
		assert.Equal(t, want, CanTransitionCase(CaseManualReview, to), "MANUAL_REVIEW -> %s", to)
	}
	for _, s := range AllCaseStatuses() {
		assert.Equal(t, s == CaseManualReview, ResolutionRequired(s), "%s", s)
	}
	assert.False(t, CaseManualReview.Terminal())
	assert.False(t, CaseManualReview.Automated())
	assert.True(t, CaseManualReview.Closed())
	for _, s := range []CaseStatus{CaseAwaitingPrice, CaseAwaitingVenue, CaseInProgress} {
		assert.True(t, s.Automated())
		assert.False(t, s.Closed())
	}
	for _, o := range AllResolutionOutcomes() {
		assert.True(t, CanTransitionCase(CaseManualReview, o.CaseStatus()), "%s", o)
	}
	assert.Equal(t, CaseInProgress, ResolveResume.CaseStatus())
	assert.Equal(t, CaseBankrupt, ResolveDeclareBankrupt.CaseStatus())
	assert.True(t, ActionSell.Trading())
	assert.False(t, ActionApplyCollateral.Trading(), "applying collateral is terminal accounting")
	assert.False(t, ActionSweepWallet.Trading(), "sweeping the wallet is terminal accounting")
}

func TestStatuses_RiskPermissions(t *testing.T) {
	assert.True(t, StatusNormal.AllowsNewRisk())
	assert.True(t, StatusWarning.AllowsNewRisk())
	for _, s := range []AccountStatus{StatusMarginCall, StatusLiquidating, StatusBankrupt, StatusClosed} {
		assert.False(t, s.AllowsNewRisk(), s)
	}
	assert.True(t, StatusLiquidating.AllowsRiskReduction(), "a customer may top up during a liquidation")
	assert.False(t, StatusBankrupt.AllowsRiskReduction())
	assert.Equal(t, []string{"PAPER", "SANDBOX"}, RunModes(), "margin never runs LIVE or DEVNET")
}

func TestRanks_AreTotalOrders(t *testing.T) {
	for i, c := range AllVolatilityClasses() {
		assert.Equal(t, i, c.Rank())
	}
	for i, c := range AllLiquidityClasses() {
		assert.Equal(t, i, c.Rank())
	}
	for i, c := range AllVenueTiers() {
		assert.Equal(t, i, c.Rank())
	}
	assert.Equal(t, -1, VolatilityClass("WILD").Rank())
}
