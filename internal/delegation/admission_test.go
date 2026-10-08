package delegation

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// TestAgentAdmissionExposesOnlyAdmissionAndTheCapabilityRead pins the
// facade's whole surface. An agent tree may propose, never sign or decide:
// the facade's method set is exactly the two admissions and the agent's own
// capability read, each with the signature of the Service method it forwards
// to, and the only thing it holds is the unexported Service. A method added
// here -- or an accessor handing out the Service, its signer or its seeds --
// fails this test before it can reach an agent.
func TestAgentAdmissionExposesOnlyAdmissionAndTheCapabilityRead(t *testing.T) {
	t.Parallel()
	ptr := reflect.TypeOf(&AgentAdmission{})
	var names []string
	for i := 0; i < ptr.NumMethod(); i++ {
		names = append(names, ptr.Method(i).Name)
	}
	assert.Equal(t, []string{"AgentCapabilities", "ProposeWithdrawal", "RequestSwap"}, names,
		"the facade exposes the two admissions and the capability read, nothing else")
	assert.Zero(t, reflect.TypeOf(AgentAdmission{}).NumMethod(), "no method on the value receiver either")

	ctxT := reflect.TypeOf((*context.Context)(nil)).Elem()
	txT := reflect.TypeOf((*pgx.Tx)(nil)).Elem()
	querierT := reflect.TypeOf((*db.Querier)(nil)).Elem()
	errT := reflect.TypeOf((*error)(nil)).Elem()
	for _, want := range []struct {
		name string
		in   []reflect.Type
		out  []reflect.Type
	}{
		{"RequestSwap", []reflect.Type{ctxT, txT, reflect.TypeOf(SwapRequest{})}, []reflect.Type{reflect.TypeOf(SwapTicket{}), errT}},
		{"ProposeWithdrawal", []reflect.Type{ctxT, txT, reflect.TypeOf(ProposalRequest{})}, []reflect.Type{reflect.TypeOf(WithdrawalTicket{}), errT}},
		{"AgentCapabilities", []reflect.Type{ctxT, querierT, reflect.TypeOf(accounts.AccountID{})}, []reflect.Type{reflect.TypeOf([]Capability(nil)), errT}},
	} {
		m, ok := ptr.MethodByName(want.name)
		require.True(t, ok, want.name)
		require.Equal(t, len(want.in)+1, m.Type.NumIn(), want.name)
		for i, in := range want.in {
			assert.Equal(t, in, m.Type.In(i+1), "%s argument %d", want.name, i)
		}
		require.Equal(t, len(want.out), m.Type.NumOut(), want.name)
		for i, out := range want.out {
			assert.Equal(t, out, m.Type.Out(i), "%s result %d", want.name, i)
		}
		// The forwarded Service method has the same signature: the facade
		// narrows what can be called, never what a call means.
		if want.name != "AgentCapabilities" {
			sm, ok := reflect.TypeOf(&Service{}).MethodByName(want.name)
			require.True(t, ok)
			assert.Equal(t, sm.Type.NumIn(), m.Type.NumIn())
			for i := 1; i < sm.Type.NumIn(); i++ {
				assert.Equal(t, sm.Type.In(i), m.Type.In(i))
			}
		}
	}

	st := reflect.TypeOf(AgentAdmission{})
	require.Equal(t, 1, st.NumField(), "the facade holds the Service and nothing else")
	f := st.Field(0)
	assert.False(t, f.IsExported(), "the Service is never reachable through the facade")
	assert.Equal(t, reflect.TypeOf(&Service{}), f.Type)

	// Whatever is added later, these never belong on the agent's side.
	for _, name := range names {
		for _, forbidden := range []string{"Sign", "Execute", "Decide", "Grant", "Revoke", "Venue", "Policy", "Settle",
			"Submit", "Approve", "Cancel", "Expire", "Reconcile", "Destination", "Service", "Signer", "Seed", "Key"} {
			assert.NotContains(t, name, forbidden)
		}
	}
}

// stubHost is a ChainHost that is never dialed: construction only reads its
// configuration (for the fee each chain charges).
type stubHost struct{ cfg devnethost.Config }

func (h stubHost) Config() devnethost.Config { return h.cfg }
func (stubHost) ViewIn(context.Context, db.Querier, func(v devnethost.View) error) error {
	return errs.New(errs.CodeInternal, "stub host")
}
func (stubHost) Submit(context.Context, devnet.ChainID, []byte) (devnet.TxID, error) {
	return "", errs.New(errs.CodeInternal, "stub host")
}
func (stubHost) GetTransaction(context.Context, devnet.ChainID, devnet.TxID) (devnet.TxRecord, error) {
	return devnet.TxRecord{}, errs.New(errs.CodeInternal, "stub host")
}
func (stubHost) ProduceBlocks(context.Context, devnet.ChainID, uint64) (devnet.BlockInfo, error) {
	return devnet.BlockInfo{}, errs.New(errs.CodeInternal, "stub host")
}
func (stubHost) EnsureNativeBalance(context.Context, devnet.ChainID, devnet.PublicKey, uint64, uint64) error {
	return errs.New(errs.CodeInternal, "stub host")
}
func (stubHost) Health(context.Context, devnet.ChainID) error {
	return errs.New(errs.CodeInternal, "stub host")
}

func completeAdmissionConfig() AdmissionConfig {
	return AdmissionConfig{
		DB: new(db.DB), Clock: clock.NewFake(unitNow), Environment: config.EnvTest,
		Host:                stubHost{cfg: devnethost.DefaultConfig("admission-unit", config.EnvTest, ukey("attester"))},
		EnabledCapabilities: []string{" agent_signing ", "AGENT_WITHDRAWALS", "NOT_A_CAPABILITY"},
		SignerSeed:          useed("master"),
		VenueSeeds:          map[string][devnet.SeedSize]byte{"venue-b": useed("venue-b"), "venue-a": useed("venue-a")},
	}
}

// TestNewAgentAdmissionRefusesAnIncompleteConfig: every collaborator the
// admission needs from its caller is required, and a zero signer seed -- the
// one secret input -- is refused rather than defaulted.
func TestNewAgentAdmissionRefusesAnIncompleteConfig(t *testing.T) {
	t.Parallel()
	// The control: the complete configuration builds.
	a, err := NewAgentAdmission(completeAdmissionConfig())
	require.NoError(t, err)
	require.NotNil(t, a)

	for name, blank := range map[string]func(*AdmissionConfig){
		"database":    func(c *AdmissionConfig) { c.DB = nil },
		"clock":       func(c *AdmissionConfig) { c.Clock = nil },
		"environment": func(c *AdmissionConfig) { c.Environment = "" },
		"chain":       func(c *AdmissionConfig) { c.Host = nil },
		"signer seed": func(c *AdmissionConfig) { c.SignerSeed = [32]byte{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := completeAdmissionConfig()
			blank(&c)
			out, err := NewAgentAdmission(c)
			require.Errorf(t, err, "a missing %s was accepted", name)
			assert.Nil(t, out)
		})
	}
}

// TestCapabilityUsableAt: a capability is offered only while it is ACTIVE,
// inside its lifetime and intact.
func TestCapabilityUsableAt(t *testing.T) {
	t.Parallel()
	c := unitCapability(t)
	assert.True(t, c.UsableAt(unitNow), "usable from the instant of its grant")
	assert.True(t, c.UsableAt(c.Terms.ExpiresAt.Add(-time.Microsecond)), "usable up to its expiry")
	assert.False(t, c.UsableAt(c.Terms.ExpiresAt), "expired at its expiry")
	assert.False(t, c.UsableAt(unitNow.Add(-time.Second)), "not before its grant")

	revoked := c
	revoked.Status = CapabilityRevoked
	assert.False(t, revoked.UsableAt(unitNow.Add(time.Hour)))

	tampered := c
	tampered.Terms.MaxSingleTrade = money.USDFromMinor(tampered.Terms.MaxSingleTrade.Minor() + 1)
	assert.False(t, tampered.UsableAt(unitNow.Add(time.Hour)), "a row edited behind the hash reads as unusable")

	upper := c
	upper.Terms.AgentID = strings.ToUpper(upper.Terms.AgentID)
	assert.True(t, upper.UsableAt(unitNow.Add(time.Hour)), "an id's case is normalized before hashing, as Check does")
}
