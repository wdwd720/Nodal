package capacity

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// TestAudit_AdmitAmountOverflowsInsteadOfRefusing reproduced
// F-credits-payments-9, and is inverted here into the regression for F-159.
//
// AdmitAmount computed `after := r.AtRiskMinor + amountMinor` in int64 and
// refused only when `after > MaxAtRiskMinor`. The sum was unchecked, so an
// amount near math.MaxInt64 wrapped negative and the ceiling admitted it: the
// guard that exists to answer "what could this deployment owe after the action"
// answered with a negative number.
//
// It was inert only because internal/credit calls the guard BEFORE pricing and
// PricingPolicy.CreditsFor then refused the amount for exceeding
// MaxAmountMinor. The guard itself did not refuse it, and the order of those
// two calls was the only thing between that arithmetic and a ceiling that
// could be stepped over.
//
// So this test now asks the guard, rather than restating its arithmetic: the
// defect was reproducible in local variables precisely because nothing
// exercised the guard with an amount like this.
func TestAudit_AdmitAmountRefusesAnAmountThatWouldOverflowTheSum(t *testing.T) {
	t.Parallel()
	const ceiling = int64(200_000) // the blueprint's $2,000
	atRisk := int64(199_000)

	// The old arithmetic, kept so the shape of the defect stays on the record.
	wrapped := atRisk + math.MaxInt64
	require.Negative(t, wrapped, "the sum wrapped")
	assert.False(t, wrapped > ceiling,
		"which is why the ceiling check passed and an amount larger than every dollar on earth was admitted")

	g, err := NewGuard(Budget{MaxAtRiskMinor: ceiling}, at("2026-09-10T12:00:00Z"))
	require.NoError(t, err)
	q := &stubQuerier{atRisk: atRisk}

	_, err = g.AdmitAmount(context.Background(), q, ActionCreditPurchase, math.MaxInt64)
	require.Error(t, err, "an amount that overflows the sum must be refused, not admitted")
	assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err))
	assert.ErrorIs(t, err, ErrAtCapacity)

	// And the ordinary cases still decide the ordinary way, on headroom.
	_, err = g.AdmitAmount(context.Background(), q, ActionCreditPurchase, 1_000)
	require.NoError(t, err, "exactly the headroom fits")
	_, err = g.AdmitAmount(context.Background(), q, ActionCreditPurchase, 1_001)
	require.Error(t, err, "one minor unit past the headroom does not")
}

// A guard measuring MORE exposure than its own ceiling refuses everything,
// including a zero amount, rather than computing a negative headroom that some
// amount could satisfy.
func TestAudit_AGuardAlreadyPastItsCeilingRefusesEveryAmount(t *testing.T) {
	t.Parallel()
	g, err := NewGuard(Budget{MaxAtRiskMinor: 200_000}, at("2026-09-10T12:00:00Z"))
	require.NoError(t, err)
	q := &stubQuerier{atRisk: 250_000}

	for _, amount := range []int64{0, 1, math.MaxInt64} {
		_, aerr := g.AdmitAmount(context.Background(), q, ActionCreditPurchase, amount)
		require.Error(t, aerr, "amount %d", amount)
		assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(aerr))
	}
}
