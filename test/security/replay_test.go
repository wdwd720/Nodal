//go:build integration

package security

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// TestReplay_AuthorizationCodeAndStateCannotBeReplayed: the login attempt is
// claimed exactly once, under a row lock, so re-sending the same code and
// state never yields a second session. The assertion is made against the
// database, not against the response: a 401 with a session quietly created
// behind it would be the worst possible outcome.
func TestReplay_AuthorizationCodeAndStateCannotBeReplayed(t *testing.T) {
	requireAPI(t)

	state, loginState := beginLogin(t)
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM login_attempts WHERE state = $1`, state),
		"the login attempt must be persisted server-side before the callback, or there is nothing to replay against")
	require.Zero(t, countRows(t, `SELECT count(*) FROM login_attempts WHERE state = $1 AND consumed_at IS NOT NULL`, state),
		"the attempt must start unconsumed")

	before := dbNow(t)
	first := completeLoginFrom(t, "customer-a", state, loginState)
	require.Equal(t, http.StatusFound, first.Status, "first callback: %s", first.text())
	token := sessionCookie(t, first)
	require.NotEmpty(t, token)

	subject := subjectOfToken(t, token)
	sessionsAfterFirst := sessionsSince(t, subject, before)
	require.Equal(t, 1, sessionsAfterFirst, "the first callback should have created exactly one session")

	require.Equal(t, 1, countRows(t,
		`SELECT count(*) FROM login_attempts WHERE state = $1 AND consumed_at IS NOT NULL AND outcome = 'SUCCESS'`, state),
		"the attempt must be marked consumed and successful")

	replayState := state
	// The negative control replays a FRESH, unconsumed state, so a second
	// session is created and both assertions below fire.
	if secBreak(t, "replay_uses_a_fresh_state") {
		replayState, loginState = beginLogin(t)
	}

	// GET replay.
	second := completeLoginFrom(t, "customer-a", replayState, loginState)
	require.Equal(t, http.StatusUnauthorized, second.Status, "the replayed callback was not refused: %s", second.text())
	require.Equal(t, string(errs.CodeUnauthenticated), second.Problem.Code)
	require.Empty(t, sessionCookie(t, second), "the replayed callback issued a session cookie")

	// POST replay of the same parameters. The contract only declares GET, so
	// this must not be a second way in either.
	post := postAs(t, "", "/v1/auth/callback?code=customer-a&state="+replayState, "", "")
	require.NotEqual(t, http.StatusFound, post.Status, "POST to the callback issued a redirect: %s", post.text())
	require.Empty(t, sessionCookie(t, post), "POST to the callback issued a session cookie")
	require.Less(t, post.Status, 500, "POST to the callback produced %d", post.Status)

	require.Equal(t, sessionsAfterFirst, sessionsSince(t, subject, before),
		"the replay created another session for %s", subject)

	// A third identity replaying the same state must not get in either.
	third := completeLoginFrom(t, "admin", replayState, loginState)
	require.Equal(t, http.StatusUnauthorized, third.Status, "a different identity replayed the state: %s", third.text())
	require.Empty(t, sessionCookie(t, third))
}

// TestReplay_UnknownAndExpiredStateAreRefused rounds out the state contract.
func TestReplay_UnknownAndExpiredStateAreRefused(t *testing.T) {
	requireAPI(t)
	for _, c := range []struct{ name, state string }{
		{"unknown", "advsec-state-that-was-never-issued"},
		{"empty", ""},
		{"sql", "%27%20OR%201%3D1--"},
		{"very_long", strings.Repeat("a", 4096)},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := completeLogin(t, "customer-a", c.state)
			require.Less(t, r.Status, 500, "state %q produced %d: %s", c.name, r.Status, r.text())
			require.GreaterOrEqual(t, r.Status, 400, "state %q was accepted: %s", c.name, r.text())
			require.Empty(t, sessionCookie(t, r), "state %q issued a session cookie", c.name)
		})
	}
	// An unknown code against a valid state consumes the attempt and issues
	// nothing: the attempt must not remain redeemable after a failed exchange.
	state, loginState := beginLogin(t)
	bad := completeLoginFrom(t, "not-an-identity", state, loginState)
	require.GreaterOrEqual(t, bad.Status, 400, "an unknown identity was accepted: %s", bad.text())
	require.Empty(t, sessionCookie(t, bad))
	require.Equal(t, 1, countRows(t,
		`SELECT count(*) FROM login_attempts WHERE state = $1 AND consumed_at IS NOT NULL`, state),
		"a failed exchange left the attempt redeemable")
	retry := completeLoginFrom(t, "customer-a", state, loginState)
	require.Equal(t, http.StatusUnauthorized, retry.Status,
		"an attempt burned by a failed exchange was redeemable by a valid identity: %s", retry.text())
	require.Empty(t, sessionCookie(t, retry))
}

// TestReplay_LogoutRevokesTheSessionRow: the response changing is not
// evidence. The session row must carry revoked_at, and the old cookie must be
// refused on every route, not only on the one that was probed.
func TestReplay_LogoutRevokesTheSessionRow(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	sessionID := currentSessionID(t, a.Token)
	require.Zero(t, countRows(t, `SELECT count(*) FROM sessions WHERE id = $1 AND revoked_at IS NOT NULL`, sessionID),
		"the session must start un-revoked, or the check below proves nothing")

	out := postAs(t, a.Token, "/v1/auth/logout", "", "")
	require.Contains(t, []int{http.StatusNoContent, http.StatusOK}, out.Status, "logout: %s", out.text())

	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM sessions WHERE id = $1 AND revoked_at IS NOT NULL`, sessionID),
		"logout answered success but the session row is not revoked")

	probeToken := a.Token
	// The negative control presents a freshly issued cookie, so the requests
	// succeed and the refusal assertions fire.
	if secBreak(t, "logout_probe_uses_a_new_cookie") {
		probeToken = mustLogin(t, "customer-a").Token
	}

	for _, path := range []string{"/v1/me", "/v1/accounts", "/v1/sessions", "/v1/instruments"} {
		r := getAs(t, probeToken, path)
		require.Equal(t, http.StatusUnauthorized, r.Status,
			"the revoked cookie still worked on %s: %s", path, r.text())
		require.Equal(t, string(errs.CodeUnauthenticated), r.Problem.Code)
	}
	// And it must not be usable to command either.
	cmd := postAs(t, probeToken, "/v1/intents", key("after-logout"), `{"account_id":"`+firstAccount(t, a)+`"}`)
	require.Equal(t, http.StatusUnauthorized, cmd.Status, "the revoked cookie could still command: %s", cmd.text())

	// Logging out twice is not an error and does not resurrect anything.
	again := postAs(t, probeToken, "/v1/auth/logout", "", "")
	require.Less(t, again.Status, 500, "a second logout produced %d", again.Status)
	require.Empty(t, sessionCookie(t, again), "a second logout issued a session cookie")
}

// TestReplay_ForgedSessionCookiesAreRefused: nothing but the exact 43-character
// token a login issued may authenticate, and knowing a session's public id
// must not be enough to forge one.
func TestReplay_ForgedSessionCookiesAreRefused(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	b := mustLogin(t, "customer-b")
	sessionID := currentSessionID(t, a.Token)
	tokenHash := tokenHashOf(t, sessionID)

	forged := map[string]string{
		"empty":              "",
		"appended":           a.Token + "x",
		"truncated":          a.Token[:len(a.Token)-1],
		"truncated_half":     a.Token[:len(a.Token)/2],
		"prefixed":           "x" + a.Token,
		"bit_flipped":        flipOneBit(t, a.Token),
		"padded_base64":      a.Token + "=",
		"session_id":         sessionID,
		"session_id_nodash":  strings.ReplaceAll(sessionID, "-", ""),
		"stored_hash":        tokenHash,
		"whitespace_wrapped": " " + a.Token + " ",
		"case_flipped":       strings.ToUpper(a.Token),
		"sql":                "' OR 1=1--",
		"null_bytes":         strings.Repeat("A", 43),
	}
	for name, v := range forged {
		t.Run(name, func(t *testing.T) {
			r := getAs(t, v, "/v1/me")
			require.Equal(t, http.StatusUnauthorized, r.Status,
				"forged cookie %s (%q) authenticated: %s", name, v, r.text())
			require.Equal(t, string(errs.CodeUnauthenticated), r.Problem.Code)
			require.NotContains(t, r.text(), a.SubjectID, "the refusal named the real subject")
		})
	}
	// Sanity: the untouched token still works, so the loop above was refusing
	// forgeries rather than everything.
	ok := getAs(t, a.Token, "/v1/me")
	require.Equal(t, http.StatusOK, ok.Status, "the genuine cookie stopped working: %s", ok.text())

	// A valid token belonging to another user authenticates as THAT user and
	// never grants the first user's identity or resources.
	asB := getAs(t, b.Token, "/v1/me")
	require.Equal(t, http.StatusOK, asB.Status)
	var doc meDoc
	require.NoError(t, json.Unmarshal(asB.Body, &doc))
	require.Equal(t, b.SubjectID, doc.SubjectID, "customer-b's token resolved to a different subject")
	require.NotContains(t, doc.AccountIDs, firstAccount(t, a), "customer-b's session carried customer-a's account")
}

// TestReplay_RevokingAnotherUsersSessionIsRefused: /v1/sessions/{id} must not
// double as a way to knock other people offline, nor as a probe for their
// session ids.
func TestReplay_RevokingAnotherUsersSessionIsRefused(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	victim := mustLogin(t, "customer-b")
	victimSession := currentSessionID(t, victim.Token)
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM sessions WHERE id = $1 AND revoked_at IS NULL`, victimSession),
		"the victim session must exist and be live, or this proves nothing")

	req := deleteAs(t, a.Token, "/v1/sessions/"+victimSession)
	require.Equal(t, http.StatusNotFound, req.Status,
		"deleting another user's session answered %d: %s", req.Status, req.text())

	// Existence must not be observable: an id that names nothing answers the
	// same way as one that names someone else's session.
	absent := deleteAs(t, a.Token, "/v1/sessions/"+absentRecord)
	require.Equal(t, req.Status, absent.Status, "a foreign session id is distinguishable from an absent one")
	require.Equal(t, req.Problem.Code, absent.Problem.Code)
	require.Equal(t, req.Problem.Detail, absent.Problem.Detail)

	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM sessions WHERE id = $1 AND revoked_at IS NULL`, victimSession),
		"the victim's session was revoked by another user")
	ok := getAs(t, victim.Token, "/v1/me")
	require.Equal(t, http.StatusOK, ok.Status, "the victim was logged out by another user")
}

// --- helpers ---------------------------------------------------------------

func deleteAs(t *testing.T, token, path string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, apiBaseURL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Cookie", cookieName+"="+token)
	return do(t, req)
}

func subjectOfToken(t *testing.T, token string) string {
	t.Helper()
	r := getAs(t, token, "/v1/me")
	require.Equal(t, http.StatusOK, r.Status, "GET /v1/me: %s", r.text())
	var doc meDoc
	require.NoError(t, json.Unmarshal(r.Body, &doc))
	require.NotEmpty(t, doc.SubjectID)
	return doc.SubjectID
}

func currentSessionID(t *testing.T, token string) string {
	t.Helper()
	r := getAs(t, token, "/v1/sessions")
	require.Equal(t, http.StatusOK, r.Status, "GET /v1/sessions: %s", r.text())
	var list []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &list))
	for _, s := range list {
		if s.Current {
			return s.ID
		}
	}
	require.FailNow(t, "the session listing did not mark a current session")
	return ""
}

// sessionsSince counts the sessions created for one subject after ts, so the
// count is scoped to this test rather than to whatever earlier runs left in a
// database the suite deliberately never cleans.
func sessionsSince(t *testing.T, subject string, ts time.Time) int {
	t.Helper()
	return countRows(t, `SELECT count(*) FROM sessions WHERE user_id = $1 AND created_at > $2`, subject, ts)
}

// tokenHashOf returns the stored session hash encoded the way a token is
// encoded, which is what makes it usable as a forged cookie value.
//
// The case this feeds is the one that matters most in this table: an attacker
// holding a database dump has sessions.token_hash, and presenting it must not
// authenticate. The server stores a hash precisely so that reading the table is
// not equivalent to holding the credential.
//
// It must be base64url-encoded to be tested at all. An earlier version returned
// string(raw) — 32 bytes of binary — which is not a legal cookie value, so Go's
// client refused to transmit it and the subtest failed inside the HTTP client
// rather than against the server. That looked like a finding and proved nothing.
func tokenHashOf(t *testing.T, sessionID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw []byte
	require.NoError(t, testPool.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE id = $1`, sessionID).Scan(&raw))
	require.NotEmpty(t, raw, "sessions.token_hash is empty; the forgery case would be vacuous")
	return base64.RawURLEncoding.EncodeToString(raw)
}

// flipOneBit decodes the base64url token, flips a single bit and re-encodes,
// so the result is a well-formed token of the right length that is simply not
// the one that was issued.
func flipOneBit(t *testing.T, token string) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err, "the session token is not base64url; this test assumes auth.NewToken's format")
	require.NotEmpty(t, b)
	b[len(b)/2] ^= 0x01
	out := base64.RawURLEncoding.EncodeToString(b)
	require.NotEqual(t, token, out)
	require.Len(t, out, len(token))
	return out
}

// TestReplay_APlantedCallbackCannotSignAnybodyIn is the other half of what
// `state` is for, and the half that was missing (F-87).
//
// The suite above proves a callback cannot be REPLAYED: the attempt is claimed
// once under a row lock. It said nothing about a callback being PLANTED, and
// those are different attacks. An attacker begins a flow in their own browser,
// authenticates as themselves, keeps code and state without following the
// redirect, and induces a victim's browser to load the callback. The server
// found the attempt by state, exchanged the code, and set a session cookie in
// the victim's browser for the attacker's subject -- so everything the victim
// then did landed in the attacker's account.
func TestReplay_APlantedCallbackCannotSignAnybodyIn(t *testing.T) {
	requireAPI(t)

	state, loginState := beginLogin(t)
	before := dbNow(t)

	// The victim's browser: the parameters are valid, the cookie is not there.
	planted := completeLogin(t, "customer-a", state)
	require.Equal(t, http.StatusUnauthorized, planted.Status,
		"a callback that did not begin in this browser signed somebody in: %s", planted.text())
	require.Empty(t, sessionCookie(t, planted), "no session may be established")

	// And it consumed nothing, so the refusal costs the real browser nothing.
	require.Zero(t, countRows(t,
		`SELECT count(*) FROM login_attempts WHERE state = $1 AND consumed_at IS NOT NULL`, state),
		"a refused callback must not consume the attempt")

	// The control: the browser that began the flow still completes it. Without
	// this the refusal above could be a login that no longer works at all.
	ok := completeLoginFrom(t, "customer-a", state, loginState)
	require.Equal(t, http.StatusFound, ok.Status, "callback from the right browser: %s", ok.text())
	token := sessionCookie(t, ok)
	require.NotEmpty(t, token)
	require.Equal(t, 1, sessionsSince(t, subjectOfToken(t, token), before),
		"exactly one session, and it belongs to the browser that asked for it")
}
