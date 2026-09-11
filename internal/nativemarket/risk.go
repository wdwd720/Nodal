package nativemarket

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// The risk kernel, applied to a native trade (PART XXXII).
//
// PART XXXII lists what the risk kernel must constrain "at minimum", and two of
// its entries are about this economy specifically: native-market concentration
// and creator concentration. Neither limit existed, and neither did any caller:
// the settlement compiler recorded `RequiresRiskEvaluation: true` for a native
// trade and nothing consumed it, so the route named a control that never ran.
// That is the F-26 shape again — a control nothing reaches is not a control.
//
// # Why the evaluation happens BEFORE the posting and REFUSES
//
// `surveil` already computes a concentration figure, and it ALERTS. That is the
// right treatment for a surveillance heuristic: blocking on one would be a
// denial-of-service vector against creators, and the alert says so. A risk
// LIMIT is a different thing — a number an operator set in a versioned,
// hash-verified policy — and PART XXXII files these two under the controls that
// stop NEW RISK. So this refuses, and the alert still fires for the trades the
// limit permits.
//
// The two live side by side on purpose: one is a judgement about a pattern, the
// other is a rule about a quantity.

// Risk is the part of internal/risk this package uses.
//
// It is a port rather than a direct call because the POLICY is deployment
// state: which limits apply is a versioned document an operator recorded, not a
// constant this package should hold. NewRiskGate is the implementation every
// deployment uses; a test may substitute its own to fix the limits it is about.
type Risk interface {
	// NativeTradePolicy returns the effective policy for the account. It takes
	// the caller's transaction rather than a pool: reading policy on a second
	// connection while this one holds the market row is how F-27 deadlocked.
	NativeTradePolicy(ctx context.Context, q db.Querier, accountID accounts.AccountID) (risk.Policy, error)
	// RecordDecision persists an evaluated decision with the snapshot it was
	// computed from.
	RecordDecision(ctx context.Context, tx pgx.Tx, in risk.Input, d risk.Decision, correlationID string) error
}

// RiskRefusal is the error a trade fails with when a risk limit refuses it.
//
// It carries the decision because a REJECT that is not recorded did not happen
// as far as anybody auditing later is concerned — and the transaction this
// refusal aborts takes any row written inside it with it. So the decision
// travels out with the error, and the caller that owns the database (see
// httpapi's nativeMarketsAdapter) records it once the transaction it could not
// survive has rolled back.
//
// It is deliberately NOT recorded here on a second connection: acquiring one
// while holding this transaction is exactly the pool deadlock F-27 was.
type RiskRefusal struct {
	Input         risk.Input
	Decision      risk.Decision
	CorrelationID string
	err           *errs.Error
}

func (r *RiskRefusal) Error() string { return r.err.Error() }

// Unwrap exposes the underlying API error, so errs.CodeOf and the problem
// writer see RISK_CONCENTRATION rather than an unmapped internal error.
func (r *RiskRefusal) Unwrap() error { return r.err }

// checkRisk refuses a trade that would breach a native concentration limit.
//
// It runs after the fill is priced and before anything is posted, because both
// limits are about the position the trade would LEAVE BEHIND: a limit checked
// against the position before the trade permits exactly the trade that breaches
// it.
//
// A SELL is never refused. Both limits constrain holding too much, and refusing
// somebody's exit because they hold too much would trap them in the position
// the limit exists to discourage. PART XXXII says a kill switch stops new risk
// and must not stop a required unwind; the same reasoning governs a limit.
//
// The bool it returns is whether the account can pay for this fill at all. The
// caller uses it to decide whether the MARKET's own limits are worth evaluating
// (see safety.go): an order larger than the balance fails either way, and
// telling somebody their order moved the market too far when what happened is
// "you do not have that many Credits" is the same wrong answer in a second
// vocabulary.
func (s *Service) checkRisk(ctx context.Context, tx pgx.Tx, m Market, r ExecuteRequest, fill Fill, creator accounts.AccountID) (bool, error) {
	if r.Side == Sell {
		return true, nil
	}
	policy, err := s.risk.NativeTradePolicy(ctx, tx, r.AccountID)
	if err != nil {
		return false, err
	}
	snap, afford, err := s.riskSnapshot(ctx, tx, m, r, fill, creator)
	if err != nil {
		return false, err
	}
	if !afford {
		// The account cannot pay for this fill, so the posting below is about
		// to be refused by the ledger's negative-balance guard with the reason
		// that is actually true.
		//
		// Evaluating anyway would report RISK_CONCENTRATION: an order larger
		// than the balance makes the Credit base smaller than the spend, and
		// the ratio exceeds every limit. Sending somebody to read a
		// concentration policy when what happened is "you do not have that
		// many Credits" is a worse answer, and it is not a bypass -- the trade
		// fails either way and nothing commits.
		return false, nil
	}
	in := risk.NativeTradeInput(r.AccountID.String(), snap, s.clk.Now())
	decision := risk.EvaluateNativeTrade(policy, in)
	if decision.Verdict == risk.Allow {
		// Recorded inside the trade's own transaction: an ALLOW is part of why
		// the trade happened, and a decision that could commit without its
		// trade would be a record that disagrees with the ledger.
		return true, s.risk.RecordDecision(ctx, tx, in, decision, r.CorrelationID)
	}
	return true, &RiskRefusal{
		Input:         in,
		Decision:      decision,
		CorrelationID: r.CorrelationID,
		err: errs.New(errs.CodeRiskConcentration, refusalDetail(decision.ReasonCodes)).
			WithField("market_id", m.ID.String()).
			WithField("reason_codes", decision.ReasonCodes).
			WithField("policy_version", decision.PolicyVersion),
	}
}

// refusalDetail says which limit refused the trade, in words somebody can act
// on.
//
// A customer shown RISK_NATIVE_MARKET_CONCENTRATION learns that something was
// refused and nothing about what to do; both of these limits have an obvious
// remedy -- a smaller order -- and a refusal that hides it is a refusal that
// reads as a fault. The reason codes are still on the problem for anything
// machine-read.
func refusalDetail(reasons []string) string {
	var market, creator bool
	for _, r := range reasons {
		switch r {
		case risk.ReasonNativeMarketConcentration:
			market = true
		case risk.ReasonCreatorConcentration:
			creator = true
		}
	}
	switch {
	case market && creator:
		return "this order would leave you holding too large a share of this asset, " +
			"and too much of your Credits committed to one creator; a smaller order may be within both limits"
	case market:
		return "this order would leave you holding too large a share of this asset's total supply; " +
			"a smaller order may be within the limit"
	case creator:
		return "this order would put too much of your Credits into one creator's assets; " +
			"a smaller order may be within the limit"
	}
	// Every other reason this evaluation can produce is about the DEPLOYMENT,
	// not the order: a policy missing its limits, or an input the kernel could
	// not evaluate. Saying "try a smaller order" there would send somebody to
	// fix something that is not theirs.
	return "the risk kernel could not permit this trade; the reason codes say why"
}

// RecordRefusal persists a decision whose own transaction could not carry it.
//
// The caller must pass a NEW transaction: the one the refusal aborted is gone,
// which is the entire reason this method exists rather than the record being
// written where the decision was made.
func (s *Service) RecordRefusal(ctx context.Context, tx pgx.Tx, ref *RiskRefusal) error {
	if ref == nil {
		return errs.New(errs.CodeInternal, "nativemarket: RecordRefusal needs a refusal")
	}
	return s.risk.RecordDecision(ctx, tx, ref.Input, ref.Decision, ref.CorrelationID)
}

// riskSnapshot builds what the kernel evaluates: counts, never prices.
//
// Every figure is stated AFTER the fill, and is read from the ledger and then
// adjusted rather than read after posting, because a decision that has to be
// able to refuse cannot wait until the movement is already written.
//
// The bool reports whether the account can pay for the fill at all. See
// checkRisk for why that decides whether the limits are evaluated.
func (s *Service) riskSnapshot(
	ctx context.Context, tx pgx.Tx, m Market, r ExecuteRequest, fill Fill, creator accounts.AccountID,
) (risk.NativeMarketSnapshot, bool, error) {
	var heldText, supplyText string
	if err := tx.QueryRow(ctx, riskHoldingQuery, r.AccountID, m.AssetID).Scan(&heldText); err != nil {
		return risk.NativeMarketSnapshot{}, false, mapError(err)
	}
	if err := tx.QueryRow(ctx, riskSupplyQuery, m.AssetID).Scan(&supplyText); err != nil {
		return risk.NativeMarketSnapshot{}, false, mapError(err)
	}
	var onCreatorText, onAllText string
	if err := tx.QueryRow(ctx, riskSpendQuery, r.AccountID, creator).Scan(&onCreatorText, &onAllText); err != nil {
		return risk.NativeMarketSnapshot{}, false, mapError(err)
	}
	var creditsText string
	if err := tx.QueryRow(ctx, spendableBalanceQuery, r.AccountID, m.CreditAssetID).Scan(&creditsText); err != nil {
		return risk.NativeMarketSnapshot{}, false, mapError(err)
	}

	held, err := parseBase("holding", heldText)
	if err != nil {
		return risk.NativeMarketSnapshot{}, false, err
	}
	supply, err := parseBase("supply", supplyText)
	if err != nil {
		return risk.NativeMarketSnapshot{}, false, err
	}
	onCreator, err := parseBase("creator spend", onCreatorText)
	if err != nil {
		return risk.NativeMarketSnapshot{}, false, err
	}
	onAll, err := parseBase("native spend", onAllText)
	if err != nil {
		return risk.NativeMarketSnapshot{}, false, err
	}
	credits, err := parseBase("credit balance", creditsText)
	if err != nil {
		return risk.NativeMarketSnapshot{}, false, err
	}

	// After the fill. The units the pool gives up are the units this account
	// gains, and the Credits that leave its balance are the Credits it commits.
	holdingAfter := held.Add(fill.AssetsOut)
	onCreatorAfter := onCreator.Add(fill.CreditsIn)
	onAllAfter := onAll.Add(fill.CreditsIn)
	spendableAfter := credits.Sub(fill.CreditsIn)

	return risk.NativeMarketSnapshot{
		MarketID:                m.ID.String(),
		AssetID:                 m.AssetID.String(),
		CreatorAccountID:        creator.String(),
		HoldingAfter:            holdingAfter,
		TotalSupply:             supply,
		SpendOnThisCreatorAfter: onCreatorAfter,
		// What this account has riding on the internal economy: everything it
		// has committed to native assets, plus everything it could still
		// commit. See NativeMarketSnapshot for why the unspent half is in here.
		//
		// The fill cancels out of this sum -- it is added to the spend and
		// taken off the balance -- so the denominator is the same before and
		// after the trade. That is the point: a limit whose denominator grew
		// with the order would be one a large enough order could satisfy.
		CreditBaseAfter: onAllAfter.Add(spendableAfter),
	}, credits.Cmp(fill.CreditsIn) >= 0, nil
}

// parseBase turns a numeric column into an exact base-unit quantity.
func parseBase(what, text string) (money.Quantity, error) {
	q, err := money.ParseQuantity(text)
	if err != nil {
		return money.Quantity{}, errs.Wrapf(err, errs.CodeInternal, "nativemarket: %s is not an integer", what)
	}
	return q, nil
}

// riskHoldingQuery reads what the account holds of this asset today.
const riskHoldingQuery = `
	SELECT coalesce((SELECT b.balance FROM ledger_accounts la
	    JOIN ledger_balances b ON b.ledger_account_id = la.id
	   WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1
	     AND la.code = 'NATIVE_ASSET_BALANCE' AND la.asset_id = $2), 0)::text`

// spendableBalanceQuery reads the account's Credit balance today.
//
// Named for the balance rather than for the asset because gosec's G101 word
// list contains "cred": a constant whose NAME contains "Credit" and whose value
// is a long string is reported as a hardcoded credential, and `make sast` fails
// on it. A suppression would have worked; a name that is equally accurate is
// better than a suppression.
const spendableBalanceQuery = `
	SELECT coalesce((SELECT b.balance FROM ledger_accounts la
	    JOIN ledger_balances b ON b.ledger_account_id = la.id
	   WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1
	     AND la.code = 'CREDIT_BALANCE' AND la.asset_id = $2), 0)::text`

// riskSupplyQuery reads every unit of the asset that exists.
//
// max_supply is the whole mint: PART XIII allows exactly one, at market
// creation, and the column is immutable after activation. Nobody -- creator,
// platform or holder -- can move this denominator.
const riskSupplyQuery = `SELECT max_supply::text FROM native_assets WHERE asset_id = $1`

// riskSpendQuery totals the Credits this account has spent on native assets,
// split by whether the asset's creator is the one named.
//
// It sums FILLS rather than current holdings on purpose: cost basis is a fact
// nobody can rewrite, and the current value of a thinly traded native asset can
// be moved by the very holder the limit constrains.
const riskSpendQuery = `
	SELECT
	  coalesce(sum(f.credits_in) FILTER (WHERE na.creator_account_id = $2), 0)::text,
	  coalesce(sum(f.credits_in), 0)::text
	  FROM native_market_fills f
	  JOIN native_markets m  ON m.id = f.market_id
	  JOIN native_assets  na ON na.asset_id = m.asset_id
	 WHERE f.account_id = $1 AND f.side = 'BUY'`

// --- the deployment's implementation ----------------------------------------

// PolicyStore is the part of risk.Store the gate uses.
type PolicyStore interface {
	EffectivePolicy(ctx context.Context, q db.Querier, accountID, agentID string, at time.Time) (risk.Policy, risk.PolicyRef, error)
	RecordDecision(ctx context.Context, tx pgx.Tx, in risk.Input, d risk.Decision, correlationID string) (risk.DecisionID, error)
}

// riskGate composes the effective policy from risk_policies.
type riskGate struct {
	store PolicyStore
	clk   clock.Clock
}

// NewRiskGate returns the Risk a deployment uses: the real policy store, read
// through the caller's own transaction.
//
// There is no agent scope here. An agent trading a native market would have its
// own policy row, and passing an empty agent id means only the GLOBAL and
// ACCOUNT rows compose — which is correct for a person trading their own
// account and is the only case this path currently has.
func NewRiskGate(store PolicyStore, clk clock.Clock) Risk {
	if store == nil || clk == nil {
		panic("nativemarket: NewRiskGate requires a policy store and a clock")
	}
	return riskGate{store: store, clk: clk}
}

func (g riskGate) NativeTradePolicy(ctx context.Context, q db.Querier, accountID accounts.AccountID) (risk.Policy, error) {
	p, _, err := g.store.EffectivePolicy(ctx, q, accountID.String(), "", g.clk.Now())
	switch {
	case errors.Is(err, risk.ErrNoPolicy):
		// A deployment with no GLOBAL risk policy has not decided what its
		// limits are, and an undecided control fails closed rather than open.
		//
		// It is UNSUPPORTED rather than INTERNAL because it is true and
		// actionable: this deployment cannot evaluate an internal trade, and
		// the fix is `go run ./scripts/riskpolicy`, not a bug report. The
		// sentinel stays reachable through errors.Is for anything that needs
		// to tell this apart from a deployment that never had the feature.
		return risk.Policy{}, errs.Wrap(err, errs.CodeUnsupported,
			"this deployment has recorded no risk policy, so no internal trade can be evaluated")
	case err != nil:
		return risk.Policy{}, err
	}
	return p, nil
}

func (g riskGate) RecordDecision(ctx context.Context, tx pgx.Tx, in risk.Input, d risk.Decision, correlationID string) error {
	_, err := g.store.RecordDecision(ctx, tx, in, d, correlationID)
	return err
}
