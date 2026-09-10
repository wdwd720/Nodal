package reconciliation

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/observability"
)

// An alert says something out loud (F-118).
//
// Raise used to be silent. It incremented an in-process atomic, added to an
// OTel counter only when a FinancialMetrics was attached, and called an
// observer only when one was registered -- and in the deployed system all three
// are absent: both composition roots passed NoopMetrics(), OnAlert has no
// caller anywhere in the repository, and CP_TELEMETRY_OTLP_ENDPOINT is unset so
// the meter provider is a no-op.
//
// So a ledger integrity violation incremented a counter in RAM that no exporter
// read and the process discarded on exit. Nobody was told anything.
//
// A log line is not paging, and this does not claim to be: nothing in this
// deployment pages. It is the difference between an incident that is findable
// in the service's logs afterwards and one that left no trace at all.
func TestMetrics_AnAlertIsSaidOutLoud(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	ctx := observability.WithLogger(context.Background(),
		slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// The shape the deployment actually runs: no exporter, no observer.
	m := NoopMetrics()
	m.Raise(ctx, Alert{Name: AlertLedgerIntegrity, Severity: SEV1, Detail: "balances disagree", RecordID: "rec-1"})
	m.Raise(ctx, Alert{Name: AlertUnknownTransaction, Severity: SEV2, Detail: "no observer answered", RecordID: "rec-2"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2, "an alert nobody exports and nobody observes still has to be recorded")

	var first, second map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))

	// SEV1 at ERROR so it is separable from ordinary traffic.
	assert.Equal(t, "ERROR", first["level"])
	assert.Equal(t, AlertLedgerIntegrity, first["alert"])
	assert.Equal(t, "rec-1", first["record_id"])
	assert.Equal(t, "balances disagree", first["detail"])

	assert.Equal(t, "WARN", second["level"])
	assert.Equal(t, AlertUnknownTransaction, second["alert"])

	// The in-process counters still work, because the health endpoint and the
	// tests read them.
	assert.EqualValues(t, 1, m.SEV1Count())
	assert.EqualValues(t, 1, m.SEV2Count())
	assert.EqualValues(t, 1, m.AlertCount(AlertLedgerIntegrity))
}
