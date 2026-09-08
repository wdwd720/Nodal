package security

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// StrongAMR lists the RFC 8176 Authentication Method Reference values (plus
// the widely used "webauthn" and "passkey") that count as a second factor
// for RequireStepUp:
//
//	mfa       multiple-factor authentication performed by the IdP
//	otp       one-time password (TOTP/HOTP)
//	hwk       proof-of-possession of a hardware-secured key
//	swk       proof-of-possession of a software-secured key
//	pop       proof-of-possession of a key
//	webauthn  WebAuthn / FIDO2 assertion
//	passkey   platform passkey assertion
//
// Password-only ("pwd"), SMS ("sms") and knowledge-based ("kba") values do
// not qualify. Matching is case-insensitive.
var StrongAMR = []string{"mfa", "otp", "hwk", "swk", "pop", "webauthn", "passkey"}

// HasStrongAMR reports whether amr contains at least one StrongAMR value.
func HasStrongAMR(amr []string) bool {
	for _, have := range amr {
		for _, strong := range StrongAMR {
			if strings.EqualFold(have, strong) {
				return true
			}
		}
	}
	return false
}

// wallClock is the fallback clock for the convenience wrappers. It is only
// consulted to decide whether a break-glass grant is still live.
func wallClock() time.Time { return time.Now().UTC() }

// principalFor extracts and validates the principal, failing closed.
func principalFor(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	if err := p.Validate(); err != nil {
		return Principal{}, fmt.Errorf("%w: invalid principal: %v", ErrForbidden, err)
	}
	return p, nil
}

// Require returns nil when the context principal holds perm. It returns
// ErrUnauthenticated for anonymous contexts and ErrForbidden otherwise.
// Break-glass liveness is judged against the wall clock; use RequireAt to
// inject a clock.
func Require(ctx context.Context, perm Permission) error {
	return RequireAt(ctx, perm, wallClock)
}

// RequireAt is Require with an injected clock.
func RequireAt(ctx context.Context, perm Permission, now func() time.Time) error {
	p, err := principalFor(ctx)
	if err != nil {
		return err
	}
	if !p.has(perm, p.BreakGlassActive(now())) {
		return fmt.Errorf("%w: missing permission %q", ErrForbidden, perm)
	}
	return nil
}

// RequireAny returns nil when the principal holds at least one of perms.
// An empty list is refused (fail closed).
func RequireAny(ctx context.Context, perms ...Permission) error {
	return RequireAnyAt(ctx, wallClock, perms...)
}

// RequireAnyAt is RequireAny with an injected clock.
func RequireAnyAt(ctx context.Context, now func() time.Time, perms ...Permission) error {
	p, err := principalFor(ctx)
	if err != nil {
		return err
	}
	if len(perms) == 0 {
		return fmt.Errorf("%w: no permission requested", ErrForbidden)
	}
	live := p.BreakGlassActive(now())
	for _, perm := range perms {
		if p.has(perm, live) {
			return nil
		}
	}
	return fmt.Errorf("%w: missing all of %v", ErrForbidden, perms)
}

// RequireAccount enforces tenant scoping for a READ (PART 92). It returns nil
// when:
//   - the principal owns accountID (customers, and agents on their single
//     bound account), or
//   - the principal is not an agent and holds account:read_any (operators).
//
// Every other case is ErrCrossTenant; anonymous is ErrUnauthenticated. An
// empty accountID is always refused. Ownership never depends on timing, so
// break-glass is irrelevant here.
//
// # Reads only
//
// The operator override here is `account:read_any` — a READ permission — and
// for a long time this function was the only tenant check on the WRITE routes
// too. That was wrong in a way nothing could see: RoleAdmin is every
// permission except the dual-control and agent-only sets, so it holds
// `account:read_any` AND `native_market:trade`, `commerce:buy`,
// `payout:create`, `withdrawal:create` and the rest of the customer surface.
// Those customer permissions exist so an operator can use their OWN account;
// combined with the override, one ADMIN session could buy, sell, and reserve
// a payout out of ANY customer's balance, with no second signature and no
// admin_actions row. Use RequireAccountOwner for anything that changes state.
func RequireAccount(ctx context.Context, accountID string) error {
	p, err := principalFor(ctx)
	if err != nil {
		return err
	}
	if accountID == "" {
		return fmt.Errorf("%w: empty account id", ErrCrossTenant)
	}
	if p.OwnsAccount(accountID) {
		return nil
	}
	if p.ActorType == ActorAgent {
		return fmt.Errorf("%w: agent %q is not bound to account %q", ErrCrossTenant, p.SubjectID, accountID)
	}
	if p.has(PermAccountReadAny, false) {
		return nil
	}
	return fmt.Errorf("%w: subject %q does not own account %q", ErrCrossTenant, p.SubjectID, accountID)
}

// RequireAccountOwner enforces tenant scoping for a WRITE. It returns nil only
// when the principal OWNS accountID.
//
// There is no operator override, deliberately. An operator who needs to change
// a customer's position has the admin plane, where the act needs a reason, a
// second principal for anything consequential, and a permanent row naming both.
// A customer endpoint that accepted an operator acting "as" someone would give
// the same power with none of that, which is what `account:read_any` was
// silently doing on fourteen write routes.
//
// The refusal is ErrCrossTenant, the same as RequireAccount's, so an operator
// who tries gets the answer a stranger gets. That is the point: on a write
// there is nothing special about being an operator.
func RequireAccountOwner(ctx context.Context, accountID string) error {
	p, err := principalFor(ctx)
	if err != nil {
		return err
	}
	if accountID == "" {
		return fmt.Errorf("%w: empty account id", ErrCrossTenant)
	}
	if p.OwnsAccount(accountID) {
		return nil
	}
	return fmt.Errorf("%w: subject %q may not act on account %q; "+
		"changing another account's state is an admin action, not a customer request",
		ErrCrossTenant, p.SubjectID, accountID)
}

// RequireStepUp returns nil when the principal authenticated within maxAge
// of now() using a strong method (HasStrongAMR). Otherwise it returns
// ErrStepUpRequired, and the caller should redirect to the identity
// provider with stepUp=true. A zero or negative maxAge is a configuration
// error and fails closed. An AuthTime slightly in the future (clock skew) is
// treated as "now".
func RequireStepUp(ctx context.Context, maxAge time.Duration, now func() time.Time) error {
	p, err := principalFor(ctx)
	if err != nil {
		return err
	}
	if maxAge <= 0 {
		return fmt.Errorf("%w: non-positive max age", ErrStepUpRequired)
	}
	if p.AuthTime.IsZero() {
		return fmt.Errorf("%w: no authentication time", ErrStepUpRequired)
	}
	age := now().Sub(p.AuthTime)
	if age < 0 {
		age = 0
	}
	if age > maxAge {
		return fmt.Errorf("%w: authenticated %s ago, max %s", ErrStepUpRequired, age.Round(time.Second), maxAge)
	}
	if !HasStrongAMR(p.AMR) {
		return fmt.Errorf("%w: no strong authentication method in amr", ErrStepUpRequired)
	}
	return nil
}

// RequireDualControl authorizes the approve side of a two-person action.
// perm must be a dual-control permission (IsDualControl); the principal
// must hold it (in practice: a live BREAK_GLASS elevation) and must not be
// the subject that proposed the action. It does not itself require step-up:
// the elevation that grants BREAK_GLASS is where step-up is enforced.
func RequireDualControl(ctx context.Context, perm Permission, proposerSubjectID string, now func() time.Time) error {
	if !IsDualControl(perm) {
		return fmt.Errorf("%w: %q is not a dual-control permission", ErrForbidden, perm)
	}
	if err := RequireAt(ctx, perm, now); err != nil {
		return err
	}
	p, _ := PrincipalFrom(ctx)
	if proposerSubjectID == "" || p.SubjectID == proposerSubjectID {
		return fmt.Errorf("%w: subject %q", ErrSelfApproval, p.SubjectID)
	}
	return nil
}
