package stableasset

import (
	"context"
	"crypto/ed25519"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
)

// StableAssetProvider is the external system a platform stable asset depends
// on (advanced goal §27): the chain its mint authority mints on and its
// holders burn on, and the custody of the keys that sign there -- the mint
// authority, every holder's wallet and the reserve snapshot key. The service
// reaches neither the chain nor a key except through it.
//
// Its promises, which TestContract_SandboxProviderKeepsTheProviderContract
// checks for the sandbox provider:
//
//   - Effects on the chain (EnsureMint, EnsureNativeBalance, Submit,
//     ProduceBlocks) are calls outside any database transaction, recorded
//     by the provider itself: a caller's rollback cannot undo one. A
//     transaction the chain already holds is acknowledged without being
//     applied twice.
//   - ViewIn is one consistent read of the chains at one instant; a caller
//     inside a transaction passes it as q, and the provider reads only
//     through it (never a second pooled connection, F-27).
//   - Signing is custody, not authority: SignMint signs only a mint whose
//     message the inspector proved is exactly its plan, with the mint
//     authority as the only signer; SignHolderBurn only a burn of the
//     account's own wallet against its plan; SignSnapshot only a reserve
//     snapshot hash, domain-separated. No method returns, logs or exposes a
//     private key or seed.
//   - HolderAddress is a function of the account alone, the same on every
//     chain and across restarts.
type StableAssetProvider interface {
	// Mode is the provider mode the provider was built for.
	Mode() config.ProviderMode
	StableChain
	StableCustody
}

// StableChain is the chain half of a StableAssetProvider.
type StableChain interface {
	// Network names the chain network; every stable asset the provider
	// serves is registered on it.
	Network() string
	// ChainParams returns a chain's block-production parameters (finality
	// depth, blockhash window), false when the provider does not run it.
	ChainParams(chain devnet.ChainID) (devnethost.ChainParams, bool)
	// EnsureMint creates cfg's mint if it does not exist; an existing mint
	// must be exactly cfg.
	EnsureMint(ctx context.Context, chain devnet.ChainID, cfg devnet.MintConfig) error
	// EnsureNativeBalance keeps owner able to pay network fees: below
	// minimum, it is topped up to topUp.
	EnsureNativeBalance(ctx context.Context, chain devnet.ChainID, owner devnet.PublicKey, minimum, topUp uint64) error
	// Submit hands signed bytes to the chain once.
	Submit(ctx context.Context, chain devnet.ChainID, raw []byte) (devnet.TxID, error)
	// Tip returns the chain's newest block.
	Tip(ctx context.Context, chain devnet.ChainID) (devnet.BlockInfo, error)
	// ProduceBlocks produces up to n blocks (at most devnethost.
	// MaxBlocksPerCall per call): only where the platform is the block
	// producer.
	ProduceBlocks(ctx context.Context, chain devnet.ChainID, n uint64) (devnet.BlockInfo, error)
	// ViewIn runs fn with one consistent read of the chains, catching up
	// through q when the caller is inside a transaction.
	ViewIn(ctx context.Context, q db.Querier, fn func(v ChainView) error) error
}

// ChainView is one consistent read of a provider's chains: every read through
// one view sees the same instant. It is valid only inside the callback it was
// passed to. devnethost.View is one.
type ChainView interface {
	Status(chain devnet.ChainID, id devnet.TxID) (devnet.TxStatus, error)
	IsBlockhashValid(chain devnet.ChainID, bh devnet.Hash) (bool, error)
	FinalizedHeight(chain devnet.ChainID) (uint64, error)
	Tip(chain devnet.ChainID) (devnet.BlockInfo, error)
	Supply(chain devnet.ChainID, mint devnet.PublicKey) (uint64, error)
	TokenBalance(chain devnet.ChainID, owner, mint devnet.PublicKey) (uint64, error)
}

// StableCustody is the key-custody half of a StableAssetProvider.
type StableCustody interface {
	// MintAuthority is the address whose key the provider custodies as the
	// stable asset's mint authority (and fee payer of its mints).
	MintAuthority() devnet.PublicKey
	// SignMint inspects msg against plan and, only if it is exactly the
	// plan's mint, signs it as the mint authority.
	SignMint(plan MintPlan, msg devnet.Message) (SignedTransaction, error)
	// HolderAddress is the account's custodied wallet address.
	HolderAddress(account accounts.AccountID) devnet.PublicKey
	// SignHolderBurn inspects msg against plan and, only if it is exactly
	// the plan's burn from the account's own wallet, signs it as that wallet.
	SignHolderBurn(account accounts.AccountID, plan BurnPlan, msg devnet.Message) (SignedTransaction, error)
	// SnapshotKey is the key a reserve snapshot's signature verifies under.
	SnapshotKey() ed25519.PublicKey
	// SignSnapshot signs a reserve snapshot's body hash (and nothing else:
	// the signed message is domain-separated, snapshotSigningMessage).
	SignSnapshot(bodyHash [32]byte) []byte
}

// ProviderConfig are the parts of the sandbox provider: the persistent devnet
// host (internal/devnethost) and the three in-process signers whose keys
// derive from the sandbox seed (internal/sandboxchain).
type ProviderConfig struct {
	Host      *devnethost.Host
	Issuer    *IssuerSigner
	Custody   *CustodialWalletSigner
	Snapshots *SnapshotSigner
}

func (c ProviderConfig) missing() []string {
	var out []string
	if c.Host == nil {
		out = append(out, "Host")
	}
	if c.Issuer == nil {
		out = append(out, "Issuer")
	}
	if c.Custody == nil {
		out = append(out, "Custody")
	}
	if c.Snapshots == nil {
		out = append(out, "Snapshots")
	}
	return out
}

func (c ProviderConfig) isZero() bool { return len(c.missing()) == 4 }

// SandboxProvider is the stable asset's sandbox: the persistent simulated
// chain host and the in-process custody of the sandbox keys. Its mode is
// sandbox because it is the sandbox itself, not a stand-in for a chain or a
// custodian somewhere else (doc.go, "Provider"). It holds nothing mutable of
// its own; the host and the database hold the chain.
type SandboxProvider struct {
	host      *devnethost.Host
	issuer    *IssuerSigner
	custody   *CustodialWalletSigner
	snapshots *SnapshotSigner
}

func newSandboxProvider(c ProviderConfig) (*SandboxProvider, error) {
	if m := c.missing(); len(m) > 0 {
		return nil, errs.New(errs.CodeInternal, "stableasset: the sandbox provider is missing parts").WithField("missing", m)
	}
	return &SandboxProvider{host: c.Host, issuer: c.Issuer, custody: c.Custody, snapshots: c.Snapshots}, nil
}

// Mode implements StableAssetProvider.
func (p *SandboxProvider) Mode() config.ProviderMode { return config.ProviderModeSandbox }

// Network implements StableChain.
func (p *SandboxProvider) Network() string { return p.host.Config().Network }

// ChainParams implements StableChain.
func (p *SandboxProvider) ChainParams(chain devnet.ChainID) (devnethost.ChainParams, bool) {
	return p.host.Config().Params(chain)
}

// EnsureMint implements StableChain.
func (p *SandboxProvider) EnsureMint(ctx context.Context, chain devnet.ChainID, cfg devnet.MintConfig) error {
	return p.host.EnsureMint(ctx, chain, cfg)
}

// EnsureNativeBalance implements StableChain: the host's faucet.
func (p *SandboxProvider) EnsureNativeBalance(ctx context.Context, chain devnet.ChainID, owner devnet.PublicKey, minimum, topUp uint64) error {
	return p.host.EnsureNativeBalance(ctx, chain, owner, minimum, topUp)
}

// Submit implements StableChain.
func (p *SandboxProvider) Submit(ctx context.Context, chain devnet.ChainID, raw []byte) (devnet.TxID, error) {
	return p.host.Submit(ctx, chain, raw)
}

// Tip implements StableChain.
func (p *SandboxProvider) Tip(ctx context.Context, chain devnet.ChainID) (devnet.BlockInfo, error) {
	return p.host.Tip(ctx, chain)
}

// ProduceBlocks implements StableChain: in the sandbox the platform is the
// block producer.
func (p *SandboxProvider) ProduceBlocks(ctx context.Context, chain devnet.ChainID, n uint64) (devnet.BlockInfo, error) {
	return p.host.ProduceBlocks(ctx, chain, n)
}

// ViewIn implements StableChain.
func (p *SandboxProvider) ViewIn(ctx context.Context, q db.Querier, fn func(v ChainView) error) error {
	return p.host.ViewIn(ctx, q, func(v devnethost.View) error { return fn(v) })
}

// MintAuthority implements StableCustody.
func (p *SandboxProvider) MintAuthority() devnet.PublicKey { return p.issuer.PublicKey() }

// SignMint implements StableCustody.
func (p *SandboxProvider) SignMint(plan MintPlan, msg devnet.Message) (SignedTransaction, error) {
	return p.issuer.SignMint(plan, msg)
}

// HolderAddress implements StableCustody.
func (p *SandboxProvider) HolderAddress(account accounts.AccountID) devnet.PublicKey {
	return p.custody.HolderAddress(account)
}

// SignHolderBurn implements StableCustody.
func (p *SandboxProvider) SignHolderBurn(account accounts.AccountID, plan BurnPlan, msg devnet.Message) (SignedTransaction, error) {
	return p.custody.SignHolderBurn(account, plan, msg)
}

// SnapshotKey implements StableCustody.
func (p *SandboxProvider) SnapshotKey() ed25519.PublicKey { return p.snapshots.PublicKey() }

// SignSnapshot implements StableCustody.
func (p *SandboxProvider) SignSnapshot(bodyHash [32]byte) []byte {
	return p.snapshots.sign(snapshotSigningMessage(bodyHash))
}

// String names the provider by its network and public keys only.
func (p *SandboxProvider) String() string {
	return "SandboxProvider(" + p.Network() + ", " + p.issuer.String() + ", " + p.snapshots.String() + ")"
}

// Stable reasons for refusing a provider.
const (
	// ReasonLiveProviderUnsupported: no live stable asset provider exists in
	// this build. No environment, flag or configuration changes that.
	ReasonLiveProviderUnsupported = "STABLE_ASSET_LIVE_PROVIDER_UNSUPPORTED"
	// ReasonProviderModeNotAllowed: a fake provider never runs where the
	// environment refuses fakes (STAGING and PROD).
	ReasonProviderModeNotAllowed = "STABLE_ASSET_PROVIDER_MODE_NOT_ALLOWED"
	// ReasonProviderModeUnimplemented: this build has no fake stable asset
	// provider; the devnet host is the sandbox.
	ReasonProviderModeUnimplemented = "STABLE_ASSET_PROVIDER_MODE_UNIMPLEMENTED"
)

// NewProvider returns the stable asset provider for a provider mode in an
// environment. Live is refused unconditionally, before the environment is
// even looked at; fake is refused where the environment refuses fakes, and
// everywhere else because no fake provider exists; sandbox is the devnet
// host and the in-process signers, in every environment -- in STAGING and
// PROD the stable asset's gates still have to be ACTIVE before a mint
// reaches it (gates.DecideMode). The host must have been opened for env: its
// environment is what allows fault injection, which only an environment that
// allows fakes may have.
func NewProvider(env config.Environment, mode config.ProviderMode, cfg ProviderConfig) (StableAssetProvider, error) {
	if mode == config.ProviderModeLive {
		return nil, errs.New(errs.CodeUnsupported, "stableasset: no live stable asset provider exists in this build").
			WithField("reason", ReasonLiveProviderUnsupported)
	}
	if !mode.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "stableasset: unknown provider mode %q", mode)
	}
	if !env.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "stableasset: unknown environment %q", env)
	}
	if mode == config.ProviderModeFake {
		if !env.AllowsFakeProviders() {
			return nil, errs.Newf(errs.CodeForbidden, "stableasset: provider mode fake is not allowed in environment %s", env).
				WithField("reason", ReasonProviderModeNotAllowed)
		}
		return nil, errs.New(errs.CodeUnsupported, "stableasset: there is no fake stable asset provider; the devnet host is the sandbox").
			WithField("reason", ReasonProviderModeUnimplemented)
	}
	p, err := newSandboxProvider(cfg)
	if err != nil {
		return nil, err
	}
	if hostEnv := cfg.Host.Config().Environment; hostEnv != env {
		return nil, errs.Newf(errs.CodeValidationFailed, "stableasset: the chain host was opened for environment %s, not %s", hostEnv, env)
	}
	return p, nil
}

// ProviderModeFor is the provider mode a binary builds the stable asset
// provider in when no configuration names one: sandbox, in every
// environment. The devnet host is the only provider, and it is the sandbox;
// there is no fake to choose where fakes are allowed, and live is refused
// everywhere.
func ProviderModeFor(config.Environment) config.ProviderMode { return config.ProviderModeSandbox }

// checkProvider refuses a provider a Service must not issue through.
func checkProvider(p StableAssetProvider) error {
	switch m := p.Mode(); {
	case m == config.ProviderModeLive:
		return errs.New(errs.CodeUnsupported, "stableasset: a live stable asset provider is refused").
			WithField("reason", ReasonLiveProviderUnsupported)
	case !m.IsValid():
		return errs.Newf(errs.CodeInternal, "stableasset: the provider reports an unknown mode %q", m)
	}
	return nil
}
