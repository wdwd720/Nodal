package workflows

import (
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"

	"github.com/nodal/controlplane/internal/errs"
)

// Registry is the part of worker.Worker this package uses. Declaring it here
// keeps the composition root free to pass a real worker, and keeps this
// package testable against a recording fake.
type Registry interface {
	RegisterWorkflowWithOptions(w any, options workflow.RegisterOptions)
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}

// Deps are the implementations the activities need. Both groups are optional
// so a deployment can host one workflow family without the other, but at
// least one must be present.
type Deps struct {
	Funding    *FundingActivities
	Escalation *EscalationActivities
}

// Register registers every workflow and activity under its stable name.
//
// Names are explicit rather than derived from Go identifiers. A workflow in
// flight is resumed by the name recorded in its history, so a rename or a
// refactor that changed a function name would strand running executions;
// pinning the names makes that impossible by accident.
func Register(r Registry, d Deps) error {
	if r == nil {
		return errs.New(errs.CodeInternal, "workflows: nil registry")
	}
	if d.Funding == nil && d.Escalation == nil {
		return errs.New(errs.CodeInternal, "workflows: at least one activity group is required")
	}
	RegisterWorkflows(r)
	if d.Funding != nil {
		r.RegisterActivityWithOptions(d.Funding.AdvanceDeposit, activity.RegisterOptions{Name: AdvanceDepositName})
		r.RegisterActivityWithOptions(d.Funding.DescribeDeposit, activity.RegisterOptions{Name: DescribeDepositName})
		r.RegisterActivityWithOptions(d.Funding.EscalateDeposit, activity.RegisterOptions{Name: EscalateDepositName})
	}
	if d.Escalation != nil {
		r.RegisterActivityWithOptions(d.Escalation.DescribeRecord, activity.RegisterOptions{Name: DescribeRecordName})
		r.RegisterActivityWithOptions(d.Escalation.MarkInvestigating, activity.RegisterOptions{Name: MarkInvestigatingName})
		r.RegisterActivityWithOptions(d.Escalation.MarkEscalated, activity.RegisterOptions{Name: MarkEscalatedName})
		r.RegisterActivityWithOptions(d.Escalation.NotifyEscalation, activity.RegisterOptions{Name: NotifyEscalationName})
		r.RegisterActivityWithOptions(d.Escalation.RequestContainment, activity.RegisterOptions{Name: RequestContainmentName})
	}
	return nil
}

// RegisterWorkflows registers only the workflow definitions. The replayer
// needs exactly this and no activities, because a replay never executes one.
func RegisterWorkflows(r interface {
	RegisterWorkflowWithOptions(w any, options workflow.RegisterOptions)
},
) {
	r.RegisterWorkflowWithOptions(FundingWorkflow, workflow.RegisterOptions{Name: FundingWorkflowName})
	r.RegisterWorkflowWithOptions(ReconciliationEscalationWorkflow, workflow.RegisterOptions{Name: EscalationWorkflowName})
}

// WorkflowNames lists every workflow this package defines, in a fixed order.
func WorkflowNames() []string { return []string{FundingWorkflowName, EscalationWorkflowName} }

// ActivityNames lists every activity this package defines, in a fixed order.
func ActivityNames() []string {
	return []string{
		AdvanceDepositName, DescribeDepositName, EscalateDepositName,
		DescribeRecordName, MarkInvestigatingName, MarkEscalatedName,
		NotifyEscalationName, RequestContainmentName,
	}
}
