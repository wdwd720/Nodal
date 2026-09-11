//go:build integration

package identity_test

// Adversarial audit (goal §54), area accounts-auth. These demonstrated defects
// and are inverted into the regressions for their fixes. The return_to one is
// fixed on fix/config-deploy, so it FAILS on this branch and passes once both
// branches are merged; see its own comment.

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
//
// The guard and postLoginDestination belong to the config-deploy fix branch, so
// THIS TEST FAILS ON fix/accounts BY DESIGN and passes once both branches are
// merged. It is inverted rather than left as written because the auditor's file
// exists only on this branch: nobody on the branch carrying the fix can turn the
// demonstration into a regression, so it is done here and the failure is
// reported rather than hidden.
func TestAudit_BeginAcceptsABackslashReturnToThatBrowsersResolveOffSite(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	for _, returnTo := range []string{
		`/\evil.test`,
		`/\evil.test/path`,
		`/\/evil.test`,
		`/\\evil.test`,
	} {
		_, err := svc.Begin(ctx, identity.BeginRequest{ReturnTo: returnTo})
		require.Errorf(t, err, "Begin accepted %q, which every WHATWG-conformant browser resolves off site", returnTo)
		assert.Equalf(t, errs.CodeValidationFailed, errs.CodeOf(err), "%q", returnTo)
	}

	// The comparison: the two shapes the existing suite tests ARE refused, and
	// an ordinary local path is still accepted, so this is refusing the
	// backslash rather than refusing everything.
	_, err := svc.Begin(ctx, identity.BeginRequest{ReturnTo: "//evil.test"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = svc.Begin(ctx, identity.BeginRequest{ReturnTo: "https://evil.test/x"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = svc.Begin(ctx, identity.BeginRequest{ReturnTo: "/portfolio"})
	assert.NoError(t, err, "an ordinary local path must still be accepted")
}

// F-177. PART 192 requires session rotation on privilege change and
// auth.Manager.Rotate is the mechanism for it -- with no caller anywhere in the
// repository. A step-up login (`/v1/auth/login?step_up=true`) is a privilege
// change: it raises the session's AuthTime and AMR, and it is the thing a user
// is asked to do before closing their account or registering a payout
// destination. identity.Complete called Sessions.Issue, so the pre-step-up
// session stayed live, kept its own full absolute lifetime, and was still a
// usable credential carrying the weaker authentication.
//
// The callback passes the session the browser already holds now, and a step-up
// by the same subject rotates it.
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
	// request or a payout destination. The browser sends the cookie it already
	// holds, and the callback hands the session it resolved to to Complete.
	begin2, err := svc.Begin(ctx, identity.BeginRequest{StepUp: true})
	require.NoError(t, err)
	current := first.Issued.Session
	second, err := svc.Complete(ctx, identity.CompleteRequest{
		Code: "security:mfa", State: begin2.State, UserAgent: "browser-1", Current: &current,
	})
	require.NoError(t, err)
	require.True(t, second.StepUp)
	require.Equal(t, first.User.ID, second.User.ID)
	require.NotEqual(t, first.Issued.Session.ID, second.Issued.Session.ID)

	// The session the step-up replaced is revoked, and the replacement records
	// what it replaced.
	var revoked *string
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT revoked_at::text FROM sessions WHERE id = $1`, first.Issued.Session.ID).Scan(&revoked))
	assert.NotNil(t, revoked, "the pre-step-up session was not revoked")
	assert.Equal(t, first.Issued.Session.ID, second.Issued.Session.RotatedFrom,
		"the new session does not record a rotation, so nothing links it to the one it replaced")

	// A rotation can never extend a login: the replacement keeps the absolute
	// expiry of the session it replaced.
	assert.Equal(t, first.Issued.Session.ExpiresAt.UTC(), second.Issued.Session.ExpiresAt.UTC(),
		"the rotated session was given a fresh absolute lifetime")

	// And the old token no longer authenticates.
	_, err = mgr.Authenticate(ctx, d.Pool(), first.Issued.Token)
	require.Error(t, err, "the old cookie still works after the step-up")
	assert.ErrorIs(t, err, auth.ErrSessionRevoked)

	// Which the user's own security page counts: one browser that stepped up
	// reports one active session, not two devices.
	var live int
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`,
		first.User.ID).Scan(&live))
	assert.Equal(t, 1, live,
		"GET /v1/me/security counts these, so one browser that stepped up must report one session")
}

// The other half of the same rule: a step-up with no session behind it is a cold
// sign-in and is issued a session rather than refused, and a step-up carrying
// somebody ELSE's session issues too -- rotation replaces the caller's own
// login, and must never be reachable as a way to end a stranger's.
func TestAudit_AColdStepUpIssuesAndAStrangersSessionIsNotRotated(t *testing.T) {
	svc, d, _ := newService(t)
	ctx := context.Background()

	begin, err := svc.Begin(ctx, identity.BeginRequest{StepUp: true})
	require.NoError(t, err)
	cold, err := svc.Complete(ctx, identity.CompleteRequest{Code: "security:mfa", State: begin.State})
	require.NoError(t, err)
	assert.Empty(t, cold.Issued.Session.RotatedFrom, "a cold step-up rotated from something")

	// A different person's live session, offered as the current one.
	other, err := svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	stranger, err := svc.Complete(ctx, identity.CompleteRequest{Code: "compliance", State: other.State})
	require.NoError(t, err)
	require.NotEqual(t, cold.User.ID, stranger.User.ID)

	begin3, err := svc.Begin(ctx, identity.BeginRequest{StepUp: true})
	require.NoError(t, err)
	strangerSession := stranger.Issued.Session
	again, err := svc.Complete(ctx, identity.CompleteRequest{
		Code: "security:mfa", State: begin3.State, Current: &strangerSession,
	})
	require.NoError(t, err)
	assert.Empty(t, again.Issued.Session.RotatedFrom)

	var revoked *string
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT revoked_at::text FROM sessions WHERE id = $1`, stranger.Issued.Session.ID).Scan(&revoked))
	assert.Nil(t, revoked, "a step-up ended a session belonging to somebody else")
}
