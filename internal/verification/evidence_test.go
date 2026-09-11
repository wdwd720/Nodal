package verification

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/valuedomain"
)

func check(kind CheckKind, outcome Outcome, at time.Time, sandbox bool) Check {
	return Check{ID: NewCheckID(), Kind: kind, Outcome: outcome, RecordedAt: at, Sandbox: sandbox}
}

var t0 = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// withCheck returns base plus one more check, copied rather than appended in
// place so a caller's slice is never aliased.
func withCheck(base []Check, extra Check) []Check {
	out := make([]Check, 0, len(base)+1)
	out = append(out, base...)
	return append(out, extra)
}

func passing(at time.Time) []Check {
	return []Check{
		check(CheckIdentityDocument, OutcomePass, at, false),
		check(CheckAge, OutcomePass, at, false),
		check(CheckJurisdiction, OutcomePass, at, false),
		check(CheckSanctions, OutcomePass, at, false),
	}
}

// Evidence is required, not implied. Absence, UNKNOWN and NEEDS_INFORMATION all
// fail closed, and each names the kind that is missing so a person can be told
// which one.
func TestEvidenceSatisfies_FailsClosedInEveryDirection(t *testing.T) {
	t.Parallel()

	ok, missing := EvidenceSatisfies(PurposePayoutKYC, passing(t0))
	assert.True(t, ok)
	assert.Empty(t, missing)

	ok, missing = EvidenceSatisfies(PurposePayoutKYC, nil)
	assert.False(t, ok)
	assert.ElementsMatch(t, RequiredChecks(PurposePayoutKYC), missing, "nothing recorded means everything missing")

	for _, refusing := range []Outcome{OutcomeFail, OutcomeUnknown, OutcomeNeedsInformation, OutcomeNotApplicable} {
		checks := withCheck(passing(t0)[:3], check(CheckSanctions, refusing, t0, false))
		ok, missing = EvidenceSatisfies(PurposePayoutKYC, checks)
		assert.Falsef(t, ok, "sanctions %s must not satisfy the level", refusing)
		assert.Equalf(t, []CheckKind{CheckSanctions}, missing, "sanctions %s", refusing)
	}
}

// PEP is required for ENHANCED and only for ENHANCED. A politically exposed
// person is not disqualified from PAYOUT_KYC — that is a reason for diligence,
// not a prohibition — but a provider that cannot answer the question at all
// cannot support ENHANCED.
func TestEvidenceSatisfies_PEPIsEnhancedOnly(t *testing.T) {
	t.Parallel()
	base := passing(t0)

	ok, _ := EvidenceSatisfies(PurposePayoutKYC, base)
	assert.True(t, ok, "PAYOUT_KYC does not depend on the PEP answer")

	ok, missing := EvidenceSatisfies(PurposeEnhanced, base)
	assert.False(t, ok)
	assert.Equal(t, []CheckKind{CheckPEP}, missing)

	ok, _ = EvidenceSatisfies(PurposeEnhanced, append(base, check(CheckPEP, OutcomePass, t0, false)))
	assert.True(t, ok)

	ok, missing = EvidenceSatisfies(PurposeEnhanced, append(base, check(CheckPEP, OutcomeNotApplicable, t0, false)))
	assert.False(t, ok, `"we do not screen for that" is honest and still not enough for ENHANCED`)
	assert.Equal(t, []CheckKind{CheckPEP}, missing)

	// And a PEP failure still does not block PAYOUT_KYC.
	withHit := withCheck(base, check(CheckPEP, OutcomeFail, t0, false))
	ok, _ = EvidenceSatisfies(PurposePayoutKYC, withHit)
	assert.True(t, ok, "political exposure is a reason for diligence, not a refusal of the base level")
}

// The latest answer per kind wins, and a later refusal overrides an earlier
// pass. A sanctions list that changes tomorrow is exactly why.
func TestEvidenceSatisfies_TheLatestAnswerWins(t *testing.T) {
	t.Parallel()
	later := t0.Add(time.Hour)
	checks := withCheck(passing(t0), check(CheckSanctions, OutcomeFail, later, false))
	ok, missing := EvidenceSatisfies(PurposePayoutKYC, checks)
	assert.False(t, ok)
	assert.Equal(t, []CheckKind{CheckSanctions}, missing)

	// And the other way round: a refusal that was later cleared.
	checks = []Check{
		check(CheckIdentityDocument, OutcomeFail, t0, false),
		check(CheckIdentityDocument, OutcomePass, later, false),
		check(CheckAge, OutcomePass, t0, false),
		check(CheckJurisdiction, OutcomePass, t0, false),
		check(CheckSanctions, OutcomePass, t0, false),
	}
	ok, _ = EvidenceSatisfies(PurposePayoutKYC, checks)
	assert.True(t, ok)
}

func TestAnySandbox(t *testing.T) {
	t.Parallel()
	assert.False(t, AnySandbox(passing(t0)))
	assert.True(t, AnySandbox(withCheck(passing(t0), check(CheckPEP, OutcomePass, t0, true))),
		"one rehearsal answer makes the whole level a rehearsal")
}

// levelFrom is the composite rule, and it fails closed at every step.
func TestLevelFrom_EarnsTheLevelFromEvidence(t *testing.T) {
	t.Parallel()
	future := t0.Add(24 * time.Hour)
	past := t0.Add(-time.Second)
	base := valuedomain.VerificationNodalIdentity

	t.Run("a base of NONE caps everything", func(t *testing.T) {
		got := levelFrom(valuedomain.VerificationNone, StateVerified, &future, passing(t0), t0)
		assert.Equal(t, valuedomain.VerificationNone, got,
			"an account that is not in good standing is NONE however good the KYC evidence is")
	})

	t.Run("a state that is not VERIFIED reports the base", func(t *testing.T) {
		for _, s := range AllStates() {
			if s == StateVerified {
				continue
			}
			assert.Equalf(t, base, levelFrom(base, s, &future, passing(t0), t0), "state %s", s)
		}
	})

	t.Run("an elapsed window reports the base before any sweep runs", func(t *testing.T) {
		assert.Equal(t, base, levelFrom(base, StateVerified, &past, passing(t0), t0),
			"reporting PAYOUT_KYC on a stale row is how an expired verification pays somebody out")
	})

	t.Run("incomplete evidence reports the base", func(t *testing.T) {
		partial := passing(t0)[:2]
		assert.Equal(t, base, levelFrom(base, StateVerified, &future, partial, t0))
	})

	t.Run("complete evidence earns PAYOUT_KYC", func(t *testing.T) {
		assert.Equal(t, valuedomain.VerificationPayoutKYC,
			levelFrom(base, StateVerified, &future, passing(t0), t0))
		assert.Equal(t, valuedomain.VerificationPayoutKYC,
			levelFrom(base, StateVerified, nil, passing(t0), t0), "no expiry recorded is not an expired one")
	})

	t.Run("a political-exposure answer earns ENHANCED", func(t *testing.T) {
		full := withCheck(passing(t0), check(CheckPEP, OutcomePass, t0, false))
		assert.Equal(t, valuedomain.VerificationEnhanced,
			levelFrom(base, StateVerified, &future, full, t0))
	})
}

// profileStateFor is the only place a session status becomes a verification
// state, and the one case worth naming is an approval with a failed sub-check:
// it produces RESTRICTED, not VERIFIED. Collapsing the two is how a sanctions
// hit becomes invisible.
func TestProfileStateFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status SessionStatus
		checks []Check
		want   State
		moves  bool
	}{
		{SessionCreated, nil, "", false},
		{SessionPendingUserAction, nil, StateStarted, true},
		{SessionProcessing, nil, StatePending, true},
		{SessionManualReview, nil, StatePending, true},
		{SessionRequiresInput, nil, StateNeedsInformation, true},
		{SessionApproved, passing(t0), StateVerified, true},
		{SessionApproved, nil, StateRestricted, true},
		{SessionApproved, withCheck(passing(t0)[:3], check(CheckSanctions, OutcomeFail, t0, false)), StateRestricted, true},
		{SessionDeclined, nil, StateRejected, true},
		{SessionCancelled, nil, StateRequired, true},
		{SessionExpired, nil, StateExpired, true},
	}
	for _, c := range cases {
		got, moved := profileStateFor(c.status, PurposePayoutKYC, c.checks)
		require.Equalf(t, c.moves, moved, "%s", c.status)
		assert.Equalf(t, c.want, got, "%s", c.status)
	}
}
