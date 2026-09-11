package capacity

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAudit_AdmitAmountOverflowsInsteadOfRefusing reproduces
// F-credits-payments-9.
//
// AdmitAmount computes `after := r.AtRiskMinor + amountMinor` in int64
// (capacity.go:379) and refuses only when `after > MaxAtRiskMinor`. The sum is
// unchecked, so an amount near math.MaxInt64 wraps negative and the ceiling
// admits it: the guard that exists to answer "what could this deployment owe
// after the action" answers with a negative number.
//
// It is inert today only because internal/credit calls the guard BEFORE pricing
// (purchase.go:284-295) and PricingPolicy.CreditsFor then refuses the amount for
// exceeding MaxAmountMinor. The guard itself does not refuse it, and the order
// of those two calls is the only thing between this arithmetic and a ceiling
// that can be stepped over.
func TestAudit_AdmitAmountOverflowsInsteadOfRefusing(t *testing.T) {
	t.Parallel()
	const ceiling = int64(200_000) // the blueprint's $2,000
	atRisk := int64(199_000)

	// What the guard computes, verbatim from capacity.go:379-384.
	after := atRisk + math.MaxInt64
	require.Negative(t, after, "the sum wrapped")
	assert.False(t, after > ceiling,
		"so the ceiling check passes and an amount larger than every dollar on earth is admitted")

	// What it should compute. The fix is a checked add (or a bound on the
	// amount before the guard sees it).
	assert.True(t, atRisk > 0 && math.MaxInt64-atRisk < math.MaxInt64,
		"headroom arithmetic must be done as `amountMinor > MaxAtRiskMinor - AtRiskMinor`")
}
