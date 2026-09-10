//go:build integration

package identity_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/identity"
)

// opsPool opens the cp_ops connection the purge needs. It is derived from the
// migrate URL rather than configured separately, because the test harness is
// given the app and migrate DSNs and cp_ops differs from cp_migrate only in the
// credential.
func opsPool(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_MIGRATE_DATABASE_URL not set; skipping the retention test")
	}
	url = strings.Replace(url, "cp_migrate:cp_migrate_local", "cp_ops:cp_ops_local", 1)
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 2, MinConns: 0, AppName: "identity-ops-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// A login attempt does not keep its plaintext secrets forever (F-79).
//
// login_attempts holds the OIDC state, the nonce and the PKCE code_verifier in
// plaintext plus the caller's IP and user agent. Migration 00641 granted DELETE
// to cp_ops and said the table "is transient and purged by the ops role
// instead"; nothing anywhere deleted a row, so every login ever begun was still
// there in full.
func TestIntegration_ExpiredLoginAttemptsArePurged(t *testing.T) {
	_, d, _ := newService(t)
	ops := opsPool(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Three attempts: long expired, expired within the retention window, and
	// not expired at all. Only the first may go.
	type row struct {
		state   string
		expires time.Time
		survive bool
	}
	rows := []row{
		{"purge-old-" + newState(t), now.Add(-90 * 24 * time.Hour), false},
		{"purge-recent-" + newState(t), now.Add(-time.Hour), true},
		{"purge-live-" + newState(t), now.Add(time.Hour), true},
	}
	for _, r := range rows {
		_, err := d.Exec(ctx, `INSERT INTO login_attempts (state, nonce, code_verifier, step_up, created_at, expires_at)
			VALUES ($1, $2, $3, false, $4, $5)`, r.state, "nonce-"+r.state, "verifier-"+r.state, now.Add(-2*time.Hour), r.expires)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, r := range rows {
			_, _ = ops.Exec(context.Background(), `DELETE FROM login_attempts WHERE state = $1`, r.state)
		}
	})

	// The privilege that makes the ops role necessary, asserted first: the
	// application role may write a login attempt and never remove one, so an
	// attacker holding that credential cannot erase the record of the logins
	// they tried.
	_, err := d.Exec(ctx, `DELETE FROM login_attempts WHERE state = $1`, rows[0].state)
	require.Error(t, err, "the application role deleted a login attempt")
	assert.Equal(t, "42501", db.SQLState(err), "expected insufficient_privilege, got %v", err)

	n, err := identity.PurgeLoginAttempts(ctx, ops.Pool(), now, 2*24*time.Hour)
	require.NoError(t, err)
	assert.Positive(t, n, "the purge deleted nothing at all")

	for _, r := range rows {
		var count int
		require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE state = $1`, r.state).Scan(&count))
		if r.survive {
			assert.Equal(t, 1, count, "%s was purged and should not have been", r.state)
		} else {
			assert.Zero(t, count, "%s survived the purge", r.state)
		}
	}
}

// TestIntegration_ThePurgeRefusesTooShortARetention: a retention of zero would
// delete every attempt in flight, including the one the customer is completing
// right now. The floor is in the code rather than only in the configuration,
// because a configuration value is one edit away from meaning "all of them".
func TestIntegration_ThePurgeRefusesTooShortARetention(t *testing.T) {
	ops := opsPool(t)
	_, err := identity.PurgeLoginAttempts(context.Background(), ops.Pool(), time.Now().UTC(), time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "below the")
}

// newState returns a state value unique to this test run. UUIDv7 ids minted in
// the same millisecond share their head, so the tail is what varies -- a defect
// this repository has produced twice.
func newState(t *testing.T) string {
	t.Helper()
	return time.Now().UTC().Format("150405.000000000")
}

// Expired sessions are purged by the ops role, under the column grant 00754
// leaves it (F-133, and the sessions half of F-47).
//
// The purge existed -- 00011 called it "an operations job" -- and nothing on
// any tier ran it, so every session ever issued was kept forever. It runs
// from cmd/api's retention loop now, as cp_ops, which after 00754 can read
// exactly the column the DELETE filters by and not the token hash beside it.
func TestIntegration_ExpiredSessionsArePurgedAsOps(t *testing.T) {
	_, d, _ := newService(t)
	ops := opsPool(t)
	ctx := context.Background()

	suffix := id.New[id.Any]().String()
	u, err := accounts.NewRepository().CreateUser(ctx, d, "itest", "purge-"+suffix[len(suffix)-12:], nil)
	require.NoError(t, err)
	insert := func(tag string, expiresAt time.Time) string {
		sid := id.New[id.Any]().String()
		_, err := d.Exec(ctx, `INSERT INTO sessions (id, token_hash, user_id, actor_type, expires_at, auth_time)
			VALUES ($1::uuid, $2, $3, 'USER', $4, now())`, sid, []byte("hash-"+tag+"-"+suffix), u.ID, expiresAt)
		require.NoError(t, err)
		return sid
	}
	old := insert("old", time.Now().Add(-5*24*time.Hour))
	fresh := insert("fresh", time.Now().Add(-time.Hour))

	n, err := pgstore.New().PurgeExpired(ctx, ops, 48*time.Hour)
	require.NoError(t, err, "the purge cannot run as cp_ops; 00754's column grant is wrong")
	assert.GreaterOrEqual(t, n, 1)

	var count int
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1::uuid`, old).Scan(&count))
	assert.Zero(t, count, "a session expired beyond the window survived the purge")
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1::uuid`, fresh).Scan(&count))
	assert.Equal(t, 1, count, "a session inside the window was purged")

	// The housekeeping role reads the column it filters by and nothing else --
	// so the probe references only that column, the way the DELETE does; a
	// WHERE on id would need SELECT on id, which it deliberately lacks.
	var when time.Time
	require.NoError(t, ops.QueryRow(ctx, `SELECT expires_at FROM sessions WHERE expires_at < now() ORDER BY expires_at DESC LIMIT 1`).Scan(&when))
	var hash []byte
	err = ops.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE id = $1::uuid`, fresh).Scan(&hash)
	require.Error(t, err, "cp_ops read a session token hash")
	assert.Contains(t, err.Error(), "permission denied")
}
