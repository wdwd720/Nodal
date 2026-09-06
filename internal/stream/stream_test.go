package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/event/eventtest"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

func customer(acct string) security.Principal {
	return security.Principal{SubjectID: "u-" + acct, ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{acct}, AuthTime: time.Now()}
}

func operator() security.Principal {
	return security.Principal{SubjectID: "op", ActorType: security.ActorOperator, Roles: []security.Role{security.RoleSupportReadOnly}, AuthTime: time.Now()}
}

func envelope(t *testing.T, topic event.Topic, accountID, resourceID string) []byte {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"account_id": accountID, "id": resourceID, "from_status": "RECEIVED", "to_status": "PLANNED", "balance": "12345.67"})
	env := event.Envelope{
		ID: id.New[id.Any]().String(), Type: string(topic), SchemaVersion: 1, Source: "test", AggregateType: "intent", AggregateID: resourceID,
		CorrelationID: "corr", OccurredAt: time.Now().UTC(), RecordedAt: time.Now().UTC(), Payload: payload,
	}
	b, err := env.CanonicalBytes()
	require.NoError(t, err)
	return b
}

func TestHub_FanOutFilteringAndSummaries(t *testing.T) {
	hub := NewHub(8, nil)
	a, _ := hub.Subscribe(customer("acct-a"), 0, 8)
	b, _ := hub.Subscribe(customer("acct-b"), 0, 8)
	op, _ := hub.Subscribe(operator(), 0, 8)
	defer hub.Unsubscribe(a)
	defer hub.Unsubscribe(b)
	defer hub.Unsubscribe(op)

	require.NoError(t, hub.handle(context.Background(), event.Message{Topic: string(event.TopicIntentTransitioned), Value: envelope(t, event.TopicIntentTransitioned, "acct-a", "int-1")}))

	select {
	case e := <-a.Events():
		assert.Equal(t, TypeIntentTransitioned, e.Type)
		assert.Equal(t, "acct-a", e.AccountID)
		assert.Equal(t, uint64(1), e.ID)
		var data map[string]any
		require.NoError(t, json.Unmarshal(e.Data, &data))
		assert.Equal(t, "PLANNED", data["to_status"])
		_, hasBalance := data["balance"]
		assert.False(t, hasBalance, "financial figures are never relayed; clients refetch REST")
	case <-time.After(time.Second):
		t.Fatal("customer a did not receive her event")
	}
	select {
	case e := <-b.Events():
		t.Fatalf("customer b must not see account a's event: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case e := <-op.Events():
		assert.Equal(t, "int-1", e.ResourceID)
	case <-time.After(time.Second):
		t.Fatal("operator with account:read_any did not receive the event")
	}

	// Unknown topics and undecodable messages are dropped without error.
	require.NoError(t, hub.handle(context.Background(), event.Message{Topic: "nope.topic", Value: envelope(t, "nope.topic", "acct-a", "x")}))
	require.NoError(t, hub.handle(context.Background(), event.Message{Topic: string(event.TopicOrderTransitioned), Value: []byte("garbage")}))
}

func TestHub_ResumeAndResync(t *testing.T) {
	hub := NewHub(3, nil)
	for i := 1; i <= 5; i++ {
		hub.Publish(Event{Type: TypeOrderTransitioned, AccountID: "acct-a", ResourceID: "o", OccurredAt: time.Now()})
	}
	// Buffer holds ids 3,4,5. Resuming after 3 replays 4 and 5 without resync.
	s, replay := hub.Subscribe(customer("acct-a"), 3, 8)
	hub.Unsubscribe(s)
	require.Len(t, replay, 2)
	assert.Equal(t, uint64(4), replay[0].ID)
	assert.Equal(t, uint64(5), replay[1].ID)

	// Resuming after 1 (fallen out of the buffer) yields a resync first, then what is retained.
	s, replay = hub.Subscribe(customer("acct-a"), 1, 8)
	hub.Unsubscribe(s)
	require.NotEmpty(t, replay)
	assert.Equal(t, TypeResync, replay[0].Type)
	assert.Equal(t, uint64(3), replay[1].ID)

	// Replay honors tenant filtering.
	s, replay = hub.Subscribe(customer("acct-b"), 3, 8)
	hub.Unsubscribe(s)
	assert.Empty(t, replay)
}

func TestHub_SlowSubscriberDropped(t *testing.T) {
	hub := NewHub(16, nil)
	s, _ := hub.Subscribe(customer("acct-a"), 0, 1)
	hub.Publish(Event{Type: TypeOrderTransitioned, AccountID: "acct-a"})
	hub.Publish(Event{Type: TypeOrderTransitioned, AccountID: "acct-a"}) // buffer full → dropped after 50 ms
	_, open := <-s.Events()
	assert.True(t, open, "first event delivered")
	_, open = <-s.Events()
	assert.False(t, open, "channel closed for the slow subscriber")
}

func TestHub_AttachToMemoryBus(t *testing.T) {
	bus, err := eventtest.NewMemoryBus("TEST")
	require.NoError(t, err)
	defer func() { _ = bus.Close(context.Background()) }()
	hub := NewHub(16, nil)
	require.NoError(t, hub.Attach(context.Background(), bus, "stream-test"))
	s, _ := hub.Subscribe(customer("acct-z"), 0, 8)
	defer hub.Unsubscribe(s)
	require.NoError(t, bus.Publish(context.Background(), string(event.TopicFundingDepositTransitioned), "dep-1", envelope(t, event.TopicFundingDepositTransitioned, "acct-z", "dep-1"), nil))
	select {
	case e := <-s.Events():
		assert.Equal(t, TypeDepositTransitioned, e.Type)
	case <-time.After(2 * time.Second):
		t.Fatal("no event through the bus")
	}
}

func TestSSEHandler_FramingHeartbeatAndAuth(t *testing.T) {
	hub := NewHub(16, nil)
	h := NewHandler(hub, 30*time.Millisecond)

	// Unauthenticated / agent principals are refused.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), customer("acct-a"))))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	req.Header.Set("Last-Event-ID", "0")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// Give the connection time to subscribe, then publish.
	time.Sleep(50 * time.Millisecond)
	hub.Publish(Event{Type: TypeOrderTransitioned, AccountID: "acct-a", ResourceID: "o-1", OccurredAt: time.Now().UTC()})

	reader := bufio.NewReader(resp.Body)
	deadline := time.After(3 * time.Second)
	var lines []string
	for len(lines) < 12 {
		select {
		case <-deadline:
			t.Fatalf("timed out; got %q", lines)
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			break
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
		if strings.HasPrefix(line, "data:") && strings.Contains(line, "o-1") {
			break
		}
	}
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, ": keepalive")
	assert.Contains(t, joined, "event: order.transitioned")
	assert.Regexp(t, `id: \d+`, joined)
	assert.Contains(t, joined, `"resource_id":"o-1"`)
	cancel()
}
