//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/security"
)

// The API states the scale of the asset it is reporting quantities in (F-44).
//
// The market page rendered `circulating_supply`, `asset_reserve`, every holder's
// balance and a BUY quote's `expected_output` at a hardcoded six decimals -- the
// Credit's scale. Those are asset quantities, and a native asset's scale is
// chosen by its creator and may be anything up to eighteen. Six is only the
// default.
//
// So the page was accidentally right, and would go wrong by a factor of ten to
// the difference the first time a creator asked for something else -- on the
// supply and concentration figures a buyer judges a market by. F-44 fixed the
// price scale and recorded this half as left.
//
// The API could not have been used correctly, because it never said what the
// scale was: neither NativeMarket nor NativeAsset carried it, though
// CreateNativeAssetRequest accepts it.
//
// This test uses NINE decimals on purpose. With six it would pass against the
// old hardcoded constant and prove nothing.

const scaleTestDecimals = 9

// customerFor is the principal a signed-in owner of that account carries. The
// neighbouring tests build it inline; it is a helper here because two tests in
// this file need the same one.
func customerFor(account accounts.AccountID) *security.Principal {
	return &security.Principal{
		SubjectID: "cust-" + newKey()[:8], ActorType: security.ActorUser,
		Roles:      []security.Role{security.RoleCustomer},
		AccountIDs: []string{account.String()},
		SessionID:  testSessionID, AuthTime: testNow.Add(-time.Minute),
		AMR: []string{"pwd"},
	}
}

func TestIntegration_TheMarketResponseStatesItsAssetsScale(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.launchMarketWithDecimals(t, commerceCreditAsset(t, d), scaleTestDecimals)

	customer := seedCustomerAccount(t, d)
	res := h.as(customerFor(customer)).do(http.MethodGet,
		"/v1/native-markets/"+h.market.ID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var body struct {
		AssetDecimals *int `json:"asset_decimals"`
		PriceScale    *int `json:"price_scale"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))

	require.NotNil(t, body.AssetDecimals, "the response must state the asset's scale, not leave a client to assume one")
	assert.Equal(t, scaleTestDecimals, *body.AssetDecimals)

	require.NotNil(t, body.PriceScale)
	assert.NotEqual(t, *body.PriceScale, *body.AssetDecimals,
		"the price scale and the asset scale are different facts; a response where they agree by accident is how F-44 hid")
	assert.NotEqual(t, 6, *body.AssetDecimals,
		"six is the Credit's scale and the default; this fixture uses nine so a hardcoded six cannot pass")
}

// TestIntegration_TheQuoteStatesTheScaleOfItsAssetSide: a BUY's expected_output
// is in asset units, and the client is told at what scale. Without it the number
// beside "You receive" is a guess.
func TestIntegration_TheQuoteStatesTheScaleOfItsAssetSide(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.launchMarketWithDecimals(t, commerceCreditAsset(t, d), scaleTestDecimals)

	customer := seedCustomerAccount(t, d)
	h.fundCredits(t, customer, "10000000000")

	res := h.as(customerFor(customer)).do(http.MethodPost,
		"/v1/native-markets/"+h.market.ID.String()+"/quotes",
		map[string]any{"account_id": customer.String(), "side": "BUY", "amount": "1000000"},
		"Idempotency-Key", newKey())
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var body struct {
		AssetDecimals  *int   `json:"asset_decimals"`
		ExpectedOutput string `json:"expected_output"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.NotNil(t, body.AssetDecimals, "a quote must state the scale of the asset it is quoting")
	assert.Equal(t, scaleTestDecimals, *body.AssetDecimals)
	assert.NotEmpty(t, body.ExpectedOutput)
}

// launchMarketWithDecimals is launchMarket with the asset's scale chosen rather
// than defaulted, which is the only way to tell a real scale from a constant.
func (h *domainAHarness) launchMarketWithDecimals(t *testing.T, creditAsset assets.AssetID, decimals uint8) {
	t.Helper()
	raw := strings.ReplaceAll(id.New[id.Any]().String(), "-", "")
	suffix := raw[len(raw)-6:]
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			a, _, err := h.nativeAssets.CreateDraft(ctx, tx, nativeasset.CreateRequest{
				CreatorAccountID: h.creator,
				Name:             "Scaled " + suffix,
				Symbol:           "SC" + suffix[:4],
				Description:      "an asset whose scale is not the default",
				Decimals:         decimals,
				Supply: nativeasset.SupplyModel{
					MaxSupply:         qq("1000000000000000"),
					CreatorAllocation: qq("100000000000000"),
				},
			})
			if err != nil {
				return err
			}
			h.asset = a
			if _, err := h.nativeAssets.SetStatus(ctx, tx, a.AssetID, nativeasset.StatusPendingReview, "submitted"); err != nil {
				return err
			}
			if _, err := h.nativeAssets.SetModeration(ctx, tx, a.AssetID, nativeasset.ModerationApproved, "fixture"); err != nil {
				return err
			}
			if _, err := h.nativeAssets.Activate(ctx, tx, a.AssetID, "fixture"); err != nil {
				return err
			}
			m, err := h.nativeMarkets.Create(ctx, tx, nativemarket.CreateRequest{
				AssetID: a.AssetID, CreditAssetID: creditAsset, CreatorID: h.creator,
				PoolSupply: a.Supply.PoolSupply(), CreatorAllocation: a.Supply.CreatorAllocation,
				VirtualCreditReserve: qq("30000000000"),
				Fees:                 nativemarket.Fees{PlatformBPS: 100, CreatorBPS: 50},
				IdempotencyKey:       "scaled-" + suffix,
				EffectiveAt:          h.clk.Now(),
			})
			if err != nil {
				return err
			}
			m, err = h.nativeMarkets.SetStatus(ctx, tx, m.ID, nativemarket.StatusActive, "fixture")
			if err != nil {
				return err
			}
			h.market = m
			return nil
		}))

	// The registry is the authority, so the fixture is checked against it
	// rather than against what it asked for.
	var got int
	require.NoError(t, h.db.QueryRow(context.Background(),
		`SELECT decimals FROM assets WHERE id = $1`, h.asset.AssetID).Scan(&got))
	require.Equal(t, int(decimals), got, "the asset registry did not record the scale this fixture asked for")
}
