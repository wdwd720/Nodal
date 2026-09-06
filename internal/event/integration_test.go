//go:build integration

package event_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/event"
)

// The suite needs an isolated database (go run ./scripts/testdb -name event).
// It truncates outbox_events and inbox_messages between tests, so it refuses
// the shared controlplane_test database.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	testAdmin      *pgx.Conn
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	for _, u := range []string{testAppURL, testMigrateURL} {
		parsed, err := url.Parse(u)
		if err != nil || strings.TrimPrefix(parsed.Path, "/") == "controlplane_test" {
			fmt.Fprintln(os.Stderr, "event integration: refusing the shared controlplane_test database; use scripts/testdb -name event")
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The database is provisioned and migrated by scripts/testdb. Up is a
	// no-op when current; if it refuses (for example out-of-order migrations
	// added since provisioning) the suite only needs migration 00002, so it
	// proceeds as long as both tables exist.
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		if !tablesExist(ctx, testMigrateURL) {
			fmt.Fprintln(os.Stderr, "event integration: migrate up:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "event integration: migrate up refused, continuing with existing schema:", err)
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "event-itest", MaxConns: 40})
	if err != nil {
		fmt.Fprintln(os.Stderr, "event integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	testAdmin, err = pgx.Connect(ctx, testMigrateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "event integration: connect as migrate role:", err)
		return 1
	}
	defer func() { _ = testAdmin.Close(ctx) }()

	return m.Run()
}

func tablesExist(ctx context.Context, url string) bool {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename IN ('outbox_events', 'inbox_messages')`).Scan(&n)
	return err == nil && n == 2
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

// resetTables empties both tables as the owning role (cp_app cannot delete).
func resetTables(t *testing.T) {
	t.Helper()
	requireEnv(t)
	_, err := testAdmin.Exec(context.Background(), "TRUNCATE outbox_events, inbox_messages")
	require.NoError(t, err)
}

var itestStart = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

type fixture struct {
	clk    *clock.Fake
	outbox *event.Outbox
	inbox  *event.Inbox
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	resetTables(t)
	clk := clock.NewFake(itestStart)
	return fixture{clk: clk, outbox: event.NewOutbox(clk), inbox: event.NewInbox(clk)}
}

func (f fixture) envelope(topic event.Topic, aggregateID string) event.Envelope {
	sp, _ := event.Lookup(topic)
	return event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          string(topic),
		SchemaVersion: sp.SchemaVersion,
		Source:        "event-itest",
		AggregateType: sp.AggregateType,
		AggregateID:   aggregateID,
		CorrelationID: "corr-" + aggregateID,
		OccurredAt:    f.clk.Now(),
		Headers:       map[string]string{"x-trace": "t-" + aggregateID},
		Payload:       json.RawMessage(`{"aggregate":"` + aggregateID + `","amount":"1.00"}`),
	}
}

// enqueue commits the events in one transaction.
func (f fixture) enqueue(t *testing.T, topic event.Topic, events ...event.Envelope) {
	t.Helper()
	err := testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return f.outbox.Enqueue(ctx, tx, topic.String(), events...)
	})
	require.NoError(t, err)
}

type outboxState struct {
	published bool
	attempts  int
	lastError *string
}

func outboxRowState(t *testing.T, id string) outboxState {
	t.Helper()
	var (
		s           outboxState
		publishedAt *time.Time
	)
	err := testDB.QueryRow(context.Background(),
		`SELECT published_at, publish_attempts, last_error FROM outbox_events WHERE id = $1`, id).
		Scan(&publishedAt, &s.attempts, &s.lastError)
	require.NoError(t, err)
	s.published = publishedAt != nil
	return s
}

func countUnpublished(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n))
	return n
}

func ids(msgs []event.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}
