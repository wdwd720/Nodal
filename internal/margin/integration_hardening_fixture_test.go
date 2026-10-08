//go:build integration

package margin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

// switchExposure is a derivatives book an operator can close: while on, every
// account carries one small risk-increasing derivative exposure.
type switchExposure struct{ on atomic.Bool }

func (e *switchExposure) DerivativeExposures(context.Context, db.Querier, accounts.AccountID) ([]DerivativeExposure, error) {
	if !e.on.Load() {
		return nil, nil
	}
	return []DerivativeExposure{{InstrumentID: "itest-perp", GrossNotional: money.USDFromMinor(1_000),
		InitialMargin: money.USDFromMinor(100), MaintenanceMargin: money.USDFromMinor(50), RiskIncreasingDirection: true}}, nil
}

// rebuild re-wires the fixture's service after a change to its policy or
// dependencies. A changed policy is a new policy version.
func (f *fixture) rebuild(change func(cfg *Config, deps *Dependencies)) {
	f.t.Helper()
	change(&f.cfg, &f.deps)
	f.cfg.Policy.Version = "margin-itest-" + uuid.NewString()
	require.NoError(f.t, f.cfg.Policy.Validate())
	svc, err := NewService(f.cfg, f.deps)
	require.NoError(f.t, err)
	f.svc = svc
}

// markAsset records an observation of an asset's price at the current
// instant (mark advances the clock and marks SOL).
func (f *fixture) markAsset(asset assets.AssetID, price string) {
	f.t.Helper()
	p, err := money.PriceFromDecimalString(price, f.usd.String(), "itest", f.clk.Now())
	require.NoError(f.t, err)
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: asset, QuoteAssetID: f.usd,
		Mantissa: p.Mantissa, Scale: p.Scale, Source: "itest", ObservedAt: f.clk.Now()})
	require.NoError(f.t, err)
}

// withdraw sends wallet value out of the platform (SANDBOX_WITHDRAWAL: Dr
// WITHDRAWN / Cr WALLET), e.g. the borrowed cash a customer spent elsewhere.
func (f *fixture) withdraw(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxWithdrawal, IdempotencyKey: "sbx-withdraw:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_withdrawal", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWithdrawn, asset), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

func operator(name string) econ.Actor {
	return econ.Actor{Type: security.ActorOperator, ID: name + "-" + uuid.NewString()[:8]}
}

func (f *fixture) propose(cid CaseID, outcome ResolutionOutcome, by econ.Actor) (Resolution, error) {
	var r Resolution
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.ProposeCaseResolution(ctx, tx, ProposeResolutionRequest{CaseID: cid, Outcome: outcome,
			Reason: "itest: " + string(outcome), Proposer: by})
		return err
	})
	return r, err
}

func (f *fixture) decide(rid ResolutionID, d ResolutionDecision, by econ.Actor) (ResolutionResult, error) {
	var r ResolutionResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.DecideCaseResolution(ctx, tx, DecideResolutionRequest{ResolutionID: rid, Decision: d,
			Reason: "itest: " + string(d), Decider: by})
		return err
	})
	return r, err
}

func (f *fixture) repayDeficit(acct accounts.AccountID, amount money.Quantity, key string) (DeficitRepaymentResult, error) {
	var r DeficitRepaymentResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.RepayDeficit(ctx, tx, DeficitRepaymentRequest{AccountID: acct, Amount: amount, IdempotencyKey: key,
			Requester: userRequester})
		return err
	})
	return r, err
}

// reviewAccount needs a service whose policy allows one sale per budget
// window. 10 SOL and 10 METH back a 1,000 loan whose cash has left the
// wallet; both crash to 10, the case sells the METH (its one sale) and stops
// in MANUAL_REVIEW with the SOL still to sell.
func (f *fixture) reviewAccount() (accounts.AccountID, Account, LiquidationResult) {
	f.t.Helper()
	require.Equal(f.t, 1, f.svc.Policy().MaxLiquidationSteps, "reviewAccount needs a one-sale budget")
	eth := f.simAsset("METH", 9, assets.RiskMajor)
	acct := f.account()
	f.fund(acct, f.sol, sol(10))
	f.fund(acct, eth, sol(10))
	a := f.open(acct, ModeCross)
	f.mark("100")
	f.markAsset(eth, "100")
	f.post(acct, f.sol, sol(10))
	f.post(acct, eth, sol(10))
	_, err := f.borrow(acct, usd(1_000), "review-borrow-"+uuid.NewString())
	require.NoError(f.t, err)
	f.withdraw(acct, f.usd, usd(1_000))

	f.clk.Advance(time.Minute)
	f.mark("10")
	f.markAsset(eth, "10")
	first, err := f.liquidate(acct, 0)
	require.NoError(f.t, err)
	require.Equal(f.t, DecisionSellAll, first.Decision)
	require.Equal(f.t, eth, first.Step.AssetID, "the riskier holding is sold first")
	review, err := f.liquidate(acct, 0)
	require.NoError(f.t, err)
	require.Equal(f.t, DecisionManualReview, review.Decision, "%s", review.Reason)
	require.Equal(f.t, CaseManualReview, review.Case.Status)
	return acct, a, review
}

// bankruptAccount is the fixed account whose borrowed cash has left the
// wallet, liquidated at 5 to a bankrupt case whose deficit is not covered yet.
func (f *fixture) bankruptAccount() (accounts.AccountID, Account, Case) {
	f.t.Helper()
	acct, a := f.fixedAccountOnBooks()
	f.withdraw(acct, f.usd, usd(2_000))
	f.clk.Advance(time.Minute)
	f.mark("5")
	for i := 0; i < 5; i++ {
		r, err := f.liquidate(acct, 0)
		require.NoError(f.t, err)
		if r.Decision == DecisionBankrupt {
			require.NotNil(f.t, r.Case.DeficitUnits)
			return acct, a, *r.Case
		}
	}
	f.t.Fatalf("the account did not go bankrupt")
	return accounts.AccountID{}, Account{}, Case{}
}

// countRows counts a query's rows.
func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}
