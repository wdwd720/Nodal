package alert

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// blockingSink holds every delivery until it is released, so the queue can be
// filled deterministically.
type blockingSink struct {
	release chan struct{}
	got     atomic.Int64
}

func (b *blockingSink) Deliver(context.Context, Event) error {
	b.got.Add(1)
	<-b.release
	return nil
}
func (b *blockingSink) Describe() string { return "blocking" }

// Rule 1: an alert must never fail, or delay, a financial transaction.
//
// Raise is called from inside transactions that move money. If Enqueue blocked
// on a slow webhook host, a ledger write would wait on somebody else's HTTP
// server -- which is a worse failure than the missed alert, and the reason the
// queue drops instead of blocking.
func TestEnqueueNeverBlocks(t *testing.T) {
	t.Parallel()
	sink := &blockingSink{release: make(chan struct{})}
	d := NewDispatcher(Options{Sink: sink, QueueSize: 2, Logger: discardLogger()})

	// One delivery is in flight and blocked; the queue holds two more. Every
	// enqueue after that must be dropped rather than parked.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			d.Enqueue(Event{Name: "probe", Severity: SEV1})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked: a money-moving transaction would now be waiting on a webhook host")
	}

	_, _, dropped, _ := d.Stats()
	assert.Positive(t, dropped, "nothing was dropped, so the queue absorbed 200 alerts and this test proves nothing")

	close(sink.release)
	d.Close()
}

// Rule 2: only allowlisted fields leave the process.
//
// Alerts today carry identifiers and booleans. This is an egress path, and what
// a Raise call site might attach tomorrow is not fixed -- so an unknown key is
// dropped and counted rather than forwarded.
func TestOnlyAllowlistedFieldsLeaveTheProcess(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		body Event
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(Options{
		Sink:   NewWebhookSink(srv.URL, "test", FormatGeneric, srv.Client()),
		Logger: discardLogger(),
	})
	d.Enqueue(EventFrom("probe", SEV1, "detail", "rec-1", map[string]any{
		"order_id":     "ord-1",      // allowed
		"account_id":   "acct-1",     // allowed
		"customer_dob": "1990-01-01", // NOT allowed: the shape of the thing that must never egress
		"api_key":      "sk_live_x",  // NOT allowed
	}, time.Now(), "TEST", "svc"))
	d.Close()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "ord-1", body.Fields["order_id"])
	assert.Equal(t, "acct-1", body.Fields["account_id"])
	assert.NotContains(t, body.Fields, "customer_dob", "a field nobody allowlisted left the process")
	assert.NotContains(t, body.Fields, "api_key", "a field nobody allowlisted left the process")
	assert.Equal(t, 2, body.DroppedFields,
		"the count must be sent: an operator seeing it knows the alert carried something this package would not forward")

	// And the payload identifies its sender, because an alert with no idea
	// which deployment raised it is a page nobody can act on.
	assert.Equal(t, "TEST", body.Environment)
	assert.Equal(t, "svc", body.Service)
}

// A destination that answers badly must not be reported as delivery.
func TestANonSuccessResponseIsAFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewDispatcher(Options{
		Sink:   NewWebhookSink(srv.URL, "test", FormatGeneric, srv.Client()),
		Logger: discardLogger(),
	})
	d.Enqueue(Event{Name: "probe", Severity: SEV1})
	d.Close()

	_, delivered, _, failed := d.Stats()
	assert.Zero(t, delivered, "a 500 was counted as a delivered alert")
	assert.Equal(t, int64(1), failed)
}

// The severity floor is a filter, not a suggestion.
func TestMinSeverityFilters(t *testing.T) {
	t.Parallel()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(Options{
		Sink:        NewWebhookSink(srv.URL, "test", FormatGeneric, srv.Client()),
		MinSeverity: SEV1,
		Logger:      discardLogger(),
	})
	d.Enqueue(Event{Name: "noisy", Severity: SEV2})
	d.Enqueue(Event{Name: "serious", Severity: SEV1})
	d.Close()

	assert.Equal(t, int64(1), count.Load(), "SEV2 was delivered under a SEV1 floor")
}

// Close drains rather than discards: the alerts most worth delivering are the
// ones raised just before a shutdown.
func TestCloseDrainsTheQueue(t *testing.T) {
	t.Parallel()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(Options{
		Sink:      NewWebhookSink(srv.URL, "test", FormatGeneric, srv.Client()),
		QueueSize: 32,
		Logger:    discardLogger(),
	})
	for i := 0; i < 10; i++ {
		d.Enqueue(Event{Name: "probe", Severity: SEV1})
	}
	d.Close()
	assert.Equal(t, int64(10), count.Load(), "Close returned before the queue drained")

	// And a second Close is safe, because shutdown paths run twice more often
	// than anyone intends.
	d.Close()
}

// A nil dispatcher is a working configuration -- LOCAL and TEST have no
// destination -- so every method has to tolerate it rather than panic inside a
// transaction.
func TestNilDispatcherIsUsable(t *testing.T) {
	t.Parallel()
	var d *Dispatcher
	assert.NotPanics(t, func() {
		d.Enqueue(Event{Name: "probe", Severity: SEV1})
		d.Close()
		_, _, _, _ = d.Stats()
	})
}

// Each recognised destination is spoken to in the shape it accepts. The
// generic shape is what the first draft sent to everything, and Slack and
// Discord answer it with 400 -- a destination that rejects every alert is the
// original finding with a URL attached.
func TestEachDestinationGetsTheShapeItAccepts(t *testing.T) {
	t.Parallel()
	ev := EventFrom("ledger_integrity_violation", SEV1, "debits and credits disagree", "rec-1",
		map[string]any{"order_id": "ord-1", "secret": "no"}, time.Now(), "STAGING", "nodal-api")
	want := "[SEV1] STAGING nodal-api ledger_integrity_violation: debits and credits disagree (record rec-1) order_id=ord-1 (+1 fields withheld)"
	require.Equal(t, want, ev.Summary())

	type got struct {
		body    string
		ctype   string
		headers http.Header
	}
	capture := func(t *testing.T, format Format) got {
		t.Helper()
		var g got
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			g = got{body: string(b), ctype: r.Header.Get("Content-Type"), headers: r.Header.Clone()}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		sink := NewWebhookSink(srv.URL, "test", format, srv.Client())
		require.NoError(t, sink.Deliver(context.Background(), ev))
		return g
	}

	slack := capture(t, FormatSlack)
	assert.JSONEq(t, `{"text":"`+want+`"}`, slack.body)
	assert.Equal(t, "application/json", slack.ctype)

	discord := capture(t, FormatDiscord)
	assert.JSONEq(t, `{"content":"`+want+`"}`, discord.body)

	ntfy := capture(t, FormatNtfy)
	assert.Equal(t, want, ntfy.body, "ntfy renders the body as the message, so it is the summary and nothing else")
	assert.Contains(t, ntfy.ctype, "text/plain")
	assert.Equal(t, "[SEV1] ledger_integrity_violation", ntfy.headers.Get("Title"))
	assert.Equal(t, "5", ntfy.headers.Get("Priority"), "a SEV1 is urgent, which is what makes a phone make a noise")
	assert.Equal(t, "rotating_light", ntfy.headers.Get("Tags"))

	generic := capture(t, FormatGeneric)
	var e Event
	require.NoError(t, json.Unmarshal([]byte(generic.body), &e))
	assert.Equal(t, ev.Name, e.Name)
	assert.Equal(t, 1, e.DroppedFields)
	assert.NotContains(t, generic.body, "secret", "the generic shape still applies the allowlist")
}

// The format is derived from the host unless the deployment says otherwise,
// because an operator who pastes a Slack URL and is answered by 400s has not
// been given alerting.
func TestFormatIsDetectedFromTheHost(t *testing.T) {
	t.Parallel()
	cases := map[string]Format{
		"hooks.slack.com":      FormatSlack,
		"discord.com":          FormatDiscord,
		"discordapp.com":       FormatDiscord,
		"canary.discord.com":   FormatDiscord,
		"ntfy.sh":              FormatNtfy,
		"alerts.example.com":   FormatGeneric,
		"HOOKS.SLACK.COM":      FormatSlack,
		"notslack.hooks.slack": FormatGeneric,
	}
	for host, want := range cases {
		assert.Equal(t, want, DetectFormat(host), host)
		assert.Equal(t, want, NewWebhookSink("https://"+host+"/x", host, FormatAuto, nil).Format(), host)
		assert.Equal(t, want, NewWebhookSink("https://"+host+"/x", host, "", nil).Format(), "empty means auto: "+host)
	}
	// An explicit format wins, for a self-hosted ntfy or a Mattermost hook.
	assert.Equal(t, FormatNtfy, NewWebhookSink("https://ntfy.internal/x", "ntfy.internal", FormatNtfy, nil).Format())
	assert.Contains(t, NewWebhookSink("https://ntfy.internal/x", "ntfy.internal", FormatNtfy, nil).Describe(), "(ntfy)")
	for _, f := range Formats() {
		assert.True(t, f.Valid())
	}
	assert.False(t, Format("pagerduty").Valid())
}

// A summary is bounded, so a destination with a cap cannot cut the record id
// off the end -- the fields go, not the identifier.
func TestSummaryIsBounded(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 5000)
	e := Event{Name: "probe", Severity: SEV2, Detail: long, RecordID: "rec-9", Environment: "TEST", Service: "svc"}
	s := e.Summary()
	assert.LessOrEqual(t, len([]rune(s)), summaryLimit)
	assert.True(t, strings.HasSuffix(s, "\u2026"))
}
