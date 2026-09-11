package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// Ports for the withdrawal journey (goal PARTS 19-25).
//
// They follow the same rule as every other port here: one method per domain
// call, no transaction handling above this line, and a nil port answers
// UNSUPPORTED rather than pretending. A deployment with no identity vendor and
// no conversion provider says so; it does not report that a person failed
// verification.

// VerificationPort is the financial verification boundary
// (internal/verification).
type VerificationPort interface {
	// Profile is the §24 view: state, level, evidence, what is missing and
	// what to do next. It carries no personal data.
	Profile(ctx context.Context, accountID accounts.AccountID) (verification.Snapshot, error)
	// Start opens a provider-hosted session and returns a single-use URL.
	Start(ctx context.Context, r StartVerification) (verification.Started, error)
	// Poll asks the provider what happened and records it. Never trust the
	// redirect: a customer arriving back says they came back, not that they
	// passed.
	Poll(ctx context.Context, accountID accounts.AccountID, sessionID verification.SessionID) (verification.Session, error)
	// SandboxOutcome chooses what a rehearsal session decides. It is refused
	// outside a sandbox tier, by the service, before anything is written.
	SandboxOutcome(ctx context.Context, accountID accounts.AccountID, outcome verification.SandboxOutcome) (verification.Session, error)
	// SandboxTier reports whether this deployment may produce a sandbox
	// outcome at all, so the handler can refuse before it reaches the service.
	SandboxTier() bool
}

// StartVerification is the command behind POST /me/verification/sessions.
type StartVerification struct {
	AccountID     accounts.AccountID
	Purpose       verification.Purpose
	Jurisdiction  rules.Jurisdiction
	CorrelationID string
}

// EligibilityPort explains withdrawal eligibility (internal/eligibility).
type EligibilityPort interface {
	// Withdrawal composes the payout policy, the capability gates, the
	// verification level and the provider into one answer, broken down by the
	// origin each unit of value came from. It moves and reserves nothing.
	Withdrawal(ctx context.Context, accountID accounts.AccountID) (eligibility.WithdrawalExplanation, error)
}

// ConversionPort is the conversion-request surface of PROVIDER_BOUNDARY §2:
// where value would go, what leaving would cost, and what value would leave.
//
// It is deliberately separate from PayoutsPort. That port creates and reads the
// conversion REQUEST; this one handles the things a person sets up before they
// make one, and a deployment can have one without the other.
type ConversionPort interface {
	Destinations(ctx context.Context, accountID accounts.AccountID, limit int) ([]payout.Destination, error)
	// AddDestination registers a provider token. It never accepts an account
	// number: internal/payout refuses an input that looks like one.
	AddDestination(ctx context.Context, r AddPayoutDestination) (payout.Destination, error)
	// DisableDestination stops using one. It is a disable rather than a
	// delete: a destination value has left through is financial history.
	DisableDestination(ctx context.Context, accountID accounts.AccountID, id payout.DestinationID) (payout.Destination, error)
	// Quote is the pre-commitment call: gross, fee, net and an expiry, plus
	// the provenance the payout would draw on.
	Quote(ctx context.Context, r CreatePayoutQuote) (payout.Quote, []payout.ProvenanceSlice, error)
}

// AddPayoutDestination is the command behind POST /me/payout-destinations.
//
// There is no account-number field, here or anywhere above it. The only way to
// name a destination is with the provider's token for it, which is the same
// shape of protection that keeps a Credit quantity off the purchase command.
type AddPayoutDestination struct {
	AccountID      accounts.AccountID
	Kind           payout.DestinationKind
	ProviderToken  string
	DisplayLabel   string
	MaskedDisplay  string
	Currency       string
	Country        string
	IdempotencyKey string
	CorrelationID  string
}

// CreatePayoutQuote is the command behind POST /payouts/quote.
type CreatePayoutQuote struct {
	AccountID      accounts.AccountID
	DestinationID  payout.DestinationID
	Amount         money.Quantity
	IdempotencyKey string
	CorrelationID  string
}
