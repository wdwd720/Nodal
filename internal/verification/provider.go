package verification

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// Provider is the identity-verification contract, derived from
// PROVIDER_BOUNDARY.md §3 rather than from any one vendor.
//
// Every method exists because at least one viable provider — Persona, Veriff,
// Sumsub, Stripe Identity — demands it, and none of them leaks a single
// provider's vocabulary. No real provider is integrated: that is a human action
// (an account, terms, a data-processing agreement) recorded in BLOCKERS.md, and
// this interface exists so that when one is chosen the choice is reversible.
//
// What the contract deliberately cannot express: a way for Nodal to decide that
// somebody is verified. Every decision here comes from a provider, and the
// sandbox provider says in its name and in every row it writes that it is a
// rehearsal.
type Provider interface {
	// Name identifies the provider in configuration, evidence and audit.
	Name() string

	// Capabilities describes what this provider actually performs, read from
	// its own contract and never inferred from marketing copy.
	Capabilities() Capabilities

	// Start opens a hosted session. The returned URL is single-use, expires,
	// and is handed to the browser that asked for it — never stored.
	Start(ctx context.Context, req SessionRequest) (StartedSession, error)

	// Get is the reconciliation poll. Never trust a redirect: a customer
	// returning to the return URL says they came back, not that they passed.
	Get(ctx context.Context, providerRef string) (Result, error)

	// Resume issues a fresh hosted URL on the SAME session, preserving the
	// failed-attempt history a provider counts against its own limits.
	Resume(ctx context.Context, providerRef string) (StartedSession, error)

	// ParseWebhook verifies the provider's signature over the RAW bytes and
	// returns what it said. Signature verification lives in the adapter
	// because every provider signs differently; a webhook is never the sole
	// source of truth, which is what Get is for.
	ParseWebhook(headers http.Header, body []byte) (Result, error)
}

// Capabilities is what a provider's contract says it does.
//
// Every field defaults to false or empty. An adapter nobody has verified
// against a real contract reports nothing, and a nothing-capable provider can
// establish no level at all — which is the correct behaviour for an unverified
// integration.
type Capabilities struct {
	// PerformsIdentityDocument is document capture and verification.
	PerformsIdentityDocument bool
	// PerformsAgeCheck is whether the provider attests an age. Stripe Identity
	// does; a provider that does not cannot support a jurisdiction whose floor
	// is above what it attests.
	PerformsAgeCheck bool
	// AttestsAgeAtLeast is the age a positive age check attests, typically 18.
	// A jurisdiction requiring more than this fails closed.
	AttestsAgeAtLeast int
	// PerformsSanctionsScreening is AML/sanctions screening. Stripe Identity
	// does NOT do this, which is why it is never the whole answer
	// (PROVIDER_BOUNDARY §5).
	PerformsSanctionsScreening bool
	// PerformsPEPScreening is politically-exposed-person screening.
	PerformsPEPScreening bool
	// HostedFlow is whether the provider renders the document capture. Nodal
	// never does, so a provider without this cannot be used at all.
	HostedFlow bool
	// SupportsWebhooks is whether results arrive asynchronously as well as by
	// poll. A provider without it is usable; a provider without Get is not.
	SupportsWebhooks bool
	// SupportedCountries are the ISO 3166-1 alpha-2 codes the provider will
	// verify in. An empty list is NOT "everywhere": it is the absence of an
	// answer.
	SupportedCountries []string
	// Availability is how far this provider is actually usable, as opposed to
	// how far its documentation reads.
	Availability Availability
	// ContractReference names the commercial contract these capabilities come
	// from. An adapter with no contract reference is a sandbox by definition,
	// and the registry refuses it where sandboxes are not allowed.
	ContractReference string
}

// SupportsCountry reports whether the provider verifies in a country.
func (c Capabilities) SupportsCountry(code string) bool {
	want := strings.ToUpper(strings.TrimSpace(code))
	if want == "" {
		return false
	}
	for _, x := range c.SupportedCountries {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}

// Supports reports whether a provider can establish a purpose at all, and what
// is missing when it cannot.
//
// PAYOUT_KYC needs a hosted document check, an age attestation and sanctions
// screening; ENHANCED needs political-exposure screening on top. A provider
// that does three of the four cannot establish the level, and saying so here is
// what stops a person being sent through an onboarding flow that could never
// have finished.
func (c Capabilities) Supports(purpose Purpose, j rules.Jurisdiction) (bool, []string) {
	var missing []string
	if !c.HostedFlow {
		missing = append(missing, "HOSTED_FLOW")
	}
	if !c.PerformsIdentityDocument {
		missing = append(missing, string(CheckIdentityDocument))
	}
	if !c.PerformsAgeCheck {
		missing = append(missing, string(CheckAge))
	} else if need, _ := rules.MinimumAge(j); c.AttestsAgeAtLeast < need {
		missing = append(missing, string(CheckAge))
	}
	if !c.PerformsSanctionsScreening {
		missing = append(missing, string(CheckSanctions))
	}
	if purpose == PurposeEnhanced && !c.PerformsPEPScreening {
		missing = append(missing, string(CheckPEP))
	}
	if !c.SupportsCountry(j.Country) {
		missing = append(missing, "COUNTRY")
	}
	if !c.Availability.Usable() {
		missing = append(missing, "AVAILABILITY")
	}
	return len(missing) == 0, missing
}

// Availability is the difference between a documented product and a usable one.
// It mirrors payout.Availability deliberately rather than importing it: the two
// packages describe different provider roles (PROVIDER_BOUNDARY §1 roles B and
// C) and must be separately configurable.
type Availability string

// Availabilities, in increasing order of usefulness.
const (
	// AvailabilityUnknown is the zero value and means nobody has said. It is
	// never usable.
	AvailabilityUnknown Availability = ""
	// AvailabilityNotOffered means the provider does not offer this.
	AvailabilityNotOffered Availability = "NOT_OFFERED"
	// AvailabilityRequiresApplication means an account must be opened and
	// terms accepted. That is a human action; see BLOCKERS.md.
	AvailabilityRequiresApplication Availability = "REQUIRES_APPLICATION"
	// AvailabilityApplicationPending means the application is with the
	// provider and no decision has come back.
	AvailabilityApplicationPending Availability = "APPLICATION_PENDING"
	// AvailabilityApplicationDenied means the provider said no.
	AvailabilityApplicationDenied Availability = "APPLICATION_DENIED"
	// AvailabilitySandbox means the provider's test environment only.
	AvailabilitySandbox Availability = "SANDBOX_ONLY"
	// AvailabilityLive means the account is approved and decisions are real.
	AvailabilityLive Availability = "LIVE"
)

// Usable reports whether a verification may actually be attempted. Everything
// short of LIVE and SANDBOX_ONLY is a refusal, and the zero value is a refusal,
// which is what makes an unset field fail closed.
func (a Availability) Usable() bool {
	return a == AvailabilityLive || a == AvailabilitySandbox
}

// SessionRequest is what Nodal hands a provider to open a session.
//
// SubjectRef is Nodal's own opaque identifier for the person. It is never an
// e-mail address, a name or anything else that identifies them outside this
// system: the provider learns who they are from the documents they upload, not
// from us.
type SessionRequest struct {
	SubjectRef   string
	Purpose      Purpose
	Jurisdiction rules.Jurisdiction
	ReturnURL    string
	RefreshURL   string
}

// Validate checks the request without contacting anything.
func (r SessionRequest) Validate() error {
	if strings.TrimSpace(r.SubjectRef) == "" {
		return errs.New(errs.CodeValidationFailed, "a verification session needs a subject reference")
	}
	if !r.Purpose.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown verification purpose %q", r.Purpose)
	}
	return nil
}

// StartedSession is a hosted session a person can be sent to.
type StartedSession struct {
	// ProviderRef is the provider's identifier for the attempt. It is what
	// every later poll and webhook is matched on.
	ProviderRef string
	// HostedURL is single-use and short-lived. It is returned to the browser
	// that asked for it and is never persisted.
	HostedURL string
	// ExpiresAt is when the hosted URL stops working.
	ExpiresAt time.Time
	// Environment is a FIELD rather than a base URL, so a sandbox session
	// cannot be mistaken for a live one by reading a hostname.
	Environment string
	// Sandbox marks a rehearsal. It is written onto every row derived from
	// this session and shown on every response that mentions it.
	Sandbox bool
}

// Result is what a provider said about a session, in this system's vocabulary.
//
// Decisions, never personal data: PROVIDER_BOUNDARY §3 is explicit that the
// answer is `age_verified`, `jurisdiction`, `sanctions_clear` and
// `verified_at`, and this type carries exactly that shape as sub-check
// outcomes.
type Result struct {
	ProviderRef string
	Status      SessionStatus
	// RawStatus is the provider's own string, recorded so a provider that
	// later contradicts itself can be argued against something.
	RawStatus string
	// FailureReason is a safe reason code, never a document or a reason that
	// identifies the person.
	FailureReason string
	// Checks are the sub-check answers. A kind the provider did not answer is
	// absent rather than defaulted, and absence is not a pass.
	Checks []CheckResult
	// Jurisdiction is what the provider determined, when it determines one.
	Jurisdiction rules.Jurisdiction
	// AgeAtLeast is the age the provider attests, or zero for "did not say".
	AgeAtLeast int
	// Sandbox marks a rehearsal result.
	Sandbox bool
}

// CheckResult is one sub-check answer from a provider.
type CheckResult struct {
	Kind    CheckKind
	Outcome Outcome
	Detail  string
}

// ErrNoProvider is returned when no verification provider is configured. It is
// the honest state of a deployment with no identity vendor, and every surface
// that reaches it says so rather than reporting that verification failed.
var ErrNoProvider = errors.New("no verification provider is configured")

// Registry holds the configured providers.
//
// A provider with no contract reference is a sandbox by definition, and the
// registry refuses one unless it was built to allow sandboxes — which
// `cmd/api` does exactly when the deployment declared itself a sandbox tier.
// It is the same programmatic assertion `payout.Registry` makes for the
// conversion role: production cannot load a rehearsal.
type Registry struct {
	byName       map[string]Provider
	allowSandbox bool
}

// NewRegistry returns an empty registry.
func NewRegistry(allowSandbox bool) *Registry {
	return &Registry{byName: map[string]Provider{}, allowSandbox: allowSandbox}
}

// Register adds a provider, refusing one that cannot be used.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return errs.New(errs.CodeValidationFailed, "verification: a nil provider cannot be registered")
	}
	name := strings.TrimSpace(p.Name())
	if name == "" {
		return errs.New(errs.CodeValidationFailed, "verification: a provider must have a name")
	}
	if _, dup := r.byName[name]; dup {
		return errs.Newf(errs.CodeConflict, "verification: provider %q is already registered", name)
	}
	caps := p.Capabilities()
	if strings.TrimSpace(caps.ContractReference) == "" && !r.allowSandbox {
		return errs.Newf(errs.CodeForbidden,
			"verification: provider %q has no contract reference, so it is a sandbox; this deployment does not permit one", name)
	}
	if !caps.Availability.Usable() {
		return errs.Newf(errs.CodeForbidden,
			"verification: provider %q reports availability %q and cannot be used", name, caps.Availability)
	}
	if caps.Availability == AvailabilitySandbox && !r.allowSandbox {
		return errs.Newf(errs.CodeForbidden,
			"verification: provider %q is sandbox-only and this deployment does not permit one", name)
	}
	if !caps.HostedFlow {
		return errs.Newf(errs.CodeForbidden,
			"verification: provider %q does not host the document capture; Nodal never renders one", name)
	}
	r.byName[name] = p
	return nil
}

// Get returns a registered provider by name.
func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.byName[strings.TrimSpace(name)]
	if !ok {
		return nil, errs.Newf(errs.CodeProviderUnavailable, "verification: no provider named %q is configured", name)
	}
	return p, nil
}

// Only returns the single configured provider, or an error naming why there is
// not exactly one. A deployment runs one identity vendor at a time; choosing
// between two is a product decision nobody has made.
func (r *Registry) Only() (Provider, error) {
	switch len(r.byName) {
	case 0:
		return nil, errs.Wrap(ErrNoProvider, errs.CodeProviderUnavailable,
			"verification is not available on this deployment")
	case 1:
		for _, p := range r.byName {
			return p, nil
		}
	}
	return nil, errs.Newf(errs.CodeProviderUnavailable,
		"verification: %d providers are configured and nothing chooses between them", len(r.byName))
}

// Names returns the registered provider names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
