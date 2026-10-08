//go:build integration

package orders

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_ACrashAfterExecuteIsCompletedBySweep: a submission that
// stopped after its order traded -- the process died before Complete -- is
// finished by CompleteTerminal or by resuming it, never re-executed.
func TestIntegration_ACrashAfterExecuteIsCompletedBySweep(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	accts := f.fundedAccounts(m, 2)
	f.submit(f.limit(m, accts[0], SideSell, "100.00", units(1)))
	var accepted StageOutcome
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accepted, err = f.svc.Accept(ctx, tx, f.limit(m, accts[1], SideBuy, "100.00", units(1)))
		return err
	}))
	for _, stage := range []func(context.Context, pgx.Tx, OrderID) (StageOutcome, error){f.svc.Approve, f.svc.Reserve, f.svc.Execute} {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := stage(ctx, tx, accepted.Order.ID); return err }))
	}
	o := f.orderView(accepted.Order.ID)
	require.Equal(t, StatusFilled, o.Status)
	require.Equal(t, econ.StatusExecuting, f.intentStatus(o), "the crash left the intent executing")
	var swept int
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		swept, err = f.svc.CompleteTerminal(ctx, tx, 100)
		return err
	}))
	assert.GreaterOrEqual(t, swept, 1)
	assert.Equal(t, econ.StatusCompleted, f.intentStatus(o))
	resumed, err := f.pipe.Resume(f.ctx, accepted.Order.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusFilled, resumed.Order.Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM book_commands WHERE order_id = $1`, accepted.Order.ID), "never executed twice")
}

type fixedKills struct{ snap killswitch.Snapshot }

func (k fixedKills) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	return k.snap, nil
}

// TestIntegration_KillSwitchesStopNewOrders: a CAPABILITY_DISABLE switch on
// INTERNAL_MATCHING stops new orders at the capability check, and an
// INSTRUMENT_HALT stops every order on the instrument in the risk kernel; both
// refusals are recorded, and nothing reaches the book.
func TestIntegration_KillSwitchesStopNewOrders(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	x := f.fundedAccounts(m, 1)[0]
	switched := func(sw killswitch.Switch) *Pipeline {
		kills := fixedKills{snap: killswitch.Snapshot{Switches: []killswitch.Switch{sw}}}
		checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, f.clk)
		require.NoError(t, err)
		svc, err := NewService(Config{
			Clock: f.clk, Ledger: f.ledger, Capital: f.capital, Intents: f.intents, Effects: f.effects,
			Authorizer: econ.NewAuthorizer(checker, kills), Registry: f.registry, Risk: f.risk,
			RiskPolicy: risk.DefaultAdvancedPolicy(), Router: f.router, Instruments: f.instr, Assets: f.assets,
			Prices: f.prices, KillSwitches: kills, FeeLedger: f.fees,
		})
		require.NoError(t, err)
		p, err := NewPipeline(svc, testDB)
		require.NoError(t, err)
		return p
	}
	before := f.count(`SELECT count(*) FROM book_commands`)

	disabled := switched(killswitch.Switch{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.InternalMatching), Active: true})
	out, err := disabled.Submit(f.ctx, f.limit(m, x, SideBuy, "99.00", units(1)))
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	assert.Equal(t, StatusRejected, out.Order.Status)
	assert.Equal(t, ReasonCapability, out.Order.RejectReason)
	assert.Equal(t, econ.StatusRejected, out.Intent.Status)

	halted := switched(killswitch.Switch{Kind: killswitch.InstrumentHalt, ScopeID: m.inst.String(), Active: true})
	for _, side := range AllSides() {
		out, err := halted.Submit(f.ctx, f.limit(m, x, side, "99.00", units(1)))
		assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err), "%s", side)
		assert.Contains(t, out.Order.RejectReason, risk.ReasonKillSwitch, "%s", side)
	}
	assert.Equal(t, before, f.count(`SELECT count(*) FROM book_commands`), "nothing reached the book")

	// A cancel is not new risk: it still works while the capability is off.
	resting := f.submit(f.limit(m, x, SideBuy, "98.00", units(1)))
	c, err := disabled.Cancel(f.ctx, CancelRequest{OrderID: resting.Order.ID, AccountID: x, ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, c.Order.Status)
}
