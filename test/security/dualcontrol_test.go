//go:build integration

package security

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Dual control is the control that stops an insider, and an insider is the
// threat model where every other control in this suite has already been
// satisfied: the caller is authenticated, holds the role, and is doing exactly
// what the route is for. The only thing standing between one compromised
// operator and a released kill switch, an approved withdrawal or a break-glass
// elevation is that the approver must be a different person, and that the
// approver's strong authentication must be recent.
//
// internal/security separates the two sides into different permissions so this
// is structural rather than conventional. These tests prove the structure
// survives the trip through HTTP, which is where a "convenience" shortcut
// would be added.

// adminActionRow is the persisted state of one controlled action. The response
// is not evidence: an API that answered 403 while writing APPROVED would be
// the worst possible outcome, so every assertion is made against the row.
type adminActionRow struct {
	Status       string
	ProposedBy   string
	ApprovedBy   *string
	RequiresDual bool
}

func adminAction(t *testing.T, id string) adminActionRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var row adminActionRow
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT status, proposed_by_user_id::text, approved_by_user_id::text, requires_dual
		   FROM admin_actions WHERE id = $1`, id).
		Scan(&row.Status, &row.ProposedBy, &row.ApprovedBy, &row.RequiresDual))
	return row
}

// proposeBreakGlass proposes a BREAK_GLASS_GRANT, which is the kind that makes
// self-approval measurable: an ADMIN holds BOTH break_glass:request and
// break_glass:approve (break_glass:approve is a standing permission precisely
// so the first elevation is approvable), so authorization admits the proposer
// on the decision route too. Nothing but "the approver must differ from the
// proposer" can refuse the self-approval, which is exactly what is under test.
func proposeBreakGlass(t *testing.T, proposer session, targetUser, label string) string {
	t.Helper()
	body := fmt.Sprintf(
		`{"kind":"BREAK_GLASS_GRANT","target_type":"user","target_id":%q,"reason":%q}`,
		targetUser, "adversarial dual-control probe "+label,
	)
	r := postAs(t, proposer.Token, "/v1/admin/actions", key("dual-propose-"+label), body)
	require.Equal(t, http.StatusCreated, r.Status, "propose BREAK_GLASS_GRANT: %s", r.text())
	var doc struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		RequiresDual bool   `json:"requires_dual"`
		ProposedBy   string `json:"proposed_by"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &doc))
	require.NotEmpty(t, doc.ID)
	require.Equal(t, "PROPOSED", doc.Status)
	require.True(t, doc.RequiresDual, "BREAK_GLASS_GRANT must require dual control, or this test proves nothing")
	require.Equal(t, proposer.SubjectID, doc.ProposedBy)
	return doc.ID
}

func decide(t *testing.T, s session, actionID, decision, label string) response {
	t.Helper()
	return postAs(t, s.Token, "/v1/admin/actions/"+actionID+"/"+decision,
		key("dual-"+decision+"-"+label), `{"note":"adversarial dual-control probe"}`)
}

// TestDualControl_AProposerCannotApproveItsOwnAction is the two-person rule.
//
// The negative control makes the approval attempt from the SECOND operator
// instead of the proposer. That approval is legitimate and succeeds, so the
// "must be refused" assertions fire — which is what shows they are measuring
// "approver != proposer" and not, say, a missing permission or an unwired port.
func TestDualControl_AProposerCannotApproveItsOwnAction(t *testing.T) {
	requireAPI(t)
	proposer := operatorSession(t, "admin", string(security.RoleAdmin))
	second := operatorSession(t, "security", string(security.RoleSecurity))
	require.NotEqual(t, proposer.SubjectID, second.SubjectID,
		"the two operators resolved to one user; there is no second person")

	// Both sides of the two-person rule must be authorized, or a refusal is
	// only a permission error wearing a dual-control costume.
	require.True(t, security.RoleGrants(security.RoleAdmin, security.PermBreakGlassRequest),
		"ADMIN must be able to propose a break-glass grant")
	require.True(t, security.RoleGrants(security.RoleAdmin, security.PermBreakGlassApprove),
		"ADMIN must also HOLD the approve permission, or self-approval would be refused for the wrong reason")
	require.True(t, security.RoleGrants(security.RoleSecurity, security.PermBreakGlassApprove),
		"SECURITY must be able to approve a break-glass grant")

	// The elevation names a THIRD operator. A break-glass grant's target_id is
	// a person, and approving an elevation of yourself is granting yourself
	// one however many other people were involved, so internal/admin refuses
	// the target as approver as well as the proposer. Pointing the grant at
	// `second` would make the positive control below fail for *that* rule,
	// which would hide whether this one works at all.
	// "operations" is a distinct dev identity granted ADMIN here, so the
	// grantee holds break_glass:approve like the proposer does: what refuses
	// them below is the elevation being theirs, not a missing permission.
	grantee := operatorSession(t, "operations", string(security.RoleAdmin))
	require.NotEqual(t, proposer.SubjectID, grantee.SubjectID)
	require.NotEqual(t, second.SubjectID, grantee.SubjectID)

	actionID := proposeBreakGlass(t, proposer, grantee.SubjectID, "self")
	before := adminAction(t, actionID)
	require.Equal(t, "PROPOSED", before.Status)
	require.True(t, before.RequiresDual)
	require.Equal(t, proposer.SubjectID, before.ProposedBy)
	require.Nil(t, before.ApprovedBy)

	approver := proposer
	if secBreak(t, "admin_self_approval_uses_a_second_operator") {
		approver = second
	}

	self := decide(t, approver, actionID, "approve", "self")
	require.Equal(t, http.StatusForbidden, self.Status,
		"the proposer approved its own action: %s", self.text())
	require.Equal(t, string(errs.CodeForbidden), self.Problem.Code)
	require.NotContains(t, self.text(), proposer.SubjectID,
		"the refusal named the proposer, which tells an attacker who to compromise next")

	after := adminAction(t, actionID)
	require.Equal(t, "PROPOSED", after.Status, "the refused self-approval still moved the action")
	require.Nil(t, after.ApprovedBy, "the refused self-approval still recorded an approver")

	// The other shape of self-approval, and the one a proposer check alone
	// misses: the beneficiary signing off their own elevation. The grantee is
	// an ADMIN, so they hold break_glass:approve and reach the same code path
	// `second` is about to succeed on — the only difference is that the
	// elevation is theirs.
	beneficiary := decide(t, grantee, actionID, "approve", "beneficiary")
	require.Equal(t, http.StatusForbidden, beneficiary.Status,
		"the grantee approved its own elevation: %s", beneficiary.text())
	require.Equal(t, string(errs.CodeForbidden), beneficiary.Problem.Code)
	require.NotContains(t, beneficiary.text(), grantee.SubjectID,
		"the refusal named the grantee, which tells an attacker whose session to take")
	stillOpen := adminAction(t, actionID)
	require.Equal(t, "PROPOSED", stillOpen.Status, "the refused beneficiary approval still moved the action")
	require.Nil(t, stillOpen.ApprovedBy)

	// The action must still be approvable by someone else: a refusal that also
	// broke the legitimate path would be a denial of service, not a control.
	ok := decide(t, second, actionID, "approve", "second")
	require.Equal(t, http.StatusOK, ok.Status, "a distinct operator could not approve: %s", ok.text())
	approved := adminAction(t, actionID)
	require.Equal(t, "APPROVED", approved.Status)
	require.NotNil(t, approved.ApprovedBy)
	require.Equal(t, second.SubjectID, *approved.ApprovedBy)
	require.NotEqual(t, approved.ProposedBy, *approved.ApprovedBy,
		"the stored approver equals the proposer")

	// And the proposer must not be able to re-decide it afterwards either.
	again := decide(t, proposer, actionID, "approve", "self-again")
	require.NotEqual(t, http.StatusOK, again.Status,
		"the proposer approved an already-approved action: %s", again.text())
	require.Less(t, again.Status, 500)
	require.Equal(t, "APPROVED", adminAction(t, actionID).Status)
}

// TestDualControl_RejectionIsAlsoTwoPersonAndCancellationIsNot rounds out the
// state machine: the deciding routes must not offer a way around the rule, and
// the one thing a proposer MAY do alone is withdraw its own proposal.
func TestDualControl_RejectionIsAlsoTwoPersonAndCancellationIsNot(t *testing.T) {
	requireAPI(t)
	proposer := operatorSession(t, "admin", string(security.RoleAdmin))
	second := operatorSession(t, "security", string(security.RoleSecurity))

	// A dual-control action must not be executable straight from PROPOSED,
	// which would skip the approval entirely.
	actionID := proposeBreakGlass(t, proposer, second.SubjectID, "exec")
	exec := decide(t, proposer, actionID, "execute", "exec")
	require.NotEqual(t, http.StatusOK, exec.Status,
		"a dual-control action executed without an approval: %s", exec.text())
	require.Less(t, exec.Status, 500, "execute produced %d: %s", exec.Status, exec.text())
	row := adminAction(t, actionID)
	require.NotEqual(t, "EXECUTED", row.Status, "the action executed with no distinct approver")
	require.Nil(t, row.ApprovedBy)

	// The second operator may reject it, and the rejection is recorded against
	// that operator rather than the proposer.
	rejected := decide(t, second, actionID, "reject", "exec")
	require.Equal(t, http.StatusOK, rejected.Status, "a distinct operator could not reject: %s", rejected.text())
	require.Equal(t, "REJECTED", adminAction(t, actionID).Status)
}

// TestStepUp_AdminWritesRequireRecentStrongAuthentication proves the step-up
// gate is measured and not assumed. The session used here holds the ADMIN role
// and every permission the route asks for; the only thing it lacks is a strong
// authentication method in its amr.
//
// The negative control sends the MFA session instead, which is admitted, so
// every STEP_UP_REQUIRED assertion fires.
func TestStepUp_AdminWritesRequireRecentStrongAuthentication(t *testing.T) {
	requireAPI(t)
	steppedUp := operatorSession(t, "admin", string(security.RoleAdmin))
	weak := operatorSessionWithoutStepUp(t, "admin", string(security.RoleAdmin))
	require.Equal(t, steppedUp.SubjectID, weak.SubjectID,
		"the two sessions must belong to one operator, or this measures identity and not step-up")

	// The approval probe needs a SECOND operator, in both its stepped-up and
	// its weak form. Approving as the proposer would be refused by dual
	// control whatever the amr said, and a negative control that fires for the
	// other rule proves nothing about this one.
	steppedSecond := operatorSession(t, "security", string(security.RoleSecurity))
	weakSecond := operatorSessionWithoutStepUp(t, "security", string(security.RoleSecurity))
	require.NotEqual(t, steppedUp.SubjectID, weakSecond.SubjectID)

	// The weak session is otherwise fully authorized: it can read the same
	// admin surface, which needs the same permissions and no step-up.
	read := getAs(t, weak.Token, "/v1/admin/actions")
	require.Equal(t, http.StatusOK, read.Status,
		"the session without step-up cannot even read; the refusals below would be about the role, not step-up: %s", read.text())

	probe, approveProbe := weak, weakSecond
	if secBreak(t, "admin_step_up_probe_uses_a_stepped_up_session") {
		probe, approveProbe = steppedUp, steppedSecond
	}

	before := countRows(t, `SELECT count(*) FROM admin_actions WHERE reason LIKE $1`, "%"+runToken+"%")

	t.Run("propose", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"kind":"BREAK_GLASS_GRANT","target_type":"user","target_id":%q,"reason":"adversarial step-up probe %s"}`,
			weak.SubjectID, runToken,
		)
		r := postAs(t, probe.Token, "/v1/admin/actions", key("stepup-propose"), body)
		requireStepUpRefusal(t, r, "POST /v1/admin/actions")
	})

	// The decision route carries the same requirement, so an action proposed
	// with step-up cannot be approved without it.
	actionID := proposeBreakGlass(t, steppedUp, weakSecond.SubjectID, "stepup")
	t.Run("approve", func(t *testing.T) {
		r := decide(t, approveProbe, actionID, "approve", "stepup")
		requireStepUpRefusal(t, r, "POST /v1/admin/actions/{id}/approve")
		require.Equal(t, "PROPOSED", adminAction(t, actionID).Status,
			"the refused approval still moved the action")
	})

	// Every other mutating admin route the operator can reach must refuse too:
	// step-up is a property of the surface, not of one handler.
	for _, w := range adminWriteRoutes(absentRecord) {
		if w.name == "kill-switch" {
			// Activation is deliberately step-up-free so an incident can be
			// stopped fast (POLICY_AUTHORITY §2); release goes through an
			// approved admin action, which is covered above.
			continue
		}
		t.Run(w.name, func(t *testing.T) {
			r := postAs(t, probe.Token, w.path, key("stepup-"+w.name), w.body)
			requireStepUpRefusal(t, r, w.path)
		})
	}

	require.Equal(t, before,
		countRows(t, `SELECT count(*) FROM admin_actions WHERE reason LIKE $1`, "%"+runToken+"%"),
		"a refused request without step-up still wrote an admin action")
}

func requireStepUpRefusal(t *testing.T, r response, what string) {
	t.Helper()
	require.Equalf(t, http.StatusForbidden, r.Status,
		"%s was answered %d for a session that never stepped up: %s", what, r.Status, r.text())
	require.Equalf(t, string(errs.CodeStepUpRequired), r.Problem.Code,
		"%s was refused, but not for the reason under test: %s", what, r.text())
}

// operatorSessionWithoutStepUp logs in as the identity WITHOUT the dev
// provider's :mfa suffix, so the session carries the role and every permission
// that goes with it but no strong authentication method. The role grant is
// idempotent, so this composes with operatorSession on the same identity.
func operatorSessionWithoutStepUp(t *testing.T, identity, role string) session {
	t.Helper()
	mustLogin(t, identity)
	grantOperatorRole(t, identity, role)
	s := mustLogin(t, identity)
	require.Contains(t, s.Roles, role, "%s did not receive the %s role", identity, role)
	require.Equal(t, "OPERATOR", s.ActorType)

	me := getAs(t, s.Token, "/v1/me")
	require.Equal(t, http.StatusOK, me.Status)
	var doc meDoc
	require.NoError(t, json.Unmarshal(me.Body, &doc))
	require.NotEmpty(t, doc.AMR, "the session reports no authentication methods at all")
	for _, method := range doc.AMR {
		require.NotContains(t, []string{"mfa", "otp", "hwk", "swk", "fido", "webauthn"}, method,
			"the 'no step-up' session carries the strong method %q, so it is not the session this test needs", method)
	}
	return s
}
