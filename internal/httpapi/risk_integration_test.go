//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// A refused trade still leaves a record.
//
// The risk decision for a REJECT is written by the same transaction the refusal
// aborts, so recording it there would roll it back with the trade: the
// deployment would keep every ALLOW and lose every REJECT, which is exactly
// backwards for an audit trail. internal/nativemarket hands the decision out
// with the error and this adapter writes it once the failed transaction is
// finished — see recordRiskRefusal.
//
// The test is at the ADAPTER rather than in the domain package because that
// ordering is the adapter's job, and a domain-level test cannot observe it: the
// transaction that would have to be gone is the one the test is holding.
func TestIntegration_ARefusedTradeStillRecordsItsRiskDecision(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.launchMarket(t, commerceCreditAsset(t, d))

	buyer := seedCustomerAccount(t, d)
	h.fundCredits(t, buyer, "10000000000") // 10,000 Credits
	tightenNativeLimitsFor(t, d, buyer, 1, 1, h.clk.Now())

	adapter := nativeMarketsAdapter{db: d, deps: NativeEconomyDeps{
		NativeMarkets: h.nativeMarkets,
		LegalRouter:   mustRouter(t, legalrouter.DevelopmentPolicy()),
		Capabilities:  commerceCaps{valuedomain.CapNativeMarketTrading: true},
		Verification:  verifiedAt(valuedomain.VerificationNodalIdentity),
		Jurisdiction:  fixedJurisdiction("US-CA"),
		Clock:         h.clk,
	}}

	_, err := adapter.Execute(t.Context(), nativemarket.ExecuteRequest{
		MarketID: h.market.ID, AccountID: buyer, Side: nativemarket.Buy,
		Amount: qq("1000000000"), MinOutput: qq("1"),
		IdempotencyKey: "refused-" + id.New[id.Any]().String(),
		CorrelationID:  "corr-" + id.New[id.Any]().String(),
		EffectiveAt:    h.clk.Now(),
	})
	require.Error(t, err)
	require.Equal(t, errs.CodeRiskConcentration, errs.CodeOf(err))

	var (
		decision, marketID string
		reasons            []string
	)
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT decision, reason_codes, market_snapshot->'native_market'->>'market_id'
		   FROM risk_decisions WHERE account_id = $1
		  ORDER BY evaluated_at DESC LIMIT 1`, buyer).Scan(&decision, &reasons, &marketID),
		"the refusal must survive the transaction it refused")
	assert.Equal(t, string(risk.Reject), decision)
	assert.Contains(t, reasons, risk.ReasonNativeMarketConcentration)
	assert.Equal(t, h.market.ID.String(), marketID)

	// And the trade did not happen.
	st, serr := h.nativeMarkets.State(t.Context(), d, h.market.ID)
	require.NoError(t, serr)
	assert.EqualValues(t, 0, st.Version, "a refused trade must not move the market")
}

// tightenNativeLimitsFor records an ACCOUNT-scope policy that lowers the two
// native concentration limits for one account. ACCOUNT rows can only tighten
// (Compose takes the minimum), so no other account's limits move.
func tightenNativeLimitsFor(t *testing.T, d *db.DB, account accounts.AccountID, market, creator money.BPS, at time.Time) {
	t.Helper()
	rules, err := json.Marshal(map[string]any{
		"max_native_market_concentration_bps": market,
		"max_creator_concentration_bps":       creator,
	})
	require.NoError(t, err)
	ctx := security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: "domaina-itest", ActorType: security.ActorSystem, AuthTime: at,
	})
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, rerr := risk.NewStore().RecordPolicy(ctx, tx, risk.PolicyRecord{
				Scope:       risk.ScopeAccount,
				ScopeID:     account.String(),
				Version:     "domaina-itest-account-" + id.New[id.Any]().String(),
				Rules:       json.RawMessage(rules),
				EffectiveAt: at.Add(-time.Minute),
				ActorType:   security.ActorSystem,
				ActorID:     "domaina-itest",
				Reason:      "a tighter native concentration limit for one test account",
			})
			return rerr
		}))
}

func mustRouter(t *testing.T, p legalrouter.Policy) *legalrouter.Router {
	t.Helper()
	r, err := legalrouter.New(p)
	require.NoError(t, err)
	return r
}
