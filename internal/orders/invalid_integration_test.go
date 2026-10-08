//go:build integration

package orders

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_InvalidOrdersNeverPanicAndNeverPost drives hundreds of
// malformed orders through the service -- prices off the tick, too precise,
// zero, negative, in the wrong asset or out of range; quantities off the lot,
// below the minimum, zero, negative or astronomic; impossible shapes; bad keys
// and client ids; unknown instruments -- and requires every one to be refused
// with a stable reason, without a panic, and without a single journal
// transaction, book command, reservation or trade.
func TestIntegration_InvalidOrdersNeverPanicAndNeverPost(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	x := f.fundedAccounts(m, 1)[0]
	counts := func() [4]int {
		return [4]int{
			f.count(`SELECT count(*) FROM journal_transactions`),
			f.count(`SELECT count(*) FROM book_commands`),
			f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, x),
			f.count(`SELECT count(*) FROM book_trades`),
		}
	}
	before := counts()
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	price := func(mantissa int64, scale int32, quote string) *money.Price {
		return &money.Price{Mantissa: money.QuantityFromInt64(mantissa), Scale: scale, QuoteAsset: quote, Source: "fuzz", At: at}
	}
	q := m.quote.ID.String()
	rng := rand.New(rand.NewPCG(42, 4242))
	var cases []OrderRequest
	mutations := []func(r *OrderRequest){
		func(r *OrderRequest) { r.Price = price(1_000_001, 4, q) },                               // below the tick
		func(r *OrderRequest) { r.Price = price(0, 2, q) },                                       // zero
		func(r *OrderRequest) { r.Price = price(-10_000, 2, q) },                                 // negative
		func(r *OrderRequest) { r.Price = price(10_000, 2, assets.NewAssetID().String()) },       // wrong quote asset
		func(r *OrderRequest) { r.Price = price(10_000, 39, q) },                                 // scale beyond any price
		func(r *OrderRequest) { r.Price = price(1, -1, q) },                                      // negative scale
		func(r *OrderRequest) { r.Price = price(9_000_000_000_000_000_000, 0, q) },               // out of range
		func(r *OrderRequest) { r.Price = nil },                                                  // no price
		func(r *OrderRequest) { r.Quantity = money.QuantityFromInt64(150) },                      // off the lot
		func(r *OrderRequest) { r.Quantity = money.QuantityFromInt64(99) },                       // below the minimum
		func(r *OrderRequest) { r.Quantity = money.QuantityFromInt64(0) },                        // zero
		func(r *OrderRequest) { r.Quantity = money.QuantityFromInt64(-100) },                     // negative
		func(r *OrderRequest) { r.Quantity = pow10(70) },                                         // astronomic
		func(r *OrderRequest) { r.Quantity = money.QuantityFromInt64(1_000_000_000_000_000_00) }, // notional too large
		func(r *OrderRequest) { r.Side = "SHORT" },                                               // bad side
		func(r *OrderRequest) { r.Type = "STOP" },                                                // bad type
		func(r *OrderRequest) { r.TimeInForce = "DAY" },                                          // bad TIF
		func(r *OrderRequest) { r.Type = TypeMarket },                                            // market GTC
		func(r *OrderRequest) { r.PostOnly, r.TimeInForce = true, IOC },                          // post-only IOC
		func(r *OrderRequest) { r.ReduceOnly = true },                                            // reduce-only buy
		func(r *OrderRequest) { r.STP = "CANCEL_EVERYONE" },                                      // bad STP
		func(r *OrderRequest) { r.ClientOrderID = strings.Repeat("c", 129) },                     // client id too long
		func(r *OrderRequest) { r.ClientOrderID = "tab\tinside" },                                // control character
		func(r *OrderRequest) { r.IdempotencyKey = "" },                                          // no key
		func(r *OrderRequest) { r.IdempotencyKey = " padded " },                                  // padded key
		func(r *OrderRequest) { r.Mode = "LIVE" },                                                // live is unrepresentable
		func(r *OrderRequest) { r.InstrumentID = instruments.NewInstrumentID() },                 // no such book
		func(r *OrderRequest) { r.AccountID = accounts.AccountID{} },                             // no account
	}
	for i := 0; i < 400; i++ {
		r := f.limit(m, x, SideBuy, "100.00", units(1))
		r.IdempotencyKey = "invalid-" + strconv.Itoa(i)
		mutations[i%len(mutations)](&r)
		if rng.IntN(3) == 0 {
			mutations[rng.IntN(len(mutations))](&r)
		}
		cases = append(cases, r)
	}
	for i, r := range cases {
		var err error
		require.NotPanics(t, func() { _, err = f.pipe.Submit(f.ctx, r) }, "case %d", i)
		require.Error(t, err, "case %d must be refused: %+v", i, r)
		code := errs.CodeOf(err)
		assert.True(t, ReasonOf(err) != "" || code == errs.CodeNotFound || code == errs.CodeValidationFailed,
			"case %d refused without a stable reason: %v", i, err)
	}
	assert.Equal(t, before, counts(), "no invalid order journalled, reserved, traded or posted anything")
}
