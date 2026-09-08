package security_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/auth/oidc"
	"github.com/nodal/controlplane/internal/security"
)

// A future authentication time is a broken clock, not a fresh authentication
// (F-66).
//
// `auth_time` is copied verbatim out of the ID token and validated nowhere:
// `checkClaims` checks `azp`, `nbf`, `iat`, `nonce` and `sub`, and not this one.
// `RequireStepUp` then clamped a negative age to zero without a bound, so a
// value any distance in the future made the age zero -- which satisfies every
// step-up window in the system (15 minutes for the HTTP surface, 5 for the
// tightest admin kinds) for the entire 12-hour life of the session.
//
// Clock skew is real and small; an hour is not skew. Within the tolerance the
// token verifier already applies, "now" is the right reading. Beyond it the age
// is unknown, and an unknown age fails closed.

func principal(authTime time.Time) security.Principal {
	return security.Principal{
		SubjectID: "op-1", ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleAdmin}, AMR: []string{"mfa"},
		AuthTime: authTime, SessionID: "s-1",
	}
}

func TestRequireStepUp_AFutureAuthTimeBeyondSkewIsRefused(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	const window = 15 * time.Minute

	// Inside the tolerated skew: still "now", which is what makes a slightly
	// fast identity provider workable.
	for _, skew := range []time.Duration{0, time.Second, security.MaxAuthTimeSkew} {
		ctx := security.WithPrincipal(t.Context(), principal(now.Add(skew)))
		require.NoErrorf(t, security.RequireStepUp(ctx, window, clock),
			"an auth time %s in the future is within the tolerated skew and must still pass", skew)
	}

	// Beyond it: refused, and the message says why rather than reporting a
	// stale authentication, which would send a reader looking in the wrong
	// place.
	for _, skew := range []time.Duration{
		security.MaxAuthTimeSkew + time.Second,
		time.Hour,
		365 * 24 * time.Hour,
	} {
		ctx := security.WithPrincipal(t.Context(), principal(now.Add(skew)))
		err := security.RequireStepUp(ctx, window, clock)
		require.Errorf(t, err, "an auth time %s in the future was accepted as a fresh step-up", skew)
		assert.ErrorIs(t, err, security.ErrStepUpRequired)
		assert.Contains(t, err.Error(), "in the future")
	}

	// The positive control, and the reason this is not simply "refuse more":
	// an ordinary recent authentication still passes, and an ordinary stale one
	// still fails for the ordinary reason.
	fresh := security.WithPrincipal(t.Context(), principal(now.Add(-time.Minute)))
	require.NoError(t, security.RequireStepUp(fresh, window, clock))

	stale := security.WithPrincipal(t.Context(), principal(now.Add(-time.Hour)))
	err := security.RequireStepUp(stale, window, clock)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ago")
}

// TestStepUpSkewMatchesTheTokenVerifier pins the constant to the tolerance the
// ID token is actually checked against. Two numbers that must agree and live in
// packages that do not import each other are two numbers that will drift.
func TestStepUpSkewMatchesTheTokenVerifier(t *testing.T) {
	assert.Equal(t, oidc.DefaultClockSkew, security.MaxAuthTimeSkew,
		"the step-up skew tolerance and the token verifier's clock skew must be the same number: "+
			"a value the verifier accepts on iat/nbf is exactly the value that is skew rather than nonsense here")
}
