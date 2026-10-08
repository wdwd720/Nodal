//go:build integration

package marketplace

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/settlement"
)

// confirmationRouter is a legal policy that permits simulation only after
// the given step -- a person's confirmation or an operator's review -- and
// denies everything else.
func confirmationRouter(t *testing.T, outcome legalrouter.Outcome) *legalrouter.Router {
	t.Helper()
	r, err := legalrouter.New(legalrouter.Policy{Version: "itest-confirm-" + uuid.NewString(), Rules: []legalrouter.Rule{
		{Match: legalrouter.Key{Product: legalrouter.ProductSimulation}, Outcome: outcome,
			ReasonCode: "ITEST_SIMULATION_NEEDS_A_PERSON", Detail: "a person must look first", ApprovalReference: "ITEST-1"},
		{Match: legalrouter.Key{}, Outcome: legalrouter.Deny, ReasonCode: "ITEST_DENY", Detail: "catch-all"},
	}})
	require.NoError(t, err)
	return r
}

// TestIntegration_AFollowNeverDispatchesARouteThatNeedsAPerson (review 3,
// F5): when the legal router permits a follow's paper intents only once a
// person confirms them (or an operator reviews them), the route is Permitted
// but not MayExecuteNow. A follow has nobody to ask, so the intent is
// refused at the route -- ROUTE_REFUSED, naming the step -- and never reaches
// the risk kernel or the executor.
func TestIntegration_AFollowNeverDispatchesARouteThatNeedsAPerson(t *testing.T) {
	for _, c := range []struct {
		outcome legalrouter.Outcome
		reason  string
	}{
		{legalrouter.RequiresUserConfirmation, "REQUIRES_" + string(settlement.ConfirmUserReview)},
		{legalrouter.RequiresManualReview, "REQUIRES_" + string(settlement.ConfirmManualReview)},
	} {
		t.Run(string(c.outcome), func(t *testing.T) {
			f := newFixture(t)
			_, v := f.approved("momentum")
			in := f.install(v)
			cfg := f.config(f.sandbox)
			cfg.Router = confirmationRouter(t, c.outcome)
			svc, err := NewService(cfg)
			require.NoError(t, err)

			run, err := f.follow(svc, in)
			require.NoError(t, err, "a refused intent is a recorded outcome of the follow, not a failure of it")
			require.Len(t, run.Intents, 1)
			fi := run.Intents[0]
			assert.Equal(t, OutcomeRouteRefused, fi.Outcome)
			assert.Equal(t, []string{c.reason}, fi.ReasonCodes, "the step the follow would have skipped")
			assert.Empty(t, fi.DecisionHash, "the risk kernel was never asked")
			assert.Nil(t, fi.EffectID, "no dispatch was claimed")
			assert.Empty(t, f.exec.snapshot(), "nothing reached the executor")

			intent, err := f.intents.Get(f.ctx, testDB, fi.IntentID)
			require.NoError(t, err)
			assert.Equal(t, econ.StatusRejected, intent.Status)
			assert.Equal(t, "ROUTE_REFUSED", intent.RejectionCode)
			assert.Zero(t, f.count(`SELECT count(*) FROM economic_intent_transitions WHERE intent_id = $1 AND to_status = 'ROUTED'`,
				fi.IntentID), "it was never ROUTED")
			assert.Zero(t, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1`, fi.IntentID))
			assert.Zero(t, f.count(`SELECT count(*) FROM marketplace_paper_fills WHERE install_id = $1`, in.ID), "the paper book did not move")
		})
	}
}
