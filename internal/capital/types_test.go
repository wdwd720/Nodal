package capital

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

func validReserveRequest() ReserveRequest {
	return ReserveRequest{
		AccountID:      accounts.NewAccountID().String(),
		AssetID:        assets.NewAssetID(),
		Quantity:       money.QuantityFromInt64(500_000_000),
		USDMinor:       50_000,
		IntentID:       uuid.NewString(),
		ActorType:      security.ActorAgent,
		ActorID:        "agent-1",
		IdempotencyKey: "intent:" + uuid.NewString(),
		TTL:            5 * time.Minute,
		Reason:         "intent",
	}
}

func TestReserveRequest_Validate(t *testing.T) {
	assert.NoError(t, validReserveRequest().Validate())
	cases := []struct {
		name   string
		mutate func(*ReserveRequest)
		want   string
	}{
		{"empty account", func(r *ReserveRequest) { r.AccountID = "" }, "account_id must be a canonical uuid"},
		{"non-v7 account", func(r *ReserveRequest) { r.AccountID = uuid.New().String() }, "account_id must be a canonical uuid"},
		{"zero asset", func(r *ReserveRequest) { r.AssetID = assets.AssetID{} }, "asset_id required"},
		{"zero quantity", func(r *ReserveRequest) { r.Quantity = money.Quantity{} }, "quantity must be positive"},
		{"negative quantity", func(r *ReserveRequest) { r.Quantity = money.QuantityFromInt64(-1) }, "quantity must be positive"},
		{"negative usd", func(r *ReserveRequest) { r.USDMinor = -1 }, "usd_minor must not be negative"},
		{"zero envelope pointer", func(r *ReserveRequest) { r.EnvelopeID = &EnvelopeID{} }, "envelope_id must be set when present"},
		{"bad intent id", func(r *ReserveRequest) { r.IntentID = "intent-1" }, "intent_id must be a canonical uuid"},
		{"unknown actor type", func(r *ReserveRequest) { r.ActorType = "ROBOT" }, `unknown actor type "ROBOT"`},
		{"blank actor id", func(r *ReserveRequest) { r.ActorID = " " }, "actor_id required"},
		{"blank key", func(r *ReserveRequest) { r.IdempotencyKey = "" }, "idempotency_key required"},
		{"zero ttl", func(r *ReserveRequest) { r.TTL = 0 }, "ttl must be positive"},
		{"huge ttl", func(r *ReserveRequest) { r.TTL = 25 * time.Hour }, "ttl exceeds 24h0m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validReserveRequest()
			tc.mutate(&r)
			assert.Contains(t, problemsOf(t, r.Validate()), tc.want)
		})
	}
	// Every problem is reported at once.
	all := problemsOf(t, ReserveRequest{}.Validate())
	assert.GreaterOrEqual(t, len(all), 7)
}

func validEnvelope() Envelope {
	return Envelope{
		AccountID:         accounts.NewAccountID(),
		AgentID:           uuid.NewString(),
		StrategyVersionID: uuid.NewString(),
		SettlementAssetID: assets.NewAssetID(),
		Allocation:        usd(1_000_000),
		Available:         usd(1_000_000),
		MaxDailyLoss:      usd(50_000),
		MaxDrawdown:       usd(100_000),
		MaxSingleTrade:    usd(50_000),
		MaxPosition:       usd(200_000),
		PolicyVersion:     "risk-v1",
		Status:            EnvelopeDraft,
		EffectiveAt:       time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC),
	}
}

func TestEnvelope_Validate(t *testing.T) {
	assert.NoError(t, validEnvelope().Validate())
	exp := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*Envelope)
		want   string
	}{
		{"zero account", func(e *Envelope) { e.AccountID = accounts.AccountID{} }, "account_id required"},
		{"bad agent", func(e *Envelope) { e.AgentID = "agent" }, "agent_id must be a canonical uuid"},
		{"bad strategy version", func(e *Envelope) { e.StrategyVersionID = "" }, "strategy_version_id must be a canonical uuid"},
		{"zero asset", func(e *Envelope) { e.SettlementAssetID = assets.AssetID{} }, "settlement_asset_id required"},
		{"negative allocation", func(e *Envelope) { e.Allocation = usd(-1); e.Available = usd(-1) }, "allocation must not be negative"},
		{"available differs", func(e *Envelope) { e.Available = usd(1) }, "available must equal allocation at creation"},
		{"reserved preset", func(e *Envelope) { e.Reserved = usd(1) }, "reserved must be zero at creation"},
		{"deployed preset", func(e *Envelope) { e.Deployed = usd(1) }, "deployed must be zero at creation"},
		{"pnl preset", func(e *Envelope) { e.RealizedPnL = usd(1) }, "realized_pnl must be zero at creation"},
		{"negative limit", func(e *Envelope) { e.MaxDrawdown = usd(-1) }, "max_drawdown must not be negative"},
		{"negative rate", func(e *Envelope) { e.MaxOrderRatePerHour = -1 }, "max_order_rate_per_hour must not be negative"},
		{"bad instrument", func(e *Envelope) { e.AllowedInstruments = []string{"SOL"} }, "allowed_instruments entries must be canonical uuids"},
		{"blank policy", func(e *Envelope) { e.PolicyVersion = "" }, "policy_version required"},
		{"bad status", func(e *Envelope) { e.Status = EnvelopePaused }, `status must be DRAFT or ACTIVE at creation, got "PAUSED"`},
		{"zero effective", func(e *Envelope) { e.EffectiveAt = time.Time{} }, "effective_at required"},
		{"expires before effective", func(e *Envelope) { e.ExpiresAt = &exp }, "expires_at must be after effective_at"},
		{"agent creator", func(e *Envelope) { e.CreatedByActorType = security.ActorAgent }, "an agent cannot create an envelope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnvelope()
			tc.mutate(&e)
			assert.Contains(t, problemsOf(t, e.Validate()), tc.want)
		})
	}
}

func TestEnvelope_Windows(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	e := validEnvelope()
	e.Status = EnvelopeActive
	assert.True(t, e.UsableAt(now))
	assert.False(t, e.ExpiredAt(now))
	later := now.Add(time.Hour)
	e.ExpiresAt = &later
	assert.True(t, e.UsableAt(now))
	assert.False(t, e.UsableAt(later), "expiry instant itself is closed")
	e.ExpiresAt = nil
	e.EffectiveAt = now.Add(time.Minute)
	assert.False(t, e.UsableAt(now), "not yet effective")
	assert.True(t, e.UsableAt(now.Add(time.Minute)))
	for _, s := range []EnvelopeStatus{EnvelopeDraft, EnvelopePaused, EnvelopeExhausted, EnvelopeExpired, EnvelopeRevoked} {
		e.Status = s
		assert.False(t, e.UsableAt(now.Add(time.Hour)), s)
	}
}

func TestWithdrawalHold_ActiveAt(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	h := WithdrawalHold{}
	assert.True(t, h.ActiveAt(now))
	exp := now.Add(time.Second)
	h.ExpiresAt = &exp
	assert.True(t, h.ActiveAt(now))
	assert.False(t, h.ActiveAt(exp))
	h.ExpiresAt = nil
	h.ReleasedAt = &now
	assert.False(t, h.ActiveAt(now))
}

func TestReservation_Remaining(t *testing.T) {
	r := Reservation{Quantity: money.QuantityFromInt64(100), ConsumedQuantity: money.QuantityFromInt64(30), USD: usd(1000), ConsumedUSD: usd(250)}
	assert.Equal(t, "70", r.Remaining().String())
	rem, err := r.RemainingUSD()
	require.NoError(t, err)
	assert.Equal(t, usd(750), rem)
	assert.False(t, r.IsLocked())
	r.LockedByOrderID = uuid.NewString()
	assert.True(t, r.IsLocked())
}

func TestPurpose_Valid(t *testing.T) {
	for _, p := range []Purpose{PurposeTrade, PurposeAgentDeploy, PurposeWithdrawal, PurposeDisplay} {
		assert.True(t, p.Valid(), p)
	}
	assert.False(t, Purpose("").Valid())
	assert.False(t, Purpose("trade").Valid())
}

// The buying-power output shape is fixed by FINANCIAL_MODEL §6; consumers
// (API, web) key on exactly these names.
func TestBuyingPower_JSONShape(t *testing.T) {
	bp := BuyingPower{
		UnderlyingBalances: []UnderlyingBalance{{AssetID: assets.NewAssetID(), Quantity: money.QuantityFromInt64(1), USDValue: usd(1), PriceRef: "peg:USD", Status: "NORMAL"}},
		Haircuts:           []Haircut{{AssetID: assets.NewAssetID(), FactorBPS: 9_500, Reason: "DEGRADED"}},
		Restrictions:       []Restriction{{Code: buyingpower.RestrictionAccountFrozen, Detail: "frozen", Scope: buyingpower.ScopeAccount, Blocking: true}},
		PolicyVersion:      "assets-v1",
		AsOf:               time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(bp)
	require.NoError(t, err)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &top))
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"as_of", "available_now", "buying_power", "haircuts", "pending", "policy_version", "portfolio_value", "purpose", "reserved", "restrictions", "underlying_balances", "withdrawable"}, keys)
	// Money is always a JSON string, never a number.
	assert.Equal(t, `"0.00"`, string(top["buying_power"]))

	var ub []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["underlying_balances"], &ub))
	require.Len(t, ub, 1)
	for _, k := range []string{"asset", "quantity", "usd_value", "price_ref", "status"} {
		assert.Contains(t, ub[0], k)
	}
	var hc []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["haircuts"], &hc))
	for _, k := range []string{"asset", "factor_bps", "reason"} {
		assert.Contains(t, hc[0], k)
	}
	var rs []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["restrictions"], &rs))
	assert.Equal(t, `"ACCOUNT_FROZEN"`, string(rs[0]["code"]))
	assert.Contains(t, rs[0], "detail")
}

func TestIDs_RoundTrip(t *testing.T) {
	r := NewReservationID()
	got, err := ParseReservationID(r.String())
	require.NoError(t, err)
	assert.Equal(t, r, got)
	e := NewEnvelopeID()
	got2, err := ParseEnvelopeID(e.String())
	require.NoError(t, err)
	assert.Equal(t, e, got2)
	h := NewWithdrawalHoldID()
	got3, err := ParseWithdrawalHoldID(h.String())
	require.NoError(t, err)
	assert.Equal(t, h, got3)
	_, err = ParseEnvelopeID("not-an-id")
	assert.Error(t, err)
}
