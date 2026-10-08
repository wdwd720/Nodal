//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxfund"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/valuation"
)

// TestIntegration_TransfersThroughHTTPOverTheRealServices drives the transfer
// and faucet routes over the real adapters, services and database, wired the
// way cmd/api wires them: the faucet funds a sender, a send the service
// refuses is recorded and readable, a send to a handle moves value, its replay
// moves nothing, a stranger is told the transfer does not exist, and an
// operator reverses it -- with the ledger agreeing at every step.
func TestIntegration_TransfersThroughHTTPOverTheRealServices(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.System()
	aliceUser, alice := seedAccount(t, d)
	bobUser, bob := seedAccount(t, d)
	_, carol := seedAccount(t, d)

	// --- wiring, as cmd/api does it ---------------------------------------
	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-itest")
	led := ledger.NewService(clk, "httpapi-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	screen, err := transfers.NewListScreen("transfers-list-screen-v1", nil)
	require.NoError(t, err)
	svc, err := transfers.NewService(transfers.Deps{
		Clock: clk, Ledger: led, Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: outbox, Clock: clk}),
		Intents: econ.NewIntentStore(clk, outbox, aw), Effects: econ.NewEffectStore(clk, outbox, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), KillSwitches: kills, Router: router,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: transfers.DefaultPolicy(),
		Valuer: transfers.RegistryValuer{
			Prices: valuation.NewPriceStore(clk), Assets: assets.NewRepository(), QuoteAssetID: econ.SandboxDollarID(), MaxMarkAge: 5 * time.Minute,
		},
		Screen: screen, Notifier: transfers.NewNotificationNotifier(notification.NewRepository()), Audit: aw,
	})
	require.NoError(t, err)
	faucet, err := sandboxfund.New(sandboxfund.Deps{
		Clock: clk, Ledger: led, Effects: econ.NewEffectStore(clk, outbox, aw),
		Environment: config.EnvTest, Policy: sandboxfund.DefaultPolicy(), KillSwitches: kills,
	})
	require.NoError(t, err)
	controller, err := killswitch.NewController(clk, noopKillAudit{}, NewApprovalVerifier(admin.NewService(clk, audit.NewWriter())))
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: config.EnvTest, KillSwitches: controller, Advanced: AdvancedDeps{
		Transfers: TransfersDeps{Service: svc, Faucet: faucet},
		Operator:  OperatorDeps{Prices: valuation.NewPriceStore(clk), Audit: aw, MaxMarkAge: 5 * time.Minute},
	}})
	require.NoError(t, err)
	require.NotNil(t, ports.Transfers, "the transfer service wires its port")
	require.NotNil(t, ports.TransferReversals, "and the two-operator reversal")
	require.NotNil(t, ports.SandboxFaucet, "the faucet wires its port")
	require.NotNil(t, ports.Operator, "the console wires with its price store, audit writer and kill switches")

	h := newIntegrationHarness(t, d, alice, aliceUser)
	h.server.opts.Ports.Transfers = ports.Transfers
	h.server.opts.Ports.TransferReversals = ports.TransferReversals
	h.server.opts.Ports.SandboxFaucet = ports.SandboxFaucet
	h.server.opts.Ports.Operator = ports.Operator
	asAlice := transferCustomer(aliceUser, alice)
	asBob := transferCustomer(bobUser, bob)
	asCarol := transferCustomer(accounts.NewUserID(), carol)
	operator := security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
	usdID := econ.SandboxDollarID()
	wallet := func(acct accounts.AccountID) string {
		t.Helper()
		b, err := led.Balance(ctx, d, ledger.CustomerAccount(acct, ledger.CodeWallet, usdID))
		require.NoError(t, err)
		return b.String()
	}
	key := func(name string) string { return "itest-" + name + "-" + uuid.NewString() }

	// --- the faucet funds Alice ---------------------------------------------
	h.as(&asAlice)
	res := h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants",
		map[string]any{"account_id": alice.String(), "amount": "1000"}, "Idempotency-Key", key("grant"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var grant api.AdvancedFaucetGrant
	res.json(&grant)
	assert.Equal(t, "1000000000", grant.Quantity, "1000 sandbox dollars at 6 decimals, exactly")
	assert.Equal(t, usdID.String(), grant.AssetId.String())
	assert.Equal(t, api.SIMULATED, grant.ValueDomain)
	assert.Equal(t, "1000000000", wallet(alice))

	res = h.do(http.MethodGet, "/v1/advanced/transfers/faucet/allowance?account_id="+alice.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var allowance api.AdvancedFaucetAllowance
	res.json(&allowance)
	assert.Equal(t, "1000000000", allowance.Used)
	assert.Equal(t, sandboxfund.DefaultPolicy().Cap(econ.SandboxDollarDecimals).Sub(money.QuantityFromInt64(1_000_000_000)).String(),
		allowance.Remaining)

	// --- Bob registers a handle ---------------------------------------------
	handle := "bob-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	h.as(&asBob)
	res = h.do(http.MethodPost, "/v1/advanced/transfers/handles",
		map[string]any{"account_id": bob.String(), "handle": "@" + strings.ToUpper(handle)}, "Idempotency-Key", key("handle"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var registered api.AdvancedTransferHandle
	res.json(&registered)
	assert.Equal(t, handle, registered.Handle, "normalized by the service")

	// --- a send the service refuses is recorded -------------------------------
	// Alice has no verified identity yet; Risk Kernel V2 refuses, and the
	// refusal is a decision with a record, not an error that leaves nothing.
	h.as(&asAlice)
	send := func(k, amount string) *response {
		return h.do(http.MethodPost, "/v1/advanced/transfers", map[string]any{
			"account_id": alice.String(), "recipient_handle": "@" + handle, "amount": amount,
			"mode": "SANDBOX", "memo": "rent for october",
		}, "Idempotency-Key", k)
	}
	res = send(key("refused"), "10")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	refusal := res.problem()
	assert.Equal(t, errs.CodeRiskRejected, refusal.Code)
	assert.Equal(t, risk.ReasonIdentityUnverified, refusal.Fields["rejection_code"])
	refusedID, _ := refusal.Fields["transfer_id"].(string)
	require.NotEmpty(t, refusedID, "the refusal names the transfer it recorded")
	assert.NotContains(t, res.Body.String(), bob.String(), "a handle is never resolved for the sender")
	assert.Equal(t, "1000000000", wallet(alice), "a refusal moves nothing")

	res = h.do(http.MethodGet, "/v1/advanced/transfers/"+refusedID, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var refused api.AdvancedTransfer
	res.json(&refused)
	assert.Equal(t, api.AdvancedTransferStatus("REJECTED"), refused.Status)
	require.NotNil(t, refused.RejectionCode)
	assert.Equal(t, risk.ReasonIdentityUnverified, *refused.RejectionCode)
	assert.Nil(t, refused.JournalTransactionId)
	assert.NotNil(t, refused.RiskDecisionId, "the risk decision is persisted whatever it says")

	// --- verified, Alice pays Bob's handle ------------------------------------
	transferVerifiedIdentity(t, d, aliceUser)
	sendKey := key("send")
	first := send(sendKey, "12.345678")
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())
	var sent api.AdvancedTransfer
	first.json(&sent)
	assert.Equal(t, api.AdvancedTransferStatus("COMPLETED"), sent.Status)
	assert.Equal(t, "12345678", sent.Quantity)
	assert.Equal(t, "12.35", sent.NotionalUsd, "the notional is rounded up for the limits")
	assert.Equal(t, "SBX-USD", sent.AssetSymbol)
	assert.Equal(t, 6, sent.AssetDecimals)
	assert.Equal(t, api.SIMULATED, sent.ValueDomain)
	assert.Nil(t, sent.RecipientAccountId, "Alice paid a handle; the account behind it stays on the server")
	require.NotNil(t, sent.RecipientHandle)
	assert.Equal(t, handle, *sent.RecipientHandle)
	require.NotNil(t, sent.JournalTransactionId)
	assert.Equal(t, "987654322", wallet(alice))
	assert.Equal(t, "12345678", wallet(bob))

	// The same key and body is the same answer and the same single transfer.
	again := send(sendKey, "12.345678")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, first.Body.String(), again.Body.String())
	assert.Equal(t, 1, transferRowCount(t, d, `SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1 AND idempotency_key = $2`,
		alice, sendKey))
	assert.Equal(t, 1, transferRowCount(t, d, `SELECT count(*) FROM journal_transactions WHERE kind = 'P2P_TRANSFER' AND reference_id = $1`,
		sent.Id.String()))
	assert.Equal(t, "987654322", wallet(alice), "the replay moved nothing")

	// --- the listings ----------------------------------------------------------
	res = h.do(http.MethodGet, "/v1/advanced/transfers?account_id="+alice.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var alicePage api.AdvancedTransferPage
	res.json(&alicePage)
	require.Len(t, alicePage.Items, 2, "the sender sees what it sent and what it was refused")
	assert.Equal(t, sent.Id, alicePage.Items[0].Id, "newest first")
	assert.Equal(t, refusedID, alicePage.Items[1].Id.String())
	assert.Equal(t, api.SIMULATED, alicePage.ValueDomain)

	h.as(&asBob)
	res = h.do(http.MethodGet, "/v1/advanced/transfers?account_id="+bob.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var bobPage api.AdvancedTransferPage
	res.json(&bobPage)
	require.Len(t, bobPage.Items, 1, "the recipient sees what reached it, not the sender's refused attempt")
	received := bobPage.Items[0]
	assert.Equal(t, sent.Id, received.Id)
	require.NotNil(t, received.RecipientAccountId)
	assert.Equal(t, bob.String(), received.RecipientAccountId.String())
	assert.Nil(t, received.RiskDecisionId, "the sender's decision record is the sender's")
	require.NotNil(t, received.Memo)
	assert.Equal(t, "rent for october", *received.Memo)

	res = h.do(http.MethodGet, "/v1/advanced/transfers/"+refusedID, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "the refused attempt never reached Bob; body=%s", res.Body.String())

	// --- a stranger is told the transfer does not exist -------------------------
	h.as(&asCarol)
	foreign := h.do(http.MethodGet, "/v1/advanced/transfers/"+sent.Id.String(), nil)
	absent := h.do(http.MethodGet, "/v1/advanced/transfers/"+transfers.NewTransferID().String(), nil)
	require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
	require.Equal(t, http.StatusNotFound, absent.Code, "body=%s", absent.Body.String())
	fp, ap := foreign.problem(), absent.problem()
	assert.Equal(t, ap.Code, fp.Code)
	assert.Equal(t, ap.Detail, fp.Detail, "an existing foreign transfer and an absent one are indistinguishable")
	res = h.do(http.MethodGet, "/v1/advanced/transfers?account_id="+alice.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	// --- two operators reverse it ------------------------------------------------
	// One operator proposes; the proposer cannot approve; a second operator
	// approves and the reversal executes once; the console counted the
	// proposal while it waited.
	h.as(&operator)
	pendingNow := func() int64 {
		t.Helper()
		r := h.do(http.MethodGet, "/v1/advanced/ops/overview", nil)
		require.Equal(t, http.StatusOK, r.Code, "body=%s", r.Body.String())
		var o api.AdvancedOpsOverview
		r.json(&o)
		return o.DualControl.TransferReversalsPending
	}
	pendingBefore := pendingNow()
	proposeBody := map[string]any{"reason_code": "DISPUTE_UPHELD", "reason": "sender disputed the payment"}
	res = h.do(http.MethodPost, "/v1/advanced/transfers/"+sent.Id.String()+"/reversal-proposals", proposeBody,
		"Idempotency-Key", key("propose"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var proposal api.AdvancedTransferReversalProposal
	res.json(&proposal)
	assert.Equal(t, api.TransferReversalPending, proposal.Status)
	assert.Equal(t, operator.SubjectID, proposal.ProposedBy)
	assert.Equal(t, api.AdvancedTransferStatus("COMPLETED"), proposal.Transfer.Status, "a proposal moves nothing")
	assert.Equal(t, "987654322", wallet(alice))

	assert.Equal(t, pendingBefore+1, pendingNow(), "the console counts what awaits a second operator")

	decide := func(verb string, as *security.Principal, k string, body map[string]any) *response {
		h.as(as)
		return h.do(http.MethodPost, "/v1/advanced/transfers/reversal-proposals/"+proposal.Id.String()+"/"+verb, body, "Idempotency-Key", k)
	}
	res = decide("approve", &operator, key("self-approve"), map[string]any{})
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "TRANSFER_REVERSAL_SELF_APPROVAL", res.problem().Fields["rejection_code"])
	assert.Zero(t, transferRowCount(t, d, `SELECT count(*) FROM journal_transactions WHERE reversal_of = $1`,
		sent.JournalTransactionId.String()), "the proposer's approval moved nothing")

	// A customer reaches no step.
	res = decide("approve", &asAlice, key("customer-approve"), map[string]any{})
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	// The second operator is Finance: the correction authority, without sandbox:operate.
	finance := security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleFinance},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
	approveKey := key("approve")
	res = decide("approve", &finance, approveKey, map[string]any{"reason": "dispute evidence reviewed"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var approved api.AdvancedTransferReversalDecisionResult
	res.json(&approved)
	assert.False(t, approved.Replayed)
	assert.Equal(t, api.TransferReversalApproved, approved.Proposal.Status)
	require.NotNil(t, approved.Proposal.DecidedBy)
	assert.Equal(t, finance.SubjectID, *approved.Proposal.DecidedBy)
	reversed := approved.Proposal.Transfer
	assert.Equal(t, api.AdvancedTransferStatus("REVERSED"), reversed.Status)
	require.NotNil(t, reversed.ReversalReasonCode)
	assert.Equal(t, "DISPUTE_UPHELD", *reversed.ReversalReasonCode, "the proposal's reason code becomes the journal's")
	require.NotNil(t, reversed.ReversalJournalTransactionId)
	require.NotNil(t, reversed.ReversalProposalId)
	assert.Equal(t, proposal.Id, *reversed.ReversalProposalId, "the reversal names the proposal it executed")
	require.NotNil(t, reversed.RecipientAccountId, "the operator sees the whole record")
	assert.Equal(t, bob.String(), reversed.RecipientAccountId.String())
	assert.Equal(t, "1000000000", wallet(alice))
	assert.Equal(t, "0", wallet(bob))

	replayed := decide("approve", &finance, approveKey, map[string]any{"reason": "dispute evidence reviewed"})
	require.Equal(t, http.StatusOK, replayed.Code, "body=%s", replayed.Body.String())
	assert.JSONEq(t, res.Body.String(), replayed.Body.String())
	// A third operator's approval of the consumed proposal executes nothing.
	third := operator
	third.SubjectID = uuid.NewString()
	res = decide("approve", &third, key("third-approve"), map[string]any{})
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	res = decide("reject", &third, key("late-reject"), map[string]any{"reason": "too late"})
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, transferRowCount(t, d, `SELECT count(*) FROM journal_transactions WHERE reversal_of = $1`,
		sent.JournalTransactionId.String()), "one compensation, whatever the retries")

	// A REVERSED transfer takes no new proposal.
	h.as(&operator)
	res = h.do(http.MethodPost, "/v1/advanced/transfers/"+sent.Id.String()+"/reversal-proposals", proposeBody, "Idempotency-Key", key("again"))
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)

	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals?transfer_id="+sent.Id.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var listed api.AdvancedTransferReversalProposalPage
	res.json(&listed)
	require.Len(t, listed.Items, 1)
	assert.Equal(t, api.TransferReversalApproved, listed.Items[0].Status)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals/"+proposal.Id.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	h.as(&operator)
	assert.Equal(t, pendingBefore, pendingNow(), "a consumed proposal awaits nobody")

	// --- and the books agree --------------------------------------------------------
	h.as(&operator)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reconciliation?asset_id="+usdID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rep api.AdvancedTransferReconciliation
	res.json(&rep)
	assert.True(t, rep.Ok, "discrepancies: %+v", rep.Discrepancies)
	assert.GreaterOrEqual(t, rep.Transfers, 2)
	assert.Equal(t, api.SIMULATED, rep.ValueDomain)
}

// TestIntegration_TransferStepUpIsTheServicesOwn: above the transfer policy's
// threshold the service demands a recent strong authentication, through HTTP,
// and the refusal writes nothing.
func TestIntegration_TransferStepUpIsTheServicesOwn(t *testing.T) {
	d := openTestDB(t)
	clk := clock.System()
	aliceUser, alice := seedAccount(t, d)
	_, bob := seedAccount(t, d)
	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-itest")
	led := ledger.NewService(clk, "httpapi-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	screen, err := transfers.NewListScreen("transfers-list-screen-v1", nil)
	require.NoError(t, err)
	svc, err := transfers.NewService(transfers.Deps{
		Clock: clk, Ledger: led, Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: outbox, Clock: clk}),
		Intents: econ.NewIntentStore(clk, outbox, aw), Effects: econ.NewEffectStore(clk, outbox, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), KillSwitches: kills, Router: router,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: transfers.DefaultPolicy(),
		Valuer: transfers.RegistryValuer{}, Screen: screen,
		Notifier: transfers.NewNotificationNotifier(notification.NewRepository()), Audit: aw,
	})
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: config.EnvTest, Advanced: AdvancedDeps{Transfers: TransfersDeps{Service: svc}}})
	require.NoError(t, err)
	require.Nil(t, ports.SandboxFaucet, "no faucet, no faucet port")
	h := newIntegrationHarness(t, d, alice, aliceUser)
	h.server.opts.Ports.Transfers = ports.Transfers
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))

	// Authenticated strongly, but ten minutes ago: enough for the route, not
	// for a transfer above the policy's step-up threshold.
	stale := transferCustomer(aliceUser, alice)
	stale.AuthTime = time.Now().UTC().Add(-10 * time.Minute)
	h.as(&stale)
	res := h.do(http.MethodPost, "/v1/advanced/transfers", map[string]any{
		"account_id": alice.String(), "recipient_account_id": bob.String(),
		"quantity": "1500000000", "mode": "SANDBOX",
	}, "Idempotency-Key", "itest-stepup-"+uuid.NewString())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeStepUpRequired, p.Code)
	assert.Equal(t, transfers.DefaultPolicy().StepUpAbove.String(), p.Fields["step_up_above"])
	assert.Zero(t, transferRowCount(t, d, `SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, alice),
		"a refusal before the request is accepted writes nothing")

	// The unwired faucet says so.
	res = h.do(http.MethodGet, "/v1/advanced/transfers/faucet/allowance?account_id="+alice.String(), nil)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
}

// TestIntegration_TransferReversalOverHTTPExecutesOnceForTwoOperators: over
// the real adapter and database, a rejected proposal executes nothing and a
// later proposal is still possible, and when several distinct operators
// approve the same proposal at once -- each through its own server, under its
// own Idempotency-Key -- exactly one approval reverses the transfer.
func TestIntegration_TransferReversalOverHTTPExecutesOnceForTwoOperators(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.System()
	aliceUser, alice := seedAccount(t, d)
	_, bob := seedAccount(t, d)
	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-itest")
	led := ledger.NewService(clk, "httpapi-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	screen, err := transfers.NewListScreen("transfers-list-screen-v1", nil)
	require.NoError(t, err)
	svc, err := transfers.NewService(transfers.Deps{
		Clock: clk, Ledger: led, Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: outbox, Clock: clk}),
		Intents: econ.NewIntentStore(clk, outbox, aw), Effects: econ.NewEffectStore(clk, outbox, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), KillSwitches: kills, Router: router,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: transfers.DefaultPolicy(),
		Valuer: transfers.RegistryValuer{
			Prices: valuation.NewPriceStore(clk), Assets: assets.NewRepository(), QuoteAssetID: econ.SandboxDollarID(), MaxMarkAge: 5 * time.Minute,
		},
		Screen: screen, Notifier: transfers.NewNotificationNotifier(notification.NewRepository()), Audit: aw,
	})
	require.NoError(t, err)
	faucet, err := sandboxfund.New(sandboxfund.Deps{
		Clock: clk, Ledger: led, Effects: econ.NewEffectStore(clk, outbox, aw),
		Environment: config.EnvTest, Policy: sandboxfund.DefaultPolicy(), KillSwitches: kills,
	})
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: config.EnvTest, Advanced: AdvancedDeps{
		Transfers: TransfersDeps{Service: svc, Faucet: faucet},
	}})
	require.NoError(t, err)
	serverAs := func(p security.Principal) *harness {
		h := newIntegrationHarness(t, d, alice, aliceUser)
		h.server.opts.Ports.Transfers = ports.Transfers
		h.server.opts.Ports.TransferReversals = ports.TransferReversals
		h.server.opts.Ports.SandboxFaucet = ports.SandboxFaucet
		h.as(&p)
		return h
	}
	newOperator := func() security.Principal {
		return security.Principal{
			SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
			SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
		}
	}
	key := func(name string) string { return "itest-rev-" + name + "-" + uuid.NewString() }
	usdID := econ.SandboxDollarID()
	wallet := func(acct accounts.AccountID) string {
		t.Helper()
		b, err := led.Balance(ctx, d, ledger.CustomerAccount(acct, ledger.CodeWallet, usdID))
		require.NoError(t, err)
		return b.String()
	}

	// Alice is funded and verified, and pays Bob 25 sandbox dollars.
	asAlice := serverAs(transferCustomer(aliceUser, alice))
	res := asAlice.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants",
		map[string]any{"account_id": alice.String(), "amount": "100"}, "Idempotency-Key", key("grant"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	transferVerifiedIdentity(t, d, aliceUser)
	res = asAlice.do(http.MethodPost, "/v1/advanced/transfers", map[string]any{
		"account_id": alice.String(), "recipient_account_id": bob.String(), "amount": "25", "mode": "SANDBOX",
	}, "Idempotency-Key", key("send"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var sent api.AdvancedTransfer
	res.json(&sent)
	require.NotNil(t, sent.JournalTransactionId)
	compensations := func() int {
		return transferRowCount(t, d, `SELECT count(*) FROM journal_transactions WHERE reversal_of = $1`, sent.JournalTransactionId.String())
	}
	propose := func(by security.Principal, reason string) api.AdvancedTransferReversalProposal {
		t.Helper()
		r := serverAs(by).do(http.MethodPost, "/v1/advanced/transfers/"+sent.Id.String()+"/reversal-proposals",
			map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": reason}, "Idempotency-Key", key("propose"))
		require.Equal(t, http.StatusCreated, r.Code, "body=%s", r.Body.String())
		var p api.AdvancedTransferReversalProposal
		r.json(&p)
		return p
	}

	// --- a rejected proposal executes nothing ----------------------------------
	proposer := newOperator()
	first := propose(proposer, "the sender reported a takeover")
	assert.Equal(t, 1, first.Seq)
	// A second proposal while the first is pending is refused.
	res = serverAs(newOperator()).do(http.MethodPost, "/v1/advanced/transfers/"+sent.Id.String()+"/reversal-proposals",
		map[string]any{"reason_code": "DISPUTE_UPHELD", "reason": "another view"}, "Idempotency-Key", key("second"))
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	// The proposer withdraws it.
	res = serverAs(proposer).do(http.MethodPost, "/v1/advanced/transfers/reversal-proposals/"+first.Id.String()+"/reject",
		map[string]any{"reason": "the sender withdrew the claim"}, "Idempotency-Key", key("reject"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rejected api.AdvancedTransferReversalDecisionResult
	res.json(&rejected)
	assert.Equal(t, api.TransferReversalRejected, rejected.Proposal.Status)
	assert.Equal(t, api.AdvancedTransferStatus("COMPLETED"), rejected.Proposal.Transfer.Status)
	res = serverAs(newOperator()).do(http.MethodPost, "/v1/advanced/transfers/reversal-proposals/"+first.Id.String()+"/approve",
		map[string]any{}, "Idempotency-Key", key("approve-rejected"))
	require.Equal(t, http.StatusConflict, res.Code, "a rejected proposal can never be executed; body=%s", res.Body.String())
	assert.Zero(t, compensations())
	assert.Equal(t, "25000000", wallet(bob))

	// --- many operators approve the next proposal at once ------------------------
	second := propose(newOperator(), "the sender reported a takeover again")
	assert.Equal(t, 2, second.Seq, "a rejected proposal leaves room for the next")
	const n = 6
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		h := serverAs(newOperator())
		k := key("race")
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := h.do(http.MethodPost, "/v1/advanced/transfers/reversal-proposals/"+second.Id.String()+"/approve",
				map[string]any{}, "Idempotency-Key", k)
			codes <- r.Code
		}()
	}
	wg.Wait()
	close(codes)
	byCode := map[int]int{}
	for c := range codes {
		byCode[c]++
	}
	assert.Equal(t, 1, byCode[http.StatusOK], "exactly one approval executes: %v", byCode)
	assert.Equal(t, n-1, byCode[http.StatusConflict], "every other approval finds the proposal decided: %v", byCode)
	assert.Equal(t, 1, compensations(), "one compensation journal")
	assert.Equal(t, "100000000", wallet(alice))
	assert.Equal(t, "0", wallet(bob))
	assert.Equal(t, 1, transferRowCount(t, d, `SELECT count(*) FROM transfer_reversal_decisions WHERE proposal_id = $1`, second.Id),
		"one decision per proposal")
}

func transferCustomer(user accounts.UserID, acct accounts.AccountID) security.Principal {
	return security.Principal{
		SubjectID: user.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{acct.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

// transferVerifiedIdentity records a VERIFIED compliance profile, the fact an identity
// verification provider would establish; Risk Kernel V2 reads it.
func transferVerifiedIdentity(t *testing.T, d *db.DB, user accounts.UserID) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := compliance.NewRepository(audit.NewWriterWithBuildVersion("httpapi-itest")).Upsert(ctx, tx, compliance.Profile{
			UserID: user, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear, VerifiedAt: &at,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "httpapi-itest-idv", Reason: "fixture: identity verified"})
		return err
	}))
}

func transferRowCount(t *testing.T, d *db.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(t.Context(), sql, args...).Scan(&n))
	return n
}
