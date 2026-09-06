//go:build integration

package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/security"
)

// Dual control, proved over HTTP against the real internal/admin service and a
// real database.
//
// The rest of this package's suite runs against in-memory doubles, which is
// right for testing the boundary — but a double would answer "approved" to a
// proposer approving their own action, so none of the properties below can be
// proved there. The other direction is also true: internal/admin's own suite
// proves the refusals, but it calls Go functions, and an attacker sends HTTP
// requests. What is only provable here is that the two layers compose: that
// the boundary's deliberately coarse route floor is actually backstopped, that
// the refusal survives the adapter and the transaction, and that it reaches the
// wire as the right problem+json code rather than a 500.
//
// Each test carries its own positive control — the same request, made by a
// principal who should succeed, succeeding. Without that a refusal proves
// nothing: a test that only ever sees 403 passes just as well against a route
// that is broken for everyone.

// newKey returns a fresh Idempotency-Key. A UUID satisfies the header's
// charset and length contract by construction.
func newKey() string { return id.New[id.Any]().String() }

// newDualControlHarness builds the real router over the real admin service,
// the real kill-switch controller and a real database. Only the ports that are
// irrelevant to dual control stay doubles.
func newDualControlHarness(t *testing.T, d *db.DB) *harness {
	t.Helper()
	fx := newFixtures()
	clk := clock.NewFake(testNow)

	adminSvc := admin.NewService(clk, audit.NewWriter())
	appender := NewAuditAppender(audit.NewWriter())
	ctl, err := killswitch.NewController(clk, appender.KillSwitchAudit(), NewApprovalVerifier(adminSvc))
	require.NoError(t, err)

	ports := fx.ports()
	// executors is deliberately nil: this deployment can approve and reject
	// over the API but not execute, and the handler says so with 422
	// UNSUPPORTED rather than inventing an effect.
	ports.AdminActions = adminActionsAdapter{svc: adminSvc, rm: NewReadModel(d), db: d, q: d}
	ports.KillSwitches = killSwitchesAdapter{ctl: ctl, rm: NewReadModel(d), db: d}

	h := &harness{t: t, ports: fx}
	srv, err := New(Options{
		Env:           config.EnvTest,
		BuildVersion:  "test-build",
		ConfigHash:    "hash-1",
		PublicBaseURL: "https://app.test",
		CORSOrigins:   []string{"https://app.test"},
		CookieName:    "cp_session",
		SessionTTL:    time.Hour,
		Clock:         clk,
		Authenticator: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if h.princip == nil {
					next.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), *h.princip)))
			})
		},
		Ports: ports,
	})
	require.NoError(t, err)
	h.server = srv
	return h
}

// seedOperator inserts a users row and returns an operator principal for it
// with a recent multi-factor sign-in. A real row is required: admin_actions
// references users(id), and the proposer/approver columns are what the
// database's own dual-control CHECK compares.
func seedOperator(t *testing.T, d *db.DB, roles ...security.Role) security.Principal {
	t.Helper()
	u, err := accounts.NewRepository().CreateUser(t.Context(), d,
		"httpapi-dualcontrol-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	return security.Principal{
		SubjectID: u.ID.String(),
		ActorType: security.ActorOperator,
		Roles:     roles,
		SessionID: testSessionID,
		AuthTime:  testNow.Add(-time.Minute),
		AMR:       []string{"pwd", "mfa"},
	}
}

// elevate returns a copy of p carrying a break-glass elevation until `until`.
// No standing role holds an approve-side permission, so this is the only way
// any principal can approve anything.
func elevate(p security.Principal, until time.Time) security.Principal {
	c := p
	c.Roles = append(append([]security.Role(nil), p.Roles...), security.RoleBreakGlass)
	c.BreakGlassUntil = &until
	return c
}

// wireAction is the part of the API's AdminAction these tests read.
type wireAction struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	ProposedBy string  `json:"proposed_by"`
	ApprovedBy *string `json:"approved_by"`
}

// propose creates a controlled action over HTTP and returns it.
func propose(t *testing.T, h *harness, kind admin.Kind, targetType, targetID string) wireAction {
	t.Helper()
	res := h.do(http.MethodPost, "/v1/admin/actions", map[string]any{
		"kind":        string(kind),
		"target_type": targetType,
		"target_id":   targetID,
		"reason":      "stage 15 dual-control verification",
	}, "Idempotency-Key", newKey())
	require.Equal(t, http.StatusCreated, res.Code, "propose failed; body=%s", res.Body.String())
	var a wireAction
	res.json(&a)
	require.NotEmpty(t, a.ID)
	require.Equal(t, string(admin.StatusProposed), a.Status)
	return a
}

// decide posts one decision on a stored action.
func decide(h *harness, actionID, decision, note string) *response {
	return h.do(http.MethodPost, "/v1/admin/actions/"+actionID+"/"+decision,
		map[string]any{"note": note}, "Idempotency-Key", newKey())
}

// storedStatus reads the row straight from the database, so an assertion that
// nothing moved is made against the record rather than against a response.
func storedStatus(t *testing.T, d *db.DB, actionID string) (status string, approvedBy *string) {
	t.Helper()
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT status, approved_by_user_id::text FROM admin_actions WHERE id = $1`, actionID).
		Scan(&status, &approvedBy))
	return status, approvedBy
}

// TestIntegration_ProposerCannotApproveTheirOwnActionOverHTTP is the central
// property of the whole stage.
//
// The proposer here is not under-privileged: they are an ADMIN holding a live
// break-glass elevation, so they hold gate:propose *and* gate:approve — every
// permission the approval needs. Nothing is left to refuse them except that
// they are the proposer, which is exactly the bypass a careless operator tries
// when they are the only person on call.
func TestIntegration_ProposerCannotApproveTheirOwnActionOverHTTP(t *testing.T) {
	d := openTestDB(t)
	h := newDualControlHarness(t, d)

	proposer := elevate(seedOperator(t, d, security.RoleAdmin), testNow.Add(time.Hour))
	h.as(&proposer)
	action := propose(t, h, admin.KindCapabilityGateApprove, "capability_gate", "LIVE_FUNDING")

	res := decide(h, action.ID, "approve", "I proposed this and I am approving it")
	require.Equal(t, http.StatusForbidden, res.Code,
		"the proposer must not be able to approve their own action; body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeForbidden, p.Code)
	assert.Contains(t, p.Detail, "proposer",
		"the refusal must say why, or an operator will retry it forever")

	status, approvedBy := storedStatus(t, d, action.ID)
	assert.Equal(t, string(admin.StatusProposed), status, "a refused approval must not move the record")
	assert.Nil(t, approvedBy)

	// Positive control: a *different* principal, holding exactly the same
	// permissions, is allowed. The refusal above was about identity, not
	// authority, and the route is not simply broken.
	approver := elevate(seedOperator(t, d, security.RoleAdmin), testNow.Add(time.Hour))
	h.as(&approver)
	ok := decide(h, action.ID, "approve", "reviewed the gate evidence and agree")
	require.Equal(t, http.StatusOK, ok.Code, "body=%s", ok.Body.String())

	status, approvedBy = storedStatus(t, d, action.ID)
	assert.Equal(t, string(admin.StatusApproved), status)
	require.NotNil(t, approvedBy)
	assert.Equal(t, approver.SubjectID, *approvedBy)
	assert.NotEqual(t, proposer.SubjectID, *approvedBy)
}

// TestIntegration_ApproveRefusesAPrincipalLackingTheApprovePermission closes
// the gap the boundary deliberately leaves open.
//
// PostAdminActionsActionIdDecision's route floor is the union of the propose-
// and approve-side permissions, because reject and execute are available to
// either side; a floor of approve-side permissions alone would lock the route
// for everyone, since no standing role holds one. So an ADMIN holding only
// gate:propose *does* reach the approve path — authorization at the edge says
// yes. The only thing that refuses the approval is internal/admin checking the
// kind's own approve permission, and this proves that check is really there,
// through the adapter and the transaction, and arrives as FORBIDDEN.
func TestIntegration_ApproveRefusesAPrincipalLackingTheApprovePermission(t *testing.T) {
	d := openTestDB(t)
	h := newDualControlHarness(t, d)

	proposer := seedOperator(t, d, security.RoleAdmin)
	h.as(&proposer)
	action := propose(t, h, admin.KindCapabilityGateApprove, "capability_gate", "LIVE_FUNDING")

	// A distinct human, so self-approval is not what refuses this. An ADMIN
	// holds every permission except the seven approve-side ones.
	approver := seedOperator(t, d, security.RoleAdmin)
	require.NotEqual(t, proposer.SubjectID, approver.SubjectID)
	require.True(t, approver.Has(security.PermGatePropose, testNow),
		"the approver must reach the route, or this tests the wrong refusal")
	require.False(t, approver.Has(security.PermGateApprove, testNow),
		"no standing role may hold an approve-side permission")

	h.as(&approver)
	res := decide(h, action.ID, "approve", "approving without the approve permission")
	require.Equal(t, http.StatusForbidden, res.Code,
		"a principal without the kind's approve permission must be refused; body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	status, approvedBy := storedStatus(t, d, action.ID)
	assert.Equal(t, string(admin.StatusProposed), status)
	assert.Nil(t, approvedBy)

	// Positive control: the same person, same session, same request — with a
	// live elevation. Only the permission changed.
	elevated := elevate(approver, testNow.Add(time.Hour))
	h.as(&elevated)
	ok := decide(h, action.ID, "approve", "reviewed the gate evidence and agree")
	require.Equal(t, http.StatusOK, ok.Code, "body=%s", ok.Body.String())
	status, _ = storedStatus(t, d, action.ID)
	assert.Equal(t, string(admin.StatusApproved), status)
}

// TestIntegration_ExpiredBreakGlassElevationIsNotHonoured proves the elevation
// is judged against the clock at the moment of use, not at the moment it was
// granted.
//
// A session that carried a live elevation keeps carrying the BREAK_GLASS role
// after the deadline passes — the role is in the token, and nothing rewrites
// it. If liveness were not re-checked on every use, an operator (or anyone
// holding their session) would keep approve-side authority indefinitely after
// a four-hour grant expired. That is the difference between a time-boxed
// elevation and a permanent one.
func TestIntegration_ExpiredBreakGlassElevationIsNotHonoured(t *testing.T) {
	d := openTestDB(t)
	h := newDualControlHarness(t, d)

	proposer := seedOperator(t, d, security.RoleAdmin)
	h.as(&proposer)
	action := propose(t, h, admin.KindCapabilityGateApprove, "capability_gate", "LIVE_FUNDING")

	approver := seedOperator(t, d, security.RoleAdmin)

	for name, until := range map[string]time.Time{
		// One second past the deadline, and the deadline itself: expiry is
		// exclusive, so holding the elevation "until T" does not include T.
		"expired an hour ago":    testNow.Add(-time.Hour),
		"expired one second ago": testNow.Add(-time.Second),
		"expires exactly now":    testNow,
	} {
		t.Run(name, func(t *testing.T) {
			stale := elevate(approver, until)
			require.False(t, stale.Has(security.PermGateApprove, testNow),
				"a dead elevation must grant nothing")

			h.as(&stale)
			res := decide(h, action.ID, "approve", "approving on a dead elevation")
			require.Equal(t, http.StatusForbidden, res.Code,
				"an expired elevation must not be honored; body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			status, approvedBy := storedStatus(t, d, action.ID)
			assert.Equal(t, string(admin.StatusProposed), status)
			assert.Nil(t, approvedBy)
		})
	}

	// Positive control: the same principal, one second before the deadline.
	live := elevate(approver, testNow.Add(time.Second))
	h.as(&live)
	ok := decide(h, action.ID, "approve", "reviewed the gate evidence and agree")
	require.Equal(t, http.StatusOK, ok.Code,
		"a live elevation must still work, or the test above proves nothing; body=%s", ok.Body.String())
	status, _ := storedStatus(t, d, action.ID)
	assert.Equal(t, string(admin.StatusApproved), status)
}

// TestIntegration_ReleaseApprovalCannotBeReplayedAgainstAnotherSwitch is the
// approval-replay attack in the shape it actually takes.
//
// An approval is not a bearer token that means "some two people agreed to
// something". It names one kind and one target, and the domain that consumes
// it re-checks both. The realistic misuse is not forging an approval — it is
// quoting a genuine one, obtained honestly for switch A, while releasing
// switch B: same kind, same approvers, same freshness, wrong record. Scopes
// are freshly generated per run so the suite can be run repeatedly against one
// database.
func TestIntegration_ReleaseApprovalCannotBeReplayedAgainstAnotherSwitch(t *testing.T) {
	d := openTestDB(t)
	h := newDualControlHarness(t, d)

	const kind = killswitch.ChainDisableNewActions
	require.Equal(t, killswitch.SeveritySevere, kind.Severity(),
		"this test needs a SEVERE switch, whose release is the one that demands an approval")

	approved := "chain-approved-" + id.New[id.Any]().String()
	other := "chain-other-" + id.New[id.Any]().String()

	// One operator stops risk on two chains. Activation is single-control and
	// needs no step-up: stopping new risk must never wait.
	proposer := seedOperator(t, d, security.RoleAdmin)
	h.as(&proposer)
	for _, scope := range []string{approved, other} {
		res := h.do(http.MethodPost, "/v1/admin/kill-switches", map[string]any{
			"kind": string(kind), "scope_id": scope, "action": "activate",
			"reason": "halting chain activity for stage 15 verification",
		}, "Idempotency-Key", newKey())
		require.Equal(t, http.StatusOK, res.Code, "activate %s; body=%s", scope, res.Body.String())
	}

	// A release approval is obtained honestly, for one switch only.
	action := propose(t, h, admin.KindKillSwitchRelease, "kill_switch",
		killswitch.ReleaseTargetID(kind, approved))

	releaser := elevate(seedOperator(t, d, security.RoleAdmin), testNow.Add(time.Hour))
	h.as(&releaser)
	ok := decide(h, action.ID, "approve", "chain is healthy again; agree to release")
	require.Equal(t, http.StatusOK, ok.Code, "body=%s", ok.Body.String())

	release := func(scope string, approvalID *string) *response {
		body := map[string]any{
			"kind": string(kind), "scope_id": scope, "action": "release",
			"reason": "resuming chain activity after verification",
		}
		if approvalID != nil {
			body["approval_id"] = *approvalID
		}
		return h.do(http.MethodPost, "/v1/admin/kill-switches", body, "Idempotency-Key", newKey())
	}

	t.Run("the approval does not travel to another switch", func(t *testing.T) {
		res := release(other, &action.ID)
		require.Equal(t, http.StatusForbidden, res.Code,
			"an approval for one switch must not release another; body=%s", res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeForbidden, p.Code)
		// The refusal comes from admin.VerifyApproved, which compares the
		// stored kind and target_id before returning anything. killswitch's own
		// verifyApproval re-checks the same two fields on the value it gets
		// back, so there are two independent refusals here — but the first one
		// wins, and it is the first one an operator sees. Asserting
		// killswitch's wording instead would pass only if the verifier had
		// become lenient, which is precisely the regression this guards.
		assert.Contains(t, p.Detail, "does not cover this kind and target")
	})

	t.Run("a SEVERE switch cannot be released with no approval at all", func(t *testing.T) {
		res := release(other, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	})

	t.Run("the switch it does name is released", func(t *testing.T) {
		res := release(approved, &action.ID)
		require.Equal(t, http.StatusOK, res.Code,
			"the approval must work for its own switch, or the refusals above prove nothing; body=%s",
			res.Body.String())
		var sw struct {
			Active bool `json:"active"`
		}
		res.json(&sw)
		assert.False(t, sw.Active)
	})

	t.Run("the other switch is still active", func(t *testing.T) {
		var active bool
		require.NoError(t, d.QueryRow(t.Context(),
			`SELECT active FROM kill_switches WHERE kind = $1 AND scope_id = $2`,
			string(kind), other).Scan(&active))
		assert.True(t, active, "the switch the approval did not name must still be stopping risk")
	})
}

// TestIntegration_AgentPrincipalIsRefusedByTheDomainNotOnlyTheBoundary is
// defense in depth for the containment rule.
//
// authz_test.go already proves the boundary refuses every AGENT principal on
// every route. That check is one `if` in one middleware, and an agent that
// somehow reached the handler — a future route with AllowAgent set, a
// refactor that reorders the middleware — must still be refused. internal/admin
// rejects agents before any query, and this proves that second refusal is real
// by calling the port directly with an agent in the context, bypassing the
// boundary exactly as a mistake there would.
func TestIntegration_AgentPrincipalIsRefusedByTheDomainNotOnlyTheBoundary(t *testing.T) {
	d := openTestDB(t)
	h := newDualControlHarness(t, d)

	proposer := seedOperator(t, d, security.RoleAdmin)
	h.as(&proposer)
	action := propose(t, h, admin.KindCapabilityGateApprove, "capability_gate", "LIVE_FUNDING")

	agent := security.AgentPrincipal(id.New[id.Any]().String(), testAccountID.String())
	ctx := security.WithPrincipal(t.Context(), agent)
	port := h.server.opts.Ports.AdminActions

	t.Run("propose", func(t *testing.T) {
		_, err := port.Propose(ctx, admin.Proposal{
			Kind: admin.KindCapabilityGateApprove, TargetType: "capability_gate",
			TargetID: "LIVE_FUNDING", Reason: "an agent should never get here",
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	t.Run("approve", func(t *testing.T) {
		_, err := port.Approve(ctx, action.ID, "an agent should never get here")
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	t.Run("reject", func(t *testing.T) {
		_, err := port.Reject(ctx, action.ID, "an agent should never get here")
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	status, approvedBy := storedStatus(t, d, action.ID)
	assert.Equal(t, string(admin.StatusProposed), status)
	assert.Nil(t, approvedBy)
}

// TestIntegration_UnexecutableKindAnswersUnsupported: this deployment
// registers no admin executors, and an approved action it cannot execute must
// say so as 422 UNSUPPORTED rather than reporting a success that never
// happened or a 500. The console shows that verbatim.
func TestIntegration_UnexecutableKindAnswersUnsupported(t *testing.T) {
	d := openTestDB(t)
	h := newDualControlHarness(t, d)

	proposer := seedOperator(t, d, security.RoleAdmin)
	h.as(&proposer)
	action := propose(t, h, admin.KindCapabilityGateApprove, "capability_gate", "LIVE_FUNDING")

	approver := elevate(seedOperator(t, d, security.RoleAdmin), testNow.Add(time.Hour))
	h.as(&approver)
	require.Equal(t, http.StatusOK, decide(h, action.ID, "approve", "reviewed and agree").Code)

	res := decide(h, action.ID, "execute", "applying the approved change")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)

	status, _ := storedStatus(t, d, action.ID)
	assert.Equal(t, string(admin.StatusApproved), status,
		"an action that could not be executed stays approved")
}
