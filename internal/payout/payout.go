package payout

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

type (
	requestKind       struct{}
	destinationKind   struct{}
	transitionKind    struct{}
	allocationKind    struct{}
	providerEventKind struct{}
)

// RequestID identifies a payout request.
type RequestID = id.ID[requestKind]

// NewRequestID returns a fresh request id.
func NewRequestID() RequestID { return id.New[requestKind]() }

// ParseRequestID parses the canonical form.
func ParseRequestID(s string) (RequestID, error) { return id.Parse[requestKind](s) }

// DestinationID identifies a payout destination.
type DestinationID = id.ID[destinationKind]

// NewDestinationID returns a fresh destination id.
func NewDestinationID() DestinationID { return id.New[destinationKind]() }

// ParseDestinationID parses the canonical form.
func ParseDestinationID(s string) (DestinationID, error) { return id.Parse[destinationKind](s) }

// TransitionID identifies a recorded state change.
type TransitionID = id.ID[transitionKind]

// NewTransitionID returns a fresh transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// AllocationID identifies one reserved lot slice.
type AllocationID = id.ID[allocationKind]

// NewAllocationID returns a fresh allocation id.
func NewAllocationID() AllocationID { return id.New[allocationKind]() }

// ProviderEventID identifies one recorded provider interaction.
type ProviderEventID = id.ID[providerEventKind]

// NewProviderEventID returns a fresh provider event id.
func NewProviderEventID() ProviderEventID { return id.New[providerEventKind]() }

// State is where a payout request has got to (PART XXI).
type State string

// Payout states.
const (
	StateDraft                State = "DRAFT"
	StateEligibilityCheck     State = "ELIGIBILITY_CHECK"
	StateVerificationRequired State = "VERIFICATION_REQUIRED"
	StateVerificationPending  State = "VERIFICATION_PENDING"
	StateVerified             State = "VERIFIED"
	StateSubmitted            State = "SUBMITTED"
	StateProviderPending      State = "PROVIDER_PENDING"
	// StateStatusUnknown is the state PART XXI insists on: the provider's
	// answer is uncertain, the money may or may not have moved, and the
	// reservation STAYS while reconciliation finds out. Releasing it and
	// retrying is how a payout gets sent twice.
	StateStatusUnknown State = "PAYOUT_STATUS_UNKNOWN"
	StateSettled       State = "SETTLED"
	StateFailed        State = "FAILED"
	StateRejected      State = "REJECTED"
	StateReversed      State = "REVERSED"
	StateManualReview  State = "MANUAL_REVIEW"
)

var allStates = []State{
	StateDraft, StateEligibilityCheck, StateVerificationRequired, StateVerificationPending,
	StateVerified, StateSubmitted, StateProviderPending, StateStatusUnknown,
	StateSettled, StateFailed, StateRejected, StateReversed, StateManualReview,
}

// AllStates returns every declared state in declaration order (a copy).
func AllStates() []State { return append([]State(nil), allStates...) }

// Valid reports whether s is declared.
func (s State) Valid() bool {
	for _, x := range allStates {
		if x == s {
			return true
		}
	}
	return false
}

func (s State) String() string { return string(s) }

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	switch s {
	case StateSettled, StateFailed, StateRejected, StateReversed:
		return true
	}
	return false
}

// HoldsValue reports whether a request in this state has value reserved out of
// the user's spendable balance.
//
// It is the question a reconciler asks: everything that holds value and is not
// terminal is something a human or a sweep has to finish.
func (s State) HoldsValue() bool {
	switch s {
	case StateVerificationRequired, StateVerificationPending, StateVerified,
		StateSubmitted, StateProviderPending, StateStatusUnknown, StateManualReview:
		return true
	}
	return false
}

// stateTransitions is the explicit legal transition table.
var stateTransitions = map[State][]State{
	StateDraft:            {StateEligibilityCheck, StateRejected},
	StateEligibilityCheck: {StateVerificationRequired, StateVerified, StateRejected, StateManualReview},
	// VERIFICATION_REQUIRED can reach VERIFIED directly: a user may already
	// have completed identity verification out of band, in which case there is
	// no pending provider flow to sit in.
	StateVerificationRequired: {StateVerificationPending, StateVerified, StateRejected, StateFailed},
	StateVerificationPending:  {StateVerified, StateRejected, StateManualReview, StateFailed},
	StateVerified:             {StateSubmitted, StateRejected, StateManualReview, StateFailed},
	// A submission either lands, fails outright, leaves us not knowing, or
	// turns out to need a human -- which is what happens when the provider
	// settles and the settlement cannot be recorded.
	StateSubmitted:       {StateProviderPending, StateStatusUnknown, StateFailed, StateSettled, StateManualReview},
	StateProviderPending: {StateSettled, StateFailed, StateStatusUnknown, StateManualReview},
	// The unknown state resolves only by finding out, never by assuming.
	StateStatusUnknown: {StateSettled, StateFailed, StateManualReview},
	// A provider can claw back a settled payout.
	StateSettled:      {StateReversed},
	StateManualReview: {StateVerified, StateSubmitted, StateSettled, StateFailed, StateRejected},
	StateFailed:       {},
	StateRejected:     {},
	StateReversed:     {},
}

// StateEdges returns every legal edge as a flat from,to sequence, in
// declaration order.
//
// Migration 00807 populates `payout_request_state_edges` from it and
// cp_payout_apply_state_transition consults it, so an edge this table does not
// have cannot be written by inserting a transition row that claims it. The
// enum parity suite holds the two identical.
func StateEdges() []string {
	out := make([]string, 0, 48)
	for _, from := range allStates {
		for _, to := range stateTransitions[from] {
			out = append(out, string(from), string(to))
		}
	}
	return out
}

// CanTransition reports whether from → to is legal.
//
// Note what is absent: PAYOUT_STATUS_UNKNOWN cannot go back to VERIFIED or
// SUBMITTED. Once a submission may have happened, the only ways out are
// finding out that it did (SETTLED), finding out that it did not (FAILED), or
// asking a human (MANUAL_REVIEW). Re-submitting is how a payout gets sent
// twice, so the state machine does not offer it.
func CanTransition(from, to State) bool {
	for _, t := range stateTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// DestinationKind is where value would go (PART XVIII).
type DestinationKind string

// Destination kinds. Which of these a given provider actually supports is a
// provider capability, never an assumption.
const (
	DestinationBank         DestinationKind = "BANK"
	DestinationCardPush     DestinationKind = "CARD_PUSH"
	DestinationFiatWallet   DestinationKind = "FIAT_WALLET"
	DestinationCryptoWallet DestinationKind = "CRYPTO_WALLET"
)

var allDestinationKinds = []DestinationKind{
	DestinationBank, DestinationCardPush, DestinationFiatWallet, DestinationCryptoWallet,
}

// AllDestinationKinds returns every declared kind in declaration order (a copy).
func AllDestinationKinds() []DestinationKind {
	return append([]DestinationKind(nil), allDestinationKinds...)
}

// Valid reports whether k is declared.
func (k DestinationKind) Valid() bool {
	for _, x := range allDestinationKinds {
		if x == k {
			return true
		}
	}
	return false
}

// DestinationStatus is whether a destination may receive value.
type DestinationStatus string

// Destination statuses.
const (
	DestinationUnverified DestinationStatus = "UNVERIFIED"
	DestinationVerified   DestinationStatus = "VERIFIED"
	DestinationRejected   DestinationStatus = "REJECTED"
	DestinationDisabled   DestinationStatus = "DISABLED"
)

// Valid reports whether s is declared.
func (s DestinationStatus) Valid() bool {
	switch s {
	case DestinationUnverified, DestinationVerified, DestinationRejected, DestinationDisabled:
		return true
	}
	return false
}

// Usable reports whether a payout may be sent to a destination in this state.
func (s DestinationStatus) Usable() bool { return s == DestinationVerified }

// Destination is where a user has asked value to go.
//
// Nodal stores the provider's reference and never the underlying account
// number or key: PART LXXXIV says not to duplicate identity data the provider
// already holds, and a bank account number here is a liability with no
// compensating benefit.
type Destination struct {
	ID                DestinationID
	AccountID         accounts.AccountID
	Kind              DestinationKind
	Provider          string
	ProviderReference string
	DisplayLabel      string
	Currency          string
	Status            DestinationStatus
	// Country is the ISO 3166-1 alpha-2 code of the jurisdiction this pays
	// into. `Capabilities` carries SupportedCountries and ExcludedRegions and
	// nothing could feed them until the destination knew (00763).
	Country string
	// Region is the subdivision within Country, without its country prefix.
	//
	// 00763 fed half the question. `CanPayRecipient` has answered
	// RECIPIENT_REGION_EXCLUDED and RECIPIENT_REGION_UNKNOWN since it was
	// written and nothing ever set a region, so a provider that pays the United
	// States but not New York had no way to refuse a New York recipient
	// (F-228, D-122). It is required whenever the provider publishes excluded
	// regions for the country, because "we do not know which state" is not
	// "any state".
	Region string
	// MaskedDisplay is what a person recognises without Nodal holding the
	// number: "••••4242". It is validated to be a mask rather than a number.
	MaskedDisplay string
	// Sandbox marks a destination that belongs to a rehearsal. It is shown on
	// every response that mentions it.
	Sandbox    bool
	VerifiedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Request is a payout_requests row.
type Request struct {
	ID            RequestID
	AccountID     accounts.AccountID
	DestinationID *DestinationID
	CreditAssetID assets.AssetID

	State State

	RequestedQuantity money.Quantity
	ReservedQuantity  money.Quantity
	SettledQuantity   money.Quantity

	PolicyVersion      string
	PolicyHash         string
	EligibilityReasons []string
	VerificationLevel  valuedomain.VerificationLevel

	Provider               string
	ProviderIdempotencyKey string
	ProviderReference      string
	ProviderStatus         string

	IdempotencyKey string
	QuoteID        *QuoteID

	// The price the customer was shown, recorded on the request rather than
	// left to be re-derived from a fee schedule that may since have been
	// repriced (00808, D-119). Zero when the row predates the quote becoming
	// required.
	QuoteGrossAmountMinor int64
	QuoteFeeAmountMinor   int64
	QuoteNetAmountMinor   int64
	QuoteCurrency         string

	// Sandbox is whether this was a rehearsal, as recorded at creation. A row
	// written before 00810 has no recorded fact and reads as a rehearsal,
	// because an unrecorded mode cannot be asserted to be real.
	Sandbox     bool
	Environment string

	ReservedAt    *time.Time
	SubmittedAt   *time.Time
	SettledAt     *time.Time
	FailureReason string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Allocation is one lot slice a payout reserved.
type Allocation struct {
	ID        AllocationID
	RequestID RequestID
	LotID     credit.LotID
	Origin    valuedomain.CreditOrigin
	Quantity  money.Quantity
	Returned  bool
	CreatedAt time.Time
}

// ProviderTerms are the published numbers a payout has to be judged against:
// what the provider will charge, and the smallest payout it will send.
//
// They are an input rather than a lookup for the reason everything else in this
// package is one -- the caller already has the provider's capabilities, a second
// lookup could disagree with the first, and a decision made in March has to be
// replayable in June against the inputs it was made with.
//
// There is no permissive zero value. `FeeModelPublished` false is not a fee of
// zero; it is the absence of an answer, and `CreateRequest.Validate` refuses it,
// so a caller that forgets to supply the terms stops a payout rather than
// letting one through unpriced.
type ProviderTerms struct {
	// FeeModelPublished is whether the adapter read a fee schedule out of a
	// real contract or a published price list.
	FeeModelPublished bool
	// FeeFlat and FeeBasisPoints are the two halves of one payout's fee.
	FeeFlat        money.USD
	FeeBasisPoints money.BPS
	// FeeModelVersion identifies the schedule those numbers came from.
	FeeModelVersion string
	// MinimumAmount is the smallest payout the provider will send, judged NET
	// of fees because sub-minimum dust is destroyed rather than returned
	// (PROVIDER_BOUNDARY §3). A zero minimum is "the provider publishes none".
	MinimumAmount money.USD
}

// TermsFrom reads the terms out of a provider's published capabilities, so no
// caller assembles them field by field and none can assemble a wrong one.
func TermsFrom(c Capabilities) ProviderTerms {
	return ProviderTerms{
		FeeModelPublished: c.FeeModelPublished,
		FeeFlat:           c.FeeFlat,
		FeeBasisPoints:    c.FeeBasisPoints,
		FeeModelVersion:   c.FeeModelVersion,
		MinimumAmount:     c.MinimumAmount,
	}
}

// CreateRequest asks for a payout.
type CreateRequest struct {
	AccountID     accounts.AccountID
	DestinationID *DestinationID
	Quantity      money.Quantity

	// QuoteID names the pre-commitment quote the customer was shown, and it is
	// REQUIRED.
	//
	// It used to be optional here and optional on POST /v1/payouts, with a
	// comment saying an operator resolving a stuck payout has no quote to name.
	// That was false: the route is accountScopeWrite, and an operator resolves
	// through ResolveManualReview, which creates nothing. What it bought
	// instead was a payout below the provider's published minimum, reserved and
	// settled with the fee never taken, because the whole minimum-and-fee branch
	// of Create sat inside `if r.QuoteID != nil` (F-224, D-119).
	//
	// The quote is consumed inside the same transaction that reserves the value,
	// so one quote funds exactly one payout.
	QuoteID *QuoteID

	// ProviderTerms are what the provider publishes TODAY. The quote records
	// what it published when the customer was shown a number; Create judges the
	// minimum against both, so a minimum raised between the quote and the
	// commit refuses rather than sending something the provider will bounce.
	ProviderTerms ProviderTerms

	// Sandbox and Environment are the deployment facts this request was made
	// under, recorded at creation rather than read from today's provider mode.
	//
	// The by-id read used to answer `"sandbox": true` by asking the port what
	// the provider is NOW, and the list and the create response did not answer
	// it at all -- so one payout was a rehearsal on one screen, unlabelled on
	// two others, and would silently become a real payout in the record the day
	// a deployment swapped its provider (F-232). Environment has no default:
	// Validate refuses a request without one, because a row that cannot say
	// where it was made cannot be checked against the rule that a rehearsal
	// never exists in PROD.
	Sandbox     bool
	Environment string

	// DisclosureAccepted says whether the person has accepted the current
	// WITHDRAWAL_DISCLOSURE. It is an input rather than a lookup, like every
	// other fact this package decides on: the caller already knows it, a second
	// lookup could disagree with the first, and a decision made in March has to
	// be replayable in June against the inputs it was made with.
	//
	// The field has no "unknown" value on purpose. false is refused, so a
	// caller that forgets to supply it refuses a payout rather than permitting
	// one -- which is the direction a mistake here has to fall.
	DisclosureAccepted bool

	IdempotencyKey string
	EffectiveAt    time.Time
	CorrelationID  string
}

// Validate checks the request without touching the database.
func (r CreateRequest) Validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a payout needs an account")
	}
	if r.Quantity.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "a payout amount must be positive")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "a payout needs an idempotency key")
	}
	if r.EffectiveAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a payout needs effective_at")
	}
	if r.QuoteID == nil || r.QuoteID.IsZero() {
		return errs.New(errs.CodeValidationFailed,
			"a payout names the quote the customer was shown").
			WithField("field", "quote_id")
	}
	if strings.TrimSpace(r.Environment) == "" {
		return errs.New(errs.CodeValidationFailed,
			"a payout records the environment it was created in").
			WithField("field", "environment")
	}
	if r.Sandbox && r.Environment == "PROD" {
		return errs.New(errs.CodeForbidden,
			"a rehearsal payout cannot exist in PROD").
			WithField("environment", r.Environment)
	}
	if !r.ProviderTerms.FeeModelPublished {
		// Not a fee of zero. A caller that has not read a fee schedule out of a
		// real contract cannot say what would reach the customer, and promising
		// a net amount nobody agreed is worse than refusing.
		return errs.New(errs.CodeProviderUnavailable,
			"this payout provider has not published a fee model, so no payout can be judged against it").
			WithField("fee_model", "UNPUBLISHED")
	}
	return nil
}

// Decision is the outcome of an eligibility evaluation.
//
// It is a value, not an action: nothing has been reserved when a Decision is
// produced, and the same inputs always produce the same Decision.
type Decision struct {
	// Eligible is how much of the request the policy permits.
	Eligible money.Quantity
	// Requested is what was asked for.
	Requested money.Quantity
	// Lots are the specific provenance lots the eligible amount would come
	// from, in consumption order.
	Lots []credit.Lot
	// Origins is the set of origins those lots carry. Consumption is
	// restricted to exactly these, so lot selection cannot stray outside what
	// this decision approved even if the two run a moment apart.
	Origins []valuedomain.CreditOrigin

	// Reasons explains a shortfall. Empty when the full amount is eligible.
	Reasons []valuedomain.PermitReason
	// RequiredVerification is the verification level that would be needed for
	// this request, so a caller can tell the user what to do next rather than
	// only that they cannot proceed.
	RequiredVerification valuedomain.VerificationLevel

	// VerificationWouldSuffice is true when the ONLY thing standing between
	// this user and their money is identity verification: the value is there,
	// its provenance is approved, the capability is on, and the account is in
	// good standing.
	//
	// This is the "KYC at exit" experience of PART XIX made explicit. Without
	// it the product can only say "no", when the useful answer is "verify and
	// this will work" — and the difference between those two answers is the
	// difference between a dead end and a next step.
	VerificationWouldSuffice bool

	PolicyVersion string
	PolicyHash    string
}

// Sufficient reports whether the full requested amount is eligible.
func (d Decision) Sufficient() bool {
	return d.Eligible.Cmp(d.Requested) >= 0 && d.Requested.IsPositive()
}

// ReasonStrings renders the reasons for storage.
func (d Decision) ReasonStrings() []string {
	out := make([]string, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		out = append(out, string(r))
	}
	return out
}
