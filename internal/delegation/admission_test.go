package delegation

import (
	"context"
	"crypto/ed25519"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxchain"
)

// TestAgentAdmissionExposesOnlyAdmissionAndTheCapabilityRead pins the
// facade's whole surface. An agent tree may propose, never sign or decide:
// the facade's method set is exactly the two admissions and the agent's own
// capability read, each with the signature of the admitter method it
// forwards to (which the Service exposes too), and the only thing it holds is
// the unexported admitter. A method added here -- or an accessor handing out
// what it holds -- fails this test before it can reach an agent.
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
		// The forwarded method has the same signature on the admitter and on
		// the Service, which admits through the same admitter: the facade
		// narrows what can be called, never what a call means.
		if want.name != "AgentCapabilities" {
			for _, owner := range []reflect.Type{reflect.TypeOf(&admitter{}), reflect.TypeOf(&Service{})} {
				sm, ok := owner.MethodByName(want.name)
				require.True(t, ok, "%s.%s", owner, want.name)
				assert.Equal(t, sm.Type.NumIn(), m.Type.NumIn())
				for i := 1; i < sm.Type.NumIn(); i++ {
					assert.Equal(t, sm.Type.In(i), m.Type.In(i))
				}
			}
		}
	}

	st := reflect.TypeOf(AgentAdmission{})
	require.Equal(t, 1, st.NumField(), "the facade holds the admitter and nothing else")
	f := st.Field(0)
	assert.False(t, f.IsExported(), "the admitter is never reachable through the facade")
	assert.Equal(t, reflect.TypeOf(&admitter{}), f.Type)

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
		Clock: clock.NewFake(unitNow), Environment: config.EnvTest,
		Host:                stubHost{cfg: devnethost.DefaultConfig("admission-unit", config.EnvTest, ukey("attester"))},
		EnabledCapabilities: []string{" agent_signing ", "AGENT_WITHDRAWALS", "NOT_A_CAPABILITY"},
	}
}

// TestNewAgentAdmissionRefusesAnIncompleteConfig: every collaborator the
// admission needs from its caller is required -- and none of them is a key.
func TestNewAgentAdmissionRefusesAnIncompleteConfig(t *testing.T) {
	t.Parallel()
	// The control: the complete configuration builds.
	a, err := NewAgentAdmission(completeAdmissionConfig())
	require.NoError(t, err)
	require.NotNil(t, a)

	for name, blank := range map[string]func(*AdmissionConfig){
		"clock":       func(c *AdmissionConfig) { c.Clock = nil },
		"environment": func(c *AdmissionConfig) { c.Environment = "" },
		"chain":       func(c *AdmissionConfig) { c.Host = nil },
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

// keyMaterial are the types that hold or carry private key material in
// delegated authority: the delegated signer (its master seed, in a
// closure), a venue co-signer (its key, in a closure), a devnet key pair, the
// sandbox chain seed every platform key derives from, and a raw Ed25519
// private key.
var keyMaterial = []reflect.Type{
	reflect.TypeOf(DelegatedSigner{}), reflect.TypeOf(VenueSigner{}), reflect.TypeOf(devnet.Keypair{}),
	reflect.TypeOf(sandboxchain.Seed{}), reflect.TypeOf(ed25519.PrivateKey(nil)),
}

// keyMaterialIn returns the key-material types (keyMaterial) among types,
// whether held directly or behind a pointer, and every unnamed byte array:
// the shape a seed is passed in ([32]byte, as the facade's configuration
// once took the signer's and the venues' seeds). A named one -- a public
// key, a hash -- says what it is.
func keyMaterialIn(types map[reflect.Type]bool) []string {
	var out []string
	for t := range types {
		if t.Kind() == reflect.Array && t.Elem().Kind() == reflect.Uint8 && t.Name() == "" {
			out = append(out, t.String())
		}
		for _, k := range keyMaterial {
			if t == k || t == reflect.PointerTo(k) {
				out = append(out, t.String())
			}
		}
	}
	return out
}

// declaredTypes is every type t's declaration reaches: struct fields,
// pointer, slice, array and map elements, map keys. An interface or a func
// is recorded and not opened -- what it holds is a value, which heldTypes
// walks.
func declaredTypes(t reflect.Type) map[reflect.Type]bool {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		if seen[t] {
			return
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				walk(t.Field(i).Type)
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(t.Elem())
		case reflect.Map:
			walk(t.Key())
			walk(t.Elem())
		}
	}
	walk(t)
	return seen
}

// heldTypes is the type of every value reachable from v: through pointers,
// interfaces, struct fields (exported or not), slices, arrays and maps. A
// closure's captured variables are out of reach of reflection, which is why
// keyMaterial names the types whose closures hold keys.
func heldTypes(v reflect.Value) map[reflect.Type]bool {
	type ref struct {
		ptr uintptr
		typ reflect.Type
	}
	seen := map[reflect.Type]bool{}
	visited := map[ref]bool{}
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		seen[v.Type()] = true
		switch v.Kind() {
		case reflect.Pointer, reflect.Map:
			if v.IsNil() || visited[ref{v.Pointer(), v.Type()}] {
				return
			}
			visited[ref{v.Pointer(), v.Type()}] = true
			if v.Kind() == reflect.Pointer {
				walk(v.Elem())
				return
			}
			for it := v.MapRange(); it.Next(); {
				walk(it.Key())
				walk(it.Value())
			}
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(v)
	return seen
}

// TestTheAgentAdmissionHoldsNoKeyMaterial proves what the agent runtime's
// facade holds: no delegated signer, no venue co-signer, no key pair and no
// seed -- neither a sandbox chain seed nor an unnamed byte array, the shape
// the signer's and the venues' seeds were once handed to it in. By its type
// -- the admitter it wraps declares no field, at any depth, that could hold
// one -- and by its value: everything reachable from the facade
// NewAgentAdmission builds, through every pointer, interface, struct, slice
// and map, is free of them. The walk can see them: a Service, which does
// sign, shows its signer and its co-signers to the same walk.
func TestTheAgentAdmissionHoldsNoKeyMaterial(t *testing.T) {
	t.Parallel()
	assert.Empty(t, keyMaterialIn(declaredTypes(reflect.TypeOf(AgentAdmission{}))),
		"the facade's type declares key material")

	a, err := NewAgentAdmission(completeAdmissionConfig())
	require.NoError(t, err)
	held := heldTypes(reflect.ValueOf(a))
	require.True(t, held[reflect.TypeOf(&admitter{})], "the walk reaches the admitter")
	require.True(t, held[reflect.TypeOf(stubHost{})], "the walk opens the interfaces it holds")
	assert.Empty(t, keyMaterialIn(held), "the facade holds key material")

	// The negative control: the walk finds a signer and a co-signer where
	// they are.
	signer, err := NewDelegatedSigner(useed("master"), SignerConfig{Environment: config.EnvTest, Clock: clock.NewFake(unitNow),
		Policy: DefaultSigningPolicy(), Fees: HostFees(stubHost{cfg: devnethost.DefaultConfig("admission-unit", config.EnvTest, ukey("attester"))})})
	require.NoError(t, err)
	venue := NewVenueSigner(useed("venue"))
	svc := &Service{admitter: *a.adm, signer: signer, venues: map[devnet.PublicKey]*VenueSigner{venue.PublicKey(): venue}}
	signers := []string{"*delegation.DelegatedSigner", "delegation.DelegatedSigner", "*delegation.VenueSigner", "delegation.VenueSigner"}
	assert.ElementsMatch(t, signers, keyMaterialIn(heldTypes(reflect.ValueOf(svc))))
	assert.Subset(t, keyMaterialIn(declaredTypes(reflect.TypeOf(Service{}))), signers)
}

// TestNewAgentAdmissionTakesNoSeedAndNoSigner pins what the agent runtime
// hands the facade: the deployment's settings and a read of the chain --
// none of them a seed, a key, a signer or a co-signer, and the chain one
// that cannot submit, produce a block or fund a wallet.
func TestNewAgentAdmissionTakesNoSeedAndNoSigner(t *testing.T) {
	t.Parallel()
	ct := reflect.TypeOf(AdmissionConfig{})
	var names []string
	for i := 0; i < ct.NumField(); i++ {
		names = append(names, ct.Field(i).Name)
	}
	assert.Equal(t, []string{"Clock", "Environment", "Host", "EnabledCapabilities", "Router", "BuildVersion"}, names,
		"the admission's configuration is the deployment's settings and the chain, nothing else")
	declared := declaredTypes(ct)
	assert.Empty(t, keyMaterialIn(declared))
	for typ := range declared {
		if typ.Kind() == reflect.Array && typ.Elem().Kind() == reflect.Uint8 {
			t.Errorf("the admission's configuration declares a byte array (%s), the shape of a seed", typ)
		}
	}

	host, ok := ct.FieldByName("Host")
	require.True(t, ok)
	assert.Equal(t, reflect.TypeOf((*ChainReader)(nil)).Elem(), host.Type, "the admission is handed a chain it can only read")
	reader := reflect.TypeOf((*ChainReader)(nil)).Elem()
	var methods []string
	for i := 0; i < reader.NumMethod(); i++ {
		methods = append(methods, reader.Method(i).Name)
	}
	assert.Equal(t, []string{"Config", "Health"}, methods, "a chain reader reads the configuration and the health, nothing else")

	nt := reflect.TypeOf(NewAgentAdmission)
	require.Equal(t, 1, nt.NumIn())
	assert.Equal(t, ct, nt.In(0), "NewAgentAdmission takes the configuration alone")
}

// keyQuerier answers the delegated wallet key read: the registered key, or
// none. Any other statement -- a write above all -- fails the test: an
// admission reads the key and registers nothing.
type keyQuerier struct {
	t   *testing.T
	key string
	sql []string
}

func (q *keyQuerier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	q.t.Errorf("an admission's key read wrote: %s", sql)
	return pgconn.CommandTag{}, errs.New(errs.CodeInternal, "keyQuerier: no writes")
}

func (q *keyQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	q.t.Errorf("unexpected query: %s", sql)
	return nil, errs.New(errs.CodeInternal, "keyQuerier: no queries")
}

func (q *keyQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.sql = append(q.sql, sql)
	return keyRow{key: q.key}
}

type keyRow struct{ key string }

func (r keyRow) Scan(dest ...any) error {
	if r.key == "" {
		return pgx.ErrNoRows
	}
	*(dest[0].(*string)) = r.key
	return nil
}

// TestAnAdmissionPlansFromTheRegisteredKeyOrRefuses: the wallet an
// admission plans from is the key registered at the grant, read and never
// derived or written; an account with none on the chain is refused with
// DELEGATION_SIGNER_KEY_NOT_REGISTERED, a FORBIDDEN that names the chain.
func TestAnAdmissionPlansFromTheRegisteredKeyOrRefuses(t *testing.T) {
	t.Parallel()
	account := accounts.AccountID(id.New[id.Any]())
	registered := ukey("registered wallet")
	q := &keyQuerier{t: t, key: registered.String()}
	got, err := admittedWallet(context.Background(), q, account, devnethost.ChainSolana)
	require.NoError(t, err)
	assert.Equal(t, registered, got)
	require.Len(t, q.sql, 1)
	assert.Contains(t, q.sql[0], "FROM delegated_signer_keys")

	none := &keyQuerier{t: t}
	_, err = admittedWallet(context.Background(), none, account, devnethost.ChainEthereum)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Equal(t, ReasonSignerKeyNotRegistered, reasonOf(err))
	assert.Contains(t, err.Error(), string(devnethost.ChainEthereum))
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
