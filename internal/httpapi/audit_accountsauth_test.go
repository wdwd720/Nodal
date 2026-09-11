package httpapi

// Adversarial audit (goal §54), area accounts-auth. These demonstrated defects.
// All but one are inverted here into the regressions for their fixes; the
// return_to one is fixed on another branch and is left exactly as the auditor
// wrote it, so it fails here and flips once that branch merges.

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/security"
)

// F-accounts-auth-4 (boundary half). The login service accepts `/\evil.test` as
// a "local path", and with CP_AUTH_POST_LOGIN_URL unset -- which is the
// documented same-origin deployment and which config.Validate permits in every
// environment, including PROD -- postLoginDestination hands it straight back as
// the Location of the redirect that also sets the session cookie.
//
// `/\host` is not a path to a browser. The WHATWG URL parser's relative-slash
// state treats U+005C exactly as U+002F, so `/\evil.test` resolves against any
// https base to `https://evil.test/`: an open redirect that fires immediately
// after a successful sign-in, which is the single best moment to land a person
// on a page that asks them to sign in again.
//
// internal/identity's Begin guard and postLoginDestination belong to the
// config-deploy fix branch, so this test is left as written.
func TestAudit_TheCallbackWillRedirectOffSiteOnABackslashReturnTo(t *testing.T) {
	t.Parallel()

	// The pure function first: nothing normalises the stored path.
	assert.Equal(t, `/\evil.test`, postLoginDestination("", `/\evil.test`),
		"with no configured app origin the stored return_to is the Location verbatim")

	// And through the real router, with the login-state cookie the callback
	// requires, so this is the whole transcript and not a unit of it.
	h := newHarness(t)
	h.as(nil)
	h.ports.identity.complete.ReturnTo = `/\evil.test`

	begin := h.do(http.MethodGet, `/v1/auth/login?return_to=%2F%5Cevil.test`, nil)
	require.Equal(t, http.StatusFound, begin.Code)
	assert.Equal(t, `/\evil.test`, h.ports.identity.lastBegin.ReturnTo,
		"the boundary passed the backslash form through to the login service")

	res := h.doWithCookies(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil,
		begin.Result().Cookies())
	require.Equal(t, http.StatusFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, `/\evil.test`, res.Header().Get("Location"),
		"browsers resolve this to https://evil.test/")
	require.NotNil(t, namedCookie(res, "cp_session"),
		"and the session cookie is set on the way out, so the victim is signed in when they land there")
}

// F-178. handlers_profile.go stated the invariant as "Every `/me/...` route is
// self-scoped by construction: there is no identifier in the path, and the
// subject comes from the request principal, so a caller cannot name somebody
// else's record. Cross-tenant access is not prevented by a check that could be
// forgotten -- it is unrepresentable."
//
// TestProfile_NoSelfServiceRouteTakesAnIdentifier was the proof, and it walked
// chi route PATTERNS looking for `{`. Ten /v1/me operations take the identifier
// in the QUERY STRING or the BODY, where the walk cannot see it, and one of
// them -- GetMeVerificationSessionsSessionId, a GET that CHANGES STATE because
// Poll ingests the provider's answer -- resolved it with accountScope, the
// READ-grade helper that security.RequireAccount lets any principal holding
// account:read_any satisfy for any account in the system.
//
// That one is scoped with accountScopeWrite now, and the invariant is proved
// from the contract rather than the router: see
// TestProfile_EveryMeOperationNamingAnAccountIsAccountedFor, which holds every
// /me operation carrying an account id against a list that names the helper it
// uses and why.
func TestAudit_MeRoutesNameAnAccountInTheQueryStringWhereTheInvariantTestCannotSee(t *testing.T) {
	t.Parallel()

	// Enumerate the generated operations whose request object carries an
	// AccountId, so this is measured from the contract rather than listed.
	iface := reflect.TypeOf((*api.StrictServerInterface)(nil)).Elem()
	var meOpsWithAnAccountID []string
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		if !strings.Contains(m.Name, "Me") {
			continue
		}
		req := m.Type.In(1)
		carries := false
		for _, field := range []string{"Params", "Body"} {
			f, ok := req.FieldByName(field)
			if !ok {
				continue
			}
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() != reflect.Struct {
				continue
			}
			if _, found := ft.FieldByName("AccountId"); found {
				carries = true
			}
		}
		if carries {
			meOpsWithAnAccountID = append(meOpsWithAnAccountID, m.Name)
		}
	}
	sort.Strings(meOpsWithAnAccountID)
	assert.NotEmpty(t, meOpsWithAnAccountID,
		"/me operations taking an account id: %v", meOpsWithAnAccountID)
	for _, name := range meOpsWithAnAccountID {
		assert.NotEmptyf(t, meOperationsNamingAnAccount[name],
			"%s names an account outside its path and nothing says which helper resolves it", name)
	}

	// And the consequence, end to end: a SUPPORT_READ_ONLY operator -- the
	// least-privileged staff role, holding operatorBase and nothing else.
	h := newHarness(t)
	support := security.Principal{
		SubjectID: accounts.NewUserID().String(),
		ActorType: security.ActorOperator,
		Roles:     []security.Role{security.RoleSupportReadOnly},
		SessionID: testSessionID,
		AuthTime:  testNow.Add(-time.Minute),
		AMR:       []string{"pwd", "mfa"},
		// No AccountIDs at all: this operator owns nothing.
	}
	require.NoError(t, support.Validate())
	h.as(&support)

	victim := testAccountID.String()

	// The reads stay reachable, and that is a decision rather than an
	// oversight: account:read_any is the operator READ override this system
	// grants on purpose, to a role a person was given in a directory with a
	// reason attached, and nothing behind these three moves. The same facts are
	// on the admin plane.
	for _, path := range []string{
		"/v1/me/payout-destinations?account_id=" + victim,
		"/v1/me/verification?account_id=" + victim,
		"/v1/me/eligibility?account_id=" + victim,
	} {
		res := h.do(http.MethodGet, path, nil)
		assert.Equalf(t, http.StatusOK, res.Code,
			"%s is a read behind account:read_any and should still answer; body=%s", path, res.Body.String())
	}

	// The sharpest one, which is now refused: GetMeVerificationSessionsSessionId
	// is a GET that CHANGES STATE (it polls the provider and records the
	// outcome), so it scopes with accountScopeWrite -- ownership only -- and the
	// same operator can no longer drive another person's verification forward.
	res := h.do(http.MethodGet,
		"/v1/me/verification/sessions/"+testSessionID+"?account_id="+victim, nil)
	assert.Equal(t, http.StatusForbidden, res.Code,
		"a state-changing /me GET was reachable for an account the caller does not own; body=%s", res.Body.String())
}

// F-178. The allowlist in handlers_profile_test.go annotated
// /v1/me/verification/sessions/{sessionId} as "internal/verification: a session
// is loaded under the caller's profile; another person's id is not found". The
// first half was true and the second was not: the account the session was
// loaded under came from accountScope, so "the caller's profile" meant
// "whatever profile an account:read_any holder names".
//
// The comment is now true of the code, because the code changed: the account
// comes from accountScopeWrite, which has no override.
func TestAudit_TheMeAllowlistCommentIsWrongAboutVerificationSessions(t *testing.T) {
	t.Parallel()
	require.True(t, meRoutesNamingTheCallersOwnObject["/v1/me/verification/sessions/{sessionId}"],
		"the allowlist entry this test is about has moved")
	require.Contains(t, meOperationsNamingAnAccount["GetMeVerificationSessionsSessionId"], "accountScopeWrite",
		"the operation-level entry no longer claims the ownership-only helper")

	h := newHarness(t)
	support := security.Principal{
		SubjectID: accounts.NewUserID().String(),
		ActorType: security.ActorOperator,
		Roles:     []security.Role{security.RoleSupportReadOnly},
		SessionID: testSessionID,
		AuthTime:  testNow.Add(-time.Minute),
		AMR:       []string{"pwd", "mfa"},
	}
	h.as(&support)

	res := h.do(http.MethodGet,
		"/v1/me/verification/sessions/"+testSessionID+"?account_id="+testAccountID.String(), nil)
	assert.Equal(t, http.StatusForbidden, res.Code,
		"another account's verification session was served on a /me route; body=%s", res.Body.String())
}

// Coverage probe (no defect expected): every mutating accounts-auth route is
// behind the CSRF guard when the request carries the session cookie. The
// existing suite proves this on /v1/intents only.
func TestAudit_CSRFCoversTheAccountsAuthCommands(t *testing.T) {
	t.Parallel()
	h := newProfileHarnessForCSRF(t)
	p := customerPrincipal()
	h.as(&p)

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/me/profile", map[string]any{"display_name": "Ada"}},
		{http.MethodPost, "/v1/me/terms-acceptances", map[string]any{"document_ids": []string{"TERMS_OF_SERVICE"}}},
		{http.MethodPost, "/v1/me/account/close", map[string]any{}},
		{http.MethodPost, "/v1/me/account/close/cancel", map[string]any{}},
		{
			http.MethodPost, "/v1/admin/users/" + testUserID.String() + "/closure",
			map[string]any{"decision": "REFUSE", "reason": "an unsettled payout"},
		},
		{http.MethodPost, "/v1/auth/logout", nil},
		{http.MethodDelete, "/v1/sessions/" + testSessionID, nil},
	} {
		res := h.do(tc.method, tc.path, tc.body,
			"Idempotency-Key", "csrf-probe-0000001",
			"Cookie", "cp_session=token", "Origin", "https://evil.test")
		assert.Equalf(t, http.StatusForbidden, res.Code,
			"%s %s was not refused as a cross-site request; body=%s", tc.method, tc.path, res.Body.String())
	}
}

func profileAccountViewForCSRF() profile.AccountView {
	return profile.AccountView{UserID: testUserID.String(), UserStatus: "ACTIVE", CoolingOff: profile.DefaultCoolingOff}
}

func newProfileHarnessForCSRF(t *testing.T) *harness {
	t.Helper()
	h, _ := newProfileHarness(t, &fakeProfile{
		prof: sampleProfile(),
		view: profileAccountViewForCSRF(),
	})
	return h
}

// F-179. EFFECT is the one irreversible operator action on the support surface:
// it writes users.status = CLOSED, closes every account the person owns and
// revokes every session, after which identity.Complete refuses the login, so
// nothing the account still holds is reachable by its owner again.
//
// internal/profile.Decide checked exactly three things before doing it: that a
// PENDING request exists, that the operator is not its subject, and that the
// cooling-off period had passed. It read no balance, no open payout request and
// no open position -- and neither did the view the operator decided from.
// migration 00758 and internal/profile/closure.go both say REFUSED exists for
// "an unsettled payout, an open dispute, or a balance to deal with first", and
// the operator was given none of those three facts.
//
// The decision surface carries all three now. This is the wire half; the
// service half is TestAudit_EffectingAClosureChecksNothingFinancial and the two
// blocker tests beside it in internal/profile.
func TestAudit_TheClosureDecisionSurfaceCarriesNoFinancialFact(t *testing.T) {
	t.Parallel()
	money := []string{"balance", "credit", "payout", "position"}
	seen := map[string]bool{}
	var walk func(reflect.Type, int)
	walk = func(tp reflect.Type, depth int) {
		for tp.Kind() == reflect.Pointer || tp.Kind() == reflect.Slice {
			tp = tp.Elem()
		}
		if tp.Kind() != reflect.Struct || depth > 3 {
			return
		}
		for i := 0; i < tp.NumField(); i++ {
			f := tp.Field(i)
			seen[strings.ToLower(f.Name)] = true
			walk(f.Type, depth+1)
		}
	}
	walk(reflect.TypeOf(api.AdminUserView{}), 0)
	require.NotEmpty(t, seen)
	for _, word := range money {
		var found bool
		for name := range seen {
			if strings.Contains(name, word) {
				found = true
				break
			}
		}
		assert.Truef(t, found,
			"AdminUserView carries no %s field, so the operator decides an irreversible closure without it", word)
	}

	// And the values reach the wire, rather than the field merely existing.
	view := profile.AdminUserView{
		UserID: testUserID.String(), UserStatus: "ACTIVE", AuditStream: "account:x",
		Blockers: profile.ClosureBlockers{CreditBalance: "900", OpenPayoutRequests: 1, OpenNativePositions: 2},
	}
	got := toAPIAdminUser(view, testNow)
	assert.False(t, got.ClosureBlockers.Clear)
	assert.Equal(t, "900", got.ClosureBlockers.CreditBalance)
	assert.Equal(t, 1, got.ClosureBlockers.OpenPayoutRequests)
	assert.Equal(t, 2, got.ClosureBlockers.OpenNativePositions)
	require.Len(t, got.ClosureBlockers.Reasons, 3)

	// A clear account says so, with a zero balance rather than an empty string
	// a console would render as a blank.
	clear := toAPIAdminUser(profile.AdminUserView{UserID: testUserID.String(), UserStatus: "ACTIVE"}, testNow)
	assert.True(t, clear.ClosureBlockers.Clear)
	assert.Equal(t, "0", clear.ClosureBlockers.CreditBalance)
	assert.Empty(t, clear.ClosureBlockers.Reasons)
}

// F-accounts-auth-12, decided as D-103. The login-state cookie is a single
// slot: SetLoginState writes one name at Path=/, so a second GET /v1/auth/login
// overwrites the first flow's digest. Two sign-in tabs, or a sign-in started and
// then a step-up begun in another tab, leave the older tab's callback answering
// 401 -- and the message described an attack when what happened is two tabs.
//
// The slot stays one slot: that is F-87's control and it is what makes a planted
// callback unusable. The copy names the likely cause first.
func TestAudit_ASecondLoginInvalidatesTheFirstTabsFlow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)

	tabA := h.do(http.MethodGet, "/v1/auth/login", nil)
	require.Equal(t, http.StatusFound, tabA.Code)
	stateA := namedCookie(tabA, httpmw.LoginStateCookieName)
	require.NotNil(t, stateA)

	tabB := h.do(http.MethodGet, "/v1/auth/login?step_up=true", nil)
	require.Equal(t, http.StatusFound, tabB.Code)
	stateB := namedCookie(tabB, httpmw.LoginStateCookieName)
	require.NotNil(t, stateB)
	require.Equal(t, stateA.Name, stateB.Name, "one cookie name, one slot")

	// The browser now holds tab B's value. Tab A comes back from the provider.
	res := h.doWithCookies(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil,
		[]*http.Cookie{{Name: stateB.Name, Value: httpmw.LoginStateDigest("tab-a-state")}})
	assert.Equal(t, http.StatusUnauthorized, res.Code,
		"the older tab's callback is refused because the newer flow overwrote the slot")
	body := res.Body.String()
	assert.Contains(t, body, "a newer sign-in replaced it",
		"the refusal must name the likely cause -- a second tab -- and not only the attack")
	assert.Contains(t, body, "start again")
}

// F-177 (boundary half). auth.Manager.Rotate had no caller anywhere in the
// repository because identity.Complete was never told which session the browser
// already held. The callback reads it from the session middleware and hands it
// over; internal/identity decides whether to rotate.
func TestAudit_TheCallbackHandsTheCurrentSessionToTheLoginService(t *testing.T) {
	t.Parallel()

	// A cold sign-in: no session on the request, nothing handed over.
	h := newHarness(t)
	h.as(nil)
	begin := h.do(http.MethodGet, "/v1/auth/login?step_up=true", nil)
	require.Equal(t, http.StatusFound, begin.Code)
	res := h.doWithCookies(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil, begin.Result().Cookies())
	require.Equal(t, http.StatusFound, res.Code, "body=%s", res.Body.String())
	assert.Nil(t, h.ports.identity.lastComplete.Current,
		"a callback with no session invented one")

	// And with one: the authenticated harness resolves a session, and it is the
	// one the login service is given.
	h2 := newHarness(t)
	p := customerPrincipal()
	live := auth.Session{
		ID: testSessionID, SubjectID: p.SubjectID, ActorType: p.ActorType,
		Roles: p.Roles, AccountIDs: p.AccountIDs,
		CreatedAt: testNow.Add(-time.Hour), LastSeenAt: testNow, ExpiresAt: testNow.Add(time.Hour),
		AuthTime: p.AuthTime, AMR: p.AMR,
	}
	h2.as(&p).withSession(&live)
	begin2 := h2.do(http.MethodGet, "/v1/auth/login?step_up=true", nil)
	require.Equal(t, http.StatusFound, begin2.Code)
	res2 := h2.doWithCookies(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil, begin2.Result().Cookies())
	require.Equal(t, http.StatusFound, res2.Code, "body=%s", res2.Body.String())
	current := h2.ports.identity.lastComplete.Current
	require.NotNil(t, current, "the callback did not hand over the session the browser holds")
	assert.Equal(t, p.SubjectID, current.SubjectID)
	assert.Equal(t, live.ID, current.ID, "the callback handed over a different session")
}
