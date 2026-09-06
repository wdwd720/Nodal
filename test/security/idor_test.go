//go:build integration

package security

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// nilUUID and absentAccount are the two shapes of "an id that names nothing":
// the RFC 4122 nil UUID, which the id package refuses outright, and a
// well-formed UUIDv7 that simply does not exist.
const (
	nilUUID       = "00000000-0000-0000-0000-000000000000"
	absentAccount = "01a00000-0000-7000-8000-0000000000aa"
	absentRecord  = "01a00000-0000-7000-8000-0000000000bb"
)

// accountSubPaths is every route under /v1/accounts/{accountId}. The list is
// asserted complete against the router in TestIDOR_CoversEveryAccountScopedRoute
// below, so a new account-scoped route cannot be added without being probed.
var accountSubPaths = []string{"", "/buying-power", "/holdings", "/ledger/transactions", "/activity", "/export"}

// tenants is the fixture: two customers who own different accounts, plus the
// seeded ADMIN operator whose own customer account is a third target.
type tenants struct {
	a, b  session
	admin session
}

func newTenants(t *testing.T) tenants {
	t.Helper()
	a := mustLogin(t, "customer-a")
	b := mustLogin(t, "customer-b")
	adm := operatorSession(t, "admin", string(security.RoleAdmin))

	// Preconditions. A cross-tenant test whose "foreign" account does not
	// exist, or which is really the caller's own account, proves nothing.
	require.NotEqual(t, a.SubjectID, b.SubjectID, "customer-a and customer-b resolved to one user")
	aid, bid := firstAccount(t, a), firstAccount(t, b)
	require.NotEqual(t, aid, bid, "customer-a and customer-b resolved to one account")
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM accounts WHERE id = $1`, aid),
		"customer-a's own account must exist")
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM accounts WHERE id = $1 AND owner_user_id = $2`, bid, b.SubjectID),
		"the foreign account must exist AND belong to customer-b, or the 403 proves nothing")
	require.Zero(t, countRows(t, `SELECT count(*) FROM accounts WHERE id = $1`, absentAccount),
		"the 'absent' account id must really name nothing")
	require.Equal(t, "CUSTOMER", strings.ToUpper(a.Roles[0]))
	return tenants{a: a, b: b, admin: adm}
}

// TestIDOR_CrossTenantAccountReadsAreRefused walks every account-scoped route
// with customer-a's session pointed at customer-b's account, at the seeded
// admin's account, and at ids that name nothing. Each must be refused; the
// caller's own account must succeed on the same route, so a blanket failure
// (an unwired port, a dead session) cannot masquerade as a pass.
func TestIDOR_CrossTenantAccountReadsAreRefused(t *testing.T) {
	requireAPI(t)
	tn := newTenants(t)
	own := firstAccount(t, tn.a)
	foreign := firstAccount(t, tn.b)
	adminAccount := firstAccount(t, tn.admin)

	// The negative control points every probe at the caller's own account, so
	// the refusal assertions see 200 and fire.
	if secBreak(t, "idor_targets_own_account") {
		foreign, adminAccount = own, own
	}

	for _, sub := range accountSubPaths {
		t.Run("own"+orRoot(sub), func(t *testing.T) {
			r := getAs(t, tn.a.Token, "/v1/accounts/"+own+sub)
			require.Equal(t, http.StatusOK, r.Status,
				"the caller must be able to read its OWN account on this route, or the refusals below prove nothing: %s", r.text())
		})
		t.Run("foreign"+orRoot(sub), func(t *testing.T) {
			r := getAs(t, tn.a.Token, "/v1/accounts/"+foreign+sub)
			requireForbidden(t, r, "customer-a read customer-b's account")
		})
		t.Run("admin_account"+orRoot(sub), func(t *testing.T) {
			r := getAs(t, tn.a.Token, "/v1/accounts/"+adminAccount+sub)
			requireForbidden(t, r, "customer-a read the operator's account")
		})
	}
}

// TestIDOR_ForeignAndAbsentAccountsAreIndistinguishable is the account
// enumeration oracle test. An answer that tells "this account exists but is
// not yours" apart from "no such account" lets any customer enumerate the
// platform's accounts, so the two answers must agree on everything except the
// echoed instance path and the per-request id.
func TestIDOR_ForeignAndAbsentAccountsAreIndistinguishable(t *testing.T) {
	requireAPI(t)
	tn := newTenants(t)
	foreign := firstAccount(t, tn.b)
	absent := absentAccount

	// The negative control makes the "absent" id the caller's own account, so
	// the two answers differ and the comparison fires.
	if secBreak(t, "idor_absent_id_is_own_account") {
		absent = firstAccount(t, tn.a)
	}

	for _, sub := range accountSubPaths {
		t.Run(orRoot(sub), func(t *testing.T) {
			existing := getAs(t, tn.a.Token, "/v1/accounts/"+foreign+sub)
			missing := getAs(t, tn.a.Token, "/v1/accounts/"+absent+sub)

			require.Equal(t, existing.Status, missing.Status,
				"an existing-but-foreign account answers %d and a non-existent one answers %d: the API is an account-enumeration oracle",
				existing.Status, missing.Status)
			require.Equal(t, existing.Problem.Code, missing.Problem.Code, "the codes differ, which is an enumeration oracle")
			require.Equal(t, existing.Problem.Detail, missing.Problem.Detail, "the details differ, which is an enumeration oracle")
			require.Equal(t, existing.Problem.Title, missing.Problem.Title)
			require.Equal(t, existing.Problem.Type, missing.Problem.Type)
			assert.Equal(t, normaliseProblem(existing, foreign), normaliseProblem(missing, absent),
				"the two rejections differ beyond the echoed path and request id")
		})
	}
}

// TestIDOR_DEFECT_RecordLookupDistinguishesForeignFromAbsent pins an observed
// DEFECT, not a security property. Fetching a record by its own id
// (/v1/intents/{intentId}) answers 403 FORBIDDEN when the record exists and
// belongs to someone else, but 404 NOT_FOUND when no such record exists, so a
// customer holding an id can learn whether it names a real trade intent.
//
// The same shape is in GetOrdersOrderId, GetFundingDepositsDepositId and
// PostIntentsIntentIdCancel: each fetches first and calls
// security.RequireAccount second. The account routes above do not have it
// (RequireAccount runs before the fetch), and DeleteSessionsSessionId
// deliberately collapses both cases to NOT_FOUND, which is what these should
// do too.
//
// WHEN THIS IS FIXED THIS TEST WILL FAIL. That is the point: invert it to
// require.Equal and delete this comment.
func TestIDOR_DEFECT_RecordLookupDistinguishesForeignFromAbsent(t *testing.T) {
	requireAPI(t)
	tn := newTenants(t)
	foreignIntent := createIntent(t, tn.b, key("defect-oracle-intent"))
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM trade_intents WHERE id = $1`, foreignIntent),
		"the foreign intent must exist, or this proves nothing")
	require.Zero(t, countRows(t, `SELECT count(*) FROM trade_intents WHERE id = $1`, absentRecord))

	existing := getAs(t, tn.a.Token, "/v1/intents/"+foreignIntent)
	missing := getAs(t, tn.a.Token, "/v1/intents/"+absentRecord)

	t.Logf("FINDING: GET /v1/intents/{id} answers %d %s for an existing foreign record and %d %s for an absent one; "+
		"a customer can therefore test whether an id names a real trade intent",
		existing.Status, existing.Problem.Code, missing.Status, missing.Problem.Code)

	require.Equal(t, http.StatusForbidden, existing.Status, "expected the observed defect: foreign record answers 403")
	require.Equal(t, string(errs.CodeForbidden), existing.Problem.Code)
	require.Equal(t, http.StatusNotFound, missing.Status, "expected the observed defect: absent record answers 404")
	require.Equal(t, string(errs.CodeNotFound), missing.Problem.Code)
}

// TestIDOR_MalformedAndDegenerateIdentifiers: nothing a client can put in the
// path position may produce a 5xx, and every refusal carries a stable code.
func TestIDOR_MalformedAndDegenerateIdentifiers(t *testing.T) {
	requireAPI(t)
	tn := newTenants(t)
	own := firstAccount(t, tn.a)
	foreign := firstAccount(t, tn.b)

	cases := []struct {
		name string
		path string
	}{
		{"nil_uuid", "/v1/accounts/" + nilUUID},
		{"nil_uuid_sub", "/v1/accounts/" + nilUUID + "/holdings"},
		{"not_a_uuid", "/v1/accounts/not-a-uuid"},
		{"empty_segment", "/v1/accounts/"},
		{"uuid_with_braces", "/v1/accounts/{" + own + "}"},
		{"uuid_urn_form", "/v1/accounts/urn:uuid:" + own},
		{"uuid_no_dashes", "/v1/accounts/" + strings.ReplaceAll(own, "-", "")},
		{"uuid_with_space", "/v1/accounts/" + own + "%20"},
		{"uuid_with_nul", "/v1/accounts/" + own + "%00"},
		{"uuid_with_newline", "/v1/accounts/" + own + "%0a"},
		{"sql_in_path", "/v1/accounts/%27%20OR%201%3D1--"},
		{"trailing_slash", "/v1/accounts/" + own + "/"},
		{"duplicated_prefix", "/v1/v1/accounts/" + foreign},
		{"double_slash", "/v1/accounts//" + foreign},
		{"dotdot_segment", "/v1/accounts/" + own + "/../" + foreign},
		{"encoded_dotdot", "/v1/accounts/" + own + "%2f..%2f" + foreign},
		{"encoded_dotdot_prefix", "/v1/%2e%2e/v1/accounts/" + foreign},
		{"encoded_slash_suffix", "/v1/accounts/" + own + "%2fholdings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := getRawAs(t, tn.a.Token, c.path)
			require.Less(t, r.Status, 500, "%s produced %d: %s", c.path, r.Status, r.text())
			require.GreaterOrEqual(t, r.Status, 400, "%s was ACCEPTED with %d: %s", c.path, r.Status, r.text())
			require.NotEmpty(t, r.Problem.Code, "%s answered %d with no stable code: %s", c.path, r.Status, r.text())
			require.Contains(t,
				[]string{string(errs.CodeValidationFailed), string(errs.CodeNotFound), string(errs.CodeForbidden)},
				r.Problem.Code, "%s answered with an unexpected code", c.path)
			// Nothing that reached a foreign account may come back as content.
			require.NotContains(t, r.text(), `"id":"`+foreign+`"`, "%s returned the foreign account", c.path)
		})
	}
}

// TestIDOR_UUIDCaseVariationIsNotABypass: UUIDs are case-insensitive, so an
// upper-cased id must resolve to the same account and must NOT slip past the
// tenant check by failing a case-sensitive string comparison against the
// principal's account list.
func TestIDOR_UUIDCaseVariationIsNotABypass(t *testing.T) {
	requireAPI(t)
	tn := newTenants(t)
	own := firstAccount(t, tn.a)
	foreign := firstAccount(t, tn.b)

	upperOwn := getAs(t, tn.a.Token, "/v1/accounts/"+strings.ToUpper(own))
	require.Equal(t, http.StatusOK, upperOwn.Status, "an upper-cased own id must still resolve: %s", upperOwn.text())
	require.Contains(t, upperOwn.text(), own, "the canonical (lower-case) id must come back")

	for _, variant := range []string{strings.ToUpper(foreign), mixedCase(foreign)} {
		r := getAs(t, tn.a.Token, "/v1/accounts/"+variant)
		requireForbidden(t, r, "case-varied foreign account "+variant)
	}
}

// TestIDOR_QueryParameterSmuggling: the account-scoped list endpoints take the
// account in a query parameter rather than the path. The same tenant check
// must apply, and a duplicated parameter must not resolve to whichever copy
// the binder happens to prefer.
func TestIDOR_QueryParameterSmuggling(t *testing.T) {
	requireAPI(t)
	tn := newTenants(t)
	own := firstAccount(t, tn.a)
	foreign := firstAccount(t, tn.b)
	if secBreak(t, "idor_targets_own_account") {
		foreign = own
	}

	for _, path := range []string{"/v1/intents", "/v1/orders", "/v1/funding/deposits"} {
		t.Run(strings.TrimPrefix(path, "/v1/"), func(t *testing.T) {
			okOwn := getAs(t, tn.a.Token, path+"?account_id="+own)
			require.Equal(t, http.StatusOK, okOwn.Status, "own account must list: %s", okOwn.text())

			requireForbidden(t, getAs(t, tn.a.Token, path+"?account_id="+foreign), "foreign account_id on "+path)

			// A duplicated parameter must not be resolved silently in the
			// attacker's favor, whichever copy that would be.
			for _, dup := range []string{
				path + "?account_id=" + own + "&account_id=" + foreign,
				path + "?account_id=" + foreign + "&account_id=" + own,
			} {
				r := getAs(t, tn.a.Token, dup)
				require.NotEqual(t, http.StatusOK, r.Status, "a duplicated account_id was accepted: %s", dup)
				require.Less(t, r.Status, 500, "%s produced %d", dup, r.Status)
			}
		})
	}
}

// TestIDOR_CustomerCannotReachAdminSurface: every /v1/admin route refuses a
// customer session outright. It is deliberately driven from the route list, so
// a new admin route joins the probe automatically.
func TestIDOR_CustomerCannotReachAdminSurface(t *testing.T) {
	requireAPI(t)
	tn := newTenants(t)
	token := tn.a.Token
	// The negative control sends the ADMIN session, so every 403 assertion
	// sees a success and fires.
	if secBreak(t, "admin_probe_uses_privileged_session") {
		token = tn.admin.Token
	}

	own := firstAccount(t, tn.a)
	for _, p := range adminReadRoutes() {
		t.Run("GET"+strings.ReplaceAll(p, "/", "_"), func(t *testing.T) {
			r := getAs(t, token, p)
			require.Equal(t, http.StatusForbidden, r.Status, "customer reached %s: %s", p, r.text())
			require.Equal(t, string(errs.CodeForbidden), r.Problem.Code)
		})
	}
	for _, w := range adminWriteRoutes(own) {
		t.Run("POST"+strings.ReplaceAll(w.path, "/", "_"), func(t *testing.T) {
			r := postAs(t, token, w.path, key("cust-admin-"+w.name), w.body)
			require.Equal(t, http.StatusForbidden, r.Status, "customer reached %s: %s", w.path, r.text())
			require.Contains(t, []string{string(errs.CodeForbidden), string(errs.CodeStepUpRequired)}, r.Problem.Code)
		})
	}
}

// TestIDOR_SupportReadOnlyGetsExactlyItsMatrix drives the SUPPORT_READ_ONLY
// role against the whole admin surface and checks each answer against the
// permission matrix in internal/security rather than a hand-written list, so
// the test cannot drift from the policy it is testing.
func TestIDOR_SupportReadOnlyGetsExactlyItsMatrix(t *testing.T) {
	requireAPI(t)
	sup := operatorSession(t, "support", string(security.RoleSupportReadOnly))
	require.Equal(t, []string{string(security.RoleSupportReadOnly)}, sup.Roles)
	own := firstAccount(t, sup)

	granted := func(any ...security.Permission) bool {
		for _, p := range any {
			if security.RoleGrants(security.RoleSupportReadOnly, p) {
				return true
			}
		}
		return false
	}

	reads := []struct {
		path string
		perm []security.Permission
	}{
		{"/v1/admin/accounts", []security.Permission{security.PermAccountReadAny}},
		{"/v1/admin/gates", []security.Permission{security.PermGateRead}},
		{"/v1/admin/kill-switches", []security.Permission{security.PermRiskRead, security.PermGateRead}},
		{"/v1/admin/actions", admin.ReadPermissions()},
		{"/v1/admin/providers", []security.Permission{security.PermAdminAuditRead}},
		{"/v1/admin/reconciliation/records", []security.Permission{security.PermReconciliationRead}},
	}
	allowedCount, refusedCount := 0, 0
	for _, rd := range reads {
		t.Run(strings.ReplaceAll(rd.path, "/", "_"), func(t *testing.T) {
			r := getAs(t, sup.Token, rd.path)
			if granted(rd.perm...) {
				allowedCount++
				// UNSUPPORTED means the port is not wired in this deployment;
				// authorization still let the request through, which is what
				// is under test here.
				require.Contains(t, []int{http.StatusOK, http.StatusUnprocessableEntity}, r.Status,
					"SUPPORT_READ_ONLY holds %v but %s answered %d: %s", rd.perm, rd.path, r.Status, r.text())
				if r.Status == http.StatusUnprocessableEntity {
					require.Equal(t, string(errs.CodeUnsupported), r.Problem.Code)
				}
				return
			}
			refusedCount++
			require.Equal(t, http.StatusForbidden, r.Status,
				"SUPPORT_READ_ONLY holds none of %v but %s answered %d: %s", rd.perm, rd.path, r.Status, r.text())
			require.Equal(t, string(errs.CodeForbidden), r.Problem.Code)
		})
	}
	// The matrix must actually contain both outcomes, or the loop above is
	// asserting one thing under two names.
	require.Positive(t, allowedCount, "no admin read was expected to be allowed; the matrix lookup is wrong")
	require.Positive(t, refusedCount, "no admin read was expected to be refused; the matrix lookup is wrong")

	// Read-only means read-only: every mutating admin route is refused,
	// whatever the step-up state.
	for _, w := range adminWriteRoutes(own) {
		t.Run("write"+strings.ReplaceAll(w.path, "/", "_"), func(t *testing.T) {
			r := postAs(t, sup.Token, w.path, key("sup-"+w.name), w.body)
			require.Equal(t, http.StatusForbidden, r.Status,
				"SUPPORT_READ_ONLY reached the mutating route %s: %s", w.path, r.text())
			require.Contains(t, []string{string(errs.CodeForbidden), string(errs.CodeStepUpRequired)}, r.Problem.Code)
		})
	}
}

// --- helpers ---------------------------------------------------------------

func orRoot(sub string) string {
	if sub == "" {
		return "/"
	}
	return sub
}

func requireForbidden(t *testing.T, r response, what string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, r.Status, "%s was not refused with 403: %d %s", what, r.Status, r.text())
	require.Equal(t, string(errs.CodeForbidden), r.Problem.Code, "%s: unexpected code", what)
	require.Empty(t, r.Problem.Fields, "%s: the refusal carried fields that could describe the target", what)
}

// normaliseProblem blanks the two parts of a problem document that legitimately
// differ between two requests, so everything else can be compared verbatim.
func normaliseProblem(r response, id string) string {
	p := r.Problem
	p.Instance = strings.ReplaceAll(p.Instance, id, "<id>")
	p.RequestID = ""
	b, err := json.Marshal(p)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func mixedCase(s string) string {
	out := []rune(s)
	for i := range out {
		if i%2 == 0 {
			out[i] = []rune(strings.ToUpper(string(out[i])))[0]
		}
	}
	return string(out)
}

func adminReadRoutes() []string {
	return []string{
		"/v1/admin/accounts",
		"/v1/admin/gates",
		"/v1/admin/kill-switches",
		"/v1/admin/actions",
		"/v1/admin/providers",
		"/v1/admin/reconciliation/records",
	}
}

type adminWrite struct {
	name, path, body string
}

func adminWriteRoutes(accountID string) []adminWrite {
	return []adminWrite{
		{
			"account-status", "/v1/admin/accounts/" + accountID + "/status",
			`{"to":"FROZEN","reason":"adversarial security probe"}`,
		},
		{
			"gate-propose", "/v1/admin/gates/LIVE_MANUAL_TRADING/propose",
			`{"reason":"adversarial security probe"}`,
		},
		{
			"kill-switch", "/v1/admin/kill-switches",
			`{"kind":"GLOBAL","action":"activate","reason":"adversarial security probe"}`,
		},
		{
			"admin-action", "/v1/admin/actions",
			fmt.Sprintf(`{"kind":"ACCOUNT_UNFREEZE","target_type":"account","target_id":%q,"reason":"adversarial security probe"}`, accountID),
		},
		{"action-decision", "/v1/admin/actions/" + absentRecord + "/approve", `{"note":"adversarial security probe"}`},
		{
			"instrument-status", "/v1/admin/instruments/" + absentRecord + "/status",
			`{"to":"RESTRICTED","reason":"adversarial security probe"}`,
		},
		{
			"reconciliation-resolve", "/v1/admin/reconciliation/records/" + absentRecord + "/resolve",
			`{"resolution":"MATCHED","reason":"adversarial security probe"}`,
		},
	}
}
