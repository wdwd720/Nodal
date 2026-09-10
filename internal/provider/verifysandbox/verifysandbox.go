// Package verifysandbox is the identity-verification provider of a sandbox
// tier: it hosts nothing, decides nothing on its own, and answers exactly what
// a person driving the rehearsal told it to answer.
//
// It exists so the withdrawal journey — verify, become eligible, request a
// conversion, watch a provider settle it — can be exercised end to end on a
// deployment where every value is a rehearsal, by the real verification
// service against the real Provider contract. It is a first-class provider
// rather than a test double for the same two reasons `payoutsandbox` is:
// production wiring may not import a test double (scripts/lintfin), and a
// provider that refuses to exist in PROD has to say so in a place PROD
// compiles.
//
// # There is no default outcome, and that is the whole point
//
// A session this provider has issued sits in PENDING_USER_ACTION until
// somebody chooses what it should decide. It never drifts to approved, it has
// no timer that approves, and `Get` on a session nobody has answered reports
// the pending status rather than a verdict. "Approved unless told otherwise" is
// a fabricated approval with extra steps, and the goal forbids one.
//
// # It is faithful about the inconvenient parts
//
//   - The hosted URL it returns is `sandbox:` scheme. No browser will follow
//     it by accident, which is the point: there is no hosted page here, and a
//     link that looked like one would be the first thing somebody mistook for
//     a real identity flow.
//   - Every session, every result and every sub-check it produces carries
//     `Sandbox: true`, and migration 00762's CHECK refuses that combination in
//     PROD.
//   - A reference it has never seen answers "no such session" rather than
//     declining it. An instance restart on a free tier loses this memory, and
//     a person must not be declared unverifiable on the strength of
//     forgetfulness.
//   - `Resume` reissues a link on the SAME session and keeps whatever outcome
//     was already chosen, because a real provider counts attempts.
package verifysandbox

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/verification"
)

// Name is the provider name configuration selects it by.
const Name = "sandbox_verification"

// LinkTTL is how long a hosted link lasts. Real hosted links expire in minutes
// (PROVIDER_BOUNDARY §3) and a sandbox that never expired would rehearse a
// flow the real one does not have.
const LinkTTL = 15 * time.Minute

// AttestsAgeAtLeast is the age a passing sandbox age check attests. It matches
// the default minimum in `verification/rules`, so a sandbox provider can
// satisfy the jurisdictions the rule table permits and no more.
const AttestsAgeAtLeast = 18

// Provider implements verification.Provider and verification.SandboxController.
type Provider struct {
	mu  sync.Mutex
	now func() time.Time
	byRef map[string]*session
}

type session struct {
	ref       string
	purpose   verification.Purpose
	issuedAt  time.Time
	expiresAt time.Time
	outcome   verification.SandboxOutcome // empty until somebody chooses
	chosenAt  time.Time
}

// New returns the sandbox verification provider, or refuses when the
// environment is PROD.
//
// The refusal is the provider's own and does not depend on any registry being
// built correctly: a production binary that somehow asked for this gets an
// error, not a provider that pretends to verify people.
func New(env config.Environment, now func() time.Time) (*Provider, error) {
	if env == config.EnvProd {
		return nil, errs.New(errs.CodeForbidden,
			"verifysandbox: the sandbox verification provider cannot exist in PROD; a production identity decision comes from a contracted provider or from nowhere")
	}
	if now == nil {
		now = time.Now
	}
	return &Provider{now: now, byRef: map[string]*session{}}, nil
}

// Name implements verification.Provider.
func (p *Provider) Name() string { return Name }

// Capabilities implements verification.Provider.
//
// There is no contract reference, which is what makes the registry refuse this
// provider anywhere sandboxes are not explicitly allowed. It claims to perform
// all four PAYOUT_KYC sub-checks and the PEP screen, because the point of the
// rehearsal is to exercise every branch of §21 — and it claims US only, which
// is what the rule table and the sandbox payout provider both say.
func (p *Provider) Capabilities() verification.Capabilities {
	return verification.Capabilities{
		PerformsIdentityDocument:   true,
		PerformsAgeCheck:           true,
		AttestsAgeAtLeast:          AttestsAgeAtLeast,
		PerformsSanctionsScreening: true,
		PerformsPEPScreening:       true,
		HostedFlow:                 true,
		SupportsWebhooks:           false,
		SupportedCountries:         []string{"US"},
		Availability:               verification.AvailabilitySandbox,
		ContractReference:          "",
	}
}

// Start implements verification.Provider. It issues a reference and a
// deliberately non-navigable link, and decides nothing.
func (p *Provider) Start(_ context.Context, req verification.SessionRequest) (verification.StartedSession, error) {
	if err := req.Validate(); err != nil {
		return verification.StartedSession{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().UTC()
	ref := "sandbox-verif-" + req.SubjectRef
	s, ok := p.byRef[ref]
	if !ok {
		s = &session{ref: ref, purpose: req.Purpose, issuedAt: now}
		p.byRef[ref] = s
	}
	s.expiresAt = now.Add(LinkTTL)
	return p.startedLocked(s), nil
}

// Resume implements verification.Provider: a fresh link on the same session,
// with whatever outcome was already chosen left alone.
func (p *Provider) Resume(_ context.Context, providerRef string) (verification.StartedSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byRef[providerRef]
	if !ok {
		return verification.StartedSession{}, errs.Newf(errs.CodeNotFound,
			"verifysandbox: no session %q on this instance", providerRef)
	}
	s.expiresAt = p.now().UTC().Add(LinkTTL)
	return p.startedLocked(s), nil
}

func (p *Provider) startedLocked(s *session) verification.StartedSession {
	return verification.StartedSession{
		ProviderRef: s.ref,
		// Deliberately not an http URL. There is no hosted page here, and a
		// link that looked navigable is the first thing somebody would mistake
		// for a real identity flow. The API returns the sandbox control path
		// alongside it.
		HostedURL:   "sandbox:verification/" + s.ref,
		ExpiresAt:   s.expiresAt,
		Environment: "SANDBOX",
		Sandbox:     true,
	}
}

// Get implements verification.Provider.
//
// A session nobody has answered is PENDING_USER_ACTION, forever. A session
// whose link has expired and which nobody answered is EXPIRED, because a real
// hosted link does expire and the journey has to be able to rehearse that.
func (p *Provider) Get(_ context.Context, providerRef string) (verification.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byRef[providerRef]
	if !ok {
		return verification.Result{}, errs.Newf(errs.CodeNotFound,
			"verifysandbox: no session %q on this instance; a restart loses this memory and nobody is declined for that",
			providerRef)
	}
	return p.resultLocked(s), nil
}

// ParseWebhook implements verification.Provider. This provider delivers no
// webhooks — it has nowhere to deliver them from — and says so rather than
// accepting a body somebody could post.
func (p *Provider) ParseWebhook(http.Header, []byte) (verification.Result, error) {
	return verification.Result{}, errs.New(errs.CodeUnsupported,
		"verifysandbox: the sandbox verification provider sends no webhooks; poll it instead")
}

// SetOutcome implements verification.SandboxController.
func (p *Provider) SetOutcome(providerRef string, outcome verification.SandboxOutcome) error {
	if !outcome.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "verifysandbox: unknown outcome %q", outcome)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byRef[providerRef]
	if !ok {
		return errs.Newf(errs.CodeNotFound, "verifysandbox: no session %q on this instance", providerRef)
	}
	if s.outcome != "" && s.outcome != outcome {
		// A decided session is decided. Letting a rehearsal be re-decided
		// would rehearse a provider nobody has.
		return errs.Newf(errs.CodeConflict,
			"verifysandbox: session %q already decided %s", providerRef, s.outcome)
	}
	s.outcome = outcome
	s.chosenAt = p.now().UTC()
	return nil
}

// resultLocked renders a session as a Result. Every branch names the sub-checks
// it answers and leaves the rest ABSENT rather than defaulting them: absence is
// not a pass anywhere in this system.
func (p *Provider) resultLocked(s *session) verification.Result {
	res := verification.Result{
		ProviderRef: s.ref,
		Sandbox:     true,
	}
	pass := func(kind verification.CheckKind) verification.CheckResult {
		return verification.CheckResult{Kind: kind, Outcome: verification.OutcomePass, Detail: "SANDBOX_OUTCOME"}
	}
	fail := func(kind verification.CheckKind, detail string) verification.CheckResult {
		return verification.CheckResult{Kind: kind, Outcome: verification.OutcomeFail, Detail: detail}
	}

	switch s.outcome {
	case "":
		if !p.now().UTC().Before(s.expiresAt) {
			res.Status = verification.SessionExpired
			res.RawStatus = "sandbox_link_expired"
			res.FailureReason = "SANDBOX_LINK_EXPIRED"
			return res
		}
		res.Status = verification.SessionPendingUserAction
		res.RawStatus = "sandbox_awaiting_outcome"
		return res

	case verification.SandboxVerified:
		res.Status = verification.SessionApproved
		res.RawStatus = "sandbox_approved"
		res.AgeAtLeast = AttestsAgeAtLeast
		res.Checks = []verification.CheckResult{
			pass(verification.CheckIdentityDocument),
			pass(verification.CheckAge),
			pass(verification.CheckJurisdiction),
			pass(verification.CheckSanctions),
			pass(verification.CheckPEP),
		}

	case verification.SandboxNeedsInformation:
		res.Status = verification.SessionRequiresInput
		res.RawStatus = "sandbox_requires_input"
		res.FailureReason = "SANDBOX_MORE_INFORMATION_REQUESTED"
		res.Checks = []verification.CheckResult{{
			Kind: verification.CheckIdentityDocument, Outcome: verification.OutcomeNeedsInformation,
			Detail: "SANDBOX_OUTCOME",
		}}

	case verification.SandboxRejected:
		res.Status = verification.SessionDeclined
		res.RawStatus = "sandbox_declined"
		res.FailureReason = "SANDBOX_DOCUMENT_NOT_ACCEPTED"
		res.Checks = []verification.CheckResult{fail(verification.CheckIdentityDocument, "SANDBOX_OUTCOME")}

	case verification.SandboxUnderage:
		// The document is fine and the person is too young. Two different
		// answers, which is exactly why §21 refuses to let one boolean stand
		// for both.
		res.Status = verification.SessionDeclined
		res.RawStatus = "sandbox_declined_underage"
		res.FailureReason = "SANDBOX_UNDER_MINIMUM_AGE"
		res.Checks = []verification.CheckResult{
			pass(verification.CheckIdentityDocument),
			fail(verification.CheckAge, "SANDBOX_OUTCOME"),
		}

	case verification.SandboxSanctioned:
		res.Status = verification.SessionDeclined
		res.RawStatus = "sandbox_declined_sanctions"
		res.FailureReason = "SANDBOX_SANCTIONS_MATCH"
		res.AgeAtLeast = AttestsAgeAtLeast
		res.Checks = []verification.CheckResult{
			pass(verification.CheckIdentityDocument),
			pass(verification.CheckAge),
			pass(verification.CheckJurisdiction),
			fail(verification.CheckSanctions, "SANDBOX_OUTCOME"),
		}
	}
	return res
}
