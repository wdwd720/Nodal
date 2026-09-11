//go:build integration

package agent

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/security"
)

// A transition row has to describe the change it licenses (F-78).
//
// 00690 binds agents.state to a row in agent_lifecycle_transitions written in
// the same transaction, and its header says why: "without this, a bare
// UPDATE agents SET state = 'LIVE' by the application role would move an agent
// onto real customer capital leaving no evidence of the promotion, its approval
// or its gate evidence".
//
// The binding compares one thing: the flag the transition row sets, against the
// NEW state. It never looks at from_state, and it does not look at `stage` at
// all. Both of the table's promotion CHECKs open with `from_stage = to_stage
// OR ...`, because a pause or a resume keeps the stage and must not have to
// carry promotion evidence.
//
// Together those give the application role a promotion with no approval and no
// evidence, in one transaction: insert a transition row that says the stage did
// not change, then change it.
func TestIntegration_APromotionCannotBeLicensedByARowThatDeniesIt(t *testing.T) {
	f := newFixture(t)
	// CANARY: on the ladder, carrying an envelope, so agents_check2 is already
	// satisfied and nothing but the binding stands between here and LIVE.
	a := f.walkTo(t, StageCanary)
	require.Equal(t, StageCanary, a.Stage)

	err := inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		// The lie: from_stage = to_stage = LIVE. Both promotion CHECKs are
		// satisfied by their first clause, so no approval_id, no ir_hash, no
		// risk policy and no evidence_hash are needed -- and to_state = LIVE
		// is exactly what the state binding demands.
		if err := insertTransition(ctx, tx, Transition{
			ID: NewTransitionID(), AgentID: a.ID,
			FromState: StateCanary, ToState: StateLive,
			FromStage: StageLive, ToStage: StageLive,
			ActorType: security.ActorOperator, ActorID: f.operatorID,
			Reason: "promotion with no approval and no evidence", OccurredAt: f.clk.Now(),
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE agents SET state = 'LIVE', stage = 'LIVE', mode = 'LIVE' WHERE id = $1`, a.ID)
		return err
	})
	require.Error(t, err, "the application role promoted an agent to LIVE with no approval and no evidence")

	// Refused earlier since 00750: a promotion to LIVE must name the envelope
	// and the mode it grants, because those now travel on the transition row
	// rather than being written beside it. This row names neither, so the
	// destination is not a legal agent and the row is refused where it is
	// written -- before the agent is touched at all.
	assert.Contains(t, err.Error(), "agent_lifecycle_transitions_destination_is_legal",
		"expected the forged promotion row to be refused, got %v", err)

	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StageCanary, current.Stage, "the agent moved")
	assert.Equal(t, StateCanary, current.State)
}

// TestIntegration_ABareStageUpdateIsRefused: the stage column carries authority
// of its own. agents_check permits any stage while the state is a side state,
// so a PAUSED agent's stage could be moved with no transition row at all, and
// Resume -- which needs only agent:pause -- would then set the state to
// whatever stage it found.
func TestIntegration_ABareStageUpdateIsRefused(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageCanary)

	// Pause through the real lifecycle, so the state is legitimately PAUSED and
	// agents_check stops refusing a stage that disagrees with it.
	require.NoError(t, inTx(ctxAs(f.operator()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseOperator, Reason: "paused before the stage was moved",
			OpenOrdersPolicy: LeaveOpenOrders,
		})
		return err
	}))

	// The owner, since 00750: cp_app cannot write agents.stage at all, so a
	// probe from it would be refused by privilege and would stop measuring the
	// stage binding this test is named for.
	err := inTxAs(context.Background(), t, testOwnerDB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agents SET stage = 'LIVE', mode = 'LIVE' WHERE id = $1`, a.ID)
		return err
	})
	require.Error(t, err, "a bare stage update must be refused")
	assert.True(t, IsTransitionRequired(err), "expected AU001, got %v", err)

	// And the application cannot reach the column at all, which is the newer
	// and stronger half.
	_, perr := testDB.Exec(context.Background(),
		`UPDATE agents SET stage = 'LIVE', mode = 'LIVE' WHERE id = $1`, a.ID)
	require.Error(t, perr)
	assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(perr), "got %v", perr)

	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StageCanary, current.Stage, "the stage moved with no transition row")
}

// TestIntegration_AStageChangeWithAnHonestRowIsAccepted is the control. A
// binding one condition too strict would refuse every real promotion, and the
// ladder walk elsewhere in this suite would be the only thing to notice.
func TestIntegration_AStageChangeWithAnHonestRowIsAccepted(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)

	require.NoError(t, inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertTransition(ctx, tx, Transition{
			ID: NewTransitionID(), AgentID: a.ID,
			FromState: StateShadow, ToState: StatePaused,
			FromStage: StageShadow, ToStage: StageShadow,
			ActorType: security.ActorOperator, ActorID: f.operatorID,
			Reason: "an honest pause keeps the stage", OccurredAt: f.clk.Now(),
			// The destination, in full. Since 00750 the row IS the change, so a
			// pause that keeps the stage must restate the authority the stage
			// carries -- omitting the mode would clear it and produce an agent
			// SHADOW at stage with no mode, which agents_check3 forbids.
			ToMode: ModeShadow, ToStrategyVersionID: a.StrategyVersionID,
			ToRiskPolicyVersion: a.RiskPolicyVersion,
		}); err != nil {
			return err
		}
		return nil
	}))

	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatePaused, current.State)
	assert.Equal(t, StageShadow, current.Stage)
}
