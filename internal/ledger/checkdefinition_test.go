package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
)

// checkDefinition is the service's own guard: a stored ledger account whose
// normal side or negative-balance policy disagrees with the chart of accounts
// is refused rather than used.
//
// It used to be covered by an integration test that forged the disagreement
// with an UPDATE through the migration role. Since migration 00717 the database
// refuses that write (F-49), so the guard can no longer be reached from a
// database — which is the right outcome and leaves the Go check with no
// integration coverage. This builds the disagreeing value directly.
//
// The guard is kept rather than deleted because it defends against a database
// whose constraint is missing: an older snapshot, a restore from before 00717,
// or a deployment somebody has edited.
func TestCheckDefinition_RefusesARowThatDisagreesWithTheChart(t *testing.T) {
	t.Parallel()
	ref := PlatformAccount(CodeMarketReserve, assets.NewAssetID())
	require.False(t, ref.Code.AllowsNegative(), "this case needs a code the chart forbids going negative")

	honest := LedgerAccount{Ref: ref, NormalSide: ref.Code.NormalSide(), AllowNegative: false}
	_, err := checkDefinition(honest)
	assert.NoError(t, err, "the account the chart describes must be usable")

	exempt := honest
	exempt.AllowNegative = true
	_, err = checkDefinition(exempt)
	assert.Error(t, err, "an account claiming an exemption the chart does not give must be refused")

	flipped := honest
	if flipped.NormalSide == Debit {
		flipped.NormalSide = Credit
	} else {
		flipped.NormalSide = Debit
	}
	_, err = checkDefinition(flipped)
	assert.Error(t, err, "an account whose normal side disagrees with its code must be refused")
}
