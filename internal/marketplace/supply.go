package marketplace

import (
	"context"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
)

// Supply-controlled assets (migration 00763; econ.RefuseSupplyControlled).
//
// A follow's dispatches are filled by the IntentExecutor, which in a
// deployment is the orders paper venue: a fill against external liquidity
// whose side is never booked. The venue refuses an instrument whose base or
// quote is a platform stable asset or a bridged twin, because such a fill
// would create or destroy units of it against no counterparty (SU002), and it
// refuses it before it writes anything. A follow, though, writes plenty before
// it dispatches -- the sandbox run, each intent, its route and its risk
// decision -- and would record every dispatch to such an instrument as
// DISPATCH_FAILED, run after run.
//
// So the marketplace refuses, before anything is written, a follow -- and an
// install, which exists only to be followed -- of a version whose instruments
// name a supply-controlled asset. The executor is an interface, so the check
// cannot ask the venue which assets it would book; it checks every asset the
// instrument names instead: the base, quote and settlement asset of the
// instrument and of its current terms. That set contains the two a paper fill
// books (the base, and the quote or, without one, the settlement asset), and
// is no larger for any instrument whose settlement asset is its quote.

// instrumentAsset is one asset an instrument names.
type instrumentAsset struct {
	instrument string
	asset      assets.AssetID
}

// namedAssets lists every asset the instruments of ids name: the base, quote
// and settlement asset of each instrument and of its current terms. An id
// that is not an instrument names nothing.
func namedAssets(ctx context.Context, q db.Querier, ids []string) ([]instrumentAsset, error) {
	repo := instruments.NewRepository()
	var out []instrumentAsset
	for _, raw := range ids {
		iid, err := instruments.ParseInstrumentID(raw)
		if err != nil {
			continue
		}
		inst, err := repo.Get(ctx, q, iid)
		if errs.CodeOf(err) == errs.CodeNotFound {
			continue
		}
		if err != nil {
			return nil, dbErr("load instrument", err)
		}
		named := []*assets.AssetID{inst.BaseAssetID, inst.QuoteAssetID, &inst.SettlementAssetID}
		v, err := repo.CurrentTerms(ctx, q, iid)
		switch {
		case err == nil:
			named = append(named, v.Terms.BaseAssetID, v.Terms.QuoteAssetID, &v.Terms.SettlementAssetID)
		case errs.CodeOf(err) != errs.CodeNotFound:
			return nil, dbErr("load instrument terms", err)
		}
		for _, a := range named {
			if a != nil && !a.IsZero() {
				out = append(out, instrumentAsset{instrument: inst.ID.String(), asset: *a})
			}
		}
	}
	return out, nil
}

// refuseSupplyControlledInstruments refuses, for what, a version whose
// instruments (ids) name a supply-controlled asset.
func refuseSupplyControlledInstruments(ctx context.Context, q db.Querier, ids []string, what string) error {
	named, err := namedAssets(ctx, q, ids)
	if err != nil {
		return err
	}
	assetIDs := make([]assets.AssetID, 0, len(named))
	for _, n := range named {
		assetIDs = append(assetIDs, n.asset)
	}
	controlled, err := econ.SupplyControllers(ctx, q, assetIDs)
	if err != nil {
		return err
	}
	for _, n := range named {
		if controller, ok := controlled[n.asset]; ok {
			return econ.SupplyControlledRefusal(n.asset, controller,
				"marketplace: "+what+" of a version trading instrument "+n.instrument+", which names it,",
				"every dispatch of a follow is a paper-venue fill, whose side is external and never booked, so a fill of that "+
					"instrument would create or destroy units of it against no counterparty, a SIM_TRADE the database refuses "+
					"for such an asset (SU002, migration 00763), and the paper venue refuses it")
		}
	}
	return nil
}
