//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
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

// The two halves of the composition root that only exist here: the adapter that
// turns a follower pass into stream events, and the hook that answers
// Last-Event-ID from the table rather than from a buffer a redeploy emptied.
//
// internal/stream tests the hook's SHAPE against a fake. This tests the real
// one against the real table, because "the durable resume works" is a claim
// about a query, and a fake would agree with whatever it was told.

func openNotificationsDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 4, MinConns: 1, AppName: "api-notifications-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func TestIntegration_NotificationResumeReadsTheTableNotTheBuffer(t *testing.T) {
	d := openNotificationsDB(t)
	ctx := context.Background()

	repo := accounts.NewRepository()
	user, err := repo.CreateUser(ctx, d, "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)

	principal := security.Principal{
		SubjectID: user.ID.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AuthTime: time.Now(),
	}
	ctx = security.WithPrincipal(ctx, principal)

	// Three notifications about things that happened an hour ago, written now.
	// The gap between the two instants is the whole subject of D-104: a resume
	// filtered on the occurrence would have answered every one of these with
	// "you are up to date".
	base := time.Now().UTC().Add(-time.Hour)
	producer := notifications.NewProducer(time.Now, false)
	written := make([]notifications.Notification, 0, 3)
	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			out, eerr := producer.Emit(ctx, tx, notifications.Notification{
				UserID: user.ID, Kind: notifications.KindSystem,
				Title: "Announcement", Body: "Something happened.",
				Occurrence: string(rune('a' + i)), OccurredAt: at,
			})
			if eerr == nil {
				written = append(written, out.Notification)
			}
			return eerr
		}))
	}
	require.Len(t, written, 3)

	resume := notificationResume(d)

	// The instant the FIRST notification was WRITTEN at: the two after it are
	// what a client reconnecting from there missed. It is inserted_at that
	// decides, not created_at -- every one of these occurred an hour ago.
	missed, truncated, err := resume(ctx, principal, written[0].InsertedAt)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, missed, 2)
	for i, e := range missed {
		assert.Equal(t, stream.TypeNotification, e.Type)
		assert.Equal(t, user.ID.String(), e.UserID)
		assert.Equal(t, stream.EventIDAt(written[i+1].InsertedAt), e.ID,
			"the replayed id must encode the instant the row was written, which is what Since filters on, "+
				"so a client that disconnects mid-replay resumes from where it got to and never moves backwards")
		var payload map[string]any
		require.NoError(t, json.Unmarshal(e.Data, &payload))
		assert.Equal(t, "SYSTEM", payload["kind"])
		assert.Equal(t, "Announcement", payload["title"])
		assert.NotContains(t, payload, "body", "a stream event carries identifiers and a title, never the body")
		if i > 0 {
			assert.False(t, e.ID < missed[i-1].ID, "oldest first")
		}
	}

	// F-186 stated directly: a cursor past every one of these occurrences, but
	// before they were written, misses nothing. Under the old filter this
	// returned an empty list and the person was never told.
	behind, _, err := resume(ctx, principal, base.Add(3*time.Minute))
	require.NoError(t, err)
	assert.Len(t, behind, 3,
		"a notification written after the cursor is returned however old the thing it describes is")

	// Caught up: nothing to replay.
	missed, _, err = resume(ctx, principal, time.Now().UTC())
	require.NoError(t, err)
	assert.Empty(t, missed)

	// An operator session whose subject is not a customer identity has no
	// centre to resume from, and that is not an error.
	missed, _, err = resume(ctx, security.Principal{SubjectID: "not-a-uuid", ActorType: security.ActorOperator}, base)
	require.NoError(t, err)
	assert.Empty(t, missed)
}

func TestIntegration_TheHubPublisherAddressesTheRecipientAndNobodyElse(t *testing.T) {
	hub := stream.NewHub(16, nil)
	hub.UseClock(time.Now)
	pub := hubPublisher{hub: hub}

	mine := accounts.NewUserID()
	theirs := accounts.NewUserID()
	me := security.Principal{
		SubjectID: mine.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AuthTime: time.Now(),
	}

	pub.Notify(notifications.Notification{
		ID: notifications.NewID(), UserID: theirs, Kind: notifications.KindSystem,
		Severity: notifications.SeverityInfo, Title: "not for you", Body: "b", OccurredAt: time.Now().UTC(),
	})
	pub.Notify(notifications.Notification{
		ID: notifications.NewID(), UserID: mine, Kind: notifications.KindPayoutSettled,
		Severity: notifications.SeverityInfo, Title: "Your payout settled", Body: "b", OccurredAt: time.Now().UTC(),
	})
	pub.Signal(notifications.Signal{UserID: mine.String(), Scope: notifications.ScopeBalance})
	pub.Signal(notifications.Signal{Scope: notifications.ScopeMarket, Ref: "m-1", Broadcast: true})

	_, replay := hub.Subscribe(me, 1, 16)
	var got []stream.Event
	for _, e := range replay {
		if e.Type == stream.TypeResync {
			continue
		}
		got = append(got, e)
	}
	require.Len(t, got, 3, "somebody else's notification must not be in this subscriber's replay")
	assert.Equal(t, stream.TypeNotification, got[0].Type)
	assert.Equal(t, mine.String(), got[0].UserID)
	assert.Contains(t, string(got[0].Data), "Your payout settled")
	assert.Equal(t, stream.TypeDataChanged, got[1].Type)
	assert.Contains(t, string(got[1].Data), notifications.ScopeBalance)
	assert.Equal(t, stream.TypeDataChanged, got[2].Type)
	assert.True(t, got[2].Broadcast, "a market's price is public and travels as a broadcast")
}
