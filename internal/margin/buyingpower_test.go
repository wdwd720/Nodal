package margin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

func TestBuyingPowerReader_ConvertsConservatively(t *testing.T) {
	// 6-decimal numeraire: 1.234567 units owed rounds up to 1.24, a gain of
	// 0.999999 rounds down to 0.99, a loss of 0.000001 rounds down to -0.01.
	e := Evaluation{
		Mode: ModeCross, NumeraireDecimals: 6, LeverageCapBPS: 30_000,
		BorrowLiabilities: money.QuantityFromInt64(1_234_567),
		AccruedInterest:   money.QuantityFromInt64(1),
		UnrealizedPnL:     money.QuantityFromInt64(999_999),
		InitialMargin:     money.QuantityFromInt64(2_000_001),
		MaintenanceMargin: money.QuantityFromInt64(1_000_000),
	}
	st, ok, err := v2State(e)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "CROSS", st.Mode)
	assert.Equal(t, "1.24", st.BorrowLiabilities.String(), "a liability rounds up")
	assert.Equal(t, "0.01", st.AccruedInterest.String(), "a single base unit of interest is not dropped")
	assert.Equal(t, "0.99", st.UnrealizedPnL.String(), "a gain rounds down")
	assert.Equal(t, "2.01", st.InitialMarginUsed.String())
	assert.Equal(t, "1.00", st.MaintenanceMargin.String())
	assert.Equal(t, money.BPS(3_334), st.InitialMarginRateBPS, "a 3x cap needs a third, rounded up")

	e.UnrealizedPnL = money.QuantityFromInt64(-1)
	st, _, err = v2State(e)
	require.NoError(t, err)
	assert.Equal(t, "-0.01", st.UnrealizedPnL.String(), "a loss rounds away from the account's favour")
}

func TestBuyingPowerReader_InitialRateFromLeverageCap(t *testing.T) {
	assert.Equal(t, money.OneHundredPercent, initialRateFromLeverageCap(0), "no leverage: the whole exposure is margin")
	assert.Equal(t, money.OneHundredPercent, initialRateFromLeverageCap(10_000))
	assert.Equal(t, money.BPS(5_000), initialRateFromLeverageCap(20_000))
	assert.Equal(t, money.BPS(2_000), initialRateFromLeverageCap(50_000))
	assert.Equal(t, money.BPS(3_334), initialRateFromLeverageCap(30_000))
}
