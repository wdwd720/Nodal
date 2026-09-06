package adminplane

import (
	"time"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/security"
)

// ElevationState is what a console shows about break-glass.
type ElevationState string

// Elevation states.
const (
	// ElevationNone: the principal holds no BREAK_GLASS role at all.
	ElevationNone ElevationState = "NONE"
	// ElevationActive: the role is held and BreakGlassUntil is in the future.
	ElevationActive ElevationState = "ACTIVE"
	// ElevationExpired: the role is still on the principal but the deadline has
	// passed. This is the state that must never be rendered as authority: a
	// session issued before the expiry keeps carrying the role, and only the
	// clock revokes it.
	ElevationExpired ElevationState = "EXPIRED"
	// ElevationIncoherent: the role is present with no deadline, or a deadline
	// is present with no role. security.Principal.Validate refuses the first
	// shape outright; both mean the session is not to be trusted for
	// dual-control approval.
	ElevationIncoherent ElevationState = "INCOHERENT"
)

// Elevation is the break-glass status of one principal at one instant.
type Elevation struct {
	State ElevationState `json:"state"`
	// Until is the deadline when one is recorded, zero otherwise.
	Until time.Time `json:"until,omitempty"`
	// Remaining is Until - now, never negative.
	Remaining Seconds `json:"remaining_seconds,omitempty"`
	// Grants is the sorted set of dual-control permissions this elevation
	// actually confers at now: empty unless State is ACTIVE.
	Grants []security.Permission `json:"grants,omitempty"`
	// MaxDuration is the ceiling any single elevation may be issued for.
	MaxDuration Seconds `json:"max_duration_seconds"`
}

// ElevationOf reports the break-glass status of p at now. It is a pure read of
// the principal: an elevation is granted by an executed BREAK_GLASS_GRANT and
// applied to a session by admin.PrincipalWithBreakGlass, and it dies of the
// clock, not of a revocation call.
func ElevationOf(p security.Principal, now time.Time) Elevation {
	e := Elevation{State: ElevationNone, MaxDuration: Sec(admin.MaxBreakGlassDuration)}
	hasRole := p.HasRole(security.RoleBreakGlass)
	switch {
	case !hasRole && p.BreakGlassUntil == nil:
		return e
	case hasRole != (p.BreakGlassUntil != nil):
		e.State = ElevationIncoherent
		if p.BreakGlassUntil != nil {
			e.Until = p.BreakGlassUntil.UTC()
		}
		return e
	}
	e.Until = p.BreakGlassUntil.UTC()
	if !p.BreakGlassActive(now) {
		e.State = ElevationExpired
		return e
	}
	e.State = ElevationActive
	e.Remaining = Sec(e.Until.Sub(now))
	for _, perm := range security.DualControlPermissions() {
		if p.Has(perm, now) {
			e.Grants = append(e.Grants, perm)
		}
	}
	return e
}

// ElevationRequired reports whether kind's approve step can only be satisfied
// by a live break-glass elevation — that is, whether no standing role holds the
// approve permission. It is read from the matrix, never asserted.
func ElevationRequired(kind admin.Kind) bool {
	spec, ok := admin.Spec(kind)
	if !ok || !spec.RequiresDual || spec.ApprovePermission == "" {
		return false
	}
	return security.IsDualControl(spec.ApprovePermission)
}
