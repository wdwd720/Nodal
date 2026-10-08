package orders

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/matching"
)

// House-versus-customer crossing policy.
//
// Owners are typed: a customer account or a house (principal) book. By
// default a command that would make a house book trade with a customer is
// refused before it touches the book. Whether it would is decided exactly,
// not estimated: the live engine's snapshot is restored into a private copy,
// the command is applied to the copy, and the makers of every TRADE it
// produces are looked up. Only a versioned crossing rule recorded for the book
// (SetCrossingRule) can allow it, and then every such trade names the rule
// version in book_trades (the database refuses a cross-class trade without
// one). Each decision taken where a cross was possible is a
// book_crossing_decisions row with an archived evidence document naming the
// rule version that applied (0: the default, which forbids).

// crossingDecision is the outcome of the policy for one command.
type crossingDecision struct {
	// Possible is true when a resting order of the other owner type sat on
	// the opposite side within the command's price limit.
	Possible bool
	// WouldCross is true when the dry run traded across owner types.
	WouldCross bool
	Allowed    bool
	// RuleVersion is the crossing rule in force (0: none on record).
	RuleVersion int
	Trades      int
	// DryRun is the engine's result on the private copy, when one ran.
	DryRun    matching.Result
	DryRunRan bool
	Evidence  econ.EvidenceRef
}

// checkCrossing applies the crossing policy to cmd, a NEW_ORDER or REPLACE
// from an owner of takerType trading on side within limitTicks. alwaysDryRun
// runs the dry run even when no cross is possible (a replace uses it to learn,
// before adjusting any reservation, whether the engine would accept it).
func (s *Service) checkCrossing(ctx context.Context, tx pgx.Tx, c *bookConfig, eng *engineEntry, kind CommandKind, cmd matching.Command,
	orderID OrderID, takerType OwnerType, side Side, limitTicks int64, alwaysDryRun bool) (crossingDecision, error) {
	var d crossingDecision
	opposite := SideSell
	cond := `price_ticks <= $4`
	if side == SideSell {
		opposite, cond = SideBuy, `price_ticks >= $4`
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM book_orders WHERE book_id = $1 AND side = $2
		AND status IN ('OPEN','PARTIALLY_FILLED') AND owner_type <> $3 AND `+cond+`)`,
		c.ID, opposite, takerType, limitTicks).Scan(&d.Possible); err != nil {
		return crossingDecision{}, dbErr("crossing pre-check", err)
	}
	if !d.Possible && !alwaysDryRun {
		d.Allowed = true
		return d, nil
	}
	shadow, err := matching.Restore(eng.book.Snapshot())
	if err != nil {
		return crossingDecision{}, errs.Wrap(err, errs.CodeInternal, "orders: the live book does not restore into a dry-run copy")
	}
	res, err := shadow.Apply(cmd)
	if err != nil {
		return crossingDecision{}, errs.Wrap(err, errs.CodeInternal, "orders: dry run")
	}
	d.DryRun, d.DryRunRan = res, true
	var makers []string
	for _, e := range res.Events {
		if e.Type == matching.EventTrade {
			makers = append(makers, e.MakerOrderID)
		}
	}
	types := map[string]OwnerType{}
	if len(makers) > 0 {
		rows, err := tx.Query(ctx, `SELECT id::text, owner_type FROM book_orders WHERE id = ANY($1::uuid[])`, makers)
		if err != nil {
			return crossingDecision{}, dbErr("crossing makers", err)
		}
		for rows.Next() {
			var oid string
			var t OwnerType
			if err := rows.Scan(&oid, &t); err != nil {
				rows.Close()
				return crossingDecision{}, dbErr("crossing makers", err)
			}
			types[oid] = t
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return crossingDecision{}, dbErr("crossing makers", err)
		}
	}
	var crossed []string
	for _, m := range makers {
		t, ok := types[m]
		if !ok {
			return crossingDecision{}, errs.New(errs.CodeInternal, "orders: a resting order is missing from the database").WithField("order_id", m)
		}
		if t != takerType {
			d.Trades++
			crossed = append(crossed, m)
		}
	}
	d.WouldCross = d.Trades > 0
	if !d.Possible {
		d.Allowed = true
		return d, nil
	}
	rule, _, err := s.crossingRule(ctx, tx, c.ID)
	if err != nil {
		return crossingDecision{}, err
	}
	d.RuleVersion = rule.Version
	d.Allowed = !d.WouldCross || rule.AllowHouseCustomerCrossing
	decisionID := id.New[id.Any]()
	if crossed == nil {
		crossed = []string{}
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "orders/book/" + c.ID.String() + "/crossing/" + decisionID.String(),
		Document: map[string]any{
			"kind": "orders.crossing_decision.v1", "book_id": c.ID.String(), "instrument_id": c.InstrumentID.String(),
			"order_id": orderID.String(), "command_kind": string(kind), "taker_owner_type": string(takerType),
			"would_cross": d.WouldCross, "crossing_trades": d.Trades, "crossed_maker_orders": crossed,
			"allowed": d.Allowed, "rule_version": d.RuleVersion, "rule_allows_crossing": rule.AllowHouseCustomerCrossing,
			"default_policy": "a house book never trades with a customer unless a versioned crossing rule allows it",
			"decided_at":     s.clk.Now().UTC(),
		},
	})
	if err != nil {
		return crossingDecision{}, err
	}
	d.Evidence = ev
	if _, err := tx.Exec(ctx, `INSERT INTO book_crossing_decisions (id, book_id, order_id, command_kind, taker_owner_type,
		would_cross, allowed, rule_version, crossing_trades, evidence_ref, evidence_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		decisionID, c.ID, orderID, kind, takerType, d.WouldCross, d.Allowed, d.RuleVersion, d.Trades, ev.URI, ev.SHA256); err != nil {
		return crossingDecision{}, dbErr("record crossing decision", err)
	}
	return d, nil
}

// ruleVersionFor is the rule version a command's cross-class trades carry.
func (d crossingDecision) ruleVersionFor() int {
	if d.WouldCross && d.Allowed {
		return d.RuleVersion
	}
	return 0
}
