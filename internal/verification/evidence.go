package verification

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// CheckKind is one of the things §21 says must be able to refuse on its own:
// "age, country, state, jurisdiction, sanctions, risk, provider availability
// ... Do not fake this with a checkbox."
//
// Five kinds, because five independent answers can each be no. Country and
// state are one kind (JURISDICTION) because they are one question asked at two
// resolutions, and provider availability is not a check on the PERSON — it is a
// property of the deployment and lives in the eligibility explanation.
type CheckKind string

// Check kinds.
const (
	// CheckIdentityDocument is "is this person who they say they are". The
	// provider captures and verifies the document; Nodal records the verdict.
	CheckIdentityDocument CheckKind = "IDENTITY_DOCUMENT"
	// CheckAge is "is this person old enough here". The threshold comes from
	// the versioned rule table, never from a provider's default.
	CheckAge CheckKind = "AGE"
	// CheckJurisdiction is "is this person somewhere this is offered".
	CheckJurisdiction CheckKind = "JURISDICTION"
	// CheckSanctions is the sanctions screening result.
	CheckSanctions CheckKind = "SANCTIONS"
	// CheckPEP is the politically-exposed-person flag. It is separate from
	// sanctions because it is not a prohibition — it is a reason for enhanced
	// diligence — and merging the two would turn a review into a refusal.
	CheckPEP CheckKind = "PEP"
)

var allCheckKinds = []CheckKind{
	CheckIdentityDocument, CheckAge, CheckJurisdiction, CheckSanctions, CheckPEP,
}

// AllCheckKinds returns every declared kind in declaration order (a copy).
func AllCheckKinds() []CheckKind { return append([]CheckKind(nil), allCheckKinds...) }

// Valid reports whether k is declared.
func (k CheckKind) Valid() bool {
	for _, x := range allCheckKinds {
		if x == k {
			return true
		}
	}
	return false
}

func (k CheckKind) String() string { return string(k) }

// ParseCheckKind parses the canonical uppercase form.
func ParseCheckKind(in string) (CheckKind, error) {
	k := CheckKind(strings.ToUpper(strings.TrimSpace(in)))
	if !k.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown verification check kind %q", in)
	}
	return k, nil
}

// Outcome is one sub-check's answer.
type Outcome string

// Outcomes.
const (
	// OutcomePass is the only answer that contributes to a level.
	OutcomePass Outcome = "PASS"
	// OutcomeFail is a refusal.
	OutcomeFail Outcome = "FAIL"
	// OutcomeNeedsInformation is the provider asking for more.
	OutcomeNeedsInformation Outcome = "NEEDS_INFORMATION"
	// OutcomeUnknown is the provider not having answered. It is NOT a pass and
	// NOT a fail: a provider that has not screened somebody has not cleared
	// them either, and the resolver treats it as missing evidence.
	OutcomeUnknown Outcome = "UNKNOWN"
	// OutcomeNotApplicable is a check this provider does not perform, or one
	// that does not apply in this jurisdiction. Only PEP may legitimately be
	// NOT_APPLICABLE and still support a level; the other four must PASS.
	OutcomeNotApplicable Outcome = "NOT_APPLICABLE"
)

var allOutcomes = []Outcome{
	OutcomePass, OutcomeFail, OutcomeNeedsInformation, OutcomeUnknown, OutcomeNotApplicable,
}

// AllOutcomes returns every declared outcome in declaration order (a copy).
func AllOutcomes() []Outcome { return append([]Outcome(nil), allOutcomes...) }

// Valid reports whether o is declared.
func (o Outcome) Valid() bool {
	for _, x := range allOutcomes {
		if x == o {
			return true
		}
	}
	return false
}

func (o Outcome) String() string { return string(o) }

// ParseOutcome parses the canonical uppercase form.
func ParseOutcome(in string) (Outcome, error) {
	o := Outcome(strings.ToUpper(strings.TrimSpace(in)))
	if !o.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown verification outcome %q", in)
	}
	return o, nil
}

// Check mirrors a verification_checks row: one sub-check, its answer, who
// produced it, under which rule version, and whether it is a rehearsal.
//
// Detail is a safe reason code. It is never a document, a government
// identifier or a date of birth: the provider holds the evidence and Nodal
// holds the conclusion (goal §20).
type Check struct {
	ID           CheckID
	SessionID    SessionID
	UserID       accounts.UserID
	Kind         CheckKind
	Outcome      Outcome
	Provider     string
	ProviderRef  string
	RulesVersion string
	Environment  string
	Sandbox      bool
	Detail       string
	RecordedAt   time.Time
}

// requiredForPayoutKYC are the checks that must have PASSED before this system
// will report PAYOUT_KYC. PEP is deliberately absent from the list and handled
// separately: a politically exposed person is not disqualified, they are a
// reason for enhanced diligence.
var requiredForPayoutKYC = []CheckKind{
	CheckIdentityDocument, CheckAge, CheckJurisdiction, CheckSanctions,
}

// RequiredChecks returns the sub-checks a level needs, in declaration order.
func RequiredChecks(purpose Purpose) []CheckKind {
	base := append([]CheckKind(nil), requiredForPayoutKYC...)
	if purpose == PurposeEnhanced {
		// Enhanced due diligence is exactly the ordinary set plus an explicit
		// answer on political exposure. "We did not look" is not an answer.
		base = append(base, CheckPEP)
	}
	return base
}

// latest returns the most recent check of each kind, preferring the later
// RecordedAt and, on a tie, the later id — so the answer is deterministic even
// when two rows share a timestamp.
func latest(checks []Check) map[CheckKind]Check {
	out := make(map[CheckKind]Check, len(allCheckKinds))
	for _, c := range checks {
		prev, ok := out[c.Kind]
		if !ok || c.RecordedAt.After(prev.RecordedAt) ||
			(c.RecordedAt.Equal(prev.RecordedAt) && id.Compare(c.ID, prev.ID) > 0) {
			out[c.Kind] = c
		}
	}
	return out
}

// EvidenceSatisfies reports whether the checks support a purpose, and which
// kinds are missing or refusing when they do not.
//
// It fails closed in every direction: an absent kind, an UNKNOWN answer and a
// NEEDS_INFORMATION answer all count as unsatisfied, and only PEP may be
// NOT_APPLICABLE.
func EvidenceSatisfies(purpose Purpose, checks []Check) (bool, []CheckKind) {
	byKind := latest(checks)
	var missing []CheckKind
	for _, kind := range RequiredChecks(purpose) {
		c, ok := byKind[kind]
		switch {
		case !ok:
			missing = append(missing, kind)
		case c.Outcome == OutcomePass:
		default:
			// FAIL, NEEDS_INFORMATION, UNKNOWN and NOT_APPLICABLE all land
			// here. NOT_APPLICABLE is worth naming: a provider that does not
			// screen for political exposure has said so honestly, and for
			// ENHANCED that is still not good enough. PEP is only in
			// RequiredChecks for ENHANCED, so this never refuses PAYOUT_KYC
			// over a check that level does not need.
			missing = append(missing, kind)
		}
	}
	return len(missing) == 0, missing
}

// AnySandbox reports whether any of the evidence is a rehearsal. It drives the
// sandbox label on every response that shows a level, so a sandbox-derived
// verification is never displayed as a real one.
func AnySandbox(checks []Check) bool {
	for _, c := range checks {
		if c.Sandbox {
			return true
		}
	}
	return false
}
