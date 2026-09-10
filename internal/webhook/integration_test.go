//go:build integration

package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/funding/fundingtest"
	"github.com/nodal/controlplane/internal/webhook"
	"github.com/nodal/controlplane/internal/webhook/webhooktest"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	// testMigrate connects as the owning role, which holds every privilege.
	// A control observed only through cp_app cannot distinguish a trigger from
	// a missing grant; this pool is how the two are told apart.
	testMigrate *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "webhook integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "webhook-itest", MaxConns: 30})
	if err != nil {
		fmt.Fprintln(os.Stderr, "webhook integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	testMigrate, err = db.Open(ctx, db.Config{URL: testMigrateURL, AppName: "webhook-itest-migrate", MaxConns: 4})
	if err != nil {
		fmt.Fprintln(os.Stderr, "webhook integration: open migrate pool:", err)
		return 1
	}
	defer testMigrate.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test (go run ./scripts/testdb -name funding)")
	}
}

// effectDispatcher records one audit_events row per applied event in the
// pipeline transaction (the "economic effect" whose count the property
// bounds), ignores type "noise", and fails while failing is true.
type effectDispatcher struct {
	audit   audit.Writer
	clk     clock.Clock
	failing atomic.Bool
	calls   atomic.Int32
}

func (d *effectDispatcher) Dispatch(ctx context.Context, tx pgx.Tx, ev funding.WebhookEvent) (webhook.Disposition, error) {
	d.calls.Add(1)
	if d.failing.Load() {
		return "", errs.New(errs.CodeInternal, "simulated domain failure")
	}
	if !ev.SessionKnown {
		return webhook.Ignored, nil
	}
	_, err := d.audit.Append(ctx, tx, audit.Event{
		Stream: audit.SystemStream, ActorType: "SYSTEM", ActorID: "webhook-itest", Action: "effect", ResourceType: "provider_event",
		ResourceID: ev.Identity.EventID, OccurredAt: d.clk.Now(),
	})
	if err != nil {
		return "", err
	}
	return webhook.Applied, nil
}

type fixture struct {
	t        *testing.T
	clk      *clock.Fake
	provider *fundingtest.Provider
	disp     *effectDispatcher
	archive  *webhooktest.MemoryArchive
	handler  *webhook.Handler[funding.WebhookEvent]
	server   *httptest.Server
	session  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	prov := fundingtest.NewProvider(clk, "whsec_"+uuid.NewString())
	prov.ProviderName = "fake-" + uuid.NewString()[:8] // isolate provider_events rows per test
	s, err := prov.CreateSession(context.Background(), funding.CreateSessionRequest{IdempotencyKey: "k", DestinationNetwork: "solana", DestinationCurrency: "usdc", WalletAddress: "w"})
	require.NoError(t, err)
	f := &fixture{t: t, clk: clk, provider: prov, session: s.ID, archive: webhooktest.NewMemoryArchive()}
	f.disp = &effectDispatcher{audit: audit.NewWriterWithBuildVersion("webhook-itest"), clk: clk}
	h, err := webhook.NewHandler[funding.WebhookEvent](webhook.Config[funding.WebhookEvent]{
		Verifier: prov, Dispatcher: f.disp, Archive: f.archive, DB: testDB, Inbox: event.NewInbox(clk), Clock: clk, MaxBodyBytes: 4096,
	})
	require.NoError(t, err)
	f.handler = h
	f.server = httptest.NewServer(h)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) deliver(raw []byte, headers http.Header) (int, string) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.server.URL+"/webhooks/fake", bytes.NewReader(raw))
	require.NoError(f.t, err)
	for k, v := range headers {
		req.Header[k] = v
	}
	resp, err := f.server.Client().Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Outcome string `json:"outcome"`
	}
	require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&body))
	return resp.StatusCode, body.Outcome
}

func (f *fixture) effects(eventID string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE stream = 'system' AND action = 'effect' AND resource_id = $1`, eventID).Scan(&n))
	return n
}

func (f *fixture) providerEvent(eventID string) (status, errText *string, found bool) {
	f.t.Helper()
	var s string
	var e *string
	err := testDB.QueryRow(context.Background(), `SELECT processing_status, error FROM provider_events WHERE provider = $1 AND provider_event_id = $2`, f.provider.Name(), eventID).Scan(&s, &e)
	if err != nil {
		return nil, nil, false
	}
	return &s, e, true
}

func (f *fixture) securityEvents(kind string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM security_events WHERE kind = $1 AND detail->>'provider' = $2`, kind, f.provider.Name()).Scan(&n))
	return n
}

// TestProp_DuplicateWebhookOneEffect: any number of deliveries of one
// provider event produce exactly one economic effect.
func TestProp_DuplicateWebhookOneEffect(t *testing.T) {
	f := newFixture(t)
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(rt, "deliveries")
		eventID := "evt_" + uuid.NewString()
		raw, headers := f.provider.Webhook(eventID, "session.updated", f.session)
		for i := 0; i < n; i++ {
			status, outcome := f.deliver(raw, headers)
			require.Equal(rt, http.StatusOK, status)
			if i == 0 {
				require.Equal(rt, "processed", outcome)
			} else {
				require.Equal(rt, "duplicate", outcome)
			}
		}
		require.Equal(rt, 1, f.effects(eventID))
		st, _, ok := f.providerEvent(eventID)
		require.True(rt, ok)
		require.Equal(rt, "PROCESSED", *st)
	})
}

// TestIntegration_ConcurrentDuplicatesOneEffect fires the same delivery
// concurrently: one effect, every response is 200 or 409.
func TestIntegration_ConcurrentDuplicatesOneEffect(t *testing.T) {
	f := newFixture(t)
	eventID := "evt_" + uuid.NewString()
	raw, headers := f.provider.Webhook(eventID, "session.updated", f.session)
	const n = 12
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = f.deliver(raw, headers)
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, c)
	}
	require.Equal(t, 1, f.effects(eventID))
	// A follow-up delivery after the race is a plain duplicate.
	status, outcome := f.deliver(raw, headers)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "duplicate", outcome)
	require.Equal(t, 1, f.effects(eventID))
}

func TestIntegration_ForgedSignature(t *testing.T) {
	f := newFixture(t)
	eventID := "evt_" + uuid.NewString()
	raw, _ := f.provider.Webhook(eventID, "session.updated", f.session)
	forger := fundingtest.NewProvider(f.clk, "whsec_wrong")
	status, outcome := f.deliver(raw, forger.Sign(raw, f.clk.Now()))
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "rejected", outcome)
	require.Equal(t, 1, f.securityEvents(webhook.SecurityEventSignatureFailed))
	_, _, found := f.providerEvent(eventID)
	require.False(t, found, "no evidence row for an unverified delivery")
	require.Equal(t, 0, f.effects(eventID))
	require.Equal(t, 0, f.archive.Len(), "nothing archived before verification")
	require.Equal(t, int32(0), f.disp.calls.Load())

	// Missing header entirely.
	status, _ = f.deliver(raw, http.Header{})
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, 2, f.securityEvents(webhook.SecurityEventSignatureFailed))
}

func TestIntegration_StaleTimestamp(t *testing.T) {
	f := newFixture(t)
	eventID := "evt_" + uuid.NewString()
	raw, _ := f.provider.Webhook(eventID, "session.updated", f.session)
	status, _ := f.deliver(raw, f.provider.Sign(raw, f.clk.Now().Add(-6*time.Minute)))
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, 1, f.securityEvents(webhook.SecurityEventTimestampStale))
	status, _ = f.deliver(raw, f.provider.Sign(raw, f.clk.Now().Add(6*time.Minute)))
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, 2, f.securityEvents(webhook.SecurityEventTimestampStale))
	_, _, found := f.providerEvent(eventID)
	require.False(t, found)
	require.Equal(t, int32(0), f.disp.calls.Load())
	// Within tolerance it is accepted.
	status, _ = f.deliver(raw, f.provider.Sign(raw, f.clk.Now().Add(-4*time.Minute)))
	require.Equal(t, http.StatusOK, status)
}

func TestIntegration_OversizedBody(t *testing.T) {
	f := newFixture(t)
	raw := bytes.Repeat([]byte("x"), 5000)
	status, outcome := f.deliver(raw, f.provider.Sign(raw, f.clk.Now()))
	require.Equal(t, http.StatusRequestEntityTooLarge, status)
	require.Equal(t, "too_large", outcome)
	require.Equal(t, int32(0), f.disp.calls.Load())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, f.server.URL, nil)
	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestIntegration_UnknownEventTypeIsIgnored(t *testing.T) {
	f := newFixture(t)
	eventID := "evt_" + uuid.NewString()
	raw, headers := f.provider.Webhook(eventID, "something.new", f.session)
	status, outcome := f.deliver(raw, headers)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "ignored", outcome)
	st, _, ok := f.providerEvent(eventID)
	require.True(t, ok)
	require.Equal(t, "IGNORED", *st)
	require.Equal(t, 0, f.effects(eventID))
	status, outcome = f.deliver(raw, headers)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "duplicate", outcome, "ignored events are deduplicated too")
	require.Equal(t, 1, f.archive.Len(), "raw request preserved once")
}

func TestIntegration_HandlerErrorIsFailedAndReprocessable(t *testing.T) {
	f := newFixture(t)
	eventID := "evt_" + uuid.NewString()
	raw, headers := f.provider.Webhook(eventID, "session.updated", f.session)
	f.disp.failing.Store(true)
	status, outcome := f.deliver(raw, headers)
	require.Equal(t, http.StatusInternalServerError, status)
	require.Equal(t, "failed", outcome)
	st, errText, ok := f.providerEvent(eventID)
	require.True(t, ok)
	require.Equal(t, "FAILED", *st)
	require.NotNil(t, errText)
	require.Contains(t, *errText, "INTERNAL")
	require.Equal(t, 0, f.effects(eventID))
	rec, found, err := event.NewInbox(f.clk).Get(context.Background(), testDB, f.provider.Name(), eventID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, event.StatusFailed, rec.Status)

	f.disp.failing.Store(false)
	status, outcome = f.deliver(raw, headers)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "processed", outcome)
	st, errText, _ = f.providerEvent(eventID)
	require.Equal(t, "PROCESSED", *st)
	require.Nil(t, errText)
	require.Equal(t, 1, f.effects(eventID))
}

func TestIntegration_PayloadMismatchAndArchiveDown(t *testing.T) {
	f := newFixture(t)
	eventID := "evt_" + uuid.NewString()
	raw, headers := f.provider.Webhook(eventID, "session.updated", f.session)
	status, _ := f.deliver(raw, headers)
	require.Equal(t, http.StatusOK, status)

	// Same event id, different body: never acknowledged silently.
	f.provider.SetStatus(f.session, funding.ProviderStatusProcessing, "fulfillment_processing")
	raw2, headers2 := f.provider.Webhook(eventID, "session.updated", f.session)
	require.NotEqual(t, raw, raw2)
	status, outcome := f.deliver(raw2, headers2)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "rejected", outcome)
	require.Equal(t, 1, f.securityEvents(webhook.SecurityEventPayloadMismatch))
	require.Equal(t, 1, f.effects(eventID))

	f.archive.Err = errs.New(errs.CodeProviderUnavailable, "archive down")
	raw3, headers3 := f.provider.Webhook("evt_"+uuid.NewString(), "session.updated", f.session)
	status, outcome = f.deliver(raw3, headers3)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "unavailable", outcome)
	require.Equal(t, int32(1), f.disp.calls.Load(), "no dispatch without a preserved raw request")
}

// TestIntegration_ARetriedDeliveryIsNotAnArchiveFailure.
//
// Evidence is archived BEFORE the inbox deduplicates, and the archive key is
// the event id plus the payload hash -- so every retry lands on exactly the
// same key with exactly the same bytes, and a write-once archive refuses it.
// If the pipeline treats that refusal as "archive unavailable" it answers 503,
// which is itself a request to retry, and a provider doing precisely what its
// delivery contract says never gets a 2xx from us.
//
// That is not hypothetical. It is what the deployed launch tier did to every
// Stripe retry, and no test in this package could see it: the archive double
// used to overwrite silently and never fail, so the duplicate tests above ran
// against an archive that had no write-once behaviour to get wrong.
func TestIntegration_ARetriedDeliveryIsNotAnArchiveFailure(t *testing.T) {
	f := newFixture(t)
	eventID := "evt_" + uuid.NewString()
	raw, headers := f.provider.Webhook(eventID, "session.updated", f.session)

	before := f.archive.Len()
	for i := 1; i <= 4; i++ {
		status, _ := f.deliver(raw, headers)
		require.Equal(t, http.StatusOK, status,
			"delivery %d was refused; a 503 here is a request for yet another retry", i)
	}

	require.Equal(t, before+1, f.archive.Len(),
		"four deliveries of one event stored more than one object")
	require.GreaterOrEqual(t, f.archive.Puts(), 4,
		"the archive must be asked on every delivery, not skipped after the first")
	require.Equal(t, 1, f.effects(eventID), "more than one economic effect")
}
