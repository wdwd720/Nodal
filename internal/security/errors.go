package security

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the Require* family. They are local to this
// package so that it has no dependency on the shared errs package; the API
// layer maps them onto stable codes (UNAUTHENTICATED, FORBIDDEN,
// STEP_UP_REQUIRED). Wrapped variants keep errors.Is chains intact, so
// errors.Is(ErrCrossTenant, ErrForbidden) is true.
var (
	// ErrUnauthenticated is returned when no Principal is attached to the
	// context (anonymous request).
	ErrUnauthenticated = errors.New("security: unauthenticated")

	// ErrForbidden is returned when a Principal is present but lacks the
	// required permission, or is structurally invalid (fail closed).
	ErrForbidden = errors.New("security: forbidden")

	// ErrStepUpRequired is returned when the principal's authentication is
	// too old or was not performed with a strong (multi-factor) method.
	ErrStepUpRequired = errors.New("security: step-up authentication required")

	// ErrCrossTenant is returned when a principal addresses an account it
	// does not own and lacks account:read_any. The API layer may choose to
	// render it as NOT_FOUND to avoid confirming identifier existence; the
	// audit log must record it as a tenant violation regardless.
	ErrCrossTenant = fmt.Errorf("security: cross-tenant access: %w", ErrForbidden)

	// ErrSelfApproval is returned by RequireDualControl when the approving
	// principal is the same subject that proposed the action.
	ErrSelfApproval = fmt.Errorf("security: self-approval of a dual-control action: %w", ErrForbidden)
)
