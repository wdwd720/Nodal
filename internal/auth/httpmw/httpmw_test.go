package httpmw_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/authtest"
	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

const cookieName = "cp_session"

type fixture struct {
	clk   *authtest.Clock
	store *authtest.MemorySessionStore
	mgr   *auth.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := authtest.NewClock(t0)
	store := authtest.NewMemorySessionStore(clk.Now)
	mgr, err := auth.NewManager(store, auth.ManagerConfig{TTL: time.Hour, IdleTimeout: 30 * time.Minute, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{clk: clk, store: store, mgr: mgr}
}

func (f *fixture) issue(t *testing.T, p auth.IssueParams) auth.Issued {
	t.Helper()
	iss, err := f.mgr.Issue(context.Background(), nil, p)
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

func customerParams(sub string) auth.IssueParams {
	return auth.IssueParams{
		SubjectID: sub, ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{"acct-" + sub}, AuthTime: t0, AMR: []string{"pwd", "mfa"},
	}
}

func operatorParams(sub string, roles ...security.Role) auth.IssueParams {
	return auth.IssueParams{SubjectID: sub, ActorType: security.ActorOperator, Roles: roles, AuthTime: t0, AMR: []string{"pwd"}}
}

// echo records what the downstream handler saw.
type seen struct {
	called    bool
	principal security.Principal
	hasP      bool
	session   auth.Session
	hasS      bool
}

func echo(s *seen) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.called = true
		s.principal, s.hasP = security.PrincipalFrom(r.Context())
		s.session, s.hasS = httpmw.SessionFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func do(h http.Handler, method, path string, headers map[string]string, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type problemBody struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Title  string `json:"title"`
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type %q, body %s", ct, w.Body.String())
	}
	var p problemBody
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem json: %v: %s", err, w.Body.String())
	}
	if p.Status != w.Code {
		t.Fatalf("problem status %d vs http %d", p.Status, w.Code)
	}
	return p
}

func clearedCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge < 0 && c.Value == "" {
			return c
		}
	}
	return nil
}

func TestSession_NoCookieIsAnonymous(t *testing.T) {
	f := newFixture(t)
	var s seen
	h := httpmw.Session(f.mgr, nil, cookieName)(echo(&s))
	w := do(h, http.MethodGet, "/", nil, "")
	if w.Code != 200 || !s.called || s.hasP || s.hasS {
		t.Fatalf("code %d called %v principal %v", w.Code, s.called, s.hasP)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("cookie written for an anonymous request")
	}
}

func TestSession_ValidCookieAttachesPrincipal(t *testing.T) {
	f := newFixture(t)
	iss := f.issue(t, customerParams("alice"))
	var s seen
	h := httpmw.Session(f.mgr, nil, cookieName)(echo(&s))
	w := do(h, http.MethodGet, "/", nil, iss.Token)
	if w.Code != 200 || !s.hasP || !s.hasS {
		t.Fatalf("code %d principal %v session %v", w.Code, s.hasP, s.hasS)
	}
	if s.principal.SubjectID != "alice" || s.principal.SessionID != iss.Session.ID || !s.principal.HasRole(security.RoleCustomer) {
		t.Fatalf("principal %+v", s.principal)
	}
	if s.session.ID != iss.Session.ID || s.session.TokenHash != auth.HashToken(iss.Token) {
		t.Fatalf("session %+v", s.session)
	}
	if strings.Contains(w.Body.String(), iss.Token) {
		t.Fatal("token echoed in response")
	}
}

// tamperToken changes a character in the middle of the token so the decoded
// bytes are guaranteed to differ. Replacing the final base64url character is
// not enough: it carries only two significant bits, so one time in four the
// "tampered" token decodes to the original and the case is flaky.
func tamperToken(tok string) string {
	b := []byte(tok)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestSession_BadCookiesAreAnonymousAndCleared(t *testing.T) {
	f := newFixture(t)
	iss := f.issue(t, customerParams("alice"))
	revoked := f.issue(t, customerParams("bob"))
	_ = f.mgr.Revoke(context.Background(), nil, revoked.Session.ID)
	unknown, _ := auth.NewToken()
	expired := f.issue(t, customerParams("carol"))

	cases := map[string]struct {
		token  string
		before func()
	}{
		"garbage":  {"not-a-token", nil},
		"unknown":  {unknown, nil},
		"revoked":  {revoked.Token, nil},
		"expired":  {expired.Token, func() { f.clk.Set(t0.Add(2 * time.Hour)) }},
		"tampered": {tamperToken(iss.Token), nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f.clk.Set(t0)
			if tc.before != nil {
				tc.before()
			}
			var s seen
			h := httpmw.Session(f.mgr, nil, cookieName)(echo(&s))
			w := do(h, http.MethodGet, "/", nil, tc.token)
			if w.Code != 200 || !s.called || s.hasP {
				t.Fatalf("code %d called %v principal %v", w.Code, s.called, s.hasP)
			}
			c := clearedCookie(t, w, cookieName)
			if c == nil {
				t.Fatalf("stale cookie not cleared: %v", w.Result().Cookies())
			}
			if !c.HttpOnly || c.Path != "/" {
				t.Fatalf("clearing cookie attributes: %+v", c)
			}
		})
	}
}

func TestSession_StoreOutageFailsClosed(t *testing.T) {
	f := newFixture(t)
	iss := f.issue(t, customerParams("alice"))
	f.store.FailNext(errors.New("connection refused"))
	var s seen
	h := httpmw.Session(f.mgr, nil, cookieName)(echo(&s))
	w := do(h, http.MethodGet, "/", nil, iss.Token)
	if w.Code != http.StatusServiceUnavailable || s.called {
		t.Fatalf("code %d called %v", w.Code, s.called)
	}
	if p := problemOf(t, w); p.Code != httpmw.CodeInternal {
		t.Fatalf("problem %+v", p)
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Fatal("internal error text leaked")
	}
	if clearedCookie(t, w, cookieName) != nil {
		t.Fatal("cookie cleared on an outage")
	}
}

func TestSession_IgnoresClientAssertedIdentity(t *testing.T) {
	f := newFixture(t)
	var s seen
	h := httpmw.Session(f.mgr, nil, cookieName)(httpmw.RequireAuth(echo(&s)))
	w := do(h, http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer whatever",
		"X-User-Id":     "admin",
		"X-Roles":       "ADMIN",
		"X-Subject":     "alice",
	}, "")
	if w.Code != http.StatusUnauthorized || s.called {
		t.Fatalf("headers granted access: %d", w.Code)
	}
}

func TestRequireAuth(t *testing.T) {
	f := newFixture(t)
	iss := f.issue(t, customerParams("alice"))
	var s seen
	h := httpmw.Session(f.mgr, nil, cookieName)(httpmw.RequireAuth(echo(&s)))

	w := do(h, http.MethodGet, "/v1/me", nil, "")
	if w.Code != http.StatusUnauthorized || s.called {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if p := problemOf(t, w); p.Code != httpmw.CodeUnauthenticated {
		t.Fatalf("problem %+v", p)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 without WWW-Authenticate")
	}
	w = do(h, http.MethodGet, "/v1/me", nil, iss.Token)
	if w.Code != 200 || !s.called {
		t.Fatalf("authenticated: %d", w.Code)
	}
}

func TestRequireRole(t *testing.T) {
	f := newFixture(t)
	customer := f.issue(t, customerParams("alice"))
	admin := f.issue(t, operatorParams("root", security.RoleAdmin))
	ops := f.issue(t, operatorParams("ops", security.RoleOperations))
	agent := f.issue(t, auth.IssueParams{SubjectID: "agent-1", ActorType: security.ActorAgent, AccountIDs: []string{"acct-1"}})
	until := t0.Add(10 * time.Minute)
	bg := f.issue(t, auth.IssueParams{SubjectID: "bg", ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin, security.RoleBreakGlass}, BreakGlassUntil: &until})

	guard := httpmw.RequireRoleAt(f.clk.Now, security.RoleAdmin, security.RoleOperations)
	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"anonymous", "", 401},
		{"customer", customer.Token, 403},
		{"admin", admin.Token, 200},
		{"operations", ops.Token, 200},
		{"agent", agent.Token, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s seen
			h := httpmw.Session(f.mgr, nil, cookieName)(guard(echo(&s)))
			w := do(h, http.MethodGet, "/v1/admin", nil, tc.token)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 {
				problemOf(t, w)
			}
		})
	}
	t.Run("break glass live then expired", func(t *testing.T) {
		var s seen
		h := httpmw.Session(f.mgr, nil, cookieName)(httpmw.RequireRoleAt(f.clk.Now, security.RoleBreakGlass)(echo(&s)))
		if w := do(h, http.MethodGet, "/", nil, bg.Token); w.Code != 200 {
			t.Fatalf("live break-glass refused: %d", w.Code)
		}
		f.clk.Set(until)
		if w := do(h, http.MethodGet, "/", nil, bg.Token); w.Code != 403 {
			t.Fatalf("expired break-glass accepted: %d", w.Code)
		}
	})
	t.Run("wall clock wrapper", func(t *testing.T) {
		var s seen
		h := httpmw.Session(f.mgr, nil, cookieName)(httpmw.RequireRole(security.RoleAdmin)(echo(&s)))
		f.clk.Set(t0)
		if w := do(h, http.MethodGet, "/", nil, admin.Token); w.Code != 200 {
			t.Fatalf("admin refused: %d", w.Code)
		}
	})
}

func TestRequirePermissionAndStepUp(t *testing.T) {
	f := newFixture(t)
	customer := f.issue(t, customerParams("alice"))
	ops := f.issue(t, operatorParams("ops", security.RoleOperations))
	agent := f.issue(t, auth.IssueParams{SubjectID: "agent-1", ActorType: security.ActorAgent, AccountIDs: []string{"acct-1"}})

	perm := httpmw.RequirePermissionAt(security.PermKillActivate, f.clk.Now)
	for name, tc := range map[string]struct {
		token string
		want  int
		code  string
	}{
		"anonymous": {"", 401, httpmw.CodeUnauthenticated},
		"customer":  {customer.Token, 403, httpmw.CodeForbidden},
		"agent":     {agent.Token, 403, httpmw.CodeForbidden},
		"ops":       {ops.Token, 200, ""},
	} {
		t.Run("permission "+name, func(t *testing.T) {
			var s seen
			h := httpmw.Session(f.mgr, nil, cookieName)(perm(echo(&s)))
			w := do(h, http.MethodPost, "/v1/kill", nil, tc.token)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d", w.Code, tc.want)
			}
			if tc.code != "" && problemOf(t, w).Code != tc.code {
				t.Fatalf("code %s", problemOf(t, w).Code)
			}
		})
	}

	stepUp := httpmw.RequireStepUp(5*time.Minute, f.clk.Now)
	t.Run("step-up fresh mfa", func(t *testing.T) {
		var s seen
		h := httpmw.Session(f.mgr, nil, cookieName)(stepUp(echo(&s)))
		if w := do(h, http.MethodPost, "/v1/withdraw", nil, customer.Token); w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("step-up stale", func(t *testing.T) {
		f.clk.Set(t0.Add(10 * time.Minute))
		defer f.clk.Set(t0)
		var s seen
		h := httpmw.Session(f.mgr, nil, cookieName)(stepUp(echo(&s)))
		w := do(h, http.MethodPost, "/v1/withdraw", nil, customer.Token)
		if w.Code != 403 || problemOf(t, w).Code != httpmw.CodeStepUpRequired {
			t.Fatalf("code %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("step-up password only", func(t *testing.T) {
		var s seen
		h := httpmw.Session(f.mgr, nil, cookieName)(stepUp(echo(&s)))
		w := do(h, http.MethodPost, "/v1/withdraw", nil, ops.Token)
		if w.Code != 403 || problemOf(t, w).Code != httpmw.CodeStepUpRequired {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("step-up anonymous", func(t *testing.T) {
		var s seen
		h := httpmw.Session(f.mgr, nil, cookieName)(httpmw.RequireStepUp(time.Minute, nil)(echo(&s)))
		if w := do(h, http.MethodPost, "/v1/withdraw", nil, ""); w.Code != 401 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestCSRF_Matrix(t *testing.T) {
	opts := httpmw.CSRFOptions{AllowedOrigins: []string{"https://app.example.com", "http://localhost:3000", "not a url"}}
	if err := (httpmw.CSRFOptions{AllowedOrigins: []string{"https://ok.example"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := opts.Validate(); err == nil {
		t.Fatal("malformed origin passed Validate")
	}
	mw := httpmw.CSRF(opts)
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"GET plain", "GET", nil, 200},
		{"HEAD plain", "HEAD", nil, 200},
		{"OPTIONS plain", "OPTIONS", nil, 200},
		{"POST no headers", "POST", nil, 403},
		{"PUT no headers", "PUT", nil, 403},
		{"PATCH no headers", "PATCH", nil, 403},
		{"DELETE no headers", "DELETE", nil, 403},
		{"POST sfs same-origin", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"POST sfs none", "POST", map[string]string{"Sec-Fetch-Site": "none"}, 200},
		{"POST sfs cross-site", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"POST sfs same-site", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"POST sfs cross-site allowed origin", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://app.example.com"}, 200},
		{"POST sfs same-site allowed origin", "POST", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://localhost:3000"}, 200},
		{"POST sfs cross-site evil origin", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"POST sfs same-origin with evil origin header", "POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://evil.example"}, 200},
		{"POST origin allowed", "POST", map[string]string{"Origin": "https://app.example.com"}, 200},
		{"POST origin allowed mixed case", "POST", map[string]string{"Origin": "HTTPS://App.Example.COM"}, 200},
		{"POST origin allowed with trailing slash", "POST", map[string]string{"Origin": "https://app.example.com/"}, 200},
		{"POST origin wrong scheme", "POST", map[string]string{"Origin": "http://app.example.com"}, 403},
		{"POST origin wrong port", "POST", map[string]string{"Origin": "https://app.example.com:8443"}, 403},
		{"POST origin subdomain", "POST", map[string]string{"Origin": "https://evil.app.example.com"}, 403},
		{"POST origin suffix attack", "POST", map[string]string{"Origin": "https://app.example.com.evil.example"}, 403},
		{"POST origin null", "POST", map[string]string{"Origin": "null"}, 403},
		{"POST origin garbage", "POST", map[string]string{"Origin": "::not-an-origin::"}, 403},
		{"POST legacy xhr + allowed referer", "POST", map[string]string{"X-Requested-With": "XMLHttpRequest", "Referer": "https://app.example.com/page?x=1"}, 200},
		{"POST legacy xhr + evil referer", "POST", map[string]string{"X-Requested-With": "XMLHttpRequest", "Referer": "https://evil.example/app.example.com"}, 403},
		{"POST legacy xhr no referer", "POST", map[string]string{"X-Requested-With": "XMLHttpRequest"}, 403},
		{"POST legacy referer without xhr", "POST", map[string]string{"Referer": "https://app.example.com/page"}, 403},
		{"POST legacy xhr wrong value", "POST", map[string]string{"X-Requested-With": "fetch", "Referer": "https://app.example.com/page"}, 403},
		{"POST legacy fallback ignored when sfs present", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "X-Requested-With": "XMLHttpRequest", "Referer": "https://app.example.com/page"}, 403},
		{"POST legacy fallback ignored when origin present", "POST", map[string]string{"Origin": "https://evil.example", "X-Requested-With": "XMLHttpRequest", "Referer": "https://app.example.com/page"}, 403},
		{"POST host header is not trusted", "POST", map[string]string{"Host": "app.example.com", "Referer": "https://app.example.com/"}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s seen
			w := do(mw(echo(&s)), tc.method, "/v1/orders", tc.headers, "")
			if w.Code != tc.want {
				t.Fatalf("code %d want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if (w.Code == 200) != s.called {
				t.Fatal("handler invocation inconsistent with status")
			}
			if w.Code == 403 && problemOf(t, w).Code != httpmw.CodeForbidden {
				t.Fatal("wrong problem code")
			}
		})
	}
	t.Run("empty allow-list only accepts sec-fetch-site", func(t *testing.T) {
		strict := httpmw.CSRF(httpmw.CSRFOptions{})
		var s seen
		if w := do(strict(echo(&s)), "POST", "/", map[string]string{"Origin": "https://app.example.com"}, ""); w.Code != 403 {
			t.Fatalf("code %d", w.Code)
		}
		if w := do(strict(echo(&s)), "POST", "/", map[string]string{"Sec-Fetch-Site": "same-origin"}, ""); w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestSecureHeaders(t *testing.T) {
	var s seen
	w := do(httpmw.SecureHeaders(httpmw.SecureHeadersOptions{Secure: true})(echo(&s)), "GET", "/", nil, "")
	h := w.Header()
	want := map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            httpmw.DefaultReferrerPolicy,
		"Permissions-Policy":         httpmw.DefaultPermissionsPolicy,
		"Content-Security-Policy":    "frame-ancestors 'none'",
		"X-Frame-Options":            "DENY",
		"Cache-Control":              "no-store",
		"Strict-Transport-Security":  "max-age=31536000; includeSubDomains",
		"Cross-Origin-Opener-Policy": "same-origin",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	w = do(httpmw.SecureHeaders(httpmw.SecureHeadersOptions{})(echo(&s)), "GET", "/", nil, "")
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS emitted on an insecure deployment")
	}
	w = do(httpmw.SecureHeaders(httpmw.SecureHeadersOptions{Secure: true, HSTSPreload: true, HSTSExcludeSubdomains: true, HSTSMaxAge: time.Hour, NoCacheControl: true})(echo(&s)), "GET", "/", nil, "")
	if got := w.Header().Get("Strict-Transport-Security"); got != "max-age=3600; preload" {
		t.Errorf("HSTS %q", got)
	}
	if w.Header().Get("Cache-Control") != "" {
		t.Error("Cache-Control set despite NoCacheControl")
	}
	// Handlers can override a default.
	override := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Cache-Control", "public, max-age=60") })
	w = do(httpmw.SecureHeaders(httpmw.SecureHeadersOptions{})(override), "GET", "/static", nil, "")
	if w.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Error("handler override lost")
	}
}

func TestCookieHelpers(t *testing.T) {
	cases := []struct {
		name, domain string
		secure       bool
		wantName     string
		wantDomain   string
	}{
		{"cp_session", "", true, "__Host-cp_session", ""},
		{"cp_session", "", false, "cp_session", ""},
		{"cp_session", "example.com", true, "cp_session", "example.com"},
		{"cp_session", "example.com", false, "cp_session", "example.com"},
		{"__Host-cp_session", "", true, "__Host-cp_session", ""},
	}
	for _, tc := range cases {
		t.Run(tc.wantName+"/"+tc.domain, func(t *testing.T) {
			if got := httpmw.EffectiveCookieName(tc.name, tc.domain, tc.secure); got != tc.wantName {
				t.Fatalf("EffectiveCookieName = %q, want %q", got, tc.wantName)
			}
			w := httptest.NewRecorder()
			httpmw.SetSessionCookie(w, tc.name, "tok", tc.domain, tc.secure, 90*time.Minute)
			cookies := w.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies %v", cookies)
			}
			c := cookies[0]
			if c.Name != tc.wantName || c.Value != "tok" || !c.HttpOnly || c.Secure != tc.secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
				t.Fatalf("cookie %+v", c)
			}
			if c.Domain != tc.wantDomain || c.MaxAge != 5400 {
				t.Fatalf("domain %q max-age %d", c.Domain, c.MaxAge)
			}
			if strings.HasPrefix(c.Name, "__Host-") && (c.Domain != "" || !c.Secure || c.Path != "/") {
				t.Fatalf("__Host- invariants violated: %+v", c)
			}
			w = httptest.NewRecorder()
			httpmw.ClearSessionCookie(w, tc.name, tc.domain, tc.secure)
			cleared := w.Result().Cookies()[0]
			if cleared.Name != tc.wantName || cleared.MaxAge != -1 || cleared.Value != "" || cleared.Domain != tc.wantDomain || cleared.Path != "/" {
				t.Fatalf("clear cookie %+v", cleared)
			}
		})
	}
	w := httptest.NewRecorder()
	httpmw.SetSessionCookie(w, "s", "tok", "", false, 0)
	if c := w.Result().Cookies()[0]; c.MaxAge != 0 {
		t.Fatalf("session cookie should have no max-age: %d", c.MaxAge)
	}
}

func TestChiIntegration(t *testing.T) {
	f := newFixture(t)
	alice := f.issue(t, customerParams("alice"))
	admin := f.issue(t, operatorParams("root", security.RoleAdmin))
	name := httpmw.EffectiveCookieName(cookieName, "", false)

	r := chi.NewRouter()
	r.Use(httpmw.SecureHeaders(httpmw.SecureHeadersOptions{}))
	r.Use(httpmw.CSRF(httpmw.CSRFOptions{AllowedOrigins: []string{"http://localhost:3000"}}))
	r.Use(httpmw.Session(f.mgr, nil, name))
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	r.Group(func(r chi.Router) {
		r.Use(httpmw.RequireAuth)
		r.Get("/v1/me", func(w http.ResponseWriter, r *http.Request) {
			p, _ := security.PrincipalFrom(r.Context())
			_, _ = w.Write([]byte(p.SubjectID))
		})
		r.Post("/v1/accounts/{id}/orders", func(w http.ResponseWriter, r *http.Request) {
			if err := security.RequireAccount(r.Context(), chi.URLParam(r, "id")); err != nil {
				w.WriteHeader(http.StatusNotFound) // tenant violations look like not-found
				return
			}
			if err := security.Require(r.Context(), security.PermTradeCreate); err != nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusCreated)
		})
		r.With(httpmw.RequireRoleAt(f.clk.Now, security.RoleAdmin)).Get("/v1/admin", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	})

	get := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: name, Value: token})
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	post := func(path, token string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if token != "" {
			req.AddCookie(&http.Cookie{Name: name, Value: token})
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if w := get("/health", ""); w.Code != 200 || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("health %d", w.Code)
	}
	if w := get("/v1/me", ""); w.Code != 401 {
		t.Fatalf("anonymous me %d", w.Code)
	}
	if w := get("/v1/me", alice.Token); w.Code != 200 || w.Body.String() != "alice" {
		t.Fatalf("me %d %s", w.Code, w.Body.String())
	}
	if w := get("/v1/admin", alice.Token); w.Code != 403 {
		t.Fatalf("customer admin %d", w.Code)
	}
	if w := get("/v1/admin", admin.Token); w.Code != 200 {
		t.Fatalf("admin %d", w.Code)
	}
	sameOrigin := map[string]string{"Sec-Fetch-Site": "same-origin"}
	if w := post("/v1/accounts/acct-alice/orders", alice.Token, nil); w.Code != 403 {
		t.Fatalf("csrf should block: %d", w.Code)
	}
	if w := post("/v1/accounts/acct-alice/orders", alice.Token, sameOrigin); w.Code != 201 {
		t.Fatalf("own account order %d", w.Code)
	}
	if w := post("/v1/accounts/acct-bob/orders", alice.Token, sameOrigin); w.Code != 404 {
		t.Fatalf("cross-tenant order %d", w.Code)
	}
	support := f.issue(t, operatorParams("sup", security.RoleSupportReadOnly))
	if w := post("/v1/accounts/acct-bob/orders", support.Token, sameOrigin); w.Code != 403 {
		t.Fatalf("support can see the account but must not trade on it: %d", w.Code)
	}
	if w := post("/v1/accounts/acct-alice/orders", "", sameOrigin); w.Code != 401 {
		t.Fatalf("anonymous order %d", w.Code)
	}
}
