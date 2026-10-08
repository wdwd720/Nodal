//go:build integration

package predmarket

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

// withRouter replaces the fixture's legal router.
func withRouter(r *legalrouter.Router) fixtureOption { return func(c *Config) { c.Router = r } }

// confirmationRouter is a legal policy that permits simulation only after
// the given step -- a person's confirmation or an operator's review -- and
// denies everything else.
func confirmationRouter(t testing.TB, outcome legalrouter.Outcome) *legalrouter.Router {
	t.Helper()
	r, err := legalrouter.New(legalrouter.Policy{Version: "itest-confirm-" + uuid.NewString(), Rules: []legalrouter.Rule{
		{Match: legalrouter.Key{Product: legalrouter.ProductSimulation}, Outcome: outcome,
			ReasonCode: "ITEST_SIMULATION_NEEDS_A_PERSON", Detail: "a person must look first", ApprovalReference: "ITEST-1"},
		{Match: legalrouter.Key{}, Outcome: legalrouter.Deny, ReasonCode: "ITEST_DENY", Detail: "catch-all"},
	}})
	require.NoError(t, err)
	return r
}

// TestIntegration_ARouteThatNeedsAPersonIsNeverExecuted (review 3, F5): when
// the legal router permits a market's issuance and redemption only once a
// person confirms them (or an operator reviews them), the route is Permitted
// but not MayExecuteNow, and the market executes nothing: the intent is
// REJECTED at the route with the step among its reasons, and no collateral,
// share, reservation or effect moves.
func TestIntegration_ARouteThatNeedsAPersonIsNeverExecuted(t *testing.T) {
	for _, c := range []struct {
		outcome legalrouter.Outcome
		reason  string
	}{
		{legalrouter.RequiresUserConfirmation, "REQUIRES_" + string(settlement.ConfirmUserReview)},
		{legalrouter.RequiresManualReview, "REQUIRES_" + string(settlement.ConfirmManualReview)},
	} {
		t.Run(string(c.outcome), func(t *testing.T) {
			f := newFixture(t, withRouter(confirmationRouter(t, c.outcome)))
			m := f.openMarket("YES", "NO")
			acct := f.customer(100)
			before := f.wallet(acct, f.collateral)

			for name, op := range map[string]func(ctx context.Context, tx pgx.Tx, req PositionRequest) (ActionResult, error){
				"issue": f.svc.Issue, "redeem": f.svc.Redeem,
			} {
				req := f.positionRequest(m, acct, 5)
				var res ActionResult
				err := f.inTxKeepingRefusal(func(ctx context.Context, tx pgx.Tx) error {
					var err error
					res, err = op(ctx, tx, req)
					return err
				})
				requireCode(t, err, errs.CodeForbidden, name)
				ee, ok := errs.As(err)
				require.True(t, ok, name)
				assert.Equal(t, []string{c.reason}, ee.Fields["reasons"], name)
				assert.Equal(t, "route", ee.Fields["refusal_stage"], name)
				assert.Equal(t, econ.StatusRejected, res.Intent.Status, name)
				assert.Equal(t, string(errs.CodeForbidden), res.Intent.RejectionCode, name)
				assert.Zero(t, f.count(`SELECT count(*) FROM economic_intent_transitions WHERE intent_id = $1 AND to_status = 'ROUTED'`,
					res.Intent.ID), "%s was never ROUTED", name)
				assert.Zero(t, f.count(`SELECT count(*) FROM economic_effects WHERE intent_id = $1`, res.Intent.ID), name)
			}
			assert.True(t, before.Equal(f.wallet(acct, f.collateral)), "the collateral did not move")
			for _, o := range m.Outcomes {
				assert.True(t, f.wallet(acct, o.ShareAssetID).IsZero(), "no share was issued")
			}
			assert.Zero(t, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, acct.String()), "nothing was reserved")
			books := f.books(m)
			assert.True(t, books.Escrow.IsZero(), "the escrow is empty")
		})
	}
}
