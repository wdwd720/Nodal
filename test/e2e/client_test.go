//go:build integration && e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// clientTimeout bounds every non-streaming request. The SSE test builds its
// own client with no timeout, because a stream that ends is a failed stream.
const clientTimeout = 20 * time.Second

// client speaks HTTP to one apiServer the way a browser does: it never
// follows redirects on its own (the login handshake needs the Location and
// the Set-Cookie of each hop) and it holds no cookie jar, so a test that
// wants a session must pass it explicitly and can therefore keep using a
// revoked one.
type client struct {
	t   *testing.T
	srv *apiServer
	hc  *http.Client
}

func newClient(t *testing.T, srv *apiServer) *client {
	t.Helper()
	return &client{
		t:   t,
		srv: srv,
		hc: &http.Client{
			Timeout: clientTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// session is a signed-in principal. cookieName is read from the Set-Cookie
// header rather than hardcoded: with CP_AUTH_COOKIE_SECURE=true it becomes
// __Host-cp_session, and a test that assumed the name would then pass while
// sending no cookie at all.
type session struct {
	identity   string
	cookieName string
	cookie     string
	sessionID  string
}

// response is a completed HTTP exchange with its body already drained, so no
// caller can leak a connection.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

type option func(*http.Request)

// asSession attaches the session cookie.
func asSession(s session) option {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: s.cookieName, Value: s.cookie})
	}
}

// asRawCookie attaches a cookie by literal name and value, for the cases
// where the value is deliberately not a live session.
func asRawCookie(name, value string) option {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
}

func header(k, v string) option {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

// idempotency sets the Idempotency-Key header the mutating endpoints require.
func idempotency(key string) option { return header("Idempotency-Key", key) }

// noCSRFHeader removes the Sec-Fetch-Site header that do() adds by default to
// unsafe methods.
func noCSRFHeader() option {
	return func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") }
}

func (c *client) get(ctx context.Context, path string, opts ...option) response {
	c.t.Helper()
	return c.do(ctx, http.MethodGet, path, nil, opts...)
}

// postJSON marshals v and posts it. The CSRF middleware refuses any unsafe
// method that carries a session cookie without a first-party signal, so
// Sec-Fetch-Site: same-origin is set by default in do().
func (c *client) postJSON(ctx context.Context, path string, v any, opts ...option) response {
	c.t.Helper()
	body, err := json.Marshal(v)
	require.NoError(c.t, err)
	return c.do(ctx, http.MethodPost, path, body,
		append([]option{header("Content-Type", "application/json")}, opts...)...)
}

func (c *client) postRaw(ctx context.Context, path string, body []byte, opts ...option) response {
	c.t.Helper()
	return c.do(ctx, http.MethodPost, path, body,
		append([]option{header("Content-Type", "application/json")}, opts...)...)
}

func (c *client) do(ctx context.Context, method, path string, body []byte, opts ...option) response {
	c.t.Helper()
	resp, err := c.try(ctx, method, path, body, opts...)
	require.NoErrorf(c.t, err, "%s %s", method, path)
	return resp
}

// try is do() without the assertion, for callers (the concurrency test) that
// must report a transport failure themselves rather than fail the test from
// a goroutine.
func (c *client) try(ctx context.Context, method, path string, body []byte, opts ...option) (response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.srv.baseURL+path, reader)
	if err != nil {
		return response{}, err
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
	default:
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	return response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}, nil
}

// signIn completes the real development OIDC handshake:
//
//	GET /v1/auth/login          → 302 to /auth/dev/login?...&state=S
//	GET /v1/auth/callback?...   → 302 with Set-Cookie
//
// code is the dev identity, optionally suffixed ":mfa" for AMR [pwd, mfa]
// and ACR phr, which the step-up endpoints require.
func (c *client) signIn(ctx context.Context, code string) session {
	c.t.Helper()

	start := c.get(ctx, "/v1/auth/login")
	require.Equalf(c.t, http.StatusFound, start.Status,
		"GET /v1/auth/login should redirect to the identity provider; body: %s", start.Body)
	loc := start.Header.Get("Location")
	require.NotEmpty(c.t, loc, "login redirect carried no Location")

	u, err := url.Parse(loc)
	require.NoErrorf(c.t, err, "parsing login Location %q", loc)
	state := u.Query().Get("state")
	require.NotEmptyf(c.t, state, "login Location %q carried no state", loc)

	cb := c.get(ctx, "/v1/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state))
	require.Equalf(c.t, http.StatusFound, cb.Status,
		"GET /v1/auth/callback should redirect after establishing the session; body: %s", cb.Body)

	name, value := sessionCookie(c.t, cb.Header)
	s := session{identity: code, cookieName: name, cookie: value}

	// The session id is read back from /v1/sessions so a test can assert on
	// the row the server actually created.
	me := c.get(ctx, "/v1/sessions", asSession(s))
	require.Equalf(c.t, http.StatusOK, me.Status, "GET /v1/sessions after sign-in: %s", me.Body)
	var sessions []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	require.NoError(c.t, json.Unmarshal(me.Body, &sessions))
	for _, row := range sessions {
		if row.Current {
			s.sessionID = row.ID
		}
	}
	require.NotEmptyf(c.t, s.sessionID, "no session in /v1/sessions was marked current: %s", me.Body)
	return s
}

// sessionCookie pulls the session cookie out of Set-Cookie by shape rather
// than by name, then returns the name it actually found.
func sessionCookie(t *testing.T, h http.Header) (name, value string) {
	t.Helper()
	for _, raw := range h.Values("Set-Cookie") {
		parts := strings.SplitN(raw, ";", 2)
		kv := strings.SplitN(parts[0], "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.Contains(kv[0], "cp_session") && kv[1] != "" {
			return strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		}
	}
	t.Fatalf("no cp_session cookie in Set-Cookie: %v", h.Values("Set-Cookie"))
	return "", ""
}

// problem decodes an application/problem+json body and asserts the media type.
func (r response) problem(t *testing.T) errs.Problem {
	t.Helper()
	require.Equalf(t, errs.ContentType, mediaType(r.Header.Get("Content-Type")),
		"a refusal must be %s; body: %s", errs.ContentType, r.Body)
	var p errs.Problem
	require.NoErrorf(t, json.Unmarshal(r.Body, &p), "decoding problem body %s", r.Body)
	return p
}

// jsonObject decodes a body into raw members so a test can assert on the JSON
// representation itself — specifically that money is a string and never a
// number.
func (r response) jsonObject(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoErrorf(t, json.Unmarshal(r.Body, &m), "decoding JSON object %s", r.Body)
	return m
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	require.NoErrorf(t, json.Unmarshal(r.Body, v), "decoding %s", r.Body)
}

func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(strings.ToLower(v))
}

// requireJSONString asserts a member is a JSON string with the given value.
// Money is a string in this API, never a number; a test that only compared
// the decoded Go value would pass against `"buying_power": 10000.00`, which
// is precisely the representation the goal forbids.
func requireJSONString(t *testing.T, obj map[string]json.RawMessage, key, want string) {
	t.Helper()
	raw, ok := obj[key]
	require.Truef(t, ok, "member %q is absent", key)
	require.Truef(t, len(raw) > 0 && raw[0] == '"',
		"member %q must be a JSON string, got %s", key, raw)
	var got string
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equalf(t, want, got, "member %q", key)
}

// idemKey builds an Idempotency-Key that is unique to this process run, so
// the suite can be run repeatedly against one database. 8–128 printable
// ASCII, per the spec.
func idemKey(name string) string {
	return fmt.Sprintf("e2e-%s-%s", runToken, name)
}
