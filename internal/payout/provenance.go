package payout

import (
	"context"
	"sort"
	"strings"

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
// A provenance is an ORIGIN, A FLOOR AND A ROOT SET, not an origin. Folding by
// origin alone reported a payout drawn on trading proceeds out of a settled
// purchase and one drawn on trading proceeds out of a promotional grant as one
// line of MARKET_TRADING_PROCEEDS -- which is the same collapse that let the
// reservation take the wrong lot, one surface along (D-136, F-270).
//
// Folding by (origin, floor) had the same shape one step further in. The floor
// is the most restricted ROOT, so two different root sets share a floor
// whenever they share a minimum -- {CREATOR_EARNING} and {CREATOR_EARNING,
// PURCHASED} both floor at CREATOR_EARNING -- and D-138 made the permission
// itself read the whole set, because a policy can refuse a root this build's
// rank does not call the most restricted. Under the policy B-02 can come back
// with, one of those two may leave and the other may not, and they were
// reported as one value (D-141, F-282).
type ProvenanceSlice struct {
	Origin valuedomain.CreditOrigin
	// OriginFloor is the most restricted origin in the provenance of the units
	// in this slice. It equals Origin for value nothing else funded.
	OriginFloor valuedomain.CreditOrigin
	// RootOrigins is every origin the units in this slice bottom out in. It is
	// what the policy reads; the floor is the most restricted of it.
	RootOrigins []valuedomain.CreditOrigin
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

// foldProvenance sums allocations per (origin, floor, root set) and orders them
// by consumption rank, with the floor, the origin name and then the set
// breaking a tie so the answer is deterministic.
//
// Outstanding and returned slices are kept apart: folding them together would
// report a cancelled payout as though it still held the value.
func foldProvenance(allocations []Allocation) []ProvenanceSlice {
	type key struct {
		origin   valuedomain.CreditOrigin
		floor    valuedomain.CreditOrigin
		roots    string
		returned bool
	}
	sums := map[key]money.Quantity{}
	sets := map[key][]valuedomain.CreditOrigin{}
	for _, a := range allocations {
		k := key{
			origin: a.Origin, floor: a.OriginFloor,
			roots: rootKey(a.RootOrigins), returned: a.Returned,
		}
		sums[k] = sums[k].Add(a.Quantity)
		sets[k] = a.RootOrigins
	}
	out := make([]ProvenanceSlice, 0, len(sums))
	for k, qty := range sums {
		out = append(out, ProvenanceSlice{
			Origin:          k.origin,
			OriginFloor:     k.floor,
			RootOrigins:     sets[k],
			Quantity:        qty,
			ConsumptionRank: credit.ConsumptionRank(k.origin),
			Returned:        k.returned,
		})
	}
	sortProvenance(out)
	return out
}

// rootKey is a root set as a map key: canonical, so two lots with the same
// provenance fold together however their arrays were ordered on the way in.
func rootKey(roots []valuedomain.CreditOrigin) string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, string(r))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// originsOf converts a stored root-origin array. A row with none is returned as
// nil rather than as an empty non-nil slice, so a caller cannot tell "no roots"
// from "roots I did not read": `Policy.Permits` refuses both.
func originsOf(in []string) []valuedomain.CreditOrigin {
	if len(in) == 0 {
		return nil
	}
	out := make([]valuedomain.CreditOrigin, 0, len(in))
	for _, o := range in {
		out = append(out, valuedomain.CreditOrigin(o))
	}
	return out
}

// originStrings is the inverse, for the write. Nil stays nil, so an allocation
// with no recorded provenance is refused by the column's NOT NULL rather than
// written as an empty set that would read as "we looked and there was nothing".
func originStrings(in []valuedomain.CreditOrigin) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, o := range in {
		out = append(out, string(o))
	}
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
		roots  string
	}
	remaining := d.Eligible
	sums := map[key]money.Quantity{}
	sets := map[key][]valuedomain.CreditOrigin{}
	for _, lot := range d.Lots {
		if !remaining.IsPositive() {
			break
		}
		take := lot.Remaining.Min(remaining)
		if !take.IsPositive() {
			continue
		}
		k := key{origin: lot.Origin, floor: lot.OriginFloor, roots: rootKey(lot.RootOrigins)}
		sums[k] = sums[k].Add(take)
		sets[k] = lot.RootOrigins
		remaining = remaining.Sub(take)
	}
	out := make([]ProvenanceSlice, 0, len(sums))
	for k, qty := range sums {
		out = append(out, ProvenanceSlice{
			Origin:          k.origin,
			OriginFloor:     k.floor,
			RootOrigins:     sets[k],
			Quantity:        qty,
			ConsumptionRank: credit.ConsumptionRank(k.origin),
		})
	}
	sortProvenance(out)
	return out
}

// sortProvenance orders slices the way the engine consumes them: outstanding
// before returned, then by consumption rank, then by the most restricted floor,
// then by origin name, then by the root set.
//
// The floor is the third key rather than a display afterthought: two slices of
// one origin differ only in it, and an order that left them in map order would
// make one payout's provenance render differently on two reads. The set is the
// last key for the same reason one step further in -- two slices can share a
// floor and differ in what is behind it (D-141, F-282).
func sortProvenance(in []ProvenanceSlice) {
	sort.SliceStable(in, func(i, j int) bool {
		switch {
		case in[i].Returned != in[j].Returned:
			return !in[i].Returned
		case in[i].ConsumptionRank != in[j].ConsumptionRank:
			return in[i].ConsumptionRank < in[j].ConsumptionRank
		case in[i].OriginFloor != in[j].OriginFloor:
			return valuedomain.MoreRestricted(in[i].OriginFloor, in[j].OriginFloor)
		case in[i].Origin != in[j].Origin:
			return in[i].Origin < in[j].Origin
		default:
			return rootKey(in[i].RootOrigins) < rootKey(in[j].RootOrigins)
		}
	})
}
