//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

// TestIntegration_AdvancedReadsServeTheRealStores wires the advanced core
// adapter over the real database and reads back, through HTTP, state written
// by the real stores: a funded sandbox balance, an intent and the effect that
// applied it with its journal.
func TestIntegration_AdvancedReadsServeTheRealStores(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.System()
	userID, acct := seedAccount(t, d)

	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-itest")
	deps := AdvancedDeps{
		Authorizer: econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{})),
		Intents:    econ.NewIntentStore(clk, outbox, aw),
		Effects:    econ.NewEffectStore(clk, outbox, aw),
		Risk:       risk.NewStore(),
		Prices:     valuation.NewPriceStore(clk),
		// The account holds only the sandbox dollar, which is the quote
		// asset and needs no mark.
		MaxPriceAge: time.Minute,
	}
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: "TEST", Instruments: instruments.NewRepository(), Advanced: deps})
	require.NoError(t, err)
	require.NotNil(t, ports.Advanced, "complete advanced deps wire the port")

	h := newIntegrationHarness(t, d, acct, userID)
	h.server.opts.Ports.Advanced = ports.Advanced

	// Before the sandbox dollar exists, buying power says the deployment has
	// no simulated value -- not that the account does not exist.
	var provisioned bool
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1)`, econ.SandboxDollarID()).Scan(&provisioned))
	if !provisioned {
		res := h.do(http.MethodGet, "/v1/advanced/buying-power?account_id="+acct.String(), nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
	}

	// Provision and fund 1,000 sandbox dollars.
	var usdID = econ.SandboxDollarID()
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	led := ledger.NewService(clk, "httpapi-itest")
	thousand := money.QuantityFromInt64(1_000_000_000)
	var fundRes ledger.PostResult
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		var err error
		fundRes, err = led.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, usdID), Side: ledger.Debit, Quantity: thousand},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, usdID), Side: ledger.Credit, Quantity: thousand},
			},
		})
		return err
	}))

	// An intent and the effect that applied the funding journal.
	q := money.QuantityFromInt64(1_000_000)
	var in econ.Intent
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		in, _, err = deps.Intents.Submit(ctx, tx, econ.Intent{
			AccountID: acct, Kind: econ.KindTransferValue, Mode: gates.ModeSandbox, Capability: gates.P2PTransfers,
			ActorType: security.ActorUser, ActorID: userID.String(), Quantity: &q,
			Terms: json.RawMessage(`{"memo":"itest"}`), IdempotencyKey: "itest-" + uuid.NewString(),
		})
		if err != nil {
			return err
		}
		e, _, err := deps.Effects.Begin(ctx, tx, econ.BeginRequest{
			Key: "itest:" + uuid.NewString(), Subsystem: econ.SubsystemTransfers, Kind: "SANDBOX_FUNDING",
			IntentID: &in.ID, AccountID: acct.String(), Request: map[string]string{"qty": "1000000000"},
			PostsLedger: true, Actor: econ.SystemActor("httpapi-itest"),
		})
		if err != nil {
			return err
		}
		_, err = deps.Effects.Apply(ctx, tx, e.ID, econ.ApplyRequest{
			JournalTransactionID: &fundRes.TransactionID, Actor: econ.SystemActor("httpapi-itest"), Reason: "funded",
		})
		return err
	}))

	// Capabilities: TEST is ungated for simulated modes; LIVE is refused.
	res := h.do(http.MethodGet, "/v1/advanced/capabilities", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var caps api.AdvancedCapabilityList
	res.json(&caps)
	assert.Equal(t, "TEST", caps.Environment)
	require.Len(t, caps.Items, len(gates.AdvancedSpecs()))
	for _, c := range caps.Items {
		for _, dec := range c.Decisions {
			if dec.Mode == api.AdvancedMode(gates.ModeLive) {
				assert.False(t, dec.Allowed, c.Capability)
			}
		}
	}

	// Buying power: exactly the funded thousand, in the simulated domain.
	res = h.do(http.MethodGet, "/v1/advanced/buying-power?account_id="+acct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var bp api.BuyingPowerV2
	res.json(&bp)
	assert.Equal(t, api.SIMULATED, bp.ValueDomain)
	assert.Equal(t, econ.SandboxDollarSymbol, bp.QuoteSymbol)
	assert.Equal(t, econ.SandboxDollarID().String(), bp.QuoteAssetId.String())
	assert.Equal(t, "1000.00", bp.GrossEquity)

	// The intent and the effect, with the journal that backs it.
	res = h.do(http.MethodGet, "/v1/advanced/intents?account_id="+acct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var intents api.EconomicIntentPage
	res.json(&intents)
	require.Len(t, intents.Items, 1)
	assert.Equal(t, in.ID.String(), intents.Items[0].Id.String())

	res = h.do(http.MethodGet, "/v1/advanced/intents/"+in.ID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	res = h.do(http.MethodGet, "/v1/advanced/effects?account_id="+acct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var effects api.EconomicEffectPage
	res.json(&effects)
	require.Len(t, effects.Items, 1)
	assert.Equal(t, api.EconomicEffectStatus("APPLIED"), effects.Items[0].Status)
	require.NotNil(t, effects.Items[0].JournalTransactionId)
	assert.Equal(t, fundRes.TransactionID.String(), effects.Items[0].JournalTransactionId.String())

	// Another customer's intent is not found, not forbidden.
	_, other := seedAccount(t, d)
	stranger := security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{other.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
	h.as(&stranger)
	res = h.do(http.MethodGet, "/v1/advanced/intents/"+in.ID.String(), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	res = h.do(http.MethodGet, "/v1/advanced/effects?account_id="+acct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}
