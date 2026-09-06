package auth

import (
	"errors"
	"fmt"

	"github.com/nodal/controlplane/internal/security"
)

// Sentinel errors. Session failures wrap security.ErrUnauthenticated so the
// API layer maps them to UNAUTHENTICATED without knowing the detail; the
// detail itself is for logs and tests, never for the response body.
var (
	// ErrDevIdPNotAllowed is returned by the dev identity provider's
	// constructor outside LOCAL/TEST/DEV.
	//
	//lint:ignore ST1003 the name is fixed by the auth contract (IdP, not IDP)
	ErrDevIdPNotAllowed = errors.New("auth: dev identity provider not allowed in this environment")

	// ErrInvalidSession is the umbrella for every reason a presented token
	// does not yield a usable session.
	ErrInvalidSession = fmt.Errorf("auth: invalid session: %w", security.ErrUnauthenticated)

	// ErrInvalidToken means the presented token is not even well formed.
	ErrInvalidToken = fmt.Errorf("auth: malformed session token: %w", ErrInvalidSession)

	// ErrSessionNotFound is returned by stores when no row matches the hash.
	ErrSessionNotFound = fmt.Errorf("auth: session not found: %w", ErrInvalidSession)

	// ErrSessionExpired means the absolute lifetime has elapsed.
	ErrSessionExpired = fmt.Errorf("auth: session expired: %w", ErrInvalidSession)

	// ErrSessionRevoked means the session was revoked (logout, rotation,
	// admin action, revoke-all).
	ErrSessionRevoked = fmt.Errorf("auth: session revoked: %w", ErrInvalidSession)

	// ErrSessionIdle means the idle timeout elapsed since LastSeenAt.
	ErrSessionIdle = fmt.Errorf("auth: session idle timeout: %w", ErrInvalidSession)

	// ErrSessionExists is returned by stores on a token-hash or id collision.
	ErrSessionExists = errors.New("auth: session already exists")

	// ErrStepUpNotSatisfied is returned by an identity provider when a
	// step-up exchange completed without a strong authentication method.
	ErrStepUpNotSatisfied = fmt.Errorf("auth: identity provider did not perform step-up: %w", security.ErrStepUpRequired)
)
