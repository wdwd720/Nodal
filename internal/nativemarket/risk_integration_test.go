//go:build integration

package nativemarket

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

// The risk kernel, actually applied to a Domain A trade.
//
// Before this, the settlement compiler recorded RequiresRiskEvaluation for
// every native trade and nothing consumed it: the route named a control that
// never ran. These tests are the difference between the two.
//
// Each one tightens an ACCOUNT-scope policy for its own account. Compose takes
// the minimum of GLOBAL, ACCOUNT and AGENT, so an ACCOUNT row can only tighten,
// and tightening one account leaves every other test in this package alone.

// TestIntegration_ATradeThatWouldOwnTooMuchOfAMarketIsRefused: the limit that
// stops one holder owning a market. A holder who owns a market sets its price,
// which is the position PART XXXII exists to prevent.
func TestIntegration_ATradeThatWouldOwnTooMuchOfAMarketIsRefused(t *testing.T) {
	f := newFixture(t)
	// 1% of total supply. A 1,000-Credit order against this curve buys about
	// 2.9%, so this is a limit the next trade breaches.
	tightenNativeLimits(t, f.trader, 100, 10_000, f.clk.Now())

	before := f.balance(f.trader, f.creditAsset, ledger.CodeCreditBalance)
	stBefore, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)

	_, err = f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskConcentration, errs.CodeOf(err))

	assert.Contains(t, err.Error(), "too large a share of this asset",
		"the refusal must say what to do about it, not only which code fired")

	var refusal *RiskRefusal
	require.True(t, errors.As(err, &refusal), "the refusal must carry its decision: %v", err)
	assert.Equal(t, risk.Reject, refusal.Decision.Verdict)
	assert.Contains(t, refusal.Decision.ReasonCodes, risk.ReasonNativeMarketConcentration)
	assert.NotEmpty(t, refusal.Decision.PolicyVersion, "a decision must name the policy it applied")
	assert.NotEmpty(t, refusal.Decision.Hash)

	// Nothing happened. A refusal that costs the user Credits or moves the
	// market is not a refusal.
	assert.Equal(t, before.String(),
		f.balance(f.trader, f.creditAsset, ledger.CodeCreditBalance).String(),
		"a refused trade must not move Credits")
	stAfter, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	assert.Equal(t, stBefore.Version, stAfter.Version, "a refused trade must not move the market")
}

// TestIntegration_ATradeThatWouldConcentrateOneCreatorIsRefused: the limit on
// how much of one account's Credit position may ride on a single creator.
func TestIntegration_ATradeThatWouldConcentrateOneCreatorIsRefused(t *testing.T) {
	f := newFixture(t)
	buyer := newAccount(t)
	f.fund(buyer, 10_000_000_000) // 10,000 Credits
	// 5% of the account's Credit position. A 1,000-Credit order is 10% of it.
	tightenNativeLimits(t, buyer, 10_000, 500, f.clk.Now())

	_, err := f.buy(buyer, 1_000_000_000, money.Quantity{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskConcentration, errs.CodeOf(err))

	assert.Contains(t, err.Error(), "one creator's assets")

	var refusal *RiskRefusal
	require.True(t, errors.As(err, &refusal))
	assert.Contains(t, refusal.Decision.ReasonCodes, risk.ReasonCreatorConcentration)
	assert.NotContains(t, refusal.Decision.ReasonCodes, risk.ReasonNativeMarketConcentration,
		"only the limit that was actually breached should be reported")
}

// TestIntegration_ASellIsNeverRefusedByAConcentrationLimit: the exit stays open.
//
// Both limits constrain holding too much. Refusing somebody's sale because they
// hold too much would trap them in the position the limit exists to discourage,
// and would hand any operator who tightened a limit the power to freeze
// holders. PART XXXII says a kill switch stops NEW risk and must not stop a
// required unwind; a limit is the same.
func TestIntegration_ASellIsNeverRefusedByAConcentrationLimit(t *testing.T) {
	f := newFixture(t)
	buyer := newAccount(t)
	f.fund(buyer, 10_000_000_000)
	bought, err := f.buy(buyer, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	// A limit so tight that everything this account holds breaches it.
	tightenNativeLimits(t, buyer, 1, 1, f.clk.Now())

	// A buy is refused...
	_, err = f.buy(buyer, 1_000_000_000, money.Quantity{})
	require.Error(t, err, "a limit this tight must refuse new risk")
	assert.Equal(t, errs.CodeRiskConcentration, errs.CodeOf(err))

	// ...and the sale of everything they hold is not.
	sold, err := f.sell(buyer, bought.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err, "a concentration limit must never trap a holder in a position")
	assert.True(t, sold.Fill.CreditsOut.IsPositive())
}

// TestIntegration_AnOrderBiggerThanTheBalanceSaysSo: the refusal has to name
// what actually happened.
//
// An order larger than the account's Credits makes the creator-concentration
// denominator smaller than its numerator, so the ratio exceeds every limit and
// the kernel would answer RISK_CONCENTRATION. Sending somebody to read a
// concentration policy when what happened is "you do not have that many
// Credits" is the wrong answer, and it is the kind of wrong answer nobody
// notices, because the trade is correctly refused either way.
//
// So the limits are not evaluated for a trade the account cannot pay for, and
// the ledger's own negative-balance guard produces the refusal.
func TestIntegration_AnOrderBiggerThanTheBalanceSaysSo(t *testing.T) {
	f := newFixture(t)
	poor := newAccount(t)
	f.fund(poor, 1_000_000_000) // 1,000 Credits

	_, err := f.buy(poor, 5_000_000_000, money.Quantity{}) // 5,000
	require.Error(t, err)
	assert.NotEqual(t, errs.CodeRiskConcentration, errs.CodeOf(err),
		"an overspend must not be reported as a concentration breach: %v", err)
	assert.Equal(t, errs.CodeLedgerNegativeBalance, errs.CodeOf(err))
}

// TestIntegration_AnAllowedTradeRecordsItsRiskDecision: the evidence.
//
// A control that runs and leaves no record is one nobody can audit afterwards.
// The decision is written in the trade's own transaction, so a decision cannot
// commit without the trade it permitted.
func TestIntegration_AnAllowedTradeRecordsItsRiskDecision(t *testing.T) {
	f := newFixture(t)
	buyer := newAccount(t)
	f.fund(buyer, 10_000_000_000)
	_, err := f.buy(buyer, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	var (
		decision, policyVersion, marketID, evaluator string
		reasons                                      []string
	)
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT decision, policy_version, evaluator_version, reason_codes,
		        market_snapshot->'native_market'->>'market_id'
		   FROM risk_decisions
		  WHERE account_id = $1
		  ORDER BY evaluated_at DESC LIMIT 1`, buyer).
		Scan(&decision, &policyVersion, &evaluator, &reasons, &marketID))

	assert.Equal(t, string(risk.Allow), decision)
	// Compose names every scope it merged, so a decision says which rows it
	// applied rather than only the tightest one.
	assert.Contains(t, policyVersion, "nm-itest-global")
	assert.Empty(t, reasons)
	assert.Contains(t, evaluator, "native-trade")
	assert.Equal(t, f.market.ID.String(), marketID,
		"the decision must carry the snapshot it was computed from, or it cannot be recomputed")
}

// TestIntegration_ADeploymentWithNoRiskPolicyRefusesEveryNativeTrade: fail
// closed.
//
// A deployment that has never recorded a risk policy has not decided that any
// amount is fine. It is checked here against a real database rather than only
// in a unit test because the failure mode that matters is the wiring: a gate
// that swallowed ErrNoPolicy and returned a zero Policy would evaluate every
// limit as absent and let everything through.
func TestIntegration_ADeploymentWithNoRiskPolicyRefusesEveryNativeTrade(t *testing.T) {
	f := newFixture(t)
	// A store that finds no policy, which is what risk_policies looks like on
	// a deployment where nobody has run scripts/riskpolicy.
	svc := NewService(f.led, f.credits, valuation.NewPriceStore(f.clk), audit.NewWriter(),
		instruments.NewRepository(), NewRiskGate(emptyPolicyStore{}, f.clk), f.clk)

	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, e := svc.Execute(ctx, tx, ExecuteRequest{
			MarketID: f.market.ID, AccountID: f.trader, Side: Buy,
			// A fresh key per run. A fixed one is found by
			// fillByIdempotencyKey on the second run against the same database
			// and replays the earlier result, so this test passed once and then
			// reported "an error is expected but got nil" forever -- which is
			// the fixture failure MASTER_BUILD_STATE warns about, caught by
			// running the suite twice.
			Amount: q(1_000_000_000), IdempotencyKey: "no-policy-" + uuid.NewString(),
			EffectiveAt: f.clk.Now(),
		})
		return e
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, risk.ErrNoPolicy,
		"with no policy the trade must fail closed on the missing policy, not proceed")
	// And it says so at the boundary. An opaque INTERNAL would send somebody
	// to read a stack trace for a deployment step nobody ran.
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "no risk policy")
}

// emptyPolicyStore is risk_policies with nothing in it.
type emptyPolicyStore struct{}

func (emptyPolicyStore) EffectivePolicy(context.Context, db.Querier, string, string, time.Time) (risk.Policy, risk.PolicyRef, error) {
	return risk.Policy{}, risk.PolicyRef{}, risk.ErrNoPolicy
}

func (emptyPolicyStore) RecordDecision(context.Context, pgx.Tx, risk.Input, risk.Decision, string) (risk.DecisionID, error) {
	return risk.DecisionID{}, nil
}
