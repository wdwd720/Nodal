package verification

import (
	"context"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// Action is what a person can do next about a missing requirement. It is the
// difference between a dead end and a next step, and goal §19 is explicit that
// the product must offer the second: "Verify your identity to enable withdrawal
// eligibility", never "verify now to turn your Credits into cash".
type Action string

// Actions.
const (
	// ActionStartVerification is "begin a verification".
	ActionStartVerification Action = "START_VERIFICATION"
	// ActionContinueVerification is "finish the one you started".
	ActionContinueVerification Action = "CONTINUE_VERIFICATION"
	// ActionProvideInformation is "the provider asked you for something".
	ActionProvideInformation Action = "PROVIDE_INFORMATION"
	// ActionReverify is "your verification aged out; do it again".
	ActionReverify Action = "REVERIFY"
	// ActionContactSupport is a state a person cannot resolve alone.
	ActionContactSupport Action = "CONTACT_SUPPORT"
	// ActionNone is "there is nothing you can do about this one", which is the
	// honest answer for a jurisdiction the product is not offered in and for a
	// provider that has not been contracted.
	ActionNone Action = "NONE"
	// ActionWait is "somebody else is deciding".
	ActionWait Action = "WAIT"
)

// Requirement is one thing standing between a person and a level, with what
// they can do about it.
type Requirement struct {
	// Code is machine-readable: a CheckKind, or one of the codes below.
	Code   string
	Detail string
	Action Action
}

// Requirement codes that are not sub-checks.
const (
	// RequirementSession is "no verification has been started".
	RequirementSession = "VERIFICATION_SESSION"
	// RequirementJurisdiction is the country or subdivision refusing.
	RequirementJurisdiction = "JURISDICTION"
	// RequirementProvider is a deployment with no identity vendor, or one
	// whose vendor cannot establish the level here.
	RequirementProvider = "PROVIDER"
	// RequirementOperator is a suspension only an operator can lift.
	RequirementOperator = "OPERATOR_REVIEW"
	// RequirementExpired is a decision that aged out.
	RequirementExpired = "VERIFICATION_EXPIRED"
)

// Snapshot is the financial profile area of goal §24: what has been
// established, what has not, and what to do next — with no PII anywhere in it.
//
// There is no document identifier, no government number and no date of birth,
// because this package never had any. AgeVerified is a boolean and
// MinimumAge is the threshold that boolean was judged against.
type Snapshot struct {
	AccountID accounts.AccountID
	UserID    accounts.UserID

	State State
	// Level is the composite level, computed the same way the resolver
	// computes it, so the profile screen and the payout engine can never
	// disagree.
	Level valuedomain.VerificationLevel

	Jurisdiction          rules.Jurisdiction
	JurisdictionSupported bool
	// JurisdictionRefusals says why it is not supported, when it is not.
	JurisdictionRefusals []string
	MinimumAge           int

	AgeVerified    bool
	SanctionsState compliance.SanctionsState
	Restrictions   []string

	VerifiedAt *time.Time
	ExpiresAt  *time.Time

	// Session is the person's live attempt, if there is one.
	Session *Session
	// Checks is the evidence, latest answer per kind, ordered by kind.
	Checks []Check
	// Sandbox is true when any of it is a rehearsal. Every response that
	// carries a Snapshot repeats this, so a sandbox verification is never
	// displayed as a real one.
	Sandbox bool

	// Missing is what stands between this person and PAYOUT_KYC, with the
	// action for each. Empty when nothing does.
	Missing []Requirement

	RulesVersion string
	// Provider is the configured identity vendor, or empty when there is
	// none — which is the honest state of this deployment today.
	Provider string
	// ProviderAvailability is how far that vendor is actually usable.
	ProviderAvailability Availability
}

// PayoutReady reports whether this person's verification, by itself, no longer
// stands between them and a withdrawal.
//
// It says nothing about whether a withdrawal is possible: the value-domain
// policy, the capability gates and the provider each answer separately, and
// `internal/eligibility` composes all four. Verification is one input.
func (s Snapshot) PayoutReady() bool {
	return s.Level.AtLeast(valuedomain.VerificationPayoutKYC) && len(s.Missing) == 0
}

// Snapshot assembles the profile view for an account.
func (s *Service) Snapshot(ctx context.Context, q db.Querier, accountID accounts.AccountID, base valuedomain.VerificationLevel) (Snapshot, error) {
	owner, err := s.owner(ctx, q, accountID)
	if err != nil {
		return Snapshot{}, err
	}
	now := s.deps.Clock.Now().UTC()

	state, verifiedAt, expiresAt, err := s.deps.Repo.ProfileState(ctx, q, owner)
	if err != nil {
		return Snapshot{}, err
	}
	profile, err := s.deps.Compliance.Get(ctx, q, owner)
	if err != nil && errs.CodeOf(err) != errs.CodeNotFound {
		return Snapshot{}, err
	}
	checks, err := s.deps.Repo.ChecksForUser(ctx, q, owner)
	if err != nil {
		return Snapshot{}, err
	}
	session, hasSession, err := s.deps.Repo.OpenSession(ctx, q, owner)
	if err != nil {
		return Snapshot{}, err
	}

	j := rules.Jurisdiction{Country: profile.JurisdictionCountry, Region: profile.JurisdictionRegion}
	jurisdictionOK, refusals := rules.CheckJurisdiction(j)
	minimumAge, regionKnown := rules.MinimumAge(j)
	if !regionKnown {
		minimumAge = rules.HighestMinimumAge(j.Country)
	}

	out := Snapshot{
		AccountID:             accountID,
		UserID:                owner,
		State:                 state,
		Level:                 levelFrom(base, state, expiresAt, checks, now),
		Jurisdiction:          j.Normalized(),
		JurisdictionSupported: jurisdictionOK,
		JurisdictionRefusals:  refusalStrings(refusals),
		MinimumAge:            minimumAge,
		AgeVerified:           profile.AgeVerified,
		SanctionsState:        profile.SanctionsState,
		Restrictions:          append([]string(nil), profile.Restrictions...),
		VerifiedAt:            verifiedAt,
		ExpiresAt:             expiresAt,
		Checks:                latestOrdered(checks),
		Sandbox:               AnySandbox(checks) || (hasSession && session.Sandbox),
		RulesVersion:          rules.Version,
	}
	if out.SanctionsState == "" {
		out.SanctionsState = compliance.SanctionsUnknown
	}
	if hasSession {
		out.Session = &session
	}
	if p, perr := s.Provider(); perr == nil {
		out.Provider = p.Name()
		out.ProviderAvailability = p.Capabilities().Availability
	}
	out.Missing = s.missing(out, checks, hasSession, now)
	return out, nil
}

// missing is "what is missing and what to do next", derived rather than
// narrated: every entry names a machine-readable code and an action, so a
// client renders the sentence and the server decides the facts.
func (s *Service) missing(snap Snapshot, checks []Check, hasSession bool, now time.Time) []Requirement {
	var out []Requirement
	add := func(code, detail string, a Action) { out = append(out, Requirement{Code: code, Detail: detail, Action: a}) }

	if snap.Provider == "" {
		add(RequirementProvider,
			"this deployment has no identity verification provider configured, so verification cannot be started here",
			ActionNone)
	}
	if !snap.JurisdictionSupported {
		detail := "verification is not offered in " + snap.Jurisdiction.String()
		if snap.Jurisdiction.Country == "" {
			detail = "tell us which country you are in; it is never inferred from your network address"
			add(RequirementJurisdiction, detail, ActionStartVerification)
		} else {
			add(RequirementJurisdiction, detail, ActionNone)
		}
	}

	switch snap.State {
	case StateSuspended:
		add(RequirementOperator, "your account is under review", ActionContactSupport)
	case StateRejected:
		add(RequirementSession, "the last verification was not accepted; you can try again", ActionStartVerification)
	case StateExpired:
		add(RequirementExpired, "your verification has expired and needs to be renewed", ActionReverify)
	case StateUnverified, StateRequired:
		add(RequirementSession, "identity verification has not been started", ActionStartVerification)
	case StateStarted:
		add(RequirementSession, "your verification is open and has not been completed", ActionContinueVerification)
	case StateNeedsInformation:
		add(RequirementSession, "the provider has asked you for more information", ActionProvideInformation)
	case StatePending:
		add(RequirementSession, "the provider is reviewing your verification", ActionWait)
	case StateVerified, StateRestricted:
		if expired(snap.ExpiresAt, now) {
			add(RequirementExpired, "your verification has passed its validity window", ActionReverify)
		}
	}

	// The sub-checks, whatever the state says. A person in RESTRICTED needs to
	// know WHICH of the five refused, and a person in VERIFIED with a stale
	// sanctions screen needs to know that too.
	if _, gaps := EvidenceSatisfies(PurposePayoutKYC, checks); len(gaps) > 0 && snap.State != StateUnverified {
		byKind := latest(checks)
		for _, kind := range gaps {
			c, seen := byKind[kind]
			switch {
			case !seen:
				add(string(kind), "this check has not been performed", checkAction(hasSession))
			case c.Outcome == OutcomeFail:
				add(string(kind), "this check did not pass", failAction(kind))
			case c.Outcome == OutcomeNeedsInformation:
				add(string(kind), "the provider needs more for this check", ActionProvideInformation)
			default:
				add(string(kind), "this check has no answer yet", checkAction(hasSession))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func checkAction(hasSession bool) Action {
	if hasSession {
		return ActionContinueVerification
	}
	return ActionStartVerification
}

// failAction is what a person can do about a failed sub-check. Age and
// jurisdiction are facts about them that another attempt will not change, and
// saying "try again" would be a lie; a document can be retried, and a sanctions
// result is a person's decision to make, not an automated retry.
func failAction(kind CheckKind) Action {
	switch kind {
	case CheckIdentityDocument:
		return ActionStartVerification
	case CheckSanctions, CheckPEP:
		return ActionContactSupport
	}
	return ActionNone
}

// latestOrdered returns the latest answer per kind, ordered by kind, so two
// identical states render identically.
func latestOrdered(checks []Check) []Check {
	byKind := latest(checks)
	out := make([]Check, 0, len(byKind))
	for _, kind := range AllCheckKinds() {
		if c, ok := byKind[kind]; ok {
			out = append(out, c)
		}
	}
	return out
}
