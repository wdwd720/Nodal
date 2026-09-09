package agent

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/intent"
)

type stubIntents struct{}

func (stubIntents) Create(context.Context, pgx.Tx, intent.TradeIntent) (intent.TradeIntent, error) {
	return intent.TradeIntent{}, nil
}

// The emitter refuses every missing dependency, including the one that used to
// be optional (F-75).
//
// NewBroker refuses six nil dependencies. NewEmitter refused four and let the
// envelope reader be nil, and Emit guarded the whole envelope block on
// `deps.Envelope != nil` -- so a caller that forgot it lost the instrument
// allow-list and the single-trade cap, silently, at any stage, while the
// agent's own frozen authority still carried an envelope id.
//
// The path is inert today: the only production EmitterFor refuses outright,
// because no worker emits intents yet. That is why this is a constructor test
// rather than a behavioural one -- and it is worth having precisely because the
// wiring has not happened. A fail-open default is at its most dangerous in the
// window between "nothing calls it" and "something does".
func TestNewEmitterRefusesEveryMissingDependency(t *testing.T) {
	t.Parallel()
	auth, err := NewAuthority(AuthorityInput{
		AgentID: NewAgentID(), AgentVersion: 1, AccountID: "acct", StrategyVersionID: "sv",
		Stage: StageShadow, Mode: ModeShadow, IR: testIR(),
	})
	require.NoError(t, err)

	full := func() EmitterDeps {
		return EmitterDeps{
			Clock: clock.NewFake(nowForTest()), Intents: stubIntents{},
			Pauses: NewPauseChecker(), Budgets: NewBudgetReader(), Envelope: NewEnvelopeReader(),
		}
	}
	// The control first: the complete set is accepted, or every refusal below
	// could be passing for some other reason.
	e, err := NewEmitter(full(), auth)
	require.NoError(t, err)
	require.NotNil(t, e)

	for name, blank := range map[string]func(*EmitterDeps){
		"clock":           func(d *EmitterDeps) { d.Clock = nil },
		"intent writer":   func(d *EmitterDeps) { d.Intents = nil },
		"pause checker":   func(d *EmitterDeps) { d.Pauses = nil },
		"budget reader":   func(d *EmitterDeps) { d.Budgets = nil },
		"envelope reader": func(d *EmitterDeps) { d.Envelope = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := full()
			blank(&deps)
			out, err := NewEmitter(deps, auth)
			require.Errorf(t, err, "a nil %s was accepted", name)
			assert.Nil(t, out)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			assert.Contains(t, err.Error(), name)
		})
	}
}
