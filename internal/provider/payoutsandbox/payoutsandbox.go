// Package payoutsandbox is the payout provider of a sandbox tier: it accepts a
// payout, settles it a little later, and moves nothing.
//
// It exists so the withdrawal journey -- verification, eligibility, reserve,
// submit, provider pending, reconcile, settle -- can be exercised end to end
// on a deployment where every value is a rehearsal, by the real payout
// service against the real Provider contract. It is a first-class provider
// rather than the test double in internal/payout/payouttest because production
// wiring may not import a test double (scripts/lintfin), and because a
// provider that refuses to exist in PROD has to say so in a place PROD
// compiles.
//
// It is deliberately faithful about the inconvenient parts of a real provider:
// the same idempotency key is the same payout; a submission is ACCEPTED first
// and SETTLED only on a later lookup, so the request passes through
// PROVIDER_PENDING and the reconciler has work to do; and a key this instance
// has never seen answers UNKNOWN rather than FAILED, because an instance
// restart on a free tier loses this memory and a payout must not be declared
// failed on the strength of forgetfulness.
package payoutsandbox

import (
	"context"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
)

// Name is the provider name configuration selects it by.
const Name = "sandbox_payout"

// SettleAfter is how long an accepted payout stays pending before a lookup
// reports it settled. Long enough to observe PROVIDER_PENDING, short enough
// for a browser test.
const SettleAfter = 10 * time.Second

// Provider implements payout.Provider.
type Provider struct {
	mu    sync.Mutex
	now   func() time.Time
	byKey map[string]record
}

type record struct {
	acceptedAt time.Time
	result     payout.SubmitResult
}

// New returns the sandbox provider, or refuses when the environment is PROD.
// The refusal is the provider's own and does not depend on any registry
// being built correctly: a production binary that somehow asked for this
// would get an error, not a provider that pretends to pay.
func New(env config.Environment, now func() time.Time) (*Provider, error) {
	if env == config.EnvProd {
		return nil, errs.New(errs.CodeForbidden,
			"payoutsandbox: the sandbox payout provider cannot exist in PROD; a production payout is a licensed provider or nothing")
	}
	if now == nil {
		now = time.Now
	}
	return &Provider{now: now, byKey: map[string]record{}}, nil
}

// Name implements payout.Provider.
func (p *Provider) Name() string { return Name }

// Capabilities implements payout.Provider. There is no contract reference,
// which is what makes the registry refuse this provider anywhere sandboxes are
// not explicitly allowed. Identity verification is declared as performed by
// the provider because the sandbox verification provider is the one that
// performs it on this tier; nothing here collects a document.
func (p *Provider) Capabilities() payout.Capabilities {
	return payout.Capabilities{
		SupportsBankPayout:     true,
		SupportsFiatWallet:     true,
		SupportsLookup:         true,
		Currencies:             []string{"USD"},
		RequiresKYC:            true,
		KYCPerformedByProvider: true,
		RecipientKinds:         []string{"individual"},
		SupportedCountries:     []string{"US"},
		Availability:           payout.AvailabilitySandbox,
		// A fee model so the quote step has something to quote FROM. The
		// numbers are placeholders and the version name says so in words: they
		// are not a price anybody has agreed, and nothing may present them as
		// one. They are shaped like a real payout fee — a flat part plus a
		// proportional part, which is what every provider in
		// PROVIDER_BOUNDARY §5 charges — so the arithmetic, the rounding and
		// the "net is below the minimum" branch are all exercised.
		FeeModelPublished: true,
		FeeFlat:           money.USDFromMinor(25),
		FeeBasisPoints:    money.BPS(25),
		FeeModelVersion:   "SANDBOX-PLACEHOLDER-NOT-A-PRICE",
		// A minimum, so the journey can rehearse a payout the provider refuses
		// for being too small — which is a real refusal a customer meets and
		// which is judged NET of fees.
		MinimumAmount: money.USDFromMinor(100),
	}
}

// Submit implements payout.Provider. The first submission under a key is
// recorded ACCEPTED; every later one returns the stored result.
func (p *Provider) Submit(_ context.Context, req payout.SubmitRequest) (payout.SubmitResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.byKey[req.IdempotencyKey]; ok {
		return p.settleIfDue(req.IdempotencyKey, existing), nil
	}
	now := p.now().UTC()
	res := payout.SubmitResult{
		Status:            payout.ProviderAccepted,
		ProviderReference: "sandbox-" + req.IdempotencyKey,
		RawStatus:         "accepted",
	}
	p.byKey[req.IdempotencyKey] = record{acceptedAt: now, result: res}
	return res, nil
}

// Lookup implements payout.Provider.
func (p *Provider) Lookup(_ context.Context, key string) (payout.SubmitResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.byKey[key]
	if !ok {
		// Not "failed": this instance may simply not be the one that accepted
		// it. UNKNOWN sends the request to manual review, where a person
		// decides, which is the honest answer for a provider with no memory.
		return payout.SubmitResult{
			Status: payout.ProviderUnknown, RawStatus: "unknown",
			FailureReason: "the sandbox provider has no record of this payout on this instance",
		}, nil
	}
	return p.settleIfDue(key, rec), nil
}

func (p *Provider) settleIfDue(key string, rec record) payout.SubmitResult {
	if rec.result.Status == payout.ProviderAccepted && !p.now().Before(rec.acceptedAt.Add(SettleAfter)) {
		at := rec.acceptedAt.Add(SettleAfter)
		rec.result.Status = payout.ProviderSettled
		rec.result.RawStatus = "settled"
		rec.result.SettledAt = &at
		p.byKey[key] = rec
	}
	return rec.result
}
