package delegation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

func unitPolicy(t tb) WithdrawalPolicy {
	t.Helper()
	p := WithdrawalPolicy{
		AccountID: accounts.AccountID(id.New[id.Any]()), Version: 1, AutoApproveEnabled: true,
		AutoApproveMax: money.USDFromMinor(10_000), AutoApproveMaxRiskScore: 20, MaxRiskScore: 80,
		PerWithdrawalLimit: money.USDFromMinor(50_000), DailyLimit: money.USDFromMinor(100_000), VelocityMaxCount: 10,
		MinDestinationAge: time.Hour, MinAccountAge: 0, CoolingPeriod: 0, StrongAuthRequired: true, StrongAuthMaxAge: 5 * time.Minute,
	}
	require.NoError(t, p.Validate())
	h, err := p.ComputeHash()
	require.NoError(t, err)
	p.PolicyHash = h
	return p
}

// lowRisk is an input every auto bound of unitPolicy accepts.
func lowRisk() EvaluationInput {
	return EvaluationInput{Environment: config.EnvTest, Now: unitNow, Amount: money.USDFromMinor(5_000), DestinationActive: true,
		DestinationAddedAt: unitNow.Add(-2 * time.Hour), AccountCreatedAt: unitNow.Add(-365 * 24 * time.Hour),
		IdentityVerified: true, SanctionsClear: true, AgentInScope: true, AgentWithinLimits: true}
}

func TestALowRiskProposalAutoApprovesOnlyInTestMode(t *testing.T) {
	p := unitPolicy(t)
	ev := Evaluate(p, lowRisk())
	assert.Equal(t, OutcomeAutoApprove, ev.Outcome, "%v", ev.ReasonCodes)
	assert.Equal(t, 0, ev.RiskScore)
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		in := lowRisk()
		in.Environment = env
		ev := Evaluate(p, in)
		assert.Equal(t, OutcomeHumanRequired, ev.Outcome, "%s is not test mode", env)
		assert.Contains(t, ev.ReasonCodes, ReasonHumanNotTestMode)
	}
}

func TestEveryRiskFactorSendsTheProposalToAPerson(t *testing.T) {
	p := unitPolicy(t)
	cases := map[string]struct {
		mutate func(*WithdrawalPolicy, *EvaluationInput)
		code   string
	}{
		"auto-approval disabled": {func(p *WithdrawalPolicy, _ *EvaluationInput) { p.AutoApproveEnabled = false }, ReasonHumanAutoApprovalDisabled},
		"amount above the auto limit": {func(_ *WithdrawalPolicy, in *EvaluationInput) {
			in.Amount = money.USDFromMinor(10_001)
		}, ReasonHumanAmountAboveAuto},
		"a destination younger than the minimum": {func(_ *WithdrawalPolicy, in *EvaluationInput) {
			in.DestinationAddedAt = unitNow.Add(-59 * time.Minute)
		}, ReasonHumanDestinationTooNew},
		"an account younger than the minimum": {func(p *WithdrawalPolicy, in *EvaluationInput) {
			p.MinAccountAge = 30 * 24 * time.Hour
			in.AccountCreatedAt = unitNow.Add(-time.Hour)
		}, ReasonHumanAccountTooNew},
		"an account created in the future reads as brand new": {func(p *WithdrawalPolicy, in *EvaluationInput) {
			p.MinAccountAge = time.Second
			in.AccountCreatedAt = unitNow.Add(time.Hour)
		}, ReasonHumanAccountTooNew},
		"an anomaly": {func(_ *WithdrawalPolicy, in *EvaluationInput) { in.Anomalies = []Anomaly{AnomalyRecentDecline} }, ReasonHumanAnomaly},
		"a risk score above the auto limit": {func(p *WithdrawalPolicy, in *EvaluationInput) {
			p.AutoApproveMaxRiskScore = 0
			in.CountLast24h = 6
		}, ReasonHumanRiskScoreAboveAuto},
	}
	for name, tc := range cases {
		pp, in := p, lowRisk()
		tc.mutate(&pp, &in)
		ev := Evaluate(pp, in)
		assert.Equal(t, OutcomeHumanRequired, ev.Outcome, name)
		assert.Contains(t, ev.ReasonCodes, tc.code, name)
	}
}

func TestEveryHardLimitRejects(t *testing.T) {
	p := unitPolicy(t)
	cases := map[string]struct {
		mutate func(*EvaluationInput)
		code   string
	}{
		"a destination not allowlisted": {func(in *EvaluationInput) { in.DestinationActive = false }, ReasonWithdrawalDestinationNotAllowlisted},
		"an unverified identity":        {func(in *EvaluationInput) { in.IdentityVerified = false }, ReasonWithdrawalComplianceNotClear},
		"a sanctions hit":               {func(in *EvaluationInput) { in.SanctionsClear = false }, ReasonWithdrawalComplianceNotClear},
		"over the per-withdrawal limit": {func(in *EvaluationInput) { in.Amount = money.USDFromMinor(50_001) }, ReasonWithdrawalPerWithdrawalLimit},
		"over the daily limit": {func(in *EvaluationInput) {
			in.DayTotal = money.USDFromMinor(95_001)
		}, ReasonWithdrawalDailyLimit},
		"over the velocity":         {func(in *EvaluationInput) { in.CountLast24h = 10 }, ReasonWithdrawalVelocity},
		"outside the agent's scope": {func(in *EvaluationInput) { in.AgentInScope = false }, ReasonWithdrawalAgentScope},
		"beyond the agent's limits": {func(in *EvaluationInput) { in.AgentWithinLimits = false }, ReasonWithdrawalAgentLimit},
		"a risk score above the max": {func(in *EvaluationInput) {
			in.Anomalies = []Anomaly{AnomalyRecentDecline, AnomalyAmountSpike, AnomalyUnresolvedEffects}
			in.DestinationAddedAt = unitNow
		}, ReasonWithdrawalRiskScore},
	}
	for name, tc := range cases {
		in := lowRisk()
		tc.mutate(&in)
		ev := Evaluate(p, in)
		assert.Equal(t, OutcomeReject, ev.Outcome, name)
		assert.Contains(t, ev.ReasonCodes, tc.code, name)
	}
}

func TestTheRiskScoreIsTheSumOfItsMeasuredFactors(t *testing.T) {
	p := unitPolicy(t)
	in := lowRisk()
	assert.Equal(t, 0, RiskScore(p, in))
	in.DestinationAddedAt = unitNow
	assert.Equal(t, weightYoungDestination, RiskScore(p, in))
	in.Amount = money.USDFromMinor(25_001) // more than half the per-withdrawal limit
	assert.Equal(t, weightYoungDestination+weightLargeAmount, RiskScore(p, in))
	in.DayTotal = money.USDFromMinor(30_000) // 55_001 > half the daily limit
	assert.Equal(t, weightYoungDestination+weightLargeAmount+weightDailyPressure, RiskScore(p, in))
	in.CountLast24h = 5 // 6 > half of 10
	assert.Equal(t, weightYoungDestination+weightLargeAmount+weightDailyPressure+weightVelocityPressure, RiskScore(p, in))
	in.Anomalies = []Anomaly{AnomalyAmountSpike, AnomalyRecentDecline}
	assert.Equal(t, 100, RiskScore(p, in), "capped at 100")
}

func TestWithdrawalPolicyValidation(t *testing.T) {
	p := unitPolicy(t)
	for name, mutate := range map[string]func(*WithdrawalPolicy){
		"daily below per-withdrawal": func(p *WithdrawalPolicy) { p.DailyLimit = money.USDFromMinor(1) },
		"auto above per-withdrawal":  func(p *WithdrawalPolicy) { p.AutoApproveMax = money.USDFromMinor(50_001) },
		"auto risk above max":        func(p *WithdrawalPolicy) { p.AutoApproveMaxRiskScore = 90 },
		"no velocity":                func(p *WithdrawalPolicy) { p.VelocityMaxCount = 0 },
		"fractional seconds":         func(p *WithdrawalPolicy) { p.CoolingPeriod = 1500 * time.Millisecond },
		"negative age":               func(p *WithdrawalPolicy) { p.MinDestinationAge = -time.Second },
		"strong auth too long":       func(p *WithdrawalPolicy) { p.StrongAuthMaxAge = 25 * time.Hour },
		"zero per-withdrawal":        func(p *WithdrawalPolicy) { p.PerWithdrawalLimit = money.USDFromMinor(0) },
	} {
		q := p
		mutate(&q)
		assert.Error(t, q.Validate(), name)
	}
	h, err := p.ComputeHash()
	require.NoError(t, err)
	q := p
	q.MinDestinationAge = 2 * time.Hour
	h2, err := q.ComputeHash()
	require.NoError(t, err)
	assert.NotEqual(t, h, h2)
}
