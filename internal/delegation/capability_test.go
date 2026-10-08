package delegation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

func TestCapabilityCoversAUseInsideEveryBound(t *testing.T) {
	c := unitCapability(t)
	check := c.Check(unitUse(c))
	assert.True(t, check.OK(), "%v", check.Codes)
	assert.Equal(t, CapabilityActive, c.Status)
	snap := check.Snapshot()
	assert.True(t, snap.Present && !snap.Expired && !snap.Revoked && snap.WithinLimits && snap.InScope)
}

// Every way a use can fall outside a grant has its own stable code, and the
// risk kernel's authority section says so too.
func TestCapabilityRefusesEveryUseOutsideItsGrant(t *testing.T) {
	c := unitCapability(t)
	base := unitUse(c)
	cases := []struct {
		name   string
		mutate func(*Capability, *Use)
		code   string
	}{
		{"expired", func(_ *Capability, u *Use) { u.Now = c.Terms.ExpiresAt }, ReasonCapabilityExpired},
		{"not yet valid", func(_ *Capability, u *Use) { u.Now = c.GrantedAt.Add(-time.Second) }, ReasonCapabilityNotYetValid},
		{"revoked", func(k *Capability, _ *Use) { k.Status = CapabilityRevoked }, ReasonCapabilityRevoked},
		{"another account", func(_ *Capability, u *Use) { u.AccountID = accounts.AccountID(id.New[id.Any]()) }, ReasonAccountMismatch},
		{"another agent", func(_ *Capability, u *Use) { u.AgentID = id.New[id.Any]().String() }, ReasonAgentMismatch},
		{"another strategy", func(_ *Capability, u *Use) { u.StrategyID = id.New[id.Any]().String() }, ReasonStrategyMismatch},
		{"another environment", func(_ *Capability, u *Use) { u.Environment = config.EnvProd }, ReasonEnvironmentMismatch},
		{"another mode", func(_ *Capability, u *Use) { u.Mode = gates.ModeDevnet }, ReasonModeMismatch},
		{"an action not granted", func(k *Capability, _ *Use) {
			k.Terms.ActionClasses = []ActionClass{ActionReduceExposure}
		}, ReasonActionNotGranted},
		{"an instrument class not granted", func(_ *Capability, u *Use) {
			u.Classes = []InstrumentClass{ClassSpeculativeToken}
		}, ReasonInstrumentNotGranted},
		{"no instrument class", func(_ *Capability, u *Use) { u.Classes = nil }, ReasonInstrumentNotGranted},
		{"a chain not granted", func(_ *Capability, u *Use) { u.Chain = devnethost.ChainEthereum }, ReasonChainNotGranted},
		{"a destination not granted", func(_ *Capability, u *Use) { u.Destination = ukey("elsewhere") }, ReasonDestinationNotGranted},
		{"the zero destination", func(_ *Capability, u *Use) { u.Destination = devnet.PublicKey{} }, ReasonDestinationNotGranted},
		{"over the single limit", func(_ *Capability, u *Use) { u.Notional = money.USDFromMinor(100_001) }, ReasonSingleLimitExceeded},
		{"a zero notional", func(_ *Capability, u *Use) { u.Notional = money.USDFromMinor(0) }, ReasonSingleLimitExceeded},
		{"over the daily limit", func(_ *Capability, u *Use) { u.DayUsed = money.USDFromMinor(490_001) }, ReasonDailyLimitExceeded},
		{"over the lifetime limit", func(_ *Capability, u *Use) { u.TotalUsed = money.USDFromMinor(990_001) }, ReasonLifetimeLimitExceeded},
		{"a tampered row", func(k *Capability, _ *Use) { k.Terms.MaxSingleTrade = money.USDFromMinor(900_000) }, ReasonCapabilityTampered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, u := c, base
			k.Terms.ActionClasses = append([]ActionClass(nil), c.Terms.ActionClasses...)
			tc.mutate(&k, &u)
			check := k.Check(u)
			require.False(t, check.OK())
			assert.Contains(t, check.Codes, tc.code)
			snap := check.Snapshot()
			assert.False(t, snap.Present && !snap.Expired && !snap.Revoked && snap.WithinLimits && snap.InScope,
				"the risk kernel's authority section must refuse too")
		})
	}
}

func TestCapabilityHashCoversEveryTermAndTheNonce(t *testing.T) {
	c := unitCapability(t)
	mutations := map[string]func(*Capability){
		"account":     func(k *Capability) { k.Terms.AccountID = accounts.AccountID(id.New[id.Any]()) },
		"agent":       func(k *Capability) { k.Terms.AgentID = id.New[id.Any]().String() },
		"strategy":    func(k *Capability) { k.Terms.StrategyID = id.New[id.Any]().String() },
		"environment": func(k *Capability) { k.Terms.Environment = config.EnvDev },
		"mode":        func(k *Capability) { k.Terms.Mode = gates.ModeDevnet },
		"classes":     func(k *Capability) { k.Terms.InstrumentClasses = []InstrumentClass{ClassSpotToken} },
		"actions":     func(k *Capability) { k.Terms.ActionClasses = []ActionClass{ActionAcquireExposure} },
		"max":         func(k *Capability) { k.Terms.MaxNotional = money.USDFromMinor(1_000_001) },
		"daily":       func(k *Capability) { k.Terms.MaxDailyNotional = money.USDFromMinor(500_001) },
		"single":      func(k *Capability) { k.Terms.MaxSingleTrade = money.USDFromMinor(100_001) },
		"destination": func(k *Capability) { k.Terms.DestinationAllowlist = []devnet.PublicKey{ukey("venue")} },
		"chain": func(k *Capability) {
			k.Terms.ChainAllowlist = []devnet.ChainID{devnethost.ChainSolana, devnethost.ChainEthereum}
		},
		"expiry":   func(k *Capability) { k.Terms.ExpiresAt = k.Terms.ExpiresAt.Add(time.Second) },
		"approval": func(k *Capability) { k.Terms.RequiresHumanApproval = true },
		"nonce":    func(k *Capability) { k.Nonce = "f" + k.Nonce[1:] },
		"granted":  func(k *Capability) { k.GrantedAt = k.GrantedAt.Add(time.Microsecond) },
		"grantor":  func(k *Capability) { k.GrantedBy.ID = id.New[id.Any]().String() },
		"id":       func(k *Capability) { k.ID = id.New[capabilityKind]() },
	}
	for name, mutate := range mutations {
		k := c
		mutate(&k)
		h, err := k.ComputeContentHash()
		require.NoError(t, err)
		assert.NotEqual(t, c.ContentHash, h, "the hash must cover %s", name)
	}
	// Revocation is not a term: the hash of what was granted does not move.
	k := c
	k.Status = CapabilityRevoked
	h, err := k.ComputeContentHash()
	require.NoError(t, err)
	assert.Equal(t, c.ContentHash, h)
}

func TestCapabilityTermsNormalizeIsOrderInsensitive(t *testing.T) {
	c := unitCapability(t)
	shuffled := c.Terms
	shuffled.ActionClasses = []ActionClass{ActionProposeWithdrawal, ActionAcquireExposure, ActionReduceExposure, ActionAcquireExposure}
	shuffled.InstrumentClasses = []InstrumentClass{ClassSpotToken, ClassStableValue, ClassSpotToken}
	shuffled.DestinationAllowlist = []devnet.PublicKey{ukey("dest"), ukey("venue"), ukey("dest")}
	a, err := termsKey(c.Terms)
	require.NoError(t, err)
	b, err := termsKey(shuffled)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestCapabilityTermsValidation(t *testing.T) {
	c := unitCapability(t)
	require.NoError(t, c.Terms.Validate(unitNow))
	bad := map[string]func(*CapabilityTerms){
		"no actions":          func(t *CapabilityTerms) { t.ActionClasses = nil },
		"unknown action":      func(t *CapabilityTerms) { t.ActionClasses = []ActionClass{"WITHDRAW"} },
		"no classes":          func(t *CapabilityTerms) { t.InstrumentClasses = nil },
		"unknown class":       func(t *CapabilityTerms) { t.InstrumentClasses = []InstrumentClass{"EQUITY"} },
		"inverted limits":     func(t *CapabilityTerms) { t.MaxSingleTrade = money.USDFromMinor(600_000) },
		"zero single":         func(t *CapabilityTerms) { t.MaxSingleTrade = money.USDFromMinor(0) },
		"no destination":      func(t *CapabilityTerms) { t.DestinationAllowlist = nil },
		"zero destination":    func(t *CapabilityTerms) { t.DestinationAllowlist = []devnet.PublicKey{{}} },
		"no chain":            func(t *CapabilityTerms) { t.ChainAllowlist = nil },
		"unknown chain":       func(t *CapabilityTerms) { t.ChainAllowlist = []devnet.ChainID{"sbx-bitcoin"} },
		"already expired":     func(t *CapabilityTerms) { t.ExpiresAt = unitNow },
		"too long":            func(t *CapabilityTerms) { t.ExpiresAt = unitNow.Add(MaxCapabilityLifetime + time.Second) },
		"live mode":           func(t *CapabilityTerms) { t.Mode = gates.ModeLive },
		"paper mode":          func(t *CapabilityTerms) { t.Mode = gates.ModePaper },
		"no agent":            func(t *CapabilityTerms) { t.AgentID = "" },
		"bad environment":     func(t *CapabilityTerms) { t.Environment = "QA" },
		"no account":          func(t *CapabilityTerms) { t.AccountID = accounts.AccountID{} },
		"malformed strategy":  func(t *CapabilityTerms) { t.StrategyID = "not-a-uuid" },
		"too many addresses":  func(t *CapabilityTerms) { t.DestinationAllowlist = manyKeys(MaxDestinations + 1) },
		"expiry before grant": func(t *CapabilityTerms) { t.ExpiresAt = unitNow.Add(-time.Hour) },
	}
	for name, mutate := range bad {
		terms := c.Terms
		mutate(&terms)
		err := terms.Validate(unitNow)
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
}

func manyKeys(n int) []devnet.PublicKey {
	out := make([]devnet.PublicKey, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ukey("k"+itoa(i)))
	}
	return out
}

func TestClassifyAsset(t *testing.T) {
	stable := usdAsset(6)
	c, ok := ClassifyAsset(stable)
	require.True(t, ok)
	assert.Equal(t, ClassStableValue, c)

	spot := stable
	spot.IsStablecoin, spot.PegCurrency, spot.RiskClass = false, "", assets.RiskMajor
	c, ok = ClassifyAsset(spot)
	require.True(t, ok)
	assert.Equal(t, ClassSpotToken, c)

	spec := spot
	spec.RiskClass = assets.RiskSpeculative
	c, ok = ClassifyAsset(spec)
	require.True(t, ok)
	assert.Equal(t, ClassSpeculativeToken, c)

	unsupported := spot
	unsupported.RiskClass = assets.RiskUnsupported
	_, ok = ClassifyAsset(unsupported)
	assert.False(t, ok, "an unsupported asset is never delegated")

	real := spot
	real.Kind = assets.KindSPLToken
	_, ok = ClassifyAsset(real)
	assert.False(t, ok, "only SIMULATED assets have a delegation class")

	// A peg in another currency is not the USD stable class.
	eur := stable
	eur.PegCurrency = "EUR"
	c, ok = ClassifyAsset(eur)
	require.True(t, ok)
	assert.Equal(t, ClassSpotToken, c)
}

func TestTheActorsOfAGrantAreNeverAgents(t *testing.T) {
	for _, a := range RevokerActorTypes() {
		assert.NotEqual(t, security.ActorAgent, a)
	}
	for _, a := range VenueCreatorActorTypes() {
		assert.NotEqual(t, security.ActorAgent, a)
	}
	for _, a := range WithdrawalApproverActorTypes() {
		assert.NotEqual(t, security.ActorAgent, a)
	}
}
