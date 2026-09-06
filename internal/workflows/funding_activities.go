package workflows

import (
	"context"
	"log/slog"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
)

// DepositState is what a workflow is allowed to know about a deposit: its
// status, whether that status is terminal, and whether it may be withdrawn.
// Amounts are deliberately absent — a workflow never reasons about money
// (PART 116).
type DepositState struct {
	DepositID          string `json:"deposit_id"`
	Status             string `json:"status"`
	Terminal           bool   `json:"terminal"`
	WithdrawalEligible bool   `json:"withdrawal_eligible"`
}

// AdvanceDepositInput names the deposit to advance.
type AdvanceDepositInput struct {
	DepositID     string `json:"deposit_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// EscalateDepositInput describes a deposit that has not moved.
type EscalateDepositInput struct {
	DepositID     string        `json:"deposit_id"`
	Status        string        `json:"status"`
	StuckFor      time.Duration `json:"stuck_for"`
	CorrelationID string        `json:"correlation_id,omitempty"`
}

// FundingDriver is the funding side of the world as the workflow needs it.
// internal/funding's Driver and Repository satisfy it through a thin adapter
// in cmd/workflow-worker; this package never imports a provider or a database
// handle of its own.
//
// Advance must be idempotent: Temporal retries an activity whose result was
// lost in transit, so a second Advance for a deposit that already moved must
// be a no-op rather than a second step.
type FundingDriver interface {
	Advance(ctx context.Context, depositID string) error
	Describe(ctx context.Context, depositID string) (DepositState, error)
}

// OperatorAlerter delivers an operator-facing alert. It is a notification
// boundary, never a financial one.
type OperatorAlerter interface {
	Alert(ctx context.Context, a OperatorAlert) error
}

// OperatorAlert is one alert. Severity is a stable string so alert routing
// can key on it.
type OperatorAlert struct {
	Kind          string            `json:"kind"`
	Severity      string            `json:"severity"`
	Subject       string            `json:"subject"`
	ResourceType  string            `json:"resource_type"`
	ResourceID    string            `json:"resource_id"`
	Detail        string            `json:"detail"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"`
}

// Alert severities.
const (
	SeverityInfo     = "INFO"
	SeverityWarning  = "WARNING"
	SeverityCritical = "CRITICAL"
)

// FundingActivities are the funding workflow's I/O. Each method is a single
// idempotent step; none of them decides anything the database has not already
// decided.
type FundingActivities struct {
	driver FundingDriver
	alert  OperatorAlerter
	log    *slog.Logger
}

// NewFundingActivities wires the funding activities.
func NewFundingActivities(d FundingDriver, a OperatorAlerter, log *slog.Logger) (*FundingActivities, error) {
	if d == nil || a == nil {
		return nil, errs.New(errs.CodeInternal, "workflows: funding activities need a driver and an alerter")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &FundingActivities{driver: d, alert: a, log: log}, nil
}

// AdvanceDeposit performs the next lifecycle step and reports the resulting
// state. Advancing and describing are separate calls so the reported state is
// always read back from the database rather than inferred from the step.
func (a *FundingActivities) AdvanceDeposit(ctx context.Context, in AdvanceDepositInput) (DepositState, error) {
	if in.DepositID == "" {
		return DepositState{}, ActivityError(errs.New(errs.CodeValidationFailed, "workflows: deposit id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, in.CorrelationID)
	if err := a.driver.Advance(ctx, in.DepositID); err != nil {
		return DepositState{}, ActivityError(err)
	}
	state, err := a.driver.Describe(ctx, in.DepositID)
	if err != nil {
		return DepositState{}, ActivityError(err)
	}
	state.DepositID = in.DepositID
	return state, nil
}

// DescribeDeposit reports the deposit's current state without advancing it.
func (a *FundingActivities) DescribeDeposit(ctx context.Context, in AdvanceDepositInput) (DepositState, error) {
	if in.DepositID == "" {
		return DepositState{}, ActivityError(errs.New(errs.CodeValidationFailed, "workflows: deposit id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, in.CorrelationID)
	state, err := a.driver.Describe(ctx, in.DepositID)
	if err != nil {
		return DepositState{}, ActivityError(err)
	}
	state.DepositID = in.DepositID
	return state, nil
}

// EscalateDeposit tells an operator that a deposit has not moved. It changes
// no financial state; it is a message.
func (a *FundingActivities) EscalateDeposit(ctx context.Context, in EscalateDepositInput) error {
	if in.DepositID == "" {
		return ActivityError(errs.New(errs.CodeValidationFailed, "workflows: deposit id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, in.CorrelationID)
	a.log.WarnContext(ctx, "workflows: deposit has not progressed",
		"deposit_id", in.DepositID, "status", in.Status, "stuck_for", in.StuckFor.String())
	return ActivityError(a.alert.Alert(ctx, OperatorAlert{
		Kind:          "FUNDING_DEPOSIT_STALLED",
		Severity:      SeverityWarning,
		Subject:       "deposit has not progressed",
		ResourceType:  "deposit",
		ResourceID:    in.DepositID,
		Detail:        "the deposit has been in " + in.Status + " for " + in.StuckFor.String(),
		CorrelationID: in.CorrelationID,
		Fields:        map[string]string{"status": in.Status, "stuck_for": in.StuckFor.String()},
	}))
}
