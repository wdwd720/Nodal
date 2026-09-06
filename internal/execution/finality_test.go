package execution

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalityPolicy_DefaultAndRequired(t *testing.T) {
	t.Parallel()
	p := DefaultFinalityPolicy()
	require.NoError(t, p.Validate())
	require.Equal(t, FinalityObserved, p.Required(FinalityForUIProvisional))
	require.Equal(t, FinalityConfirmed, p.Required(FinalityForPositionProvisional))
	require.Equal(t, FinalityConfirmed, p.Required(FinalityForLedgerPosting))
	require.Equal(t, FinalityFinalized, p.Required(FinalityForFundingAvailability))
	require.Equal(t, FinalityFinalized, p.Required(FinalityForWithdrawal))
	require.Equal(t, FinalityFinalized, p.Required("SOMETHING_NEW"), "unknown classes need the strongest evidence")
	require.Len(t, AllFinalityActionClasses(), 5)
}

func TestFinalityPolicy_Satisfies(t *testing.T) {
	t.Parallel()
	p := DefaultFinalityPolicy()
	levels := AllFinalityLevels()
	for i, observed := range levels {
		for j, required := range levels {
			require.Equal(t, i >= j, p.Satisfies(observed, required), "%s satisfies %s", observed, required)
		}
	}
	require.False(t, p.Satisfies("SUCCESS", FinalityObserved), "levels are never collapsed into SUCCESS")
	require.False(t, p.Satisfies(FinalityConfirmed, "WHATEVER"))
	require.True(t, p.Satisfies(FinalityFinalized, "WHATEVER"), "only FINALIZED satisfies an unknown requirement")
	require.Equal(t, FinalityFinalized, Stronger(FinalityObserved, FinalityFinalized))
	require.Equal(t, 0, FinalityLevel("").Rank())
}

func TestFinalityPolicy_ValidateMonotonic(t *testing.T) {
	t.Parallel()
	p := DefaultFinalityPolicy()
	p.LedgerPosting = FinalityObserved
	require.Error(t, p.Validate(), "ledger posting cannot be weaker than provisional positions")
	p = DefaultFinalityPolicy()
	p.Withdrawal = FinalityObserved
	require.Error(t, p.Validate())
	p = DefaultFinalityPolicy()
	p.UIProvisional = "NOW"
	require.Error(t, p.Validate())
	p = DefaultFinalityPolicy()
	p.Version = ""
	require.Error(t, p.Validate())
}
