package adminplane

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/security"
)

// SurfaceID names one operator surface of the console.
type SurfaceID string

// Surfaces. Every one of these is a page the operator console renders; there
// is deliberately no surface for editing a balance, a position or a ledger row,
// because no such command exists anywhere in the system.
const (
	SurfaceAccounts       SurfaceID = "accounts"
	SurfaceReconciliation SurfaceID = "reconciliation"
	SurfaceGates          SurfaceID = "gates"
	SurfaceKillSwitches   SurfaceID = "kill_switches"
	SurfaceActions        SurfaceID = "actions"
	SurfaceWithdrawals    SurfaceID = "withdrawals"
	SurfaceEnvelopes      SurfaceID = "envelopes"
	SurfaceAgentPromotion SurfaceID = "agent_promotion"
	SurfaceBreakGlass     SurfaceID = "break_glass"
	SurfaceProviders      SurfaceID = "providers"
	SurfaceInstruments    SurfaceID = "instruments"
)

// Write is one state-changing operation a surface offers, with everything the
// console needs to decide whether to render it and what to demand first.
type Write struct {
	// ID is stable and machine-readable; the console keys its handlers off it.
	ID string `json:"id"`
	// AnyOf is the permission set, at least one of which the principal must
	// hold. Rendering a write without one of these is a dead button.
	AnyOf []security.Permission `json:"any_of"`
	// StepUpMaxAge is the strong-authentication freshness the write demands,
	// zero when it demands none.
	StepUpMaxAge Seconds `json:"step_up_max_age_seconds,omitempty"`
	// RequiresReason is true when the command refuses a reason shorter than
	// MinReasonLength.
	RequiresReason bool `json:"requires_reason"`
	// RequiresEvidence is true when the command refuses without an evidence
	// reference (a link to the incident, ticket, chain explorer or statement).
	RequiresEvidence bool `json:"requires_evidence,omitempty"`
	// ApprovalKind, when set, names the admin action kind whose APPROVED (or
	// EXECUTED) record the command demands before it will act. A console must
	// route the operator to obtain that approval first rather than offering
	// the write and letting it fail.
	ApprovalKind admin.Kind `json:"approval_kind,omitempty"`
	// ApprovalCondition explains, machine-readably, when ApprovalKind applies.
	ApprovalCondition string `json:"approval_condition,omitempty"`
	// NeverForAgent is true for writes an AGENT principal must never reach.
	// It is true for every write here; the field exists so the export states
	// it rather than leaving it implied.
	NeverForAgent bool `json:"never_for_agent"`
}

// Surface is one page of the operator console.
type Surface struct {
	ID SurfaceID `json:"id"`
	// ReadAnyOf is the permission set that makes the surface readable.
	ReadAnyOf []security.Permission `json:"read_any_of"`
	// Writes are the state-changing operations, in stable order.
	Writes []Write `json:"writes,omitempty"`
	// ActionKinds are the admin action kinds this surface proposes or reviews.
	ActionKinds []admin.Kind `json:"action_kinds,omitempty"`
}

// MinReasonLength is the shortest reason every operator command accepts. It is
// the same constant in internal/admin, internal/reconciliation and the HTTP
// boundary; the golden test pins them equal so a console can validate locally
// without being wrong somewhere.
const MinReasonLength = admin.MinReasonLength

// gateWrite builds the Write for one gate state-machine step. The permissions
// are the ones internal/gates checks, not a restatement: propose takes
// gate:propose, suspend takes kill:activate because suspending is stopping
// risk, and every other step takes gate:approve, which no standing role holds.
func gateWrite(action string, perm security.Permission) Write {
	return Write{
		ID: "gate." + action, AnyOf: []security.Permission{perm},
		StepUpMaxAge: Sec(gates.StepUpMaxAge), RequiresReason: true, NeverForAgent: true,
	}
}

var surfaces = []Surface{
	{
		ID:        SurfaceAccounts,
		ReadAnyOf: []security.Permission{security.PermAccountReadAny},
		Writes: []Write{{
			ID: "account.status", AnyOf: []security.Permission{security.PermAccountFreeze},
			StepUpMaxAge: Sec(stepUpHTTP), RequiresReason: true, NeverForAgent: true,
		}},
		ActionKinds: []admin.Kind{admin.KindAccountUnfreeze},
	},
	{
		ID:        SurfaceReconciliation,
		ReadAnyOf: []security.Permission{security.PermReconciliationRead},
		Writes: []Write{{
			ID: "reconciliation.resolve", AnyOf: []security.Permission{security.PermReconciliationResolve},
			StepUpMaxAge: Sec(stepUpHTTP), RequiresReason: true, RequiresEvidence: true,
			ApprovalKind:      admin.KindReconciliationResolveMaterial,
			ApprovalCondition: "record.material",
			NeverForAgent:     true,
		}},
		ActionKinds: []admin.Kind{admin.KindReconciliationResolveMaterial, admin.KindLedgerCorrection},
	},
	{
		ID:        SurfaceGates,
		ReadAnyOf: []security.Permission{security.PermGateRead},
		Writes: []Write{
			gateWrite(string(httpGateActionPropose), security.PermGatePropose),
			gateWrite(string(httpGateActionApprove), security.PermGateApprove),
			gateWrite(string(httpGateActionActivate), security.PermGateApprove),
			gateWrite(string(httpGateActionSuspend), security.PermKillActivate),
			gateWrite(string(httpGateActionResume), security.PermGateApprove),
			gateWrite(string(httpGateActionRevoke), security.PermGateApprove),
		},
		ActionKinds: []admin.Kind{admin.KindCapabilityGateApprove},
	},
	{
		ID:        SurfaceKillSwitches,
		ReadAnyOf: []security.Permission{security.PermRiskRead, security.PermGateRead},
		Writes: []Write{
			{
				// Deliberately no step-up: stopping new risk must never wait
				// on a second factor (POLICY_AUTHORITY §2).
				ID: "kill.activate", AnyOf: []security.Permission{security.PermKillActivate},
				RequiresReason: true, NeverForAgent: true,
			},
			{
				ID: "kill.release", AnyOf: []security.Permission{security.PermKillRelease},
				StepUpMaxAge: Sec(killswitch.StepUpMaxAge), RequiresReason: true,
				ApprovalKind:      admin.KindKillSwitchRelease,
				ApprovalCondition: "switch.severity == SEVERE",
				NeverForAgent:     true,
			},
		},
		ActionKinds: []admin.Kind{admin.KindKillSwitchRelease},
	},
	{
		ID:          SurfaceActions,
		ReadAnyOf:   admin.ReadPermissions(),
		ActionKinds: admin.Kinds(),
	},
	{
		ID:          SurfaceWithdrawals,
		ReadAnyOf:   []security.Permission{security.PermWithdrawalReview, security.PermWithdrawalApprove},
		ActionKinds: []admin.Kind{admin.KindWithdrawalApprove},
	},
	{
		ID:          SurfaceEnvelopes,
		ReadAnyOf:   []security.Permission{security.PermEnvelopeAuthorityWrite, security.PermEnvelopeApprove},
		ActionKinds: []admin.Kind{admin.KindEnvelopeAuthorityChange},
	},
	{
		ID:          SurfaceAgentPromotion,
		ReadAnyOf:   []security.Permission{security.PermAgentPromote, security.PermAgentPromoteApprove},
		ActionKinds: []admin.Kind{admin.KindAgentPromote},
	},
	{
		ID:          SurfaceBreakGlass,
		ReadAnyOf:   []security.Permission{security.PermBreakGlassRequest, security.PermBreakGlassApprove},
		ActionKinds: []admin.Kind{admin.KindBreakGlassGrant},
	},
	{
		ID:        SurfaceProviders,
		ReadAnyOf: []security.Permission{security.PermAdminAuditRead},
	},
	{
		ID:        SurfaceInstruments,
		ReadAnyOf: []security.Permission{security.PermAccountReadAny},
		Writes: []Write{{
			ID: "instrument.status", AnyOf: []security.Permission{security.PermInstrumentStatusWrite},
			StepUpMaxAge: Sec(stepUpHTTP), RequiresReason: true, NeverForAgent: true,
		}},
	},
}

// stepUpHTTP is the freshness the HTTP boundary demands of admin writes that
// have no domain-specific window of their own. It equals the domain windows it
// sits in front of (gates.StepUpMaxAge, killswitch.StepUpMaxAge), and the
// golden test asserts that equality rather than trusting the coincidence.
const stepUpHTTP = 15 * time.Minute

// httpGateAction is the gate step as it appears in the URL of
// POST /v1/admin/gates/{capability}/{action}.
type httpGateAction string

const (
	httpGateActionPropose  httpGateAction = "propose"
	httpGateActionApprove  httpGateAction = "approve"
	httpGateActionActivate httpGateAction = "activate"
	httpGateActionSuspend  httpGateAction = "suspend"
	httpGateActionResume   httpGateAction = "resume"
	httpGateActionRevoke   httpGateAction = "revoke"
)

// Surfaces returns every operator surface, in declaration order.
func Surfaces() []Surface {
	out := make([]Surface, 0, len(surfaces))
	for _, s := range surfaces {
		out = append(out, s.clone())
	}
	return out
}

// SurfaceByID returns one surface.
func SurfaceByID(idv SurfaceID) (Surface, bool) {
	for _, s := range surfaces {
		if s.ID == idv {
			return s.clone(), true
		}
	}
	return Surface{}, false
}

func (s Surface) clone() Surface {
	c := s
	c.ReadAnyOf = append([]security.Permission(nil), s.ReadAnyOf...)
	c.ActionKinds = append([]admin.Kind(nil), s.ActionKinds...)
	c.Writes = make([]Write, 0, len(s.Writes))
	for _, w := range s.Writes {
		w.AnyOf = append([]security.Permission(nil), w.AnyOf...)
		c.Writes = append(c.Writes, w)
	}
	if len(c.Writes) == 0 {
		c.Writes = nil
	}
	return c
}

// VisibleSurfaces returns the surfaces the actor may read at now, in
// declaration order. An agent, an invalid principal and an anonymous request
// see nothing at all.
func VisibleSurfaces(a Actor, now time.Time) []SurfaceID {
	if a.Anonymous() || a.Principal.ActorType == security.ActorAgent || a.Principal.Validate() != nil {
		return nil
	}
	var out []SurfaceID
	for _, s := range surfaces {
		if a.hasAny(s.ReadAnyOf, now) {
			out = append(out, s.ID)
		}
	}
	return out
}

// AllowedWrites returns the ids of the writes on one surface that the actor
// may attempt at now, in declaration order. It answers the permission and
// step-up questions only: a write whose ApprovalKind applies still needs an
// approved admin action, which is a per-record fact the console reads from the
// record itself.
func AllowedWrites(a Actor, s SurfaceID, now time.Time) []string {
	surface, ok := SurfaceByID(s)
	if !ok || a.Anonymous() || a.Principal.ActorType == security.ActorAgent || a.Principal.Validate() != nil {
		return nil
	}
	var out []string
	for _, w := range surface.Writes {
		if !a.hasAny(w.AnyOf, now) {
			continue
		}
		if w.StepUpMaxAge > 0 && !a.steppedUp(w.StepUpMaxAge.Duration(), now) {
			continue
		}
		out = append(out, w.ID)
	}
	return out
}

// KillSwitchKind is the export view of one kill-switch kind.
type KillSwitchKind struct {
	Kind     killswitch.Kind     `json:"kind"`
	Severity killswitch.Severity `json:"severity"`
	// ReleaseNeedsApproval is true exactly when the severity is SEVERE.
	ReleaseNeedsApproval bool `json:"release_needs_approval"`
}

// KillSwitchKinds returns every kind with the severity that decides how it is
// released, read from internal/killswitch.
func KillSwitchKinds() []KillSwitchKind {
	kinds := killswitch.AllKinds()
	out := make([]KillSwitchKind, 0, len(kinds))
	for _, k := range kinds {
		sev := k.Severity()
		out = append(out, KillSwitchKind{Kind: k, Severity: sev, ReleaseNeedsApproval: sev == killswitch.SeveritySevere})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Capabilities returns every capability gate this deployment knows about. They
// all default DISABLED, and no environment variable can activate one: the
// configuration flag is condition 1 of the five internal/gates evaluates.
func Capabilities() []gates.Capability { return gates.AllCapabilities() }
