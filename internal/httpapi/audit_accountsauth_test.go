package httpapi

// Adversarial audit (goal §54), area accounts-auth. These tests DEMONSTRATE
// defects; they are expected to fail once the defects are fixed, at which point
// they should be inverted into regressions by the fixer.

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

// F-accounts-auth-6. handlers_profile.go states the invariant as "Every
// `/me/...` route is self-scoped by construction: there is no identifier in the
// path, and the subject comes from the request principal, so a caller cannot
// name somebody else's record. Cross-tenant access is not prevented by a check
// that could be forgotten -- it is unrepresentable."
//
// TestProfile_NoSelfServiceRouteTakesAnIdentifier is the proof, and it walks
// chi route PATTERNS looking for `{`. Six /v1/me operations take the identifier
// in the QUERY STRING or the BODY instead, where the walk cannot see it, and
// five of them resolve it with accountScope -- the READ-grade helper, which
// security.RequireAccount lets any principal holding account:read_any satisfy
// for any account in the system.
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
			for ft.Kind() == reflect.Ptr {
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
	t.Logf("/me operations that name an account outside the path: %v", meOpsWithAnAccountID)

	// And the consequence, end to end: a SUPPORT_READ_ONLY operator -- the
	// least-privileged staff role, holding operatorBase and nothing else --
	// reads a customer's payout destinations through the customer's own route.
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
	res := h.do(http.MethodGet, "/v1/me/payout-destinations?account_id="+victim, nil)
	assert.Equal(t, http.StatusOK, res.Code,
		"a /me route answered for an account the caller does not own; body=%s", res.Body.String())

	res = h.do(http.MethodGet, "/v1/me/verification?account_id="+victim, nil)
	assert.Equal(t, http.StatusOK, res.Code,
		"a /me route answered for an account the caller does not own; body=%s", res.Body.String())

	res = h.do(http.MethodGet, "/v1/me/eligibility?account_id="+victim, nil)
	assert.Equal(t, http.StatusOK, res.Code,
		"a /me route answered for an account the caller does not own; body=%s", res.Body.String())

	// The sharpest one: GetMeVerificationSessionsSessionId is a GET that
	// CHANGES STATE (it polls the provider and records the outcome) and is
	// scoped with the read-grade helper, so the same operator drives another
	// person's verification session forward -- the F-102 shape, on a route the
	// /me walk does not look at because its identifier is a query parameter.
	res = h.do(http.MethodGet,
		"/v1/me/verification/sessions/"+testSessionID+"?account_id="+victim, nil)
	assert.NotEqual(t, http.StatusForbidden, res.Code,
		"a state-changing /me GET was reachable for an account the caller does not own; body=%s", res.Body.String())
}

// F-accounts-auth-7. The allowlist in handlers_profile_test.go annotates
// /v1/me/verification/sessions/{sessionId} as "internal/verification: a session
// is loaded under the caller's profile; another person's id is not found". The
// first half is true and the second is not: the account the session is loaded
// under comes from accountScope, so "the caller's profile" means "whatever
// profile an account:read_any holder names".
func TestAudit_TheMeAllowlistCommentIsWrongAboutVerificationSessions(t *testing.T) {
	t.Parallel()
	require.True(t, meRoutesNamingTheCallersOwnObject["/v1/me/verification/sessions/{sessionId}"],
		"the allowlist entry this test is about has moved")

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
	assert.Equal(t, http.StatusOK, res.Code,
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
		{http.MethodPost, "/v1/admin/users/" + testUserID.String() + "/closure",
			map[string]any{"decision": "REFUSE", "reason": "an unsettled payout"}},
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

// F-accounts-auth-8. EFFECT is the one irreversible operator action on the
// support surface: it writes users.status = CLOSED, closes every account the
// person owns and revokes every session, after which identity.Complete refuses
// the login ("user is not active"), so nothing the account still holds is
// reachable by its owner again.
//
// internal/profile.Decide checks exactly three things before doing it: that a
// PENDING request exists, that the operator is not its subject, and that the
// cooling-off period has passed. It reads no balance, no open payout request
// and no open position -- and neither does the view the operator decides from.
// migration 00758 and internal/profile/closure.go both say REFUSED exists for
// "an unsettled payout, an open dispute, or a balance to deal with first", and
// the operator is given none of those three facts.
func TestAudit_TheClosureDecisionSurfaceCarriesNoFinancialFact(t *testing.T) {
	t.Parallel()
	money := []string{"balance", "credit", "payout", "position", "holding", "reserved", "buyingpower"}
	seen := map[string]bool{}
	var walk func(reflect.Type, int)
	walk = func(tp reflect.Type, depth int) {
		for tp.Kind() == reflect.Ptr || tp.Kind() == reflect.Slice {
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
		for name := range seen {
			assert.NotContainsf(t, name, word,
				"AdminUserView gained a %s field; this test is out of date", word)
		}
	}
}

// F-accounts-auth-12. The login-state cookie is a single slot: SetLoginState
// writes one name at Path=/, so a second GET /v1/auth/login overwrites the
// first flow's digest. Two sign-in tabs, or a sign-in started and then a
// step-up begun in another tab, leave the older tab's callback answering 401
// "this sign-in did not start in this browser; start again from the beginning"
// -- a message that describes an attack when what happened is two tabs.
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
}
