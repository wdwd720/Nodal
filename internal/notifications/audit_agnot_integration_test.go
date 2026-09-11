//go:build integration

package notifications_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/notifications"
)

// F-agnot (the follower cursor wedge, fixed on fix/platform as F-167): a burst
// of more rows than one batch inside one lap used to pin the cursor at the
// 200th row of its own re-read window, forever. runSource now drains from the
// cursor itself and re-reads the lap separately (D-097), so a burst is told in
// as many passes as it needs and the cursor only ever moves forward.
//
// The fixture is stamped in the PAST on purpose. Every test in this package
// shares one database and the cursor is one row per source; a row stamped in
// the future stays ahead of every later test's cursor until the clock catches
// up, and a working follower keeps advancing to it -- which is how this test's
// first version, stamped up to 26 seconds ahead, made two follower tests after
// it report a cursor that was not theirs. Ten minutes behind, the rows are
// outside every later lap and touch nothing.
func TestAuditAgnot_ABurstDoesNotWedgeTheFollowerCursor(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()

	// Create the cursor rows, then stand this source's cursor ten minutes ago.
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	start := time.Now().Add(-10 * time.Minute)
	_, err = d.Exec(ctx, `UPDATE notification_follower_cursors SET last_at = $1, last_id =
		'00000000-0000-0000-0000-000000000000' WHERE source = 'credit_funding_transitions'`, start.UTC())
	require.NoError(t, err)

	// 250 captures inside 25 seconds, nine minutes ago: more than DefaultBatch
	// (200) inside one lap, all ahead of the cursor.
	const burst = 250
	base := time.Now().Add(-9 * time.Minute)
	for i := 0; i < burst; i++ {
		fundingID := aCreditFunding(t, d, acct)
		aTransition(t, d, fundingID, "CREATED", "CAPTURED", base.Add(time.Duration(i)*100*time.Millisecond))
	}

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	assert.Equal(t, 200, n, "one pass drains one batch")
	after1, _, _ := cursorOf(t, d, "credit_funding_transitions")
	require.True(t, after1.After(start), "the first pass moved the cursor forward")

	// The second pass drains the rest; the ones after it find nothing and
	// leave the cursor where it stands. It never goes backwards.
	n, err = f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	assert.Equal(t, burst-200, n, "the second pass drains the remainder of the burst")
	after2, _, _ := cursorOf(t, d, "credit_funding_transitions")
	assert.True(t, after2.After(after1), "the second pass moved the cursor past the batch boundary")
	for pass := 3; pass <= 6; pass++ {
		n, err := f.RunOnce(ctx, d, &recorder{})
		require.NoError(t, err)
		at, _, _ := cursorOf(t, d, "credit_funding_transitions")
		assert.Zero(t, n, "pass %d found nothing new", pass)
		assert.True(t, at.Equal(after2), "pass %d moved a cursor that had nothing to move for: %s", pass, at)
	}

	var told int
	require.NoError(t, d.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = 'CREDIT_PURCHASE_CAPTURED'`, uid).Scan(&told))
	assert.Equal(t, burst, told, "every capture in the burst is notified; %d of %d were", told, burst)

	// And a capture written after the burst is seen on the next pass.
	late := aCreditFunding(t, d, acct)
	aTransition(t, d, late, "CREATED", "CAPTURED", base.Add(5*time.Minute))
	n, err = f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a capture after the burst is reported on the next pass")
	var seen int
	require.NoError(t, d.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND data->>'funding_id' = $2`, uid, late).Scan(&seen))
	assert.Equal(t, 1, seen)
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

// F-188 (was TestAuditAgnot_ALoginsIPAddressLandsInATableNothingCanPurge on
// audit/agents-notifications @ 8a69aaa, which asserted the address was there).
//
// The follower copied a person's IP address and User-Agent out of
// security_events -- a partitioned table whose partitions exist so months can
// be DROPped (00740) -- into notifications.data, a table no role can delete a
// row from: notifications_guard raises NOTIFICATION_IMMUTABLE on DELETE for
// every role including the owner, and no retention pass in cmd/api names the
// table. cp_readonly and cp_ops both held SELECT on it.
//
// Three things changed and this asserts all three: the notification carries a
// /24 locality and a two-word device summary instead of the address and the
// header; the exact address is still in the trail that CAN be dropped, one
// documented route away; and neither analytics role can read the table at all
// (D-106). What has not changed is the guard: nothing deletes a notification,
// which is exactly why what goes into one is now bounded.
func TestAuditAgnot_ALoginsIPAddressDoesNotLandInATableNothingCanPurge(t *testing.T) {
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

	var data []byte
	require.NoError(t, d.QueryRow(ctx,
		`SELECT data FROM notifications
		  WHERE user_id = $1 AND kind = 'SECURITY_NEW_SESSION' ORDER BY created_at DESC LIMIT 1`,
		uid).Scan(&data))
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.NotContains(t, got, "ip")
	assert.NotContains(t, got, "user_agent")
	assert.Equal(t, "198.51.100.0/24", got["ip_prefix"], "a /24 says 'not where you live' and nothing finer")
	assert.Equal(t, "/v1/me/audit", got["exact_address_at"])

	// The exact address is still recorded -- in the partitioned trail the
	// retention pass drops a month at a time.
	var exact string
	require.NoError(t, d.QueryRow(ctx,
		`SELECT host(ip) FROM security_events WHERE user_id = $1 AND kind = 'login'`, uid).Scan(&exact))
	assert.Equal(t, "198.51.100.42", exact)

	// Neither analytics role can read the table at all (ADR-0021 section 4),
	// and the DELETE grant cp_ops held, which the guard refuses anyway, is
	// gone with it.
	for _, role := range []string{"cp_readonly", "cp_ops"} {
		var canRead, canDelete bool
		require.NoError(t, d.QueryRow(ctx,
			`SELECT has_table_privilege($1, 'public.notifications', 'SELECT'),
			        has_table_privilege($1, 'public.notifications', 'DELETE')`, role).Scan(&canRead, &canDelete))
		assert.False(t, canRead, "%s can read a customer's notification centre", role)
		assert.False(t, canDelete, "%s holds a DELETE the table's own guard refuses", role)
	}

	// And nothing can delete it. The owner role is the strongest the deployment
	// has and the guard refuses it too -- which is the whole reason the address
	// above is coarse: what is written here is kept forever.
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
}
