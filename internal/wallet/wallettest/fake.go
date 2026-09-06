package wallettest

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/wallet"
)

// Name is the provider name the fake reports.
const Name = "fake"

// Fake is a deterministic wallet + signing provider.
type Fake struct {
	mu sync.Mutex

	env   config.Environment
	seed  string
	clk   clock.Clock
	calls []wallet.SignRequest

	failErr    error
	delay      time.Duration
	delegated  bool
	capability wallet.CapabilityState
	results    map[string]wallet.SignResult // by idempotency key
}

// Option configures a Fake.
type Option func(*Fake)

// WithSeed changes the key derivation seed (different seeds, different keys).
func WithSeed(seed string) Option { return func(f *Fake) { f.seed = seed } }

// WithClock injects the clock used for SignedAt.
func WithClock(c clock.Clock) Option { return func(f *Fake) { f.clk = c } }

// WithDelegationVerified sets the initial delegation/capability state.
func WithDelegationVerified(v bool) Option {
	return func(f *Fake) {
		f.delegated = v
		if v {
			f.capability = wallet.CapabilityVerified
		} else {
			f.capability = wallet.CapabilityUnverified
		}
	}
}

// New returns a Fake. It fails closed outside LOCAL/TEST/DEV.
func New(env config.Environment, opts ...Option) (*Fake, error) {
	if !env.AllowsFakeProviders() {
		return nil, errs.Newf(errs.CodeForbidden, "wallettest: fake provider is not permitted in %s", env)
	}
	f := &Fake{env: env, seed: "wallettest", clk: clock.System(), delegated: true, capability: wallet.CapabilityVerified, results: map[string]wallet.SignResult{}}
	for _, o := range opts {
		o(f)
	}
	return f, nil
}

// Name implements WalletProvider and SigningProvider.
func (f *Fake) Name() string { return Name }

// ProviderWalletID is the deterministic provider id for (account, chain).
func (f *Fake) ProviderWalletID(accountID, chain string) string {
	h := sha256.Sum256([]byte(f.seed + "|wallet|" + accountID + "|" + chain))
	return "fake-" + hex.EncodeToString(h[:12])
}

// KeyFor derives the wallet's private key from the provider wallet id.
func (f *Fake) KeyFor(providerWalletID string) solana.PrivateKey {
	h := sha256.Sum256([]byte(f.seed + "|key|" + providerWalletID))
	return solana.PrivateKey(ed25519.NewKeyFromSeed(h[:]))
}

// AddressFor is KeyFor(...).PublicKey() as base58.
func (f *Fake) AddressFor(providerWalletID string) string {
	return f.KeyFor(providerWalletID).PublicKey().String()
}

// FailWith makes every subsequent provider call return err (nil clears).
func (f *Fake) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = err
}

// Delay makes every subsequent call wait d (honoring ctx) before answering.
func (f *Fake) Delay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// SetDelegationVerified changes what VerifyDelegation and Capabilities report.
func (f *Fake) SetDelegationVerified(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delegated = v
	if v {
		f.capability = wallet.CapabilityVerified
	} else {
		f.capability = wallet.CapabilityUnverified
	}
}

// Calls returns a copy of every SignTransaction request received.
func (f *Fake) Calls() []wallet.SignRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wallet.SignRequest(nil), f.calls...)
}

// SignCalls returns the number of SignTransaction invocations.
func (f *Fake) SignCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *Fake) gate(ctx context.Context) error {
	f.mu.Lock()
	delay, failErr := f.delay, f.failErr
	f.mu.Unlock()
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return errs.Wrap(ctx.Err(), errs.CodeProviderUnavailable, "fake wallet provider timed out")
		case <-t.C:
		}
	}
	if failErr != nil {
		return failErr
	}
	return ctx.Err()
}

// CreateWallet implements WalletProvider.
func (f *Fake) CreateWallet(ctx context.Context, accountID, chain string) (wallet.Wallet, error) {
	if err := f.gate(ctx); err != nil {
		return wallet.Wallet{}, err
	}
	acct, err := accounts.ParseAccountID(accountID)
	if err != nil {
		return wallet.Wallet{}, errs.New(errs.CodeValidationFailed, "wallettest: account id must be a canonical uuid")
	}
	if chain == "" {
		return wallet.Wallet{}, errs.New(errs.CodeValidationFailed, "wallettest: chain required")
	}
	pid := f.ProviderWalletID(accountID, chain)
	return wallet.Wallet{
		AccountID:        acct,
		Provider:         Name,
		ProviderWalletID: pid,
		Chain:            chain,
		Address:          f.AddressFor(pid),
		Kind:             wallet.KindEmbeddedDelegated,
		Status:           wallet.StatusActive,
	}, nil
}

// GetWallet implements WalletProvider. Any id the fake could have issued is
// resolvable (keys are derived, not stored).
func (f *Fake) GetWallet(ctx context.Context, providerWalletID string) (wallet.Wallet, error) {
	if err := f.gate(ctx); err != nil {
		return wallet.Wallet{}, err
	}
	if len(providerWalletID) < 6 || providerWalletID[:5] != "fake-" {
		return wallet.Wallet{}, errs.New(errs.CodeNotFound, "wallettest: unknown provider wallet id")
	}
	return wallet.Wallet{
		Provider:         Name,
		ProviderWalletID: providerWalletID,
		Address:          f.AddressFor(providerWalletID),
		Kind:             wallet.KindEmbeddedDelegated,
		Status:           wallet.StatusActive,
	}, nil
}

// VerifyDelegation implements WalletProvider.
func (f *Fake) VerifyDelegation(ctx context.Context, providerWalletID string) (wallet.DelegationStatus, error) {
	if err := f.gate(ctx); err != nil {
		return wallet.DelegationStatus{}, err
	}
	f.mu.Lock()
	delegated := f.delegated
	f.mu.Unlock()
	st := wallet.DelegationStatus{
		ProviderWalletID: providerWalletID,
		State:            wallet.DelegationUnverified,
		PolicyID:         "fake-policy",
		PolicyVersion:    "fake-policy/1",
		SignerID:         "fake-signer",
		EvidenceRef:      "fake://delegation/" + providerWalletID,
		CheckedAt:        f.clk.Now(),
		Detail:           "fake provider",
	}
	if delegated {
		st.State = wallet.DelegationVerified
	}
	return st, nil
}

// Capabilities implements WalletProvider.
func (f *Fake) Capabilities(ctx context.Context) (wallet.Capability, error) {
	if err := f.gate(ctx); err != nil {
		return wallet.Capability{}, err
	}
	f.mu.Lock()
	state := f.capability
	f.mu.Unlock()
	return wallet.Capability{
		Provider:          Name,
		DelegatedSigning:  state,
		VerificationLabel: provider.CodeComplete,
		SupportedChains:   []string{"solana-devnet", "solana-mainnet", "solana-local"},
		ProbedAt:          f.clk.Now(),
		Detail:            "in-process fake; keys derived from the seed",
	}, nil
}

// SignTransaction implements SigningProvider: it signs the message with the
// wallet's derived key and returns the serialized signed transaction. The
// same idempotency key returns the previously produced result; the call is
// still recorded so tests can prove the signing service never asked twice.
func (f *Fake) SignTransaction(ctx context.Context, req wallet.SignRequest) (wallet.SignResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if err := req.Validate(); err != nil {
		return wallet.SignResult{}, err
	}
	if err := f.gate(ctx); err != nil {
		return wallet.SignResult{}, err
	}
	f.mu.Lock()
	if prev, ok := f.results[req.IdempotencyKey]; ok {
		f.mu.Unlock()
		return prev, nil
	}
	f.mu.Unlock()

	key := f.KeyFor(req.ProviderWalletID)
	pub := key.PublicKey()
	tx, err := solana.TransactionFromBytes(req.UnsignedTx)
	if err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeValidationFailed, "wallettest: transaction does not decode")
	}
	if len(tx.Message.AccountKeys) == 0 || !tx.Message.AccountKeys[0].Equals(pub) {
		return wallet.SignResult{}, errs.New(errs.CodeSigningRejected, "wallettest: fee payer is not this wallet")
	}
	sigs, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		if k.Equals(pub) {
			return &key
		}
		return nil
	})
	if err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeSigningRejected, "wallettest: signing failed")
	}
	if len(sigs) == 0 {
		return wallet.SignResult{}, errors.New("wallettest: no signature produced")
	}
	signed, err := tx.MarshalBinary()
	if err != nil {
		return wallet.SignResult{}, fmt.Errorf("wallettest: marshal signed: %w", err)
	}
	h := sha256.Sum256(signed)
	res := wallet.SignResult{
		SignedTx:    signed,
		Signature:   append([]byte(nil), sigs[0][:]...),
		ProviderRef: "fake:" + hex.EncodeToString(h[:8]),
		RetryClass:  provider.IdempotentWrite,
		SignedAt:    f.clk.Now(),
	}
	f.mu.Lock()
	f.results[req.IdempotencyKey] = res
	f.mu.Unlock()
	return res, nil
}

// Verify checks that signed carries a valid signature by the wallet.
func (f *Fake) Verify(providerWalletID string, signed []byte) error {
	tx, err := solana.TransactionFromBytes(signed)
	if err != nil {
		return err
	}
	return tx.VerifySignatures()
}
