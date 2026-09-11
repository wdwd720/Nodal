//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/provider/verifysandbox"
	"github.com/nodal/controlplane/internal/verification"
)

// The expiry sweeps, in the process that runs them (F-170).
//
// Four ExpireDue passes existed, were tested by their own packages, were
// documented as "meant for a periodic worker", and were called by nothing in
// any binary. Each package's tests prove what its pass does; this proves the
// thing none of them could -- that something calls them, against the real
// schema, on the deployment that has no worker tier.
//
// The verification half is asserted end to end because it is the one with a
// person waiting: 00762 permits one open session per person, so a hosted link
// nothing closes is that person's verification blocked for good.

// noopEmitter satisfies capital's outbox dependency for a pass that finds
// nothing to expire and therefore emits nothing.
type noopEmitter struct{}

func (noopEmitter) Emit(context.Context, pgx.Tx, string, any) error { return nil }

func TestIntegration_TheExpirySweepsRunInThisProcess(t *testing.T) {
	database := openSweepDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	clk := clock.NewFake(now)

	auditWriter := audit.NewWriter()
	gateAdmin, err := gates.NewAdmin("LOCAL", clk, httpapi.NewAuditAppender(auditWriter).GateAudit())
	require.NoError(t, err)
	registry := verification.NewRegistry(true)
	sandboxProvider, err := verifysandbox.New(config.EnvLocal, clk.Now)
	require.NoError(t, err)
	require.NoError(t, registry.Register(sandboxProvider))
	verificationSvc, err := verification.NewService(verification.Deps{
		Repo:        verification.NewRepository(),
		Compliance:  compliance.NewRepository(auditWriter),
		Providers:   registry,
		Clock:       clk,
		Environment: "LOCAL",
	})
	require.NoError(t, err)

	// A person whose hosted link ran out an hour ago, with the session left
	// open the way every deployment left it until now.
	user, err := accounts.NewRepository().CreateUser(ctx, database, "sweep-itest", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	sessionID := verification.NewSessionID()
	_, err = database.Exec(ctx, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, rules_version, environment, sandbox, provider_ref, expires_at)
		VALUES ($1,$2,'PAYOUT_KYC','sweep-itest','CREATED','v1','LOCAL',false,'prov_ref',$3)`,
		sessionID, user.ID, now.Add(-time.Hour))
	require.NoError(t, err)
	_, err = database.Exec(ctx, `INSERT INTO verification_session_transitions
		(id, session_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,'CREATED','PENDING_USER_ACTION','SYSTEM','sweep-itest','a link was issued',now())`,
		verification.NewTransitionID(), sessionID)
	require.NoError(t, err)

	deps := expiryDeps{
		Gates:        gateAdmin,
		Admin:        admin.NewService(clk, auditWriter),
		Capital:      capital.NewService(clk, noopEmitter{}),
		Verification: verificationSvc,
	}

	// One pass: every one of the four runs against the real schema, and the
	// one with a due row moves it.
	expireDueOnce(ctx, database, deps, clk, quietLogger())

	var status string
	require.NoError(t, database.QueryRow(ctx,
		`SELECT status FROM verification_sessions WHERE id = $1`, sessionID).Scan(&status))
	assert.Equal(t, "EXPIRED", status,
		"the sweep did not close a link that ran out, so its owner cannot verify at all")

	// And the point of closing it.
	_, open, err := verification.NewRepository().OpenSession(ctx, database, user.ID)
	require.NoError(t, err)
	assert.False(t, open, "the one-open-session index still holds this person's verification shut")

	// A second pass is a no-op: the sweeps converge rather than churn.
	expireDueOnce(ctx, database, deps, clk, quietLogger())
	require.NoError(t, database.QueryRow(ctx,
		`SELECT status FROM verification_sessions WHERE id = $1`, sessionID).Scan(&status))
	assert.Equal(t, "EXPIRED", status)

	// A binary that constructs none of these sweeps none of them, rather than
	// panicking on a nil service.
	assert.NotPanics(t, func() { expireDueOnce(ctx, database, expiryDeps{}, clk, quietLogger()) })
}

func openSweepDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; provision one with `go run ./scripts/testdb -name api`")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 4, MinConns: 1, AppName: "expiry-sweeps-itest"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}
