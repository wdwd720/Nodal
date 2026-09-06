//go:build integration

package security

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cookieName is what cmd/api sets when CP_AUTH_COOKIE_SECURE is false. The
// __Host- prefix is only added for secure cookies.
const cookieName = "cp_session"

// noRedirect is the client used everywhere: the login flow is asserted on its
// 302s, so redirects must never be followed.
var noRedirect = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// response is one answer from the API, fully read so the connection is
// released before any assertion runs.
type response struct {
	Status  int
	Body    []byte
	Header  http.Header
	Problem problem
}

// problem mirrors the fields of application/problem+json this suite asserts on.
type problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail"`
	Instance  string         `json:"instance"`
	Code      string         `json:"code"`
	Fields    map[string]any `json:"fields"`
	RequestID string         `json:"request_id"`
}

func (r response) text() string { return string(r.Body) }

// do issues one request and reads the whole answer.
func do(t *testing.T, req *http.Request) response {
	t.Helper()
	resp, err := noRedirect.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := response{Status: resp.StatusCode, Body: body, Header: resp.Header.Clone()}
	if len(body) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "problem+json") {
		_ = json.Unmarshal(body, &out.Problem)
	}
	assertNotThrottled(t, out, req.Method+" "+req.URL.String())
	return out
}

// assertNotThrottled fails loudly when a probe was rate limited. A 429 in
// place of an authorization answer would make every status assertion in this
// suite meaningless, so it is never tolerated silently.
func assertNotThrottled(t *testing.T, r response, what string) {
	t.Helper()
	require.NotEqual(t, http.StatusTooManyRequests, r.Status,
		"%s was rate limited; the suite's own limits are misconfigured and every status assertion below it is unsound", what)
}

func getAs(t *testing.T, token, path string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, apiBaseURL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Cookie", cookieName+"="+token)
	}
	return do(t, req)
}

// getRaw issues a GET to an absolute path without touching url.URL's cleaning,
// so duplicated segments and percent-encoded traversal reach the server as
// written.
func getRawAs(t *testing.T, token, rawPath string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, apiBaseURL+"/", nil)
	require.NoError(t, err)
	req.URL.Opaque = rawPath
	req.URL.Path = ""
	req.URL.RawPath = ""
	if token != "" {
		req.Header.Set("Cookie", cookieName+"="+token)
	}
	return do(t, req)
}

// postAs issues a mutating request with the two headers every command needs:
// Sec-Fetch-Site (the CSRF control is Fetch Metadata, not a token) and
// Idempotency-Key. An empty key is sent as an absent header.
func postAs(t *testing.T, token, path, idemKey, body string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, apiBaseURL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if token != "" {
		req.Header.Set("Cookie", cookieName+"="+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return do(t, req)
}

// session is an authenticated dev identity.
type session struct {
	Identity  string
	Token     string
	SubjectID string
	Accounts  []string
	Roles     []string
	ActorType string
}

// meDoc is the shape of GET /v1/me.
type meDoc struct {
	SubjectID  string   `json:"subject_id"`
	ActorType  string   `json:"actor_type"`
	Roles      []string `json:"roles"`
	AccountIDs []string `json:"account_ids"`
	AMR        []string `json:"amr"`
}

// login completes a real authorization-code exchange against the dev identity
// provider: GET /v1/auth/login for a state, then GET /v1/auth/callback with
// code=<identity>[:mfa]. It returns the raw state and code too, because the
// replay tests re-send exactly these.
func login(t *testing.T, identity string) (session, string, string) {
	t.Helper()
	state := beginLogin(t)
	resp := completeLogin(t, identity, state)
	require.Equal(t, http.StatusFound, resp.Status, "callback for %s: %s", identity, resp.text())
	token := sessionCookie(t, resp)
	require.NotEmpty(t, token, "callback for %s set no session cookie", identity)

	me := getAs(t, token, "/v1/me")
	require.Equal(t, http.StatusOK, me.Status, "GET /v1/me for %s: %s", identity, me.text())
	var doc meDoc
	require.NoError(t, json.Unmarshal(me.Body, &doc))
	return session{
		Identity: identity, Token: token, SubjectID: doc.SubjectID,
		Accounts: doc.AccountIDs, Roles: doc.Roles, ActorType: doc.ActorType,
	}, identity, state
}

func beginLogin(t *testing.T) string {
	t.Helper()
	resp := getAs(t, "", "/v1/auth/login")
	require.Equal(t, http.StatusFound, resp.Status, "GET /v1/auth/login: %s", resp.text())
	loc := resp.Header.Get("Location")
	require.NotEmpty(t, loc, "login redirect carried no Location")
	idx := strings.Index(loc, "state=")
	require.Positive(t, idx, "login redirect %q carried no state", loc)
	state := loc[idx+len("state="):]
	if amp := strings.IndexByte(state, '&'); amp >= 0 {
		state = state[:amp]
	}
	require.NotEmpty(t, state)
	return state
}

func completeLogin(t *testing.T, code, state string) response {
	t.Helper()
	return getAs(t, "", "/v1/auth/callback?code="+code+"&state="+state)
}

func sessionCookie(t *testing.T, r response) string {
	t.Helper()
	for _, c := range r.Header.Values("Set-Cookie") {
		if !strings.HasPrefix(c, cookieName+"=") {
			continue
		}
		v := strings.TrimPrefix(c, cookieName+"=")
		if semi := strings.IndexByte(v, ';'); semi >= 0 {
			v = v[:semi]
		}
		if v != "" {
			return v
		}
	}
	return ""
}

// mustLogin is login without the state and code, for the common case.
func mustLogin(t *testing.T, identity string) session {
	t.Helper()
	s, _, _ := login(t, identity)
	return s
}

// firstAccount asserts the identity owns exactly one account and returns it.
func firstAccount(t *testing.T, s session) string {
	t.Helper()
	require.Len(t, s.Accounts, 1, "%s should own exactly one account, got %v", s.Identity, s.Accounts)
	return s.Accounts[0]
}

// grantOperatorRole gives a dev identity a standing operator role. The dev IdP
// carries role claims, but internal/identity deliberately ignores them and
// reads the operator directory instead, so a role only exists once it is a row
// in operator_roles. The insert is idempotent, so the suite is re-runnable.
func grantOperatorRole(t *testing.T, subject, role string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tag, err := testPool.Exec(ctx,
		`INSERT INTO operator_roles (user_id, role, reason)
		 SELECT id, $2, 'test/security adversarial suite'
		 FROM users WHERE idp_issuer = 'devidp' AND idp_subject = $1
		 ON CONFLICT (user_id, role) DO UPDATE SET revoked_at = NULL, expires_at = NULL`,
		"dev:"+subject, role)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "no user row for dev:%s; log in as that identity first", subject)
}

// operatorSession logs in once to create the user, grants the role, then logs
// in again so the session carries it.
func operatorSession(t *testing.T, identity, role string) session {
	t.Helper()
	mustLogin(t, identity)
	grantOperatorRole(t, identity, role)
	s := mustLogin(t, identity+":mfa")
	require.Contains(t, s.Roles, role, "%s did not receive the %s role", identity, role)
	require.Equal(t, "OPERATOR", s.ActorType)
	return s
}

// countRows runs a scalar count with the migrate role, which can read
// everything the app writes.
func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var n int
	require.NoError(t, testPool.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

// stringsFrom returns one string column, for assertions about WHICH rows exist
// rather than how many. A count can pass while the wrong actor owns a row.
func stringsFrom(t *testing.T, sql string, args ...any) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := testPool.Query(ctx, sql, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// key builds an Idempotency-Key that is unique to this run, so a second run
// against the same database never replays the first run's records.
func key(name string) string { return "advsec-" + runToken + "-" + name }
