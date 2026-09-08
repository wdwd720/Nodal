//go:build integration

package security

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// TestIdempotency_SameKeyDifferentBodyIsRefused: reusing a key with a
// different canonical request is a client bug that must never be answered with
// the first request's result, and never by running the second request.
func TestIdempotency_SameKeyDifferentBodyIsRefused(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	acct := firstAccount(t, a)
	inst := instrumentID(t, a)

	first := key("reuse-conflict")
	r1 := postAs(t, a.Token, "/v1/intents", first, intentBody(acct, inst))
	require.Equal(t, http.StatusAccepted, r1.Status, "first submission: %s", r1.text())

	second := first
	// The negative control sends a FRESH key, so the second request is a new
	// command and no conflict is produced.
	if secBreak(t, "idem_reuses_fresh_key") {
		second = key("reuse-conflict-fresh")
	}

	different := fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"action":"ACQUIRE_NOTIONAL","mode":"PAPER","notional_usd":"11.00"}`,
		acct, inst,
	)
	r2 := postAs(t, a.Token, "/v1/intents", second, different)
	require.Equal(t, http.StatusConflict, r2.Status, "same key with a different body: %s", r2.text())
	require.Equal(t, string(errs.CodeInvalidIdempotencyReuse), r2.Problem.Code)

	// And the rejected second body must not have taken effect.
	require.Equal(t, 1, intentCount(t, acct, first),
		"the refused reuse still created an intent")
}

// TestIdempotency_KeysAreScopedPerPrincipal: an idempotency cache shared
// between principals is both a data leak (customer-b reads customer-a's
// result) and an activity oracle (customer-b learns which keys customer-a has
// used). The record is keyed on (actor_id, endpoint, key), and this proves it
// end to end.
func TestIdempotency_KeysAreScopedPerPrincipal(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	b := mustLogin(t, "customer-b")
	acctA, acctB := firstAccount(t, a), firstAccount(t, b)
	require.NotEqual(t, acctA, acctB)
	require.NotEqual(t, a.SubjectID, b.SubjectID)
	inst := instrumentID(t, a)

	shared := key("cross-tenant")
	r1 := postAs(t, a.Token, "/v1/intents", shared, intentBody(acctA, inst))
	require.Equal(t, http.StatusAccepted, r1.Status, "customer-a's first submission: %s", r1.text())
	aIntent := idOf(t, r1)
	require.Equal(t, 1, idempotencyRows(t, a.SubjectID, shared), "customer-a's record must exist before the replay")

	replayer, replayerAccount := b, acctB
	// The negative control replays as customer-a, who legitimately gets the
	// stored result back, so the "must not be customer-a's result" assertions
	// fire.
	if secBreak(t, "idem_cross_tenant_replay_by_same_actor") {
		replayer, replayerAccount = a, acctA
	}

	r2 := postAs(t, replayer.Token, "/v1/intents", shared, intentBody(replayerAccount, inst))
	require.Equal(t, http.StatusAccepted, r2.Status,
		"the second principal's identical-key request must run as a NEW command, not replay: %s", r2.text())
	bIntent := idOf(t, r2)

	require.NotEqual(t, aIntent, bIntent,
		"customer-b's request replayed customer-a's stored result: the idempotency cache is not scoped per principal")
	var doc struct {
		AccountID string `json:"account_id"`
	}
	require.NoError(t, json.Unmarshal(r2.Body, &doc))
	require.Equal(t, acctB, doc.AccountID,
		"customer-b received a record belonging to account %s", doc.AccountID)

	// Independent records per actor on one key.
	require.Equal(t, 1, idempotencyRows(t, a.SubjectID, shared))
	require.Equal(t, 1, idempotencyRows(t, b.SubjectID, shared))

	// The security property is that no row on this key belongs to an actor who
	// did not submit it — NOT that exactly two rows exist. Two layers record
	// under one key and they spell the actor differently: the HTTP command
	// layer writes actor_id "<uuid>" with the operation id as endpoint, and
	// internal/intent writes actor_id "USER:<uuid>" with endpoint
	// "intent.submit". The primary key is (actor_id, endpoint, key), so one POST
	// legitimately produces two rows. An earlier version asserted a total of 2
	// and failed at 4 while scoping was in fact correct at both layers.
	//
	// Asserting the property directly also catches what the count would have
	// missed: a third layer appearing later, or any row whose actor is neither
	// principal.
	actors := stringsFrom(t, `SELECT DISTINCT actor_id FROM idempotency_keys WHERE key = $1`, shared)
	require.NotEmpty(t, actors)
	for _, actor := range actors {
		require.Truef(t,
			strings.HasSuffix(actor, a.SubjectID) || strings.HasSuffix(actor, b.SubjectID),
			"idempotency row on a shared key belongs to actor %q, who is neither submitter (%s, %s)",
			actor, a.SubjectID, b.SubjectID)
	}

	// Customer-b must also not be able to learn anything about customer-a's
	// record through the endpoint: the answer to a fresh key and to a key
	// customer-a has already burned must be the same shape.
	fresh := key("cross-tenant-fresh")
	r3 := postAs(t, b.Token, "/v1/intents", fresh, intentBody(acctB, inst))
	require.Equal(t, r2.Status, r3.Status,
		"customer-b can tell a key customer-a used from one nobody used")
}

// TestIdempotency_MalformedKeysAreRefusedWithAStableCode: every degenerate
// key shape is refused before the command runs, with a stable code, and never
// with a 500. The NUL and DEL cases are sent down a raw socket, because Go's
// own client refuses to transmit them and the server is what is under test.
func TestIdempotency_MalformedKeysAreRefusedWithAStableCode(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	acct := firstAccount(t, a)
	inst := instrumentID(t, a)
	body := intentBody(acct, inst)

	// Keys Go's client will transmit.
	transmittable := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"one_char", "x"},
		{"seven_chars", "abcdefg"},
		{"one_twenty_nine", strings.Repeat("k", 129)},
		{"one_thousand", strings.Repeat("k", 1000)},
		{"non_ascii_latin1", "cle-idempotente-éè"},
		{"non_ascii_cjk", "幂等鍵-abcdefgh"},
		{"emoji", "idem-\U0001f600-key"},
		{"tab", "abcdefgh\tijkl"},
		{"only_spaces", "        "},
		{"json_fragment", `{"a":1,"b":[2,3]}`},
		{"sql_fragment", "' OR 1=1--abcdefgh"},
	}
	for _, c := range transmittable {
		t.Run(c.name, func(t *testing.T) {
			r := postKeyed(t, a.Token, "/v1/intents", c.key, body)
			require.Less(t, r.Status, 500, "key %q produced %d: %s", c.name, r.Status, r.text())
			// Every case here, spaces included, is now outside the contract
			// charset (letters, digits, . _ : -). An earlier version exempted
			// only_spaces because printable ASCII was the contract, while still
			// demanding rejection of json_fragment and sql_fragment, which are
			// equally printable ASCII. Both could not be right. The contract was
			// tightened to the narrow charset (D-038), so all of them are refused.
			require.GreaterOrEqual(t, r.Status, 400, "key %q was ACCEPTED: %s", c.name, r.text())
			require.Equal(t, string(errs.CodeValidationFailed), r.Problem.Code,
				"key %q: %d %s", c.name, r.Status, r.text())
			require.NotContains(t, strings.ToLower(r.text()), "sql", "the refusal echoed something query-shaped")
		})
	}

	// Keys that only a raw socket can carry.
	raw := []struct {
		name string
		key  string
	}{
		{"nul_byte", "abcdefgh\x00ijkl"},
		{"del_byte", "abcdefgh\x7fijkl"},
		{"bell", "abcdefgh\x07ijkl"},
	}
	for _, c := range raw {
		t.Run("raw_"+c.name, func(t *testing.T) {
			status, respBody := rawPost(t, "/v1/intents", map[string]string{
				"Cookie":          cookieName + "=" + a.Token,
				"Sec-Fetch-Site":  "same-origin",
				"Content-Type":    "application/json",
				"Idempotency-Key": c.key,
			}, body)
			require.Less(t, status, 500, "raw key %q produced %d: %s", c.name, status, respBody)
			require.GreaterOrEqual(t, status, 400, "raw key %q was ACCEPTED: %s", c.name, respBody)
			require.NotContains(t, respBody, "X-Injected",
				"a header injected through the Idempotency-Key was reflected")
		})
	}

	// CRLF is handled on its own because the correct answer here is NOT a
	// refusal. Sending "abcdefgh\r\nX-Injected: yes" down a raw socket is header
	// smuggling, and Go's HTTP parser resolves it the right way: it reads two
	// headers — an Idempotency-Key of "abcdefgh" and an unknown X-Injected that
	// nothing consumes. What reaches the handler is then an ordinary request
	// carrying a legal 8-character key, so accepting it is correct, and
	// demanding >=400 would demand that the server reject a well-formed request.
	// An earlier version of this test did exactly that and failed against
	// behavior that was right. What actually matters is that the smuggled
	// header changed nothing, was never reflected, and never became part of a
	// stored key.
	t.Run("raw_crlf_is_split_not_smuggled", func(t *testing.T) {
		status, respBody := rawPost(t, "/v1/intents", map[string]string{
			"Cookie":          cookieName + "=" + a.Token,
			"Sec-Fetch-Site":  "same-origin",
			"Content-Type":    "application/json",
			"Idempotency-Key": "abcdefgh\r\nX-Injected: yes",
		}, body)
		require.Less(t, status, 500, "produced %d: %s", status, respBody)
		require.NotContains(t, respBody, "X-Injected",
			"a header injected through the Idempotency-Key was reflected")
		require.Zero(t, countRows(t,
			`SELECT count(*) FROM idempotency_keys WHERE key LIKE '%X-Injected%'`),
			"the smuggled header text was stored as part of an idempotency key")
	})

	// Nothing above may have created an intent.
	require.Zero(t, countRows(t,
		`SELECT count(*) FROM trade_intents WHERE account_id = $1 AND idempotency_key !~ '^[\x20-\x7e]{8,128}$'`, acct),
		"a malformed key reached the intent store")
}

// TestIdempotency_ConcurrentSameKeyHasOneEffect fires N callers at one key.
//
// The count of trade_intents rows alone would prove nothing about this layer:
// trade_intents carries its OWN unique constraint on
// (account_id, idempotency_key), so exactly one row would survive even with
// the HTTP idempotency record removed entirely. The assertions that can only
// be satisfied by internal/idempotency are therefore: every successful answer
// carries the SAME resource id and the SAME body, exactly one caller ran the
// command (202) while the rest replayed (200) or were told it was in progress
// (409 IDEMPOTENCY_IN_PROGRESS), no caller saw a 5xx or a raw conflict, and
// the stored response body equals what the replayers received.
func TestIdempotency_ConcurrentSameKeyHasOneEffect(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	acct := firstAccount(t, a)
	inst := instrumentID(t, a)
	body := intentBody(acct, inst)

	const callers = 12
	sharedKey := key("concurrent")
	keys := make([]string, callers)
	for i := range keys {
		keys[i] = sharedKey
	}
	// The negative control gives every caller its own key, so N effects appear
	// and the "exactly one" counts fire.
	if secBreak(t, "idem_concurrency_uses_distinct_keys") {
		for i := range keys {
			keys[i] = fmt.Sprintf("%s-%02d", sharedKey, i)
		}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]response, callers)
	)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := postAs(t, a.Token, "/v1/intents", keys[i], body)
			mu.Lock()
			results[i] = r
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		ran, replayed, inProgress int
		ids                       = map[string]struct{}{}
		bodies                    = map[string]struct{}{}
	)
	for i, r := range results {
		require.Less(t, r.Status, 500, "caller %d saw %d: %s", i, r.Status, r.text())
		switch r.Status {
		case http.StatusAccepted:
			ran++
			ids[idOf(t, r)] = struct{}{}
			bodies[canonicalIntent(t, r)] = struct{}{}
		case http.StatusOK:
			replayed++
			ids[idOf(t, r)] = struct{}{}
			bodies[canonicalIntent(t, r)] = struct{}{}
		case http.StatusConflict:
			inProgress++
			require.Equal(t, string(errs.CodeIdempotencyInProgress), r.Problem.Code,
				"caller %d got a conflict that was not IDEMPOTENCY_IN_PROGRESS: %s", i, r.text())
			require.NotEmpty(t, r.Header.Get("Retry-After"), "an in-progress answer must say when to retry")
		default:
			require.FailNowf(t, "unexpected status", "caller %d: %d %s", i, r.Status, r.text())
		}
		require.NotEmpty(t, r.Problem.RequestID+idOrEmpty(r), "caller %d produced a body with neither an id nor a request id", i)
	}

	require.Equal(t, 1, ran, "exactly one caller must have executed the command; %d did", ran)
	require.Len(t, ids, 1, "the callers saw %d distinct resources for one key: %v", len(ids), ids)
	require.Len(t, bodies, 1, "the callers saw %d distinct bodies for one key", len(bodies))
	require.Positive(t, replayed+inProgress, "no caller replayed or was told the command was in progress, so nothing was serialized")

	// One effect, and one record that produced it.
	require.Equal(t, 1, intentCount(t, acct, sharedKey), "the shared key produced more than one intent")
	require.Equal(t, 1, idempotencyRows(t, a.SubjectID, sharedKey))

	stored := storedResponseBody(t, a.SubjectID, sharedKey)
	require.NotEmpty(t, stored, "the completed record stored no response body, so a replay could not have reproduced it")
	var storedDoc, seenDoc map[string]any
	require.NoError(t, json.Unmarshal(stored, &storedDoc))
	for b := range bodies {
		require.NoError(t, json.Unmarshal([]byte(b), &seenDoc))
	}
	assert.Equal(t, storedDoc["id"], seenDoc["id"], "the replayed body did not come from the stored record")
}

// --- helpers ---------------------------------------------------------------

func idOf(t *testing.T, r response) string {
	t.Helper()
	var doc struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &doc), "body was not a JSON document: %s", r.text())
	require.NotEmpty(t, doc.ID, "body carried no id: %s", r.text())
	return doc.ID
}

func idOrEmpty(r response) string {
	var doc struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body, &doc)
	return doc.ID
}

// canonicalIntent re-encodes the answer with the fields that legitimately
// differ between a fresh run and a replay removed, so two answers can be
// compared for "same result".
func canonicalIntent(t *testing.T, r response) string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(r.Body, &doc))
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return string(b)
}

func storedResponseBody(t *testing.T, actorID, idemKey string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var body []byte
	err := testPool.QueryRow(ctx,
		`SELECT response_body FROM idempotency_keys WHERE actor_id = $1 AND key = $2 AND status = 'COMPLETED'`,
		actorID, idemKey).Scan(&body)
	require.NoError(t, err)
	return body
}

// postKeyed is postAs but able to send an EMPTY Idempotency-Key header, which
// postAs treats as "omit the header".
func postKeyed(t *testing.T, token, path, idemKey, body string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, apiBaseURL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Cookie", cookieName+"="+token)
	req.Header["Idempotency-Key"] = []string{idemKey}
	resp, err := noRedirect.Do(req)
	if err != nil {
		// Go's transport refuses to transmit some header bytes. That is the
		// client protecting the server, not the server protecting itself, so
		// the case is re-run over a raw socket by the caller.
		t.Fatalf("the client refused to send key %q: %v", idemKey, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return readResponse(t, resp)
}

func readResponse(t *testing.T, resp *http.Response) response {
	t.Helper()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	out := response{Status: resp.StatusCode, Body: buf, Header: resp.Header.Clone()}
	if strings.Contains(resp.Header.Get("Content-Type"), "problem+json") {
		_ = json.Unmarshal(buf, &out.Problem)
	}
	assertNotThrottled(t, out, resp.Request.Method+" "+resp.Request.URL.String())
	return out
}

// rawPost writes an HTTP/1.1 request byte for byte, so header values Go's
// client would refuse to transmit still reach the server. It returns the
// status line's code and the whole response text.
func rawPost(t *testing.T, path string, headers map[string]string, body string) (int, string) {
	t.Helper()
	u, err := url.Parse(apiBaseURL)
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp", u.Host, 10*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(20*time.Second)))

	var sb strings.Builder
	fmt.Fprintf(&sb, "POST %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nContent-Length: %d\r\n", path, u.Host, len(body))
	for k, v := range headers {
		fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
	}
	sb.WriteString("\r\n")
	sb.WriteString(body)
	_, err = conn.Write([]byte(sb.String()))
	require.NoError(t, err)

	r := bufio.NewReader(conn)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := r.Read(buf)
		out.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	text := out.String()
	require.NotEmpty(t, text, "the server closed the connection without answering")
	var proto string
	var code int
	_, err = fmt.Sscanf(text, "%s %d", &proto, &code)
	require.NoError(t, err, "unparseable status line: %q", firstLine(text))
	require.NotEqual(t, http.StatusTooManyRequests, code, "raw probe was rate limited")
	return code, text
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
