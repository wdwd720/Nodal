//go:build integration

package notifications_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
)

// AUDIT (platform-hardening) — F-platform-1.
//
// Follower.runSource reads from (cursor - DefaultLap), takes at most
// DefaultBatch rows, and then sets the cursor to the LAST row of that batch.
// The new cursor is therefore always the 200th row counted from a point two
// minutes BEHIND where the cursor already stood — so the cursor can never
// advance past a two-minute window that holds 200 or more rows.
//
// Once a source produces DefaultBatch (200) rows inside one DefaultLap
// (2 minutes), every later pass re-reads the same first 200 rows of that
// window, deduplicates all of them, and writes the same instant back. The
// source is stuck at that instant for the life of the deployment: every later
// row — a reversal, a failed payout, a frozen account, a new sign-in — sits
// behind a cursor that will never reach it, and the dedup index makes the
// repeated re-read completely silent. Nothing logs, nothing alerts, and
// `emitted` stops climbing.
//
// It is reachable by one ordinary caller. The Command rate limit is 120/min
// and readNativeFills has no state filter, so a single trader spending their
// own budget writes 240 fills inside the lap window.
//
// The test uses credit_funding_transitions because that is the fixture this
// package already has; the defect is in Follower.runSource and applies to all
// six sources.
func TestAudit_FollowerStallsForeverAfterABatchSizedBurst(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	f := newFollower()

	// Start the cursor at "now", the way a running process does.
	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	start, _, _ := cursorOf(t, d, "credit_funding_transitions")

	// 260 captured purchases (DefaultBatch is 200), all inside one 52-second
	// window, which is well inside the 2-minute lap. Each belongs to its own
	// user so nothing is collapsed by a dedup key.
	const total = 260
	base := start.Add(1 * time.Second)
	for i := 0; i < total; i++ {
		uid := newUser(t, d)
		acct := newAccount(t, d, uid)
		fundingID := aCreditFunding(t, d, acct)
		aTransition(t, d, fundingID, "CREATED", "CAPTURED",
			base.Add(time.Duration(i)*200*time.Millisecond))
	}

	// Pass one: 200 of the 260 are told, which is the batch bound working as
	// designed. The remaining 60 are meant to drain on the next passes.
	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 200, n, "one pass is bounded by DefaultBatch")
	stuckAt, _, _ := cursorOf(t, d, "credit_funding_transitions")

	// Five more passes. A backlog is supposed to "drain over several passes".
	for pass := 2; pass <= 6; pass++ {
		if _, err := f.RunOnce(ctx, d, &recorder{}); err != nil {
			require.NoError(t, err)
		}
		at, _, _ := cursorOf(t, d, "credit_funding_transitions")
		assert.True(t, at.After(stuckAt),
			"pass %d did not move the cursor: it is pinned at %s, the 200th row inside its own lap window",
			pass, stuckAt)
	}

	assert.Zero(t, unnotifiedCaptures(t, d, base),
		"purchases that were captured and that the follower will never report")

	// And the source is dead, not merely behind: a purchase captured an hour
	// later is still behind the pinned cursor, so it is never reported either.
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	fundingID := aCreditFunding(t, d, acct)
	aTransition(t, d, fundingID, "CREATED", "CAPTURED", time.Now().Add(time.Hour))

	rec = &recorder{}
	n, err = f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	assert.Equal(t, 1, n,
		"a purchase captured long after the burst was not reported: the follower is stalled for good")
	assert.Equal(t, 1, countFor(t, d, uid))
}

// unnotifiedCaptures counts CAPTURED transitions at or after `since` that
// produced no notification row.
func unnotifiedCaptures(t *testing.T, d *db.DB, since time.Time) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(context.Background(), `
		SELECT count(*) FROM credit_funding_transitions t
		 WHERE t.to_state = 'CAPTURED' AND t.occurred_at >= $1
		   AND NOT EXISTS (
		       SELECT 1 FROM notifications x
		        WHERE x.resource_type = 'credit_funding' AND x.resource_id = t.funding_id::text)`,
		since.UTC()).Scan(&n))
	return n
}
