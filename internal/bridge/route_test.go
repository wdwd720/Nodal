package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// TestRouteDefinition_RefusesMalformedRoutesAndTheSandboxDollarSymbol: a
// route needs a generic name, a source, a positive unit price and a bounded
// number of release attempts, and its bridged twin never takes the symbol of
// the platform's one sandbox dollar.
func TestRouteDefinition_RefusesMalformedRoutesAndTheSandboxDollarSymbol(t *testing.T) {
	price, err := money.NewPrice(money.QuantityFromInt64(1), 0, "USD", "ROUTE_UNIT_PRICE", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	good := RouteDefinition{Name: "sol-to-eth", SourceAssetID: newID[assets.AssetID](), DestinationChain: dstChain,
		DestinationSymbol: "BUSD-E", UnitPrice: price, MaxReleaseAttempts: 3}
	require.NoError(t, good.Validate())
	for name, mutate := range map[string]func(d *RouteDefinition){
		"name":             func(d *RouteDefinition) { d.Name = "Sol To Eth" },
		"empty symbol":     func(d *RouteDefinition) { d.DestinationSymbol = "" },
		"padded symbol":    func(d *RouteDefinition) { d.DestinationSymbol = " BUSD" },
		"sandbox dollar":   func(d *RouteDefinition) { d.DestinationSymbol = "SBX-USD" },
		"sandbox dollar 2": func(d *RouteDefinition) { d.DestinationSymbol = "sbx-usd" },
		"no source":        func(d *RouteDefinition) { d.SourceAssetID = assets.AssetID{} },
		"zero price":       func(d *RouteDefinition) { d.UnitPrice.Mantissa = money.Quantity{} },
		"no attempts":      func(d *RouteDefinition) { d.MaxReleaseAttempts = 0 },
		"many attempts":    func(d *RouteDefinition) { d.MaxReleaseAttempts = 21 },
	} {
		d := good
		mutate(&d)
		err := d.Validate()
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
}
