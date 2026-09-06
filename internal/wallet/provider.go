package wallet

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
)

// WalletProvider creates and describes provider-managed wallets.
//
// CreateWallet returns a Wallet with the provider fields populated (ID is
// zero; the Repository assigns it). GetWallet and VerifyDelegation take the
// provider's wallet identifier. Capabilities is the PART 96 probe: whether
// delegated signing is verifiably available with this provider and
// configuration.
type WalletProvider interface {
	Name() string
	CreateWallet(ctx context.Context, accountID, chain string) (Wallet, error)
	GetWallet(ctx context.Context, providerWalletID string) (Wallet, error)
	VerifyDelegation(ctx context.Context, providerWalletID string) (DelegationStatus, error)
	Capabilities(ctx context.Context) (Capability, error)
}

// Purpose labels why a signature is requested; it is recorded as evidence
// and may be constrained by provider policy.
type Purpose string

// Signing purposes.
const (
	PurposeSwap Purpose = "SWAP"
	PurposeTest Purpose = "TEST"
)

// Valid reports whether p is a declared purpose.
func (p Purpose) Valid() bool { return p == PurposeSwap || p == PurposeTest }

// SignRequest asks a SigningProvider to sign an already inspected
// transaction. IdempotencyKey is the execution attempt id: the same key must
// never produce two different signed transactions.
type SignRequest struct {
	ProviderWalletID string
	Chain            string
	UnsignedTx       []byte
	IdempotencyKey   string
	Purpose          Purpose
}

// Validate checks the request shape (never the transaction semantics; that
// is the inspector's job).
func (r SignRequest) Validate() error {
	switch {
	case r.ProviderWalletID == "":
		return errs.New(errs.CodeValidationFailed, "sign: provider wallet id required")
	case r.Chain == "":
		return errs.New(errs.CodeValidationFailed, "sign: chain required")
	case len(r.UnsignedTx) == 0:
		return errs.New(errs.CodeValidationFailed, "sign: unsigned transaction required")
	case r.IdempotencyKey == "":
		return errs.New(errs.CodeValidationFailed, "sign: idempotency key required")
	case !r.Purpose.Valid():
		return errs.Newf(errs.CodeValidationFailed, "sign: unknown purpose %q", r.Purpose)
	}
	return nil
}

// SignResult is the provider's signature over exactly the requested message.
type SignResult struct {
	// SignedTx is the fully serialized signed transaction.
	SignedTx []byte
	// Signature is the wallet's 64-byte signature (the transaction id).
	Signature []byte
	// ProviderRef is the provider's reference for the operation (evidence).
	ProviderRef string
	// RetryClass classifies a retry of the same request with the same key.
	RetryClass provider.RetryClass
	SignedAt   time.Time
}

// SigningProvider signs transactions. Implementations must never alter the
// message they are asked to sign; callers verify that the signed bytes carry
// the same message.
type SigningProvider interface {
	Name() string
	SignTransaction(ctx context.Context, req SignRequest) (SignResult, error)
}
