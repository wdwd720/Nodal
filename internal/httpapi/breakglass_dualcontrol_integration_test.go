//go:build integration

package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/security"
)

// The break-glass grant path, end to end, over HTTP.
//
// admin_dualcontrol_integration_test.go proves the refusals: a proposer
// cannot approve their own action, an approval does not travel to another
// record, a dead elevation grants nothing. Every one of those tests reaches
// its positive control by *fabricating* an elevated principal in Go, because
// until now nothing in the system could produce one. That is exactly the
// trap this file closes: the approve side of every two-person action is held
// only by BREAK_GLASS, and if no session can ever carry BREAK_GLASS then a
// SEVERE kill switch can be activated in one call and never released — the
// safety mechanism becomes the outage.
//
// So these tests fabricate nothing. Every principal here is a real session
// row, loaded by the real session middleware from a real cookie, and the
// only elevation in the file is the one a dual-controlled BREAK_GLASS_GRANT
// wrote onto a session.

// bgCookie is the session cookie name this harness issues and reads.
const bgCookie = "cp_session"

// bgOrigin is the first-party origin. A request that carries the session
// cookie must also prove it is first-party (Server.csrf), so every mutating
// call below sends it — as a browser would.
const bgOrigin = "https://app.test"

// liveHarness is the real router over the real admin service, the real
// kill-switch controller, the real Postgres session store and the real
// session middleware. Only the ports irrelevant to dual control are doubles.
//
// The clock is a fake anchored at the wall clock so the session store's own
// now() and the injected clock agree; the test then moves time forward
// explicitly to reach the elevation's deadline.
type liveHarness struct {
	t   *testing.T
	d   *db.DB
	clk *clock.Fake
	mgr *auth.Manager
	srv *Server
}

func newLiveHarness(t *testing.T, d *db.DB) *liveHarness {
	t.Helper()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))

	mgr, err := auth.NewManager(pgstore.New(), auth.ManagerConfig{
		// Long lifetimes: this suite moves the clock to reach a break-glass
		// deadline, and a session that went idle in the meantime would refuse
		// the request for the wrong reason.
		TTL: 24 * time.Hour, IdleTimeout: 12 * time.Hour, TouchInterval: time.Hour,
		Now: clk.Now,
	})
	require.NoError(t, err)

	adminSvc := admin.NewService(clk, audit.NewWriter())
	appender := NewAuditAppender(audit.NewWriter())
	ctl, err := killswitch.NewController(clk, appender.KillSwitchAudit(), NewApprovalVerifier(adminSvc))
	require.NoError(t, err)

	ports := newFixtures().ports()
	ports.AdminActions = adminActionsAdapter{
		svc: adminSvc, rm: NewReadModel(d), db: d, q: d,
		// The executor table this deployment actually registers. Anything not
		// in it still answers 422 UNSUPPORTED.
		executors: AdminExecutors(adminSvc, mgr, ctl),
	}
	ports.KillSwitches = killSwitchesAdapter{ctl: ctl, rm: NewReadModel(d), db: d}

	srv, err := New(Options{
		Env:           config.EnvTest,
		BuildVersion:  "test-build",
		ConfigHash:    "hash-1",
		PublicBaseURL: bgOrigin,
		CORSOrigins:   []string{bgOrigin},
		CookieName:    bgCookie,
		SessionTTL:    time.Hour,
		Clock:         clk,
		// The real thing: no principal is injected by the test. Authority
		// comes from the sessions table or not at all.
		Authenticator: httpmw.Session(mgr, d, bgCookie),
		Ports:         ports,
	})
	require.NoError(t, err)
	return &liveHarness{t: t, d: d, clk: clk, mgr: mgr, srv: srv}
}

// operator is a signed-in human: a users row, a persisted OPERATOR session
// and the raw cookie value that reaches it.
type operator struct {
	userID    string
	sessionID string
	token     string
}

// signIn creates the user and issues a real session with the given standing
// roles and a recent multi-factor sign-in. Roles are the caller's decision
// here exactly as they are in internal/identity: the identity provider never
// dictates them, and BREAK_GLASS is never among them.
func (h *liveHarness) signIn(roles ...security.Role) operator {
	h.t.Helper()
	for _, r := range roles {
		require.NotEqual(h.t, security.RoleBreakGlass, r, "no session is ever issued with a standing break-glass role")
	}
	ctx := h.t.Context()
	u, err := accounts.NewRepository().CreateUser(ctx, h.d,
		"httpapi-breakglass-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(h.t, err)

	var issued auth.Issued
	require.NoError(h.t, h.d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var ierr error
		issued, ierr = h.mgr.Issue(ctx, tx, auth.IssueParams{
			SubjectID: u.ID.String(), ActorType: security.ActorOperator,
			Roles: roles, AuthTime: h.clk.Now().Add(-time.Minute), AMR: []string{"pwd", "mfa"},
			IP: "203.0.113.9", UserAgent: "breakglass-itest", DeviceLabel: "laptop",
		})
		return ierr
	}))
	require.Nil(h.t, issued.Session.BreakGlassUntil, "a fresh session carries no elevation")
	return operator{userID: u.ID.String(), sessionID: issued.Session.ID, token: issued.Token}
}

// call makes one request as op. A nil op is anonymous.
func (h *liveHarness) call(op *operator, method, path string, body any) *response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(marshalJSON(body)))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Idempotency-Key", newKey())
	req.Header.Set("Origin", bgOrigin)
	if op != nil {
		req.AddCookie(&http.Cookie{Name: bgCookie, Value: op.token})
	}
	rec := httptest.NewRecorder()
	h.srv.Router().ServeHTTP(rec, req)
	return &response{ResponseRecorder: rec, t: h.t}
}

// propose creates a controlled action as op and returns it.
func (h *liveHarness) propose(op operator, kind admin.Kind, targetType, targetID string, params map[string]any, reason string) wireAction {
	h.t.Helper()
	body := map[string]any{
		"kind": string(kind), "target_type": targetType, "target_id": targetID, "reason": reason,
	}
	if params != nil {
		body["params"] = params
	}
	res := h.call(&op, http.MethodPost, "/v1/admin/actions", body)
	require.Equal(h.t, http.StatusCreated, res.Code, "propose %s; body=%s", kind, res.Body.String())
	var a wireAction
	res.json(&a)
	require.Equal(h.t, string(admin.StatusProposed), a.Status)
	return a
}

// decide posts one decision (approve, reject, execute) as op.
func (h *liveHarness) decide(op operator, actionID, decision, note string) *response {
	h.t.Helper()
	return h.call(&op, http.MethodPost, "/v1/admin/actions/"+actionID+"/"+decision,
		map[string]any{"note": note})
}

// storedElevation reads the break-glass columns of a session straight from
// the table, so an assertion about authority is made against the row the
// middleware will read and not against a response.
func (h *liveHarness) storedElevation(sessionID string) (until *time.Time, roles []string) {
	h.t.Helper()
	require.NoError(h.t, h.d.QueryRow(h.t.Context(),
		`SELECT break_glass_until, roles FROM sessions WHERE id = $1::uuid`, sessionID).
		Scan(&until, &roles))
	return until, roles
}

// switchActive reads a kill switch's current state from the table.
func (h *liveHarness) switchActive(kind killswitch.Kind, scope string) bool {
	h.t.Helper()
	var active bool
	require.NoError(h.t, h.d.QueryRow(h.t.Context(),
		`SELECT active FROM kill_switches WHERE kind = $1 AND scope_id = $2`, string(kind), scope).
		Scan(&active))
	return active
}

// TestIntegration_SevereKillSwitchIsReleasableThroughDualControl is the
// deliverable: the whole path, in one test, against a real database.
//
// A SEVERE switch is activated in a single unapproved call, as an incident
// demands. Releasing it needs kill:release, which no standing role holds, so
// the release is impossible until two other people grant a time-boxed
// break-glass elevation to a third — and that grant is itself a two-person
// action. Then the release is proposed, approved by a second principal, and
// executed.
//
// The refusals before the grant and the successes after it are the same
// requests by the same principals. Only the elevation changed.
func TestIntegration_SevereKillSwitchIsReleasableThroughDualControl(t *testing.T) {
	d := openTestDB(t)
	h := newLiveHarness(t, d)
	t0 := h.clk.Now()

	const kind = killswitch.ChainDisableNewActions
	require.Equal(t, killswitch.SeveritySevere, kind.Severity(),
		"this test needs the switch whose release demands an approval")
	scope := "chain-bg-" + id.New[id.Any]().String()

	// Three distinct humans. None of them can approve anything yet: no
	// standing role holds an approve-side permission, by design.
	requester := h.signIn(security.RoleAdmin)   // proposes; holds break_glass:request
	approver := h.signIn(security.RoleSecurity) // holds the standing break_glass:approve
	responder := h.signIn(security.RoleAdmin)   // the on-call operator to be elevated
	require.NotEqual(t, requester.userID, responder.userID)

	// --- the incident -------------------------------------------------------
	// Activation is the fast path: one operator, no approval, no step-up.
	act := h.call(&requester, http.MethodPost, "/v1/admin/kill-switches", map[string]any{
		"kind": string(kind), "scope_id": scope, "action": "activate",
		"reason": "halting chain activity for the break-glass verification",
	})
	require.Equal(t, http.StatusOK, act.Code, "activate; body=%s", act.Body.String())
	require.True(t, h.switchActive(kind, scope), "the switch must be stopping risk")

	// --- the trap, before anything is granted -------------------------------
	release := func(op operator, approvalID *string) *response {
		body := map[string]any{
			"kind": string(kind), "scope_id": scope, "action": "release",
			"reason": "chain is healthy again; resuming activity",
		}
		if approvalID != nil {
			body["approval_id"] = *approvalID
		}
		return h.call(&op, http.MethodPost, "/v1/admin/kill-switches", body)
	}

	t.Run("no standing role can release a SEVERE switch", func(t *testing.T) {
		for name, op := range map[string]operator{
			"ADMIN": requester, "SECURITY": approver, "the on-call operator": responder,
		} {
			res := release(op, nil)
			require.Equal(t, http.StatusForbidden, res.Code, "%s; body=%s", name, res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code, name)
		}
		assert.True(t, h.switchActive(kind, scope))
	})

	// The release is proposed first, so the refusal and the success below are
	// literally the same request against the same record.
	target := killswitch.ReleaseTargetID(kind, scope)
	releaseAction := h.propose(requester, admin.KindKillSwitchRelease, "kill_switch", target,
		nil, "chain incident resolved; requesting release")

	t.Run("and nobody can approve the release either", func(t *testing.T) {
		res := h.decide(responder, releaseAction.ID, "approve", "chain looks healthy to me")
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code)
		status, approvedBy := storedStatus(t, d, releaseAction.ID)
		assert.Equal(t, string(admin.StatusProposed), status, "a refused approval must not move the record")
		assert.Nil(t, approvedBy)
	})

	// --- the break-glass grant, itself under dual control -------------------
	const grantSeconds = 300
	grantAction := h.propose(requester, admin.KindBreakGlassGrant, "user", responder.userID,
		map[string]any{
			"user_id":          responder.userID,
			"scope":            "INC-" + scope + " release " + string(kind),
			"duration_seconds": grantSeconds,
		},
		"on-call operator needs break glass to release the chain switch")

	t.Run("the requester cannot approve their own elevation request", func(t *testing.T) {
		res := h.decide(requester, grantAction.ID, "approve", "I asked for this and I am approving it")
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.problem().Detail, "proposer")
	})

	t.Run("nor can the grantee approve their own elevation", func(t *testing.T) {
		res := h.decide(responder, grantAction.ID, "approve", "I would like to be elevated")
		require.Equal(t, http.StatusForbidden, res.Code,
			"an ADMIN holds break_glass:approve, so this must be refused for a reason other than authority; body=%s",
			res.Body.String())
		// It is refused because approving your own elevation is the whole
		// bypass: the second person is the control.
		assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	})

	t.Run("the elevation is granted by a second, distinct principal", func(t *testing.T) {
		res := h.decide(approver, grantAction.ID, "approve", "confirmed with the incident lead")
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		status, approvedBy := storedStatus(t, d, grantAction.ID)
		assert.Equal(t, string(admin.StatusApproved), status)
		require.NotNil(t, approvedBy)
		assert.Equal(t, approver.userID, *approvedBy)
	})

	t.Run("approving is not granting: the elevation exists only once executed", func(t *testing.T) {
		until, roles := h.storedElevation(responder.sessionID)
		assert.Nil(t, until, "an approved grant has not been applied yet")
		assert.NotContains(t, roles, string(security.RoleBreakGlass))
	})

	t.Run("executing the grant writes the elevation onto the grantee's session", func(t *testing.T) {
		res := h.decide(requester, grantAction.ID, "execute", "applying the approved elevation")
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		status, _ := storedStatus(t, d, grantAction.ID)
		assert.Equal(t, string(admin.StatusExecuted), status)

		until, roles := h.storedElevation(responder.sessionID)
		require.NotNil(t, until, "the grantee's session must carry the deadline")
		assert.True(t, t0.Add(grantSeconds*time.Second).Equal(*until), "granted until %s", until)
		assert.Contains(t, roles, string(security.RoleBreakGlass))
		assert.Contains(t, roles, string(security.RoleAdmin), "standing roles are untouched")

		// Nobody else was elevated by it.
		for name, op := range map[string]operator{"the requester": requester, "the approver": approver} {
			other, otherRoles := h.storedElevation(op.sessionID)
			assert.Nil(t, other, "%s must not be elevated by someone else's grant", name)
			assert.NotContains(t, otherRoles, string(security.RoleBreakGlass), name)
		}
	})

	// --- release: propose → approve by a second principal → execute ---------
	t.Run("the elevated operator can now approve the release", func(t *testing.T) {
		res := h.decide(responder, releaseAction.ID, "approve", "chain is healthy again; agree to release")
		require.Equal(t, http.StatusOK, res.Code,
			"the same request that was refused before the grant; body=%s", res.Body.String())
		status, approvedBy := storedStatus(t, d, releaseAction.ID)
		assert.Equal(t, string(admin.StatusApproved), status)
		require.NotNil(t, approvedBy)
		assert.Equal(t, responder.userID, *approvedBy)
		assert.NotEqual(t, requester.userID, *approvedBy, "approver and proposer are different humans")
	})

	t.Run("an approved release still cannot be executed without a live elevation", func(t *testing.T) {
		res := h.decide(requester, releaseAction.ID, "execute", "applying the approved release")
		require.Equal(t, http.StatusForbidden, res.Code,
			"the proposer holds kill:activate, not kill:release; body=%s", res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code)
		status, _ := storedStatus(t, d, releaseAction.ID)
		assert.Equal(t, string(admin.StatusApproved), status, "a failed execution must not consume the approval")
		assert.True(t, h.switchActive(kind, scope), "and must not release anything")
	})

	t.Run("the elevated operator executes it and the switch is released", func(t *testing.T) {
		res := h.decide(responder, releaseAction.ID, "execute", "applying the approved release")
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		status, _ := storedStatus(t, d, releaseAction.ID)
		assert.Equal(t, string(admin.StatusExecuted), status)
		assert.False(t, h.switchActive(kind, scope), "the SEVERE switch is released")

		// The switch records which approval released it, and who.
		var releasedBy, approvalID string
		require.NoError(t, d.QueryRow(t.Context(),
			`SELECT coalesce(released_by_actor_id,''), coalesce(release_approval_id::text,'')
			   FROM kill_switches WHERE kind = $1 AND scope_id = $2`, string(kind), scope).
			Scan(&releasedBy, &approvalID))
		assert.Equal(t, responder.userID, releasedBy)
		assert.Equal(t, releaseAction.ID, approvalID)
	})

	t.Run("the grant and the release are both on the admin audit stream", func(t *testing.T) {
		var granted int
		require.NoError(t, d.QueryRow(t.Context(),
			`SELECT count(*) FROM audit_events
			  WHERE stream = 'admin' AND action = $1 AND resource_id = $2 AND evidence_ref = $3`,
			admin.AuditBreakGlassGranted, responder.userID, grantAction.ID).Scan(&granted))
		assert.Equal(t, 1, granted, "the elevation is recorded against the action that authorized it")

		var released int
		require.NoError(t, d.QueryRow(t.Context(),
			`SELECT count(*) FROM audit_events
			  WHERE stream = 'admin' AND action = 'kill_switch.release' AND evidence_ref = $1`,
			releaseAction.ID).Scan(&released))
		assert.Equal(t, 1, released, "the release cites its approval")

		rep, err := audit.NewVerifier().VerifyStream(t.Context(), d, audit.AdminStream)
		require.NoError(t, err)
		assert.True(t, rep.OK, rep.Reason)
	})

	// --- the elevation dies of the clock ------------------------------------
	t.Run("at the deadline the elevation grants nothing, and the row is unchanged", func(t *testing.T) {
		// A second switch, so the refusal is about authority and not about a
		// switch that is already released.
		second := "chain-bg-" + id.New[id.Any]().String()
		act := h.call(&requester, http.MethodPost, "/v1/admin/kill-switches", map[string]any{
			"kind": string(kind), "scope_id": second, "action": "activate",
			"reason": "halting chain activity a second time for the expiry check",
		})
		require.Equal(t, http.StatusOK, act.Code, "body=%s", act.Body.String())
		next := h.propose(requester, admin.KindKillSwitchRelease, "kill_switch",
			killswitch.ReleaseTargetID(kind, second), nil, "second chain incident resolved")

		// Exactly the deadline: expiry is exclusive, so the elevation is
		// already gone at the instant it expires.
		h.clk.Set(t0.Add(grantSeconds * time.Second))

		res := h.decide(responder, next.ID, "approve", "approving on an elevation that has just expired")
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code,
			"a dead elevation must be FORBIDDEN, not a step-up prompt")

		status, approvedBy := storedStatus(t, d, next.ID)
		assert.Equal(t, string(admin.StatusProposed), status)
		assert.Nil(t, approvedBy)
		assert.True(t, h.switchActive(kind, second))

		// The session still carries the role and the deadline: nothing
		// rewrites a session when an elevation lapses, which is precisely why
		// liveness has to be judged against the clock at every use.
		until, roles := h.storedElevation(responder.sessionID)
		require.NotNil(t, until)
		assert.Contains(t, roles, string(security.RoleBreakGlass))

		// One second before the deadline the same request would have worked.
		h.clk.Set(t0.Add(grantSeconds*time.Second - time.Second))
		ok := h.decide(responder, next.ID, "approve", "approving one second before the deadline")
		require.Equal(t, http.StatusOK, ok.Code,
			"a live elevation must still work, or the refusal above proves nothing; body=%s", ok.Body.String())
	})

	t.Run("revoking the session takes the elevation with it", func(t *testing.T) {
		require.NoError(t, h.mgr.Revoke(t.Context(), d, responder.sessionID))
		third := "chain-bg-" + id.New[id.Any]().String()
		act := h.call(&requester, http.MethodPost, "/v1/admin/kill-switches", map[string]any{
			"kind": string(kind), "scope_id": third, "action": "activate",
			"reason": "halting chain activity for the revocation check",
		})
		require.Equal(t, http.StatusOK, act.Code, "body=%s", act.Body.String())
		next := h.propose(requester, admin.KindKillSwitchRelease, "kill_switch",
			killswitch.ReleaseTargetID(kind, third), nil, "third chain incident resolved")

		res := h.decide(responder, next.ID, "approve", "approving on a revoked session")
		require.Equal(t, http.StatusUnauthorized, res.Code,
			"a revoked session is not an elevated one; body=%s", res.Body.String())
		status, _ := storedStatus(t, d, next.ID)
		assert.Equal(t, string(admin.StatusProposed), status)
		assert.True(t, h.switchActive(kind, third))
	})
}

// TestIntegration_BreakGlassGrantRefusesTheBypasses is the other half: the
// grant path must not become a way around the very control it exists to
// serve.
func TestIntegration_BreakGlassGrantRefusesTheBypasses(t *testing.T) {
	d := openTestDB(t)
	h := newLiveHarness(t, d)

	requester := h.signIn(security.RoleAdmin)
	approver := h.signIn(security.RoleSecurity)
	grantee := h.signIn(security.RoleOperations)

	t.Run("an elevation longer than the ceiling is refused at proposal", func(t *testing.T) {
		res := h.call(&requester, http.MethodPost, "/v1/admin/actions", map[string]any{
			"kind": string(admin.KindBreakGlassGrant), "target_type": "user", "target_id": grantee.userID,
			"params": map[string]any{
				"user_id": grantee.userID, "scope": "INC-too-long",
				"duration_seconds": int64(admin.MaxBreakGlassDuration/time.Second) + 1,
			},
			"reason": "asking for more than the maximum elevation",
		})
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	})

	t.Run("the storage ceiling equals the proposal ceiling", func(t *testing.T) {
		// The two are declared in packages that do not import each other, so
		// a drift would silently let internal/auth write an elevation longer
		// than internal/admin believes it validated.
		assert.Equal(t, admin.MaxBreakGlassDuration, auth.MaxBreakGlassElevation)
	})

	t.Run("a grantee with no live session fails the grant rather than faking it", func(t *testing.T) {
		absent := h.signIn(security.RoleOperations)
		require.NoError(t, h.mgr.Revoke(t.Context(), d, absent.sessionID))

		action := h.propose(requester, admin.KindBreakGlassGrant, "user", absent.userID,
			map[string]any{"user_id": absent.userID, "scope": "INC-absent", "duration_seconds": 300},
			"elevating an operator who is not signed in")
		require.Equal(t, http.StatusOK,
			h.decide(approver, action.ID, "approve", "confirmed with the incident lead").Code)

		res := h.decide(requester, action.ID, "execute", "applying the approved elevation")
		require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.problem().Detail, "no live operator session")

		// The failed execution is rolled back with the transaction, so the
		// approval is still there to be executed once they sign in.
		status, _ := storedStatus(t, d, action.ID)
		assert.Equal(t, string(admin.StatusApproved), status)
		until, _ := h.storedElevation(absent.sessionID)
		assert.Nil(t, until)
	})

	t.Run("a grant elevates the user it names, not the one who executes it", func(t *testing.T) {
		action := h.propose(requester, admin.KindBreakGlassGrant, "user", grantee.userID,
			map[string]any{"user_id": grantee.userID, "scope": "INC-named", "duration_seconds": 300},
			"elevating the named on-call operator")
		require.Equal(t, http.StatusOK,
			h.decide(approver, action.ID, "approve", "confirmed with the incident lead").Code)
		require.Equal(t, http.StatusOK,
			h.decide(requester, action.ID, "execute", "applying the approved elevation").Code)

		granted, _ := h.storedElevation(grantee.sessionID)
		assert.NotNil(t, granted, "the named grantee is elevated")
		executor, roles := h.storedElevation(requester.sessionID)
		assert.Nil(t, executor, "the executor is not")
		assert.NotContains(t, roles, string(security.RoleBreakGlass))
	})

	t.Run("an agent is refused by the domain, not only by the boundary", func(t *testing.T) {
		agent := security.AgentPrincipal(id.New[id.Any]().String(), testAccountID.String())
		ctx := security.WithPrincipal(t.Context(), agent)
		port := h.srv.opts.Ports.AdminActions
		_, err := port.Propose(ctx, admin.Proposal{
			Kind: admin.KindBreakGlassGrant, TargetType: "user", TargetID: grantee.userID,
			Reason: "an agent must never elevate anyone",
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})
}
