//go:build integration

package notifications_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/notifications"
)

// F-agnot: the follower's cursor can never pass a two-minute window that holds
// more rows than one batch, so a burst of >= DefaultBatch (200) rows inside
// DefaultLap (2 minutes) wedges that source permanently.
//
// runSource reads from (cursor - lap) with LIMIT batch and then sets the cursor
// to the LAST row it read. When the lap window already holds a full batch, the
// last row it read is BEHIND the cursor it started from, so the cursor moves
// backwards; the next pass reads the same prefix again and stops in the same
// place. Every row after the batch boundary -- and every row written
// afterwards, forever -- is never turned into a notification.
func TestAuditAgnot_ABurstWedgesTheFollowerCursorForever(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()

	// Start the cursor where a running process would have it.
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	start, _, _ := cursorOf(t, d, "credit_funding_transitions")

	// 250 captures inside one 2-minute lap: more than DefaultBatch (200).
	const burst = 250
	base := time.Now().Add(time.Second)
	ids := make([]string, 0, burst)
	for i := 0; i < burst; i++ {
		fundingID := aCreditFunding(t, d, acct)
		ids = append(ids, aTransition(t, d, fundingID, "CREATED", "CAPTURED",
			base.Add(time.Duration(i)*100*time.Millisecond)))
	}

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	assert.Equal(t, 200, n, "one pass drains one batch")
	after1, _, _ := cursorOf(t, d, "credit_funding_transitions")
	t.Logf("cursor: start=%s afterFirstPass=%s", start, after1)

	// Every later pass reads the same prefix again and the cursor never moves.
	for pass := 2; pass <= 6; pass++ {
		rec := &recorder{}
		n, err := f.RunOnce(ctx, d, rec)
		require.NoError(t, err)
		at, _, _ := cursorOf(t, d, "credit_funding_transitions")
		t.Logf("pass %d: wrote %d, cursor=%s", pass, n, at)
		assert.Equal(t, 0, n, "pass %d wrote nothing: it is re-reading rows it already emitted", pass)
		assert.True(t, at.Equal(after1), "pass %d: the cursor is stuck at %s", pass, at)
	}

	// The 50 rows past the batch boundary were never told to anybody.
	var told int
	require.NoError(t, d.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = 'CREDIT_PURCHASE_CAPTURED'`, uid).Scan(&told))
	assert.Equal(t, burst, told,
		"every capture in the burst should eventually be notified; %d of %d were", told, burst)

	// And nothing written AFTER the burst is ever seen again either.
	late := aCreditFunding(t, d, acct)
	lateTransition := aTransition(t, d, late, "CREATED", "CAPTURED", base.Add(10*time.Minute))
	for pass := 0; pass < 3; pass++ {
		_, err := f.RunOnce(ctx, d, &recorder{})
		require.NoError(t, err)
	}
	var seen int
	require.NoError(t, d.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND data->>'funding_id' = $2`, uid, late).Scan(&seen))
	assert.Equal(t, 1, seen,
		"a capture written after the burst is never notified: the source is wedged (transition %s)", lateTransition)

	_ = ids
}

// F-agnot: the two-minute lap re-publishes every data.changed signal it
// re-reads, whether or not the notification was new. Emit is idempotent and the
// unique index refuses the second row, but Change.Signal is appended
// unconditionally in runSource, so one fill's invalidation signals are
// broadcast once per pass for the whole lap -- eight times at a 15s tick, and
// for a market signal to EVERY connected client rather than to one.
func TestAuditAgnot_TheLapRepublishesTheSameSignalsEveryPass(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	// Another test in this package may have left this source's cursor behind;
	// start this one from now.
	_, err = d.Exec(ctx, `UPDATE notification_follower_cursors SET last_at = now(), last_id =
		'00000000-0000-0000-0000-000000000000' WHERE source = 'credit_funding_transitions'`)
	require.NoError(t, err)

	fundingID := aCreditFunding(t, d, acct)
	aTransition(t, d, fundingID, "CREATED", "CAPTURED", time.Now())

	first := &recorder{}
	n, err := f.RunOnce(ctx, d, first)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, first.notified, 1)
	require.NotEmpty(t, first.signals)

	second := &recorder{}
	n, err = f.RunOnce(ctx, d, second)
	require.NoError(t, err)
	assert.Zero(t, n, "no second notification: the dedup key holds")
	assert.Empty(t, second.notified, "nothing new is notified")
	assert.Empty(t, second.signals,
		"a pass that wrote nothing published %d data.changed signals for rows it had already emitted",
		len(second.signals))
}

// Control: a kind a person may not switch off is not switched off, whatever
// the preference row says. (Expected to pass.)
func TestAuditAgnot_AnUnsuppressibleKindIgnoresAStoredPreference(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	uid := newUser(t, d)
	p := producer()
	_ = ctx

	require.NoError(t, notifications.SavePreferences(asUser(uid), d, uid,
		map[notifications.Kind]bool{
			notifications.KindSecurityNewSession: false,
			notifications.KindAccountRestricted:  false,
			notifications.KindPayoutFailed:       false,
			notifications.KindSystem:             false,
			notifications.KindNativeTradeFilled:  false,
		}, time.Now()))

	for _, k := range []notifications.Kind{
		notifications.KindSecurityNewSession, notifications.KindAccountRestricted,
		notifications.KindPayoutFailed, notifications.KindSystem,
	} {
		out := emit(t, d, p, notifications.Notification{
			UserID: uid, Kind: k, Title: "t", Body: "b",
			Occurrence: "occ-" + string(k), OccurredAt: time.Now(),
		})
		assert.False(t, out.Suppressed, "%s must not be suppressible", k)
		assert.True(t, out.Created, "%s must still be written", k)
	}
	// And the one that IS suppressible is.
	out := emit(t, d, p, notifications.Notification{
		UserID: uid, Kind: notifications.KindNativeTradeFilled, Title: "t", Body: "b",
		Occurrence: "occ-fill", OccurredAt: time.Now(),
	})
	assert.True(t, out.Suppressed)
}

// F-agnot: the follower copies a person's IP address and User-Agent out of
// security_events -- a partitioned table whose partitions exist so months can
// be DROPped (00740) -- into notifications.data, a table no role can delete a
// row from: notifications_guard raises NOTIFICATION_IMMUTABLE on DELETE for
// every role including the owner, and no retention pass in cmd/api names the
// table. cp_readonly and cp_ops both hold SELECT on it.
func TestAuditAgnot_ALoginsIPAddressLandsInATableNothingCanPurge(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	uid := newUser(t, d)
	f := newFollower()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	_, err = d.Exec(ctx, `UPDATE notification_follower_cursors SET last_at = now(), last_id =
		'00000000-0000-0000-0000-000000000000' WHERE source = 'security_events_login'`)
	require.NoError(t, err)

	_, err = d.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, user_id, session_id, detail, ip, user_agent, occurred_at)
		 VALUES ($1::uuid, 'login', 'INFO', $2, $3::uuid, '{}'::jsonb, '198.51.100.42'::inet,
		         'Mozilla/5.0 (fingerprintable)', now())`,
		id.New[id.Any]().String(), uid, id.New[id.Any]().String())
	require.NoError(t, err)

	_, err = f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	var ip, ua string
	require.NoError(t, d.QueryRow(ctx,
		`SELECT data->>'ip', data->>'user_agent' FROM notifications
		  WHERE user_id = $1 AND kind = 'SECURITY_NEW_SESSION' ORDER BY created_at DESC LIMIT 1`,
		uid).Scan(&ip, &ua))
	require.Equal(t, "198.51.100.42", ip)
	require.Equal(t, "Mozilla/5.0 (fingerprintable)", ua)

	// Both analytics roles can read it.
	for _, role := range []string{"cp_readonly", "cp_ops"} {
		var can bool
		require.NoError(t, d.QueryRow(ctx,
			`SELECT has_table_privilege($1, 'public.notifications', 'SELECT')`, role).Scan(&can))
		assert.True(t, can, "%s can SELECT a customer's IP address out of notifications.data", role)
	}

	// And nothing can delete it. The owner role is the strongest the deployment
	// has and the guard refuses it too.
	ownerURL := os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	if ownerURL == "" {
		t.Skip("CP_TEST_MIGRATE_DATABASE_URL not set")
	}
	owner, err := db.Open(ctx, db.Config{URL: ownerURL, MaxConns: 2, MinConns: 1, AppName: "agnot-owner"})
	require.NoError(t, err)
	defer owner.Close()
	_, derr := owner.Exec(ctx, `DELETE FROM notifications WHERE user_id = $1`, uid)
	require.Error(t, derr, "expected the guard to refuse")
	assert.Contains(t, derr.Error(), "NOTIFICATION_IMMUTABLE")
	t.Fatalf("a login IP address is stored in notifications.data, readable by cp_readonly and cp_ops, "+
		"and undeletable by every role: %v", derr)
}
