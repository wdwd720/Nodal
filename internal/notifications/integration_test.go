//go:build integration

package notifications_test

import (
	"context"
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
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/security"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 8, MinConns: 1, AppName: "notifications-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func newUser(t *testing.T, d *db.DB) accounts.UserID {
	t.Helper()
	u, err := accounts.NewRepository().CreateUser(context.Background(), d.Pool(),
		"https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	return u.ID
}

func newAccount(t *testing.T, d *db.DB, owner accounts.UserID) accounts.AccountID {
	t.Helper()
	a, err := accounts.NewRepository().CreateAccount(context.Background(), d.Pool(), owner, accounts.KindCustomer)
	require.NoError(t, err)
	return a.ID
}

func asUser(uid accounts.UserID) context.Context {
	return security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: uid.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AuthTime: time.Now(),
	})
}

func producer() *notifications.Producer {
	return notifications.NewProducer(func() time.Time { return time.Now().UTC() }, false)
}

// emit runs one Emit in its own committed transaction.
func emit(t *testing.T, d *db.DB, p *notifications.Producer, n notifications.Notification) notifications.Emitted {
	t.Helper()
	var out notifications.Emitted
	require.NoError(t, d.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = p.Emit(ctx, tx, n)
		return err
	}))
	return out
}

func countFor(t *testing.T, d *db.DB, uid accounts.UserID) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1`, uid).Scan(&n))
	return n
}

// TestIntegration_ANotificationExistsOnlyIfItsTransactionCommitted is the whole
// design in one test. Emit takes the caller's transaction, so a rolled-back
// state change cannot leave a person told about something that did not happen.
func TestIntegration_ANotificationExistsOnlyIfItsTransactionCommitted(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	p := producer()

	rollback := errs.New(errs.CodeConflict, "the domain change failed after the notification was written")
	err := d.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		em, eerr := p.Emit(ctx, tx, notifications.Notification{
			UserID: uid, Kind: notifications.KindSystem, Title: "t", Body: "b", Occurrence: "roll-1",
		})
		require.NoError(t, eerr)
		require.True(t, em.Created)
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	assert.Zero(t, countFor(t, d, uid), "a rolled-back transaction must leave no notification behind")

	em := emit(t, d, p, notifications.Notification{
		UserID: uid, Kind: notifications.KindSystem, Title: "t", Body: "b", Occurrence: "roll-1",
	})
	assert.True(t, em.Created)
	assert.Equal(t, 1, countFor(t, d, uid))
}

// TestIntegration_EmitIsIdempotentOnKindRefAndOccurrence.
func TestIntegration_EmitIsIdempotentOnKindRefAndOccurrence(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	other := newUser(t, d)
	p := producer()

	n := notifications.Notification{
		UserID: uid, Kind: notifications.KindNativeTradeFilled,
		Title: "You bought NODL", Body: "Your buy order filled.",
		Ref: notifications.Ref{Type: "native_market_fill", ID: "fill-1"}, Occurrence: "fill-1",
	}
	first := emit(t, d, p, n)
	require.True(t, first.Created)

	// The same fact again -- a replayed webhook, a follower re-reading its own
	// window -- is the same row and no second notification.
	n.Body = "a different body, which must not create a second row"
	again := emit(t, d, p, n)
	assert.False(t, again.Created)
	assert.Equal(t, first.Notification.ID, again.Notification.ID)
	assert.Equal(t, "Your buy order filled.", again.Notification.Body, "the first telling stands")
	assert.Equal(t, 1, countFor(t, d, uid))

	// A different occurrence of the same ref IS a different notification.
	n.Occurrence = "fill-2"
	third := emit(t, d, p, n)
	assert.True(t, third.Created)
	assert.Equal(t, 2, countFor(t, d, uid))

	// And the dedup key is scoped per person: the same fact for somebody else
	// is their own row.
	n.UserID = other
	n.Occurrence = "fill-1"
	fourth := emit(t, d, p, n)
	assert.True(t, fourth.Created)
	assert.Equal(t, 1, countFor(t, d, other))
}

// TestIntegration_ASwitchedOffKindIsNotWrittenAndAFixedOneIgnoresTheSwitch.
func TestIntegration_ASwitchedOffKindIsNotWrittenAndAFixedOneIgnoresTheSwitch(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	ctx := asUser(uid)
	p := producer()

	require.NoError(t, notifications.SavePreferences(ctx, d, uid, map[notifications.Kind]bool{
		notifications.KindNativeTradeFilled:  false,
		notifications.KindSecurityNewSession: false, // stored, and ignored
	}, time.Now()))

	off := emit(t, d, p, notifications.Notification{
		UserID: uid, Kind: notifications.KindNativeTradeFilled, Title: "t", Body: "b", Occurrence: "f-1",
	})
	assert.True(t, off.Suppressed)
	assert.False(t, off.Created)

	fixed := emit(t, d, p, notifications.Notification{
		UserID: uid, Kind: notifications.KindSecurityNewSession, Title: "t", Body: "b", Occurrence: "s-1",
	})
	assert.False(t, fixed.Suppressed, "a person cannot switch off a new-sign-in notice and still be treated as informed")
	assert.True(t, fixed.Created)
	assert.Equal(t, 1, countFor(t, d, uid))

	prefs, err := notifications.LoadPreferences(ctx, d, uid)
	require.NoError(t, err)
	byKind := map[notifications.Kind]notifications.Preference{}
	for _, pref := range prefs {
		byKind[pref.Kind] = pref
		assert.Equal(t, notifications.ChannelInApp, pref.Channel)
	}
	assert.Len(t, prefs, len(notifications.ProductKinds()), "every kind is answered, stored or not")
	assert.False(t, byKind[notifications.KindNativeTradeFilled].Enabled)
	assert.True(t, byKind[notifications.KindNativeTradeFilled].Enforced)
	assert.True(t, byKind[notifications.KindSecurityNewSession].Enabled,
		"a stored answer that cannot be enforced is reported as enabled, not as the lie it would otherwise be")
	assert.False(t, byKind[notifications.KindSecurityNewSession].Enforced)
	assert.True(t, byKind[notifications.KindPayoutSettled].Enabled, "a kind with no stored answer is on")
}

// TestIntegration_TheCentreIsTheCallersOwnAndNobodyElses.
func TestIntegration_TheCentreIsTheCallersOwnAndNobodyElses(t *testing.T) {
	d := openDB(t)
	mine := newUser(t, d)
	theirs := newUser(t, d)
	acct := newAccount(t, d, mine)
	p := producer()

	for i := 0; i < 3; i++ {
		emit(t, d, p, notifications.Notification{
			UserID: mine, AccountID: &acct, Kind: notifications.KindPayoutSettled,
			Title: "Your payout settled", Body: "The provider reported it settled.",
			Ref: notifications.Ref{Type: "payout_request", ID: "p-1"}, Occurrence: string(rune('a' + i)),
		})
	}
	emit(t, d, p, notifications.Notification{
		UserID: theirs, Kind: notifications.KindPayoutSettled, Title: "t", Body: "b", Occurrence: "x",
	})

	ctx := asUser(mine)
	page, err := notifications.List(ctx, d, mine, notifications.Filter{}, "", 2)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.NotEmpty(t, page.NextCursor)
	rest, err := notifications.List(ctx, d, mine, notifications.Filter{}, page.NextCursor, 2)
	require.NoError(t, err)
	require.Len(t, rest.Items, 1)
	assert.Empty(t, rest.NextCursor)

	seen := map[string]bool{}
	for _, n := range append(page.Items, rest.Items...) {
		assert.Equal(t, mine, n.UserID)
		assert.False(t, seen[n.ID.String()], "the cursor returned a row twice")
		seen[n.ID.String()] = true
	}

	// Asking for somebody else's is refused however the id got here.
	_, err = notifications.List(ctx, d, theirs, notifications.Filter{}, "", 10)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = notifications.UnreadCount(ctx, d, theirs)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Unread, read, unread-count, read-all.
	n, err := notifications.UnreadCount(ctx, d, mine)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	first := page.Items[0]
	marked, err := notifications.MarkRead(ctx, d, mine, first.ID, time.Now())
	require.NoError(t, err)
	require.NotNil(t, marked.ReadAt)
	at := *marked.ReadAt
	again, err := notifications.MarkRead(ctx, d, mine, first.ID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NotNil(t, again.ReadAt)
	assert.True(t, at.Equal(*again.ReadAt), "when did you see this has one answer")

	unreadOnly, err := notifications.List(ctx, d, mine, notifications.Filter{UnreadOnly: true}, "", 10)
	require.NoError(t, err)
	assert.Len(t, unreadOnly.Items, 2)

	byKind, err := notifications.List(ctx, d, mine, notifications.Filter{Kinds: []notifications.Kind{notifications.KindSystem}}, "", 10)
	require.NoError(t, err)
	assert.Empty(t, byKind.Items)

	updated, err := notifications.MarkAllRead(ctx, d, mine, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 2, updated)
	n, err = notifications.UnreadCount(ctx, d, mine)
	require.NoError(t, err)
	assert.Zero(t, n)

	// Somebody else's row is not markable even by id.
	_, err = notifications.MarkRead(asUser(theirs), d, theirs, first.ID, time.Now())
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestIntegration_SinceIsOldestFirstAndReportsTruncation(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	p := producer()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 4; i++ {
		emit(t, d, p, notifications.Notification{
			UserID: uid, Kind: notifications.KindSystem, Title: "t", Body: "b",
			Occurrence: string(rune('a' + i)), OccurredAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	ctx := asUser(uid)
	got, truncated, err := notifications.Since(ctx, d, uid, base, 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, got, 3, "strictly after the instant given")
	for i := 1; i < len(got); i++ {
		assert.False(t, got[i].OccurredAt.Before(got[i-1].OccurredAt), "oldest first")
	}

	got, truncated, err = notifications.Since(ctx, d, uid, base, 2)
	require.NoError(t, err)
	assert.True(t, truncated, "more than the caller asked for is reported, not silently dropped")
	assert.Len(t, got, 2)
}
