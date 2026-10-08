//go:build integration

package sandboxfund

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_TheFaucetRefusesAPredictionMarketShare: an outcome share
// exists only as its market issues it, so the faucet refuses to grant one by
// name, before anything is written -- a single granted unit would leave the
// market's customer holdings above its outstanding shares and freeze its
// escrow for every holder (review-2 F1). Around the faucet, the database
// refuses a SANDBOX_FUNDING journal on a share (PM008).
func TestIntegration_TheFaucetRefusesAPredictionMarketShare(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	share := f.predictionShare()

	_, err := f.fund(f.faucet, c.p, c.account, share, 1, "share-1")
	requireCode(t, err, errs.CodeForbidden)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, RefusalPredictionShare, e.Fields["refusal"])
	assert.Equal(t, share.String(), e.Fields["asset_id"])
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM sandbox_fundings WHERE account_id = $1`, c.account))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM economic_effects WHERE account_id = $1`, c.account), "no effect was claimed")
	assert.True(t, f.wallet(c.account, share).IsZero())

	// The same grant posted around the faucet is refused by the ledger.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "raw-share-grant:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(c.account, ledger.CodeWallet, share), Side: ledger.Debit, Quantity: money.QuantityFromInt64(1)},
				{Account: ledger.CustomerAccount(c.account, ledger.CodeCapital, share), Side: ledger.Credit, Quantity: money.QuantityFromInt64(1)},
			},
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "PM008", db.SQLState(err), "%v", err)

	// Any other SIMULATED asset is still granted.
	_, err = f.fund(f.faucet, c.p, c.account, f.dollar.ID, 1_000_000, "dollar-1")
	require.NoError(t, err)
}

// predictionShare defines a prediction market collateralised in the sandbox
// dollar and returns its first outcome's share asset.
func (f *fixture) predictionShare() assets.AssetID {
	f.t.Helper()
	out := event.NewOutbox(f.clk)
	aw := audit.NewWriterWithBuildVersion("sandboxfund-itest")
	gc, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(f.t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(f.t, err)
	svc, err := predmarket.NewService(predmarket.Config{
		Clock: f.clk, Intents: econ.NewIntentStore(f.clk, out, aw), Effects: f.effects,
		Authorizer: econ.NewAuthorizer(gc, kills), Registry: econ.NewRegistry(), KillSwitches: kills, Ledger: f.ledger,
		Capital: capital.NewService(f.clk, capital.OutboxEmitter{Outbox: out, Clock: f.clk}), Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Instruments: instruments.NewRepository(),
		Assets: assets.NewRepository(), Reconciliation: reconciliation.NewRepository(f.clk, out, aw), Audit: aw,
	})
	require.NoError(f.t, err)
	oracle, err := predmarket.NewSandboxOracle("sandboxfund-itest-board", seed32("sandboxfund-itest-oracle"))
	require.NoError(f.t, err)
	operator := security.ActorOperator
	var m predmarket.Market
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, _, err = svc.CreateMarket(ctx, tx, predmarket.MarketSpec{
			Symbol: "SF" + strings.ToUpper(uuid.NewString()[:8]), Question: "Will the faucet mint an outcome share?",
			Outcomes: []string{"YES", "NO"}, CollateralAssetID: f.dollar.ID, Mode: gates.ModeSandbox,
			OpenAt: f.clk.Now(), CloseAt: f.clk.Now().Add(time.Hour),
			OracleSource: oracle.Source(), OraclePublicKey: oracle.PublicKey(),
		}, econ.Actor{Type: operator, ID: uuid.NewString()})
		return err
	}))
	return m.Outcomes[0].ShareAssetID
}

func seed32(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
