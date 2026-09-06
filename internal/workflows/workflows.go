package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/nodal/controlplane/internal/errs"
)

// Registered workflow names. They are stable public identifiers: a running
// workflow is looked up by name at replay time, so renaming one strands every
// execution already in flight.
const (
	FundingWorkflowName     = "controlplane.funding.deposit.v1"
	EscalationWorkflowName  = "controlplane.reconciliation.escalation.v1"
	AdvanceDepositName      = "controlplane.funding.AdvanceDeposit"
	DescribeDepositName     = "controlplane.funding.DescribeDeposit"
	EscalateDepositName     = "controlplane.funding.EscalateDeposit"
	DescribeRecordName      = "controlplane.reconciliation.DescribeRecord"
	MarkInvestigatingName   = "controlplane.reconciliation.MarkInvestigating"
	MarkEscalatedName       = "controlplane.reconciliation.MarkEscalated"
	NotifyEscalationName    = "controlplane.reconciliation.NotifyEscalation"
	RequestContainmentName  = "controlplane.reconciliation.RequestContainment"
	DefaultTaskQueueSuffix  = "control-plane"
	depositUpdatedSignal    = "deposit.updated"
	recordResolvedSignal    = "reconciliation.resolved"
	maxStepsPerWorkflowTask = 500
)

// DepositUpdatedSignal is the signal name a provider webhook handler sends to
// wake a funding workflow before its next poll.
const DepositUpdatedSignal = depositUpdatedSignal

// RecordResolvedSignal is the signal name an operator action sends to tell an
// escalation workflow that the record was resolved.
const RecordResolvedSignal = recordResolvedSignal

// TaskQueue returns the task queue for a configured prefix. An empty prefix
// takes the default, so a misconfigured worker still shares a queue with its
// starters rather than silently listening on "".
func TaskQueue(prefix string) string {
	if prefix == "" {
		return DefaultTaskQueueSuffix
	}
	return prefix + "-" + DefaultTaskQueueSuffix
}

// FundingWorkflowID is the deterministic workflow id of a deposit. Temporal
// rejects a second running execution with the same id, so a duplicate start
// request — a redelivered webhook, an operator retrying — cannot produce two
// workflows driving one deposit.
func FundingWorkflowID(depositID string) string { return "funding-deposit:" + depositID }

// EscalationWorkflowID is the deterministic workflow id of a reconciliation
// record's escalation.
func EscalationWorkflowID(recordID string) string { return "reconciliation-escalation:" + recordID }

// Activity timing. These are workflow-visible constants: changing one changes
// the commands a workflow emits, so they are versioned with the workflow name
// rather than read from configuration.
const (
	// activityTimeout bounds one activity attempt.
	activityTimeout = 2 * time.Minute
	// activityScheduleToClose bounds all attempts of one activity together.
	activityScheduleToClose = 30 * time.Minute
	// retryInitial is the first retry delay for a failed activity.
	retryInitial = 5 * time.Second
	// retryMax caps the retry delay.
	retryMax = 5 * time.Minute
	// retryCoefficient is the backoff multiplier.
	retryCoefficient = 2.0
)

// nonRetryableCodes are the errs.Code values that mean "retrying will give
// the same answer". Temporal must not retry an activity that fails for one of
// them; everything else — a lost connection, a provider blip — is retried.
func nonRetryableCodes() []string {
	return []string{
		string(errs.CodeValidationFailed),
		string(errs.CodeNotFound),
		string(errs.CodeForbidden),
		string(errs.CodeUnauthenticated),
		string(errs.CodeInvalidStateTransition),
		string(errs.CodeUnsupported),
	}
}

// activityCtx returns a workflow context carrying the standard activity
// options. It is pure: it reads no clock and no configuration.
func activityCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    activityTimeout,
		ScheduleToCloseTimeout: activityScheduleToClose,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        retryInitial,
			BackoffCoefficient:     retryCoefficient,
			MaximumInterval:        retryMax,
			NonRetryableErrorTypes: nonRetryableCodes(),
		},
	})
}

// ActivityError converts a domain error into the Temporal error the retry
// policy understands: the errs.Code becomes the error "type", so
// NonRetryableErrorTypes can name it. A nil error stays nil.
//
// The detail is the client-safe detail of an *errs.Error and never the
// wrapped cause, so nothing sensitive reaches the workflow history — which is
// stored outside Postgres and is visible in the Temporal UI.
func ActivityError(err error) error {
	if err == nil {
		return nil
	}
	e, ok := errs.As(err)
	if !ok {
		return err
	}
	detail := e.Detail
	if detail == "" {
		detail = string(e.Code)
	}
	for _, code := range nonRetryableCodes() {
		if string(e.Code) == code {
			return temporal.NewNonRetryableApplicationError(detail, string(e.Code), nil)
		}
	}
	return temporal.NewApplicationError(detail, string(e.Code))
}
