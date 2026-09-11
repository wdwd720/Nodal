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
//   - some parent is DISPUTED or REVERSED, so the freeze branch will. A lot
//     already DISPUTED is not REVERSIBLE or SETTLED, so it is out of the set by
//     the first clause and the freeze direction converges too.
//
// And never a lot with a `credit_fundings` row. Those belong to SettleFunding,
// which keys on exactly that column; a funded lot that acquired a parent row is
// either the mint of an externally funded purchase or the forgery F-266 was, and
// in neither case is this sweep the thing that should move it.
//
// The two finality lists are passed in from valuedomain rather than written as
// SQL literals, so `PayoutEligible()` and this query cannot come to disagree.
const settleDerivedCandidates = `
	SELECT st.lot_id, st.finality
	  FROM credit_lot_state st
	 WHERE st.finality IN ('REVERSIBLE','SETTLED')
	   AND EXISTS (SELECT 1 FROM credit_lot_parents p WHERE p.lot_id = st.lot_id)
	   AND NOT EXISTS (SELECT 1 FROM credit_fundings f WHERE f.lot_id = st.lot_id)
	   AND (
	        (st.finality = 'REVERSIBLE'
	         AND NOT EXISTS (
	             SELECT 1 FROM credit_lot_parents p
	               JOIN credit_lot_state ps ON ps.lot_id = p.parent_lot_id
	              WHERE p.lot_id = st.lot_id
	                AND NOT (ps.finality = ANY($2::text[]))))
	     OR EXISTS (
	             SELECT 1 FROM credit_lot_parents p
	               JOIN credit_lot_state ps ON ps.lot_id = p.parent_lot_id
	              WHERE p.lot_id = st.lot_id
	                AND ps.finality = ANY($3::text[]))
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

// frozenFinalities are the ones that freeze a lot derived from them. They are
// the two SettleDerived's freeze branch acts on, named once here and read by
// the predicate that decides which lots it is worth looking at.
func frozenFinalities() []string {
	return []string{string(valuedomain.FinalityDisputed), string(valuedomain.FinalityReversed)}
}

// SettleDerivedResult is what one pass of SettleDerived did.
type SettleDerivedResult struct {
	// Promoted is how many derived lots reached a payout-eligible finality
	// because every parent had.
	Promoted int
	// Frozen is how many were moved to DISPUTED because a parent was disputed
	// or reversed.
	Frozen int
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
//   - a REVERSIBLE or SETTLED derived lot with a DISPUTED or REVERSED parent is
//     moved to DISPUTED, which is neither spendable nor payout-eligible. That
//     is as far as the ledger can follow a reversal: taking value back from a
//     third party who earned it and may have spent it is a posting kind this
//     ledger does not have, and D-124 records the residual rather than
//     inventing one.
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
		limit, payoutEligibleFinalities(), frozenFinalities())
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
		case worst == valuedomain.FinalityDisputed || worst == valuedomain.FinalityReversed:
			if c.from == valuedomain.FinalityDisputed {
				continue
			}
			if err := s.SetFinality(ctx, tx, c.id, valuedomain.FinalityDisputed,
				Reference{Type: "credit_lot_parents", ID: c.id.String()},
				"a lot this one was derived from is disputed or reversed"); err != nil {
				return out, err
			}
			out.Frozen++
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
