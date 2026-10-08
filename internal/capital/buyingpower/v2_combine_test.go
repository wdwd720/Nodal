package buyingpower

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
)

type fixedMargin struct {
	st  V2MarginState
	ok  bool
	err error
}

func (f fixedMargin) MarginState(context.Context, db.Querier, string, string) (V2MarginState, bool, error) {
	return f.st, f.ok, f.err
}

func TestCombinedMarginReader_SumsAndKeepsTheStrictestRate(t *testing.T) {
	m := money.USDFromMinor
	margin := fixedMargin{ok: true, st: V2MarginState{Mode: "CROSS", BorrowLiabilities: m(200_000), AccruedInterest: m(15),
		InitialMarginUsed: m(40_000), MaintenanceMargin: m(20_000), InitialMarginRateBPS: 2_000}}
	derivs := fixedMargin{ok: true, st: V2MarginState{Mode: "ISOLATED", UnrealizedPnL: m(-5_000),
		InitialMarginUsed: m(10_000), MaintenanceMargin: m(6_000), InitialMarginRateBPS: 3_334}}
	cash := fixedMargin{ok: false}

	st, ok, err := CombinedMarginReader{margin, cash, derivs}.MarginState(context.Background(), nil, "CUSTOMER", "x")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "CROSS", st.Mode)
	assert.Equal(t, m(200_000), st.BorrowLiabilities)
	assert.Equal(t, m(15), st.AccruedInterest)
	assert.Equal(t, m(-5_000), st.UnrealizedPnL)
	assert.Equal(t, m(50_000), st.InitialMarginUsed, "requirements add up")
	assert.Equal(t, m(26_000), st.MaintenanceMargin)
	assert.Equal(t, money.BPS(3_334), st.InitialMarginRateBPS, "the strictest rate wins")

	_, ok, err = CombinedMarginReader{cash, cash}.MarginState(context.Background(), nil, "CUSTOMER", "x")
	require.NoError(t, err)
	assert.False(t, ok, "no part has a margin state: a cash account")

	boom := errors.New("part failed")
	_, _, err = CombinedMarginReader{margin, fixedMargin{err: boom}}.MarginState(context.Background(), nil, "CUSTOMER", "x")
	assert.ErrorIs(t, err, boom, "a part that fails fails the read")
}
