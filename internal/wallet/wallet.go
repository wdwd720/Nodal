package wallet

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/security"
)

type walletKind struct{}

// WalletID identifies a wallets row. Provider wallet ids and addresses are
// external references, never identity.
type WalletID = id.ID[walletKind]

// NewWalletID returns a fresh UUIDv7 wallet id.
func NewWalletID() WalletID { return id.New[walletKind]() }

// ParseWalletID parses the canonical form.
func ParseWalletID(s string) (WalletID, error) { return id.Parse[walletKind](s) }

// Status is the wallet lifecycle state.
type Status string

// Wallet statuses (wallets.status CHECK constraint).
const (
	StatusActive    Status = "ACTIVE"
	StatusSuspended Status = "SUSPENDED"
	StatusRevoked   Status = "REVOKED"
)

// Valid reports whether s is a declared status.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusSuspended, StatusRevoked:
		return true
	}
	return false
}

// CanTransition reports whether from → to is a legal status transition.
// REVOKED is terminal.
func CanTransition(from, to Status) bool {
	switch from {
	case StatusActive:
		return to == StatusSuspended || to == StatusRevoked
	case StatusSuspended:
		return to == StatusActive || to == StatusRevoked
	}
	return false
}

// Kind is the custody model of a wallet.
type Kind string

// KindEmbeddedDelegated is the only V1 kind: a provider-managed embedded
// wallet whose signing is delegated to the platform under a policy.
const KindEmbeddedDelegated Kind = "EMBEDDED_DELEGATED"

// Wallet mirrors a wallets row.
type Wallet struct {
	ID                   WalletID
	AccountID            accounts.AccountID
	Provider             string
	ProviderWalletID     string
	Chain                string
	Address              string
	Kind                 Kind
	Status               Status
	DelegationRef        string
	DelegationVerifiedAt *time.Time
	SigningPolicyVersion string
	Capabilities         json.RawMessage
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// DelegationVerified reports whether delegated signing was verified for
// this wallet (wallets.delegation_verified_at is set).
func (w Wallet) DelegationVerified() bool { return w.DelegationVerifiedAt != nil }

// Validate checks structural invariants before persistence.
func (w Wallet) Validate() error {
	switch {
	case w.AccountID.IsZero():
		return errs.New(errs.CodeValidationFailed, "wallet: account id required")
	case w.Provider == "":
		return errs.New(errs.CodeValidationFailed, "wallet: provider required")
	case w.ProviderWalletID == "":
		return errs.New(errs.CodeValidationFailed, "wallet: provider wallet id required")
	case w.Chain == "":
		return errs.New(errs.CodeValidationFailed, "wallet: chain required")
	case w.Address == "":
		return errs.New(errs.CodeValidationFailed, "wallet: address required")
	case w.Kind != KindEmbeddedDelegated:
		return errs.Newf(errs.CodeValidationFailed, "wallet: unsupported kind %q", w.Kind)
	case !w.Status.Valid():
		return errs.Newf(errs.CodeValidationFailed, "wallet: unknown status %q", w.Status)
	}
	if len(w.Capabilities) > 0 && !json.Valid(w.Capabilities) {
		return errs.New(errs.CodeValidationFailed, "wallet: capabilities must be JSON")
	}
	return nil
}

// DelegationState is the outcome of a delegation verification.
type DelegationState string

// Delegation states.
const (
	DelegationVerified   DelegationState = "VERIFIED"
	DelegationUnverified DelegationState = "UNVERIFIED"
	DelegationRevoked    DelegationState = "REVOKED"
	DelegationUnknown    DelegationState = "UNKNOWN"
)

// DelegationStatus is what a WalletProvider reports about a wallet's
// delegated-signing setup: the policy attached, the signer registered, the
// programs the provider-side policy allows, and the evidence reference.
type DelegationStatus struct {
	ProviderWalletID string
	State            DelegationState
	// PolicyID / PolicyVersion identify the provider-side policy in force.
	PolicyID      string
	PolicyVersion string
	// SignerID is the platform signer (key quorum) the provider recognizes.
	SignerID string
	// AllowedPrograms are the program ids the provider policy allow-lists.
	AllowedPrograms []string
	// EvidenceRef points at the recorded provider response.
	EvidenceRef string
	CheckedAt   time.Time
	Detail      string
}

// Verified reports whether the delegation is usable for signing.
func (d DelegationStatus) Verified() bool { return d.State == DelegationVerified }

// CapabilityState is the verification state of one provider capability.
type CapabilityState string

// Capability states.
const (
	CapabilityVerified    CapabilityState = "VERIFIED"
	CapabilityUnverified  CapabilityState = "UNVERIFIED"
	CapabilityUnsupported CapabilityState = "UNSUPPORTED"
)

// Capability is the result of a provider capability probe (PART 96). The
// signing service refuses to start in STAGING/PROD unless DelegatedSigning
// is VERIFIED.
type Capability struct {
	Provider          string
	DelegatedSigning  CapabilityState
	VerificationLabel provider.VerificationLabel
	SupportedChains   []string
	ProbedAt          time.Time
	Detail            string
}

// DelegationVerified reports whether delegated signing is VERIFIED.
func (c Capability) DelegationVerified() bool { return c.DelegatedSigning == CapabilityVerified }

// StatusChange is an audited status transition request.
type StatusChange struct {
	To            Status
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	CorrelationID string
}

// Validate rejects agent actors, empty reasons and unknown statuses.
func (c StatusChange) Validate() error {
	if c.ActorType == security.ActorAgent || !c.ActorType.Valid() {
		return errs.New(errs.CodeForbidden, "wallet status can only be changed by a non-agent actor")
	}
	if c.ActorID == "" {
		return errs.New(errs.CodeValidationFailed, "wallet: actor id required")
	}
	if c.Reason == "" {
		return errs.New(errs.CodeValidationFailed, "wallet: reason required")
	}
	if !c.To.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "wallet: unknown status %q", c.To)
	}
	return nil
}

func (c StatusChange) String() string {
	return fmt.Sprintf("to=%s actor=%s/%s", c.To, c.ActorType, c.ActorID)
}
