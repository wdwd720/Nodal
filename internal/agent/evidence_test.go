package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

func hash32(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func hexHash(s string) string { return hex.EncodeToString(hash32(s)) }

// fullEvidence builds evidence that satisfies the gate into stage.
func fullEvidence(stage Stage) PromotionEvidence {
	ev := PromotionEvidence{
		StrategyVersionID: NewAgentID().String(),
		IRHash:            hash32("ir"),
		DatasetRef:        "dataset://eval/2026-09",
		DatasetHash:       hash32("dataset"),
		RiskPolicyVersion: "risk/v1",
		RiskPolicyHash:    hash32("risk"),
		Reason:            "promotion after review",
	}
	for _, kind := range RequiredEvidence(stage) {
		ev.Evidence = append(ev.Evidence, EvidenceRef{Kind: kind, Ref: "ref://" + kind, Hash: hexHash(kind)})
	}
	if RequiresApproval(stage) {
		ev.ApprovalID = NewAgentID().String()
		ev.EnvelopeID = NewAgentID().String()
	}
	return ev
}

// TestEveryGateRefusesEachMissingEvidenceKind: a promotion is refused when any
// single required artifact is absent, and the error names it.
func TestEveryGateRefusesEachMissingEvidenceKind(t *testing.T) {
	t.Parallel()
	for _, stage := range []Stage{StageShadow, StageCanary, StageLimited, StageLive} {
		t.Run(stage.String(), func(t *testing.T) {
			t.Parallel()
			require.NoError(t, fullEvidence(stage).Validate(stage), "complete evidence must pass")
			for _, missing := range RequiredEvidence(stage) {
				ev := fullEvidence(stage)
				kept := ev.Evidence[:0]
				for _, ref := range ev.Evidence {
					if ref.Kind != missing {
						kept = append(kept, ref)
					}
				}
				ev.Evidence = kept
				err := ev.Validate(stage)
				require.Errorf(t, err, "%s without %s must be refused", stage, missing)
				assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
				e, ok := errs.As(err)
				require.True(t, ok)
				assert.Containsf(t, e.Fields["evidence"], missing, "the refusal must name %s", missing)
			}
		})
	}
}

func TestPromotionIntoRealCapitalRequiresAnApproval(t *testing.T) {
	t.Parallel()
	for _, stage := range []Stage{StageCanary, StageLimited, StageLive} {
		require.True(t, RequiresApproval(stage), "%s deploys capital", stage)
		ev := fullEvidence(stage)
		ev.ApprovalID = ""
		err := ev.Validate(stage)
		require.Errorf(t, err, "%s without an approval must be refused", stage)
		e, ok := errs.As(err)
		require.True(t, ok)
		assert.Contains(t, e.Fields["approval_id"], "dual-controlled")
	}
	// SHADOW is the last stage that needs no approval, but it still needs
	// hashed evidence.
	assert.False(t, RequiresApproval(StageShadow))
	assert.True(t, RequiresEvidence(StageShadow))
}

func TestPromotionIntoRealCapitalRequiresAnEnvelope(t *testing.T) {
	t.Parallel()
	for _, stage := range []Stage{StageCanary, StageLimited, StageLive} {
		ev := fullEvidence(stage)
		ev.EnvelopeID = ""
		err := ev.Validate(stage)
		require.Errorf(t, err, "%s without an envelope must be refused", stage)
	}
}

func TestPromotionRefusesUnhashedEvidence(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "not-hex", strings.Repeat("a", 63), strings.ToUpper(hexHash("x"))} {
		ev := fullEvidence(StageShadow)
		ev.Evidence[0].Hash = bad
		err := ev.Validate(StageShadow)
		require.Errorf(t, err, "hash %q must be refused: unhashed evidence is a claim, not evidence", bad)
	}
}

func TestPromotionRefusesMissingHashes(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*PromotionEvidence){
		"strategy version": func(e *PromotionEvidence) { e.StrategyVersionID = "" },
		"ir hash":          func(e *PromotionEvidence) { e.IRHash = nil },
		"short ir hash":    func(e *PromotionEvidence) { e.IRHash = []byte{1, 2, 3} },
		"risk version":     func(e *PromotionEvidence) { e.RiskPolicyVersion = "" },
		"risk hash":        func(e *PromotionEvidence) { e.RiskPolicyHash = nil },
		"reason":           func(e *PromotionEvidence) { e.Reason = "short" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev := fullEvidence(StageShadow)
			mutate(&ev)
			require.Error(t, ev.Validate(StageShadow))
		})
	}
}

func TestEvidenceHashCoversEveryReference(t *testing.T) {
	t.Parallel()
	base := fullEvidence(StageCanary)
	h := base.Hash()
	require.Len(t, h, sha256.Size)

	// Order does not change the hash: the set is what matters.
	shuffled := base
	shuffled.Evidence = append([]EvidenceRef(nil), base.Evidence...)
	for i, j := 0, len(shuffled.Evidence)-1; i < j; i, j = i+1, j-1 {
		shuffled.Evidence[i], shuffled.Evidence[j] = shuffled.Evidence[j], shuffled.Evidence[i]
	}
	assert.Equal(t, h, shuffled.Hash(), "the aggregate hash is order-independent")

	// Changing any artifact changes the hash: a transition row cannot later
	// be re-pointed at different evidence.
	for i := range base.Evidence {
		altered := base
		altered.Evidence = append([]EvidenceRef(nil), base.Evidence...)
		altered.Evidence[i].Hash = hexHash("tampered")
		assert.NotEqualf(t, h, altered.Hash(), "changing evidence[%d] must change the aggregate hash", i)
	}
	swapped := base
	swapped.IRHash = hash32("different ir")
	assert.NotEqual(t, h, swapped.Hash(), "the IR hash is part of the aggregate")
	approved := base
	approved.ApprovalID = NewAgentID().String()
	assert.NotEqual(t, h, approved.Hash(), "the approval id is part of the aggregate")
}

func TestGateEvidenceCoversThePartSeventyRequirements(t *testing.T) {
	t.Parallel()
	// SHADOW: compile, property tests, historical replay, effect set, security,
	// risk policy, data dependencies.
	shadow := RequiredEvidence(StageShadow)
	for _, want := range []string{
		EvidenceCompileSuccess, EvidenceFinancialPropertyTest, EvidenceBacktest,
		EvidenceEffectSet, EvidenceSecurityFindings, EvidenceRiskPolicy, EvidenceDataDependencies,
	} {
		assert.Containsf(t, shadow, want, "SHADOW gate must require %s", want)
	}
	// CANARY additionally: shadow performance, no policy violations, envelope,
	// budget report.
	canary := RequiredEvidence(StageCanary)
	for _, want := range []string{EvidencePerformanceSnapshot, EvidencePolicyViolations, EvidenceEnvelope, EvidenceToolBudgetReport} {
		assert.Containsf(t, canary, want, "CANARY gate must require %s", want)
	}
	// LIMITED: real execution evidence, reconciliation, no unknown or
	// unexpected submissions, resolved predictions.
	limited := RequiredEvidence(StageLimited)
	for _, want := range []string{
		EvidenceRealFills, EvidenceReconciliation, EvidenceSubmissionUnknown,
		EvidenceUnexpectedSubmissions, EvidencePredictionOutcomes,
	} {
		assert.Containsf(t, limited, want, "LIMITED gate must require %s", want)
	}
	// LIVE: stronger approval, the capability gate, calibration and an
	// operator attestation.
	live := RequiredEvidence(StageLive)
	for _, want := range []string{EvidenceCalibrationSnapshot, EvidenceLiveTradingGate, EvidenceOperatorAttestation} {
		assert.Containsf(t, live, want, "LIVE gate must require %s", want)
	}
	// No stage below SHADOW carries a gate: those transitions are compile and
	// owner acceptance, not capital deployment.
	for _, s := range []Stage{StageDraft, StageCompiled, StageValidated, StageBacktestEligible} {
		assert.Emptyf(t, RequiredEvidence(s), "%s is not a capital gate", s)
	}
}

// TestAgentPromoteApproveIsDualControlAndDistinct is the structural half of
// PART 70: the propose and approve permissions are different, and no standing
// role holds the approve side.
func TestAgentPromoteApproveIsDualControlAndDistinct(t *testing.T) {
	t.Parallel()
	require.NotEqual(t, security.PermAgentPromote, security.PermAgentPromoteApprove)
	assert.True(t, security.IsDualControl(security.PermAgentPromoteApprove))
	assert.False(t, security.IsDualControl(security.PermAgentPromote))
	for _, r := range security.AllRoles() {
		if r == security.RoleBreakGlass {
			continue
		}
		assert.Falsef(t, security.RoleGrants(r, security.PermAgentPromoteApprove),
			"standing role %s must not hold the approve side of a promotion", r)
	}
	// An agent holds neither.
	a := security.AgentPrincipal("agent-1", "acct-1")
	assert.False(t, a.Has(security.PermAgentPromote, a.AuthTime))
	assert.False(t, a.Has(security.PermAgentPromoteApprove, a.AuthTime))
	assert.False(t, a.Has(security.PermAgentPause, a.AuthTime), "an agent cannot pause or resume itself")
}
