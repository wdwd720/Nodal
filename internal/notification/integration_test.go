//go:build integration

package notification_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/security"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 8, MinConns: 1, AppName: "notification-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func newUser(t *testing.T, d *db.DB) accounts.UserID {
	t.Helper()
	u, err := accounts.NewRepository().CreateUser(context.Background(), d.Pool(), "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	return u.ID
}

func asUser(uid accounts.UserID) context.Context {
	return security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: uid.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AuthTime: time.Now(),
	})
}

type failingProvider struct{ fail bool }

func (f *failingProvider) Deliver(context.Context, notification.Notification) error {
	if f.fail {
		return errors.New("smtp down")
	}
	return nil
}
func (f *failingProvider) Name() string { return "fake" }

func TestIntegration_Notification_CreateListReadDedup(t *testing.T) {
	d := openDB(t)
	repo := notification.NewRepository()
	uid := newUser(t, d)
	other := newUser(t, d)
	ctx := context.Background()

	var first notification.Notification
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < 3; i++ {
			n, existing, err := repo.Create(ctx, tx, notification.Notification{
				UserID: uid, Kind: notification.KindTradeFilled, Severity: notification.SeverityInfo,
				Title: "Trade filled", Body: "Your order filled.", DedupKey: "fill:" + string(rune('a'+i)),
			})
			if err != nil {
				return err
			}
			require.False(t, existing)
			if i == 0 {
				first = n
			}
		}
		// Replay of the same domain event → same row, no duplicate.
		again, existing, err := repo.Create(ctx, tx, notification.Notification{
			UserID: uid, Kind: notification.KindTradeFilled, Severity: notification.SeverityInfo, Title: "Trade filled", Body: "dup", DedupKey: "fill:a",
		})
		require.NoError(t, err)
		require.True(t, existing)
		assert.Equal(t, first.ID, again.ID)
		assert.Equal(t, "Your order filled.", again.Body)
		return nil
	}))

	page, err := repo.ListForUser(asUser(uid), d.Pool(), uid, "", 2)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.NotEmpty(t, page.NextCursor)
	page2, err := repo.ListForUser(asUser(uid), d.Pool(), uid, page.NextCursor, 2)
	require.NoError(t, err)
	require.Len(t, page2.Items, 1)
	assert.Empty(t, page2.NextCursor)

	// Tenant isolation: another customer cannot list or mark.
	_, err = repo.ListForUser(asUser(other), d.Pool(), uid, "", 10)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(repo.MarkRead(asUser(other), d.Pool(), uid, first.ID, time.Now())))

	unread, err := repo.UnreadCount(ctx, d.Pool(), uid)
	require.NoError(t, err)
	assert.Equal(t, 3, unread)
	require.NoError(t, repo.MarkRead(asUser(uid), d.Pool(), uid, first.ID, time.Now()))
	unread, err = repo.UnreadCount(ctx, d.Pool(), uid)
	require.NoError(t, err)
	assert.Equal(t, 2, unread)

	// Content is immutable; only read/delivery columns may change.
	_, err = d.Pool().Exec(ctx, `UPDATE notifications SET body = 'tampered' WHERE id = $1`, first.ID)
	require.Error(t, err)
	assert.True(t, db.IsImmutableRow(err), "got %v", err)
	_, err = d.Pool().Exec(ctx, `DELETE FROM notifications WHERE id = $1`, first.ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "got %v", err)
}

func TestIntegration_Notification_Dispatcher(t *testing.T) {
	d := openDB(t)
	repo := notification.NewRepository()
	uid := newUser(t, d)
	ctx := context.Background()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < 5; i++ {
			if _, _, err := repo.Create(ctx, tx, notification.Notification{UserID: uid, Kind: notification.KindFundingAvailable, Severity: notification.SeverityInfo, Title: "Funds available", Body: "USDC settled"}); err != nil {
				return err
			}
		}
		return nil
	}))
	prov := &failingProvider{fail: true}
	disp := notification.NewDispatcher(repo, prov)
	n, err := disp.RunOnce(ctx, d, 10, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "failures are recorded, not delivered")
	var failed int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND delivery_error IS NOT NULL`, uid).Scan(&failed))
	assert.Equal(t, 5, failed)

	// New notifications deliver once the provider recovers; failed rows are not retried automatically.
	prov.fail = false
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := repo.Create(ctx, tx, notification.Notification{UserID: uid, Kind: notification.KindAgentPaused, Severity: notification.SeverityWarn, Title: "Agent paused", Body: "Daily loss limit reached"})
		return err
	}))
	n, err = disp.RunOnce(ctx, d, 10, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = disp.RunOnce(ctx, d, 10, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "idempotent: nothing left to deliver")
}

func TestValidate(t *testing.T) {
	uid := accounts.NewUserID()
	ok := notification.Notification{UserID: uid, Kind: notification.KindTradeFilled, Severity: notification.SeverityInfo, Title: "t", Body: "b"}
	require.NoError(t, ok.Validate())
	bad := ok
	bad.Kind = "NOPE"
	assert.Error(t, bad.Validate())
	bad = ok
	bad.Data = []byte("{not json")
	assert.Error(t, bad.Validate())
	bad = ok
	bad.UserID = accounts.UserID{}
	assert.Error(t, bad.Validate())
}
