package stableasset

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

// memEvents is an in-memory devnethost.Store: the same position rules as
// devnet_events, without a database.
type memEvents struct {
	mu     sync.Mutex
	events map[string][]devnethost.Event
}

func newMemEvents() *memEvents { return &memEvents{events: map[string][]devnethost.Event{}} }

func (m *memEvents) since(network string, after uint64) []devnethost.Event {
	var out []devnethost.Event
	for _, e := range m.events[network] {
		if e.Seq > after {
			e.Args = append([]byte(nil), e.Args...)
			e.RawTx = append([]byte(nil), e.RawTx...)
			out = append(out, e)
		}
	}
	return out
}

func (m *memEvents) Since(_ context.Context, _ db.Querier, network string, after uint64) ([]devnethost.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.since(network, after), nil
}

func (m *memEvents) Exclusive(_ context.Context, network string, after uint64, fn func([]devnethost.Event) (*devnethost.Event, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, err := fn(m.since(network, after))
	if err != nil || ev == nil {
		return err
	}
	if ev.Seq != uint64(len(m.events[network]))+1 {
		return errors.New("memEvents: not the next event")
	}
	m.events[network] = append(m.events[network], *ev)
	return nil
}

var providerClock = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// sandboxParts opens a devnet host for env over store and derives the
// provider's three signers from label.
func sandboxParts(t *testing.T, store devnethost.Store, network string, env config.Environment, label string) ProviderConfig {
	t.Helper()
	attester := devnet.NewKeypairFromSeed(seed("attester-" + label)).PublicKey()
	host, err := devnethost.Open(context.Background(), store, devnethost.DefaultConfig(network, env, attester), clock.NewFake(providerClock))
	require.NoError(t, err)
	return ProviderConfig{
		Host: host, Issuer: NewIssuerSigner(seed("issuer-" + label)),
		Custody: NewCustodialWalletSigner(seed("custody-" + label)), Snapshots: NewSnapshotSigner(seed("snapshots-" + label)),
	}
}

func providerReason(t *testing.T, err error) any {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	return e.Fields["reason"]
}

// TestNewProvider_RefusesLiveEverywhereAndFakeWhereItIsNotAllowed is the
// constructor's whole table, every environment against every mode: live is
// refused in every environment (it does not exist), fake is refused where
// the environment refuses fakes and, where it allows them, because no fake
// provider exists; sandbox is the devnet host and the signers everywhere --
// a host opened for that environment, and only that one.
func TestNewProvider_RefusesLiveEverywhereAndFakeWhereItIsNotAllowed(t *testing.T) {
	store := newMemEvents()
	for _, env := range append(config.Environments(), config.Environment("NOWHERE")) {
		hostEnv := env
		if !env.IsValid() {
			hostEnv = config.EnvTest
		}
		parts := sandboxParts(t, store, "prov-"+strings.ToLower(string(hostEnv)), hostEnv, "matrix")
		for _, mode := range []config.ProviderMode{config.ProviderModeFake, config.ProviderModeSandbox, config.ProviderModeLive, "carrier-pigeon"} {
			t.Run(string(env)+"/"+string(mode), func(t *testing.T) {
				p, err := NewProvider(env, mode, parts)
				switch {
				case mode == config.ProviderModeLive:
					assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), "live, even where the environment is unknown")
					assert.Equal(t, ReasonLiveProviderUnsupported, providerReason(t, err))
				case !mode.IsValid(), !env.IsValid():
					assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
				case mode == config.ProviderModeFake && !env.AllowsFakeProviders():
					assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
					assert.Equal(t, ReasonProviderModeNotAllowed, providerReason(t, err))
				case mode == config.ProviderModeFake:
					assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
					assert.Equal(t, ReasonProviderModeUnimplemented, providerReason(t, err))
				default:
					require.NoError(t, err)
					assert.Equal(t, config.ProviderModeSandbox, p.Mode())
					assert.Equal(t, "prov-"+strings.ToLower(string(env)), p.Network())
				}
				if err != nil {
					assert.Nil(t, p, "a refusal builds nothing")
				}
			})
		}
		if env.IsValid() {
			_, err := NewProvider(env, ProviderModeFor(env), parts)
			assert.NoError(t, err, "%s: the derived mode is one this environment runs", env)
		}
	}

	// A host opened for another environment -- one whose fault injection
	// the asked-for environment would not allow -- is refused.
	local := sandboxParts(t, store, "prov-mismatch", config.EnvLocal, "matrix")
	_, err := NewProvider(config.EnvProd, config.ProviderModeSandbox, local)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	// A provider is whole or it is nothing.
	partial := local
	partial.Custody = nil
	_, err = NewProvider(config.EnvLocal, config.ProviderModeSandbox, partial)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
}

// mintPlanOn is a valid mint plan on p's chain at its current tip.
func mintPlanOn(t *testing.T, p StableAssetProvider, mint, recipient devnet.PublicKey, amount uint64) MintPlan {
	t.Helper()
	tip, err := p.Tip(context.Background(), devnethost.ChainSolana)
	require.NoError(t, err)
	auth := p.MintAuthority().String()
	return MintPlan{
		Version: mintPlanVersion, RequestID: uuid.NewString(), StableAssetID: "s", AccountID: "a", Chain: string(devnethost.ChainSolana),
		Mint: mint.String(), Recipient: recipient.String(), Amount: strconv.FormatUint(amount, 10),
		MintAuthority: auth, FeePayer: auth, RecentBlockhash: tip.Hash.String(), BlockhashHeight: strconv.FormatUint(tip.Height, 10),
		ReserveAssetID: "x", ReserveQuantity: "1", ReserveFactor: "10000",
		ReservePrice: priceDoc(price(1, 0)), Peg: priceDoc(price(1, 0)),
	}
}

// finalize produces enough blocks for a transaction submitted now to be
// final, and returns its status, the mint's supply and the holder's balance
// read in one view.
func finalize(t *testing.T, p StableAssetProvider, tx devnet.TxID, mint, holder devnet.PublicKey) (devnet.TxStatus, uint64, uint64) {
	t.Helper()
	ctx := context.Background()
	params, ok := p.ChainParams(devnethost.ChainSolana)
	require.True(t, ok)
	_, err := p.ProduceBlocks(ctx, devnethost.ChainSolana, params.FinalityDepth+2)
	require.NoError(t, err)
	var st devnet.TxStatus
	var supply, balance uint64
	require.NoError(t, p.ViewIn(ctx, nil, func(v ChainView) error {
		var err error
		if st, err = v.Status(devnethost.ChainSolana, tx); err != nil {
			return err
		}
		if supply, err = v.Supply(devnethost.ChainSolana, mint); err != nil {
			return err
		}
		balance, err = v.TokenBalance(devnethost.ChainSolana, holder, mint)
		return err
	}))
	return st, supply, balance
}

// TestContract_SandboxProviderKeepsTheProviderContract runs the sandbox
// provider through every promise of StableAssetProvider: a mint its custody
// signs only as planned lands once however often it is submitted and is read
// back consistently; a holder's burn is signed by that holder's wallet only;
// a snapshot signature verifies under the snapshot key and nothing else;
// wallets are a function of the account, across a restart; and nothing it
// prints is a key.
func TestContract_SandboxProviderKeepsTheProviderContract(t *testing.T) {
	ctx := context.Background()
	store := newMemEvents()
	parts := sandboxParts(t, store, "prov-contract", config.EnvTest, "contract")
	p, err := NewProvider(config.EnvTest, config.ProviderModeSandbox, parts)
	require.NoError(t, err)
	assert.Equal(t, "prov-contract", p.Network())
	for _, chain := range devnethost.Chains() {
		params, ok := p.ChainParams(chain)
		assert.True(t, ok, chain)
		assert.Positive(t, params.FinalityDepth)
	}
	_, ok := p.ChainParams("sbx-elsewhere")
	assert.False(t, ok, "a chain the provider does not run")

	// Wallets: a function of the account, the same after a restart.
	alice, bob := accounts.AccountID(accounts.NewAccountID()), accounts.AccountID(accounts.NewAccountID())
	assert.Equal(t, p.HolderAddress(alice), p.HolderAddress(alice))
	assert.NotEqual(t, p.HolderAddress(alice), p.HolderAddress(bob))
	restarted, err := NewProvider(config.EnvTest, config.ProviderModeSandbox, sandboxParts(t, store, "prov-contract", config.EnvTest, "contract"))
	require.NoError(t, err)
	assert.Equal(t, p.HolderAddress(alice), restarted.HolderAddress(alice), "a restart recovers every wallet")
	assert.Equal(t, p.MintAuthority(), restarted.MintAuthority())

	// The mint exists exactly once, with the custodied authority.
	mint := MintAddress(p.Network(), "PROVIDER CONTRACT")
	cfg := devnet.MintConfig{Address: mint, Decimals: 6, Authority: p.MintAuthority()}
	require.NoError(t, p.EnsureMint(ctx, devnethost.ChainSolana, cfg))
	require.NoError(t, p.EnsureMint(ctx, devnethost.ChainSolana, cfg), "ensuring it again changes nothing")
	other := cfg
	other.Decimals = 8
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(p.EnsureMint(ctx, devnethost.ChainSolana, other)))

	// A mint lands once, however often its bytes are submitted.
	require.NoError(t, p.EnsureNativeBalance(ctx, devnethost.ChainSolana, p.MintAuthority(), 1_000_000, 100_000_000))
	holder := p.HolderAddress(alice)
	plan := mintPlanOn(t, p, mint, holder, 250)
	msg, err := plan.Message()
	require.NoError(t, err)
	signed, err := p.SignMint(plan, msg)
	require.NoError(t, err)
	id, err := p.Submit(ctx, devnethost.ChainSolana, signed.Raw)
	require.NoError(t, err)
	assert.Equal(t, signed.TxID, id)
	again, err := p.Submit(ctx, devnethost.ChainSolana, signed.Raw)
	require.NoError(t, err, "the chain acknowledges a transaction it already holds")
	assert.Equal(t, id, again)
	st, supply, balance := finalize(t, p, id, mint, holder)
	assert.Equal(t, devnet.TxFinalized, st.State)
	assert.Equal(t, uint64(250), supply, "applied once")
	assert.Equal(t, uint64(250), balance)

	// Custody signs only the plan's own mint, as the mint authority alone.
	tampered := msg
	tampered.Instructions = append([]devnet.Instruction(nil), msg.Instructions...)
	mintTo := tampered.Instructions[0].(devnet.TokenMintTo)
	mintTo.Amount = 251
	tampered.Instructions[0] = mintTo
	_, err = p.SignMint(plan, tampered)
	assert.Equal(t, errs.CodeSigningRejected, errs.CodeOf(err), "a message that is not the plan's is never signed")

	// A burn is signed by the holder's own wallet, and only for the holder.
	require.NoError(t, p.EnsureNativeBalance(ctx, devnethost.ChainSolana, holder, 1_000_000, 100_000_000))
	tip, err := p.Tip(ctx, devnethost.ChainSolana)
	require.NoError(t, err)
	burn := BurnPlan{
		Version: burnPlanVersion, RequestID: uuid.NewString(), StableAssetID: "s", AccountID: alice.String(),
		Chain: string(devnethost.ChainSolana), Mint: mint.String(), Holder: holder.String(), Amount: "100", FeePayer: holder.String(),
		RecentBlockhash: tip.Hash.String(), BlockhashHeight: strconv.FormatUint(tip.Height, 10), PayoutAssetID: "p",
		PayoutQuantity: "100", RedemptionMode: "INSTANT", Peg: priceDoc(price(1, 0)),
	}
	burnMsg, err := burn.Message()
	require.NoError(t, err)
	_, err = p.SignHolderBurn(bob, burn, burnMsg)
	assert.Equal(t, errs.CodeSigningRejected, errs.CodeOf(err), "bob's wallet never signs alice's burn")
	burned, err := p.SignHolderBurn(alice, burn, burnMsg)
	require.NoError(t, err)
	_, err = p.Submit(ctx, devnethost.ChainSolana, burned.Raw)
	require.NoError(t, err)
	st, supply, balance = finalize(t, p, burned.TxID, mint, holder)
	assert.Equal(t, devnet.TxFinalized, st.State)
	assert.Equal(t, uint64(150), supply)
	assert.Equal(t, uint64(150), balance)

	// A snapshot signature verifies under the snapshot key, for its own
	// hash, and only as a snapshot signature.
	hash := sha256.Sum256([]byte("a snapshot body"))
	sig := p.SignSnapshot(hash)
	assert.True(t, ed25519.Verify(p.SnapshotKey(), snapshotSigningMessage(hash), sig))
	assert.False(t, ed25519.Verify(p.SnapshotKey(), snapshotSigningMessage(sha256.Sum256([]byte("another"))), sig))
	assert.False(t, ed25519.Verify(p.SnapshotKey(), hash[:], sig), "the signed message is domain-separated")
	key := p.SnapshotKey()
	key[0] ^= 0xff
	assert.NotEqual(t, key, p.SnapshotKey(), "the key handed out is a copy")

	// Nothing the provider prints is a secret: only its network and public
	// keys.
	printed := fmt.Sprintf("%v %+v %s", p, p, p)
	for _, secret := range [][32]byte{seed("issuer-contract"), seed("custody-contract"), seed("snapshots-contract")} {
		assert.NotContains(t, printed, fmt.Sprintf("%x", secret[:]))
	}
	assert.Contains(t, printed, p.MintAuthority().String())
}

// liveProvider claims a mode no provider may have.
type liveProvider struct{ *SandboxProvider }

func (liveProvider) Mode() config.ProviderMode { return config.ProviderModeLive }

func providerDeps(t *testing.T) Deps {
	t.Helper()
	clk := clock.NewFake(providerClock)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("unit")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	return Deps{
		DB: new(db.DB), Clock: clk, Intents: econ.NewIntentStore(clk, out, aw), Effects: econ.NewEffectStore(clk, out, aw),
		Authorizer: econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{})), Facilities: econ.NewRegistry(),
		Ledger: ledger.NewService(clk, "unit"), Capital: capital.NewService(clk, nopUnitEmitter{}), Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Prices: valuation.NewPriceStore(clk),
		Kills: killswitch.NewChecker(killswitch.Policy{}), Reconciliation: reconciliation.NewRepository(clk, out, aw), Audit: aw,
		Accounts: accounts.NewRepository(), Assets: assets.NewRepository(),
	}
}

type nopUnitEmitter struct{}

func (nopUnitEmitter) Emit(context.Context, pgx.Tx, string, any) error { return nil }

// TestNew_TakesTheProviderOnceAndRefusesALiveOne: the service issues through
// the provider it is given -- or through the sandbox provider its four parts
// make, exactly as NewProvider makes it -- never through both, never through
// a partial one, and never through a live one.
func TestNew_TakesTheProviderOnceAndRefusesALiveOne(t *testing.T) {
	store := newMemEvents()
	parts := sandboxParts(t, store, "prov-new", config.EnvTest, "new")
	p, err := NewProvider(config.EnvTest, ProviderModeFor(config.EnvTest), parts)
	require.NoError(t, err)

	d := providerDeps(t)
	d.Provider = p
	svc, err := New(d)
	require.NoError(t, err)
	assert.Same(t, p, svc.Provider())
	assert.Equal(t, "prov-new", svc.Network())

	d = providerDeps(t)
	d.Host, d.Issuer, d.Custody, d.Snapshots = parts.Host, parts.Issuer, parts.Custody, parts.Snapshots
	svc, err = New(d)
	require.NoError(t, err)
	assert.Equal(t, config.ProviderModeSandbox, svc.Provider().Mode(), "the parts are the sandbox provider")
	assert.Equal(t, p.MintAuthority(), svc.Provider().MintAuthority())

	d.Provider = p
	_, err = New(d)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "the provider given twice")

	d = providerDeps(t)
	d.Host, d.Issuer = parts.Host, parts.Issuer
	_, err = New(d)
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.ElementsMatch(t, []string{"Custody", "Snapshots"}, e.Fields["missing"], "a partial provider names what is missing")

	d = providerDeps(t)
	d.Provider = liveProvider{p.(*SandboxProvider)}
	_, err = New(d)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.Equal(t, ReasonLiveProviderUnsupported, providerReason(t, err))
}
