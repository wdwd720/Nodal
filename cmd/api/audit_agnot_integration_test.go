//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stream"
)

// F-agnot: a live stream id and the durable resume cursor are two different
// clocks for the same notification, so Last-Event-ID silently skips rows.
//
//   - notifications.created_at is set from Notification.OccurredAt (the SOURCE
//     row's instant): Producer.Emit binds $13 to both created_at and
//     delivered_at.
//   - the id a LIVE event carries is assigned by Hub.Publish from the hub's
//     clock -- the instant the follower pass published it, which is one tick
//     plus however long the row waited, later.
//   - notificationResume answers Last-Event-ID with `created_at > since`.
//
// So a client whose last live event was published at P records E(P) and asks
// for everything after P; every notification whose OccurredAt is at or before P
// but which was WRITTEN after it -- which is every row the two-minute lap
// picks up, and every row from the tick the client was disconnected for -- is
// never returned. And because the hook "succeeded", Handler.ServeHTTP then
// SKIPS the buffer's copies of notification events, so the in-memory replay
// does not cover it either.
func TestAuditAgnot_LastEventIDSkipsNotificationsWrittenBehindThePublishInstant(t *testing.T) {
	d := openNotificationsDB(t)
	ctx := context.Background()

	user, err := accounts.NewRepository().CreateUser(ctx, d,
		"https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	principal := security.Principal{
		SubjectID: user.ID.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AuthTime: time.Now(),
	}
	ctx = security.WithPrincipal(ctx, principal)

	producer := notifications.NewProducer(time.Now, false)
	emitAt := func(occurrence string, at time.Time) notifications.Notification {
		var out notifications.Emitted
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var eerr error
			out, eerr = producer.Emit(ctx, tx, notifications.Notification{
				UserID: user.ID, Kind: notifications.KindSystem,
				Title: "Announcement", Body: "Something happened.",
				Occurrence: occurrence, OccurredAt: at,
			})
			return eerr
		}))
		require.True(t, out.Created)
		return out.Notification
	}

	now := time.Now().UTC()
	t1 := now.Add(-5 * time.Minute)  // N1 happened five minutes ago
	publishAt := now.Add(-time.Hour) // irrelevant placeholder, replaced below

	n1 := emitAt("n1", t1)

	// The follower publishes N1 one tick later. Hub.Publish stamps the id from
	// the hub's own clock, not from n1.OccurredAt.
	publishAt = t1.Add(30 * time.Second)
	hub := stream.NewHub(64, nil)
	hub.UseClock(func() time.Time { return publishAt })
	pub := hubPublisher{hub: hub}
	sub, _ := hub.Subscribe(principal, 0, 8)
	pub.Notify(n1)
	live := <-sub.Events()
	require.Equal(t, stream.TypeNotification, live.Type)

	// The client records that id and disconnects.
	since, ok := stream.EventTime(live.ID)
	require.True(t, ok, "a live event id must decode to an instant")
	assert.WithinDuration(t, publishAt, since, time.Second,
		"the live id encodes the PUBLISH instant, not the notification's own")

	// While it is away, a notification whose own instant sits between N1 and
	// the publish instant is written -- exactly what the two-minute lap and the
	// fifteen-second tick produce.
	n2 := emitAt("n2", t1.Add(10*time.Second))

	// The client reconnects with Last-Event-ID = live.ID.
	missed, truncated, err := notificationResume(d)(ctx, principal, since)
	require.NoError(t, err)
	require.False(t, truncated)

	found := false
	for _, e := range missed {
		if e.ResourceID == n2.ID.String() {
			found = true
		}
	}
	assert.True(t, found,
		"notification %s (occurred %s, written after the client left) is not in the %d events the resume returned; "+
			"Last-Event-ID %d decodes to %s and the query filters created_at > that",
		n2.ID, n2.OccurredAt, len(missed), live.ID, since)
}
