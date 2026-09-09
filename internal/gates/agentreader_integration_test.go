//go:build integration

package gates

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/security"
)

// Two readers of one gate must not disagree (F-72).
//
// internal/agent may not import internal/gates -- an agent tree can never reach
// the gate controller -- so it reads capability_gates with a SELECT of its own.
// That second reader decided on `state` alone: it selected effective_at and
// expires_at and discarded both, so a gate whose window had closed still read as
// live to the agent worker while Evaluate called the same row inactive.
//
// Nothing calls Admin.ExpireDue, so the persisted state never catches up on its
// own. The divergence therefore lasted for as long as the row sat there, and
// what it permitted was live agent trading past the end of its approval window.
//
// This test lives here rather than in internal/agent because that package may
// not import this one even in a test file, and test/security's boundary check
// counts test files. The direction that is allowed is this one.
//
// It compares the two readers against the same row at the same instant, over
// every condition that can change after activation without a state transition.
// The quorum and evidence conditions Evaluate also applies are deliberately out
// of scope for the agent-side reader and are not compared: Activate enforces
// them before it writes ACTIVE, and 00701 leaves cp_app no UPDATE on this table
// beyond `version`.
func TestIntegration_TheAgentGateReaderAgreesWithTheAuthority(t *testing.T) {
	// STAGING, so this test owns the (capability, environment) pair it moves.
	f := newFixture(t, "STAGING")
	const c = LiveAgentTrading
	require.True(t, IsHighRisk(c))

	// A real proposal, approval and activation by three distinct principals,
	// with a window that is open now and closes in an hour.
	opens := f.clk.Now().Add(-time.Minute)
	closes := f.clk.Now().Add(time.Hour)
	p := highRiskProposal("agent trading pilot")
	p.EffectiveAt, p.ExpiresAt = opens, closes
	_, err := f.do(t, f.op("risk-alice", security.RoleRisk), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, p)
	})
	require.NoError(t, err)
	_, err = f.do(t, f.bg("bg-bob"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "reviewed")
	})
	require.NoError(t, err)
	f.clk.Advance(time.Minute)
	g, err := f.do(t, f.bg("bg-carol"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "second approval")
	})
	require.NoError(t, err)
	require.Equal(t, StateActive, g.State)

	// agree reads both and fails if they differ, whatever the expected answer
	// is. Reporting the disagreement itself is the point: a future change that
	// makes them differ in the other direction must fail here too.
	agree := func(t *testing.T, want bool, wantReason string) agent.LiveTrading {
		t.Helper()
		now := f.clk.Now()
		v := Evaluate(mustLoad(t, c, f.env), true, now)
		read, err := agent.Store{}.LiveTradingEnabled(context.Background(), testDB, f.env, now)
		require.NoError(t, err)
		assert.Equal(t, v.Active, read.Enabled,
			"the authority says active=%t (%s) and the agent reader says enabled=%t (%s)",
			v.Active, v.Reason, read.Enabled, read.Reason)
		assert.Equal(t, want, read.Enabled)
		assert.Equal(t, wantReason, read.Reason)
		return read
	}

	t.Run("open window", func(t *testing.T) {
		agree(t, true, "")
	})

	t.Run("expires_at has passed", func(t *testing.T) {
		// The case the old reader got wrong. The clock moves rather than the
		// row, because that is how it happens in production: nobody edits
		// anything, and the gate simply outlives its approval.
		f.clk.Advance(2 * time.Hour)
		defer f.clk.Advance(-2 * time.Hour)
		assert.Equal(t, "ACTIVE", agree(t, false, "expires_at has passed").State,
			"the persisted state is still ACTIVE, which is exactly why state alone was not enough")
	})

	t.Run("effective_at is in the future", func(t *testing.T) {
		restore := setColumnRestoring(t, g.ID, "effective_at", "now() + interval '1 hour'")
		defer restore()
		agree(t, false, "effective_at is in the future")
	})

	t.Run("effective_at is not set", func(t *testing.T) {
		restore := setColumnRestoring(t, g.ID, "effective_at", "NULL")
		defer restore()
		agree(t, false, "effective_at is not set")
	})

	t.Run("revoked_at is set while the state still says ACTIVE", func(t *testing.T) {
		// Evaluate checks revoked_at separately from state, so it treats this
		// shape as possible; the agent reader now does too.
		restore := setColumnRestoring(t, g.ID, "revoked_at", "now()")
		defer restore()
		agree(t, false, "gate is revoked")
	})

	t.Run("still open once every case has restored", func(t *testing.T) {
		agree(t, true, "")
	})

	t.Run("revoked through the workflow", func(t *testing.T) {
		_, err := f.do(t, f.bg("bg-dora"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
			return f.admin.Revoke(ctx, tx, c, "end of pilot")
		})
		require.NoError(t, err)
		agree(t, false, "gate state is not ACTIVE")
	})
}

// TestIntegration_TheAgentReaderRefusesAnAbsentGate: no row at all must read as
// disabled rather than as an error or a permit. LIVE_MANUAL_TRADING is asked
// for in an environment nothing bootstraps.
func TestIntegration_TheAgentGateReaderFailsClosedWithNoRow(t *testing.T) {
	requireEnv(t)
	read, err := agent.Store{}.LiveTradingEnabled(context.Background(), testDB, "PROD", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, read.Enabled)
	assert.Equal(t, "DISABLED", read.State)
	assert.Equal(t, "no gate row", read.Reason)
}

// mustLoad reads the gate the authority's own way.
func mustLoad(t *testing.T, c Capability, env string) *Gate {
	t.Helper()
	g, err := loadGate(context.Background(), testDB, c, env, false)
	require.NoError(t, err)
	require.NotNil(t, g)
	return g
}

// setColumnRestoring writes one clock column as the owner and returns a
// function putting the previous value back, so a subtest cannot leave the row
// broken for the next one.
func setColumnRestoring(t *testing.T, gid GateID, column, valueSQL string) func() {
	t.Helper()
	ctx := context.Background()
	var was *time.Time
	// #nosec G202 -- column is a literal in this file, never input.
	require.NoError(t, testOwnerDB.QueryRow(ctx,
		`SELECT `+column+` FROM capability_gates WHERE id = $1`, gid).Scan(&was))
	setColumn(t, gid, column, valueSQL)
	return func() {
		// #nosec G202 -- as above.
		_, err := testOwnerDB.Exec(ctx,
			`UPDATE capability_gates SET `+column+` = $2 WHERE id = $1`, gid, was)
		require.NoError(t, err)
	}
}
