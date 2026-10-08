package treasury

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func samplePolicy() Policy {
	return Policy{
		Label: "mm-v1", ValuationAsset: assets.NewAssetID(), DailyLossLimit: q(50_000_000), MaxMarkAge: 30 * time.Second,
		MaxInventory:          []AssetLimit{{Asset: assets.NewAssetID(), MaxQuantity: q(5_000_000)}},
		MaxInstrumentExposure: []InstrumentLimit{{Instrument: instruments.NewInstrumentID(), MaxExposure: q(1_000_000_000)}},
		PaperSpreadBPS:        25, PaperFeeBPS: 5,
	}
}

func TestPolicy_ValidateRefusesEveryUnusableLimit(t *testing.T) {
	require.NoError(t, samplePolicy().Validate())
	cases := map[string]func(p *Policy){
		"no label":            func(p *Policy) { p.Label = "" },
		"padded label":        func(p *Policy) { p.Label = " x" },
		"no valuation asset":  func(p *Policy) { p.ValuationAsset = assets.AssetID{} },
		"zero loss limit":     func(p *Policy) { p.DailyLossLimit = q(0) },
		"negative loss limit": func(p *Policy) { p.DailyLossLimit = q(-1) },
		"no mark age":         func(p *Policy) { p.MaxMarkAge = 0 },
		"mark age over a day": func(p *Policy) { p.MaxMarkAge = 25 * time.Hour },
		"sub-millisecond age": func(p *Policy) { p.MaxMarkAge = time.Second + time.Microsecond },
		"cap on cash":         func(p *Policy) { p.MaxInventory[0].Asset = p.ValuationAsset },
		"negative cap":        func(p *Policy) { p.MaxInventory[0].MaxQuantity = q(-1) },
		"duplicate asset":     func(p *Policy) { p.MaxInventory = append(p.MaxInventory, p.MaxInventory[0]) },
		"duplicate instrument": func(p *Policy) {
			p.MaxInstrumentExposure = append(p.MaxInstrumentExposure, p.MaxInstrumentExposure[0])
		},
		"negative exposure":    func(p *Policy) { p.MaxInstrumentExposure[0].MaxExposure = q(-1) },
		"spread of 100%":       func(p *Policy) { p.PaperSpreadBPS = money.OneHundredPercent },
		"negative paper fee":   func(p *Policy) { p.PaperFeeBPS = -1 },
		"paper fee above 100%": func(p *Policy) { p.PaperFeeBPS = money.OneHundredPercent + 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := samplePolicy()
			mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// The canonical form does not depend on the order limits were listed in, and
// decodes back to exactly the policy it encodes.
func TestPolicy_DocumentIsCanonicalAndRoundTrips(t *testing.T) {
	house := econ.NewHouseAccountID()
	p := samplePolicy()
	p.MaxInventory = append(p.MaxInventory, AssetLimit{Asset: assets.NewAssetID(), MaxQuantity: q(7)})
	a, err := p.Document(house)
	require.NoError(t, err)
	reordered := p
	reordered.MaxInventory = []AssetLimit{p.MaxInventory[1], p.MaxInventory[0]}
	b, err := reordered.Document(house)
	require.NoError(t, err)
	assert.Equal(t, a, b)
	back, h, err := decodePolicy(a)
	require.NoError(t, err)
	assert.Equal(t, house, h)
	again, err := back.Document(house)
	require.NoError(t, err)
	assert.Equal(t, a, again)
	assert.Contains(t, string(a), `"daily_loss_limit":"50000000"`, "amounts are decimal strings")

	other, err := p.Document(econ.NewHouseAccountID())
	require.NoError(t, err)
	assert.NotEqual(t, hashHex(a), hashHex(other), "a policy names its house book")
	_, _, err = decodePolicy(append([]byte(" "), a...))
	assert.Error(t, err, "a non-canonical document is refused")
}

func TestSigningPolicy_AuthorizesOnlyTheHouseServiceAndOperators(t *testing.T) {
	v := SigningPolicyVersion{HouseAccountID: econ.NewHouseAccountID(), Version: 1,
		Policy: SigningPolicy{ServiceActorID: "house-svc"}}
	service := econ.Actor{Type: security.ActorService, ID: "house-svc"}
	op := econ.Actor{Type: security.ActorOperator, ID: "op"}
	for _, k := range AllActionKinds() {
		switch {
		case k.administrative():
			assert.Error(t, v.Authorize(service, k), "%s is an operator's", k)
			assert.NoError(t, v.Authorize(op, k), k)
		case k == ActionCancelOrder:
			assert.NoError(t, v.Authorize(service, k))
			assert.NoError(t, v.Authorize(op, k), "an operator may always cancel")
		default:
			assert.NoError(t, v.Authorize(service, k), k)
			assert.Error(t, v.Authorize(op, k), "%s: operators do not trade unless allowed", k)
		}
		for _, a := range []econ.Actor{
			{Type: security.ActorUser, ID: "u"}, {Type: security.ActorAgent, ID: "a"}, {Type: security.ActorSystem, ID: "s"},
			{Type: security.ActorService, ID: "another-svc"}, {Type: security.ActorService, ID: ""},
		} {
			err := v.Authorize(a, k)
			require.Error(t, err, "%s %s %s", a.Type, a.ID, k)
			assert.Equal(t, ReasonNotAuthorized, ReasonOf(err))
			assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		}
	}
	v.Policy.OperatorsAllowed = true
	assert.NoError(t, v.Authorize(op, ActionPlaceOrder))
	assert.NoError(t, v.Authorize(op, ActionPaperFill))
}

func TestSigningPolicy_DocumentStatesNoKeyAndTheOperatorOnlyActions(t *testing.T) {
	doc, err := SigningPolicy{ServiceActorID: "house-svc"}.Document(econ.NewHouseAccountID())
	require.NoError(t, err)
	s := string(doc)
	assert.Contains(t, s, `"key_custody":"NONE_SANDBOX"`)
	assert.Contains(t, s, `"allowed_actor_types":["SERVICE","OPERATOR"]`)
	assert.Contains(t, s, `"operator_only_actions":["REGISTER","SET_POLICY","SET_SIGNING_POLICY","FUND"]`)
	_, err = SigningPolicy{}.Document(econ.NewHouseAccountID())
	assert.Error(t, err)
}

func TestClasses_FollowTheStrategies(t *testing.T) {
	for _, s := range econ.AllHouseStrategies() {
		c, ok := ClassOf(s)
		require.True(t, ok, s)
		assert.Contains(t, AllClasses(), c)
		assert.NotEmpty(t, PermittedActivities(s))
		assert.True(t, permitted(s, s), "a book may always do what its strategy is")
	}
	_, ok := ClassOf("SPECULATION")
	assert.False(t, ok)
	assert.False(t, permitted(econ.HouseTreasuryConversion, econ.HouseMarketMaking))
	assert.ElementsMatch(t, econ.AllHouseStrategies(), AllActivities())
}
