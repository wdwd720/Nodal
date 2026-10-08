package predmarket

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/settlement"
)

// One economic action -- an issuance, a redemption, one holder's payout or
// refund -- moves through the steps of ADVANCED_FINANCIAL_MODEL §6 in two
// phases inside the caller's transaction:
//
//  1. Decide: submit the intent (idempotent on account + key), route it
//     through the settlement compiler, authorize the mode, evaluate Risk V2
//     and record the decision, then check the market's own state. A refusal
//     here moves the intent to REJECTED and returns the refusal; a caller
//     that commits keeps it on record.
//  2. Execute, all or nothing, in a savepoint: reserve capital, claim the
//     effect under its deterministic key, post the journal, consume the
//     reservations, archive the evidence, apply the effect, complete the
//     intent, and re-check the escrow identity. Any failure rolls the
//     savepoint back -- no reservation, effect or journal survives it -- and
//     the intent is REJECTED with the failure's code.

// reserveSpec is one capital reservation an action takes and consumes.
type reserveSpec struct {
	Asset    assets.AssetID
	Quantity money.Quantity
	// Suffix makes the reservation's idempotency key unique within the action.
	Suffix string
}

// action describes one economic action.
type action struct {
	intent         econ.Intent
	subject        settlement.Subject
	agentAuthority agentauthority.Level
	facts          AccountFacts
	// mover is who moves the intent through its lifecycle: the settling
	// operator for a payout or refund, this service for a customer's own
	// request. Never an agent.
	mover econ.Actor
	// effectKey is the deterministic key of the action's economic effect, as
	// a function of its intent.
	effectKey  func(it econ.Intent) string
	effectKind string
	// riskInput builds the Risk V2 snapshot once the intent has its id.
	riskInput func(it econ.Intent) (risk.AdvancedInput, error)
	// precheck refuses on the market's own state once Risk V2 has allowed.
	precheck func(tx pgx.Tx) error
	reserves []reserveSpec
	request  func(it econ.Intent) any
	posting  func(it econ.Intent) (ledger.Posting, error)
	evidence func(it econ.Intent, d risk.AdvancedDecision, r settlement.Route, reservations []string, journal ledger.TransactionID) any
	// afterApply runs inside the savepoint once the effect is APPLIED and the
	// intent COMPLETED, with what the action did: it records what the action
	// is for (a held payout's claim). A failure undoes the action.
	afterApply func(tx pgx.Tx, res ActionResult) error
	// postcheck runs last inside the savepoint; a failure undoes the action.
	postcheck func(tx pgx.Tx) error
}

// ActionResult is what one economic action did.
type ActionResult struct {
	Intent               econ.Intent
	Effect               econ.Effect
	DecisionHash         string
	JournalTransactionID ledger.TransactionID
	// Replay is true when the request had already been made: nothing new
	// happened, and the result describes the first attempt.
	Replay bool
}

// run takes one economic action through both phases.
func (s *Service) run(ctx context.Context, tx pgx.Tx, a action, mode gates.Mode) (ActionResult, error) {
	now := s.now()
	it, existing, err := s.intents.Submit(ctx, tx, a.intent)
	if err != nil {
		return ActionResult{}, err
	}
	if existing {
		return s.replay(ctx, tx, it, a.effectKey(it))
	}
	res := ActionResult{Intent: it}

	route, d, err := s.decide(ctx, tx, &res, a, mode, now)
	if err != nil {
		return res, err
	}

	var done ActionResult
	err = atomically(ctx, tx, func(sp pgx.Tx) error {
		var err error
		done, err = s.execute(ctx, sp, res, a, route, d, now)
		return err
	})
	if err != nil {
		if db.IsRetryable(err) {
			return res, err
		}
		return s.refuse(ctx, tx, res, a.mover, err, "execution failed")
	}
	return done, nil
}

// routeRefusalReasons are why a route may not execute now, sorted: the
// compiler's refusals, and for a route it permits only after a person's
// confirmation or an operator's review, that step (REQUIRES_USER_CONFIRMATION,
// REQUIRES_MANUAL_REVIEW).
func routeRefusalReasons(route settlement.Route) []string {
	reasons := append([]string{}, route.Reasons...)
	if route.Permitted && route.RequiredConfirmation != settlement.ConfirmNone {
		reasons = append(reasons, "REQUIRES_"+string(route.RequiredConfirmation))
	}
	sort.Strings(reasons)
	return reasons
}

// decide is phase 1. It returns the route and the risk decision, or the
// refusal it has already recorded on the intent.
func (s *Service) decide(ctx context.Context, tx pgx.Tx, res *ActionResult, a action, mode gates.Mode, now time.Time) (settlement.Route, risk.AdvancedDecision, error) {
	it := res.Intent
	route, err := econ.Route(econ.RouteRequest{
		Intent: it, Subject: a.subject, AgentAuthority: a.agentAuthority,
		Jurisdiction: a.facts.Jurisdiction, Verification: a.facts.Verification,
	}, s.router, now)
	if err != nil {
		*res, err = s.refuse(ctx, tx, *res, a.mover, err, "the settlement compiler could not route the intent")
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}
	// The market executes what it routes, so it needs a route that may
	// execute now: one the legal router permits only once a person has
	// confirmed it (or an operator reviewed it) is permission to ask, and
	// nobody here is asked. It is refused with the step it would have
	// skipped among its reasons (review 3, F5).
	if !route.MayExecuteNow() {
		reasons := routeRefusalReasons(route)
		*res, err = s.refuse(ctx, tx, *res, a.mover,
			errs.New(errs.CodeForbidden, "predmarket: the settlement compiler did not route the intent for execution now").
				WithField("reasons", reasons).WithField("required_confirmation", string(route.RequiredConfirmation)).
				WithField("refusal_stage", "route"),
			"route refused: "+strings.Join(reasons, ", "))
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}
	if res.Intent, err = s.intents.Transition(ctx, tx, it.ID, econ.StatusRouted, a.mover, "routed: "+route.Product, ""); err != nil {
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}

	if err := s.authorize(ctx, tx, mode); err != nil {
		*res, err = s.refuse(ctx, tx, *res, a.mover, err, "not authorized in mode "+string(mode))
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}

	in, err := a.riskInput(res.Intent)
	if err != nil {
		*res, err = s.refuse(ctx, tx, *res, a.mover, err, "the risk snapshot could not be built")
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}
	d, err := s.evaluate(ctx, tx, in, it.CorrelationID)
	if err != nil {
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}
	res.DecisionHash = d.Hash
	if d.Verdict != risk.Allow {
		*res, err = s.refuse(ctx, tx, *res, a.mover, d.RejectionError(), "risk kernel rejected: "+strings.Join(d.ReasonCodes, ", "))
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}
	if a.precheck != nil {
		if err := a.precheck(tx); err != nil {
			*res, err = s.refuse(ctx, tx, *res, a.mover, err, "refused: "+string(errs.CodeOf(err)))
			return settlement.Route{}, risk.AdvancedDecision{}, err
		}
	}
	if res.Intent, err = s.intents.Transition(ctx, tx, it.ID, econ.StatusRiskApproved, a.mover, "risk approved "+d.Hash, ""); err != nil {
		return settlement.Route{}, risk.AdvancedDecision{}, err
	}
	return route, d, nil
}

// execute is phase 2, run on a savepoint.
func (s *Service) execute(ctx context.Context, tx pgx.Tx, res ActionResult, a action, route settlement.Route,
	d risk.AdvancedDecision, now time.Time) (ActionResult, error) {
	it := res.Intent
	key := a.effectKey(it)
	move := func(to econ.IntentStatus, reason string) error {
		next, err := s.intents.Transition(ctx, tx, it.ID, to, a.mover, reason, "")
		if err != nil {
			return err
		}
		res.Intent = next
		return nil
	}

	var reservations []capital.Reservation
	for _, r := range a.reserves {
		// asset_reservations.intent_id references the V1 trade_intents table,
		// so an economic intent is linked through the reservation's
		// idempotency key, the effect's reservation_id and the evidence.
		rv, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: it.AccountID.String(), AssetID: r.Asset, Quantity: r.Quantity,
			EconomicIntentID: it.ID.String(),
			ActorType:        it.ActorType, ActorID: it.ActorID, IdempotencyKey: key + ":" + r.Suffix,
			TTL: ReservationTTL, Reason: a.effectKind + " for economic intent " + it.ID.String(),
		})
		if err != nil {
			return res, err
		}
		reservations = append(reservations, rv)
	}
	if len(reservations) > 0 {
		if err := move(econ.StatusReserved, "capital reserved"); err != nil {
			return res, err
		}
	}

	eff, replayed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemPredictionMarkets, Kind: a.effectKind, IntentID: &it.ID,
		AccountID: it.AccountID.String(), Request: a.request(it), PostsLedger: true, Actor: serviceActor(),
		CorrelationID: it.CorrelationID,
	})
	if err != nil {
		return res, err
	}
	if replayed {
		// A fresh intent finding its effect already claimed would mean two
		// requests share one economic effect: it is never applied twice.
		return res, errs.New(errs.CodeReconciliationRequired, "predmarket: this economic effect was already claimed by another request").
			WithField("effect_key", key).WithField("effect_status", string(eff.Status))
	}
	res.Effect = eff
	if err := move(econ.StatusExecuting, "executing "+a.effectKind); err != nil {
		return res, err
	}

	p, err := a.posting(it)
	if err != nil {
		return res, err
	}
	posted, err := s.ledger.Post(ctx, tx, p)
	if err != nil {
		return res, err
	}
	if posted.Existing {
		return res, errs.New(errs.CodeReconciliationRequired, "predmarket: the journal transaction of this effect already exists").
			WithField("journal_transaction_id", posted.TransactionID.String())
	}
	res.JournalTransactionID = posted.TransactionID

	reservationIDs := make([]string, 0, len(reservations))
	for _, rv := range reservations {
		if _, err := s.capital.ConsumeFinal(ctx, tx, rv.ID, rv.Quantity, 0, ""); err != nil {
			return res, err
		}
		reservationIDs = append(reservationIDs, rv.ID.String())
	}

	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key:      "predmarket/" + strings.ToLower(a.effectKind) + "/" + it.ID.String(),
		Document: a.evidence(it, d, route, reservationIDs, posted.TransactionID),
	})
	if err != nil {
		return res, err
	}
	first := ""
	if len(reservationIDs) > 0 {
		first = reservationIDs[0]
	}
	applied, err := s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &posted.TransactionID, ReservationID: first, Evidence: &ref,
		Actor: serviceActor(), Reason: a.effectKind + " posted",
	})
	if err != nil {
		return res, err
	}
	res.Effect = applied
	if err := move(econ.StatusCompleted, a.effectKind+" applied at "+now.Format(time.RFC3339Nano)); err != nil {
		return res, err
	}
	if a.afterApply != nil {
		if err := a.afterApply(tx, res); err != nil {
			return res, err
		}
	}
	if a.postcheck != nil {
		if err := a.postcheck(tx); err != nil {
			return res, err
		}
	}
	return res, nil
}

// refuse moves the intent to REJECTED with the refusal's code and returns
// the refusal. If even that transition fails, the transaction is unusable and
// the caller rolls it back; the refusal is returned either way.
func (s *Service) refuse(ctx context.Context, tx pgx.Tx, res ActionResult, mover econ.Actor, cause error, reason string) (ActionResult, error) {
	code := errs.CodeOf(cause)
	if rejected, err := s.intents.Transition(ctx, tx, res.Intent.ID, econ.StatusRejected, mover, reason, string(code)); err == nil {
		res.Intent = rejected
	}
	return res, cause
}

// replay answers a request whose intent already exists: a completed request
// returns what it did, a refused one is refused again with the same code, and
// one that never finished is left to reconciliation -- never retried.
func (s *Service) replay(ctx context.Context, tx pgx.Tx, it econ.Intent, effectKey string) (ActionResult, error) {
	res := ActionResult{Intent: it, Replay: true}
	switch it.Status {
	case econ.StatusCompleted:
		e, found, err := s.effects.GetByKey(ctx, tx, effectKey)
		if err != nil {
			return res, err
		}
		if !found || e.Status != econ.EffectApplied || e.JournalTransactionID == nil {
			return res, errs.New(errs.CodeReconciliationRequired, "predmarket: a completed intent has no applied effect").
				WithField("intent_id", it.ID.String())
		}
		res.Effect = e
		res.JournalTransactionID = *e.JournalTransactionID
		return res, nil
	case econ.StatusRejected:
		code := errs.Code(it.RejectionCode)
		if code == "" {
			code = errs.CodeConflict
		}
		return res, errs.New(code, "predmarket: this request was refused when it was first made; a new request needs a new idempotency key").
			WithField("intent_id", it.ID.String())
	}
	return res, errs.New(errs.CodeReconciliationRequired,
		"predmarket: an earlier attempt of this request did not finish; it is reconciled, never retried").
		WithField("intent_id", it.ID.String()).WithField("intent_status", string(it.Status))
}
