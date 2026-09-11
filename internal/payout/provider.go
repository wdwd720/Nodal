package payout

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Provider is what an approved external payout provider must offer (PART XVIII).
//
// The interface is deliberately small and deliberately not modelled on any
// particular vendor's API. PART XVIII forbids implementing Tilia, Thunes, Zero
// Hash, Coinbase or anyone else against endpoints nobody has verified, and an
// interface shaped around a guess at one vendor's request body is exactly that
// mistake with extra steps.
//
// What every payout provider must be able to do, whatever they call it:
//
//   - accept a request under a key WE chose, so a retry is the same request;
//   - tell us what happened to that key later, so a timeout is recoverable;
//   - describe what they actually support, so nothing is inferred from
//     marketing copy.
type Provider interface {
	// Name identifies the provider in configuration, gates and evidence.
	Name() string

	// Capabilities describes what this provider supports. It is read from the
	// provider's own contract, never assumed (PART LXXVI).
	Capabilities() Capabilities

	// Submit sends a payout. The idempotency key is chosen by Nodal and
	// persisted before this is called, so calling Submit twice with the same
	// key must produce one payout and return the same result.
	//
	// A provider that cannot honour that is not usable here, and its adapter
	// must return ErrIdempotencyUnsupported from Capabilities rather than
	// pretending.
	Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error)

	// Lookup answers "what happened to this idempotency key". It is what
	// resolves PAYOUT_STATUS_UNKNOWN, and a provider without it cannot be
	// used for payouts at all, because a timed-out submission would be
	// permanently ambiguous.
	Lookup(ctx context.Context, idempotencyKey string) (SubmitResult, error)
}

// Capabilities is what a provider's contract actually says it does.
//
// Every field defaults to false. A provider adapter that has not been verified
// against a real contract reports nothing, and a nothing-capable provider can
// perform no payout — which is the correct behaviour for an unverified
// integration.
type Capabilities struct {
	SupportsBankPayout   bool
	SupportsCardPush     bool
	SupportsFiatWallet   bool
	SupportsCryptoPayout bool
	SupportsKYCAtExit    bool
	SupportsWebhooks     bool
	SupportsLookup       bool
	Currencies           []string

	// --- crypto payout specifics -----------------------------------------
	//
	// These exist because "supports crypto payout" turned out to be four
	// separate questions, and answering only the first one is how a system
	// promises a user USDC on Solana when the provider sends USDC on Base.
	// Every one of them is read from the provider's own documentation.

	// SupportedAssets are the payout assets, e.g. "USDC". Empty means none,
	// which is what an unverified adapter reports.
	SupportedAssets []string
	// SupportedNetworks are the chains the provider will actually send on,
	// e.g. "base", "polygon". A network absent from this list cannot be paid
	// to however well the wallet supports it.
	SupportedNetworks []string
	// SupportsExternalWallet is whether the destination may be a wallet the
	// user controls, as opposed to one the provider or the platform holds.
	SupportsExternalWallet bool
	// DestinationHeldByProvider is true when the provider, not Nodal, holds
	// the destination address.
	//
	// It is a capability rather than a detail because it decides who is
	// authoritative for where money goes. When it is true, a Nodal-side wallet
	// record is a mirror and must never be treated as the destination; when it
	// is false, Nodal owns the address and owes the user every protection in
	// the account-takeover section.
	DestinationHeldByProvider bool

	// --- what the provider requires before it will pay ---------------------

	// RequiresConnect is whether the recipient must exist as an account under
	// a platform relationship, rather than as a bare payee.
	RequiresConnect bool
	// RequiresRecipientAccount is whether a per-user provider object must be
	// created and onboarded before any payout.
	RequiresRecipientAccount bool
	// RequiresKYC is whether the recipient must complete identity
	// verification. Who performs it is KYCPerformedByProvider.
	RequiresKYC bool
	// KYCPerformedByProvider is true when the PROVIDER collects and verifies
	// identity documents, so Nodal stores a state and never a document.
	KYCPerformedByProvider bool
	// RequiresTaxInfo is whether the provider collects tax information from
	// recipients.
	RequiresTaxInfo bool
	// RecipientKinds are the legal kinds of recipient the provider will pay,
	// e.g. "individual", "sole_proprietor". A kind absent from this list is
	// refused before a payout is ever attempted.
	RecipientKinds []string

	// SupportedCountries are the ISO 3166-1 alpha-2 codes the provider will
	// pay recipients in.
	//
	// An empty list is NOT "everywhere". It is the absence of an answer, and
	// CanPayRecipient reads it as "no recipient has been confirmed payable" --
	// which is the only safe reading for an adapter nobody has verified.
	SupportedCountries []string

	// ExcludedRegions maps a supported country to the subdivisions inside it
	// the provider will not pay, e.g. {"US": {"NY", "HI"}}.
	//
	// It exists because a country-level answer is not always the whole answer,
	// and discovering that at the end of an onboarding flow -- after the user
	// has handed over identity documents -- is the expensive way to find out.
	ExcludedRegions map[string][]string

	// --- bounds ------------------------------------------------------------

	// MinimumAmount and MaximumAmount bound one payout. A zero MaximumAmount
	// means the provider does not publish one; it is NOT unlimited, and code
	// must treat it as unknown rather than as permission.
	MinimumAmount money.USD
	MaximumAmount money.USD

	// --- the fee model, for the pre-commitment quote ----------------------
	//
	// PROVIDER_BOUNDARY §3 makes `QuotePayout` a separate call precisely so
	// the customer sees the fee and the net before committing. These three
	// fields are what a quote is computed from.

	// FeeModelPublished is whether this adapter has read a fee schedule out of
	// a real contract or a published price list.
	//
	// It exists because zero is ambiguous and the ambiguity is dangerous: a
	// fee of zero that means "we do not know" promises a customer a net amount
	// nobody agreed to. False means there is no quote, rather than a quote of
	// zero, and `QuoteFee` says so.
	FeeModelPublished bool
	// FeeFlat is the fixed part of one payout's fee.
	FeeFlat money.USD
	// FeeBasisPoints is the proportional part, in hundredths of a percent.
	FeeBasisPoints money.BPS
	// FeeModelVersion identifies the schedule these numbers came from. It is
	// recorded on every quote, so a quote given in March is still explicable
	// in June after the provider has repriced.
	FeeModelVersion string

	// Availability is how far this provider is actually usable, as opposed to
	// how far its documentation reads.
	Availability Availability

	// ContractReference names the commercial contract these capabilities come
	// from. An adapter with no contract reference is a sandbox by definition,
	// and Verify refuses to use it in production.
	ContractReference string
}

// Availability is the difference between a documented product and a usable
// one.
//
// It exists because the most dangerous state for an integration is "the code
// is finished". A finished adapter for a product the account has not been
// granted looks exactly like a working one until the first real payout, and
// the goal document's Section 70 is entirely about not letting those two be
// confused.
type Availability string

// Availabilities, in increasing order of usefulness.
const (
	// AvailabilityUnknown is the zero value and means nobody has said. It is
	// never usable.
	AvailabilityUnknown Availability = ""
	// AvailabilityNotOffered means the provider does not offer this at all.
	AvailabilityNotOffered Availability = "NOT_OFFERED"
	// AvailabilityRequiresApplication means the product exists and this
	// account must apply for it. The application has not been made.
	AvailabilityRequiresApplication Availability = "REQUIRES_APPLICATION"
	// AvailabilityApplicationPending means the application is with the
	// provider and no decision has come back.
	AvailabilityApplicationPending Availability = "APPLICATION_PENDING"
	// AvailabilityApplicationDenied means the provider said no. It is
	// recorded rather than retried, and the architecture may need another
	// provider.
	AvailabilityApplicationDenied Availability = "APPLICATION_DENIED"
	// AvailabilitySandbox means the product works in the provider's test
	// environment only.
	AvailabilitySandbox Availability = "SANDBOX_ONLY"
	// AvailabilityLive means the account is approved and the product moves
	// real value.
	AvailabilityLive Availability = "LIVE"
)

// Usable reports whether a payout may actually be attempted. Everything short
// of LIVE and SANDBOX_ONLY is a refusal, and the zero value is a refusal,
// which is what makes an unset field fail closed.
func (a Availability) Usable() bool {
	return a == AvailabilityLive || a == AvailabilitySandbox
}

// SupportsAsset reports whether the provider pays out in an asset.
func (c Capabilities) SupportsAsset(symbol string) bool {
	return containsFold(c.SupportedAssets, symbol)
}

// SupportsNetwork reports whether the provider sends on a network.
func (c Capabilities) SupportsNetwork(network string) bool {
	return containsFold(c.SupportedNetworks, network)
}

// SupportsRecipientKind reports whether the provider pays this kind of
// recipient.
func (c Capabilities) SupportsRecipientKind(kind string) bool {
	return containsFold(c.RecipientKinds, kind)
}

func containsFold(haystack []string, needle string) bool {
	if strings.TrimSpace(needle) == "" {
		return false
	}
	for _, x := range haystack {
		if strings.EqualFold(x, needle) {
			return true
		}
	}
	return false
}

// Supports reports whether the provider handles a destination kind.
func (c Capabilities) Supports(k DestinationKind) bool {
	switch k {
	case DestinationBank:
		return c.SupportsBankPayout
	case DestinationCardPush:
		return c.SupportsCardPush
	case DestinationFiatWallet:
		return c.SupportsFiatWallet
	case DestinationCryptoWallet:
		return c.SupportsCryptoPayout
	}
	return false
}

// SupportsCurrency reports whether the provider pays in a currency.
func (c Capabilities) SupportsCurrency(code string) bool {
	if code == "" {
		return false
	}
	for _, c := range c.Currencies {
		if strings.EqualFold(c, code) {
			return true
		}
	}
	return false
}

// SubmitRequest is what Nodal hands a provider.
type SubmitRequest struct {
	// IdempotencyKey is chosen by Nodal and persisted before the call.
	IdempotencyKey string
	// DestinationReference is the provider's own handle for the destination.
	DestinationReference string
	DestinationKind      DestinationKind
	// Amount is the external value to send, in minor units of Currency. The
	// conversion from Credits happens above this interface and is recorded on
	// the request, because an exchange rate is a fact to store, not to derive.
	Amount   money.USD
	Currency string
	// Reference is Nodal's payout request id, for the provider's records.
	Reference string
}

// Validate refuses a request no provider could act on.
//
// It exists because Submit used to build `SubmitRequest{IdempotencyKey: ...,
// Reference: ..., Amount: money.USD{}}` and leave the destination, the kind and
// the currency at their zero values -- so the provider was instructed to pay no
// amount, to nobody, in no currency, and the request still reached SETTLED with
// the whole reserved quantity recorded as having left (F-225/F-wv-2).
//
// The check is HERE, on the type every adapter is handed, rather than only in
// the one function that builds it: an adapter must not be able to receive one
// of these, whichever caller assembled it.
func (r SubmitRequest) Validate() error {
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeInternal,
			"payout: a submission needs the idempotency key Nodal chose")
	}
	if r.Amount.Minor() <= 0 {
		return errs.New(errs.CodeInternal,
			"payout: a submission needs a positive amount; a provider told to pay nothing has been told nothing").
			WithField("amount_minor", r.Amount.Minor())
	}
	if strings.TrimSpace(r.Currency) == "" {
		return errs.New(errs.CodeInternal,
			"payout: a submission needs a currency; an amount without one is not money")
	}
	if strings.TrimSpace(r.DestinationReference) == "" {
		return errs.New(errs.CodeInternal,
			"payout: a submission needs the provider's reference for the destination, or it names nobody to pay")
	}
	if !r.DestinationKind.Valid() {
		return errs.Newf(errs.CodeInternal,
			"payout: a submission needs a declared destination kind; %q is not one", r.DestinationKind)
	}
	return nil
}

// ProviderStatus is the provider's own view of a payout.
type ProviderStatus string

// Provider statuses. They are deliberately coarse: a provider's own vocabulary
// is recorded verbatim in payout_provider_events, and this is only what Nodal
// needs in order to decide a state transition.
const (
	// ProviderAccepted means the provider has taken the request and it is in
	// progress.
	ProviderAccepted ProviderStatus = "ACCEPTED"
	// ProviderSettled means the money has irrevocably left.
	ProviderSettled ProviderStatus = "SETTLED"
	// ProviderFailed means it definitively did not happen.
	ProviderFailed ProviderStatus = "FAILED"
	// ProviderUnknown means the provider cannot say. It is a legitimate
	// answer and must not be collapsed into either of the others.
	ProviderUnknown ProviderStatus = "UNKNOWN"
)

// Valid reports whether s is declared.
func (s ProviderStatus) Valid() bool {
	switch s {
	case ProviderAccepted, ProviderSettled, ProviderFailed, ProviderUnknown:
		return true
	}
	return false
}

// SubmitResult is what a provider says happened.
type SubmitResult struct {
	Status ProviderStatus
	// ProviderReference is the provider's identifier for the payout.
	ProviderReference string
	// RawStatus is the provider's own status string, recorded verbatim.
	RawStatus string
	// FailureReason is set when Status is FAILED.
	FailureReason string
	// SettledAt is when the provider says the money left.
	SettledAt *time.Time
}

// ErrProviderUnavailable is returned when a provider cannot be reached. It is
// deliberately distinct from a failed payout: a network error says nothing
// about whether the money moved.
var ErrProviderUnavailable = errors.New("payout provider unavailable")

// ErrIdempotencyUnsupported is returned by an adapter whose provider cannot
// guarantee that a repeated key produces one payout.
var ErrIdempotencyUnsupported = errors.New("payout provider does not support idempotent submission")

// Registry holds the configured providers.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	// allowSandbox is set only by the composition root, and only outside
	// production. It is what makes SEC-001 -- "production cannot load a mock
	// provider" -- a property of this type rather than a convention.
	allowSandbox bool
}

// NewRegistry returns an empty registry. allowSandbox must be false in
// production; Register refuses a sandbox provider when it is.
func NewRegistry(allowSandbox bool) *Registry {
	return &Registry{providers: map[string]Provider{}, allowSandbox: allowSandbox}
}

// Register adds a provider.
//
// A provider with no ContractReference is a sandbox by definition — there is
// no commercial agreement behind it — and registering one is refused unless
// the registry was built with sandboxes allowed. That is the programmatic
// assertion PART LXV asks for, placed where every provider must pass through.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return errs.New(errs.CodeInternal, "payout: cannot register a nil provider")
	}
	name := strings.TrimSpace(p.Name())
	if name == "" {
		return errs.New(errs.CodeInternal, "payout: a provider must have a name")
	}
	caps := p.Capabilities()
	if strings.TrimSpace(caps.ContractReference) == "" && !r.allowSandbox {
		return errs.Newf(errs.CodeForbidden,
			"payout provider %q has no contract reference and cannot be loaded here; sandbox providers are refused outside LOCAL, TEST and explicit sandbox environments",
			name)
	}
	if !caps.SupportsLookup {
		return errs.Newf(errs.CodeForbidden,
			"payout provider %q cannot answer what happened to an idempotency key; a timed-out submission would be permanently ambiguous, so it cannot be used for payouts",
			name)
	}
	// A crypto adapter that names no asset or no network has not been verified
	// against anything. Registering it would let a payout be attempted against
	// a destination nobody has confirmed the provider can reach, which is
	// precisely the "do not invent supported assets and chains" failure the
	// goal document opens with.
	if caps.SupportsCryptoPayout {
		if len(caps.SupportedAssets) == 0 {
			return errs.Newf(errs.CodeForbidden,
				"payout provider %q claims crypto payouts and names no supported asset; an adapter that has not been read against a real product reports nothing",
				name)
		}
		if len(caps.SupportedNetworks) == 0 {
			return errs.Newf(errs.CodeForbidden,
				"payout provider %q claims crypto payouts and names no supported network; the asset alone does not say where it can be sent",
				name)
		}
	}
	if caps.RequiresKYC && !caps.KYCPerformedByProvider && !r.allowSandbox {
		return errs.Newf(errs.CodeForbidden,
			"payout provider %q requires identity verification but does not perform it; Nodal would have to collect government identity documents, which this deployment is not built to hold",
			name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[name]; exists {
		return errs.Newf(errs.CodeConflict, "payout provider %q is already registered", name)
	}
	r.providers[name] = p
	return nil
}

// Get returns a registered provider.
func (r *Registry) Get(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, errs.Newf(errs.CodeNotFound, "no payout provider named %q is configured", name)
	}
	return p, nil
}

// Names returns the registered provider names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}
