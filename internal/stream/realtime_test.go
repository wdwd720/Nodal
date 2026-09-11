package stream

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/security"
)

func person(userID string) security.Principal {
	return security.Principal{
		SubjectID: userID, ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{"acct-" + userID},
		AuthTime: time.Now(),
	}
}

func operatorWithReadAny() security.Principal {
	return security.Principal{
		SubjectID: "op-1", ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleAdmin}, AuthTime: time.Now(),
	}
}

// TestVisibility_ANotificationGoesToItsRecipientAndNobodyElse.
//
// account:read_any is the operator's key to every account's events, and it is
// deliberately not a key to somebody's inbox. An operator investigating an
// account reads the audit trail; a copy of what the customer was TOLD is a
// different document, and the stream is not where it is handed over.
func TestVisibility_ANotificationGoesToItsRecipientAndNobodyElse(t *testing.T) {
	t.Parallel()
	e := Event{Type: TypeNotification, UserID: "u-1", ResourceID: "n-1"}
	assert.True(t, e.visibleTo(person("u-1")))
	assert.False(t, e.visibleTo(person("u-2")))
	assert.False(t, e.visibleTo(operatorWithReadAny()),
		"account:read_any must not open somebody else's notification centre")

	// An account-scoped event keeps the old rule.
	acct := Event{Type: TypeOrderTransitioned, AccountID: "acct-u-1"}
	assert.True(t, acct.visibleTo(person("u-1")))
	assert.False(t, acct.visibleTo(person("u-2")))
	assert.True(t, acct.visibleTo(operatorWithReadAny()))

	// A broadcast is public data: a market price every client may already read
	// over REST.
	pub := Event{Type: TypeDataChanged, Broadcast: true, ResourceID: "m-1"}
	assert.True(t, pub.visibleTo(person("u-1")))
	assert.True(t, pub.visibleTo(person("u-2")))
	assert.True(t, pub.visibleTo(operatorWithReadAny()))

	// And an unaddressed, unbroadcast event still reaches nobody but an
	// operator, which is the pre-existing fail-closed default.
	assert.False(t, Event{Type: TypeDataChanged}.visibleTo(person("u-1")))
}

// TestEventIDs_SurviveARestart is the bug the epoch fixes. Without it a client
// resuming after a redeploy is silently told it is up to date.
func TestEventIDs_SurviveARestart(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	clk := func() time.Time { return at }

	first := NewHub(16, nil)
	first.UseClock(clk)
	a := first.Publish(Event{Type: TypeNotification, UserID: "u-1"})
	b := first.Publish(Event{Type: TypeNotification, UserID: "u-1"})
	assert.Greater(t, b.ID, a.ID)

	got, ok := EventTime(a.ID)
	require.True(t, ok, "a seeded id must carry its own instant")
	assert.Equal(t, at.UnixMilli(), got.UnixMilli())
	assert.Equal(t, EventIDAt(at), uint64(at.UnixMilli())<<epochShift)

	// A plain counter carries no instant, so nothing tries to read one.
	_, ok = EventTime(812)
	assert.False(t, ok)

	// The process restarts: a new hub, a later clock, an empty buffer.
	second := NewHub(16, nil)
	second.UseClock(func() time.Time { return at.Add(time.Minute) })
	next := second.Publish(Event{Type: TypeNotification, UserID: "u-1"})
	assert.Greater(t, next.ID, b.ID, "ids must be monotonic across a restart")

	// And a client resuming from the old position is told to resync rather
	// than being left believing it is caught up.
	third := NewHub(16, nil)
	third.UseClock(clk)
	_, replay := third.Subscribe(person("u-1"), b.ID, 8)
	require.NotEmpty(t, replay)
	assert.Equal(t, TypeResync, replay[0].Type)
}

// TestSubscribe_ResyncsWhateverTheBufferCannotProve.
func TestSubscribe_ResyncsWhateverTheBufferCannotProve(t *testing.T) {
	t.Parallel()
	hub := NewHub(2, nil)
	p := person("u-1")
	for i := 0; i < 4; i++ {
		hub.Publish(Event{Type: TypeNotification, UserID: "u-1"})
	}
	// Position 1 has fallen out of a two-event buffer.
	_, replay := hub.Subscribe(p, 1, 8)
	require.NotEmpty(t, replay)
	assert.Equal(t, TypeResync, replay[0].Type)

	// Position 3 is inside it: replay 4 and no resync.
	_, replay = hub.Subscribe(p, 3, 8)
	require.Len(t, replay, 1)
	assert.Equal(t, TypeNotification, replay[0].Type)
	assert.Equal(t, uint64(4), replay[0].ID)

	// A position ahead of the hub is a client from the future -- or from a
	// process that had published more than this one has.
	_, replay = hub.Subscribe(p, 99, 8)
	require.NotEmpty(t, replay)
	assert.Equal(t, TypeResync, replay[0].Type)
}

// TestSSE_CapsConcurrentStreamsPerPerson: one free instance, one process, and a
// stream that never ends. The refusal has to be legible, so it is 429 with
// Retry-After rather than a closed socket.
func TestSSE_CapsConcurrentStreamsPerPerson(t *testing.T) {
	t.Parallel()
	hub := NewHub(16, nil)
	h := NewHandler(hub, time.Hour, nil)
	h.SetMaxStreamsPerUser(2)

	p := person("u-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Two streams, opened and held: a stream is one request that never ends, and
	// holding the cap open is what this test is about. Unrolled rather than
	// looped so each response has its own deferred close.
	firstReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	first, err := http.DefaultClient.Do(firstReq)
	require.NoError(t, err)
	defer func() { _ = first.Body.Close() }()
	require.Equal(t, http.StatusOK, first.StatusCode)

	secondReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	second, err := http.DefaultClient.Do(secondReq)
	require.NoError(t, err)
	defer func() { _ = second.Body.Close() }()
	require.Equal(t, http.StatusOK, second.StatusCode)

	require.Eventually(t, func() bool { return h.OpenStreams(p.SubjectID) == 2 }, time.Second, 5*time.Millisecond)

	thirdReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(thirdReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"), "a refusal a client can act on states when to try again")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "RATE_LIMITED")

	// Closing one frees a slot, so the cap is a ceiling and not a lifetime
	// budget.
	require.NoError(t, first.Body.Close())
	require.Eventually(t, func() bool { return h.OpenStreams(p.SubjectID) < 2 }, 2*time.Second, 5*time.Millisecond)
}

// TestSSE_ResumesFromTheDurableRecordBeforeTheBuffer.
//
// The point of the hook: after a redeploy the buffer holds nothing, and the
// notifications the person missed are in a table. The id the client sent names
// the instant, and what comes back is what was written since it.
func TestSSE_ResumesFromTheDurableRecordBeforeTheBuffer(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	hub := NewHub(16, nil)
	hub.UseClock(func() time.Time { return at.Add(time.Hour) })
	h := NewHandler(hub, time.Hour, nil)

	var askedFor atomic.Int64
	h.SetResume(func(_ context.Context, p security.Principal, since time.Time) ([]Event, bool, error) {
		askedFor.Store(since.UnixMilli())
		return []Event{{
			ID: EventIDAt(since.Add(time.Second)), Type: TypeNotification,
			UserID: p.SubjectID, ResourceID: "missed-1", OccurredAt: since.Add(time.Second),
		}}, false, nil
	})

	p := person("u-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	req.Header.Set("Last-Event-ID", itoa(EventIDAt(at)))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	joined := readLines(t, resp.Body, "missed-1")
	assert.Contains(t, joined, "retry: 3000", "the browser is told how long to wait; this instance sleeps")
	assert.Contains(t, joined, "event: notification.created")
	assert.Contains(t, joined, `"resource_id":"missed-1"`)
	assert.Equal(t, at.UnixMilli(), askedFor.Load(), "the hook is asked for the instant the id encodes")
	cancel()
}

// TestSSE_DeliversANotificationAndADataChangeAfterAnEmit is the end the whole
// package exists for: a producer publishes, and a connected client sees it.
func TestSSE_DeliversANotificationAndADataChangeAfterAnEmit(t *testing.T) {
	t.Parallel()
	hub := NewHub(16, nil)
	h := NewHandler(hub, 30*time.Millisecond, nil)
	p := person("u-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Eventually(t, func() bool { return h.OpenStreams(p.SubjectID) == 1 }, time.Second, 5*time.Millisecond)
	hub.Publish(Event{Type: TypeNotification, UserID: "u-1", ResourceID: "n-9", OccurredAt: time.Now().UTC()})
	hub.Publish(Event{Type: TypeDataChanged, UserID: "u-1", ResourceID: "balance", OccurredAt: time.Now().UTC()})
	// Somebody else's notification, which must not appear.
	hub.Publish(Event{Type: TypeNotification, UserID: "u-2", ResourceID: "n-other", OccurredAt: time.Now().UTC()})

	joined := readLines(t, resp.Body, "balance")
	assert.Contains(t, joined, "event: notification.created")
	assert.Contains(t, joined, `"resource_id":"n-9"`)
	assert.Contains(t, joined, "event: data.changed")
	assert.NotContains(t, joined, "n-other", "another person's notification reached this stream")
	cancel()
}

func readLines(t *testing.T, body io.Reader, until string) string {
	t.Helper()
	reader := bufio.NewReader(body)
	deadline := time.After(3 * time.Second)
	var lines []string
	for len(lines) < 40 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", until, strings.Join(lines, "\n"))
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			break
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
		if strings.Contains(line, until) {
			break
		}
	}
	return strings.Join(lines, "\n")
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
