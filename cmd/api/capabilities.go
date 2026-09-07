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
	q       db.Querier
}

// conversionCapabilities are the keys the ledger asks about. They are listed
// rather than derived because the list is the contract: adding a conversion to
// internal/valuedomain without adding its gate here would leave the movement
// permanently refused, which is the safe direction to fail.
var conversionCapabilities = []valuedomain.CapabilityKey{
	valuedomain.CapNativeMarketTrading,
	valuedomain.CapPayoutReserve,
	valuedomain.CapPayoutSettle,
	valuedomain.CapHostedTrading,
	valuedomain.CapHostedFunding,
	// Not a conversion, but the payout policy and the agent authority ladder
	// name these and read them from the same resolver, so one call answers
	// every capability question a request can ask.
	valuedomain.CapabilityKey(gates.CreditPurchase),
	valuedomain.CapabilityKey(gates.NativeAssetCreation),
	valuedomain.CapabilityKey(gates.AgentBoundedDiscretion),
	valuedomain.CapabilityKey(gates.AgentAutonomousSelection),
	valuedomain.CapabilityKey(gates.AgentAutonomousPortfolio),
}

func (r gateCapabilityResolver) ActiveConversionCapabilities(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	return r.Active(ctx)
}

func (r gateCapabilityResolver) Active(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	out := make(map[valuedomain.CapabilityKey]bool, len(conversionCapabilities))
	if r.checker == nil {
		// No checker means nothing is active. A fresh deployment moves no
		// value across a domain boundary.
		return out, nil
	}
	for _, key := range conversionCapabilities {
		cap := gates.Capability(string(key))
		if !cap.Valid() {
			out[key] = false
			continue
		}
		v, err := r.checker.IsActive(ctx, r.q, cap)
		if err != nil {
			// An I/O failure is not an activation. Returning the error lets
			// the caller refuse the movement rather than proceed on a guess.
			return nil, err
		}
		out[key] = v.Active
	}
	return out, nil
}
