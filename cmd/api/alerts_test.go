package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/alert"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// This is F-118's actual claim, end to end: something raised inside
// reconciliation leaves the process.
//
// The finding was never that no alert type existed. `Metrics.Raise` has always
// incremented a counter, fed a meter and called an observer. The finding was
// that the observer had no production caller anywhere in the repository, so a
// ledger-integrity violation reached an in-process integer that died at exit.
// A test that only exercised internal/alert would prove a package works while
// leaving that gap exactly where it was -- so this one starts at Raise.
func TestAnAlertRaisedInReconciliationLeavesTheProcess(t *testing.T) {
	t.Parallel()
	var (
		mu  sync.Mutex
		got []alert.Event
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e alert.Event
		require.NoError(t, json.NewDecoder(r.Body).Decode(&e))
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := alert.NewDispatcher(alert.Options{
		Sink:   alert.NewWebhookSink(srv.URL, "test", alert.FormatGeneric, srv.Client()),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	m := reconciliation.NewMetrics(nil)
	attachAlertDispatcher(m, d, &config.Config{Env: config.EnvStaging, ServiceName: "nodal-api"})

	m.Raise(context.Background(), reconciliation.Alert{
		Name:     "ledger_integrity_violation",
		Severity: reconciliation.SEV1,
		Detail:   "debits and credits disagree",
		RecordID: "rec-9",
		Fields:   map[string]any{"order_id": "ord-9", "session_token": "must-not-egress"},
		At:       time.Now().UTC(),
	})
	d.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1, "an alert was raised and nothing arrived at the destination: F-118 is back")
	e := got[0]
	assert.Equal(t, "ledger_integrity_violation", e.Name)
	assert.Equal(t, alert.SEV1, e.Severity)
	assert.Equal(t, "rec-9", e.RecordID)
	assert.Equal(t, "ord-9", e.Fields["order_id"])
	// The allowlist is applied on the raise path, not only when a caller
	// remembers to. A field added to a Raise site tomorrow does not start
	// leaving the building on its own.
	assert.NotContains(t, e.Fields, "session_token")
	assert.Equal(t, 1, e.DroppedFields)
	// And the page says which deployment it came from.
	assert.Equal(t, "STAGING", e.Environment)
	assert.Equal(t, "nodal-api", e.Service)
}

// A control nobody executes decays to a claim. `attachAlertDispatcher` working
// when a test calls it proves nothing about the built service, so this asserts
// the composition root really calls it -- the same shape test/infra uses to
// check render.yaml, and for the same reason.
func TestTheServiceItselfAttachesTheDispatcher(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("wire.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "attachAlertDispatcher(",
		"the authority plane builds reconciliation metrics without attaching an alert destination; Raise would page nobody again")

	main, err := os.ReadFile("main.go")
	require.NoError(t, err)
	text := string(main)
	assert.Contains(t, text, "newAlertDispatcher(",
		"nothing constructs the dispatcher, so buildInput.alerts is always nil")
	assert.GreaterOrEqual(t, strings.Count(text, "alerts.Close()"), 2,
		"a shutdown path leaves without draining the alert queue; the alerts most worth delivering are the ones raised just before a stop")
	// Raise logs through LoggerFrom(ctx). The tickers run on the root context,
	// so if nothing puts the process logger there the "alert is said out loud"
	// line falls back to slog.Default's text handler and bypasses the JSON
	// logger the deployment reads.
	assert.Contains(t, text, "observability.WithLogger(ctx, log)",
		"the root context carries no logger; a background pass that raises an alert logs through the fallback")
}

// The absence of a destination is a working configuration, and it must say so.
// LOCAL and TEST have no webhook; what must not happen is a service that is
// quietly unalerted, which is the state F-118 described.
func TestNoDestinationIsSaidOutLoud(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{Env: config.EnvLocal}

	d := newAlertDispatcher(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log)
	assert.Nil(t, d, "a dispatcher was built with no destination to send to")
	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "CP_ALERT_WEBHOOK_URL",
		"silence about alerting is the finding; the log line must name the variable that would fix it")
	assert.Contains(t, out, "consequence",
		"a control that is off must name what will not happen, not merely that it is off")
}

// A delivery that outlives the shutdown budget it drains inside turns a clean
// stop into a kill, so the timeout is trimmed rather than trusted.
func TestDeliveryTimeoutFitsInsideTheShutdownBudget(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: config.EnvLocal}
	cfg.API.ShutdownTimeout = 5 * time.Second
	cfg.Alert.Timeout = 30 * time.Second
	cfg.Alert.MinSeverity = string(alert.SEV2)
	cfg.Alert.WebhookURL = config.SecretRef("https://alerts.example.invalid/hook")

	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	d := newAlertDispatcher(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log)
	require.NotNil(t, d)
	defer d.Close()

	assert.Contains(t, buf.String(), "trimmed",
		"a 30s delivery was accepted under a 5s shutdown budget: the drain cannot finish and the platform kills the process")
	// The destination is named without printing it: a webhook URL is its own
	// credential.
	assert.Contains(t, buf.String(), "alerts.example.invalid")
	assert.NotContains(t, buf.String(), "/hook")
	// And the shape it will be spoken to in is said, so an operator who pasted
	// a Slack URL can see "generic" and know why Slack is answering 400.
	assert.Contains(t, buf.String(), "(generic)")
}
