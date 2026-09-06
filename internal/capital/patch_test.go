package capital

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func ptrUSD(minor int64) *money.USD { u := money.USDFromMinor(minor); return &u }
func ptrStr(s string) *string       { return &s }
func ptrI32(n int32) *int32         { return &n }
func ptrList(s ...string) *[]string { return &s }
func ptrTime(t time.Time) *time.Time {
	return &t
}

func problemsOf(t *testing.T, err error) []string {
	t.Helper()
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok, "%T", err)
	require.Equal(t, errs.CodeValidationFailed, e.Code)
	p, _ := e.Fields["problems"].([]string)
	return p
}

func TestEnvelopeAuthorityPatch_Validate(t *testing.T) {
	cases := []struct {
		name  string
		patch EnvelopeAuthorityPatch
		want  []string
	}{
		{"empty patch", EnvelopeAuthorityPatch{Reason: "x"}, []string{"patch names no field"}},
		{"missing reason", EnvelopeAuthorityPatch{Allocation: ptrUSD(1)}, []string{"reason required"}},
		{"negative allocation", EnvelopeAuthorityPatch{Allocation: ptrUSD(-1), Reason: "x"}, []string{"allocation must not be negative"}},
		{
			"negative limits",
			EnvelopeAuthorityPatch{MaxDailyLoss: ptrUSD(-1), MaxDrawdown: ptrUSD(-1), MaxSingleTrade: ptrUSD(-1), MaxPosition: ptrUSD(-1), MaxModelSpend: ptrUSD(-1), MaxDataSpend: ptrUSD(-1), MaxOrderRatePerHour: ptrI32(-1), Reason: "x"},
			[]string{"max_daily_loss must not be negative", "max_drawdown must not be negative", "max_single_trade must not be negative", "max_position must not be negative", "max_model_spend must not be negative", "max_data_spend must not be negative", "max_order_rate_per_hour must not be negative"},
		},
		{"bad instrument id", EnvelopeAuthorityPatch{AllowedInstruments: ptrList("SOL"), Reason: "x"}, []string{"allowed_instruments entries must be canonical uuids"}},
		{"blank venue", EnvelopeAuthorityPatch{AllowedVenues: ptrList("jupiter", " "), Reason: "x"}, []string{"allowed_venues entries must be non-blank"}},
		{"blank asset class", EnvelopeAuthorityPatch{AllowedAssetClasses: ptrList(""), Reason: "x"}, []string{"allowed_asset_classes entries must be non-blank"}},
		{"blank policy version", EnvelopeAuthorityPatch{PolicyVersion: ptrStr("  "), Reason: "x"}, []string{"policy_version must be non-blank"}},
		{"expires and clear together", EnvelopeAuthorityPatch{ExpiresAt: ptrTime(time.Now()), ClearExpiresAt: true, Reason: "x"}, []string{"expires_at and clear_expires_at are mutually exclusive"}},
		{"zero expires", EnvelopeAuthorityPatch{ExpiresAt: &time.Time{}, Reason: "x"}, []string{"expires_at must be set (use clear_expires_at to remove it)"}},
		{"zero effective", EnvelopeAuthorityPatch{EffectiveAt: &time.Time{}, Reason: "x"}, []string{"effective_at must be set"}},
		{"bad approval id", EnvelopeAuthorityPatch{Allocation: ptrUSD(1), Reason: "x", ApprovalID: "nope"}, []string{"approval_id must be a canonical uuid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, problemsOf(t, tc.patch.Validate()))
		})
	}
	ok := EnvelopeAuthorityPatch{Allocation: ptrUSD(100), AllowedInstruments: ptrList(uuid.NewString()), Reason: "raise", ApprovalID: uuid.NewString(), ClearExpiresAt: true}
	assert.NoError(t, ok.Validate())
	assert.False(t, ok.IsEmpty())
	assert.True(t, EnvelopeAuthorityPatch{}.IsEmpty())
	assert.False(t, EnvelopeAuthorityPatch{ClearExpiresAt: true}.IsEmpty())
}

func patchBase() Envelope {
	exp := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	return Envelope{
		ID:            NewEnvelopeID(),
		Status:        EnvelopeActive,
		Allocation:    usd(1_000_000),
		Available:     usd(400_000),
		Reserved:      usd(250_000),
		Deployed:      usd(350_000),
		MaxDailyLoss:  usd(50_000),
		MaxDrawdown:   usd(100_000),
		PolicyVersion: "risk-v1",
		EffectiveAt:   time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:     &exp,
		AllowedVenues: []string{"jupiter"},
	}
}

func TestApplyAuthorityPatch_Allocation(t *testing.T) {
	t.Run("increase flows into available and keeps the conservation equality", func(t *testing.T) {
		e := patchBase()
		got, changes, err := applyAuthorityPatch(e, EnvelopeAuthorityPatch{Allocation: ptrUSD(1_500_000), Reason: "top up"})
		require.NoError(t, err)
		assert.Equal(t, usd(1_500_000), got.Allocation)
		assert.Equal(t, usd(900_000), got.Available)
		assert.Equal(t, e.Reserved, got.Reserved)
		assert.Equal(t, e.Deployed, got.Deployed)
		assert.Equal(t, FieldChange{From: int64(1_000_000), To: int64(1_500_000)}, changes["allocation_usd_minor"])
		assert.Equal(t, FieldChange{From: int64(400_000), To: int64(900_000)}, changes["available_usd_minor"])
		assert.Len(t, changes, 2)
		assertConserved(t, got)
	})

	t.Run("decrease within available is allowed", func(t *testing.T) {
		got, changes, err := applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{Allocation: ptrUSD(600_000), Reason: "trim"})
		require.NoError(t, err)
		assert.Equal(t, usd(600_000), got.Allocation)
		assert.True(t, got.Available.IsZero())
		assert.Len(t, changes, 2)
		assertConserved(t, got)
	})

	t.Run("decrease below reserved + deployed is rejected", func(t *testing.T) {
		_, _, err := applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{Allocation: ptrUSD(599_999), Reason: "too far"})
		require.Error(t, err)
		e, ok := errs.As(err)
		require.True(t, ok)
		assert.Equal(t, errs.CodeValidationFailed, e.Code)
		assert.Equal(t, int64(600_000), e.Fields["minimum_allocation_usd_minor"])
		assert.Equal(t, int64(250_000), e.Fields["reserved_usd_minor"])
		assert.Equal(t, int64(350_000), e.Fields["deployed_usd_minor"])
	})

	t.Run("same allocation is not a change", func(t *testing.T) {
		got, changes, err := applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{Allocation: ptrUSD(1_000_000), Reason: "noop"})
		require.NoError(t, err)
		assert.Empty(t, changes)
		assert.Equal(t, patchBase().Available, got.Available)
	})
}

func TestApplyAuthorityPatch_Fields(t *testing.T) {
	inst := uuid.NewString()
	t.Run("limits, lists, policy, rate and window are recorded as from/to", func(t *testing.T) {
		newExp := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
		newEff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		got, changes, err := applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{
			MaxDailyLoss:        ptrUSD(60_000),
			MaxDrawdown:         ptrUSD(100_000), // unchanged
			MaxSingleTrade:      ptrUSD(5_000),
			AllowedInstruments:  ptrList(inst, inst), // deduped (a blank entry fails validation first)
			AllowedVenues:       ptrList("jupiter"),  // unchanged
			AllowedAssetClasses: ptrList("SPL_TOKEN"),
			MaxOrderRatePerHour: ptrI32(30),
			PolicyVersion:       ptrStr("risk-v2"),
			EffectiveAt:         &newEff,
			ExpiresAt:           &newExp,
			Reason:              "quarterly review",
		})
		require.NoError(t, err)
		assert.Equal(t, usd(60_000), got.MaxDailyLoss)
		assert.Equal(t, []string{inst}, got.AllowedInstruments)
		assert.Equal(t, []string{"SPL_TOKEN"}, got.AllowedAssetClasses)
		assert.Equal(t, int32(30), got.MaxOrderRatePerHour)
		assert.Equal(t, "risk-v2", got.PolicyVersion)
		assert.True(t, got.EffectiveAt.Equal(newEff))
		assert.True(t, got.ExpiresAt.Equal(newExp))
		wantKeys := []string{"max_daily_loss_usd_minor", "max_single_trade_usd_minor", "allowed_instruments", "allowed_asset_classes", "max_order_rate_per_hour", "policy_version", "effective_at", "expires_at"}
		assert.Len(t, changes, len(wantKeys))
		for _, k := range wantKeys {
			assert.Contains(t, changes, k)
		}
		assert.Equal(t, FieldChange{From: int64(50_000), To: int64(60_000)}, changes["max_daily_loss_usd_minor"])
		assert.Equal(t, FieldChange{From: "risk-v1", To: "risk-v2"}, changes["policy_version"])
		assert.Equal(t, FieldChange{From: []string{}, To: []string{inst}}, changes["allowed_instruments"])
		assert.Equal(t, FieldChange{From: "2026-12-31T00:00:00Z", To: "2027-01-31T00:00:00Z"}, changes["expires_at"])
		assertConserved(t, got)
	})

	t.Run("clearing the expiry", func(t *testing.T) {
		got, changes, err := applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{ClearExpiresAt: true, Reason: "open-ended"})
		require.NoError(t, err)
		assert.Nil(t, got.ExpiresAt)
		assert.Equal(t, FieldChange{From: "2026-12-31T00:00:00Z", To: nil}, changes["expires_at"])
		// Clearing an already-nil expiry is not a change.
		e := patchBase()
		e.ExpiresAt = nil
		_, changes, err = applyAuthorityPatch(e, EnvelopeAuthorityPatch{ClearExpiresAt: true, Reason: "again"})
		require.NoError(t, err)
		assert.Empty(t, changes)
	})

	t.Run("expiry before effective is rejected", func(t *testing.T) {
		_, _, err := applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{ExpiresAt: ptrTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)), Reason: "bad"})
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		_, _, err = applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{EffectiveAt: ptrTime(time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)), Reason: "bad"})
		require.Error(t, err, "moving effective_at past expires_at")
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})

	t.Run("invalid patch never reaches application", func(t *testing.T) {
		_, _, err := applyAuthorityPatch(patchBase(), EnvelopeAuthorityPatch{Allocation: ptrUSD(1)})
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
}

func TestNormalizeList(t *testing.T) {
	assert.Equal(t, []string{}, normalizeList(nil))
	assert.Equal(t, []string{"a", "b"}, normalizeList([]string{" a", "b ", "a", "", "  "}))
}

// assertConserved checks the envelope budget identity.
func assertConserved(t *testing.T, e Envelope) {
	t.Helper()
	sum, err := e.Available.Add(e.Reserved)
	require.NoError(t, err)
	sum, err = sum.Add(e.Deployed)
	require.NoError(t, err)
	assert.Equal(t, e.Allocation, sum, "available + reserved + deployed must equal allocation")
	assert.False(t, e.Available.IsNegative())
	assert.False(t, e.Reserved.IsNegative())
	assert.False(t, e.Deployed.IsNegative())
}
