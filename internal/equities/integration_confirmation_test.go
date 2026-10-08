//go:build integration

package equities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
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

// TestIntegration_AnOrderWhoseRouteNeedsAPersonIsNeverExecuted (review 3,
// F5): when the legal router permits equity orders only once a person
// confirms them (or an operator reviews them), the route is Permitted but not
// MayExecuteNow. Nothing between SubmitOrderIntent and the venue asks anyone,
// so the intent is REJECTED (ROUTE_REFUSED, naming the step) and never
// ROUTED: it cannot be accepted, and nothing is reserved, ordered or placed.
func TestIntegration_AnOrderWhoseRouteNeedsAPersonIsNeverExecuted(t *testing.T) {
	for _, c := range []struct {
		outcome legalrouter.Outcome
		reason  string
	}{
		{legalrouter.RequiresUserConfirmation, "REQUIRES_" + string(settlement.ConfirmUserReview)},
		{legalrouter.RequiresManualReview, "REQUIRES_" + string(settlement.ConfirmManualReview)},
	} {
		t.Run(string(c.outcome), func(t *testing.T) {
			router := confirmationRouter(t, c.outcome)
			w := newWorld(t, func(cfg *Config) { cfg.Router = router })
			l := w.list(1, 1)
			acct := w.account()
			w.fund(acct, w.cash.ID, 1_000_000_000)
			w.mark(l, 100_00)

			in := w.submit(w.request(acct, l, market(SideBuy, 2_000_000)))
			assert.Equal(t, econ.StatusRejected, in.Status)
			assert.Equal(t, "ROUTE_REFUSED", in.RejectionCode)
			var reason string
			require.NoError(t, testDB.QueryRow(w.ctx, `SELECT reason FROM economic_intent_transitions WHERE intent_id = $1 AND to_status = 'REJECTED'`,
				in.ID).Scan(&reason))
			assert.Contains(t, reason, c.reason, "the refusal names the step that was not taken")
			assert.Zero(t, w.count(`SELECT count(*) FROM economic_intent_transitions WHERE intent_id = $1 AND to_status = 'ROUTED'`, in.ID))

			err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.svc.AcceptOrder(ctx, tx, in.ID)
				return err
			})
			require.Error(t, err)
			assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), "a refused intent is never accepted: %v", err)
			assert.Zero(t, w.count(`SELECT count(*) FROM equity_orders WHERE intent_id = $1`, in.ID), "no order")
			assert.Zero(t, w.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, acct.String()), "nothing reserved")
		})
	}
}
