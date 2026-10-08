package treasury

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// RiskDecision is one persisted Risk Kernel V2 decision about a house order
// or fill, with the treasury limit evaluation that fed it.
type RiskDecision struct {
	ID            DecisionID
	Verdict       risk.Verdict
	ReasonCodes   []string
	TreasuryCodes []string
	DecisionHash  string
	InputHash     string
	Decision      risk.AdvancedDecision
	Limits        LimitEvaluation
}

// riskRequest is everything one evaluation needs. Every value comes from the
// caller's transaction under the book's lock.
type riskRequest struct {
	book           Book
	policy         PolicyVersion
	decisionFor    ActionID
	intentKind     econ.IntentKind
	actor          econ.Actor
	mode           gates.Mode
	instrument     *instruments.InstrumentID
	instrumentKind string
	instrumentSnap risk.InstrumentSnapshot
	asset          assets.AssetID
	riskIncreasing bool
	notionalUSD    money.USD
	balance        risk.BalanceSnapshot
	mark           *money.Price
	limits         LimitEvaluation
}

// evaluateRisk builds the Risk Kernel V2 input of a house order or fill,
// evaluates it, and persists the decision. The treasury limits enter as named
// circuit breakers, which the kernel turns into RISK_CIRCUIT_BREAKER for a
// risk-increasing request; the kill switches, the house book's status, the
// instrument, the balance and (for new risk) the oracle are sections of their
// own. A missing section is a rejection, never a pass.
func (s *Service) evaluateRisk(ctx context.Context, tx pgx.Tx, r riskRequest) (RiskDecision, error) {
	now := s.clk.Now().UTC()
	kills, err := activeKillSwitches(ctx, s.kills, tx)
	if err != nil {
		return RiskDecision{}, err
	}
	inst := ""
	if r.instrument != nil {
		inst = r.instrument.String()
	}
	instrumentSection := r.instrumentSnap
	balance := r.balance
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: r.decisionFor.String(), Kind: string(r.intentKind), AccountID: r.book.HouseAccountID.String(),
			ActorType: string(r.actor.Type), Capability: string(gates.PrincipalTrading), Mode: string(r.mode),
			InstrumentID: inst, InstrumentKind: r.instrumentKind, NotionalUSD: r.notionalUSD, RiskIncreasing: r.riskIncreasing,
		},
		// A house book is a platform-operated principal book an operator
		// registered: its identity is the platform's own.
		Identity:        &risk.IdentitySnapshot{Verified: true},
		Account:         &risk.AdvancedAccountSnapshot{Status: string(r.book.Status)},
		Instrument:      &instrumentSection,
		Balance:         &balance,
		CircuitBreakers: append([]string{}, r.limits.Breakers...),
		KillSwitches:    kills,
		Now:             now,
	}
	if r.riskIncreasing {
		if r.mark != nil {
			input.Oracle = &risk.OracleSnapshot{Available: true, ObservedAt: r.mark.At.UTC()}
		} else {
			input.Oracle = &risk.OracleSnapshot{Available: false}
		}
	}
	d := risk.EvaluateAdvanced(s.policy, input)
	inputJSON, err := audit.CanonicalJSON(input)
	if err != nil {
		return RiskDecision{}, errs.Wrap(err, errs.CodeInternal, "treasury: encode risk input")
	}
	decisionJSON, err := d.CanonicalJSON()
	if err != nil {
		return RiskDecision{}, errs.Wrap(err, errs.CodeInternal, "treasury: encode risk decision")
	}
	limitsJSON, err := audit.CanonicalJSON(r.limits)
	if err != nil {
		return RiskDecision{}, errs.Wrap(err, errs.CodeInternal, "treasury: encode limit evaluation")
	}
	codes := d.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	out := RiskDecision{
		ID: id.New[decisionKind](), Verdict: d.Verdict, ReasonCodes: codes, TreasuryCodes: r.limits.Codes,
		DecisionHash: d.Hash, InputHash: d.InputHash, Decision: d, Limits: r.limits,
	}
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_risk_decisions (id, house_account_id, intent_kind, instrument_id, asset_id,
		risk_increasing, stage, verdict, reason_codes, treasury_codes, input, decision, limits, input_hash, decision_hash,
		risk_policy_version, risk_policy_hash, evaluator_version, treasury_policy_version, treasury_policy_hash, evaluated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		out.ID, r.book.HouseAccountID, r.intentKind, r.instrument, r.asset, r.riskIncreasing, d.Stage, d.Verdict, codes,
		r.limits.Codes, inputJSON, decisionJSON, limitsJSON, d.InputHash, d.Hash, d.PolicyVersion, d.PolicyHash,
		d.EvaluatorVersion, r.policy.Version, r.policy.Hash, d.EvaluatedAt); err != nil {
		return RiskDecision{}, dbErr("record treasury risk decision", err)
	}
	return out, nil
}

// activeKillSwitches returns the active switches as the kernel reads them,
// sorted so the stored input is exactly the one evaluated.
func activeKillSwitches(ctx context.Context, kills econ.KillSwitchReader, q db.Querier) ([]risk.KillSwitch, error) {
	snap, err := kills.Snapshot(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []risk.KillSwitch{}
	for _, sw := range snap.Switches {
		if sw.Active {
			out = append(out, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}

// Decision returns one persisted risk decision.
func (s *Service) Decision(ctx context.Context, q db.Querier, did DecisionID) (RiskDecision, error) {
	var d RiskDecision
	var verdict string
	err := q.QueryRow(ctx, `SELECT id, verdict, reason_codes, treasury_codes, decision_hash, input_hash FROM treasury_risk_decisions
		WHERE id = $1`, did).Scan(&d.ID, &verdict, &d.ReasonCodes, &d.TreasuryCodes, &d.DecisionHash, &d.InputHash)
	if err != nil {
		return RiskDecision{}, notFound(err, "treasury risk decision", did.String())
	}
	d.Verdict = risk.Verdict(verdict)
	return d, nil
}

// usd converts valuation-asset base units to USD at parity (the valuation
// asset is USD-pegged by registration) in the mode a check needs.
func usd(q money.Quantity, decimals uint8, mode money.RoundingMode) (money.USD, error) {
	u, err := money.QuoteQuantityToUSD(q, decimals, mode)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "treasury: amount in USD")
	}
	return u, nil
}

// tradingDay is the UTC calendar day containing t, as [start, end).
func tradingDay(t time.Time) (time.Time, time.Time) {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return start, start.Add(24 * time.Hour)
}
