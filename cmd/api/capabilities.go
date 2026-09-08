package main

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// gateCapabilityResolver answers "which capabilities are ACTIVE right now" from
// the same gate checker every other capability decision uses.
//
// It exists so that the ledger's value-domain isolation, the payout engine and
// the legal router all read one source. A second source would eventually
// disagree with the first, and the disagreement would be discovered by
// something moving that should not have.
//
// Capability names are the valuedomain.CapabilityKey strings the conversions
// and payout rules refer to. A key that does not correspond to a declared
// gates.Capability is reported INACTIVE rather than treated as an error: an
// unknown capability is not an active one, and failing the whole request
// because a policy mentioned a capability this build has never heard of would
// turn a naming mistake into an outage.
type gateCapabilityResolver struct {
	checker *gates.Checker
	// q is the fallback querier for callers that are NOT inside a transaction.
	// Every caller that IS inside one supplies its own; see F-27.
	q db.Querier
}

// conversionCapabilities are the keys this resolver answers about. They are
// listed rather than derived because the list is the contract: adding a
// conversion to internal/valuedomain without adding its gate here would leave
// the movement permanently refused.
//
// "The safe direction to fail" is how that used to be justified, and it is only
// half true. A capability missing from this list is reported INACTIVE no matter
// what its gate says, so the movement is not merely refused, it is
// UNSATISFIABLE: three principals can approve the gate, an operator can enable
// it in configuration, and the refusal still says CAPABILITY_NOT_ACTIVE with
// nothing to distinguish it from a gate nobody has approved. MARKETPLACE was
// missing exactly that way, and the whole internal marketplace was unreachable
// in every deployment (F-26).
//
// TestCapabilities_ResolverAnswersEverythingTheCompilerCanRequire now compares
// this list against settlement.AllRequiredCapabilities(), so the two cannot
// drift again.
var conversionCapabilities = []valuedomain.CapabilityKey{
	valuedomain.CapNativeMarketTrading,
	valuedomain.CapPayoutReserve,
	valuedomain.CapPayoutSettle,
	valuedomain.CapHostedTrading,
	valuedomain.CapHostedFunding,
	// Not conversions, but the payout policy, the settlement compiler and the
	// agent authority ladder name these and read them from the same resolver,
	// so one call answers every capability question a request can ask.
	valuedomain.CapabilityKey(gates.CreditPurchase),
	valuedomain.CapabilityKey(gates.NativeAssetCreation),
	valuedomain.CapabilityKey(gates.Marketplace),
	valuedomain.CapabilityKey(gates.LiveManualTrading),
	valuedomain.CapabilityKey(gates.AgentBoundedDiscretion),
	valuedomain.CapabilityKey(gates.AgentAutonomousSelection),
	valuedomain.CapabilityKey(gates.AgentAutonomousPortfolio),
}

// gateCaps is conversionCapabilities as gate names, built once.
var gateCaps = func() []gates.Capability {
	out := make([]gates.Capability, 0, len(conversionCapabilities))
	for _, k := range conversionCapabilities {
		out = append(out, gates.Capability(string(k)))
	}
	return out
}()

// ActiveConversionCapabilities is internal/ledger's resolver. The querier is
// the posting's own transaction.
func (r gateCapabilityResolver) ActiveConversionCapabilities(ctx context.Context, q db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return r.on(ctx, q)
}

// ActiveCapabilities is internal/commerce's resolver. The querier is the
// purchase's own transaction.
func (r gateCapabilityResolver) ActiveCapabilities(ctx context.Context, q db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return r.on(ctx, q)
}

// Active is the HTTP layer's resolver, called BEFORE any transaction is open,
// so it reads through the pool.
func (r gateCapabilityResolver) Active(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	return r.on(ctx, r.q)
}

// on answers every capability in ONE query.
//
// It used to be one query per capability, through the pool, from inside the
// caller's transaction. Both halves of that were wrong and the second was the
// dangerous one: with a pool of ten and a dozen concurrent postings, every
// connection was held by a transaction whose owner was waiting for a
// connection that would never come, and nothing moved until the statement
// timeout thirty seconds later (F-27).
func (r gateCapabilityResolver) on(ctx context.Context, q db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	out := make(map[valuedomain.CapabilityKey]bool, len(conversionCapabilities))
	if r.checker == nil {
		// No checker means nothing is active. A fresh deployment moves no
		// value across a domain boundary.
		return out, nil
	}
	if q == nil {
		q = r.q
	}
	verdicts, err := r.checker.ActiveSet(ctx, q, gateCaps)
	if err != nil {
		// An I/O failure is not an activation. Returning the error lets the
		// caller refuse the movement rather than proceed on a guess.
		return nil, err
	}
	for _, key := range conversionCapabilities {
		out[key] = verdicts[gates.Capability(string(key))].Active
	}
	return out, nil
}
