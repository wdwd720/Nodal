package credit

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Derived lots: value minted out of value that was already inside the system
// (D-124, F-230).
//
// A funded lot gets its finality from a `credit_fundings` row that some
// external payment moved through. A DERIVED lot -- trading proceeds, a creator
// earning, marketplace proceeds, the platform's fee on them -- has no such row
// and never will, which is why `SettleFunding` could not reach one and why every
// earning in this system was stranded at REVERSIBLE with nothing able to move
// it.
//
// What a derived lot has instead is PARENTS: the lots consumed to fund it,
// recorded at mint in the same transaction as the consumption. Its finality is
// the least final among them, because proceeds cannot be more final than the
// money behind them and must not be less final either -- a payout the platform
// refuses on value that is actually settled is a promise broken in the other
// direction.

// LotParent is one lot that funded another, and how much of it did.
type LotParent struct {
	LotID    LotID
	Quantity money.Quantity
	Finality valuedomain.FundingFinality
}

// ParentsOfAllocations turns the allocations a Consume returned into the
// parents of a derived lot of `derived` units.
//
// The consumed total is usually larger than the derived quantity -- a
// marketplace purchase of 100 funds proceeds of 90 and a fee of 10 -- so each
// parent's share is apportioned in proportion to what it contributed, with the
// rounding remainder going to the last parent so the shares add up exactly.
//
// A parent that would round to zero is still recorded, with one unit, because
// the finality rule reads WHICH parents funded a lot and dropping the smallest
// would drop exactly the reversible one somebody was hoping nobody would notice.
func ParentsOfAllocations(allocs []Allocation, derived money.Quantity) ([]LotParent, error) {
	if len(allocs) == 0 || !derived.IsPositive() {
		return nil, nil
	}
	total := money.Quantity{}
	for _, a := range allocs {
		total = total.Add(a.Quantity)
	}
	if !total.IsPositive() {
		return nil, nil
	}
	out := make([]LotParent, 0, len(allocs))
	assigned := money.Quantity{}
	for i, a := range allocs {
		share := derived
		if i < len(allocs)-1 {
			s, err := a.Quantity.MulDiv(derived, total, money.RoundDown)
			if err != nil {
				return nil, errs.Wrap(err, errs.CodeInternal, "credit: apportion a derived lot across its parents")
			}
			share = s
			if !share.IsPositive() {
				share = money.QuantityFromInt64(1)
			}
			if share.Cmp(derived.Sub(assigned)) > 0 {
				share = derived.Sub(assigned)
			}
		} else {
			share = derived.Sub(assigned)
		}
		if !share.IsPositive() {
			continue
		}
		assigned = assigned.Add(share)
		out = append(out, LotParent{LotID: a.LotID, Quantity: share, Finality: a.Finality})
	}
	return out, nil
}

// DerivedFinality is the least final finality among a set of parents.
//
// With no parents at all it is REVERSIBLE, and that is the important case
// rather than a fallback: "nothing tells us what funded this" is not "it was
// settled". A derived lot minted payout-eligible by default is the laundering
// route the whole finality model exists to close -- buy with card-funded
// Credits, convert them into an earning, withdraw, charge back.
func DerivedFinality(parents []LotParent) valuedomain.FundingFinality {
	if len(parents) == 0 {
		return valuedomain.FinalityReversible
	}
	worst := parents[0].Finality
	for _, p := range parents[1:] {
		if valuedomain.LessFinal(p.Finality, worst) {
			worst = p.Finality
		}
	}
	return worst
}

// recordParents writes the parent rows for a lot. It is called from RecordLot,
// inside the caller's transaction, so a lot and its provenance land together or
// neither does.
func (s *Service) recordParents(ctx context.Context, tx pgx.Tx, lotID LotID, parents []LotParent) error {
	// Sorted so two identical mints write their rows in the same order and
	// concurrent writers take the same locks in the same order.
	sorted := append([]LotParent(nil), parents...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].LotID.String() < sorted[j].LotID.String() })
	for _, p := range sorted {
		if p.LotID == lotID {
			return errs.New(errs.CodeValidationFailed, "credit: a lot cannot be its own parent").
				WithField("lot_id", lotID.String())
		}
		if !p.Quantity.IsPositive() {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity)
			 VALUES ($1,$2,$3::numeric)
			 ON CONFLICT (lot_id, parent_lot_id) DO NOTHING`,
			lotID, p.LotID, p.Quantity.String()); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// ParentsOf reads a lot's parents with their CURRENT finalities.
func (s *Service) ParentsOf(ctx context.Context, q db.Querier, lotID LotID) ([]LotParent, error) {
	rows, err := q.Query(ctx,
		`SELECT p.parent_lot_id, p.quantity::text, st.finality
		   FROM credit_lot_parents p
		   JOIN credit_lot_state st ON st.lot_id = p.parent_lot_id
		  WHERE p.lot_id = $1
		  ORDER BY p.parent_lot_id`, lotID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []LotParent
	for rows.Next() {
		var (
			p   LotParent
			qty string
		)
		if err := rows.Scan(&p.LotID, &qty, &p.Finality); err != nil {
			return nil, mapError(err)
		}
		v, perr := money.ParseQuantity(qty)
		if perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "credit: parent quantity is not an integer")
		}
		p.Quantity = v
		out = append(out, p)
	}
	return out, mapError(rows.Err())
}

// settleDerivedCandidates selects the derived lots that CAN MOVE on this pass.
//
// The predicate used to be `finality IN ('REVERSIBLE','SETTLED') AND EXISTS (a
// parent row)`, which never shrank: a lot this pass promoted went REVERSIBLE ->
// SETTLED and stayed in it, and a lot that was payout-eligible at birth was
// never out of it. With `ORDER BY st.lot_id` on UUIDv7s -- which are
// chronological -- the sweep returned the OLDEST `limit` derived lots on every
// pass, for ever. cmd/api runs it at 100, so the hundred-and-first derived lot a
// deployment ever minted was never examined again, whatever happened to its
// parents. That is F-230's outcome restored by the sweep written to fix it, at a
// volume any real deployment passes in its first week (F-260).
//
// Now a lot is a candidate only if this pass would do something to it:
//
//   - it is REVERSIBLE and NO parent is below a payout-eligible finality, so
//     the promotion branch will move it; or
//   - some parent is DISPUTED or REVERSED and the lot is not already one of
//     those itself, so the freeze branch will; or
//   - it is frozen at a finality it can legally leave and NO parent is frozen
//     any more, so the THAW branch will (D-140).
//
// The third clause is the mirror the freeze never had. A derived lot frozen
// because its funding was disputed matched neither of the other two once it
// moved -- the promotion clause opens `st.finality = 'REVERSIBLE'` and a frozen
// lot is not, the freeze clause opens "not already frozen" and a frozen lot is
// -- and nothing else in this system can move a lot with no `credit_fundings`
// row. So a dispute the platform WON left the earning derived from that funding
// at DISPUTED for ever: neither spendable nor payout-eligible, with the
// eligibility page telling its holder that waiting would fix it (F-278).
//
// Which frozen finalities it opens is `thawableFinalities()`, read out of the
// transition table rather than written here: REVERSED is terminal and a lot
// whose funding was actually taken back stays frozen for ever, by design.
//
// The freeze clause used to sit under an outer `st.finality IN
// ('REVERSIBLE','SETTLED')`, which is a THIRD statement of which lots can be
// frozen and it did not match the other two. A derived lot is minted at the
// least final finality among its parents, and UNFUNDED sits between REVERSIBLE
// and SETTLED -- so an earning funded by a grant and a settled card purchase is
// minted UNFUNDED and was outside the predicate for ever, whatever happened to
// the card (F-273). The freeze clause now says what the branch says: any
// derived lot with a frozen parent that is not itself frozen. The PROMOTION
// clause keeps its REVERSIBLE test, because promotion out of UNFUNDED would be
// value inventing a backer.
//
// And never a lot with a `credit_fundings` row. Those belong to SettleFunding,
// which keys on exactly that column; a funded lot that acquired a parent row is
// either the mint of an externally funded purchase or the forgery F-266 was, and
// in neither case is this sweep the thing that should move it.
//
// Both finality lists are passed in from valuedomain rather than written as SQL
// literals, so `PayoutEligible()`, `Frozen()` and this query cannot come to
// disagree.
const settleDerivedCandidates = `
	SELECT st.lot_id, st.finality
	  FROM credit_lot_state st
	 WHERE EXISTS (SELECT 1 FROM credit_lot_parents p WHERE p.lot_id = st.lot_id)
	   AND NOT EXISTS (SELECT 1 FROM credit_fundings f WHERE f.lot_id = st.lot_id)
	   AND (
	        (st.finality = 'REVERSIBLE'
	         AND NOT EXISTS (
	             SELECT 1 FROM credit_lot_parents p
	               JOIN credit_lot_state ps ON ps.lot_id = p.parent_lot_id
	              WHERE p.lot_id = st.lot_id
	                AND NOT (ps.finality = ANY($2::text[]))))
	     OR (NOT (st.finality = ANY($3::text[]))
	         AND EXISTS (
	             SELECT 1 FROM credit_lot_parents p
	               JOIN credit_lot_state ps ON ps.lot_id = p.parent_lot_id
	              WHERE p.lot_id = st.lot_id
	                AND ps.finality = ANY($3::text[])))
	     OR (st.finality = ANY($4::text[])
	         AND NOT EXISTS (
	             SELECT 1 FROM credit_lot_parents p
	               JOIN credit_lot_state ps ON ps.lot_id = p.parent_lot_id
	              WHERE p.lot_id = st.lot_id
	                AND ps.finality = ANY($3::text[])))
	   )
	 ORDER BY st.lot_id
	 LIMIT $1`

// payoutEligibleFinalities is valuedomain's answer, as a SQL array.
func payoutEligibleFinalities() []string {
	var out []string
	for _, f := range valuedomain.AllFinalities() {
		if f.PayoutEligible() {
			out = append(out, string(f))
		}
	}
	return out
}

// frozenFinalities are the ones that freeze a lot derived from them, as a SQL
// array. The set itself is valuedomain's: the predicate that selects candidates
// and the branch that acts on them read one declaration, because a list written
// twice is how the sweep came to look at REVERSIBLE and SETTLED lots while its
// own comment said DISPUTED and REVERSED (F-273).
func frozenFinalities() []string {
	fs := valuedomain.FrozenFinalities()
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return out
}

// thawableFinalities are the frozen finalities a derived lot can legally LEAVE
// again, as a SQL array: the ones from which the transition table has an edge to
// both SETTLED and REVERSIBLE, which are the two the thaw branch can reach.
//
// It is computed from `finalityTransitions` rather than written down, so the
// clause that selects a lot to thaw and the table that decides whether the move
// is legal cannot come apart. Today it is exactly DISPUTED. REVERSED is not in
// it and must not be: it is terminal, it means the money behind the lot was
// actually taken back, and a lot derived from a reversal stays frozen for ever.
func thawableFinalities() []string {
	var out []string
	for _, f := range valuedomain.FrozenFinalities() {
		if valuedomain.CanTransitionFinality(f, valuedomain.FinalitySettled) &&
			valuedomain.CanTransitionFinality(f, valuedomain.FinalityReversible) {
			out = append(out, string(f))
		}
	}
	return out
}

// SettleDerivedResult is what one pass of SettleDerived did.
type SettleDerivedResult struct {
	// Promoted is how many derived lots reached a payout-eligible finality
	// because every parent had.
	Promoted int
	// Frozen is how many were moved to DISPUTED because a parent was disputed
	// or reversed.
	Frozen int
	// Thawed is how many came back out of a freeze because the dispute behind
	// every frozen parent was resolved in the platform's favour (D-140).
	Thawed int
}

// SettleDerived is the pass that moves derived lots when their parents move.
//
// It is the counterpart of SettleFunding for value nothing external funded, and
// it exists because without it no earning in this system could ever be
// withdrawn: SettleFunding keys on `credit_fundings.lot_id`, a derived lot has
// no funding row, and `PayoutEligible()` admits only SETTLED and UNFUNDED
// (F-230).
//
// Two directions, and both are conservative:
//
//   - a REVERSIBLE derived lot whose parents have ALL reached a payout-eligible
//     finality is promoted to SETTLED. One parent short and nothing moves.
//
//   - a derived lot with a DISPUTED or REVERSED parent, at ANY finality that is
//     not already one of those, is moved to DISPUTED, which is neither
//     spendable nor payout-eligible. That is as far as the ledger can follow a
//     reversal: taking value back from a third party who earned it and may have
//     spent it is a posting kind this ledger does not have, and D-124 records
//     the residual rather than inventing one.
//
//     "At any finality" includes UNFUNDED, which is where a derived lot lands
//     whenever its least final parent is a grant. That case was unreachable
//     until D-124's amendment: the predicate looked only at REVERSIBLE and
//     SETTLED lots, and the finality table called UNFUNDED terminal, so an
//     earning funded by a grant and a charged-back card stayed spendable for
//     ever (F-273).
//
//   - a frozen derived lot NONE of whose parents is frozen any more is
//     thawed: to SETTLED when every parent is payout-eligible, to REVERSIBLE
//     when one of them is still inside a dispute window. It is the mirror
//     D-094 promised for funded value and D-124 never built for derived value,
//     and without it winning a dispute unfroze the card and stranded the
//     earning (D-140, F-278).
//
//     The target is what `DerivedFinality` says now, mapped onto the two edges
//     DISPUTED actually has. A lot minted UNFUNDED comes back SETTLED rather
//     than UNFUNDED because there is no DISPUTED -> UNFUNDED edge and inventing
//     one would give this sweep a way to un-fund value; the two are the same
//     answer to every reader that matters -- `Spendable()` and
//     `PayoutEligible()` admit both -- and SETTLED is the one that is true of a
//     lot whose whole provenance has finished moving.
//
//     A lot with a REVERSED parent is never selected: REVERSED is terminal, so
//     that parent is frozen for ever and the clause that opens this branch
//     requires no parent to be frozen. That is the design and not an oversight
//     -- the value behind it was taken back -- and it is why the thaw cannot
//     become a laundering route.
//
// Batch-bounded, and each lot is taken under the same advisory lock SetFinality
// uses, tried rather than waited for -- so two tickers racing skip past each
// other instead of blocking, and neither holds a long transaction over the lot
// tables. A row lock is not available here: `credit_lot_state` is a
// trigger-written projection the application may only SELECT, and
// `SELECT ... FOR UPDATE` needs UPDATE privilege.
func (s *Service) SettleDerived(ctx context.Context, tx pgx.Tx, limit int) (SettleDerivedResult, error) {
	var out SettleDerivedResult
	if tx == nil {
		return out, errs.New(errs.CodeInternal, "credit: SettleDerived requires a transaction")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := tx.Query(ctx, settleDerivedCandidates,
		limit, payoutEligibleFinalities(), frozenFinalities(), thawableFinalities())
	if err != nil {
		return out, mapError(err)
	}
	type candidate struct {
		id   LotID
		from valuedomain.FundingFinality
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.from); err != nil {
			rows.Close()
			return out, mapError(err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, mapError(err)
	}

	for _, c := range candidates {
		held, lerr := tryLockLot(ctx, tx, c.id)
		if lerr != nil {
			return out, lerr
		}
		if !held {
			// Another pass has it. Skipping is the whole point: this sweep is
			// idempotent and the lot will be picked up next time.
			continue
		}
		parents, perr := s.ParentsOf(ctx, tx, c.id)
		if perr != nil {
			return out, perr
		}
		if len(parents) == 0 {
			continue
		}
		worst := DerivedFinality(parents)
		switch {
		case worst.Frozen():
			// Already frozen, by this pass or an earlier one. REVERSED is
			// terminal and DISPUTED is where this branch sends things, so
			// neither is this sweep's to move again.
			if c.from.Frozen() {
				continue
			}
			if err := s.SetFinality(ctx, tx, c.id, valuedomain.FinalityDisputed,
				Reference{Type: "credit_lot_parents", ID: c.id.String()},
				"a lot this one was derived from is disputed or reversed"); err != nil {
				return out, err
			}
			out.Frozen++
		case c.from.Frozen():
			// Nothing above this lot is frozen any more, so the freeze that
			// put it here has been resolved. Back to what its parents say it
			// is, through the one edge the transition table has for it: an
			// illegal move is refused by SetFinality rather than skipped here,
			// because a thaw this sweep cannot express is a fact an operator
			// has to see.
			to := valuedomain.FinalityReversible
			if worst.PayoutEligible() {
				to = valuedomain.FinalitySettled
			}
			if err := s.SetFinality(ctx, tx, c.id, to,
				Reference{Type: "credit_lot_parents", ID: c.id.String()},
				"the dispute that froze the lots this one was derived from has been resolved"); err != nil {
				return out, err
			}
			out.Thawed++
		case c.from == valuedomain.FinalityReversible && worst.PayoutEligible():
			// UNFUNDED is terminal, so a derived lot whose parents are all
			// UNFUNDED is already as final as it can be; SETTLED is the state
			// this pass can reach and the one PayoutEligible admits.
			if err := s.SetFinality(ctx, tx, c.id, valuedomain.FinalitySettled,
				Reference{Type: "credit_lot_parents", ID: c.id.String()},
				"every lot this one was derived from has reached a final funding state"); err != nil {
				return out, err
			}
			out.Promoted++
		}
	}
	return out, nil
}
