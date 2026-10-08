//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// TestIntegration_HoldingsSayWhatTheyCannotValueInsteadOfAZero drives the
// real holdings adapter -- buying power over the real policies and prices,
// the positions engine, the read model -- over the real database, through
// HTTP. The account holds:
//
//   - USDC, a NORMAL USD stablecoin: VALUED at face;
//   - the sandbox dollar a sandbox grant leaves, which buying power has no
//     policy for and which is SIMULATED anyway: UNVALUED, SIMULATED_VALUE, and
//     not one USD figure -- the first live run of the web suite found it
//     answered "usd_mark":"0.00", and the portfolio page drew $0.00;
//   - a real token whose only price is older than its policy allows:
//     UNVALUED, NO_FRESH_MARK, with its cost basis (a fact about what was
//     paid) and no mark or unrealized figure;
//   - a real token no policy covers: UNVALUED, NO_ASSET_POLICY.
func TestIntegration_HoldingsSayWhatTheyCannotValueInsteadOfAZero(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.System()
	now := clk.Now()
	user, account := seedAccount(t, d)

	usdc := seedAsset(t, d)
	token := func(symbol string) assets.Asset {
		a, err := assets.NewRepository().Create(ctx, d, assets.Asset{
			Chain: "solana-devnet", MintAddress: "mint-" + id.New[id.Any]().String(), Kind: assets.KindSPLToken,
			ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: symbol, Name: symbol + " (holdings itest)", Decimals: 9,
			RiskClass: assets.RiskMajor, Status: assets.StatusActive,
		})
		require.NoError(t, err)
		return a
	}
	stale, uncovered := token("STALE"), token("NOPOLICY")

	led := ledger.NewService(clk, "httpapi-itest")
	led.AllowSeedPostings()
	policies, prices := valuation.NewPolicyStore(), valuation.NewPriceStore(clk)
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		for _, np := range []valuation.NewPolicy{
			{AssetID: usdc.ID, Status: assets.StatusActive, CollateralFactor: 10_000, StablecoinStatus: valuation.StablecoinNormal,
				MaxPriceAge: time.Minute, PolicyVersion: "holdings-itest", EffectiveAt: now.Add(-time.Hour), ActorType: "SYSTEM",
				ActorID: "httpapi-itest", Reason: "holdings integration test"},
			{AssetID: stale.ID, Status: assets.StatusActive, CollateralFactor: 8_000, MaxPriceAge: time.Minute,
				PolicyVersion: "holdings-itest", EffectiveAt: now.Add(-time.Hour), ActorType: "SYSTEM", ActorID: "httpapi-itest",
				Reason: "holdings integration test"},
		} {
			if _, err := policies.RecordPolicy(ctx, tx, np); err != nil {
				return err
			}
		}
		// The token's only price is an hour old; its policy accepts a minute.
		if _, err := prices.RecordPrice(ctx, tx, valuation.PriceObservation{
			AssetID: stale.ID, QuoteAssetID: usdc.ID, Mantissa: money.QuantityFromInt64(150_00), Scale: 2,
			Source: "holdings-itest", ObservedAt: now.Add(-time.Hour),
		}); err != nil {
			return err
		}
		if _, err := econ.EnsureSandboxDollar(ctx, tx); err != nil {
			return err
		}
		credit := func(kind ledger.Kind, asset assets.AssetID, units int64) error {
			ref := uuid.NewString()
			q := money.QuantityFromInt64(units)
			_, err := led.Post(ctx, tx, ledger.Posting{
				Kind: kind, IdempotencyKey: "holdings-itest:" + ref,
				Reference: ledger.FinancialEventReference{Type: "holdings_itest", ID: ref}, EffectiveAt: now,
				Entries: []ledger.Entry{
					{Account: ledger.CustomerAccount(account, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
					{Account: ledger.CustomerAccount(account, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: q},
				},
			})
			return err
		}
		for _, c := range []struct {
			kind  ledger.Kind
			asset assets.AssetID
			units int64
		}{
			{ledger.KindSeed, usdc.ID, 1_250_000_000},                       // 1,250 USDC
			{ledger.KindSeed, stale.ID, 2_000_000_000},                      // 2 STALE
			{ledger.KindSeed, uncovered.ID, 3_000_000_000},                  // 3 NOPOLICY
			{ledger.KindSandboxFunding, econ.SandboxDollarID(), 25_000_000}, // 25 SBX-USD
		} {
			if err := credit(c.kind, c.asset, c.units); err != nil {
				return err
			}
		}
		return nil
	}))

	controller, err := killswitch.NewController(clk, noopKillAudit{}, NewApprovalVerifier(admin.NewService(clk, audit.NewWriter())))
	require.NoError(t, err)
	engine, err := buyingpower.NewEngine(buyingpower.Deps{
		Clock: clk, Policies: policies, Prices: prices, KillSwitches: NewKillSwitchReader(controller, killswitch.Policy{}),
		Reconciliation: NewReconciliationBlockReader(), QuoteAssetID: usdc.ID,
	})
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Positions: positions.NewEngine(), BuyingPower: engine})
	require.NoError(t, err)
	require.NotNil(t, ports.Holdings, "positions and buying power wire the holdings port")

	h := newIntegrationHarness(t, d, account, user)
	h.server.opts.Ports.Holdings = ports.Holdings
	res := h.do(http.MethodGet, "/v1/accounts/"+account.String()+"/holdings", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.HoldingsResponse
	res.json(&out)

	bySymbol := map[string]api.Holding{}
	raw := map[string]map[string]any{}
	for i, item := range out.Holdings {
		bySymbol[item.Symbol] = item
		raw[item.Symbol] = res.raw()["holdings"].([]any)[i].(map[string]any)
	}
	require.Len(t, bySymbol, 4, "every non-zero wallet balance is a holding: %s", res.Body.String())

	u := bySymbol["USDC"]
	assert.Equal(t, api.HoldingValued, u.ValuationStatus)
	assert.Nil(t, u.UnvaluedReason)
	require.NotNil(t, u.UsdMark)
	assert.Equal(t, "1250.00", *u.UsdMark)
	require.NotNil(t, u.PriceRef)
	assert.Equal(t, "face:USD", *u.PriceRef)
	require.NotNil(t, u.CostBasisUsd)
	require.NotNil(t, u.UnrealizedPnlUsd)

	sbx := bySymbol[econ.SandboxDollarSymbol]
	assert.Equal(t, api.HoldingUnvalued, sbx.ValuationStatus)
	require.NotNil(t, sbx.UnvaluedReason)
	assert.Equal(t, api.HoldingUnvaluedSimulatedValue, *sbx.UnvaluedReason)
	assert.Equal(t, "25000000", sbx.Quantity, "the units are still stated exactly")
	for _, figure := range []string{"usd_mark", "price_ref", "cost_basis_usd", "unrealized_pnl_usd", "realized_pnl_usd"} {
		assert.NotContains(t, raw[econ.SandboxDollarSymbol], figure, "simulated value states no %s, not even a zero", figure)
	}

	s := bySymbol["STALE"]
	assert.Equal(t, api.HoldingUnvalued, s.ValuationStatus)
	require.NotNil(t, s.UnvaluedReason)
	assert.Equal(t, api.HoldingUnvaluedNoFreshMark, *s.UnvaluedReason)
	assert.Nil(t, s.UsdMark)
	assert.Nil(t, s.PriceRef)
	assert.Nil(t, s.UnrealizedPnlUsd, "no unrealized result without a mark")
	require.NotNil(t, s.CostBasisUsd, "the cost basis of a real asset is a fact, valued or not")

	n := bySymbol["NOPOLICY"]
	assert.Equal(t, api.HoldingUnvalued, n.ValuationStatus)
	require.NotNil(t, n.UnvaluedReason)
	assert.Equal(t, api.HoldingUnvaluedNoAssetPolicy, *n.UnvaluedReason)
	assert.Nil(t, n.UsdMark)

	// Buying power, the source of every mark here, agrees: it valued USDC
	// alone, and its portfolio value is exactly the one mark the holdings state.
	bp, err := engine.ComputeFor(ctx, d, account, buyingpower.PurposeDisplay)
	require.NoError(t, err)
	assert.Equal(t, "1250.00", bp.PortfolioValue.String())
}
