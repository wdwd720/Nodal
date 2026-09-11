package credit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/webhook"
)

// PurchaseProvider is what a fiat payment provider must offer in order to sell
// Nodal Credits.
//
// The interface is shaped around what any card acquirer must be able to do,
// not around one vendor's request body. Stripe is the V1 configuration of this
// interface; it is not the architecture. The goal document is explicit that
// the abstraction must survive a provider change, and the way that is made
// true rather than asserted is that nothing above this line mentions a
// PaymentIntent.
//
// What every credit purchase provider must be able to do:
//
//   - take a purchase under a key WE chose, so a retried checkout is the same
//     purchase and not a second charge;
//   - tell us later what happened to that purchase, so a lost response is
//     recoverable;
//   - hand us signed evidence of a state change, so we never learn about
//     money from an unauthenticated caller;
//   - describe what it actually supports, so nothing is inferred.
type PurchaseProvider interface {
	// Name identifies the provider in configuration, gates and evidence.
	Name() string

	// Capabilities describes what this provider supports, read from its own
	// contract and documentation rather than assumed.
	Capabilities() PurchaseCapabilities

	// CreatePurchase opens a payment for an amount the SERVER decided.
	//
	// The request carries a fiat amount and a Credit quantity that the caller
	// derived from a PricingPolicy. The provider is told both only so that
	// the pair is recorded on the provider's own object as evidence; the
	// provider never computes either.
	CreatePurchase(ctx context.Context, req CreatePurchaseRequest) (PurchaseSession, error)

	// GetPurchase answers "what happened to this purchase". It is what
	// resolves a lost create response and what reconciliation reads.
	GetPurchase(ctx context.Context, providerReference string) (PurchaseSnapshot, error)

	// ParseWebhook verifies and decodes one provider delivery. An
	// implementation that returns an event without having verified a
	// signature is a defect, not a configuration choice: the pipeline above
	// never sees an unverified event and has no way to check.
	ParseWebhook(ctx context.Context, raw []byte, headers http.Header) (PurchaseEvent, error)
}

// PurchaseCapabilities is what a provider's contract and documentation
// actually say it does.
//
// Every field defaults to false. An adapter that has not been verified
// reports nothing, and a nothing-capable provider can sell nothing, which is
// the correct behaviour for an unverified integration.
type PurchaseCapabilities struct {
	// SupportsHostedPaymentUI is true when card details are collected by the
	// provider's own UI and never touch Nodal. Anything else expands PCI
	// scope and the goal document rules it out for V1.
	SupportsHostedPaymentUI bool
	// SupportsSCA is true when the provider performs 3-D Secure / strong
	// customer authentication itself.
	SupportsSCA bool
	// SupportsIdempotentCreate is true when repeating a create with the same
	// key yields one payment. An adapter that cannot promise this must say so
	// rather than hope.
	SupportsIdempotentCreate bool
	// SupportsLookup is true when the provider can be asked what happened to
	// a purchase after the fact. Without it a lost response is permanently
	// ambiguous, so Registry refuses such a provider outright.
	SupportsLookup bool
	SupportsRefund bool
	// SupportsDisputeEvents is true when the provider notifies us of disputes
	// and chargebacks. Without it, funding finality can never be trusted and
	// no purchased value could ever become payout-eligible.
	SupportsDisputeEvents bool
	// SupportsFraudScreening is true when the provider runs its own fraud
	// engine (Stripe Radar and equivalents).
	SupportsFraudScreening bool

	// Currencies are the ISO 4217 codes the provider will charge in.
	Currencies []string

	// SharedProviderAccount is true when the provider account Nodal uses also
	// serves another product.
	//
	// It is a capability rather than a deployment note because it changes what
	// the code must do: on a shared account the webhook endpoint receives
	// events belonging to the other product, and an adapter that assumes every
	// delivery is its own will act on somebody else's payment. When this is
	// true, foreign-event rejection is mandatory and CheckSharedAccountSafety
	// enforces it.
	SharedProviderAccount bool

	// ContractReference names the commercial agreement or account these
	// capabilities come from. An adapter with none is a sandbox by
	// definition.
	ContractReference string
}

// SupportsCurrency reports whether the provider charges in a currency.
func (c PurchaseCapabilities) SupportsCurrency(code string) bool {
	if code == "" {
		return false
	}
	for _, x := range c.Currencies {
		if strings.EqualFold(x, code) {
			return true
		}
	}
	return false
}

// PurchaseStatus is the provider-neutral status of a payment.
//
// The set is the goal document's payment lifecycle minus two entries, and the
// omission is deliberate. REVERSIBLE and SETTLED are Nodal funding states, not
// provider statuses: no acquirer reports "this is still reversible", because
// reversibility is a property of the card scheme's dispute window and of our
// own hold policy, not of anything the provider observes. Accepting a
// provider claim of SETTLED would let a provider bug make value withdrawable.
// So those two states are reached by Nodal, from a clock and a policy, and a
// provider cannot assert them.
type PurchaseStatus string

// Purchase statuses.
const (
	// PurchaseCreated: the payment exists and nothing has happened to it.
	PurchaseCreated PurchaseStatus = "CREATED"
	// PurchasePaymentMethodRequired: waiting for the customer to supply a
	// payment method.
	PurchasePaymentMethodRequired PurchaseStatus = "PAYMENT_METHOD_REQUIRED"
	// PurchaseAuthenticationRequired: waiting on 3-D Secure or equivalent.
	PurchaseAuthenticationRequired PurchaseStatus = "AUTHENTICATION_REQUIRED"
	// PurchaseProcessing: the provider has the payment and is working on it.
	PurchaseProcessing PurchaseStatus = "PROCESSING"
	// PurchaseAuthorized: funds are held and not yet captured.
	PurchaseAuthorized PurchaseStatus = "AUTHORIZED"
	// PurchaseSucceeded: the provider captured the money.
	PurchaseSucceeded PurchaseStatus = "SUCCEEDED"
	// PurchaseFailed: it definitively did not happen.
	PurchaseFailed PurchaseStatus = "FAILED"
	// PurchaseCanceled: abandoned or cancelled before completion. It is not
	// FAILED: nobody's card was declined and nothing went wrong.
	PurchaseCanceled PurchaseStatus = "CANCELED"
	// PurchaseRefunded: the money was returned by us.
	PurchaseRefunded PurchaseStatus = "REFUNDED"
	// PurchaseDisputed: the cardholder has opened a dispute.
	PurchaseDisputed PurchaseStatus = "DISPUTED"
	// PurchaseChargeback: the dispute resolved against us and the funds are
	// gone.
	PurchaseChargeback PurchaseStatus = "CHARGEBACK"
	// PurchaseDisputeWon: the dispute resolved in our favour and the funds
	// stayed.
	//
	// It is a separate status rather than a return to SUCCEEDED because the
	// two mean different things to the funding. SUCCEEDED maps to CAPTURED,
	// which is before the Credits are minted; a funding that has been through
	// a dispute minted its Credits long ago, and sending it back to CAPTURED
	// would be a backwards transition the state machine correctly refuses.
	//
	// It maps to REVERSIBLE, not to SETTLED, and that is D-094. Winning a
	// dispute is strong evidence that the money is ours; it is not the fact
	// SETTLED records, which is that the reversibility window has CLOSED. A
	// card can be disputed more than once, and the same scheme rules that let
	// the first dispute arrive still apply the day after the second is won. The
	// package already refuses to let an operator assert SETTLED for exactly
	// this reason -- "an operator who could assert it by hand could make value
	// payout-eligible by closing a ticket" -- and a card network closing a
	// dispute in our favour is not more entitled to that than an operator is.
	// SettleDue, reading reversible_at against the configured window, stays the
	// only thing that settles, and 00743's trigger stamps reversible_at once,
	// so the funding returns to the window it was already in rather than
	// restarting it.
	PurchaseDisputeWon PurchaseStatus = "DISPUTE_WON"

	// PurchaseDisputeLifted: a card-network INQUIRY closed without ever
	// becoming a dispute. The freeze lifts and nothing else changes.
	//
	// Stripe calls it `charge.dispute.closed` with status `warning_closed`, and
	// it was mapped to DISPUTE_WON -- which, when DISPUTE_WON meant SETTLED,
	// promoted a payment minutes old straight to payout eligibility without its
	// reversibility window having closed (F-155). An early-fraud warning is not
	// a dispute, no money moved, and a chargeback may still follow it: the
	// money is exactly as reversible as it was before the inquiry opened.
	//
	// It is a status of its own rather than a second spelling of DISPUTE_WON
	// because the two are different provider facts. They reach the same funding
	// state today, and a deployment reading its transition rows can still tell
	// which of them happened.
	PurchaseDisputeLifted PurchaseStatus = "DISPUTE_LIFTED"
	// PurchaseManualReview: the provider said something this binary does not
	// understand. It is a legitimate answer and must not be collapsed into
	// any of the others.
	PurchaseManualReview PurchaseStatus = "MANUAL_REVIEW"
)

var allPurchaseStatuses = []PurchaseStatus{
	PurchaseCreated, PurchasePaymentMethodRequired, PurchaseAuthenticationRequired,
	PurchaseProcessing, PurchaseAuthorized, PurchaseSucceeded, PurchaseFailed,
	PurchaseCanceled, PurchaseRefunded, PurchaseDisputed, PurchaseChargeback,
	PurchaseDisputeWon, PurchaseDisputeLifted, PurchaseManualReview,
}

// AllPurchaseStatuses returns every declared status in declaration order.
func AllPurchaseStatuses() []PurchaseStatus {
	return append([]PurchaseStatus(nil), allPurchaseStatuses...)
}

// Valid reports whether s is declared.
func (s PurchaseStatus) Valid() bool {
	for _, x := range allPurchaseStatuses {
		if x == s {
			return true
		}
	}
	return false
}

func (s PurchaseStatus) String() string { return string(s) }

// FundingStateFor maps a provider status to the funding state it drives the
// funding towards, and false when the status has no mapping.
//
// A false return is not an error and must not crash: the caller routes the
// funding to MANUAL_REVIEW and a human decides. Guessing is how a payment
// nobody understood became Credits somebody spent.
func FundingStateFor(s PurchaseStatus) (FundingState, bool) {
	switch s {
	case PurchaseCreated:
		return FundingCreated, true
	case PurchasePaymentMethodRequired, PurchaseAuthenticationRequired:
		return FundingAuthorizationPending, true
	case PurchaseProcessing:
		return FundingCapturePending, true
	case PurchaseAuthorized:
		return FundingAuthorized, true
	case PurchaseSucceeded:
		return FundingCaptured, true
	case PurchaseFailed:
		return FundingFailed, true
	case PurchaseCanceled:
		return FundingCanceled, true
	case PurchaseRefunded:
		return FundingRefunded, true
	case PurchaseDisputed:
		return FundingDisputed, true
	case PurchaseChargeback:
		// A lost dispute is a reversal: the money is gone. REVERSED is what
		// the funding lifecycle calls that, and Reverse is what has to run.
		return FundingReversed, true
	case PurchaseDisputeWon, PurchaseDisputeLifted:
		// The freeze lifts and the money goes back into the window it was
		// already in. Nothing but SettleDue settles a funding (D-094).
		return FundingReversible, true
	case PurchaseManualReview:
		return FundingManualReview, true
	}
	return "", false
}

// CreatePurchaseRequest is what Nodal hands a provider to open a payment.
//
// There is no Credit quantity field a client could populate. CreditQuantity is
// filled in by the service from a PricingPolicy, and it is present only so the
// provider records it as immutable evidence beside the charge.
type CreatePurchaseRequest struct {
	// IdempotencyKey is chosen by Nodal and persisted before the call.
	IdempotencyKey string

	// FundingID is Nodal's own identifier for this purchase, and the
	// authoritative one. The provider's reference is a foreign key, never an
	// identity.
	FundingID FundingID
	AccountID accounts.AccountID

	// Amount is what the customer pays, in minor units of Currency.
	Amount   money.USD
	Currency string

	// CreditQuantity is what the server decided this amount buys, and
	// PricingVersion and PricingHash name the rules that decided it.
	CreditQuantity money.Quantity
	PricingVersion string
	PricingHash    string

	// Environment is stamped on the provider object so that a test object can
	// never be mistaken for a production one, and vice versa.
	Environment string

	// Description is shown to the customer by the provider's UI.
	Description string

	// StatementDescriptorSuffix is what the cardholder sees on their
	// statement. On a shared provider account this is the difference between
	// a recognised charge and a dispute.
	StatementDescriptorSuffix string
}

// Validate checks the request without touching the network.
func (r CreatePurchaseRequest) Validate() error {
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "credit: a purchase needs an idempotency key")
	}
	if r.FundingID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: a purchase needs a funding id")
	}
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: a purchase needs an account id")
	}
	if !r.Amount.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "credit: a purchase amount must be positive")
	}
	if len(strings.TrimSpace(r.Currency)) != 3 {
		return errs.Newf(errs.CodeValidationFailed, "credit: %q is not an ISO 4217 currency code", r.Currency)
	}
	if r.CreditQuantity.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: a purchase must buy a positive number of Credits")
	}
	if strings.TrimSpace(r.PricingVersion) == "" || strings.TrimSpace(r.PricingHash) == "" {
		return errs.New(errs.CodeValidationFailed,
			"credit: a purchase must record the pricing policy version and hash that derived its Credit quantity")
	}
	if strings.TrimSpace(r.Environment) == "" {
		return errs.New(errs.CodeValidationFailed, "credit: a purchase must record its environment")
	}
	return nil
}

// PurchaseSession is what a provider returns when a payment is opened.
type PurchaseSession struct {
	// ProviderReference is the provider's identifier for the payment.
	ProviderReference string
	Status            PurchaseStatus
	// RawStatus is the provider's own status string, recorded verbatim.
	RawStatus string
	// ClientSecret drives the provider's own payment UI in the browser. It is
	// returned to the customer once and is never persisted or logged.
	ClientSecret string
	// Livemode is the provider's own statement about which world this object
	// belongs to. It is checked against the adapter's mode on every read.
	Livemode  bool
	CreatedAt time.Time
}

// PurchaseSnapshot is the provider's current view of a payment.
type PurchaseSnapshot struct {
	ProviderReference string
	Status            PurchaseStatus
	RawStatus         string
	// Amount and Currency are what the PROVIDER says was charged. They are
	// compared against what Nodal recorded, and a disagreement is a
	// reconciliation mismatch rather than something to overwrite.
	Amount   money.USD
	Currency string
	// Metadata is the provider-held copy of the immutable purchase metadata.
	Metadata map[string]string
	Livemode bool
	// FailureReason is set when Status is FAILED.
	FailureReason string
	// AmountRefundedMinor is how much of Amount has been refunded.
	AmountRefundedMinor int64
}

// PurchaseEvent is a verified, parsed provider webhook.
//
// Recognized is false for signed events the adapter does not model; the
// pipeline records those as IGNORED rather than guessing. Foreign is true when
// the event is genuine, signed, and about something that is not Nodal's --
// which on a shared provider account is a normal, expected occurrence.
type PurchaseEvent struct {
	Identity webhook.Identity
	Snapshot PurchaseSnapshot
	// FundingID is Nodal's id, read back from the provider-held metadata. It
	// is zero on a foreign or unrecognized event.
	FundingID  FundingID
	Recognized bool
	Foreign    bool
	// ForeignReason says why the event was judged not to be Nodal's, so an
	// operator investigating a missing Credit issuance can tell "we ignored
	// it" from "it never arrived".
	ForeignReason string
	Raw           []byte
}

// WebhookIdentity implements webhook.Event.
func (e PurchaseEvent) WebhookIdentity() webhook.Identity { return e.Identity }

// ErrPurchaseProviderUnavailable is returned when a provider cannot be
// reached. It is deliberately distinct from a failed payment: a network error
// says nothing about whether the customer was charged.
var ErrPurchaseProviderUnavailable = errors.New("credit purchase provider unavailable")

// PurchaseRegistry holds the configured credit purchase providers.
type PurchaseRegistry struct {
	mu        sync.RWMutex
	providers map[string]PurchaseProvider
	// allowSandbox is set only by the composition root, and only outside
	// production.
	allowSandbox bool
}

// NewPurchaseRegistry returns an empty registry. allowSandbox must be false in
// production.
func NewPurchaseRegistry(allowSandbox bool) *PurchaseRegistry {
	return &PurchaseRegistry{providers: map[string]PurchaseProvider{}, allowSandbox: allowSandbox}
}

// Register adds a provider, refusing the ones that cannot safely sell Credits.
//
// Each refusal below corresponds to a way money goes missing, and each is
// enforced here -- at the one place every provider must pass through -- rather
// than left to a reviewer to notice.
func (r *PurchaseRegistry) Register(p PurchaseProvider) error {
	if p == nil {
		return errs.New(errs.CodeInternal, "credit: cannot register a nil purchase provider")
	}
	name := strings.TrimSpace(p.Name())
	if name == "" {
		return errs.New(errs.CodeInternal, "credit: a purchase provider must have a name")
	}
	caps := p.Capabilities()
	if strings.TrimSpace(caps.ContractReference) == "" && !r.allowSandbox {
		return errs.Newf(errs.CodeForbidden,
			"credit purchase provider %q has no contract reference and cannot be loaded here; sandbox providers are refused outside LOCAL, TEST and explicit sandbox environments",
			name)
	}
	if !caps.SupportsLookup {
		return errs.Newf(errs.CodeForbidden,
			"credit purchase provider %q cannot say what happened to a purchase after the fact; a lost create response would be permanently ambiguous, so it cannot sell Credits",
			name)
	}
	if !caps.SupportsIdempotentCreate {
		return errs.Newf(errs.CodeForbidden,
			"credit purchase provider %q cannot guarantee that a repeated create is one payment; a retried checkout would charge twice",
			name)
	}
	if !caps.SupportsHostedPaymentUI {
		return errs.Newf(errs.CodeForbidden,
			"credit purchase provider %q would route card details through Nodal; V1 collects no card data and refuses a provider that requires it",
			name)
	}
	if !caps.SupportsDisputeEvents {
		return errs.Newf(errs.CodeForbidden,
			"credit purchase provider %q does not report disputes; purchased Credits could never be known to be final and no payout could ever be justified from them",
			name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[name]; exists {
		return errs.Newf(errs.CodeConflict, "credit purchase provider %q is already registered", name)
	}
	r.providers[name] = p
	return nil
}

// Get returns a registered provider.
func (r *PurchaseRegistry) Get(name string) (PurchaseProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, errs.Newf(errs.CodeNotFound, "no credit purchase provider named %q is configured", name)
	}
	return p, nil
}

// Names returns the registered provider names.
func (r *PurchaseRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}
