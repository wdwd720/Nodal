package credit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capacity"
)

// The money-at-risk ceiling in internal/capacity sums paid_amount_minor over a
// list of funding states written as SQL. That list has to be a statement about
// THIS state machine, and nothing in either package makes it one: the query
// parses whatever states it is given, and returns a smaller number for the ones
// it forgot.
//
// That is not a hypothetical. The list shipped naming four states and omitting
// CAPTURED -- money already taken from a payer and one transition away from
// minting Credit -- so the ceiling measured less exposure than existed and
// admitted purchases past a cap that exists to bound what a failure can cost.
//
// This is the test that makes the two agree. It lives here rather than in
// internal/capacity because internal/credit imports internal/capacity, so only
// this direction can see both.

// terminalOrSettled is the classification the capacity ceiling is the
// complement of. Settlement is left out of "at risk" deliberately: it is the
// point at which this system stops treating money as reversible, and counting
// it would make the ceiling a lifetime total that can only rise.
func terminalOrSettled(s FundingState) bool { return s.Terminal() || s == FundingSettled }

// TestFundingStates_TheCapacityCeilingClassifiesEveryState.
//
// Every state in the machine is either counted as money at risk or is one of
// the decided ones. A state that is neither is a state whose money the ceiling
// cannot see.
func TestFundingStates_TheCapacityCeilingClassifiesEveryState(t *testing.T) {
	t.Parallel()

	atRisk := map[string]bool{}
	for _, s := range capacity.AtRiskFundingStates() {
		require.False(t, atRisk[s], "%s is named twice in the at-risk list", s)
		atRisk[s] = true
	}
	require.NotEmpty(t, atRisk, "a money-at-risk ceiling that sums over no states measures nothing")

	for _, s := range AllFundingStates() {
		name := string(s)
		if terminalOrSettled(s) {
			assert.False(t, atRisk[name],
				"%s is decided -- money already returned or no longer reversible -- and counting it "+
					"makes the ceiling a lifetime total that eventually refuses every purchase", name)
			continue
		}
		assert.True(t, atRisk[name],
			"%s is a live funding state and the money-at-risk ceiling does not count it, so the "+
				"ceiling admits purchases against exposure it cannot see. Add it to "+
				"atRiskFundingStates in internal/capacity, or make it terminal here", name)
	}
}

// TestFundingStates_TheCapacityCeilingNamesNoStateThisMachineLacks catches the
// other direction: a state misspelled in the capacity list matches no row, and
// the query still runs.
func TestFundingStates_TheCapacityCeilingNamesNoStateThisMachineLacks(t *testing.T) {
	t.Parallel()

	for _, s := range capacity.AtRiskFundingStates() {
		assert.True(t, FundingState(s).Valid(),
			"the money-at-risk ceiling counts %q, which is not a funding state; the sum silently "+
				"excludes whatever it was meant to be", s)
	}
}

// TestFundingStates_CapturedMoneyIsAtRisk is the regression, named.
//
// CAPTURED means the money has left the payer and Credit has not been minted
// yet. If any single state has to be in that sum, it is this one.
func TestFundingStates_CapturedMoneyIsAtRisk(t *testing.T) {
	t.Parallel()

	atRisk := map[string]bool{}
	for _, s := range capacity.AtRiskFundingStates() {
		atRisk[s] = true
	}
	for _, s := range []FundingState{FundingCaptured, FundingReversible, FundingDisputed, FundingManualReview} {
		assert.True(t, atRisk[string(s)], "%s holds real money and must count against the ceiling", s)
	}
}
