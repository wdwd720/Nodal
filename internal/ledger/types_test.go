package ledger

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
)

// TestCode_Chart pins the chart of accounts of FINANCIAL_MODEL §2.1, with the
// one documented deviation: DEFICIT is CREDIT-normal (see CodeDeficit).
func TestCode_Chart(t *testing.T) {
	t.Parallel()
	want := []struct {
		code   Code
		owner  OwnerType
		normal Side
		neg    bool
	}{
		{CodeWallet, OwnerCustomer, Debit, false},
		{CodeCapital, OwnerCustomer, Credit, false},
		{CodeTradingOutflow, OwnerCustomer, Debit, false},
		{CodeTradingInflow, OwnerCustomer, Credit, false},
		{CodeFeesNetwork, OwnerCustomer, Debit, false},
		{CodeFeesVenue, OwnerCustomer, Debit, false},
		{CodeFeesPlatform, OwnerCustomer, Debit, false},
		{CodeDeficit, OwnerCustomer, Credit, false},
		{CodeReconciliationAdjustment, OwnerCustomer, Credit, true},
		{CodePlatformFeeReceivable, OwnerPlatform, Debit, false},
		{CodePlatformFeeRevenue, OwnerPlatform, Credit, false},
		{CodePlatformAdjustment, OwnerPlatform, Credit, true},

		// Nodal-native economy (migration 00710).
		{CodeCreditBalance, OwnerCustomer, Debit, false},
		{CodeCreditIssuance, OwnerCustomer, Credit, false},
		{CodeNativeAssetBalance, OwnerCustomer, Debit, false},
		{CodeNativeTradingOutflow, OwnerCustomer, Debit, false},
		{CodeNativeTradingInflow, OwnerCustomer, Credit, false},
		{CodeCreditFees, OwnerCustomer, Debit, false},
		{CodePayoutReserved, OwnerCustomer, Debit, false},
		{CodeMarketReserve, OwnerPlatform, Debit, false},
		{CodeMarketInventory, OwnerPlatform, Debit, false},
		{CodePlatformCreditRevenue, OwnerPlatform, Credit, false},
		{CodePayoutClearing, OwnerPlatform, Credit, false},
		{CodePayoutSettled, OwnerPlatform, Debit, false},
	}
	require.Len(t, want, 24)
	require.Len(t, AllCodes(), 24)
	for i, w := range want {
		assert.Equal(t, w.code, AllCodes()[i], "chart order")
		assert.True(t, w.code.Valid(), "%s valid", w.code)
		assert.Equal(t, w.owner, w.code.OwnerType(), "%s owner", w.code)
		assert.Equal(t, w.normal, w.code.NormalSide(), "%s normal side", w.code)
		assert.Equal(t, w.neg, w.code.AllowsNegative(), "%s allow negative", w.code)
		assert.True(t, w.normal.Valid())
	}
	bogus := Code("BOGUS")
	assert.False(t, bogus.Valid())
	assert.Equal(t, Side(""), bogus.NormalSide())
	assert.False(t, bogus.AllowsNegative())
	assert.Equal(t, OwnerType(""), bogus.OwnerType())

	codes := AllCodes()
	codes[0] = "MUTATED"
	assert.Equal(t, CodeWallet, AllCodes()[0], "AllCodes returns a copy")
}

func TestSide(t *testing.T) {
	t.Parallel()
	assert.True(t, Debit.Valid())
	assert.True(t, Credit.Valid())
	assert.False(t, Side("").Valid())
	assert.False(t, Side("debit").Valid())
	assert.Equal(t, Credit, Debit.Opposite())
	assert.Equal(t, Debit, Credit.Opposite())
}

func TestKind(t *testing.T) {
	t.Parallel()
	require.Len(t, AllKinds(), 19)
	for _, k := range AllKinds() {
		assert.True(t, k.Valid(), "%s", k)
	}
	assert.False(t, Kind("").Valid())
	assert.False(t, Kind("trade_fill").Valid())

	assert.True(t, KindCompensation.RequiresReversalOf())
	assert.True(t, KindCorrection.RequiresReversalOf())
	assert.False(t, KindReconciliationAdjustment.RequiresReversalOf())
	assert.False(t, KindFundingReversal.RequiresReversalOf())

	assert.True(t, KindCompensation.RequiresReasonCode())
	assert.True(t, KindCorrection.RequiresReasonCode())
	assert.True(t, KindReconciliationAdjustment.RequiresReasonCode())
	assert.False(t, KindTradeFill.RequiresReasonCode())
}

func TestAccountRef_Validate(t *testing.T) {
	t.Parallel()
	upper := strings.ToUpper(testAccount.String())
	cases := []struct {
		name string
		ref  AccountRef
		ok   bool
	}{
		{"customer wallet", CustomerAccount(testAccount, CodeWallet, testUSDC), true},
		{"platform revenue", PlatformAccount(CodePlatformFeeRevenue, testUSDC), true},
		{"uppercase owner id accepted", AccountRef{OwnerCustomer, upper, CodeWallet, testUSDC}, true},
		{"unknown owner type", AccountRef{"BANK", testAccount.String(), CodeWallet, testUSDC}, false},
		{"unknown code", AccountRef{OwnerCustomer, testAccount.String(), "PIGGY_BANK", testUSDC}, false},
		{"platform code on customer", AccountRef{OwnerCustomer, testAccount.String(), CodePlatformFeeRevenue, testUSDC}, false},
		{"customer code on platform", AccountRef{OwnerPlatform, PlatformOwnerID, CodeWallet, testUSDC}, false},
		{"zero asset", AccountRef{OwnerCustomer, testAccount.String(), CodeWallet, assets.AssetID{}}, false},
		{"platform with other owner id", AccountRef{OwnerPlatform, testAccount.String(), CodePlatformFeeRevenue, testUSDC}, false},
		{"customer with platform owner id", AccountRef{OwnerCustomer, PlatformOwnerID, CodeWallet, testUSDC}, false},
		{"customer with garbage owner id", AccountRef{OwnerCustomer, "not-a-uuid", CodeWallet, testUSDC}, false},
		{"customer with nil uuid", AccountRef{OwnerCustomer, "00000000-0000-0000-0000-000000000000", CodeWallet, testUSDC}, false},
		{"customer with empty owner id", AccountRef{OwnerCustomer, "", CodeWallet, testUSDC}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.ref.Validate()
			if c.ok {
				require.NoError(t, err)
				return
			}
			requireCode(t, err, errs.CodeValidationFailed)
		})
	}
}

func TestAccountRef_KeyAndString(t *testing.T) {
	t.Parallel()
	a := CustomerAccount(testAccount, CodeWallet, testUSDC)
	b := AccountRef{OwnerCustomer, "  " + strings.ToUpper(testAccount.String()) + " ", CodeWallet, testUSDC}
	assert.Equal(t, a.key(), b.key(), "key is case- and whitespace-insensitive")
	assert.NotEqual(t, a.key(), CustomerAccount(testAccount, CodeCapital, testUSDC).key())
	assert.NotEqual(t, a.key(), CustomerAccount(testAccount, CodeWallet, testSOL).key())
	assert.Equal(t, "CUSTOMER/"+testAccount.String()+"/WALLET/"+testUSDC.String(), a.String())
	assert.Equal(t, PlatformOwnerID, PlatformAccount(CodePlatformAdjustment, testSOL).OwnerID)
}

func TestPosting_ReasonCode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", Posting{}.ReasonCode())
	assert.Equal(t, "", Posting{Metadata: map[string]any{MetadataReasonCode: 7}}.ReasonCode())
	assert.Equal(t, "DUST", Posting{Metadata: map[string]any{MetadataReasonCode: "DUST"}}.ReasonCode())
}
