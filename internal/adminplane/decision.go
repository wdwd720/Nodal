package adminplane

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// Verb is one thing an operator can attempt against a controlled action.
type Verb string

// Verbs, matching the entry points of admin.Actions plus Cancel.
const (
	VerbPropose Verb = "propose"
	VerbApprove Verb = "approve"
	VerbReject  Verb = "reject"
	VerbExecute Verb = "execute"
	VerbCancel  Verb = "cancel"
)

var allVerbs = []Verb{VerbPropose, VerbApprove, VerbReject, VerbExecute, VerbCancel}

// Verbs returns every verb in declaration order.
func Verbs() []Verb { return append([]Verb(nil), allVerbs...) }

// Reason is the machine-readable explanation of a Decision. It is never a
// sentence: the console owns the wording, this package owns the fact.
type Reason string

// Reasons. ReasonAllowed is the only one that accompanies Allowed == true.
const (
	ReasonAllowed Reason = "ALLOWED"
	// ReasonUnauthenticated: no principal at all.
	ReasonUnauthenticated Reason = "UNAUTHENTICATED"
	// ReasonAgentPrincipal: an AGENT reached an operator path. Agents are
	// refused before any query and before any other check.
	ReasonAgentPrincipal Reason = "AGENT_PRINCIPAL"
	// ReasonInvalidPrincipal: the principal fails security.Principal.Validate
	// (a tampered agent carrying roles, an empty subject, an unknown role).
	ReasonInvalidPrincipal Reason = "INVALID_PRINCIPAL"
	// ReasonSubjectNotUser: the subject is not a users.id, so no admin_actions
	// row could reference it.
	ReasonSubjectNotUser Reason = "SUBJECT_NOT_A_USER"
	// ReasonUnknownKind: the action kind is not in the closed table.
	ReasonUnknownKind Reason = "UNKNOWN_KIND"
	// ReasonMissingPermission: the principal does not hold the permission the
	// kind requires for this verb. Decision.Permission names it.
	ReasonMissingPermission Reason = "MISSING_PERMISSION"
	// ReasonStepUpRequired: the authentication is too old or was not strong.
	// Decision.StepUpMaxAge is the window that must be met.
	ReasonStepUpRequired Reason = "STEP_UP_REQUIRED"
	// ReasonSelfApproval: the principal proposed this action. This is the one
	// the console must make unreachable rather than merely explain.
	ReasonSelfApproval Reason = "SELF_APPROVAL"
	// ReasonNotProposer: only the proposer may cancel their own proposal.
	ReasonNotProposer Reason = "NOT_PROPOSER"
	// ReasonKindTakesNoApproval: a single-control kind has no approve step.
	ReasonKindTakesNoApproval Reason = "KIND_TAKES_NO_APPROVAL"
	// ReasonExpired: the proposal's expiry has passed.
	ReasonExpired Reason = "EXPIRED"
	// ReasonWrongStatus: the state machine forbids this transition.
	ReasonWrongStatus Reason = "WRONG_STATUS"
	// ReasonAwaitingApproval: a dual-control action cannot execute while it is
	// still only PROPOSED.
	ReasonAwaitingApproval Reason = "AWAITING_APPROVAL"
	// ReasonApproverNotDistinct: the stored approval does not name a principal
	// other than the proposer, so the dual-control record is incomplete.
	ReasonApproverNotDistinct Reason = "APPROVER_NOT_DISTINCT"
)

// Decision is the advisory verdict for one (principal, verb, action) triple.
// Code is the errs.Code the enforcing call would return, so a console can key
// its copy off the same code it would see in a problem+json refusal.
type Decision struct {
	Verb    Verb      `json:"verb"`
	Allowed bool      `json:"allowed"`
	Reason  Reason    `json:"reason"`
	Code    errs.Code `json:"code,omitempty"`
	// Permission is the permission the verb needs, when a single permission
	// decides it. For verbs that accept any of several it is empty and
	// Permissions carries the set.
	Permission security.Permission `json:"permission,omitempty"`
	// Permissions is the any-of set when more than one permission satisfies
	// the verb (Reject and Execute accept the propose or the approve side).
	Permissions []security.Permission `json:"permissions,omitempty"`
	// StepUpMaxAge is the freshness the verb demands, zero when it demands
	// none. Kill-switch activation and Reject and Cancel demand none: stopping
	// risk and refusing a proposal must never wait on a second factor.
	StepUpMaxAge Seconds `json:"step_up_max_age_seconds,omitempty"`
}

func deny(v Verb, r Reason, c errs.Code) Decision {
	return Decision{Verb: v, Allowed: false, Reason: r, Code: c}
}

// Actor is the subject a Decision is made for. It is deliberately not a bare
// security.Principal: an operator console asks about actions proposed by other
// people, and the canonical users.id form of the subject is what admin_actions
// stores and compares.
type Actor struct {
	Principal security.Principal
	// UserID is Principal.SubjectID in canonical users.id form, empty when the
	// subject is not a user id at all.
	UserID string
}

// NewActor derives the canonical user id once so every Decide call compares
// the same string admin.Service would.
func NewActor(p security.Principal) Actor {
	a := Actor{Principal: p}
	if u, err := id.ParseAny(p.SubjectID); err == nil && !u.IsZero() {
		a.UserID = u.String()
	}
	return a
}

// Anonymous reports whether there is no principal at all. The zero Actor is
// the unauthenticated request, which is what a console holds before the
// session cookie has been exchanged for a principal.
func (a Actor) Anonymous() bool {
	return a.Principal.SubjectID == "" && a.Principal.ActorType == "" && len(a.Principal.Roles) == 0
}

// authenticated reproduces admin.caller: a principal must be present, must not
// be an AGENT, and must validate. The order is the enforcing order, so the
// reason a console shows is the reason the server would give.
func (a Actor) authenticated(v Verb) (Decision, bool) {
	if a.Anonymous() {
		return deny(v, ReasonUnauthenticated, errs.CodeUnauthenticated), false
	}
	if a.Principal.ActorType == security.ActorAgent {
		return deny(v, ReasonAgentPrincipal, errs.CodeForbidden), false
	}
	if a.Principal.Validate() != nil {
		return deny(v, ReasonInvalidPrincipal, errs.CodeForbidden), false
	}
	return Decision{}, true
}

// has reports whether the actor holds perm at now.
func (a Actor) has(perm security.Permission, now time.Time) bool {
	return a.Principal.Has(perm, now)
}

// hasAny reports whether the actor holds at least one of perms at now.
func (a Actor) hasAny(perms []security.Permission, now time.Time) bool {
	for _, p := range perms {
		if a.has(p, now) {
			return true
		}
	}
	return false
}

// steppedUp reproduces security.RequireStepUp for a non-positive-safe window.
func (a Actor) steppedUp(maxAge time.Duration, now time.Time) bool {
	if maxAge <= 0 || a.Principal.AuthTime.IsZero() {
		return false
	}
	age := now.Sub(a.Principal.AuthTime)
	if age < 0 {
		age = 0
	}
	return age <= maxAge && security.HasStrongAMR(a.Principal.AMR)
}

// DecideProposal answers whether the actor may propose kind at now. It is the
// affordance behind "which action kinds does this operator's New action menu
// contain": a kind the operator cannot propose is never offered.
func DecideProposal(a Actor, kind admin.Kind, now time.Time) Decision {
	if d, ok := a.authenticated(VerbPropose); !ok {
		return d
	}
	spec, known := admin.Spec(kind)
	if !known {
		return deny(VerbPropose, ReasonUnknownKind, errs.CodeValidationFailed)
	}
	d := Decision{
		Verb: VerbPropose, Permission: spec.ProposePermission, StepUpMaxAge: Sec(spec.StepUpMaxAge),
	}
	switch {
	case !a.has(spec.ProposePermission, now):
		d.Reason, d.Code = ReasonMissingPermission, errs.CodeForbidden
	case !a.steppedUp(spec.StepUpMaxAge, now):
		d.Reason, d.Code = ReasonStepUpRequired, errs.CodeStepUpRequired
	case a.UserID == "":
		d.Reason, d.Code = ReasonSubjectNotUser, errs.CodeForbidden
	default:
		d.Allowed, d.Reason = true, ReasonAllowed
	}
	return d
}

// Decide answers whether the actor may apply verb to action at now. It never
// touches the database: everything it needs is on the action.
//
// The check order mirrors internal/admin exactly. Reordering it would still
// produce the right boolean and the wrong reason, and an operator who is told
// the wrong reason retries the wrong thing.
func Decide(a Actor, action admin.Action, verb Verb, now time.Time) Decision {
	if d, ok := a.authenticated(verb); !ok {
		return d
	}
	switch verb {
	case VerbApprove:
		return decideApprove(a, action, now)
	case VerbReject:
		return decideReject(a, action, now)
	case VerbCancel:
		return decideCancel(a, action, now)
	case VerbExecute:
		return decideExecute(a, action, now)
	case VerbPropose:
		return DecideProposal(a, action.Kind, now)
	default:
		return deny(verb, ReasonUnknownKind, errs.CodeValidationFailed)
	}
}

// DecideAll returns the verdict for every verb that can apply to a stored
// action, in Verbs order and excluding VerbPropose (which is about a kind, not
// a record). A console renders exactly the allowed ones.
func DecideAll(a Actor, action admin.Action, now time.Time) []Decision {
	out := make([]Decision, 0, len(allVerbs)-1)
	for _, v := range allVerbs {
		if v == VerbPropose {
			continue
		}
		out = append(out, Decide(a, action, v, now))
	}
	return out
}

func decideApprove(a Actor, action admin.Action, now time.Time) Decision {
	spec, known := admin.Spec(action.Kind)
	if !known {
		return deny(VerbApprove, ReasonUnknownKind, errs.CodeInternal)
	}
	d := Decision{Verb: VerbApprove, Permission: spec.ApprovePermission, StepUpMaxAge: Sec(spec.StepUpMaxAge)}
	switch {
	case !spec.RequiresDual || spec.ApprovePermission == "":
		d.Permission = ""
		d.Reason, d.Code = ReasonKindTakesNoApproval, errs.CodeInvalidStateTransition
	case !a.has(spec.ApprovePermission, now):
		d.Reason, d.Code = ReasonMissingPermission, errs.CodeForbidden
	case !a.steppedUp(spec.StepUpMaxAge, now):
		d.Reason, d.Code = ReasonStepUpRequired, errs.CodeStepUpRequired
	case a.UserID == "":
		d.Reason, d.Code = ReasonSubjectNotUser, errs.CodeForbidden
	case a.UserID == action.ProposedBy:
		// The whole point of dual control. Never rendered as a live button.
		d.Reason, d.Code = ReasonSelfApproval, errs.CodeForbidden
	case spec.ApproverIsNotTarget && a.UserID == canonicalUserID(action.TargetID):
		// The other shape of self-approval: for a kind whose target_id names
		// a person (BREAK_GLASS_GRANT), approving it is granting yourself the
		// thing it grants, however many other people were involved.
		d.Reason, d.Code = ReasonSelfApproval, errs.CodeForbidden
	case action.Expired(now):
		d.Reason, d.Code = ReasonExpired, errs.CodeInvalidStateTransition
	case !admin.CanTransition(action.Status, admin.StatusApproved):
		d.Reason, d.Code = ReasonWrongStatus, errs.CodeInvalidStateTransition
	default:
		d.Allowed, d.Reason = true, ReasonAllowed
	}
	return d
}

// decideReject mirrors admin.Service.Reject: any holder of the kind's propose
// or approve permission may refuse a proposal, no step-up is demanded, and
// expiry does not block a refusal.
func decideReject(a Actor, action admin.Action, now time.Time) Decision {
	spec, known := admin.Spec(action.Kind)
	if !known {
		return deny(VerbReject, ReasonUnknownKind, errs.CodeInternal)
	}
	d := Decision{Verb: VerbReject, Permissions: actorPermissions(spec)}
	switch {
	case !a.hasAny(d.Permissions, now):
		d.Reason, d.Code = ReasonMissingPermission, errs.CodeForbidden
	case a.UserID == "":
		d.Reason, d.Code = ReasonSubjectNotUser, errs.CodeForbidden
	case action.Status != admin.StatusProposed:
		d.Reason, d.Code = ReasonWrongStatus, errs.CodeInvalidStateTransition
	default:
		d.Allowed, d.Reason = true, ReasonAllowed
	}
	return d
}

// decideCancel mirrors admin.Service.Cancel: the proposer withdrawing their own
// proposal needs no permission and no step-up, because withdrawing a request
// removes authority rather than exercising it.
func decideCancel(a Actor, action admin.Action, _ time.Time) Decision {
	d := Decision{Verb: VerbCancel}
	switch {
	case a.UserID == "":
		d.Reason, d.Code = ReasonSubjectNotUser, errs.CodeForbidden
	case a.UserID != action.ProposedBy:
		d.Reason, d.Code = ReasonNotProposer, errs.CodeForbidden
	case action.Status != admin.StatusProposed:
		d.Reason, d.Code = ReasonWrongStatus, errs.CodeInvalidStateTransition
	default:
		d.Allowed, d.Reason = true, ReasonAllowed
	}
	return d
}

func decideExecute(a Actor, action admin.Action, now time.Time) Decision {
	spec, known := admin.Spec(action.Kind)
	if !known {
		return deny(VerbExecute, ReasonUnknownKind, errs.CodeInternal)
	}
	d := Decision{Verb: VerbExecute, Permissions: actorPermissions(spec), StepUpMaxAge: Sec(spec.StepUpMaxAge)}
	switch {
	case !a.hasAny(d.Permissions, now):
		d.Reason, d.Code = ReasonMissingPermission, errs.CodeForbidden
	case !a.steppedUp(spec.StepUpMaxAge, now):
		d.Reason, d.Code = ReasonStepUpRequired, errs.CodeStepUpRequired
	case a.UserID == "":
		d.Reason, d.Code = ReasonSubjectNotUser, errs.CodeForbidden
	case action.Expired(now):
		d.Reason, d.Code = ReasonExpired, errs.CodeInvalidStateTransition
	case action.Status == admin.StatusApproved:
		if spec.RequiresDual && (action.ApprovedBy == nil || *action.ApprovedBy == action.ProposedBy) {
			d.Reason, d.Code = ReasonApproverNotDistinct, errs.CodeForbidden
			return d
		}
		d.Allowed, d.Reason = true, ReasonAllowed
	case action.Status == admin.StatusProposed:
		if spec.RequiresDual {
			d.Reason, d.Code = ReasonAwaitingApproval, errs.CodeInvalidStateTransition
			return d
		}
		d.Allowed, d.Reason = true, ReasonAllowed
	default:
		d.Reason, d.Code = ReasonWrongStatus, errs.CodeInvalidStateTransition
	}
	return d
}

// actorPermissions mirrors admin's own any-of set for Reject and Execute: the
// propose permission plus, for dual-control kinds, the distinct approve one.
func actorPermissions(spec admin.KindSpec) []security.Permission {
	perms := []security.Permission{spec.ProposePermission}
	if spec.ApprovePermission != "" && spec.ApprovePermission != spec.ProposePermission {
		perms = append(perms, spec.ApprovePermission)
	}
	sort.Slice(perms, func(i, j int) bool { return perms[i] < perms[j] })
	return perms
}
