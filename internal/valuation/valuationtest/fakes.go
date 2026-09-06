// Package valuationtest provides in-memory fakes of the valuation read
// contracts (PolicyReader, PriceSource) for tests of packages that consume
// valuations. It is test-only and must never be imported by production code.
package valuationtest

import (
	"context"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
)

// Policies is a PolicyReader backed by a map. Assets without an entry get
// the fail-closed policy, exactly like the real store.
type Policies struct {
	mu   sync.Mutex
	by   map[assets.AssetID]valuation.AssetPolicy
	Err  error // returned by Current when set
	Call int   // number of Current calls
}

// NewPolicies returns an empty fake.
func NewPolicies() *Policies { return &Policies{by: map[assets.AssetID]valuation.AssetPolicy{}} }

// Set records the policy for p.AssetID.
func (f *Policies) Set(p valuation.AssetPolicy) *Policies {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by[p.AssetID] = p
	return f
}

// Current implements valuation.PolicyReader. The Querier is ignored.
func (f *Policies) Current(_ context.Context, _ db.Querier, assetID assets.AssetID, _ time.Time) (valuation.AssetPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Call++
	if f.Err != nil {
		return valuation.AssetPolicy{}, f.Err
	}
	if p, ok := f.by[assetID]; ok {
		return p, nil
	}
	return valuation.FailClosedPolicy(assetID), nil
}

// Prices is a PriceSource backed by a map keyed by (asset, quote). Latest
// applies the same freshness rule as the real store: the observation's At
// must be within maxAge of now, otherwise STALE_MARKET_DATA.
type Prices struct {
	mu   sync.Mutex
	by   map[[2]assets.AssetID]money.Price
	Err  error
	Call int
}

// NewPrices returns an empty fake.
func NewPrices() *Prices { return &Prices{by: map[[2]assets.AssetID]money.Price{}} }

// Set records p as the latest observation for (asset, quote).
func (f *Prices) Set(asset, quote assets.AssetID, p money.Price) *Prices {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by[[2]assets.AssetID{asset, quote}] = p
	return f
}

// Latest implements valuation.PriceSource. The Querier is ignored.
func (f *Prices) Latest(_ context.Context, _ db.Querier, assetID, quoteAssetID assets.AssetID, maxAge time.Duration, now time.Time) (money.Price, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Call++
	if f.Err != nil {
		return money.Price{}, f.Err
	}
	stale := errs.New(errs.CodeStaleMarketData, "no price observation within the maximum age").
		WithField("asset_id", assetID.String())
	p, ok := f.by[[2]assets.AssetID{assetID, quoteAssetID}]
	if !ok || maxAge <= 0 || p.At.Before(now.Add(-maxAge)) || p.At.After(now) {
		return money.Price{}, stale
	}
	return p, nil
}
