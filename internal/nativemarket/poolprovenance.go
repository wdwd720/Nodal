package nativemarket

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The pooled reserve's provenance (D-124, F-230).
//
// A native market's automated maker holds Credits that traders paid in. On a
// SELL they come back out, and there is no transaction-local answer to "whose
// Credits were those" -- the pool is fungible.
//
// Without an answer this package minted every sale's proceeds REVERSIBLE
// unconditionally, which meant no MARKET_TRADING_PROCEEDS could ever be
// withdrawn on any deployment, because the only writer that promotes a lot out
// of REVERSIBLE reads `credit_fundings.lot_id` and an earning has no funding
// row. The opposite default would be worse: proceeds that are payout-eligible
// because nobody checked is the laundering route the finality model exists to
// close -- buy with card-funded Credits, sell back, withdraw, charge back.
//
// So the pool keeps the record the ledger keeps for everybody else.
// `native_market_credit_sources` holds one row per lot paid into this market's
// reserve, and the sell side draws them down WORST FIRST. The parents of a
// sale's proceeds are exactly the rows it drew down.
//
// # Why worst first, and what it costs (D-132, F-262)
//
// It was arrival order, which is the intuitive answer and the wrong one. A pool
// is fungible, so "whose Credits left" is a choice rather than a fact, and FIFO
// makes the choice that hands a seller the BEST provenance the pool happens to
// be holding:
//
//	A pays in 40,000 settled Credits. B pays in 40,000 a card issuer can still
//	take back, sells back less than A put in, and is minted proceeds funded by
//	A's settled contribution -- payout-eligible at birth. B's own reversible
//	Credits stay in the pool, waiting to be handed to whoever sells next.
//
// That is the laundering route D-124 says the whole finality model exists to
// close, reached through somebody else's money instead of through the seller's
// own. So the draw-down is ordered by how BAD a contribution is: most
// restricted origin floor first, then least final, then oldest. A seller can
// never be handed provenance better than the pool's worst outstanding
// contribution.
//
// The cost is real and is not hidden: an honest seller paying into a pool that
// still holds somebody else's reversible contribution receives REVERSIBLE
// proceeds until that contribution settles, and somebody else's promotional
// grant in the pool gives them a promotional ORIGIN FLOOR, which no policy in
// this build releases. `credit.Service.SettleDerived` promotes the first when
// the contribution settles; the second does not move, because an origin floor
// is inherited with finality (D-131).
//
// Fail closed is the architecture's rule and this is what it looks like when it
// is inconvenient. The alternative -- pro-rata provenance, where a sale draws a
// slice of every outstanding contribution -- gives every seller a floor as bad
// as the pool's worst anyway AND multiplies the parent rows by the number of
// contributors, so it is worse in both directions.

// recordPoolSources records what a buy paid into the market's reserve.
//
// `into` is what actually reached the pool, which is less than the trader spent:
// the platform's fee and the creator's are taken out of the same movement and
// are their own derived lots. The trader's consumed lots are apportioned across
// it, so the sum of the rows this writes is exactly what the pool received.
func (s *Service) recordPoolSources(
	ctx context.Context, tx pgx.Tx, marketID MarketID, spent []credit.Allocation, into money.Quantity,
) error {
	if !into.IsPositive() || len(spent) == 0 {
		return nil
	}
	shares, err := credit.ParentsOfAllocations(spent, into)
	if err != nil {
		return err
	}
	for _, sh := range shares {
		if _, err := tx.Exec(ctx,
			`INSERT INTO native_market_credit_sources (id, market_id, lot_id, quantity, remaining)
			 VALUES ($1,$2,$3,$4::numeric,$4::numeric)`,
			uuid.New(), marketID, sh.LotID, sh.Quantity.String()); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// drawPoolSources takes `amount` out of the market's recorded reserve, WORST
// contribution first, and returns the lots it drew down.
//
// `covered` is how much of `amount` the record accounted for. A shortfall is
// not an error and is not rounded away: a market that traded before this record
// existed has Credits in it whose provenance nobody wrote down, and the caller
// mints the uncovered part REVERSIBLE, because "we do not know what funded
// this" is not "it was settled".
func (s *Service) drawPoolSources(
	ctx context.Context, tx pgx.Tx, marketID MarketID, amount money.Quantity,
) (parents []credit.LotParent, covered money.Quantity, err error) {
	if !amount.IsPositive() {
		return nil, money.Quantity{}, nil
	}
	// Read and LOCKED in arrival order, which is deliberate and is not the
	// draw-down order below: two concurrent sales against one market must take
	// the same row locks in the same sequence or they deadlock, and arrival
	// order is the one ordering that cannot change while a transaction is
	// running. The order value leaves in is decided afterwards, in memory, over
	// the rows this already holds.
	rows, qerr := tx.Query(ctx,
		`SELECT src.id, src.lot_id, src.remaining::text, st.finality, st.origin_floor
		   FROM native_market_credit_sources src
		   JOIN credit_lot_state st ON st.lot_id = src.lot_id
		  WHERE src.market_id = $1 AND src.remaining > 0
		  ORDER BY src.created_at, src.id
		  FOR UPDATE OF src`, marketID)
	if qerr != nil {
		return nil, money.Quantity{}, mapError(qerr)
	}
	type open struct {
		id        uuid.UUID
		lot       credit.LotID
		seq       int
		remaining money.Quantity
		finality  valuedomain.FundingFinality
		floor     valuedomain.CreditOrigin
	}
	var sources []open
	for rows.Next() {
		var (
			o   open
			raw string
		)
		if serr := rows.Scan(&o.id, &o.lot, &raw, &o.finality, &o.floor); serr != nil {
			rows.Close()
			return nil, money.Quantity{}, mapError(serr)
		}
		o.seq = len(sources)
		v, perr := money.ParseQuantity(raw)
		if perr != nil {
			rows.Close()
			return nil, money.Quantity{}, errs.Wrap(perr, errs.CodeInternal,
				"nativemarket: a pooled credit source is not an integer")
		}
		o.remaining = v
		sources = append(sources, o)
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return nil, money.Quantity{}, mapError(rerr)
	}

	// Worst first (D-132). Most restricted origin floor, then least final, then
	// oldest -- `seq` is the arrival order the query returned, so the last key
	// is the FIFO the first two override rather than a re-read of the clock.
	sort.SliceStable(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		if a.floor != b.floor {
			return valuedomain.MoreRestricted(a.floor, b.floor)
		}
		if a.finality != b.finality {
			return valuedomain.LessFinal(a.finality, b.finality)
		}
		return a.seq < b.seq
	})

	remaining := amount
	byLot := map[credit.LotID]int{}
	for _, src := range sources {
		if !remaining.IsPositive() {
			break
		}
		take := src.remaining.Min(remaining)
		if !take.IsPositive() {
			continue
		}
		if _, uerr := tx.Exec(ctx,
			`UPDATE native_market_credit_sources SET remaining = remaining - $2::numeric WHERE id = $1`,
			src.id, take.String()); uerr != nil {
			return nil, money.Quantity{}, mapError(uerr)
		}
		if at, ok := byLot[src.lot]; ok {
			parents[at].Quantity = parents[at].Quantity.Add(take)
		} else {
			byLot[src.lot] = len(parents)
			parents = append(parents, credit.LotParent{
				LotID: src.lot, Quantity: take, Finality: src.finality,
			})
		}
		remaining = remaining.Sub(take)
		covered = covered.Add(take)
	}
	return parents, covered, nil
}

// sharesOf apportions a derived amount across the parents a draw-down found,
// so proceeds and the creator's fee each name the lots that funded THEM rather
// than the whole movement.
func sharesOf(parents []credit.LotParent, amount money.Quantity) ([]credit.LotParent, error) {
	if len(parents) == 0 || !amount.IsPositive() {
		return nil, nil
	}
	allocs := make([]credit.Allocation, 0, len(parents))
	for _, p := range parents {
		allocs = append(allocs, credit.Allocation{
			LotID: p.LotID, Quantity: p.Quantity, Finality: p.Finality,
		})
	}
	return credit.ParentsOfAllocations(allocs, amount)
}
