package derivatives

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
)

// Effect kinds this subsystem claims.
const (
	EffectMarginPost       = "DERIV_MARGIN_POST"
	EffectMarginRelease    = "DERIV_MARGIN_RELEASE"
	EffectRealized         = "DERIV_REALIZED"
	EffectPremium          = "DERIV_OPTION_PREMIUM"
	EffectVariationMargin  = "DERIV_VARIATION_MARGIN"
	EffectFunding          = "DERIV_FUNDING"
	EffectFutureSettlement = "DERIV_FUTURE_SETTLEMENT"
	EffectOptionSettlement = "DERIV_OPTION_SETTLEMENT"
	EffectClearingFunding  = "DERIV_CLEARING_FUNDING"
)

// leg is one posting of an operation: one economic effect, one journal
// transaction, and at most one row of the cash-flow register.
type leg struct {
	// name distinguishes the legs of one operation inside its evidence.
	name string
	// key is the deterministic effect key; the ledger idempotency key is
	// derived from it.
	key        string
	effectKind string
	kind       ledger.Kind
	account    *accounts.AccountID
	intent     *econ.IntentID
	reference  ledger.FinancialEventReference
	entries    []ledger.Entry
	// flow, when set, is the register row of a customer <-> clearing house
	// cash flow. Its journal and effect are filled in when the leg posts. A
	// flow with nothing collected (a pure shortfall) is registered without a
	// posting.
	flow *CashFlow
}

// postedLeg is a leg after execution.
type postedLeg struct {
	Name    string                `json:"name"`
	Kind    ledger.Kind           `json:"kind"`
	Key     string                `json:"effect_key"`
	Journal *ledger.TransactionID `json:"journal_transaction_id,omitempty"`
	Effect  *econ.EffectID        `json:"effect_id,omitempty"`
	Flow    *registeredFlow       `json:"cash_flow,omitempty"`
	Entries []evidenceEntry       `json:"entries"`
	replay  bool
	status  econ.EffectStatus
}

type registeredFlow struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Amount    string `json:"amount"`
	Shortfall string `json:"shortfall"`
}

type evidenceEntry struct {
	Account  string `json:"account"`
	Side     string `json:"side"`
	Quantity string `json:"quantity"`
}

func evidenceEntries(es []ledger.Entry) []evidenceEntry {
	out := make([]evidenceEntry, 0, len(es))
	for _, e := range es {
		out = append(out, evidenceEntry{Account: e.Account.String(), Side: string(e.Side), Quantity: e.Quantity.String()})
	}
	return out
}

// legRequest is the canonical request an effect's hash covers: the posting
// it will make. A different posting under the same key is a reuse.
type legRequest struct {
	Kind      string          `json:"kind"`
	Reference string          `json:"reference"`
	Entries   []evidenceEntry `json:"entries"`
}

// executeLegs runs the legs of one operation in order. Each leg claims its
// effect (a replay of an APPLIED effect posts nothing; an effect in any other
// recorded state is never retried), posts its journal transaction and
// registers its cash flow. Then the operation's evidence document is archived
// once and every effect claimed here is applied with its journal and the
// evidence reference -- all inside tx.
func (s *Service) executeLegs(ctx context.Context, tx pgx.Tx, ch econ.Facility, legs []leg, evidenceKey string,
	evidence func([]postedLeg) any, correlationID string) ([]postedLeg, error) {
	now := s.clk.Now().UTC()
	posted := make([]postedLeg, 0, len(legs))
	for _, l := range legs {
		pl := postedLeg{Name: l.name, Kind: l.kind, Key: l.key, Entries: evidenceEntries(l.entries)}
		if len(l.entries) == 0 {
			if l.flow != nil {
				if !l.flow.Amount.IsZero() {
					return nil, errs.New(errs.CodeInternal, "derivatives: a cash flow that moved value needs a posting").WithField("leg", l.name)
				}
				if err := s.registerFlow(ctx, tx, ch, l.flow, nil, nil, now); err != nil {
					return nil, err
				}
				pl.Flow = flowEvidence(l.flow)
				posted = append(posted, pl)
			}
			continue
		}
		var acct string
		if l.account != nil {
			acct = l.account.String()
		}
		eff, replay, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
			Key: l.key, Subsystem: econ.SubsystemDerivatives, Kind: l.effectKind, IntentID: l.intent, AccountID: acct,
			Request:     legRequest{Kind: string(l.kind), Reference: l.reference.Type + "/" + l.reference.ID, Entries: pl.Entries},
			PostsLedger: true, Actor: s.actor, CorrelationID: correlationID,
		})
		if err != nil {
			return nil, err
		}
		effID := eff.ID
		pl.Effect = &effID
		if replay {
			switch eff.Status {
			case econ.EffectApplied:
				pl.Journal, pl.replay, pl.status = eff.JournalTransactionID, true, eff.Status
				posted = append(posted, pl)
				continue
			case econ.EffectUnknown:
				return nil, errs.New(errs.CodeReconciliationRequired,
					"derivatives: this effect's outcome is UNKNOWN; it is reconciled, never retried").WithField("effect_key", l.key)
			default:
				return nil, errs.Newf(errs.CodeConflict, "derivatives: effect %s is %s", l.key, eff.Status).WithField("effect_key", l.key)
			}
		}
		res, err := s.ledger.Post(ctx, tx, ledger.Posting{
			Kind: l.kind, IdempotencyKey: "derivatives:" + l.key, Reference: l.reference, EffectiveAt: now,
			Description: "derivatives " + l.name, CorrelationID: correlationID, Entries: l.entries,
		})
		if err != nil {
			return nil, err
		}
		jid := res.TransactionID
		pl.Journal, pl.status = &jid, econ.EffectPending
		if l.flow != nil {
			// A flow that collected nothing (a whole shortfall) names no
			// journal: the leg posted only what did not involve the
			// clearing house.
			var fj *ledger.TransactionID
			var fe *econ.EffectID
			if !l.flow.Amount.IsZero() {
				fj, fe = &jid, &effID
			}
			if err := s.registerFlow(ctx, tx, ch, l.flow, fj, fe, now); err != nil {
				return nil, err
			}
			pl.Flow = flowEvidence(l.flow)
		}
		posted = append(posted, pl)
	}
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: evidenceKey, Document: evidence(posted)})
	if err != nil {
		return nil, err
	}
	for _, pl := range posted {
		if pl.replay || pl.Effect == nil || pl.Journal == nil {
			continue
		}
		if _, err := s.effects.Apply(ctx, tx, *pl.Effect, econ.ApplyRequest{
			JournalTransactionID: pl.Journal, Evidence: &ref, Actor: s.actor, Reason: pl.Name,
		}); err != nil {
			return nil, err
		}
	}
	return posted, nil
}

func (s *Service) registerFlow(ctx context.Context, tx pgx.Tx, ch econ.Facility, f *CashFlow, journal *ledger.TransactionID, effect *econ.EffectID, now time.Time) error {
	if f.ID.IsZero() {
		f.ID = newCashFlowID()
	}
	f.JournalTransactionID = journal
	if f.OccurredAt.IsZero() {
		f.OccurredAt = now
	}
	return insertCashFlow(ctx, tx, *f, ch.ID, effect)
}

func flowEvidence(f *CashFlow) *registeredFlow {
	return &registeredFlow{ID: f.ID.String(), Kind: string(f.Kind), Amount: f.Amount.String(), Shortfall: f.Shortfall.String()}
}

// journals lists the journal transactions of executed legs.
func journals(posted []postedLeg) []ledger.TransactionID {
	var out []ledger.TransactionID
	for _, p := range posted {
		if p.Journal != nil {
			out = append(out, *p.Journal)
		}
	}
	return out
}
