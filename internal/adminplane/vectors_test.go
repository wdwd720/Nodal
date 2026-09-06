package adminplane

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/security"
)

// updateVectors rewrites the decision vectors the operator console's own tests
// replay:
//
//	go test ./internal/adminplane -run TestDecisionVectorsGolden -update-vectors
var updateVectors = flag.Bool("update-vectors", false,
	"rewrite apps/admin/src/generated/decisions.json from adminplane.Decide")

const vectorsPath = "../../apps/admin/src/generated/decisions.json"

// principalView is the principal as JSON, in the shape the console receives it
// from GET /v1/me plus the break-glass deadline that endpoint does not yet
// return (see docs/runbooks/operator-console.md).
type principalView struct {
	SubjectID       string          `json:"subject_id"`
	ActorType       string          `json:"actor_type"`
	Roles           []security.Role `json:"roles"`
	AuthTime        string          `json:"auth_time,omitempty"`
	AMR             []string        `json:"amr"`
	BreakGlassUntil string          `json:"break_glass_until,omitempty"`
	// Absent means there is no session at all.
	Absent bool `json:"absent,omitempty"`
}

// actionView is one admin action as the console receives it from
// GET /v1/admin/actions.
type actionView struct {
	Kind         admin.Kind   `json:"kind"`
	Status       admin.Status `json:"status"`
	RequiresDual bool         `json:"requires_dual"`
	ProposedBy   string       `json:"proposed_by"`
	ApprovedBy   string       `json:"approved_by,omitempty"`
	ExpiresAt    string       `json:"expires_at"`
}

type vectorCase struct {
	Principal string   `json:"principal"`
	Action    string   `json:"action"`
	Now       string   `json:"now"`
	Verb      Verb     `json:"verb"`
	Want      Decision `json:"want"`
}

type vectorDoc struct {
	Version    int                      `json:"version"`
	Source     string                   `json:"source"`
	Principals map[string]principalView `json:"principals"`
	Actions    map[string]actionView    `json:"actions"`
	Cases      []vectorCase             `json:"cases"`
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// lateInstant is the second instant every vector is evaluated at. It is past
// the longest action expiry in the table, so an action that was live at now0
// is dead here.
var lateInstant = now0.Add(48 * time.Hour)

// vectorPrincipals is the closed set of caller shapes the vectors cover: no
// session, an agent, a read-only operator, the proposer with and without an
// elevation, a distinct operator with a live, a lapsed and no elevation, an
// elevation lapsing exactly on the instant being judged, a stale step-up, a
// password-only session, a subject that is not a user, and two structurally
// invalid sessions.
//
// Every timestamp is whole seconds. JavaScript's Date.parse resolves to
// milliseconds, so a sub-millisecond offset would make the Go and TypeScript
// verdicts disagree for a reason that has nothing to do with authority.
func vectorPrincipals(proposer, other string) map[string]principalView {
	fresh := rfc(now0.Add(-time.Minute))
	stale := rfc(now0.Add(-24 * time.Hour))
	live := rfc(now0.Add(time.Hour))
	dead := rfc(now0.Add(-time.Second))
	// An elevation that ends exactly at the instant being judged. Liveness is
	// `now < until`, so this one is already gone: the boundary is the whole
	// point, because an elevation honored for even one instant past its
	// deadline is a privilege-escalation path.
	endsNow := rfc(now0)
	endsNextSecond := rfc(now0.Add(time.Second))
	// Fresh and elevated at lateInstant rather than at now0, so the later
	// instant exercises expiry of the *action* rather than of the session.
	lateFresh := rfc(lateInstant.Add(-time.Minute))
	lateLive := rfc(lateInstant.Add(time.Hour))
	op := func(uid string, roles ...security.Role) principalView {
		return principalView{SubjectID: uid, ActorType: string(security.ActorOperator), Roles: roles, AuthTime: fresh, AMR: []string{"mfa"}}
	}
	withBG := func(p principalView, until string) principalView {
		p.Roles = append(append([]security.Role(nil), p.Roles...), security.RoleBreakGlass)
		p.BreakGlassUntil = until
		return p
	}
	agent := principalView{SubjectID: "01a0754e-0000-7000-8000-00000000a9e7", ActorType: string(security.ActorAgent), Roles: []security.Role{}, AMR: []string{}}
	stalePrincipal := withBG(op(other, security.RoleAdmin), live)
	stalePrincipal.AuthTime = stale
	weak := withBG(op(other, security.RoleAdmin), live)
	weak.AMR = []string{"pwd"}
	// Elevated and strongly authenticated as at lateInstant.
	lateElevated := withBG(op(other, security.RoleAdmin), lateLive)
	lateElevated.AuthTime = lateFresh
	// A session carrying a role the matrix has never heard of: the shape a
	// tampered or downgrade-attacked session would have. Validate refuses it,
	// so it holds nothing at all rather than holding what its other roles say.
	unknownRole := op(other, security.RoleAdmin)
	unknownRole.Roles = append(append([]security.Role(nil), unknownRole.Roles...), security.Role("WIZARD"))
	// The BREAK_GLASS role with no deadline. security.Principal.Validate
	// refuses it outright, which is what stops an elevation that never expires.
	roleWithoutDeadline := op(other, security.RoleAdmin)
	roleWithoutDeadline.Roles = append(append([]security.Role(nil), roleWithoutDeadline.Roles...), security.RoleBreakGlass)
	return map[string]principalView{
		"anonymous":                        {Absent: true, Roles: []security.Role{}, AMR: []string{}},
		"agent":                            agent,
		"support_read_only":                op(other, security.RoleSupportReadOnly),
		"proposer_admin":                   op(proposer, security.RoleAdmin),
		"proposer_admin_elevated":          withBG(op(proposer, security.RoleAdmin), live),
		"other_admin":                      op(other, security.RoleAdmin),
		"other_admin_elevated":             withBG(op(other, security.RoleAdmin), live),
		"other_admin_elevation_expired":    withBG(op(other, security.RoleAdmin), dead),
		"other_admin_elevation_ends_now":   withBG(op(other, security.RoleAdmin), endsNow),
		"other_admin_elevation_ends_in_1s": withBG(op(other, security.RoleAdmin), endsNextSecond),
		"other_admin_elevated_late":        lateElevated,
		"other_admin_stale_step_up":        stalePrincipal,
		"other_admin_password_only":        weak,
		"other_operations":                 op(other, security.RoleOperations),
		"other_finance":                    op(other, security.RoleFinance),
		"subject_is_not_a_user":            withBG(op("dev:admin", security.RoleAdmin), live),
		"role_not_in_matrix":               unknownRole,
		"break_glass_without_deadline":     roleWithoutDeadline,
	}
}

// toPrincipal turns a view back into a security.Principal.
func toPrincipal(t *testing.T, v principalView) (security.Principal, bool) {
	t.Helper()
	if v.Absent {
		return security.Principal{}, false
	}
	p := security.Principal{
		SubjectID: v.SubjectID, ActorType: security.ActorType(v.ActorType),
		Roles: append([]security.Role(nil), v.Roles...), AMR: append([]string(nil), v.AMR...),
		SessionID: "sess",
	}
	if v.ActorType == string(security.ActorAgent) {
		p.AccountIDs = []string{"acct-1"}
	}
	if v.AuthTime != "" {
		at, err := time.Parse(time.RFC3339Nano, v.AuthTime)
		require.NoError(t, err)
		p.AuthTime = at
	}
	if v.BreakGlassUntil != "" {
		u, err := time.Parse(time.RFC3339Nano, v.BreakGlassUntil)
		require.NoError(t, err)
		p.BreakGlassUntil = &u
	}
	return p, true
}

// toAction turns a view back into an admin.Action.
func toAction(t *testing.T, v actionView) admin.Action {
	t.Helper()
	exp, err := time.Parse(time.RFC3339Nano, v.ExpiresAt)
	require.NoError(t, err)
	a := admin.Action{
		ID: admin.NewActionID(), Kind: v.Kind, TargetType: "target", TargetID: "t-1",
		Reason: "a reason of sufficient length", RequiresDual: v.RequiresDual,
		Status: v.Status, ProposedBy: v.ProposedBy, ProposedAt: now0, ExpiresAt: exp, UpdatedAt: now0,
	}
	if v.ApprovedBy != "" {
		by := v.ApprovedBy
		at := now0
		a.ApprovedBy, a.ApprovedAt = &by, &at
	}
	return a
}

// vectorActions covers, per kind, the stored states an operator actually sees
// in the queue plus the two dangerous ones: an approval by the proposer
// themselves, and a terminal record.
func vectorActions(proposer, other string) map[string]actionView {
	out := map[string]actionView{}
	for _, kind := range admin.Kinds() {
		spec, _ := admin.Spec(kind)
		exp := rfc(now0.Add(spec.Expiry))
		base := actionView{Kind: kind, RequiresDual: spec.RequiresDual, ProposedBy: proposer, ExpiresAt: exp}

		proposed := base
		proposed.Status = admin.StatusProposed
		out[string(kind)+"/proposed"] = proposed

		approved := base
		approved.Status = admin.StatusApproved
		approved.ApprovedBy = other
		out[string(kind)+"/approved"] = approved

		selfApproved := base
		selfApproved.Status = admin.StatusApproved
		selfApproved.ApprovedBy = proposer
		out[string(kind)+"/self_approved"] = selfApproved

		rejected := base
		rejected.Status = admin.StatusRejected
		out[string(kind)+"/rejected"] = rejected
	}
	// A kind that is not in the closed table. It is not reachable through the
	// API — Propose refuses an unknown kind — but a stored row whose kind was
	// removed from the table, or a hand-built request, produces exactly this,
	// and the console must refuse it rather than render an action with no
	// policy. The name is deliberately the one thing this system will never
	// have: there is no balance-edit action anywhere.
	out["BALANCE_EDIT/proposed"] = actionView{
		Kind: admin.Kind("BALANCE_EDIT"), Status: admin.StatusProposed, RequiresDual: true,
		ProposedBy: proposer, ExpiresAt: rfc(now0.Add(time.Hour)),
	}
	return out
}

// TestDecisionVectorsGolden generates the vectors the operator console replays
// through its own copy of the decision logic. The console's TypeScript is a
// port, and a port drifts; the vectors are what stop it. Every case here is a
// case the integration suite has already proven the enforcing service agrees
// with, so a green console test means the console agrees with the database.
func TestDecisionVectorsGolden(t *testing.T) {
	proposer := "01a0754e-1111-7000-8000-000000000001"
	other := "01a0754e-2222-7000-8000-000000000002"
	principals := vectorPrincipals(proposer, other)
	actions := vectorActions(proposer, other)

	instants := []time.Time{now0, lateInstant}

	doc := vectorDoc{
		Version:    AuthorityVersion,
		Source:     "internal/adminplane (generated by TestDecisionVectorsGolden; do not edit by hand)",
		Principals: principals,
		Actions:    actions,
	}
	principalNames := sortedKeys(principals)
	actionNames := sortedKeys(actions)
	for _, pn := range principalNames {
		p, present := toPrincipal(t, principals[pn])
		actor := NewActor(p)
		if !present {
			actor = Actor{}
		}
		for _, an := range actionNames {
			action := toAction(t, actions[an])
			for _, at := range instants {
				for _, verb := range []Verb{VerbApprove, VerbReject, VerbExecute, VerbCancel} {
					doc.Cases = append(doc.Cases, vectorCase{
						Principal: pn, Action: an, Now: rfc(at), Verb: verb,
						Want: Decide(actor, action, verb, at),
					})
				}
			}
			// Proposing is about a kind, not a record; emit it once per kind.
			if actions[an].Status == admin.StatusProposed {
				for _, at := range instants {
					doc.Cases = append(doc.Cases, vectorCase{
						Principal: pn, Action: an, Now: rfc(at), Verb: VerbPropose,
						Want: DecideProposal(actor, action.Kind, at),
					})
				}
			}
		}
	}

	rendered, err := json.Marshal(doc)
	require.NoError(t, err)
	rendered = append(rendered, '\n')

	if *updateVectors {
		require.NoError(t, os.MkdirAll(filepath.Dir(vectorsPath), 0o755))
		require.NoError(t, os.WriteFile(vectorsPath, rendered, 0o644))
		t.Logf("wrote %s (%d cases, %d bytes)", vectorsPath, len(doc.Cases), len(rendered))
		return
	}

	want, err := os.ReadFile(vectorsPath)
	if os.IsNotExist(err) {
		t.Fatalf("%s is missing; run: go test ./internal/adminplane -run TestDecisionVectorsGolden -update-vectors", vectorsPath)
	}
	require.NoError(t, err)
	assert.Equal(t, string(normalizeNewlines(want)), string(rendered),
		"the operator console's decision vectors are stale; regenerate them with -update-vectors")
}

// TestVectorsAreNotVacuous: a vector file in which nothing is ever permitted,
// or in which some refusal never occurs, would let a broken console port pass.
func TestVectorsAreNotVacuous(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(vectorsPath)
	require.NoError(t, err, "run TestDecisionVectorsGolden -update-vectors first")
	var doc vectorDoc
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Cases)

	seen := map[Reason]int{}
	verbs := map[Verb]int{}
	for _, c := range doc.Cases {
		seen[c.Want.Reason]++
		verbs[c.Verb]++
	}
	for _, r := range Reasons() {
		assert.Positive(t, seen[r], "no vector exercises %s", r)
	}
	for _, v := range Verbs() {
		assert.Positive(t, verbs[v], "no vector exercises %s", v)
	}
	t.Logf("%d cases over %d principals and %d actions: %v", len(doc.Cases), len(doc.Principals), len(doc.Actions), seen)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
