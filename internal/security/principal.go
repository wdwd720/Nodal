package security

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Principal is the authenticated identity attached to a request context by
// the session middleware. It is a value type: mutate a copy, never the one
// in the context.
//
// Invariants (enforced by Validate and, fail-closed, by every Require*):
//   - SubjectID is non-empty and ActorType is known;
//   - an AGENT carries no roles, no break-glass, and exactly one AccountID;
//   - the BREAK_GLASS role is only present together with BreakGlassUntil.
type Principal struct {
	SubjectID       string
	ActorType       ActorType
	Roles           []Role
	AccountIDs      []string
	SessionID       string
	AuthTime        time.Time
	AMR             []string
	BreakGlassUntil *time.Time
}

// AgentPrincipal builds the only shape of Principal an agent may have: the
// AGENT actor type, the agent's own identifier as subject, a single bound
// account, and no roles. There is no way to add roles to it that survives
// Validate.
func AgentPrincipal(agentID, accountID string) Principal {
	return Principal{
		SubjectID:  agentID,
		ActorType:  ActorAgent,
		AccountIDs: []string{accountID},
	}
}

// Validate checks the structural invariants documented on Principal.
func (p Principal) Validate() error {
	if p.SubjectID == "" {
		return errors.New("security: principal has empty subject")
	}
	if !p.ActorType.Valid() {
		return fmt.Errorf("security: unknown actor type %q", p.ActorType)
	}
	for _, r := range p.Roles {
		if !r.Valid() {
			return fmt.Errorf("security: unknown role %q", r)
		}
	}
	for _, a := range p.AccountIDs {
		if a == "" {
			return errors.New("security: principal has empty account id")
		}
	}
	if p.ActorType == ActorAgent {
		switch {
		case len(p.Roles) != 0:
			return errors.New("security: agent principal must not carry roles")
		case len(p.AccountIDs) != 1:
			return fmt.Errorf("security: agent principal must be bound to exactly one account, has %d", len(p.AccountIDs))
		case p.BreakGlassUntil != nil:
			return errors.New("security: agent principal must not carry break-glass")
		}
	}
	if p.HasRole(RoleBreakGlass) && p.BreakGlassUntil == nil {
		return errors.New("security: BREAK_GLASS role requires BreakGlassUntil")
	}
	return nil
}

// IsAgent reports whether the principal is an AGENT actor.
func (p Principal) IsAgent() bool { return p.ActorType == ActorAgent }

// HasRole reports whether r is among the principal's roles. It is a raw
// membership test: it does not consider break-glass timing and is always
// false for a valid agent.
func (p Principal) HasRole(r Role) bool {
	for _, have := range p.Roles {
		if have == r {
			return true
		}
	}
	return false
}

// OwnsAccount reports whether accountID is one of the principal's own
// accounts. It never consults roles.
func (p Principal) OwnsAccount(accountID string) bool {
	if accountID == "" {
		return false
	}
	for _, a := range p.AccountIDs {
		if a == accountID {
			return true
		}
	}
	return false
}

// BreakGlassActive reports whether a break-glass elevation is live at now.
func (p Principal) BreakGlassActive(now time.Time) bool {
	return p.BreakGlassUntil != nil && now.Before(*p.BreakGlassUntil)
}

// Has reports whether the principal holds perm at now. An invalid principal
// holds nothing. Agents hold exactly AgentPermissions regardless of any
// roles present; break-glass permissions count only while live.
func (p Principal) Has(perm Permission, now time.Time) bool {
	if p.Validate() != nil {
		return false
	}
	return p.has(perm, p.BreakGlassActive(now))
}

// has is the unvalidated core of Has; callers must have validated p.
func (p Principal) has(perm Permission, breakGlassActive bool) bool {
	if p.ActorType == ActorAgent {
		_, ok := agentPermSet[perm]
		return ok
	}
	for _, r := range p.Roles {
		if r == RoleBreakGlass && !breakGlassActive {
			continue
		}
		if _, ok := rolePermSets[r][perm]; ok {
			return true
		}
	}
	return false
}

// Permissions returns the sorted set of permissions the principal holds at
// now (empty for an invalid principal).
func (p Principal) Permissions(now time.Time) []Permission {
	if p.Validate() != nil {
		return []Permission{}
	}
	live := p.BreakGlassActive(now)
	out := make([]Permission, 0, len(allPermissions))
	for _, perm := range allPermissions {
		if p.has(perm, live) {
			out = append(out, perm)
		}
	}
	return sorted(out)
}

// clone returns a deep copy so context holders cannot alias slices.
func (p Principal) clone() Principal {
	c := p
	c.Roles = append([]Role(nil), p.Roles...)
	c.AccountIDs = append([]string(nil), p.AccountIDs...)
	c.AMR = append([]string(nil), p.AMR...)
	if p.BreakGlassUntil != nil {
		t := *p.BreakGlassUntil
		c.BreakGlassUntil = &t
	}
	return c
}

type ctxKey struct{}

// WithPrincipal attaches a copy of p to ctx. Only the session middleware
// (and tests) should call this; handlers never construct principals.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p.clone())
}

// PrincipalFrom returns the principal attached to ctx, if any. Absence means
// the request is anonymous.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	if !ok {
		return Principal{}, false
	}
	return p.clone(), true
}
