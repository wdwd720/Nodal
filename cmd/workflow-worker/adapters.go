package main

import (
	"context"
	"log/slog"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/workflows"
)

// fundingDriver adapts internal/funding's Lifecycle and Repository to the
// narrow FundingDriver interface internal/workflows declares. Both halves are
// deliberately separate calls: the workflow is told what the database says
// after the step, never what the step intended.
type fundingDriver struct {
	lifecycle funding.Lifecycle
	reader    depositReader
}

// depositReader is the read side, kept as an interface so the adapter can be
// tested without a database.
type depositReader interface {
	Get(ctx context.Context, depositID funding.DepositID) (funding.Deposit, error)
}

var _ workflows.FundingDriver = (*fundingDriver)(nil)

// Advance performs the next lifecycle step. funding.Driver.Advance is
// idempotent by construction — each step runs in its own transaction and is a
// no-op when the deposit has already moved — which is what makes a Temporal
// activity retry safe here.
func (d *fundingDriver) Advance(ctx context.Context, depositID string) error {
	id, err := funding.ParseDepositID(depositID)
	if err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "workflow-worker: deposit id")
	}
	return d.lifecycle.Advance(ctx, id)
}

// Describe reports the deposit's status without amounts. A workflow never
// reasons about money (PART 116), so none is handed to it.
func (d *fundingDriver) Describe(ctx context.Context, depositID string) (workflows.DepositState, error) {
	id, err := funding.ParseDepositID(depositID)
	if err != nil {
		return workflows.DepositState{}, errs.Wrap(err, errs.CodeValidationFailed, "workflow-worker: deposit id")
	}
	dep, err := d.reader.Get(ctx, id)
	if err != nil {
		return workflows.DepositState{}, err
	}
	return workflows.DepositState{
		DepositID:          depositID,
		Status:             string(dep.Status),
		Terminal:           dep.Status.Final() || dep.Status == funding.StatusAvailable,
		WithdrawalEligible: dep.WithdrawalEligible,
	}, nil
}

// newFundingDriver builds the adapter over a lifecycle driver and a deposit
// reader.
func newFundingDriver(l funding.Lifecycle, r depositReader) (workflows.FundingDriver, error) {
	if l == nil || r == nil {
		return nil, errs.New(errs.CodeInternal, "workflow-worker: funding driver needs a lifecycle and a reader")
	}
	return &fundingDriver{lifecycle: l, reader: r}, nil
}

// bindFundingDriver supplies the funding lifecycle. It is a seam, not
// indirection for its own sake: internal/funding.NewService needs a
// FundingProvider and a ChainReceiptObserver, and the Stripe onramp is
// application-gated (EB-003), so no build can construct one today.
//
// Returning (nil, nil) leaves the funding activity group unregistered. A
// funding workflow scheduled against this worker then fails with an unknown
// activity type, which is loud and traceable — unlike an activity that
// returned success without touching a deposit.
var bindFundingDriver = func(d *deps) (workflows.FundingDriver, error) {
	d.log.Warn("workflow-worker: no funding provider binding for this build; funding activities are not registered",
		"provider_mode", d.cfg.Providers.Funding.Mode)
	return nil, nil
}

// logAlerter is the operator alert sink of last resort: a structured log line
// at the alert's severity. It is deliberately dumb and always available, so a
// missing notification provider can never swallow an escalation silently.
// internal/notification's dispatcher replaces it once a workflow-facing
// notifier exists.
type logAlerter struct{ log *slog.Logger }

func newLogAlerter(log *slog.Logger) *logAlerter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &logAlerter{log: log}
}

var _ workflows.OperatorAlerter = (*logAlerter)(nil)

func (a *logAlerter) Alert(ctx context.Context, al workflows.OperatorAlert) error {
	attrs := []any{
		"alert_kind", al.Kind, "severity", al.Severity, "subject", al.Subject,
		"resource_type", al.ResourceType, "resource_id", al.ResourceID,
		"detail", al.Detail, "correlation_id", al.CorrelationID,
	}
	// Fields are sorted by the handler; the alert carries no secrets by
	// construction (ids and states only).
	for k, v := range al.Fields {
		attrs = append(attrs, "field."+k, v)
	}
	switch al.Severity {
	case workflows.SeverityCritical:
		a.log.ErrorContext(ctx, "operator alert", attrs...)
	case workflows.SeverityWarning:
		a.log.WarnContext(ctx, "operator alert", attrs...)
	default:
		a.log.InfoContext(ctx, "operator alert", attrs...)
	}
	return nil
}

// unwiredRecords fails closed for every reconciliation operation.
//
// internal/reconciliation is owned by another work stream and its repository
// shape is not fixed yet, so this build cannot bind to it. Failing closed is
// the only honest option: an escalation workflow that appeared to succeed
// while writing nothing would leave an operator believing a mismatch had been
// recorded and raised when it had not.
type unwiredRecords struct{}

var _ workflows.ReconciliationRecords = unwiredRecords{}

func (unwiredRecords) Describe(context.Context, string) (workflows.RecordState, error) {
	return workflows.RecordState{}, errUnwiredReconciliation
}

func (unwiredRecords) MarkInvestigating(context.Context, string, string) error {
	return errUnwiredReconciliation
}

func (unwiredRecords) MarkEscalated(context.Context, string, string) error {
	return errUnwiredReconciliation
}

var errUnwiredReconciliation = errs.New(errs.CodeUnsupported,
	"workflow-worker: the reconciliation record store is not wired in this build; escalation workflows cannot run here")

// bindReconciliation returns the reconciliation record store and the
// containment controller. Both are unwired today (see unwiredRecords), and a
// nil containment controller makes internal/workflows fail a containment
// request loudly rather than pretend trading was halted.
func bindReconciliation(*deps) (workflows.ReconciliationRecords, workflows.ContainmentController) {
	return unwiredRecords{}, nil
}
