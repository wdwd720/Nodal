package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Purger is the connection a retention pass needs: one Exec, on a role holding
// DELETE. It is deliberately narrower than db.Querier, because the whole point
// of this file is that the pool the application uses cannot satisfy it.
type Purger interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var _ Purger = (*pgxpool.Pool)(nil)

// MinLoginAttemptRetention is the floor a caller may ask for. An attempt is
// consumed within minutes, but the row is the only per-attempt record of a
// login that never completed, and an operator looking at a login-flow anomaly
// is usually doing it the next morning. Anything shorter deletes the evidence
// before anybody could read it.
const MinLoginAttemptRetention = 24 * time.Hour

// PurgeLoginAttempts deletes login attempts whose absolute expiry passed more
// than retention ago, and returns how many rows went.
//
// # Why this exists
//
// login_attempts holds the OIDC state, the nonce and the PKCE code_verifier in
// plaintext, plus the IP and user agent of whoever began the login. The secrets
// are single-use: Complete sets consumed_at under FOR UPDATE, and a consumed
// attempt cannot be replayed. So an expired row has no authentication value
// left in it -- it is exposure and nothing else, and it accumulated forever.
//
// Migration 00641 said the job existed and left it undone: "login_attempts is
// transient and purged by the ops role instead", with GRANT DELETE to cp_ops
// and nothing anywhere that deletes (F-79). The durable record of a login is a
// security_events row of kind 'login', which this never touches, so purging
// here loses no investigative trail.
//
// # Why it needs its own connection
//
// cp_app holds SELECT, INSERT and UPDATE on login_attempts and deliberately not
// DELETE: an attacker holding the application credential must not be able to
// erase the record of the logins they attempted. A purge therefore cannot run
// on the application pool, and the caller supplies one that holds DELETE.
func PurgeLoginAttempts(ctx context.Context, q Purger, now time.Time, retention time.Duration) (int64, error) {
	if retention < MinLoginAttemptRetention {
		return 0, fmt.Errorf("identity: login attempt retention %s is below the %s floor",
			retention, MinLoginAttemptRetention)
	}
	tag, err := q.Exec(ctx, `DELETE FROM login_attempts WHERE expires_at < $1`, now.UTC().Add(-retention))
	if err != nil {
		return 0, fmt.Errorf("identity: purge login attempts: %w", err)
	}
	return tag.RowsAffected(), nil
}
