package econ

import (
	"context"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
)

// Supply-controlled assets (migration 00763; ADVANCED_FINANCIAL_MODEL §5).
//
// An asset is SUPPLY-CONTROLLED when a controller owns its supply: a platform
// stable asset (internal/stableasset's registry, stable_assets.asset_id),
// whose units exist only as its STABLE_ISSUER facility issues them against
// reserve, or a bridged twin (internal/bridge's routes,
// bridge_routes.destination_asset_id), whose units exist only as its BRIDGE
// facility releases them against a lock on the source chain. The database
// moves such an asset only in its controllers' own kinds, in the reserve,
// bridge-source and prediction-collateral kinds where it has those roles, and
// in five ordinary customer movements -- SIM_TRADE, P2P_TRANSFER,
// COMPENSATION, MARGIN_COLLATERAL_MOVE and DERIVATIVE_MARGIN_MOVE (SU001) --
// and only in a transaction that changes its holdings by exactly what it
// changes its controller's outstanding by (SU002). Everything else is
// refused, at the entry or at COMMIT.
//
// Those refusals are the database's backstop. In Go, every entry point that
// would lead such an asset into a refused kind refuses it first, by name,
// through RefuseSupplyControlled (or RefuseFacilityFunding): a coded refusal
// -- ASSET_RESTRICTED, with the fields refusal (RefusalSupplyControlled),
// controller and asset_id -- made before anything is written, never an
// INTERNAL error at COMMIT.
//
// A check made when something is configured stays true. An asset that is
// supply-controlled stays so: its registry row is never deleted. And an asset
// that ledger accounts already name never becomes supply-controlled (SU003);
// internal/stableasset and internal/bridge create every asset they control in
// the transaction that registers it.

// The controllers of a supply-controlled asset: the controller field of a
// refusal.
const (
	// ControllerStableIssuer controls a platform stable asset: its units are
	// issued and retired only by its STABLE_ISSUER facility, against reserve
	// (STABLE_ISSUANCE, STABLE_REDEMPTION).
	ControllerStableIssuer = "STABLE_ISSUER"
	// ControllerBridge controls a bridged twin: its units are released only
	// by its route's BRIDGE facility, against a lock on the source chain
	// (BRIDGE_DESTINATION_FINALIZED).
	ControllerBridge = "BRIDGE"
)

// RefusalSupplyControlled is the refusal field of every refusal of a
// supply-controlled asset.
const RefusalSupplyControlled = "SUPPLY_CONTROLLED_ASSET"

// SupplyController names the controller that owns an asset's supply --
// ControllerStableIssuer or ControllerBridge -- or "" when no controller does.
// It reads in q, which should be the transaction the decision is made in.
func SupplyController(ctx context.Context, q db.Querier, asset assets.AssetID) (string, error) {
	var stable, twin bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stable_assets WHERE asset_id = $1),
		EXISTS (SELECT 1 FROM bridge_routes WHERE destination_asset_id = $1)`, asset).Scan(&stable, &twin); err != nil {
		return "", dbErr("check supply controllers", err)
	}
	switch {
	case stable:
		return ControllerStableIssuer, nil
	case twin:
		return ControllerBridge, nil
	}
	return "", nil
}

// SupplyControllers is SupplyController for several assets at once: the
// controller of each asset a controller owns, and no entry for any other.
func SupplyControllers(ctx context.Context, q db.Querier, ids []assets.AssetID) (map[assets.AssetID]string, error) {
	out := make(map[assets.AssetID]string)
	if len(ids) == 0 {
		return out, nil
	}
	texts := make([]string, len(ids))
	for i, a := range ids {
		texts[i] = a.String()
	}
	rows, err := q.Query(ctx, `SELECT a.id::text,
		CASE WHEN EXISTS (SELECT 1 FROM stable_assets s WHERE s.asset_id = a.id) THEN $2
		     WHEN EXISTS (SELECT 1 FROM bridge_routes r WHERE r.destination_asset_id = a.id) THEN $3
		     ELSE '' END
		FROM (SELECT DISTINCT unnest($1::uuid[]) AS id) a`, texts, ControllerStableIssuer, ControllerBridge)
	if err != nil {
		return nil, dbErr("check supply controllers", err)
	}
	defer rows.Close()
	for rows.Next() {
		var text, controller string
		if err := rows.Scan(&text, &controller); err != nil {
			return nil, dbErr("check supply controllers", err)
		}
		if controller == "" {
			continue
		}
		a, err := assets.ParseAssetID(text)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "econ: an asset id read back is not an asset id")
		}
		out[a] = controller
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("check supply controllers", err)
	}
	return out, nil
}

// RefuseSupplyControlled returns nil when no controller owns asset's supply,
// and otherwise refuses it for what: a phrase naming what is refused, its
// package first ("margin: a margin account's numeraire"), and why: what the
// database would refuse further on and what that would leave unfinished. The
// refusal is ASSET_RESTRICTED with the fields refusal
// (RefusalSupplyControlled), controller and asset_id. A caller makes it before
// it writes anything, in the transaction it would write in.
func RefuseSupplyControlled(ctx context.Context, q db.Querier, asset assets.AssetID, what, why string) error {
	controller, err := SupplyController(ctx, q, asset)
	if err != nil || controller == "" {
		return err
	}
	return SupplyControlledRefusal(asset, controller, what, why)
}

// SupplyControlledRefusal is the refusal RefuseSupplyControlled makes, for a
// caller that found the controller itself (SupplyControllers).
func SupplyControlledRefusal(asset assets.AssetID, controller, what, why string) error {
	return errs.New(errs.CodeAssetRestricted, what+" is refused: asset "+asset.String()+" is "+controlledBy(controller)+"; "+why).
		WithField("refusal", RefusalSupplyControlled).
		WithField("controller", controller).
		WithField("asset_id", asset.String())
}

func controlledBy(controller string) string {
	switch controller {
	case ControllerStableIssuer:
		return "a platform stable asset, whose units exist only as its STABLE_ISSUER issues them against reserve"
	case ControllerBridge:
		return "a bridged twin, whose units exist only as its BRIDGE releases them against a lock on the source chain"
	}
	return "supply-controlled by " + controller
}

// RefuseFacilityFunding refuses FACILITY_FUNDING of a facility of kind in a
// supply-controlled asset. It takes the facility's kind, not the facility, so
// that a funding path refuses before it ensures the facility row.
//
// The refusal holds for every kind of facility. A facility's capital in such
// an asset would be units no issuance backs, and the database refuses the
// FACILITY_FUNDING journal for it (SU001). The controllers' own facilities are
// no exception to the rule, because they never need the exception: a
// STABLE_ISSUER books its asset only in STABLE_ISSUANCE and STABLE_REDEMPTION,
// a BRIDGE only in BRIDGE_DESTINATION_FINALIZED, and a facility that holds
// such an asset as a reserve, a bridge lock or a prediction market's
// collateral books it in those kinds too. None of them is ever funded.
func RefuseFacilityFunding(ctx context.Context, q db.Querier, kind ledger.FacilityKind, asset assets.AssetID) error {
	return RefuseSupplyControlled(ctx, q, asset, "econ: funding a "+string(kind)+" facility (FACILITY_FUNDING)",
		"a facility's capital in it would be units no issuance backs, and the database refuses a FACILITY_FUNDING journal "+
			"for such an asset (SU001, migration 00763); it reaches a facility's books only in its controller's own kinds "+
			"or as a reserve, a bridge lock or a prediction market's collateral, never as funding")
}
