//go:build integration

package pgstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 8, MinConns: 1, AppName: "pgstore-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func newUserWithAccounts(t *testing.T, d *db.DB, n int) (accounts.UserID, []accounts.AccountID) {
	t.Helper()
	ctx := context.Background()
	repo := accounts.NewRepository()
	var uid accounts.UserID
	var ids []accounts.AccountID
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		u, err := repo.CreateUser(ctx, tx, "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
		if err != nil {
			return err
		}
		uid = u.ID
		for i := 0; i < n; i++ {
			a, err := repo.CreateAccount(ctx, tx, u.ID, accounts.KindCustomer)
			if err != nil {
				return err
			}
			ids = append(ids, a.ID)
		}
		return nil
	}))
	return uid, ids
}

func newSession(uid accounts.UserID, now time.Time) (auth.Session, string) {
	tok, _ := auth.NewToken()
	return auth.Session{
		ID:         id.New[id.Any]().String(),
		TokenHash:  auth.HashToken(tok),
		SubjectID:  uid.String(),
		ActorType:  security.ActorUser,
		Roles:      []security.Role{security.RoleCustomer},
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(24 * time.Hour),
		AuthTime:   now,
		AMR:        []string{"pwd", "mfa"},
		IP:         "203.0.113.7",
		UserAgent:  "test-agent",
	}, tok
}

func TestIntegration_PgStore_Lifecycle(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	st := pgstore.New()
	uid, accts := newUserWithAccounts(t, d, 2)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s, tok := newSession(uid, now)

	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return st.Create(ctx, tx, s)
	}))

	got, err := st.Get(ctx, d.Pool(), auth.HashToken(tok))
	require.NoError(t, err)
	assert.Equal(t, s.ID, got.ID)
	assert.Equal(t, uid.String(), got.SubjectID)
	assert.Equal(t, security.ActorUser, got.ActorType)
	assert.Equal(t, []security.Role{security.RoleCustomer}, got.Roles)
	assert.Equal(t, []string{"pwd", "mfa"}, got.AMR)
	assert.Equal(t, "203.0.113.7", got.IP)
	assert.Nil(t, got.RevokedAt)
	assert.ElementsMatch(t, []string{accts[0].String(), accts[1].String()}, got.AccountIDs, "account ids derive from ownership")
	p, err := got.Principal()
	require.NoError(t, err)
	assert.True(t, p.HasRole(security.RoleCustomer))

	// Duplicate token hash / id → ErrSessionExists.
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error { return st.Create(ctx, tx, s) })
	assert.ErrorIs(t, err, auth.ErrSessionExists)

	// Touch advances last_seen_at but never expires_at.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, st.Touch(ctx, d.Pool(), s.ID))
	got2, err := st.Get(ctx, d.Pool(), auth.HashToken(tok))
	require.NoError(t, err)
	assert.True(t, got2.LastSeenAt.After(got.LastSeenAt))
	assert.True(t, got2.ExpiresAt.Equal(got.ExpiresAt))

	// Revoke is idempotent and Get still returns the row (Manager decides validity).
	require.NoError(t, st.Revoke(ctx, d.Pool(), s.ID))
	require.NoError(t, st.Revoke(ctx, d.Pool(), s.ID))
	got3, err := st.Get(ctx, d.Pool(), auth.HashToken(tok))
	require.NoError(t, err)
	require.NotNil(t, got3.RevokedAt)
	assert.ErrorIs(t, st.Touch(ctx, d.Pool(), s.ID), auth.ErrSessionNotFound, "touching a revoked session is not-found")

	// Unknown hash / id.
	_, err = st.Get(ctx, d.Pool(), auth.HashToken("nope"))
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)
	assert.ErrorIs(t, st.Revoke(ctx, d.Pool(), id.New[id.Any]().String()), auth.ErrSessionNotFound)
}

func TestIntegration_PgStore_RevokeAllAndList(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	st := pgstore.New()
	uid, _ := newUserWithAccounts(t, d, 1)
	now := time.Now().UTC()
	var sessions []auth.Session
	for i := 0; i < 3; i++ {
		s, _ := newSession(uid, now.Add(time.Duration(i)*time.Second))
		sessions = append(sessions, s)
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error { return st.Create(ctx, tx, s) }))
	}
	list, err := st.ListForSubject(ctx, d.Pool(), uid.String())
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, sessions[2].ID, list[0].ID, "newest first")

	n, err := st.RevokeAllForSubject(ctx, d.Pool(), uid.String())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	n, err = st.RevokeAllForSubject(ctx, d.Pool(), uid.String())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "second revoke-all finds nothing active")
	list, err = st.ListForSubject(ctx, d.Pool(), uid.String())
	require.NoError(t, err)
	for _, s := range list {
		assert.NotNil(t, s.RevokedAt)
	}
}

func TestIntegration_PgStore_RejectsAgentAndBadSubject(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	st := pgstore.New()
	uid, accts := newUserWithAccounts(t, d, 1)
	now := time.Now().UTC()

	agent, _ := newSession(uid, now)
	agent.ActorType = security.ActorAgent
	agent.Roles = nil
	agent.AccountIDs = []string{accts[0].String()}
	err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error { return st.Create(ctx, tx, agent) })
	require.Error(t, err, "agent sessions are never persisted")

	bad, _ := newSession(uid, now)
	bad.SubjectID = "not-a-uuid"
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error { return st.Create(ctx, tx, bad) })
	require.Error(t, err, "subject must be a users.id uuid")

	// cp_app cannot DELETE sessions (cleanup is an ops-role job).
	_, err = d.Pool().Exec(ctx, "DELETE FROM sessions WHERE user_id = $1::uuid", uid.String())
	require.Error(t, err)
}

// TestIntegration_PgStore_Elevate is the storage half of the break-glass
// grant path: the executed grant has to land on the grantee's session row and
// nowhere else, because that row is what the next request reads back as
// authority.
//
// The WHERE clause is the control, so the test states each of its terms as a
// separate negative and pairs them with the one session that must be
// elevated. Without that pairing an Elevate that touched nothing would pass
// every negative.
func TestIntegration_PgStore_Elevate(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	st := pgstore.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(30 * time.Minute)

	grantee, _ := newUserWithAccounts(t, d, 0)
	bystander, _ := newUserWithAccounts(t, d, 0)

	// operator returns a session of the given user with the OPERATOR actor
	// type, which is the only type the sessions_break_glass_operator_only
	// CHECK admits.
	operator := func(uid accounts.UserID) (auth.Session, string) {
		s, tok := newSession(uid, now)
		s.ActorType = security.ActorOperator
		s.Roles = []security.Role{security.RoleAdmin}
		return s, tok
	}
	create := func(s auth.Session) {
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			return st.Create(ctx, tx, s)
		}))
	}

	live, liveTok := operator(grantee)
	create(live)

	second, secondTok := operator(grantee) // the same human on another device
	create(second)

	revoked, revokedTok := operator(grantee)
	create(revoked)
	require.NoError(t, st.Revoke(ctx, d.Pool(), revoked.ID))

	stale, staleTok := operator(grantee) // past its absolute expiry
	stale.ExpiresAt = now.Add(-time.Minute)
	stale.CreatedAt = now.Add(-2 * time.Hour)
	stale.LastSeenAt = now.Add(-2 * time.Hour)
	create(stale)

	customer, customerTok := newSession(grantee, now) // a USER session of the same subject
	create(customer)

	other, otherTok := operator(bystander)
	create(other)

	n, err := st.Elevate(ctx, d.Pool(), grantee.String(), until, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "exactly the grantee's two live operator sessions")

	elevated := func(tok string) auth.Session {
		s, err := st.Get(ctx, d.Pool(), auth.HashToken(tok))
		require.NoError(t, err)
		return s
	}
	for name, tok := range map[string]string{"first device": liveTok, "second device": secondTok} {
		s := elevated(tok)
		require.NotNil(t, s.BreakGlassUntil, "%s must carry the elevation", name)
		assert.True(t, until.Equal(*s.BreakGlassUntil), "%s: %s", name, s.BreakGlassUntil)
		assert.Contains(t, s.Roles, security.RoleBreakGlass, name)
		assert.Contains(t, s.Roles, security.RoleAdmin, "%s keeps its standing roles", name)
		p, perr := s.Principal()
		require.NoError(t, perr, name)
		assert.True(t, p.Has(security.PermKillRelease, now), "%s holds the approve side while live", name)
		assert.False(t, p.Has(security.PermKillRelease, until), "%s: expiry is exclusive", name)
	}
	for name, tok := range map[string]string{
		"a revoked session":                  revokedTok,
		"an expired session":                 staleTok,
		"a USER session of the same subject": customerTok,
		"another user's session":             otherTok,
	} {
		s := elevated(tok)
		assert.Nil(t, s.BreakGlassUntil, "%s must not be elevated", name)
		assert.NotContains(t, s.Roles, security.RoleBreakGlass, name)
	}

	// A second grant replaces the deadline rather than accumulating roles.
	later := now.Add(time.Hour)
	n, err = st.Elevate(ctx, d.Pool(), grantee.String(), later, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	again := elevated(liveTok)
	require.NotNil(t, again.BreakGlassUntil)
	assert.True(t, later.Equal(*again.BreakGlassUntil))
	count := 0
	for _, r := range again.Roles {
		if r == security.RoleBreakGlass {
			count++
		}
	}
	assert.Equal(t, 1, count, "the role is added at most once")

	// Nobody is elevated when the subject has no live session at all.
	n, err = st.Elevate(ctx, d.Pool(), id.New[id.Any]().String(), until, now)
	require.NoError(t, err)
	assert.Zero(t, n)
}
