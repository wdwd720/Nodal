package reconciliation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func TestPolicy_DefaultIsValid(t *testing.T) {
	t.Parallel()
	require.NoError(t, DefaultPolicy().Validate())
	assert.Error(t, Policy{}.Validate(), "the zero policy is not usable")
}

func TestPolicy_ValidateRejectsBadValues(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.ProvenAbsentMargin = 0
	err := p.Validate()
	require.Error(t, err, "a zero absence margin would call a transaction absent while it can still land")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	p = DefaultPolicy()
	p.MaterialThresholdUSDMinor = -1
	assert.Error(t, p.Validate())

	p = DefaultPolicy()
	p.Version = ""
	assert.Error(t, p.Validate())

	p = DefaultPolicy()
	p.MaxUnresolvedAge = -time.Second
	assert.Error(t, p.Validate())
}

func TestPolicy_DustThresholds(t *testing.T) {
	t.Parallel()
	sol := assets.NewAssetID()
	usdc := assets.NewAssetID()
	p := DefaultPolicy()
	p.DustQuantity[sol] = q(5000) // 5000 lamports of rent noise

	assert.True(t, p.IsDust(sol, q(4999)))
	assert.True(t, p.IsDust(sol, q(-5000)), "dust is absolute")
	assert.False(t, p.IsDust(sol, q(5001)))

	// An asset with no entry uses the default, which is zero: nothing but an
	// exact match is dust.
	assert.True(t, p.IsDust(usdc, q(0)))
	assert.False(t, p.IsDust(usdc, q(1)))
}

func TestPolicy_Material(t *testing.T) {
	t.Parallel()
	asset := assets.NewAssetID()
	p := DefaultPolicy()
	p.MaterialThresholdUSDMinor = 100 // $1.00
	p.DustQuantity[asset] = q(10)

	oneDollar := money.USDFromMinor(100)
	ninetyNineCents := money.USDFromMinor(99)
	minusDollar := money.USDFromMinor(-100)

	assert.True(t, p.Material(KindWalletBalance, asset, q(1_000_000), &oneDollar))
	assert.False(t, p.Material(KindWalletBalance, asset, q(990_000), &ninetyNineCents))
	assert.True(t, p.Material(KindWalletBalance, asset, q(-1_000_000), &minusDollar), "materiality is absolute")

	// No valuation: fall back to dust, and fail closed above it.
	assert.False(t, p.Material(KindWalletBalance, asset, q(10), nil))
	assert.True(t, p.Material(KindWalletBalance, asset, q(11), nil))

	// A zero difference is never material.
	assert.False(t, p.Material(KindWalletBalance, asset, q(0), nil))

	// Internal drift and unknown submissions are material whatever the size.
	assert.True(t, p.Material(KindLedgerInternal, asset, q(0), &ninetyNineCents))
	assert.True(t, p.Material(KindPositionLedger, asset, q(1), nil))
	assert.True(t, p.Material(KindSubmissionUnknown, asset, q(0), nil))
}
