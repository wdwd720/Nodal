package bridge

import (
	"context"
	"sort"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// HaircutPolicy is how much of a leg's value counts toward Universal Buying
// Power V2 (goal §20) while it is between chains. Value in flight is not
// final, so the default counts none of it.
type HaircutPolicy struct {
	Version string `json:"version"`
	// InFlightBPS is the collateral factor of value whose source lock is
	// final and whose release has not been observed on the destination.
	InFlightBPS money.BPS `json:"in_flight_bps"`
	// ObservedBPS is the factor once a release is CONFIRMED, not final, on
	// the destination. It is never below InFlightBPS.
	ObservedBPS money.BPS `json:"observed_bps"`
}

// DefaultHaircutPolicy counts nothing that is not final.
func DefaultHaircutPolicy() HaircutPolicy { return HaircutPolicy{Version: "bridge-haircut-v1"} }

// Validate checks 0 <= InFlightBPS <= ObservedBPS <= 100%.
func (p HaircutPolicy) Validate() error {
	if p.Version == "" || p.InFlightBPS < 0 || p.ObservedBPS < p.InFlightBPS || p.ObservedBPS > money.OneHundredPercent {
		return errs.New(errs.CodeValidationFailed, "bridge: a haircut policy needs a version and 0 <= in flight <= observed <= 100%")
	}
	return nil
}

// FactorFor is the collateral factor of value in BRIDGE_IN_FLIGHT for a leg in
// state st, and why. A leg in manual review counts nothing: its destination
// may never release.
func (p HaircutPolicy) FactorFor(st CrossChainCapitalState) (money.BPS, string) {
	switch st {
	case StateDestinationObserved:
		return p.ObservedBPS, "bridge release confirmed on the destination, not final"
	case StateSourceFinalized, StateMessageInFlight:
		return p.InFlightBPS, "bridge lock final on the source, release not yet observed"
	}
	return 0, "bridge leg in manual review: the destination may never release"
}

// InFlightLeg is one leg's part of the answer.
type InFlightLeg struct {
	LegID     LegID                  `json:"leg_id"`
	RouteID   RouteID                `json:"route_id"`
	AssetID   assets.AssetID         `json:"asset_id"`
	Status    CrossChainCapitalState `json:"status"`
	Holding   LedgerHolding          `json:"holding"`
	Quantity  money.Quantity         `json:"quantity"`
	FactorBPS money.BPS              `json:"factor_bps"`
	Reason    string                 `json:"reason"`
}

// CrossChainBuyingPower is the bridge's part of one account's V2 input. A
// leg's value is counted in exactly one place, the ledger account that holds
// it (Leg.Holding):
//
//   - before its lock is final the units are still in WALLET(source), on the
//     wallet line, and reserved: their value is in PendingLocks, which belongs
//     in V2Reservations.PendingTransfers, so they are not spent twice;
//   - from the lock's finality until the release's they are in
//     BRIDGE_IN_FLIGHT(source): a ComponentBridgeInFlight holding at the
//     policy factor (zero by default);
//   - once the release is final they are in WALLET(destination), on the
//     wallet line, and nothing here counts them;
//   - a failed or refunded leg is back on (or never left) the wallet line.
type CrossChainBuyingPower struct {
	Holdings     []buyingpower.V2Holding `json:"holdings"`
	PendingLocks money.USD               `json:"pending_locks"`
	Legs         []InFlightLeg           `json:"legs"`
	Policy       HaircutPolicy           `json:"policy"`
}

// ValuedLeg is a leg with the route that values it and its source asset's
// symbol.
type ValuedLeg struct {
	Leg    Leg
	Route  Route
	Symbol string
}

type holdingKey struct {
	asset  assets.AssetID
	factor money.BPS
	reason string
}

// EvaluateCrossChain is the pure part of BuyingPower: each leg counted once,
// where its value is held. It also returns the in-flight quantity per source
// asset, which must equal the account's BRIDGE_IN_FLIGHT balances.
func EvaluateCrossChain(legs []ValuedLeg, policy HaircutPolicy) (CrossChainBuyingPower, map[assets.AssetID]money.Quantity, error) {
	if err := policy.Validate(); err != nil {
		return CrossChainBuyingPower{}, nil, err
	}
	out := CrossChainBuyingPower{Holdings: []buyingpower.V2Holding{}, Legs: []InFlightLeg{}, Policy: policy}
	sums := map[holdingKey]money.Quantity{}
	perAsset := map[assets.AssetID]money.Quantity{}
	valuing := map[assets.AssetID]ValuedLeg{}
	for _, v := range legs {
		l, r := v.Leg, v.Route
		if l.RouteID != r.ID {
			return CrossChainBuyingPower{}, nil, errs.New(errs.CodeInternal, "bridge: a leg valued by another route")
		}
		line := InFlightLeg{LegID: l.ID, RouteID: r.ID, AssetID: r.SourceAssetID, Status: l.Status, Holding: l.Holding(), Quantity: l.Quantity}
		switch line.Holding {
		case HoldingSourceReserved:
			value, err := usd(l.Quantity, r, money.RoundUp)
			if err != nil {
				return CrossChainBuyingPower{}, nil, err
			}
			if out.PendingLocks, err = out.PendingLocks.Add(value); err != nil {
				return CrossChainBuyingPower{}, nil, errs.Wrap(err, errs.CodeOverflow, "bridge: pending locks exceed the USD range")
			}
			line.Reason = "bridge lock not final: still in the source wallet, reserved"
		case HoldingInFlight:
			line.FactorBPS, line.Reason = policy.FactorFor(l.Status)
			k := holdingKey{asset: r.SourceAssetID, factor: line.FactorBPS, reason: line.Reason}
			sums[k] = sums[k].Add(l.Quantity)
			perAsset[r.SourceAssetID] = perAsset[r.SourceAssetID].Add(l.Quantity)
			valuing[r.SourceAssetID] = v
		default:
			continue
		}
		out.Legs = append(out.Legs, line)
	}
	keys := make([]holdingKey, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].asset != keys[j].asset {
			return keys[i].asset.String() < keys[j].asset.String()
		}
		if keys[i].factor != keys[j].factor {
			return keys[i].factor > keys[j].factor
		}
		return keys[i].reason < keys[j].reason
	})
	for _, k := range keys {
		v := valuing[k.asset]
		marked, err := usd(sums[k], v.Route, money.RoundDown)
		if err != nil {
			return CrossChainBuyingPower{}, nil, err
		}
		out.Holdings = append(out.Holdings, buyingpower.V2Holding{
			Component: buyingpower.ComponentBridgeInFlight, AssetID: k.asset.String(), Symbol: v.Symbol, Quantity: sums[k],
			MarkedUSD: marked, FactorBPS: k.factor, Reason: k.reason,
		})
	}
	return out, perAsset, nil
}

// BuyingPower reads the bridge's part of an account's buying power through q
// (a transaction or the pool). It refuses to answer, with
// RECONCILIATION_REQUIRED, when the legs it counts do not add up to the
// account's BRIDGE_IN_FLIGHT balances in the ledger.
func (s *Service) BuyingPower(ctx context.Context, q db.Querier, account accounts.AccountID, policy HaircutPolicy) (CrossChainBuyingPower, error) {
	if err := policy.Validate(); err != nil {
		return CrossChainBuyingPower{}, err
	}
	legs, err := listLegs(ctx, q, `account_id = $1 AND status NOT IN ('DESTINATION_FINALIZED','RECONCILED','FAILED') ORDER BY id`, account)
	if err != nil {
		return CrossChainBuyingPower{}, err
	}
	routes := map[RouteID]Route{}
	symbols := map[assets.AssetID]string{}
	valued := make([]ValuedLeg, 0, len(legs))
	for _, l := range legs {
		r, ok := routes[l.RouteID]
		if !ok {
			if r, err = s.getRoute(ctx, q, l.RouteID, false); err != nil {
				return CrossChainBuyingPower{}, err
			}
			routes[l.RouteID] = r
		}
		sym, ok := symbols[r.SourceAssetID]
		if !ok {
			a, err := s.assets.Get(ctx, q, r.SourceAssetID)
			if err != nil {
				return CrossChainBuyingPower{}, err
			}
			sym = a.Symbol
			symbols[r.SourceAssetID] = sym
		}
		valued = append(valued, ValuedLeg{Leg: l, Route: r, Symbol: sym})
	}
	out, perAsset, err := EvaluateCrossChain(valued, policy)
	if err != nil {
		return CrossChainBuyingPower{}, err
	}
	if err := s.matchInFlightBooks(ctx, q, account, perAsset); err != nil {
		return CrossChainBuyingPower{}, err
	}
	return out, nil
}

// matchInFlightBooks checks that the legs holding BRIDGE_IN_FLIGHT value add
// up, asset by asset, to the account's BRIDGE_IN_FLIGHT balances.
func (s *Service) matchInFlightBooks(ctx context.Context, q db.Querier, account accounts.AccountID, perAsset map[assets.AssetID]money.Quantity) error {
	rows, err := q.Query(ctx, `SELECT la.asset_id, b.balance::text FROM ledger_accounts la
		JOIN ledger_balances b ON b.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1 AND la.code = $2 AND b.balance <> 0`, account, string(ledger.CodeBridgeInFlight))
	if err != nil {
		return dbErr("read bridge in flight balances", err)
	}
	defer rows.Close()
	books := map[assets.AssetID]money.Quantity{}
	for rows.Next() {
		var asset assets.AssetID
		var text string
		if err := rows.Scan(&asset, &text); err != nil {
			return dbErr("scan bridge in flight balance", err)
		}
		if books[asset], err = money.ScanQuantity(text); err != nil {
			return dbErr("scan bridge in flight balance", err)
		}
	}
	if err := rows.Err(); err != nil {
		return dbErr("read bridge in flight balances", err)
	}
	for asset, q := range perAsset {
		if books[asset].Cmp(q) != 0 {
			return inFlightMismatch(asset, q, books[asset])
		}
	}
	for asset, q := range books {
		if perAsset[asset].Cmp(q) != 0 {
			return inFlightMismatch(asset, perAsset[asset], q)
		}
	}
	return nil
}

func inFlightMismatch(asset assets.AssetID, legs, books money.Quantity) error {
	return errs.New(errs.CodeReconciliationRequired, "bridge: legs in flight disagree with the ledger; buying power is withheld").
		WithField("asset_id", asset.String()).WithField("legs", legs.String()).WithField("books", books.String())
}
