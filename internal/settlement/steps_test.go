package settlement

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/provider"
)

func TestV1Steps_CanonicalSequence(t *testing.T) {
	t.Parallel()
	want := []StepType{
		"VALIDATE_ELIGIBILITY", "EVALUATE_RISK", "RESERVE_CAPITAL", "RESOLVE_VENUE_LISTING", "LOCATE_SETTLEMENT_ASSET",
		"ACQUIRE_QUOTE", "VALIDATE_QUOTE", "FINAL_RISK_CHECK", "BUILD_TRANSACTION", "INSPECT_TRANSACTION",
		"REQUEST_SIGNATURE", "SUBMIT", "OBSERVE_FINALITY", "RECONCILE", "POST_LEDGER", "UPDATE_POSITION", "RELEASE_RESERVATION",
	}
	require.Equal(t, want, V1StepSequence())
	for _, s := range want {
		require.True(t, s.Valid())
		require.False(t, s.Reserved())
	}
	require.Equal(t, []StepType{"CONVERT", "TRANSFER", "WAIT_FINALITY", "TRADE"}, ReservedStepTypes())
	for _, s := range ReservedStepTypes() {
		require.True(t, s.Valid())
		require.True(t, s.Reserved())
	}
	require.False(t, StepType("BRIDGE").Valid())
}

func TestPlanner_NeverEmitsReservedSteps(t *testing.T) {
	t.Parallel()
	planner := NewPlanner(DefaultOptions())
	plan, err := planner.Plan(goldenBase())
	require.NoError(t, err)
	require.Len(t, plan.Steps, 17)
	for i, s := range plan.Steps {
		require.False(t, s.Type.Reserved(), "reserved step %s emitted", s.Type)
		require.Equal(t, V1StepSequence()[i], s.Type)
		require.Equal(t, int32(i), s.Seq)
		require.Equal(t, SemanticKey(plan.ID, s.Seq, s.Type), s.SemanticIdempotencyKey)
		require.Equal(t, StepRetryClass(s.Type), s.RetryClass)
		require.Equal(t, StepCompensation(s.Type), s.CompensationPolicy)
		require.Positive(t, s.Timeout)
		require.Equal(t, StepPending, s.State)
		require.NotEmpty(t, s.EvidenceInputs)
		if i == 0 {
			require.Empty(t, s.DependsOn)
		} else {
			require.Equal(t, []StepID{plan.Steps[i-1].ID}, s.DependsOn, "each step depends on its predecessor")
		}
	}
	sub, _ := plan.StepByType(StepSubmit)
	require.Equal(t, provider.UnknownEffectWrite, sub.RetryClass)
	obs, _ := plan.StepByType(StepObserveFinality)
	require.Equal(t, string(execution.FinalityConfirmed), obs.FinalityPolicy)
	pl, _ := plan.StepByType(StepPostLedger)
	require.Equal(t, string(execution.FinalityConfirmed), pl.FinalityPolicy)
	rel, _ := plan.StepByType(StepReleaseReservation)
	require.Equal(t, CompensationNone, rel.CompensationPolicy)
}

func TestStepRetryClasses(t *testing.T) {
	t.Parallel()
	require.Equal(t, provider.UnknownEffectWrite, StepRetryClass(StepSubmit))
	for _, s := range []StepType{StepReserveCapital, StepRequestSignature, StepPostLedger, StepUpdatePosition, StepReleaseReservation} {
		require.Equal(t, provider.IdempotentWrite, StepRetryClass(s), "%s", s)
	}
	for _, s := range []StepType{StepValidateEligibility, StepEvaluateRisk, StepResolveVenueListing, StepLocateSettlementAsset, StepAcquireQuote, StepValidateQuote, StepFinalRiskCheck, StepBuildTransaction, StepInspectTransaction, StepObserveFinality, StepReconcile} {
		require.Equal(t, provider.SafeRetry, StepRetryClass(s), "%s", s)
	}
}

func TestStepKillSwitchClasses(t *testing.T) {
	t.Parallel()
	for _, planClass := range []killswitch.ActionClass{killswitch.NewRisk, killswitch.ReduceRisk} {
		for _, s := range V1StepSequence() {
			class, guarded := s.KillSwitchClass(planClass)
			switch s {
			case StepReserveCapital, StepSubmit:
				require.True(t, guarded)
				require.Equal(t, planClass, class)
			case StepObserveFinality, StepReconcile, StepPostLedger, StepUpdatePosition, StepReleaseReservation:
				require.True(t, guarded)
				require.True(t, class.NeverBlocked(), "%s must carry a never-blocked class", s)
				require.True(t, s.PostSubmission())
			default:
				require.False(t, guarded, "%s", s)
				require.False(t, s.PostSubmission())
			}
		}
	}
	require.True(t, StepInspectTransaction.DryRunStop())
	require.False(t, StepRequestSignature.DryRunStop())
}

func TestPlanValidate_RejectsBrokenDAGs(t *testing.T) {
	t.Parallel()
	planner := NewPlanner(DefaultOptions())
	good, err := planner.Plan(goldenBase())
	require.NoError(t, err)
	require.NoError(t, good.Validate())

	missing := good
	missing.Steps = append([]Step(nil), good.Steps[:16]...)
	require.Error(t, missing.Validate(), "a plan missing a mandatory step is rejected")

	reserved := good
	reserved.Steps = append([]Step(nil), good.Steps...)
	reserved.Steps[5].Type = StepConvert
	require.Error(t, reserved.Validate(), "reserved step types are rejected")

	swapped := good
	swapped.Steps = append([]Step(nil), good.Steps...)
	swapped.Steps[0], swapped.Steps[1] = swapped.Steps[1], swapped.Steps[0]
	require.Error(t, swapped.Validate(), "order matters")

	cyclic := good
	cyclic.Steps = append([]Step(nil), good.Steps...)
	cyclic.Steps[3].DependsOn = []StepID{good.Steps[10].ID}
	require.Error(t, cyclic.Validate(), "a dependency on a later step is a cycle")

	orphan := good
	orphan.Steps = append([]Step(nil), good.Steps...)
	orphan.Steps[4].DependsOn = nil
	require.Error(t, orphan.Validate())

	badRetry := good
	badRetry.Steps = append([]Step(nil), good.Steps...)
	badRetry.Steps[11].RetryClass = provider.SafeRetry
	require.Error(t, badRetry.Validate(), "SUBMIT must be UNKNOWN_EFFECT_WRITE")

	zeroTimeout := good
	zeroTimeout.Steps = append([]Step(nil), good.Steps...)
	zeroTimeout.Steps[2].Timeout = 0
	require.Error(t, zeroTimeout.Validate())

	dupKey := good
	dupKey.Steps = append([]Step(nil), good.Steps...)
	dupKey.Steps[2].SemanticIdempotencyKey = dupKey.Steps[1].SemanticIdempotencyKey
	require.Error(t, dupKey.Validate())

	nvp := Plan{Status: PlanNoValidPlan}
	require.Error(t, nvp.Validate(), "NO_VALID_PLAN needs reasons")
	nvp.NoPlanReasonCodes = []string{ReasonRiskRejected}
	require.NoError(t, nvp.Validate())
}

func TestPlanHash_ExcludesIdentifiersAndRuntimeFields(t *testing.T) {
	t.Parallel()
	planner := NewPlanner(DefaultOptions())
	a, err := planner.Plan(goldenBase())
	require.NoError(t, err)
	b, err := planner.Plan(goldenBase())
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID)
	require.Equal(t, a.Hash, b.Hash, "identical content, different rows, same hash")

	c := a
	c.Status = PlanApproved
	now := time.Now()
	c.ApprovedAt = &now
	c.Steps = append([]Step(nil), a.Steps...)
	c.Steps[0].State = StepSucceeded
	c.Steps[0].EvidenceOutput = []byte(`{"x":1}`)
	c.Steps[0].Attempts = 3
	h, err := ComputeHash(c)
	require.NoError(t, err)
	require.Equal(t, a.Hash, h, "status, timestamps and step runtime fields are outside the hash")

	d := a
	d.HardConstraints.MaxSlippageBPS++
	h, err = ComputeHash(d)
	require.NoError(t, err)
	require.False(t, bytes.Equal(a.Hash, h), "content changes the hash")

	e := a
	e.DryRun = !a.DryRun
	h, err = ComputeHash(e)
	require.NoError(t, err)
	require.False(t, bytes.Equal(a.Hash, h), "dry_run is part of the hash")

	ok, err := VerifyHash(d)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestNoValidPlanError(t *testing.T) {
	t.Parallel()
	err := NoValidPlanError([]string{ReasonRiskRejected, ReasonDeadlineImpossible}, map[string][]string{ReasonRiskRejected: {"x"}})
	require.True(t, errs.HasCode(err, errs.CodeNoValidPlan))
	require.Equal(t, []string{ReasonDeadlineImpossible, ReasonRiskRejected}, NoValidPlanReasons(err))
	require.Nil(t, NoValidPlanReasons(errs.New(errs.CodeInternal, "x")))
	require.Nil(t, NoValidPlanReasons(nil))
}

func TestPlanner_ValidationFailures(t *testing.T) {
	t.Parallel()
	planner := NewPlanner(DefaultOptions())
	in := goldenBase()
	in.Now = time.Time{}
	_, err := planner.Plan(in)
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed))
	in = goldenBase()
	in.Intent.Mode = "SANDBOX"
	_, err = planner.Plan(in)
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed))
	in = goldenBase()
	in.Account.WalletID = ""
	_, err = planner.Plan(in)
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed))
}

func TestFeePolicyFromRef_RoundTrip(t *testing.T) {
	t.Parallel()
	planner := NewPlanner(DefaultOptions())
	plan, err := planner.Plan(goldenBase())
	require.NoError(t, err)
	p, err := FeePolicyFromRef(plan.EstimatedCosts.FeePolicy)
	require.NoError(t, err)
	require.Equal(t, goldenBase().FeePolicy.Version, p.Version)
	require.Equal(t, goldenBase().FeePolicy.PlatformFeeBPS, p.PlatformFeeBPS)
	require.Equal(t, goldenBase().FeePolicy.Hash(), p.Hash())
}
