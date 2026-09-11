//go:build integration

package identity_test

// Adversarial audit (goal §54), area accounts-auth. These tests DEMONSTRATE
// defects; they are expected to fail once the defects are fixed, at which point
// they should be inverted into regressions by the fixer.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/identity"
)

// F-accounts-auth-4. openapi.yaml says return_to "Must start with a single
// slash; anything else is refused with a validation problem", and Begin
// implements that as `HasPrefix("/") && !HasPrefix("//")`.
//
// `/\evil.test` starts with a single slash and is not `//`, so it is accepted --
// and it is a protocol-relative URL to every WHATWG-conformant browser: the URL
// parser's relative-slash state treats U+005C exactly as U+002F, so
// `Location: /\evil.test` resolves against any https base to `https://evil.test/`.
// With CP_AUTH_POST_LOGIN_URL unset (the documented same-origin deployment,
// which config.Validate permits in every environment) postLoginDestination
// returns the stored path verbatim, so the callback answers
// `302 Location: /\evil.test` AFTER setting the session cookie.
func TestAudit_BeginAcceptsABackslashReturnToThatBrowsersResolveOffSite(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	for _, returnTo := range []string{
		`/\evil.test`,
		`/\evil.test/path`,
		`/\/evil.test`,
		`/\\evil.test`,
	} {
		begin, err := svc.Begin(ctx, identity.BeginRequest{ReturnTo: returnTo})
		require.NoErrorf(t, err, "Begin refused %q; if this now fails the defect is fixed", returnTo)

		done, err := svc.Complete(ctx, identity.CompleteRequest{Code: "finance", State: begin.State})
		require.NoError(t, err)
		assert.Equalf(t, returnTo, done.ReturnTo,
			"the callback will redirect the freshly signed-in browser to %q", returnTo)
	}

	// The comparison: the two shapes the existing suite tests ARE refused.
	_, err := svc.Begin(ctx, identity.BeginRequest{ReturnTo: "//evil.test"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = svc.Begin(ctx, identity.BeginRequest{ReturnTo: "https://evil.test/x"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// F-accounts-auth-5. PART 192 requires session rotation on privilege change and
// auth.Manager.Rotate is the mechanism for it -- with no caller anywhere in the
// repository. A step-up login (`/v1/auth/login?step_up=true`) is a privilege
// change: it raises the session's AuthTime and AMR, and it is the thing a user
// is asked to do before closing their account or registering a payout
// destination. identity.Complete calls Sessions.Issue, so the pre-step-up
// session stays live, keeps its own full absolute lifetime, and is still a
// usable credential.
func TestAudit_StepUpLoginLeavesThePreviousSessionLiveAndUsable(t *testing.T) {
	svc, d, clk := newService(t)
	ctx := context.Background()
	mgr, err := auth.NewManager(pgstore.New(), auth.ManagerConfig{Now: clk.Now})
	require.NoError(t, err)

	begin, err := svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	first, err := svc.Complete(ctx, identity.CompleteRequest{Code: "security", State: begin.State, UserAgent: "browser-1"})
	require.NoError(t, err)

	// The user steps up, which is what the product asks for before a closure
	// request or a payout destination.
	begin2, err := svc.Begin(ctx, identity.BeginRequest{StepUp: true})
	require.NoError(t, err)
	second, err := svc.Complete(ctx, identity.CompleteRequest{Code: "security:mfa", State: begin2.State, UserAgent: "browser-1"})
	require.NoError(t, err)
	require.True(t, second.StepUp)
	require.Equal(t, first.User.ID, second.User.ID)
	require.NotEqual(t, first.Issued.Session.ID, second.Issued.Session.ID)

	// The session the step-up replaced is not revoked and is not rotated from.
	var revoked *string
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT revoked_at::text FROM sessions WHERE id = $1`, first.Issued.Session.ID).Scan(&revoked))
	assert.Nil(t, revoked, "the pre-step-up session was not revoked")
	assert.Empty(t, second.Issued.Session.RotatedFrom,
		"the new session does not record a rotation, so nothing links it to the one it replaced")

	// And its token still authenticates.
	sess, err := mgr.Authenticate(ctx, d.Pool(), first.Issued.Token)
	require.NoError(t, err, "the old cookie still works after the step-up")
	assert.Equal(t, first.Issued.Session.ID, sess.ID)

	// Which the user's own security page counts as a second device.
	var live int
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`,
		first.User.ID).Scan(&live))
	assert.GreaterOrEqual(t, live, 2,
		"GET /v1/me/security counts these, so one browser that stepped up reports two active sessions")
}
