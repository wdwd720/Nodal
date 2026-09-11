//go:build integration

package notifications_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/notifications"
)

// recorder captures what a pass published, so a test can assert that a
// re-read window publishes nothing rather than merely writing nothing.
type recorder struct {
	notified []notifications.Notification
	signals  []notifications.Signal
}

func (r *recorder) Notify(n notifications.Notification) { r.notified = append(r.notified, n) }
func (r *recorder) Signal(s notifications.Signal)       { r.signals = append(r.signals, s) }

func newFollower() *notifications.Follower {
	return notifications.NewFollower(producer())
}

// aCreditFunding inserts a funding row in its birth state. Its state moves only
// through a transition row (migration 00743), which is exactly what the
// follower reads.
func aCreditFunding(t *testing.T, d *db.DB, acct accounts.AccountID) string {
	t.Helper()
	fundingID := id.New[id.Any]().String()
	_, err := d.Exec(context.Background(),
		`INSERT INTO credit_fundings (id, account_id, provider, state, credit_quantity, paid_amount_minor, idempotency_key)
		 VALUES ($1::uuid, $2, 'test_provider', 'CREATED', 1000, 500, $3)`,
		fundingID, acct, "idem-"+fundingID)
	require.NoError(t, err)
	return fundingID
}

// aTransition moves a funding and returns the transition row's id.
func aTransition(t *testing.T, d *db.DB, fundingID, from, to string, at time.Time) string {
	t.Helper()
	transitionID := id.New[id.Any]().String()
	_, err := d.Exec(context.Background(),
		`INSERT INTO credit_funding_transitions (id, funding_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, 'SYSTEM', 'test', 'test fixture', $5)`,
		transitionID, fundingID, from, to, at.UTC())
	require.NoError(t, err)
	return transitionID
}

// freshCursors forgets where the follower stopped, so a test that says "start
// the cursor before anything happens" really does.
//
// Every test in this package shares one database and the cursors are one row
// per source, so a test that stamps a row in the future -- the burst
// reproduction stamps 260 of them across 52 seconds -- leaves the next test's
// cursor standing after its own fixture. That is harmless in production (the
// follower reports such a row through its lap, and the cursor may not stand
// more than half a lap ahead of the database's clock) and it is fatal to a test
// that asserts WHERE the cursor ended up. Forgetting the row is the same state
// a deployment that has never run the follower is in, which is the state each of
// these tests describes in its first line.
func freshCursors(t *testing.T, d *db.DB) {
	t.Helper()
	_, err := d.Exec(context.Background(),
		`UPDATE notification_follower_cursors
		    SET last_at = now(), last_id = '00000000-0000-0000-0000-000000000000', pending_at = NULL`)
	require.NoError(t, err)
}

func cursorOf(t *testing.T, d *db.DB, source string) (time.Time, string, *time.Time) {
	t.Helper()
	var at time.Time
	var rowID string
	var pending *time.Time
	err := d.QueryRow(context.Background(),
		`SELECT last_at, last_id::text, pending_at FROM notification_follower_cursors WHERE source = $1`,
		source).Scan(&at, &rowID, &pending)
	require.NoError(t, err)
	return at.UTC(), rowID, pending
}

// TestIntegration_EverySourceQueryRunsAgainstTheRealSchema.
//
// Six hand-written queries over six tables owned by other packages. A renamed
// column in any of them is a follower that stops silently, and "silently" is
// the whole problem with a notification system. This runs every source against
// the migrated schema and fails on the first one that will not execute.
func TestIntegration_EverySourceQueryRunsAgainstTheRealSchema(t *testing.T) {
	d := openDB(t)
	f := newFollower()
	rec := &recorder{}

	n, err := f.RunOnce(context.Background(), d, rec)
	require.NoError(t, err, "every source query must execute against the real schema")
	assert.Zero(t, n)

	for _, source := range f.SourceNames() {
		at, _, pending := cursorOf(t, d, source)
		assert.False(t, at.IsZero(), "%s: a source with no cursor starts at now, not at the beginning of history", source)
		assert.Nil(t, pending, "%s: a pass that finished leaves nothing in flight", source)
	}
}

// TestIntegration_TheFollowerNotifiesOnceForACapturedPurchase, and publishes
// once, and advances its cursor once.
func TestIntegration_TheFollowerNotifiesOnceForACapturedPurchase(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()
	ctx := context.Background()

	// Start the cursor before anything happens, the way a running process does.
	freshCursors(t, d)
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	fundingID := aCreditFunding(t, d, acct)
	transitionID := aTransition(t, d, fundingID, "CREATED", "CAPTURED", time.Now())

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.Len(t, rec.notified, 1)
	got := rec.notified[0]
	assert.Equal(t, uid, got.UserID)
	assert.Equal(t, notifications.KindCreditPurchaseCaptured, got.Kind)
	assert.Equal(t, "Your Credits are available", got.Title)
	assert.Equal(t, notifications.Ref{Type: "credit_funding", ID: fundingID}, got.Ref)
	var data map[string]any
	require.NoError(t, json.Unmarshal(got.Data, &data))
	assert.Equal(t, "1000", data["credit_quantity"],
		"a base-unit integer travels as a string; nothing here is a float")
	assert.Equal(t, "CAPTURED", data["to_state"])
	require.NotEmpty(t, rec.signals)
	assert.Equal(t, notifications.Signal{UserID: uid.String(), Scope: notifications.ScopeBalance}, rec.signals[0])

	at, lastID, pending := cursorOf(t, d, "credit_funding_transitions")
	assert.Equal(t, transitionID, lastID)
	assert.False(t, at.IsZero())
	assert.Nil(t, pending)

	// The next pass re-reads its own lap and must publish nothing.
	rec2 := &recorder{}
	n, err = f.RunOnce(ctx, d, rec2)
	require.NoError(t, err)
	assert.Zero(t, n, "a re-read window writes nothing")
	assert.Empty(t, rec2.notified, "and publishes nothing")
	assert.Equal(t, 1, countFor(t, d, uid))
}

// TestIntegration_ACrashBetweenReadingAndEmittingDuplicatesNothing.
//
// The pass is one transaction holding both the emits and the cursor advance, so
// a crash anywhere inside it leaves the OLD position and no notifications. The
// next pass therefore re-reads the same window -- which is the case this
// simulates by winding the cursor back -- and the unique index on
// (user_id, dedup_key) is what makes the re-read produce nothing rather than a
// second telling.
func TestIntegration_ACrashBetweenReadingAndEmittingDuplicatesNothing(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()
	ctx := context.Background()

	freshCursors(t, d)
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	fundingID := aCreditFunding(t, d, acct)
	aTransition(t, d, fundingID, "CREATED", "CAPTURED", time.Now())

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	before, _, _ := cursorOf(t, d, "credit_funding_transitions")

	// The crash: the pass ran, the notification committed, and the position on
	// record is the one from before it. (An interrupted pass leaves exactly
	// this state, with pending_at set to say a pass was in flight.)
	_, err = d.Exec(ctx,
		`UPDATE notification_follower_cursors
		    SET last_at = $1, last_id = '00000000-0000-0000-0000-000000000000', pending_at = $2
		  WHERE source = 'credit_funding_transitions'`,
		before.Add(-time.Minute), before)
	require.NoError(t, err)

	rec2 := &recorder{}
	n, err = f.RunOnce(ctx, d, rec2)
	require.NoError(t, err)
	assert.Zero(t, n, "re-reading a window after a crash must not tell anybody twice")
	assert.Empty(t, rec2.notified)
	assert.Equal(t, 1, countFor(t, d, uid))

	_, _, pending := cursorOf(t, d, "credit_funding_transitions")
	assert.Nil(t, pending, "a completed pass clears the in-flight marker it found")
}

// TestIntegration_TheFollowerFindsARowThatCommittedBehindItsCursor.
//
// occurred_at defaults to now(), which is the transaction's START time. A
// transaction that began before the cursor passed and committed after it writes
// a row the cursor has already gone past, and no ordering can fix that. The lap
// is what finds it; the dedup key is what makes the lap free.
func TestIntegration_TheFollowerFindsARowThatCommittedBehindItsCursor(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()
	ctx := context.Background()

	freshCursors(t, d)
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	fundingID := aCreditFunding(t, d, acct)
	aTransition(t, d, fundingID, "CREATED", "CAPTURED", time.Now())
	n, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	after, _, _ := cursorOf(t, d, "credit_funding_transitions")

	// The late committer: stamped thirty seconds before the cursor's position,
	// visible only now.
	late := aCreditFunding(t, d, acct)
	aTransition(t, d, late, "CREATED", "CAPTURED", after.Add(-30*time.Second))

	rec := &recorder{}
	n, err = f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a row that committed behind the cursor must not be lost")
	require.Len(t, rec.notified, 1)
	assert.Equal(t, late, rec.notified[0].Ref.ID)
	assert.Equal(t, 2, countFor(t, d, uid))
}

// TestIntegration_NobodyIsToldAboutHistoryTheFollowerNeverSaw: a cursor with no
// row starts at now, not at the beginning of time. The first pass on a database
// with a year of transitions must not notify a year of them.
func TestIntegration_NobodyIsToldAboutHistoryTheFollowerNeverSaw(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	ctx := context.Background()

	fundingID := aCreditFunding(t, d, acct)
	aTransition(t, d, fundingID, "CREATED", "CAPTURED", time.Now().Add(-24*time.Hour))

	f := newFollower()
	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	assert.Zero(t, n, "the first pass starts now; it does not replay history at everybody")
	assert.Zero(t, countFor(t, d, uid))
}

// TestIntegration_ANewSessionTellsThePersonWhoSignedIn. The source is the login
// security event rather than the sessions row, because a session row is UPDATEd
// on every request and a cursor over it would re-read every live session
// forever.
func TestIntegration_ANewSessionTellsThePersonWhoSignedIn(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	f := newFollower()
	ctx := context.Background()

	freshCursors(t, d)
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	sessionID := id.New[id.Any]().String()
	_, err = d.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, user_id, session_id, detail, ip, user_agent, occurred_at)
		 VALUES ($1::uuid, 'login', 'INFO', $2, $3::uuid, '{}'::jsonb, '203.0.113.7'::inet, 'Firefox', now())`,
		id.New[id.Any]().String(), uid, sessionID)
	require.NoError(t, err)

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, rec.notified, 1)
	got := rec.notified[0]
	assert.Equal(t, notifications.KindSecurityNewSession, got.Kind)
	assert.Equal(t, uid, got.UserID)
	assert.Equal(t, notifications.Ref{Type: "session", ID: sessionID}, got.Ref)
	assert.Equal(t, notifications.SeverityCritical, got.Severity)
	var loginData map[string]any
	require.NoError(t, json.Unmarshal(got.Data, &loginData))
	assert.Equal(t, "203.0.113.7", loginData["ip"], "was this me is only answerable with the address and the agent")
	assert.Equal(t, "Firefox", loginData["user_agent"])

	// It cannot be switched off, so a preference does not stop the next one.
	require.NoError(t, notifications.SavePreferences(asUser(uid), d, uid,
		map[notifications.Kind]bool{notifications.KindSecurityNewSession: false}, time.Now()))
	_, err = d.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, user_id, session_id, detail, occurred_at)
		 VALUES ($1::uuid, 'login', 'INFO', $2, $3::uuid, '{}'::jsonb, now())`,
		id.New[id.Any]().String(), uid, id.New[id.Any]().String())
	require.NoError(t, err)
	n, err = f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 2, countFor(t, d, uid))
}

// TestIntegration_AnAccountLeavingActiveTellsItsOwner.
func TestIntegration_AnAccountLeavingActiveTellsItsOwner(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()
	ctx := context.Background()

	freshCursors(t, d)
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	_, err = d.Exec(ctx,
		`INSERT INTO account_status_transitions (id, account_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1::uuid, $2, 'ACTIVE', 'FROZEN', 'OPERATOR', 'op-1', 'a compliance review', now())`,
		id.New[id.Any]().String(), acct)
	require.NoError(t, err)

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got := rec.notified[0]
	assert.Equal(t, notifications.KindAccountRestricted, got.Kind)
	assert.Equal(t, "Your account is frozen", got.Title)
	assert.Contains(t, got.Body, "a compliance review")
	assert.NotContains(t, got.Body, "op-1", "the operator who acted is not named to the customer")
}
