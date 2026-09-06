package settlement

import "github.com/nodal/controlplane/internal/id"

type (
	planKind struct{}
	stepKind struct{}
)

// PlanID identifies an execution_plans row.
type PlanID = id.ID[planKind]

// StepID identifies an execution_plan_steps row.
type StepID = id.ID[stepKind]

// NewPlanID mints a plan id.
func NewPlanID() PlanID { return id.New[planKind]() }

// NewStepID mints a step id.
func NewStepID() StepID { return id.New[stepKind]() }

// ParsePlanID parses the canonical form.
func ParsePlanID(s string) (PlanID, error) { return id.Parse[planKind](s) }

// ParseStepID parses the canonical form.
func ParseStepID(s string) (StepID, error) { return id.Parse[stepKind](s) }
