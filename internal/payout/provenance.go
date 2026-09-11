package payout

import (
	"context"
	"sort"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Provenance through trading (goal §23).
//
// A payout does not take "500 Credits". It takes specific units from specific
// provenance lots, in a defined order, and `payout_allocations` already records
// exactly which. What was missing was a way to SHOW that: a person pressing
// Withdraw is entitled to know what value is leaving, and "500 Credits" does
// not answer it any more than "18,450 Credits" answers "how much may I
// withdraw".
//
// This file is a read model over rows that already exist. It writes nothing,
// changes no ledger semantics, and re-derives no eligibility: the allocations
// are the record of what the engine decided, and this reads them back in the
// order they were consumed.
//
// # The order is the consumption order, and it is not "purchased first"
//
// `credit.ConsumptionRank` orders origins from MOST restricted to least:
// promotional, competition reward, admin adjustment, refund, purchased,
// provider settlement, then the earnings. That is a structural property of what
// the value IS rather than of what a policy currently permits, and
// `internal/credit`'s own comment gives the reason: ordering by current payout
// eligibility would make the same spend consume different lots before and after
// a policy change, so a provenance question asked twice could get two answers.
//
// The consequence for a withdrawal is worth stating plainly, because it is the
// opposite of the intuition in §23's sketch: a payout draws its ELIGIBLE lots
// in that same order, so among the origins a policy permits, the most
// restricted permitted one leaves first. Purchased value leaves before earnings
// do; promotional value never leaves at all under any policy that forbids it,
// because the engine never selects it. Nothing here changes that order — it
// reports it.

// ProvenanceSlice is one provenance's contribution to a payout.
//
// A provenance is an ORIGIN AND A FLOOR, not an origin. Folding by origin alone
// reported a payout drawn on trading proceeds out of a settled purchase and one
// drawn on trading proceeds out of a promotional grant as one line of
// MARKET_TRADING_PROCEEDS -- which is the same collapse that let the
// reservation take the wrong lot, one surface along (D-136, F-270).
type ProvenanceSlice struct {
	Origin valuedomain.CreditOrigin
	// OriginFloor is the most restricted origin in the provenance of the units
	// in this slice. It equals Origin for value nothing else funded.
	OriginFloor valuedomain.CreditOrigin
	// Quantity is the base-unit amount of this provenance the payout takes.
	Quantity money.Quantity
	// ConsumptionRank is where this origin sits in the consumption order.
	// Lower leaves first. It is reported so a client can render the order
	// without re-implementing the ranking.
	ConsumptionRank int
	// Returned is true for a slice that was given back — a cancelled payout
	// returns exactly what it took, to the exact lots. It is reported
	// separately rather than filtered out, so "what happened to my money" has
	// an answer after a cancellation.
	Returned bool
}

// Provenance returns what value a payout draws on, in consumption order.
//
// It reads `payout_allocations`, which is the engine's own record of which lots
// were reserved. A request that has not reserved yet has no allocations and
// therefore no provenance: use DecisionProvenance for what a decision WOULD
// take.
func (s *Service) Provenance(ctx context.Context, q db.Querier, id RequestID) ([]ProvenanceSlice, error) {
	allocations, err := s.allocations(ctx, q, id, true)
	if err != nil {
		return nil, err
	}
	return foldProvenance(allocations), nil
}

// foldProvenance sums allocations per (origin, floor) and orders them by
// consumption rank, with the floor and then the origin name breaking a tie so
// the answer is deterministic.
//
// Outstanding and returned slices are kept apart: folding them together would
// report a cancelled payout as though it still held the value.
func foldProvenance(allocations []Allocation) []ProvenanceSlice {
	type key struct {
		origin   valuedomain.CreditOrigin
		floor    valuedomain.CreditOrigin
		returned bool
	}
	sums := map[key]money.Quantity{}
	for _, a := range allocations {
		k := key{origin: a.Origin, floor: a.OriginFloor, returned: a.Returned}
		sums[k] = sums[k].Add(a.Quantity)
	}
	out := make([]ProvenanceSlice, 0, len(sums))
	for k, qty := range sums {
		out = append(out, ProvenanceSlice{
			Origin:          k.origin,
			OriginFloor:     k.floor,
			Quantity:        qty,
			ConsumptionRank: credit.ConsumptionRank(k.origin),
			Returned:        k.returned,
		})
	}
	sortProvenance(out)
	return out
}

// DecisionProvenance is what an eligibility Decision WOULD take, in the same
// order, before anything is reserved.
//
// It is what a quote and a refused request show: §19's "show eligible /
// ineligible value" needs an answer before the customer commits, and a decision
// that reserved nothing still knows which lots it selected.
func DecisionProvenance(d Decision) []ProvenanceSlice {
	type key struct {
		origin valuedomain.CreditOrigin
		floor  valuedomain.CreditOrigin
	}
	remaining := d.Eligible
	sums := map[key]money.Quantity{}
	for _, lot := range d.Lots {
		if !remaining.IsPositive() {
			break
		}
		take := lot.Remaining.Min(remaining)
		if !take.IsPositive() {
			continue
		}
		k := key{origin: lot.Origin, floor: lot.OriginFloor}
		sums[k] = sums[k].Add(take)
		remaining = remaining.Sub(take)
	}
	out := make([]ProvenanceSlice, 0, len(sums))
	for k, qty := range sums {
		out = append(out, ProvenanceSlice{
			Origin:          k.origin,
			OriginFloor:     k.floor,
			Quantity:        qty,
			ConsumptionRank: credit.ConsumptionRank(k.origin),
		})
	}
	sortProvenance(out)
	return out
}

// sortProvenance orders slices the way the engine consumes them: outstanding
// before returned, then by consumption rank, then by the most restricted floor,
// then by origin name.
//
// The floor is the third key rather than a display afterthought: two slices of
// one origin differ only in it, and an order that left them in map order would
// make one payout's provenance render differently on two reads.
func sortProvenance(in []ProvenanceSlice) {
	sort.SliceStable(in, func(i, j int) bool {
		switch {
		case in[i].Returned != in[j].Returned:
			return !in[i].Returned
		case in[i].ConsumptionRank != in[j].ConsumptionRank:
			return in[i].ConsumptionRank < in[j].ConsumptionRank
		case in[i].OriginFloor != in[j].OriginFloor:
			return valuedomain.MoreRestricted(in[i].OriginFloor, in[j].OriginFloor)
		default:
			return in[i].Origin < in[j].Origin
		}
	})
}
