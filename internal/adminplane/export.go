package adminplane

import (
	"encoding/json"
	"fmt"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/security"
)

// AuthorityVersion is the schema version of the exported document. Bump it
// when a consumer must change to read the document, never for a value change.
const AuthorityVersion = 1

// RoleGrant is one row of the standing permission matrix.
type RoleGrant struct {
	Role security.Role `json:"role"`
	// Permissions is the sorted set the role grants. For BREAK_GLASS these are
	// granted only while the elevation is live.
	Permissions []security.Permission `json:"permissions"`
	// Standing is false for BREAK_GLASS, the one role that is never assigned
	// standing and whose grants expire on a clock.
	Standing bool `json:"standing"`
}

// ActionKind is the export view of one controlled administrative action kind.
type ActionKind struct {
	Kind              admin.Kind          `json:"kind"`
	RequiresDual      bool                `json:"requires_dual"`
	ProposePermission security.Permission `json:"propose_permission"`
	ApprovePermission security.Permission `json:"approve_permission,omitempty"`
	StepUpMaxAge      Seconds             `json:"step_up_max_age_seconds"`
	Expiry            Seconds             `json:"expiry_seconds"`
	// RequiresBreakGlassApproval is true when no standing role holds the
	// approve permission, so only a live elevation can supply it. A console
	// that renders an approve button for such a kind to an operator without an
	// elevation is offering a button that cannot work.
	RequiresBreakGlassApproval bool `json:"requires_break_glass_approval"`
}

// GateAction is the export view of one capability-gate state-machine step.
type GateAction struct {
	Action       string              `json:"action"`
	Permission   security.Permission `json:"permission"`
	StepUpMaxAge Seconds             `json:"step_up_max_age_seconds"`
}

// BreakGlass is the export view of the elevation policy.
type BreakGlass struct {
	Role        security.Role         `json:"role"`
	Kind        admin.Kind            `json:"kind"`
	MaxDuration Seconds               `json:"max_duration_seconds"`
	Grants      []security.Permission `json:"grants"`
}

// Authority is the whole operator-authority model as one document. It is what
// apps/admin loads at build time so the console restates nothing: every
// permission, window, expiry and severity it renders came from here, and here
// came from internal/security, internal/admin, internal/gates and
// internal/killswitch.
type Authority struct {
	Version int `json:"version"`
	// Source names where the document comes from, for anyone who finds the
	// generated file and wonders whether to edit it. They must not.
	Source          string                `json:"source"`
	MinReasonLength int                   `json:"min_reason_length"`
	Permissions     []security.Permission `json:"permissions"`
	DualControl     []security.Permission `json:"dual_control_permissions"`
	// AgentPermissions is the complete, closed set an AGENT principal can ever
	// hold. The console exists to prove none of them is an operator power.
	AgentPermissions []security.Permission `json:"agent_permissions"`
	Roles            []RoleGrant           `json:"roles"`
	ActionKinds      []ActionKind          `json:"action_kinds"`
	ActionStatuses   []admin.Status        `json:"action_statuses"`
	// Transitions maps each status to the statuses reachable from it.
	Transitions     map[admin.Status][]admin.Status `json:"action_transitions"`
	Verbs           []Verb                          `json:"verbs"`
	Reasons         []Reason                        `json:"reasons"`
	GateActions     []GateAction                    `json:"gate_actions"`
	Capabilities    []gates.Capability              `json:"capabilities"`
	KillSwitchKinds []KillSwitchKind                `json:"kill_switch_kinds"`
	Surfaces        []Surface                       `json:"surfaces"`
	BreakGlass      BreakGlass                      `json:"break_glass"`
}

// allReasons is every Reason, in declaration order. A console renders a phrase
// per reason and its test asserts it covers this list, so a new refusal reason
// can never reach an operator as a blank space.
var allReasons = []Reason{
	ReasonAllowed, ReasonUnauthenticated, ReasonAgentPrincipal, ReasonInvalidPrincipal,
	ReasonSubjectNotUser, ReasonUnknownKind, ReasonMissingPermission, ReasonStepUpRequired,
	ReasonSelfApproval, ReasonNotProposer, ReasonKindTakesNoApproval, ReasonExpired,
	ReasonWrongStatus, ReasonAwaitingApproval, ReasonApproverNotDistinct,
}

// Reasons returns every reason in declaration order.
func Reasons() []Reason { return append([]Reason(nil), allReasons...) }

// gateActionPermissions is the permission internal/gates.Admin checks for each
// step of the gate state machine. Suspend takes kill:activate because
// suspending a capability is stopping risk, and stopping risk is always the
// fast path.
var gateActionPermissions = []GateAction{
	{Action: string(httpGateActionPropose), Permission: security.PermGatePropose},
	{Action: string(httpGateActionApprove), Permission: security.PermGateApprove},
	{Action: string(httpGateActionActivate), Permission: security.PermGateApprove},
	{Action: string(httpGateActionSuspend), Permission: security.PermKillActivate},
	{Action: string(httpGateActionResume), Permission: security.PermGateApprove},
	{Action: string(httpGateActionRevoke), Permission: security.PermGateApprove},
}

// GateActions returns the gate steps with their permissions and step-up window.
func GateActions() []GateAction {
	out := make([]GateAction, 0, len(gateActionPermissions))
	for _, g := range gateActionPermissions {
		g.StepUpMaxAge = Sec(gates.StepUpMaxAge)
		out = append(out, g)
	}
	return out
}

// ActionKinds returns every controlled action kind with its policy, sorted by
// kind. Nothing here is written down twice: each field is read from admin.Spec.
func ActionKinds() []ActionKind {
	kinds := admin.Kinds()
	out := make([]ActionKind, 0, len(kinds))
	for _, k := range kinds {
		spec, ok := admin.Spec(k)
		if !ok {
			continue
		}
		out = append(out, ActionKind{
			Kind:                       k,
			RequiresDual:               spec.RequiresDual,
			ProposePermission:          spec.ProposePermission,
			ApprovePermission:          spec.ApprovePermission,
			StepUpMaxAge:               Sec(spec.StepUpMaxAge),
			Expiry:                     Sec(spec.Expiry),
			RequiresBreakGlassApproval: ElevationRequired(k),
		})
	}
	return out
}

// RoleGrants returns the standing permission matrix, one row per role.
func RoleGrants() []RoleGrant {
	roles := security.AllRoles()
	out := make([]RoleGrant, 0, len(roles))
	for _, r := range roles {
		out = append(out, RoleGrant{
			Role:        r,
			Permissions: security.PermissionsForRole(r),
			Standing:    r != security.RoleBreakGlass,
		})
	}
	return out
}

// transitionTable is the admin action state machine, derived by asking
// admin.CanTransition rather than copying its table.
func transitionTable() map[admin.Status][]admin.Status {
	out := make(map[admin.Status][]admin.Status, len(admin.AllStatuses()))
	for _, from := range admin.AllStatuses() {
		reachable := []admin.Status{}
		for _, to := range admin.AllStatuses() {
			if admin.CanTransition(from, to) {
				reachable = append(reachable, to)
			}
		}
		out[from] = reachable
	}
	return out
}

// Export builds the whole authority document.
func Export() Authority {
	return Authority{
		Version:          AuthorityVersion,
		Source:           "internal/adminplane (generated; do not edit by hand)",
		MinReasonLength:  MinReasonLength,
		Permissions:      security.AllPermissions(),
		DualControl:      security.DualControlPermissions(),
		AgentPermissions: security.AgentPermissions(),
		Roles:            RoleGrants(),
		ActionKinds:      ActionKinds(),
		ActionStatuses:   admin.AllStatuses(),
		Transitions:      transitionTable(),
		Verbs:            Verbs(),
		Reasons:          Reasons(),
		GateActions:      GateActions(),
		Capabilities:     Capabilities(),
		KillSwitchKinds:  KillSwitchKinds(),
		Surfaces:         Surfaces(),
		BreakGlass: BreakGlass{
			Role:        security.RoleBreakGlass,
			Kind:        admin.KindBreakGlassGrant,
			MaxDuration: Sec(admin.MaxBreakGlassDuration),
			Grants:      security.PermissionsForRole(security.RoleBreakGlass),
		},
	}
}

// ExportJSON renders the document deterministically: indented, sorted keys
// (encoding/json sorts map keys), and a trailing newline so it is a
// well-behaved text file in git.
func ExportJSON() ([]byte, error) {
	b, err := json.MarshalIndent(Export(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("adminplane: marshal authority: %w", err)
	}
	return append(b, '\n'), nil
}
