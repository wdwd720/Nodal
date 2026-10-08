//go:build integration

package delegation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// poisonRow makes the database refuse every update of one row of table --
// at once, or only at COMMIT -- until the test ends: an item that can never
// be processed.
func poisonRow(t *testing.T, table, rowID string, atCommit bool) {
	t.Helper()
	ctx := context.Background()
	conn := ownerConn(t)
	name := "itest_poison_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err := conn.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '`+rowID+`'::uuid THEN
				RAISE EXCEPTION 'itest poison: % refuses every change', NEW.id;
			END IF;
			RETURN NEW;
		END $$`)
	require.NoError(t, err)
	trigger := `CREATE TRIGGER ` + name + ` BEFORE UPDATE ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	if atCommit {
		trigger = `CREATE CONSTRAINT TRIGGER ` + name + ` AFTER UPDATE ON ` + table +
			` DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	}
	_, err = conn.Exec(ctx, trigger)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, testMigrateURL)
		if err != nil {
			t.Errorf("drop poison: %v", err)
			return
		}
		defer func() { _ = c.Close(ctx) }()
		if _, err := c.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON `+table); err != nil {
			t.Errorf("drop poison trigger: %v", err)
		}
		if _, err := c.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`); err != nil {
			t.Errorf("drop poison function: %v", err)
		}
	})
}

func pendingFailure(rep PendingReport, subject string) (PendingFailure, bool) {
	for _, f := range rep.Failures {
		if f.SubjectID == subject {
			return f, true
		}
	}
	return PendingFailure{}, false
}

func (f *fixture) swapTransitions(sid SwapID) int {
	return f.count(`SELECT count(*) FROM delegated_swap_transitions WHERE swap_id = $1`, sid)
}

// TestIntegration_ASwapOfAnotherNetworkIsNeverObservedOrSettledHere: a swap
// records the devnet network it was admitted on. Another network's
// ReconcilePending never takes it -- that host would read its transaction as
// never seen and could fail it while it stands final on its own chain --
// neither while it is pending nor once its outcome is UNKNOWN; that
// network's ExecuteSwap and SettleSwap refuse it without changing anything;
// and its own network settles it.
func TestIntegration_ASwapOfAnotherNetworkIsNeverObservedOrSettledHere(t *testing.T) {
	here := newFixture(t)
	there := newFixture(t)
	require.NotEqual(t, here.svc.Network(), there.svc.Network())
	c := there.customer(10_000_000)
	capability := there.grant(c, nil)
	pending := there.mustSwap(c, capability.ID, 1, 1_000_000)
	assert.Equal(t, there.svc.Network(), pending.Network, "a swap records the network it was admitted on")
	unknown := there.mustSwap(c, capability.ID, 2, 1_000_000)
	require.NoError(t, there.host.TimeoutButLand(there.ctx, devnethost.ChainSolana))
	_, err := there.svc.ExecuteSwap(there.ctx, unknown.ID)
	requireCode(t, err, errs.CodeSubmissionStateUnknown)
	require.Equal(t, SwapUnknown, there.swap(unknown.ID).Status)
	_, err = there.host.ProduceBlocks(there.ctx, devnethost.ChainSolana, uint64(itWindow+itDepth+2))
	require.NoError(t, err)
	// Here, the pending swap's quote has long expired and the unknown one's
	// blockhash is far below finality: were they read here, both would end.
	here.advance(time.Hour)
	_, err = here.host.ProduceBlocks(here.ctx, devnethost.ChainSolana, uint64(itWindow+itDepth+2))
	require.NoError(t, err)
	pendingBefore, unknownBefore := there.swapTransitions(pending.ID), there.swapTransitions(unknown.ID)

	rep, err := here.svc.ReconcilePending(here.ctx, 500, true)
	require.NoError(t, err)
	assert.Zero(t, rep.Pending+rep.InFlight, "another network's swaps are never taken: %+v", rep)
	_, err = here.svc.ExecuteSwap(here.ctx, unknown.ID)
	requireCode(t, err, errs.CodeConflict)
	err = here.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := here.svc.SettleSwap(ctx, tx, unknown.ID)
		return err
	})
	requireCode(t, err, errs.CodeConflict)
	var got Swap
	require.NoError(t, here.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = here.svc.ExpireStaleSwap(ctx, tx, pending.ID)
		return err
	}))
	assert.Equal(t, SwapAdmitted, got.Status, "another network's pending swap is that network's to end")
	assert.Equal(t, pendingBefore, there.swapTransitions(pending.ID))
	assert.Equal(t, unknownBefore, there.swapTransitions(unknown.ID), "never observed or settled here")
	assert.Equal(t, SwapUnknown, there.swap(unknown.ID).Status)

	own, err := there.svc.ReconcilePending(there.ctx, 500, true)
	require.NoError(t, err)
	assert.Empty(t, own.Failures)
	assert.Equal(t, SwapSettled, there.swap(unknown.ID).Status, "its own network settles it")
}

// TestIntegration_ASubjectThatCannotBeHandledNeverStopsTheSweep: one stale
// swap the database refuses to end plus N good ones. In one ReconcilePending
// pass every good swap is ended and its funds released, and the poison is
// rolled back alone and reported with its backoff; a second pass ends a new
// stale swap without retrying the poison inside its backoff; once the
// backoff has passed it is tried again and reported again.
func TestIntegration_ASubjectThatCannotBeHandledNeverStopsTheSweep(t *testing.T) {
	f := newFixture(t)
	f.rebuild(func(d *Deps) { d.Params.QuoteTTL = 10 * time.Second })
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	var swaps []Swap
	for nonce := uint64(1); nonce <= 4; nonce++ {
		swaps = append(swaps, f.mustSwap(c, capability.ID, nonce, 1_000_000))
	}
	poison := swaps[0]
	poisonRow(t, "delegated_swaps", poison.ID.String(), false)
	f.advance(20 * time.Second)

	first, err := f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	assert.Equal(t, 3, first.Ended, "every good stale swap is ended in the one pass: %+v", first)
	failed, ok := pendingFailure(first, poison.ID.String())
	require.True(t, ok, "the poison is reported: %+v", first.Failures)
	assert.Equal(t, SubjectSwap, failed.Kind)
	assert.Equal(t, UpkeepExpireSwap, failed.Operation)
	assert.Equal(t, 1, failed.Attempts)
	assert.True(t, failed.RetryAfter.Equal(f.clk.Now().UTC().Add(time.Minute)))
	assert.Equal(t, SwapAdmitted, f.swap(poison.ID).Status, "rolled back alone")
	for _, sw := range swaps[1:] {
		got := f.swap(sw.ID)
		assert.Equal(t, SwapRejected, got.Status)
		assert.Equal(t, ReasonQuoteExpired, got.FailureCode)
	}

	fresh := f.mustSwap(c, capability.ID, 5, 1_000_000)
	f.advance(20 * time.Second)
	second, err := f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	assert.Equal(t, 1, second.Ended)
	_, retried := pendingFailure(second, poison.ID.String())
	assert.False(t, retried, "not retried inside its backoff")
	assert.Equal(t, SwapRejected, f.swap(fresh.ID).Status, "a new stale swap is ended behind the poison")

	f.advance(2 * time.Minute)
	third, err := f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	failed, ok = pendingFailure(third, poison.ID.String())
	require.True(t, ok, "tried again once its backoff passed")
	assert.Equal(t, 2, failed.Attempts)
}

// TestIntegration_ADelegatedSubjectWithoutANetworkIsRefused: the database
// refuses a delegated swap or withdrawal born without the devnet network it
// belongs to (UK002), before anything else about the row is checked.
func TestIntegration_ADelegatedSubjectWithoutANetworkIsRefused(t *testing.T) {
	requireEnv(t)
	conn := ownerConn(t)
	ctx := context.Background()
	err := rawTx(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delegated_swaps (id, capability_id, account_id, agent_id, request_nonce, intent_id,
			usage_id, reservation_id, venue_id, mode, action_class, chain_id, wallet, pay_asset_id, receive_asset_id, amount_in,
			amount_out, min_amount_out, notional_usd_minor, requested_at, quote_expires_at, requires_approval, plan, plan_hash, status)
			VALUES ($1,$2,$3,$4,1,$5,$6,$7,$8,'SANDBOX','ACQUIRE_EXPOSURE','sbx-solana','x',$9,$10,1,1,1,1,now(),now() + interval '1 minute',
			        false,'{}'::jsonb,$11,'ADMITTED')`,
			id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](),
			id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), make([]byte, 32))
		return err
	})
	requireSQLState(t, err, "UK002")
	err = rawTx(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawals (id, capability_id, account_id, agent_id, proposal_nonce, intent_id,
			usage_id, reservation_id, policy_id, destination_id, environment, mode, chain_id, wallet, destination_address, asset_id,
			amount, notional_usd_minor, proposed_at, withdrawal_day, approval_expires_at, executable_at, evaluation_outcome,
			risk_score, evaluation, plan, plan_hash, status)
			VALUES ($1,$2,$3,$4,1,$5,$6,$7,$8,$9,'TEST','SANDBOX','sbx-solana','x','y',$10,1,1,now(),(now() AT TIME ZONE 'UTC')::date,
			        now() + interval '1 day',now(),'HUMAN_REQUIRED',0,'{}'::jsonb,'{}'::jsonb,$11,'PROPOSED')`,
			id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](),
			id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), make([]byte, 32))
		return err
	})
	requireSQLState(t, err, "UK002")
}
