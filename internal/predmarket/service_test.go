package predmarket

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/security"
)

func TestNewService_RefusesAMissingDependency(t *testing.T) {
	_, err := NewService(Config{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	e, ok := errs.As(err)
	require.True(t, ok)
	missing, _ := e.Fields["missing"].([]string)
	assert.Contains(t, missing, "Ledger")
	assert.Contains(t, missing, "Authorizer")
	assert.Contains(t, missing, "RiskPolicy", "a service without a risk policy is a wiring mistake")
	assert.NotContains(t, missing, "Oracle", "the oracle provider is optional")
}

func TestActors_OnlyOperatorsAndTheSchedulerRunMarkets(t *testing.T) {
	id := uuid.NewString()
	op, err := canonicalOperator(econ.Actor{Type: security.ActorOperator, ID: strings.ToUpper(id)})
	require.NoError(t, err)
	assert.Equal(t, id, op.ID, "operator ids are canonical, so dual control cannot be dodged by case")

	for _, bad := range []econ.Actor{
		{Type: security.ActorAgent, ID: id}, {Type: security.ActorUser, ID: id},
		{Type: security.ActorService, ID: id}, {Type: security.ActorSystem, ID: "cron"},
	} {
		_, err := canonicalOperator(bad)
		require.Error(t, err, bad.Type)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	}
	_, err = canonicalOperator(econ.Actor{Type: security.ActorOperator, ID: "alice"})
	require.Error(t, err, "an operator is a user id")

	sys, err := canonicalLifecycleActor(econ.Actor{Type: security.ActorSystem, ID: "scheduler"})
	require.NoError(t, err)
	assert.Equal(t, "scheduler", sys.ID)
	_, err = canonicalLifecycleActor(econ.Actor{Type: security.ActorAgent, ID: id})
	require.Error(t, err)

	require.Error(t, requireOtherThan(op, econ.Actor{Type: security.ActorOperator, ID: id}, "x"))
	require.NoError(t, requireOtherThan(op, econ.Actor{Type: security.ActorOperator, ID: uuid.NewString()}, "x"))
}

func validSpec() MarketSpec {
	pub, _, _ := ed25519.GenerateKey(nil)
	open := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	return MarketSpec{
		Symbol: "ELECTION-2026", Question: "Who wins?", Outcomes: []string{"YES", "NO"},
		CollateralAssetID: assets.NewAssetID(), Mode: gates.ModeSandbox, OpenAt: open, CloseAt: open.Add(time.Hour),
		DisputeWindow: time.Hour, OracleSource: "board", OraclePublicKey: pub,
	}
}

func TestMarketSpec_ValidateFailsClosed(t *testing.T) {
	require.NoError(t, validSpec().Validate())
	cases := map[string]func(*MarketSpec){
		"lowercase symbol":       func(s *MarketSpec) { s.Symbol = "election" },
		"long symbol":            func(s *MarketSpec) { s.Symbol = strings.Repeat("A", 33) },
		"empty question":         func(s *MarketSpec) { s.Question = "" },
		"padded question":        func(s *MarketSpec) { s.Question = " Who? " },
		"one outcome":            func(s *MarketSpec) { s.Outcomes = []string{"YES"} },
		"duplicate outcome":      func(s *MarketSpec) { s.Outcomes = []string{"YES", "YES"} },
		"bad outcome":            func(s *MarketSpec) { s.Outcomes = []string{"YES", "no"} },
		"too many outcomes":      func(s *MarketSpec) { s.Outcomes = make([]string, 65) },
		"no collateral":          func(s *MarketSpec) { s.CollateralAssetID = assets.AssetID{} },
		"live":                   func(s *MarketSpec) { s.Mode = gates.ModeLive },
		"devnet":                 func(s *MarketSpec) { s.Mode = gates.ModeDevnet },
		"close before open":      func(s *MarketSpec) { s.CloseAt = s.OpenAt },
		"nanosecond close":       func(s *MarketSpec) { s.CloseAt = s.CloseAt.Add(time.Nanosecond) },
		"negative window":        func(s *MarketSpec) { s.DisputeWindow = -time.Second },
		"window too long":        func(s *MarketSpec) { s.DisputeWindow = MaxDisputeWindow + time.Millisecond },
		"sub-millisecond window": func(s *MarketSpec) { s.DisputeWindow = time.Microsecond },
		"no oracle source":       func(s *MarketSpec) { s.OracleSource = "" },
		"bad oracle key":         func(s *MarketSpec) { s.OraclePublicKey = []byte("short") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mutate(&s)
			err := s.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestProp_DefinitionHashIsDeterministic: the definition hash depends on
// every field of the definition and on nothing else.
func TestProp_DefinitionHashIsDeterministic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := validSpec()
		s.Symbol = rapid.StringMatching(`[A-Z][A-Z0-9]{0,10}`).Draw(rt, "symbol")
		s.DisputeWindow = time.Duration(rapid.Int64Range(0, 1000).Draw(rt, "window")) * time.Millisecond
		a, err := s.DefinitionHash()
		if err != nil {
			rt.Fatalf("hash: %v", err)
		}
		b, err := s.DefinitionHash()
		if err != nil || string(a) != string(b) {
			rt.Fatalf("the same definition hashed twice differently")
		}
		changed := s
		changed.Outcomes = []string{"NO", "YES"}
		c, err := changed.DefinitionHash()
		if err != nil || string(a) == string(c) {
			rt.Fatalf("reordering the outcomes did not change the hash")
		}
	})
}
