package delegation

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// SigningPolicy is the platform's policy over what the signing provider may
// sign, applied after the inspector has proved the transaction is the plan.
type SigningPolicy struct {
	Version string
	// MaxFeePerTx bounds the network fee (native base units) a delegated
	// wallet may pay for one transaction.
	MaxFeePerTx uint64
	// MaxSlippageBPS bounds the slippage any swap may accept.
	MaxSlippageBPS money.BPS
	// AllowedChains are the chains the provider signs for at all.
	AllowedChains []devnet.ChainID
}

// DefaultSigningPolicy is the sandbox policy.
func DefaultSigningPolicy() SigningPolicy {
	return SigningPolicy{Version: "delegated-signing-v1", MaxFeePerTx: 100_000, MaxSlippageBPS: 300, AllowedChains: Chains()}
}

// Validate checks the policy.
func (p SigningPolicy) Validate() error {
	if p.Version == "" || p.MaxSlippageBPS < 0 || p.MaxSlippageBPS > money.OneHundredPercent || len(p.AllowedChains) == 0 {
		return errs.New(errs.CodeValidationFailed, "delegation: the signing policy needs a version, a slippage bound and chains")
	}
	for _, c := range p.AllowedChains {
		if !contains(Chains(), c) {
			return errs.New(errs.CodeValidationFailed, "delegation: the signing policy names an unknown chain "+string(c))
		}
	}
	return nil
}

// Hash is the canonical hash of the policy, recorded with every decision.
func (p SigningPolicy) Hash() string {
	chains := strs(sortedUnique(p.AllowedChains))
	b, err := audit.CanonicalJSON(map[string]any{
		"version": p.Version, "max_fee_per_tx": strconv.FormatUint(p.MaxFeePerTx, 10),
		"max_slippage_bps": strconv.FormatInt(int64(p.MaxSlippageBPS), 10), "allowed_chains": chains,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// WithdrawalPolicy is one version of an account's withdrawal policy
// (delegated_withdrawal_policies). It is the customer's: written by the
// account's owner, never by an agent.
type WithdrawalPolicy struct {
	ID                      PolicyID
	AccountID               accounts.AccountID
	Version                 int
	AutoApproveEnabled      bool
	AutoApproveMax          money.USD
	AutoApproveMaxRiskScore int
	MaxRiskScore            int
	PerWithdrawalLimit      money.USD
	DailyLimit              money.USD
	VelocityMaxCount        int
	MinDestinationAge       time.Duration
	MinAccountAge           time.Duration
	CoolingPeriod           time.Duration
	StrongAuthRequired      bool
	StrongAuthMaxAge        time.Duration
	PolicyHash              [32]byte
	CreatedBy               Actor
	EffectiveAt             time.Time
	CreatedAt               time.Time
}

// Bounds on policy durations, mirroring the table's CHECKs.
const (
	maxAgeBound       = 365 * 24 * time.Hour
	maxCoolingBound   = 7 * 24 * time.Hour
	maxStrongAuthAge  = 24 * time.Hour
	maxVelocityCount  = 1000
	maxRiskScoreValue = 100
)

// Validate checks the policy's terms.
func (p WithdrawalPolicy) Validate() error {
	var problems []string
	if p.AccountID.IsZero() {
		problems = append(problems, "account_id required")
	}
	if !p.PerWithdrawalLimit.IsPositive() || p.DailyLimit.Cmp(p.PerWithdrawalLimit) < 0 {
		problems = append(problems, "limits must satisfy 0 < per-withdrawal limit <= daily limit")
	}
	if p.AutoApproveMax.IsNegative() || p.AutoApproveMax.Cmp(p.PerWithdrawalLimit) > 0 {
		problems = append(problems, "the auto-approval maximum must lie in [0, per-withdrawal limit]")
	}
	if p.MaxRiskScore < 0 || p.MaxRiskScore > maxRiskScoreValue || p.AutoApproveMaxRiskScore < 0 || p.AutoApproveMaxRiskScore > p.MaxRiskScore {
		problems = append(problems, "risk scores must satisfy 0 <= auto maximum <= maximum <= 100")
	}
	if p.VelocityMaxCount < 1 || p.VelocityMaxCount > maxVelocityCount {
		problems = append(problems, "velocity must be 1..1000 withdrawals per 24 hours")
	}
	for name, d := range map[string]time.Duration{"minimum destination age": p.MinDestinationAge, "minimum account age": p.MinAccountAge} {
		if d < 0 || d > maxAgeBound || d%time.Second != 0 {
			problems = append(problems, name+" must be whole seconds in [0, 365 days]")
		}
	}
	if p.CoolingPeriod < 0 || p.CoolingPeriod > maxCoolingBound || p.CoolingPeriod%time.Second != 0 {
		problems = append(problems, "cooling period must be whole seconds in [0, 7 days]")
	}
	if p.StrongAuthMaxAge < time.Second || p.StrongAuthMaxAge > maxStrongAuthAge || p.StrongAuthMaxAge%time.Second != 0 {
		problems = append(problems, "strong authentication age must be whole seconds in [1s, 24h]")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errs.New(errs.CodeValidationFailed, "delegation: invalid withdrawal policy").WithField("problems", problems)
	}
	return nil
}

// ComputeHash returns the canonical hash of the policy's terms.
func (p WithdrawalPolicy) ComputeHash() ([32]byte, error) {
	b, err := audit.CanonicalJSON(map[string]any{
		"version": "nodal-sandbox/withdrawal-policy/v1", "account_id": p.AccountID.String(),
		"policy_version":              strconv.Itoa(p.Version),
		"auto_approve_enabled":        p.AutoApproveEnabled,
		"auto_approve_max_minor":      strconv.FormatInt(p.AutoApproveMax.Minor(), 10),
		"auto_approve_max_risk_score": strconv.Itoa(p.AutoApproveMaxRiskScore),
		"max_risk_score":              strconv.Itoa(p.MaxRiskScore),
		"per_withdrawal_limit_minor":  strconv.FormatInt(p.PerWithdrawalLimit.Minor(), 10),
		"daily_limit_minor":           strconv.FormatInt(p.DailyLimit.Minor(), 10),
		"velocity_max_count":          strconv.Itoa(p.VelocityMaxCount),
		"min_destination_age_seconds": strconv.FormatInt(int64(p.MinDestinationAge/time.Second), 10),
		"min_account_age_seconds":     strconv.FormatInt(int64(p.MinAccountAge/time.Second), 10),
		"cooling_period_seconds":      strconv.FormatInt(int64(p.CoolingPeriod/time.Second), 10),
		"strong_auth_required":        p.StrongAuthRequired,
		"strong_auth_max_age_seconds": strconv.FormatInt(int64(p.StrongAuthMaxAge/time.Second), 10),
	})
	if err != nil {
		return [32]byte{}, errs.Wrap(err, errs.CodeInternal, "delegation: policy is not canonically encodable")
	}
	return sha256.Sum256(b), nil
}

// Anomaly names something measured about an account that makes a
// withdrawal unusual enough to need a person.
type Anomaly string

// Anomalies. Each is measured from persisted rows; none is a guess.
const (
	// AnomalyUnresolvedEffects: the account has delegated effects whose
	// outcome is UNKNOWN.
	AnomalyUnresolvedEffects Anomaly = "UNRESOLVED_DELEGATED_EFFECTS"
	// AnomalyRecentDecline: the account's owner declined a delegated
	// withdrawal in the last 24 hours.
	AnomalyRecentDecline Anomaly = "RECENT_DECLINE"
	// AnomalyAmountSpike: the amount is more than ten times the largest
	// withdrawal the account settled in the last 30 days.
	AnomalyAmountSpike Anomaly = "AMOUNT_SPIKE"
)

// EvaluationInput is every measured fact the withdrawal policy decides on.
type EvaluationInput struct {
	Environment        config.Environment
	Now                time.Time
	Amount             money.USD
	DestinationActive  bool
	DestinationAddedAt time.Time
	AccountCreatedAt   time.Time
	IdentityVerified   bool
	SanctionsClear     bool
	DayTotal           money.USD
	CountLast24h       int
	Anomalies          []Anomaly
	AgentInScope       bool
	AgentWithinLimits  bool
}

// Evaluation is the policy's decision about one proposal.
type Evaluation struct {
	Outcome         EvaluationOutcome `json:"outcome"`
	RiskScore       int               `json:"risk_score"`
	ReasonCodes     []string          `json:"reason_codes"`
	Anomalies       []string          `json:"anomalies"`
	DestinationAge  string            `json:"destination_age_seconds"`
	AccountAge      string            `json:"account_age_seconds"`
	DayTotalBefore  string            `json:"day_total_before"`
	CountLast24h    string            `json:"count_last_24h"`
	PolicyVersion   string            `json:"policy_version"`
	PolicyHash      string            `json:"policy_hash"`
	Environment     string            `json:"environment"`
	ComplianceClear bool              `json:"compliance_clear"`
}

// Withdrawal policy reason codes.
const (
	ReasonWithdrawalDestinationNotAllowlisted = "WITHDRAWAL_DESTINATION_NOT_ALLOWLISTED"
	ReasonWithdrawalComplianceNotClear        = "WITHDRAWAL_COMPLIANCE_NOT_CLEAR"
	ReasonWithdrawalPerWithdrawalLimit        = "WITHDRAWAL_PER_WITHDRAWAL_LIMIT"
	ReasonWithdrawalDailyLimit                = "WITHDRAWAL_DAILY_LIMIT"
	ReasonWithdrawalVelocity                  = "WITHDRAWAL_VELOCITY"
	ReasonWithdrawalAgentScope                = "WITHDRAWAL_AGENT_SCOPE"
	ReasonWithdrawalAgentLimit                = "WITHDRAWAL_AGENT_LIMIT"
	ReasonWithdrawalRiskScore                 = "WITHDRAWAL_RISK_SCORE"
	ReasonHumanNotTestMode                    = "HUMAN_NOT_TEST_MODE"
	ReasonHumanAutoApprovalDisabled           = "HUMAN_AUTO_APPROVAL_DISABLED"
	ReasonHumanAmountAboveAuto                = "HUMAN_AMOUNT_ABOVE_AUTO_LIMIT"
	ReasonHumanDestinationTooNew              = "HUMAN_DESTINATION_TOO_NEW"
	ReasonHumanAccountTooNew                  = "HUMAN_ACCOUNT_TOO_NEW"
	ReasonHumanRiskScoreAboveAuto             = "HUMAN_RISK_SCORE_ABOVE_AUTO"
	ReasonHumanAnomaly                        = "HUMAN_ANOMALY"
)

// Risk score weights. The score is an integer 0..100: the sum of the
// weights of every measured risk factor present, capped at 100.
const (
	weightYoungDestination = 30
	weightYoungAccount     = 20
	weightLargeAmount      = 15
	weightDailyPressure    = 10
	weightVelocityPressure = 10
	weightAnomaly          = 25
)

// RiskScore is the deterministic risk score of a proposal under a policy.
func RiskScore(p WithdrawalPolicy, in EvaluationInput) int {
	score := 0
	if age(in.Now, in.DestinationAddedAt) < p.MinDestinationAge {
		score += weightYoungDestination
	}
	if age(in.Now, in.AccountCreatedAt) < p.MinAccountAge {
		score += weightYoungAccount
	}
	// amount > 50% of the per-withdrawal limit  <=>  2 x amount > limit
	if twice(in.Amount).Cmp(p.PerWithdrawalLimit) > 0 {
		score += weightLargeAmount
	}
	if after, err := in.DayTotal.Add(in.Amount); err != nil || twice(after).Cmp(p.DailyLimit) > 0 {
		score += weightDailyPressure
	}
	if 2*(in.CountLast24h+1) > p.VelocityMaxCount {
		score += weightVelocityPressure
	}
	score += weightAnomaly * len(in.Anomalies)
	if score > maxRiskScoreValue {
		score = maxRiskScoreValue
	}
	return score
}

// Evaluate decides a proposal: REJECT on any hard limit, AUTO_APPROVE only in
// low-risk test mode with every auto bound met, HUMAN_REQUIRED otherwise. It
// is pure: the same policy and input always give the same evaluation.
func Evaluate(p WithdrawalPolicy, in EvaluationInput) Evaluation {
	score := RiskScore(p, in)
	anomalies := make([]string, 0, len(in.Anomalies))
	for _, a := range in.Anomalies {
		anomalies = append(anomalies, string(a))
	}
	sort.Strings(anomalies)
	ev := Evaluation{
		RiskScore: score, Anomalies: anomalies,
		DestinationAge: strconv.FormatInt(int64(age(in.Now, in.DestinationAddedAt)/time.Second), 10),
		AccountAge:     strconv.FormatInt(int64(age(in.Now, in.AccountCreatedAt)/time.Second), 10),
		DayTotalBefore: strconv.FormatInt(in.DayTotal.Minor(), 10), CountLast24h: strconv.Itoa(in.CountLast24h),
		PolicyVersion: strconv.Itoa(p.Version), PolicyHash: hex.EncodeToString(p.PolicyHash[:]),
		Environment: string(in.Environment), ComplianceClear: in.IdentityVerified && in.SanctionsClear,
	}
	var hard []string
	if !in.DestinationActive {
		hard = append(hard, ReasonWithdrawalDestinationNotAllowlisted)
	}
	if !ev.ComplianceClear {
		hard = append(hard, ReasonWithdrawalComplianceNotClear)
	}
	if in.Amount.Cmp(p.PerWithdrawalLimit) > 0 {
		hard = append(hard, ReasonWithdrawalPerWithdrawalLimit)
	}
	if after, err := in.DayTotal.Add(in.Amount); err != nil || after.Cmp(p.DailyLimit) > 0 {
		hard = append(hard, ReasonWithdrawalDailyLimit)
	}
	if in.CountLast24h+1 > p.VelocityMaxCount {
		hard = append(hard, ReasonWithdrawalVelocity)
	}
	if !in.AgentInScope {
		hard = append(hard, ReasonWithdrawalAgentScope)
	}
	if !in.AgentWithinLimits {
		hard = append(hard, ReasonWithdrawalAgentLimit)
	}
	if score > p.MaxRiskScore {
		hard = append(hard, ReasonWithdrawalRiskScore)
	}
	if len(hard) > 0 {
		sort.Strings(hard)
		ev.Outcome, ev.ReasonCodes = OutcomeReject, hard
		return ev
	}
	var human []string
	if !contains(TestEnvironments(), in.Environment) {
		human = append(human, ReasonHumanNotTestMode)
	}
	if !p.AutoApproveEnabled {
		human = append(human, ReasonHumanAutoApprovalDisabled)
	}
	if in.Amount.Cmp(p.AutoApproveMax) > 0 {
		human = append(human, ReasonHumanAmountAboveAuto)
	}
	if age(in.Now, in.DestinationAddedAt) < p.MinDestinationAge {
		human = append(human, ReasonHumanDestinationTooNew)
	}
	if age(in.Now, in.AccountCreatedAt) < p.MinAccountAge {
		human = append(human, ReasonHumanAccountTooNew)
	}
	if score > p.AutoApproveMaxRiskScore {
		human = append(human, ReasonHumanRiskScoreAboveAuto)
	}
	if len(in.Anomalies) > 0 {
		human = append(human, ReasonHumanAnomaly)
	}
	sort.Strings(human)
	if len(human) > 0 {
		ev.Outcome, ev.ReasonCodes = OutcomeHumanRequired, human
		return ev
	}
	ev.Outcome, ev.ReasonCodes = OutcomeAutoApprove, []string{}
	return ev
}

// age is now - since, never negative: a timestamp after now (clock skew
// between the service and the database) reads as brand new, the most
// conservative answer.
func age(now, since time.Time) time.Duration {
	d := now.Sub(since)
	if d < 0 {
		return 0
	}
	return d
}

func twice(u money.USD) money.USD {
	t, err := u.Add(u)
	if err != nil {
		return money.MaxUSD()
	}
	return t
}
