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

	// ContractReference names the commercial contract these capabilities come
	// from. An adapter with no contract reference is a sandbox by definition,
	// and Verify refuses to use it in production.
	ContractReference string
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
