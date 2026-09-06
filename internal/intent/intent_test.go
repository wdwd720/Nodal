package intent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func usd(minor int64) *money.USD {
	u := money.USDFromMinor(minor)
	return &u
}

func qty(n int64) *money.Quantity {
	q := money.QuantityFromInt64(n)
	return &q
}

func str(s string) *string { return &s }

func uuidStr() string { return id.New[id.Any]().String() }

// baseIntent is a valid USER ACQUIRE_NOTIONAL intent.
func baseIntent() intent.TradeIntent {
	return intent.TradeIntent{
		ID:           intent.NewIntentID(),
		AccountID:    accounts.NewAccountID().String(),
		ActorType:    security.ActorUser,
		ActorID:      "user-1",
		Action:       intent.ActionAcquireNotional,
		InstrumentID: instruments.NewInstrumentID(),
		NotionalUSD:  usd(200_00),
		Constraints: intent.Constraints{
			MaxSlippageBPS: 50, MaxFeeBPS: 30, MaxPriceImpactBPS: 100,
			AllowedVenues: []string{"JUPITER"}, QuoteFreshness: 500 * time.Millisecond,
		},
		Deadline:       t0.Add(time.Minute),
		RequestedAt:    t0,
		IdempotencyKey: "key-1",
		CorrelationID:  "corr-1",
		Mode:           intent.ModeLive,
	}
}

// agentIntent is a valid AGENT intent.
func agentIntent() intent.TradeIntent {
	t := baseIntent()
	t.ActorType = security.ActorAgent
	agent := uuidStr()
	t.ActorID = agent
	t.AgentID = &agent
	t.StrategyVersionID = str(uuidStr())
	t.PredictionID = str(uuidStr())
	t.Mode = intent.ModeShadow
	return t
}

func fieldsOf(t *testing.T, err error) map[string]any {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "expected *errs.Error, got %T: %v", err, err)
	return e.Fields
}

// TestValidate_ActionFieldMatrix covers every action against every
// combination of the three amount fields the table CHECK reasons about.
func TestValidate_ActionFieldMatrix(t *testing.T) {
	t.Parallel()
	type combo struct{ notional, target, quantity bool }
	var combos []combo
	for n := 0; n < 8; n++ {
		combos = append(combos, combo{n&1 != 0, n&2 != 0, n&4 != 0})
	}
	legal := map[intent.Action]func(c combo) bool{
		intent.ActionAcquireNotional: func(c combo) bool { return c.notional && !c.target && !c.quantity },
		intent.ActionReduceNotional:  func(c combo) bool { return (c.notional != c.quantity) && !c.target },
		intent.ActionClosePosition:   func(c combo) bool { return !c.notional && !c.target && !c.quantity },
		intent.ActionTargetExposure:  func(c combo) bool { return c.target && !c.notional && !c.quantity },
	}
	for _, action := range intent.Actions() {
		for _, c := range combos {
			t.Run(fmt.Sprintf("%s/n=%v/t=%v/q=%v", action, c.notional, c.target, c.quantity), func(t *testing.T) {
				t.Parallel()
				ti := baseIntent()
				ti.Action = action
				ti.NotionalUSD, ti.TargetExposureUSD, ti.Quantity = nil, nil, nil
				if c.notional {
					ti.NotionalUSD = usd(100_00)
				}
				if c.target {
					ti.TargetExposureUSD = usd(0)
				}
				if c.quantity {
					ti.Quantity = qty(5)
				}
				err := ti.Validate()
				if legal[action](c) {
					assert.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			})
		}
	}
}

func TestValidate_AmountSigns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mut   func(*intent.TradeIntent)
		field string
	}{
		{"zero notional", func(i *intent.TradeIntent) { i.NotionalUSD = usd(0) }, "notional_usd"},
		{"negative notional", func(i *intent.TradeIntent) { i.NotionalUSD = usd(-1) }, "notional_usd"},
		{"negative target", func(i *intent.TradeIntent) {
			i.Action = intent.ActionTargetExposure
			i.NotionalUSD = nil
			i.TargetExposureUSD = usd(-1)
		}, "target_exposure_usd"},
		{"zero quantity", func(i *intent.TradeIntent) {
			i.Action = intent.ActionReduceNotional
			i.NotionalUSD = nil
			i.Quantity = qty(0)
		}, "quantity"},
		{"negative quantity", func(i *intent.TradeIntent) {
			i.Action = intent.ActionReduceNotional
			i.NotionalUSD = nil
			i.Quantity = qty(-3)
		}, "quantity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ti := baseIntent()
			tc.mut(&ti)
			err := ti.Validate()
			require.Error(t, err)
			assert.Contains(t, fieldsOf(t, err), tc.field)
		})
	}
	t.Run("zero target exposure is legal", func(t *testing.T) {
		t.Parallel()
		ti := baseIntent()
		ti.Action, ti.NotionalUSD, ti.TargetExposureUSD = intent.ActionTargetExposure, nil, usd(0)
		assert.NoError(t, ti.Validate())
	})
}

func TestValidate_Actions(t *testing.T) {
	t.Parallel()
	ti := baseIntent()
	ti.Action = intent.ActionBuyEventOutcome
	err := ti.Validate()
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), "declared but disabled")
	assert.True(t, intent.ActionBuyEventOutcome.Declared())
	assert.False(t, intent.ActionBuyEventOutcome.Enabled())

	ti.Action = "SHORT_SELL"
	err = ti.Validate()
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, fieldsOf(t, err), "action")
	assert.False(t, intent.Action("SHORT_SELL").Declared())
	assert.Len(t, intent.Actions(), 4)
}

// TestValidate_ActorLinkage covers every submittable actor type against the
// agent linkage rule, and the two actor types that can never submit.
func TestValidate_ActorLinkage(t *testing.T) {
	t.Parallel()
	linkFields := []string{"agent_id", "strategy_version_id", "prediction_id"}
	for _, actor := range security.AllActorTypes() {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/linked=%v", actor, linked), func(t *testing.T) {
				t.Parallel()
				ti := baseIntent()
				ti.ActorType = actor
				if linked {
					ti.AgentID, ti.StrategyVersionID, ti.PredictionID = str(uuidStr()), str(uuidStr()), str(uuidStr())
				}
				err := ti.Validate()
				switch {
				case !intent.CanSubmit(actor):
					require.Error(t, err)
					assert.Contains(t, fieldsOf(t, err), "actor_type")
				case actor == security.ActorAgent && !linked:
					require.Error(t, err)
					for _, f := range linkFields {
						assert.Contains(t, fieldsOf(t, err), f)
					}
				case actor != security.ActorAgent && linked:
					require.Error(t, err)
					for _, f := range linkFields {
						assert.Contains(t, fieldsOf(t, err), f)
					}
				default:
					assert.NoError(t, err)
				}
			})
		}
	}
	t.Run("agent with partial linkage", func(t *testing.T) {
		t.Parallel()
		ti := agentIntent()
		ti.PredictionID = nil
		err := ti.Validate()
		require.Error(t, err)
		assert.Equal(t, map[string]any{"prediction_id": "required for AGENT intents"}, fieldsOf(t, err))
	})
	t.Run("agent with malformed linkage", func(t *testing.T) {
		t.Parallel()
		ti := agentIntent()
		ti.AgentID = str("agent-7")
		err := ti.Validate()
		require.Error(t, err)
		assert.Contains(t, fieldsOf(t, err), "agent_id")
	})
}

func TestValidate_Identity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mut   func(*intent.TradeIntent)
		field string
	}{
		{"zero id", func(i *intent.TradeIntent) { i.ID = intent.IntentID{} }, "id"},
		{"bad account", func(i *intent.TradeIntent) { i.AccountID = "acct-1" }, "account_id"},
		{"zero instrument", func(i *intent.TradeIntent) { i.InstrumentID = instruments.InstrumentID{} }, "instrument_id"},
		{"empty mode", func(i *intent.TradeIntent) { i.Mode = "" }, "mode"},
		{"unknown mode", func(i *intent.TradeIntent) { i.Mode = "REAL" }, "mode"},
		{"empty key", func(i *intent.TradeIntent) { i.IdempotencyKey = "  " }, "idempotency_key"},
		{"long key", func(i *intent.TradeIntent) { i.IdempotencyKey = strings.Repeat("k", 256) }, "idempotency_key"},
		{"control char key", func(i *intent.TradeIntent) { i.IdempotencyKey = "k\x00" }, "idempotency_key"},
		{"empty correlation", func(i *intent.TradeIntent) { i.CorrelationID = "" }, "correlation_id"},
		{"empty actor id", func(i *intent.TradeIntent) { i.ActorID = "" }, "actor_id"},
		{"zero requested_at", func(i *intent.TradeIntent) { i.RequestedAt = time.Time{} }, "requested_at"},
		{"deadline equals requested_at", func(i *intent.TradeIntent) { i.Deadline = i.RequestedAt }, "deadline"},
		{"deadline before requested_at", func(i *intent.TradeIntent) { i.Deadline = i.RequestedAt.Add(-time.Second) }, "deadline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ti := baseIntent()
			tc.mut(&ti)
			err := ti.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			assert.Contains(t, fieldsOf(t, err), tc.field)
		})
	}
	t.Run("no deadline is legal", func(t *testing.T) {
		t.Parallel()
		ti := baseIntent()
		ti.Deadline = time.Time{}
		assert.NoError(t, ti.Validate())
	})
	t.Run("every mode is legal", func(t *testing.T) {
		t.Parallel()
		for _, m := range intent.Modes() {
			ti := baseIntent()
			ti.Mode = m
			assert.NoError(t, ti.Validate(), m)
		}
	})
}

func TestValidate_Constraints(t *testing.T) {
	t.Parallel()
	badPrice := money.Price{Mantissa: money.QuantityFromInt64(1), Scale: 99, QuoteAsset: "USDC", Source: "x", At: t0}
	zeroPrice := money.Price{Mantissa: money.QuantityFromInt64(0), Scale: 6, QuoteAsset: "USDC", Source: "x", At: t0}
	goodPrice := money.Price{Mantissa: money.QuantityFromInt64(150_000000), Scale: 6, QuoteAsset: "USDC", Source: "x", At: t0}
	many := make([]string, intent.MaxAllowedVenues+1)
	for i := range many {
		many[i] = fmt.Sprintf("V%d", i)
	}
	cases := []struct {
		name  string
		mut   func(*intent.Constraints)
		field string
		ok    bool
	}{
		{"slippage -1", func(c *intent.Constraints) { c.MaxSlippageBPS = -1 }, "constraints.max_slippage_bps", false},
		{"slippage 0", func(c *intent.Constraints) { c.MaxSlippageBPS = 0 }, "", true},
		{"slippage 10000", func(c *intent.Constraints) { c.MaxSlippageBPS = 10_000 }, "", true},
		{"slippage 10001", func(c *intent.Constraints) { c.MaxSlippageBPS = 10_001 }, "constraints.max_slippage_bps", false},
		{"fee -1", func(c *intent.Constraints) { c.MaxFeeBPS = -1 }, "constraints.max_fee_bps", false},
		{"fee 10001", func(c *intent.Constraints) { c.MaxFeeBPS = 10_001 }, "constraints.max_fee_bps", false},
		{"impact -1", func(c *intent.Constraints) { c.MaxPriceImpactBPS = -1 }, "constraints.max_price_impact_bps", false},
		{"impact 10001", func(c *intent.Constraints) { c.MaxPriceImpactBPS = 10_001 }, "constraints.max_price_impact_bps", false},
		{"max price invalid", func(c *intent.Constraints) { c.MaxPrice = &badPrice }, "constraints.max_price", false},
		{"max price zero", func(c *intent.Constraints) { c.MaxPrice = &zeroPrice }, "constraints.max_price", false},
		{"max price ok", func(c *intent.Constraints) { c.MaxPrice = &goodPrice }, "", true},
		{"min receive zero", func(c *intent.Constraints) { c.MinReceive = qty(0) }, "constraints.min_receive", false},
		{"min receive negative", func(c *intent.Constraints) { c.MinReceive = qty(-1) }, "constraints.min_receive", false},
		{"min receive ok", func(c *intent.Constraints) { c.MinReceive = qty(1) }, "", true},
		{"venue blank", func(c *intent.Constraints) { c.AllowedVenues = []string{" "} }, "constraints.allowed_venues", false},
		{"venue untrimmed", func(c *intent.Constraints) { c.AllowedVenues = []string{" JUPITER"} }, "constraints.allowed_venues", false},
		{"venue duplicate", func(c *intent.Constraints) { c.AllowedVenues = []string{"A", "A"} }, "constraints.allowed_venues", false},
		{"venue too long", func(c *intent.Constraints) { c.AllowedVenues = []string{strings.Repeat("V", 65)} }, "constraints.allowed_venues", false},
		{"too many venues", func(c *intent.Constraints) { c.AllowedVenues = many }, "constraints.allowed_venues", false},
		{"no venues", func(c *intent.Constraints) { c.AllowedVenues = nil }, "", true},
		{"freshness negative", func(c *intent.Constraints) { c.QuoteFreshness = -time.Millisecond }, "constraints.quote_freshness", false},
		{"freshness sub-ms", func(c *intent.Constraints) { c.QuoteFreshness = 1500 * time.Microsecond }, "constraints.quote_freshness", false},
		{"freshness zero", func(c *intent.Constraints) { c.QuoteFreshness = 0 }, "", true},
		{"exec deadline before requested", func(c *intent.Constraints) { c.ExecutionDeadline = t0.Add(-time.Second) }, "constraints.execution_deadline", false},
		{"exec deadline equals requested", func(c *intent.Constraints) { c.ExecutionDeadline = t0 }, "constraints.execution_deadline", false},
		{"exec deadline after deadline", func(c *intent.Constraints) { c.ExecutionDeadline = t0.Add(2 * time.Minute) }, "constraints.execution_deadline", false},
		{"exec deadline equals deadline", func(c *intent.Constraints) { c.ExecutionDeadline = t0.Add(time.Minute) }, "", true},
		{"exec deadline within", func(c *intent.Constraints) { c.ExecutionDeadline = t0.Add(30 * time.Second) }, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ti := baseIntent()
			tc.mut(&ti.Constraints)
			err := ti.Validate()
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			assert.Contains(t, fieldsOf(t, err), tc.field)
		})
	}
}

func TestValidate_ReportsEveryField(t *testing.T) {
	t.Parallel()
	ti := baseIntent()
	ti.Mode, ti.IdempotencyKey, ti.CorrelationID = "", "", ""
	ti.Constraints.MaxFeeBPS = 20_000
	err := ti.Validate()
	require.Error(t, err)
	f := fieldsOf(t, err)
	for _, k := range []string{"mode", "idempotency_key", "correlation_id", "constraints.max_fee_bps"} {
		assert.Contains(t, f, k)
	}
}

func TestStatusAndModeSets(t *testing.T) {
	t.Parallel()
	assert.Len(t, intent.Statuses(), 12)
	assert.Len(t, intent.TerminalStatuses(), 6)
	for _, s := range intent.TerminalStatuses() {
		assert.True(t, s.IsTerminal(), s)
		assert.Empty(t, intent.Transitions[s], s)
	}
	assert.False(t, intent.StatusReceived.IsTerminal())
	assert.False(t, intent.Status("DONE").Valid())
	assert.Equal(t, []intent.Mode{"BACKTEST", "PAPER", "SHADOW", "CANARY", "LIMITED", "LIVE"}, intent.Modes())
	assert.False(t, intent.Mode("live").Valid(), "modes are case-sensitive wire values")
}

// FuzzValidate proves Validate and Canonical never panic and agree with
// themselves for arbitrary field values.
func FuzzValidate(f *testing.F) {
	f.Add("ACQUIRE_NOTIONAL", "USER", "LIVE", int64(100_00), int64(0), "", int64(50), int64(500), "JUPITER", int64(60), "key", "corr")
	f.Add("REDUCE_NOTIONAL", "AGENT", "SHADOW", int64(0), int64(0), "5", int64(10_000), int64(0), "", int64(0), "run:1:sell", "")
	f.Add("TARGET_EXPOSURE", "OPERATOR", "PAPER", int64(0), int64(0), "", int64(-1), int64(-7), " X", int64(-5), "k", "c")
	f.Add("BUY_EVENT_OUTCOME", "SYSTEM", "", int64(-1), int64(-1), "x", int64(99_999), int64(1), "\x00", int64(1), "", "")
	f.Fuzz(func(t *testing.T, action, actor, mode string, notional, target int64, quantity string, bps, freshnessMS int64, venue string, deadlineSec int64, key, corr string) {
		ti := intent.TradeIntent{
			ID: intent.NewIntentID(), AccountID: accounts.NewAccountID().String(),
			ActorType: security.ActorType(actor), ActorID: "actor", Action: intent.Action(action),
			InstrumentID: instruments.NewInstrumentID(), RequestedAt: t0,
			IdempotencyKey: key, CorrelationID: corr, Mode: intent.Mode(mode),
			Constraints: intent.Constraints{
				MaxSlippageBPS: money.BPS(bps), MaxFeeBPS: money.BPS(bps / 2), MaxPriceImpactBPS: money.BPS(bps / 3),
				QuoteFreshness: time.Duration(freshnessMS) * time.Millisecond,
			},
		}
		if notional != 0 {
			ti.NotionalUSD = usd(notional)
		}
		if target != 0 {
			ti.TargetExposureUSD = usd(target)
		}
		if q, err := money.ParseQuantity(quantity); err == nil {
			ti.Quantity = &q
		}
		if venue != "" {
			ti.Constraints.AllowedVenues = []string{venue}
		}
		if deadlineSec != 0 {
			ti.Deadline = t0.Add(time.Duration(deadlineSec) * time.Second)
		}
		if ti.ActorType == security.ActorAgent {
			ti.AgentID, ti.StrategyVersionID, ti.PredictionID = str(uuidStr()), str(uuidStr()), str(uuidStr())
		}
		err := ti.Validate()
		c1 := intent.Canonical(ti)
		c2 := intent.Canonical(ti)
		require.Equal(t, string(c1), string(c2), "canonical form is deterministic")
		if err != nil {
			code := errs.CodeOf(err)
			require.True(t, code == errs.CodeValidationFailed || code == errs.CodeUnsupported, "code %s", code)
			return
		}
		require.NoError(t, ti.Validate(), "validation is idempotent")
	})
}
