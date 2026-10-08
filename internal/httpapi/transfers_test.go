package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxfund"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/transfers"
)

var (
	testP2PTransferID     = transfers.NewTransferID()
	testRefusedTransferID = transfers.NewTransferID()
	testFaucetGrantID     = sandboxfund.NewGrantID()
)

// fakeTransfersPort is the transfers port. It records every call so a test can
// prove what reached the service and that a replay did not.
type fakeTransfersPort struct {
	stubErr
	mu       sync.Mutex
	view     TransferView
	items    []TransferView
	next     string
	replayed bool
	report   transfers.ReconcileReport
	handle   transfers.Handle

	sends, handles, rules, modes int

	lastSend   TransferSend
	lastRule   TransferRecipientRule
	lastMode   transfers.SenderMode
	lastAcct   accounts.AccountID
	lastCursor string
	lastLimit  int
	lastScope  *assets.AssetID
	lastHandle string
}

func (f *fakeTransfersPort) Send(_ context.Context, r TransferSend) (TransferSent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends++
	f.lastSend = r
	if err := f.fail(); err != nil {
		return TransferSent{}, err
	}
	return TransferSent{View: f.view, Replayed: f.replayed}, nil
}

func (f *fakeTransfersPort) List(_ context.Context, acct accounts.AccountID, cursor string, limit int) ([]TransferView, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAcct, f.lastCursor, f.lastLimit = acct, cursor, limit
	return f.items, f.next, f.fail()
}

func (f *fakeTransfersPort) Get(context.Context, transfers.TransferID) (TransferView, error) {
	if err := f.fail(); err != nil {
		return TransferView{}, err
	}
	return f.view, nil
}

func (f *fakeTransfersPort) Reconcile(_ context.Context, asset *assets.AssetID) (transfers.ReconcileReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastScope = asset
	return f.report, f.fail()
}

func (f *fakeTransfersPort) RegisterHandle(_ context.Context, acct accounts.AccountID, handle string) (transfers.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handles++
	f.lastAcct, f.lastHandle = acct, handle
	return f.handle, f.fail()
}

func (f *fakeTransfersPort) SetRecipientRule(_ context.Context, r TransferRecipientRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules++
	f.lastRule = r
	return f.fail()
}

func (f *fakeTransfersPort) SetSenderMode(_ context.Context, acct accounts.AccountID, mode transfers.SenderMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modes++
	f.lastAcct, f.lastMode = acct, mode
	return f.fail()
}

// fakeFaucetPort is the sandbox faucet port.
type fakeFaucetPort struct {
	stubErr
	mu        sync.Mutex
	grant     FaucetGrantView
	allowance FaucetAllowance
	grants    int
	lastReq   FaucetRequest
	lastAcct  accounts.AccountID
}

func (f *fakeFaucetPort) Grant(_ context.Context, r FaucetRequest) (FaucetGrantView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants++
	f.lastReq = r
	if err := f.fail(); err != nil {
		return FaucetGrantView{}, err
	}
	return f.grant, nil
}

func (f *fakeFaucetPort) Allowance(_ context.Context, acct accounts.AccountID) (FaucetAllowance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAcct = acct
	if err := f.fail(); err != nil {
		return FaucetAllowance{}, err
	}
	return f.allowance, nil
}

// sampleTransferRecord is a COMPLETED transfer of 12.345678 sandbox dollars from
// testAccountID to testOtherAcct, carrying every decision artefact.
func sampleTransferRecord(handle string) transfers.Transfer {
	rid := testDecisionID()
	eff := econ.NewEffectID()
	jid := ledger.NewTransactionID()
	decided := testNow
	return transfers.Transfer{
		ID: testP2PTransferID, IntentID: testEconIntentID, SenderAccountID: testAccountID, SenderUserID: testUserID,
		RecipientAccountID: testOtherAcct, RecipientHandle: handle, AssetID: testAssetID,
		Quantity: qty("12345678"), NotionalUSD: usd("12.35"), Mode: gates.ModeSandbox, Memo: "rent for october",
		IdempotencyKey: "send-key-000001", PolicyVersion: "transfers-sandbox-v1", PolicyHash: "ph1",
		Status: transfers.StatusCompleted, ReasonCodes: []string{}, RiskDecisionID: &rid, EffectID: &eff,
		JournalTransactionID: &jid, EvidenceRef: "proof://advanced/p2p/x.json", EvidenceSHA256: []byte{0xbe, 0xef},
		DecidedAt: &decided, CreatedAt: testNow, UpdatedAt: testNow,
	}
}

// sampleRefusedTransfer is a refused attempt from testAccountID to testOtherAcct.
func sampleRefusedTransfer() transfers.Transfer {
	t := sampleTransferRecord("")
	t.ID, t.Status = testRefusedTransferID, transfers.StatusRejected
	t.RejectionCode = transfers.RejectComplianceBlocked
	t.ReasonCodes = []string{transfers.RejectComplianceBlocked, "SANCTIONS_LIST_MATCH"}
	t.EffectID, t.JournalTransactionID = nil, nil
	return t
}

func transferViewOf(t transfers.Transfer) TransferView {
	return TransferView{Transfer: t, AssetSymbol: "SBX-USD", AssetDecimals: 6}
}

func newFakeTransfersPort() *fakeTransfersPort {
	return &fakeTransfersPort{
		view:  transferViewOf(sampleTransferRecord("")),
		items: []TransferView{transferViewOf(sampleTransferRecord("")), transferViewOf(sampleRefusedTransfer())},
		next:  "cursor-2",
		handle: transfers.Handle{
			ID: "0193b2e0-0000-7000-8000-0000000000e1", AccountID: testAccountID, Handle: "alice", CreatedAt: testNow,
		},
		report: transfers.ReconcileReport{
			CheckedAt: testNow, Transfers: 3,
			Assets: []transfers.AssetIdentity{{
				AssetID: testAssetID.String(), TransferOut: qty("12345678"), TransferIn: qty("12345678"),
				RegisterCompleted: qty("12345678"), WalletDelta: qty("0"),
			}},
			Discrepancies: []transfers.Discrepancy{{
				Code: transfers.DiscRegisterLedger, AccountID: testAccountID.String(), AssetID: testAssetID.String(),
				Detail: "TRANSFER_OUT is 12345678 in the ledger and 0 in the register",
			}},
		},
	}
}

func newFakeFaucetPort() *fakeFaucetPort {
	policy := sandboxfund.DefaultPolicy()
	return &fakeFaucetPort{
		grant: FaucetGrantView{
			Grant: sandboxfund.Grant{
				ID: testFaucetGrantID, AccountID: testAccountID, AssetID: econ.SandboxDollarID(), Quantity: qty("2500000000"),
				IdempotencyKey: "grant-key-000001", PolicyVersion: policy.Version, PolicyHash: "fh1",
				Environment: config.EnvTest, ActorType: security.ActorUser, ActorID: testUserID.String(),
				EffectID: econ.NewEffectID(), JournalTransactionID: ledger.NewTransactionID(),
				EvidenceRef: "proof://advanced/sandboxfund/g.json", EvidenceSHA256: []byte{0xca, 0xfe}, GrantedAt: testNow,
			},
			AssetSymbol: "SBX-USD", AssetDecimals: 6,
		},
		allowance: FaucetAllowance{
			AccountID: testAccountID, AssetID: econ.SandboxDollarID(), AssetSymbol: "SBX-USD", AssetDecimals: 6,
			Cap: qty("100000000000"), Used: qty("2500000000"), Remaining: qty("97500000000"),
			Policy: policy, PolicyHash: "fh1", Environment: "TEST",
		},
	}
}

func newTransfersHarness(t *testing.T) (*harness, *fakeTransfersPort, *fakeFaucetPort) {
	t.Helper()
	h := newHarness(t)
	ft, ff := newFakeTransfersPort(), newFakeFaucetPort()
	ports := h.ports.ports()
	ports.Transfers, ports.SandboxFaucet = ft, ff
	h.server.opts.Ports = ports
	return h, ft, ff
}

// transferRecipientPrincipal owns testOtherAcct, the recipient of the samples.
func transferRecipientPrincipal() security.Principal {
	p := customerPrincipal()
	p.SubjectID = accounts.NewUserID().String()
	p.AccountIDs = []string{testOtherAcct.String()}
	return p
}

// transferStrangerPrincipal owns an account that is party to nothing here.
func transferStrangerPrincipal() security.Principal {
	p := customerPrincipal()
	p.SubjectID = accounts.NewUserID().String()
	p.AccountIDs = []string{accounts.NewAccountID().String()}
	return p
}

func transferSupportPrincipal() security.Principal {
	p := operatorPrincipal()
	p.Roles = []security.Role{security.RoleSupportReadOnly}
	return p
}

func transferSendBody() map[string]any {
	return map[string]any{
		"account_id": testAccountID.String(), "recipient_handle": "@Bob", "amount": "12.345678",
		"mode": "SANDBOX", "memo": "rent for october",
	}
}

func TestTransfers_SendCarriesTheExactFigureAndRendersTheSimulatedTransfer(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	res := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", "send-key-000001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	// What reached the service: the decimal exactly as typed, the handle as
	// typed (the service normalizes it), the sandbox dollar by default.
	require.Equal(t, 1, ft.sends)
	got := ft.lastSend
	assert.Equal(t, testAccountID, got.SenderAccountID)
	assert.Equal(t, "12.345678", got.Amount)
	assert.Nil(t, got.Quantity)
	assert.Equal(t, "@Bob", got.RecipientHandle)
	assert.True(t, got.RecipientAccountID.IsZero())
	assert.True(t, got.AssetID.IsZero(), "an omitted asset is left for the adapter to make the sandbox dollar")
	assert.Equal(t, gates.ModeSandbox, got.Mode)
	assert.Equal(t, "rent for october", got.Memo)
	assert.Equal(t, "send-key-000001", got.IdempotencyKey, "the service's own replay key is the client's key")

	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "12345678", raw["quantity"], "base units, as an exact string")
	assert.Equal(t, "12.35", raw["notional_usd"])
	assert.Equal(t, "SBX-USD", raw["asset_symbol"])
	assert.EqualValues(t, 6, raw["asset_decimals"])
	assert.Equal(t, "COMPLETED", raw["status"])
	assert.Equal(t, "rent for october", raw["memo"])
	assert.Equal(t, testOtherAcct.String(), raw["recipient_account_id"], "named by account id, so the sender knew it")
	assert.Equal(t, "beef", raw["evidence_sha256"])
}

func TestTransfers_SendByQuantityPassesExactBaseUnits(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	body := map[string]any{
		"account_id": testAccountID.String(), "recipient_account_id": testOtherAcct.String(),
		"asset_id": testAssetID.String(), "quantity": "123456789012345678901234567890", "mode": "PAPER",
	}
	res := h.do(http.MethodPost, "/v1/advanced/transfers", body, "Idempotency-Key", "send-key-000002")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	require.NotNil(t, ft.lastSend.Quantity)
	assert.Equal(t, "123456789012345678901234567890", ft.lastSend.Quantity.String(), "no float, no rounding")
	assert.Empty(t, ft.lastSend.Amount)
	assert.Equal(t, testOtherAcct, ft.lastSend.RecipientAccountID)
	assert.Equal(t, testAssetID, ft.lastSend.AssetID)
	assert.Equal(t, gates.ModePaper, ft.lastSend.Mode)
}

func TestTransfers_SendRefusesAMalformedValueBeforeTheService(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(map[string]any){
		"both":             func(b map[string]any) { b["quantity"] = "12345678" },
		"neither":          func(b map[string]any) { delete(b, "amount") },
		"decimal quantity": func(b map[string]any) { delete(b, "amount"); b["quantity"] = "1.5" },
		"float quantity":   func(b map[string]any) { delete(b, "amount"); b["quantity"] = "1e6" },
		"no body account":  func(b map[string]any) { b["account_id"] = "not-a-uuid" },
		"bad recipient id": func(b map[string]any) { b["recipient_account_id"] = "00000000-0000-0000-0000-000000000000" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, ft, _ := newTransfersHarness(t)
			body := transferSendBody()
			mutate(body)
			res := h.do(http.MethodPost, "/v1/advanced/transfers", body, "Idempotency-Key", "send-key-bad-0001")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
			assert.Zero(t, ft.sends, "a malformed request never reaches the service")
		})
	}
}

func TestTransfers_SendIsReplayedNotReExecuted(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	const key = "send-key-replay-01"
	first := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", key)
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())
	second := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())
	var a, b api.AdvancedTransfer
	first.json(&a)
	second.json(&b)
	assert.Equal(t, a, b, "a replay reproduces the original response")
	assert.Equal(t, 1, ft.sends, "a replay must not reach the service")

	changed := transferSendBody()
	changed["amount"] = "12.345679"
	res := h.do(http.MethodPost, "/v1/advanced/transfers", changed, "Idempotency-Key", key)
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)
	assert.Equal(t, 1, ft.sends)
}

// A service that already decided the key (its own replay, e.g. after the
// HTTP record expired) answers 200: nothing new was created.
func TestTransfers_AServiceReplayIsNotACreation(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	ft.replayed = true
	res := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", "send-key-svc-replay")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

// A refusal the service records reaches the client as its stable code with
// the transfer's id and every reason, and a replay reproduces it without
// asking the service again.
func TestTransfers_ARecordedRefusalKeepsItsCodeAndIsReplayed(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	ft.err = errs.New(errs.CodeRiskRejected, "transfer refused: RISK_IDENTITY_UNVERIFIED").
		WithField("transfer_id", testRefusedTransferID.String()).
		WithField("rejection_code", "RISK_IDENTITY_UNVERIFIED").
		WithField("reason_codes", []string{"RISK_IDENTITY_UNVERIFIED"})
	const key = "send-key-refused-1"
	for i := 0; i < 2; i++ {
		res := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", key)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeRiskRejected, p.Code)
		assert.Equal(t, testRefusedTransferID.String(), p.Fields["transfer_id"])
		assert.Equal(t, "RISK_IDENTITY_UNVERIFIED", p.Fields["rejection_code"])
		assert.Equal(t, []any{"RISK_IDENTITY_UNVERIFIED"}, p.Fields["reason_codes"])
	}
	assert.Equal(t, 1, ft.sends, "the recorded refusal is replayed, not re-decided")
}

// Writing is ownership-only: neither a stranger nor an operator holding
// account:read_any may send from someone else's account.
func TestTransfers_SendIsOwnershipScoped(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]security.Principal{
		"stranger": transferStrangerPrincipal(), "operator": operatorPrincipal(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, ft, _ := newTransfersHarness(t)
			h.as(&p)
			res := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", "send-key-foreign1")
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)
			assert.Zero(t, ft.sends)
		})
	}
}

// The handle is resolved on the server and stays there: a sender who paid a
// handle never sees the account id behind it, in the answer or in a read.
func TestTransfers_TheSenderNeverSeesTheAccountBehindAHandle(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	ft.view = transferViewOf(sampleTransferRecord("bob"))
	res := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", "send-key-handle-01")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.NotContains(t, raw, "recipient_account_id")
	assert.Equal(t, "bob", raw["recipient_handle"])
	assert.NotContains(t, res.Body.String(), testOtherAcct.String())

	res = h.do(http.MethodGet, "/v1/advanced/transfers/"+testP2PTransferID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.NotContains(t, res.Body.String(), testOtherAcct.String())

	// An operator sees the whole record.
	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/"+testP2PTransferID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct.String(), res.raw()["recipient_account_id"])
}

func TestTransfers_ListIsScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/transfers?account_id="+testAccountID.String()+"&limit=5000&cursor=c-1", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, ft.lastAcct)
	assert.Equal(t, maxPageLimit, ft.lastLimit, "the limit is clamped")
	assert.Equal(t, "c-1", ft.lastCursor)
	var page api.AdvancedTransferPage
	res.json(&page)
	assert.Equal(t, api.SIMULATED, page.ValueDomain)
	require.Len(t, page.Items, 2, "the sender sees its refused attempt too")
	assert.Equal(t, api.AdvancedTransferStatus("REJECTED"), page.Items[1].Status)
	require.NotNil(t, page.Items[1].RejectionCode)
	assert.Equal(t, transfers.RejectComplianceBlocked, *page.Items[1].RejectionCode)
	next, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "cursor-2", next)

	res = h.do(http.MethodGet, "/v1/advanced/transfers?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/transfers?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, ft.lastAcct)
}

// A recipient sees what reached it -- not the sender's refused attempts, not
// the sender's decision record, not the operator's reversal reason.
func TestTransfers_TheRecipientSeesWhatReachedItAndNotTheSendersDecision(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	rp := transferRecipientPrincipal()
	h.as(&rp)

	res := h.do(http.MethodGet, "/v1/advanced/transfers?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.AdvancedTransferPage
	res.json(&page)
	require.Len(t, page.Items, 1, "the refused attempt is the sender's record")
	assert.Equal(t, testP2PTransferID.String(), page.Items[0].Id.String())
	assert.NotContains(t, res.Body.String(), "SANCTIONS_LIST_MATCH")
	next, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "cursor-2", next, "the cursor still moves past what was withheld")

	res = h.do(http.MethodGet, "/v1/advanced/transfers/"+testP2PTransferID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	for _, field := range []string{"risk_decision_id", "evidence_ref", "evidence_sha256", "rejection_code", "reversal_reason_code"} {
		assert.NotContains(t, raw, field)
	}
	assert.Equal(t, []any{}, raw["reason_codes"])
	assert.Equal(t, "rent for october", raw["memo"], "the memo is for the recipient")
	assert.Equal(t, "12345678", raw["quantity"])

	ft.view = transferViewOf(sampleRefusedTransfer())
	refused := h.do(http.MethodGet, "/v1/advanced/transfers/"+testRefusedTransferID.String(), nil)
	ft.view, ft.err = TransferView{}, transferNotFound(testRefusedTransferID)
	absent := h.do(http.MethodGet, "/v1/advanced/transfers/"+testRefusedTransferID.String(), nil)
	requireSameTransferNotFound(t, refused, absent)
}

// A stranger asking for a transfer by id gets exactly the answer an absent id
// gets: the refusal does not confirm the id exists.
func TestTransfers_ByIDIsNotFoundForAStranger(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	sp := transferStrangerPrincipal()
	h.as(&sp)
	foreign := h.do(http.MethodGet, "/v1/advanced/transfers/"+testP2PTransferID.String(), nil)
	ft.err = transferNotFound(testP2PTransferID)
	absent := h.do(http.MethodGet, "/v1/advanced/transfers/"+testP2PTransferID.String(), nil)
	requireSameTransferNotFound(t, foreign, absent)

	res := h.do(http.MethodGet, "/v1/advanced/transfers/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

func requireSameTransferNotFound(t *testing.T, a, b *response) {
	t.Helper()
	require.Equal(t, http.StatusNotFound, a.Code, "body=%s", a.Body.String())
	require.Equal(t, http.StatusNotFound, b.Code, "body=%s", b.Body.String())
	pa, pb := a.problem(), b.problem()
	assert.Equal(t, errs.CodeNotFound, pa.Code)
	assert.Equal(t, pb.Code, pa.Code)
	assert.Equal(t, pb.Detail, pa.Detail)
	assert.Equal(t, pb.Fields, pa.Fields)
}

func TestTransfers_AudienceIsDecidedPerReader(t *testing.T) {
	t.Parallel()
	byHandle := sampleTransferRecord("bob")
	byAccount := sampleTransferRecord("")
	rejected := sampleRefusedTransfer()
	reversed := sampleTransferRecord("")
	reversed.Status, reversed.ReversalReasonCode = transfers.StatusReversed, "FRAUD_CONFIRMED"
	both := customerPrincipal()
	both.AccountIDs = []string{testAccountID.String(), testOtherAcct.String()}

	cases := []struct {
		name string
		p    security.Principal
		t    transfers.Transfer
		want transferAudience
	}{
		{"sender by account", customerPrincipal(), byAccount, transferOperatorAudience},
		{"sender by handle", customerPrincipal(), byHandle, transferAudience{visible: true, decision: true}},
		{"sender owning both", both, byHandle, transferOperatorAudience},
		{"sender of a refusal", customerPrincipal(), rejected, transferOperatorAudience},
		{"recipient", transferRecipientPrincipal(), byHandle, transferAudience{visible: true, recipientAccount: true}},
		{"recipient of a reversal", transferRecipientPrincipal(), reversed, transferAudience{visible: true, recipientAccount: true}},
		{"recipient of a refusal", transferRecipientPrincipal(), rejected, transferAudience{}},
		{"operator", operatorPrincipal(), byHandle, transferOperatorAudience},
		{"support", transferSupportPrincipal(), rejected, transferOperatorAudience},
		{"stranger", transferStrangerPrincipal(), byAccount, transferAudience{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := security.WithPrincipal(context.Background(), c.p)
			assert.Equal(t, c.want, transferAudienceOf(ctx, c.t))
		})
	}
	assert.Equal(t, transferAudience{}, transferAudienceOf(context.Background(), byAccount), "no principal, nothing")
}

func TestTransfers_HandleRegistrationIsOwnedAndIdempotent(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	body := map[string]any{"account_id": testAccountID.String(), "handle": "@Alice"}
	res := h.do(http.MethodPost, "/v1/advanced/transfers/handles", body, "Idempotency-Key", "handle-key-00001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var first api.AdvancedTransferHandle
	res.json(&first)
	assert.Equal(t, "alice", first.Handle)
	assert.Equal(t, "0193b2e0-0000-7000-8000-0000000000e1", first.Id.String())
	assert.Equal(t, "@Alice", ft.lastHandle, "the service normalizes; the handler passes it as given")
	assert.Equal(t, testAccountID, ft.lastAcct)

	again := h.do(http.MethodPost, "/v1/advanced/transfers/handles", body, "Idempotency-Key", "handle-key-00001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, ft.handles)

	body["handle"] = "@Alicia"
	res = h.do(http.MethodPost, "/v1/advanced/transfers/handles", body, "Idempotency-Key", "handle-key-00001")
	require.Equal(t, http.StatusConflict, res.Code)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)

	foreign := map[string]any{"account_id": testOtherAcct.String(), "handle": "mallory"}
	res = h.do(http.MethodPost, "/v1/advanced/transfers/handles", foreign, "Idempotency-Key", "handle-key-00002")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	ft.err = errs.New(errs.CodeConflict, "that handle is taken, or this account already has one")
	res = h.do(http.MethodPost, "/v1/advanced/transfers/handles", body, "Idempotency-Key", "handle-key-00003")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeConflict, res.problem().Code)
	assert.Equal(t, 2, ft.handles)
}

func TestTransfers_RecipientRuleNamesExactlyOneRecipient(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	for name, body := range map[string]map[string]any{
		"neither": {"account_id": testAccountID.String(), "rule": "BLOCK"},
		"both": {
			"account_id": testAccountID.String(), "rule": "BLOCK",
			"recipient_handle": "bob", "recipient_account_id": testOtherAcct.String(),
		},
	} {
		res := h.do(http.MethodPost, "/v1/advanced/transfers/recipient-rules", body, "Idempotency-Key", "rule-key-bad-"+name)
		require.Equal(t, http.StatusBadRequest, res.Code, "%s: body=%s", name, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	}
	assert.Zero(t, ft.rules)

	body := map[string]any{"account_id": testAccountID.String(), "rule": "BLOCK", "recipient_handle": "@Bob", "reason": "spam"}
	res := h.do(http.MethodPost, "/v1/advanced/transfers/recipient-rules", body, "Idempotency-Key", "rule-key-000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "bob", raw["recipient_handle"], "echoed as the rule is about it")
	assert.NotContains(t, raw, "recipient_account_id", "a handle is never resolved for the client")
	assert.Equal(t, "BLOCK", raw["rule"])
	assert.Equal(t, TransferRecipientRule{
		SenderAccountID: testAccountID, RecipientHandle: "@Bob", Rule: transfers.RuleBlock, Reason: "spam",
	}, ft.lastRule)

	again := h.do(http.MethodPost, "/v1/advanced/transfers/recipient-rules", body, "Idempotency-Key", "rule-key-000001")
	require.Equal(t, http.StatusOK, again.Code)
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, ft.rules)

	byAccount := map[string]any{"account_id": testAccountID.String(), "rule": "ALLOW", "recipient_account_id": testOtherAcct.String()}
	res = h.do(http.MethodPost, "/v1/advanced/transfers/recipient-rules", byAccount, "Idempotency-Key", "rule-key-000002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, ft.lastRule.RecipientAccountID)
	assert.Equal(t, transfers.RuleAllow, ft.lastRule.Rule)

	ft.err = errs.New(errs.CodeNotFound, "no account has that handle")
	res = h.do(http.MethodPost, "/v1/advanced/transfers/recipient-rules", body, "Idempotency-Key", "rule-key-000003")
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	foreign := map[string]any{"account_id": testOtherAcct.String(), "rule": "BLOCK", "recipient_handle": "bob"}
	res = h.do(http.MethodPost, "/v1/advanced/transfers/recipient-rules", foreign, "Idempotency-Key", "rule-key-000004")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

func TestTransfers_SenderModeIsRecordedOnce(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	body := map[string]any{"account_id": testAccountID.String(), "mode": "ALLOWLIST_ONLY"}
	res := h.do(http.MethodPost, "/v1/advanced/transfers/sender-mode", body, "Idempotency-Key", "mode-key-000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "ALLOWLIST_ONLY", res.raw()["mode"])
	assert.Equal(t, transfers.SenderAllowlistOnly, ft.lastMode)
	assert.Equal(t, testAccountID, ft.lastAcct)

	again := h.do(http.MethodPost, "/v1/advanced/transfers/sender-mode", body, "Idempotency-Key", "mode-key-000001")
	require.Equal(t, http.StatusOK, again.Code)
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, ft.modes)

	body["mode"] = "OPEN"
	res = h.do(http.MethodPost, "/v1/advanced/transfers/sender-mode", body, "Idempotency-Key", "mode-key-000001")
	require.Equal(t, http.StatusConflict, res.Code)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)

	// The service validates the mode; its refusal keeps its code.
	ft.err = errs.New(errs.CodeValidationFailed, `unknown sender mode "SOMETIMES"`)
	body["mode"] = "SOMETIMES"
	res = h.do(http.MethodPost, "/v1/advanced/transfers/sender-mode", body, "Idempotency-Key", "mode-key-000002")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

func TestTransfers_ReconciliationIsAnOperatorRead(t *testing.T) {
	t.Parallel()
	h, ft, _ := newTransfersHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/transfers/reconciliation", nil)
	require.Equal(t, http.StatusForbidden, res.Code, "a customer holds neither permission; body=%s", res.Body.String())

	support := transferSupportPrincipal()
	h.as(&support)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reconciliation?asset_id="+testAssetID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "reconciliation:read suffices; body=%s", res.Body.String())
	require.NotNil(t, ft.lastScope)
	assert.Equal(t, testAssetID, *ft.lastScope)
	var rep api.AdvancedTransferReconciliation
	res.json(&rep)
	assert.False(t, rep.Ok)
	assert.Equal(t, 3, rep.Transfers)
	assert.Equal(t, api.SIMULATED, rep.ValueDomain)
	require.Len(t, rep.Assets, 1)
	assert.Equal(t, "12345678", rep.Assets[0].TransferOut)
	assert.Equal(t, "0", rep.Assets[0].WalletDelta)
	require.Len(t, rep.Discrepancies, 1)
	assert.Equal(t, transfers.DiscRegisterLedger, rep.Discrepancies[0].Code)
	require.NotNil(t, rep.Discrepancies[0].AccountId)
	assert.Equal(t, testAccountID.String(), rep.Discrepancies[0].AccountId.String())
	assert.Nil(t, rep.Discrepancies[0].TransferId)

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reconciliation", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Nil(t, ft.lastScope, "no asset_id reconciles every asset")
}

func TestFaucet_AGrantIsOwnedExactAndIdempotent(t *testing.T) {
	t.Parallel()
	h, _, ff := newTransfersHarness(t)
	body := map[string]any{"account_id": testAccountID.String(), "amount": "2500.000000"}
	res := h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", body, "Idempotency-Key", "grant-key-000001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, FaucetRequest{AccountID: testAccountID, Amount: "2500.000000", IdempotencyKey: "grant-key-000001"}, ff.lastReq)
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "2500000000", raw["quantity"])
	assert.Equal(t, econ.SandboxDollarID().String(), raw["asset_id"])
	assert.Equal(t, "SBX-USD", raw["asset_symbol"])
	assert.Equal(t, "TEST", raw["environment"])
	assert.Equal(t, "cafe", raw["evidence_sha256"])

	again := h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", body, "Idempotency-Key", "grant-key-000001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, ff.grants)

	body["amount"] = "2500.000001"
	res = h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", body, "Idempotency-Key", "grant-key-000001")
	require.Equal(t, http.StatusConflict, res.Code)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)

	byQty := map[string]any{"account_id": testAccountID.String(), "quantity": "1"}
	res = h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", byQty, "Idempotency-Key", "grant-key-000002")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	require.NotNil(t, ff.lastReq.Quantity)
	assert.Equal(t, money.QuantityFromInt64(1), *ff.lastReq.Quantity)

	for name, bad := range map[string]map[string]any{
		"both":    {"account_id": testAccountID.String(), "amount": "1", "quantity": "1"},
		"neither": {"account_id": testAccountID.String()},
	} {
		res = h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", bad, "Idempotency-Key", "grant-key-bad-"+name)
		require.Equal(t, http.StatusBadRequest, res.Code, "%s: body=%s", name, res.Body.String())
	}
	foreign := map[string]any{"account_id": testOtherAcct.String(), "amount": "1"}
	res = h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", foreign, "Idempotency-Key", "grant-key-000003")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 2, ff.grants)

	// The faucet's refusals keep their codes: its cap, its environment.
	ff.err = errs.New(errs.CodeRateLimited, "the sandbox faucet cap for this account and asset is reached in the current window")
	res = h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", byQty, "Idempotency-Key", "grant-key-000004")
	require.Equal(t, http.StatusTooManyRequests, res.Code, "body=%s", res.Body.String())
	ff.err = errs.New(errs.CodeCapabilityNotApproved, "the sandbox faucet is not enabled in PROD")
	res = h.do(http.MethodPost, "/v1/advanced/transfers/faucet/grants", byQty, "Idempotency-Key", "grant-key-000005")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeCapabilityNotApproved, res.problem().Code)
}

func TestFaucet_TheAllowanceIsScopedAndExact(t *testing.T) {
	t.Parallel()
	h, _, ff := newTransfersHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/transfers/faucet/allowance?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, ff.lastAcct)
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "100000000000", raw["cap"])
	assert.Equal(t, "2500000000", raw["used"])
	assert.Equal(t, "97500000000", raw["remaining"])
	assert.EqualValues(t, sandboxfund.DefaultPolicy().WindowMS, raw["window_ms"])
	assert.Equal(t, "sandbox-faucet-v1", raw["policy_version"])

	res = h.do(http.MethodGet, "/v1/advanced/transfers/faucet/allowance?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/faucet/allowance?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, ff.lastAcct)

	ff.err = errs.New(errs.CodeCapabilityNotApproved, "the sandbox faucet is not enabled in PROD")
	res = h.do(http.MethodGet, "/v1/advanced/transfers/faucet/allowance?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
}

// A deployment without the services says so on every route, reads and
// writes alike, and never invents an answer.
func TestTransfers_UnwiredPortsAreUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	type call struct {
		method, path string
		body         any
		as           *security.Principal
	}
	transfer := "/v1/advanced/transfers/" + testP2PTransferID.String()
	for _, c := range []call{
		{http.MethodPost, "/v1/advanced/transfers", transferSendBody(), nil},
		{http.MethodGet, "/v1/advanced/transfers?account_id=" + testAccountID.String(), nil, nil},
		{http.MethodGet, transfer, nil, nil},
		{http.MethodPost, transfer + "/reversal-proposals", map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": "x"}, &op},
		{http.MethodGet, "/v1/advanced/transfers/reversal-proposals", nil, &op},
		{http.MethodGet, "/v1/advanced/transfers/reversal-proposals/" + testReversalProposalID.String(), nil, &op},
		{http.MethodPost, "/v1/advanced/transfers/reversal-proposals/" + testReversalProposalID.String() + "/approve", map[string]any{}, &op},
		{http.MethodPost, "/v1/advanced/transfers/reversal-proposals/" + testReversalProposalID.String() + "/reject", map[string]any{"reason": "x"}, &op},
		{http.MethodPost, "/v1/advanced/transfers/handles", map[string]any{"account_id": testAccountID.String(), "handle": "alice"}, nil},
		{http.MethodPost, "/v1/advanced/transfers/recipient-rules", map[string]any{"account_id": testAccountID.String(), "rule": "BLOCK", "recipient_handle": "bob"}, nil},
		{http.MethodPost, "/v1/advanced/transfers/sender-mode", map[string]any{"account_id": testAccountID.String(), "mode": "OPEN"}, nil},
		{http.MethodGet, "/v1/advanced/transfers/reconciliation", nil, &op},
		{http.MethodPost, "/v1/advanced/transfers/faucet/grants", map[string]any{"account_id": testAccountID.String(), "amount": "1"}, nil},
		{http.MethodGet, "/v1/advanced/transfers/faucet/allowance?account_id=" + testAccountID.String(), nil, nil},
	} {
		p := customerPrincipal()
		if c.as != nil {
			p = *c.as
		}
		h.as(&p)
		res := h.do(c.method, c.path, c.body, "Idempotency-Key", "unwired-key-00001")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s %s body=%s", c.method, c.path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, c.path)
	}
}

// The service names the recipient's account id in some refusals; when the
// sender named a handle, that field would resolve it, so it is removed and
// nothing else changes.
func TestTransfers_ARefusalNeverResolvesAHandle(t *testing.T) {
	t.Parallel()
	refusal := errs.New(errs.CodeAccountFrozen, "the recipient account is frozen").
		WithField("recipient_account_id", testOtherAcct.String()).WithField("asset_id", testAssetID.String())

	out := withoutRecipientAccount(refusal, TransferSend{RecipientHandle: "bob"})
	e, ok := errs.As(out)
	require.True(t, ok)
	assert.Equal(t, errs.CodeAccountFrozen, e.Code)
	assert.Equal(t, "the recipient account is frozen", e.Detail)
	assert.NotContains(t, e.Fields, "recipient_account_id")
	assert.Equal(t, testAssetID.String(), e.Fields["asset_id"])

	named := withoutRecipientAccount(refusal, TransferSend{RecipientAccountID: testOtherAcct})
	assert.Same(t, refusal, named, "a sender who named the account already knows it")

	h, ft, _ := newTransfersHarness(t)
	ft.err = out
	res := h.do(http.MethodPost, "/v1/advanced/transfers", transferSendBody(), "Idempotency-Key", "send-key-frozen-01")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeAccountFrozen, res.problem().Code)
	assert.NotContains(t, res.Body.String(), testOtherAcct.String())
}

// --- reversal under dual control -------------------------------------------------

var (
	testReversalProposalID = uuid.MustParse("0193b2e0-0000-7000-8000-0000000000f1")
	// testReversalApprover is a second operator, distinct from testUserID.
	testReversalApprover = "0193b2e0-0000-7000-8000-0000000000f2"
)

// fakeTransferReversalsPort is the reversals port. It records every call so
// a test can prove what reached the service and that a replay did not.
type fakeTransferReversalsPort struct {
	stubErr
	mu       sync.Mutex
	view     TransferReversalProposalView
	page     TransferReversalProposalPage
	replayed bool

	proposes, approvals, rejections int

	lastPropose  TransferReversalPropose
	lastDecide   TransferReversalDecide
	lastStatus   string
	lastTransfer *transfers.TransferID
	lastCursor   string
	lastLimit    int
	lastGet      uuid.UUID
}

func (f *fakeTransferReversalsPort) Propose(_ context.Context, r TransferReversalPropose) (TransferReversalOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposes++
	f.lastPropose = r
	if err := f.fail(); err != nil {
		return TransferReversalOutcome{}, err
	}
	return TransferReversalOutcome{View: f.view, Replayed: f.replayed}, nil
}

func (f *fakeTransferReversalsPort) Approve(_ context.Context, r TransferReversalDecide) (TransferReversalOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals++
	f.lastDecide = r
	if err := f.fail(); err != nil {
		return TransferReversalOutcome{}, err
	}
	return TransferReversalOutcome{View: f.view, Replayed: f.replayed}, nil
}

func (f *fakeTransferReversalsPort) Reject(_ context.Context, r TransferReversalDecide) (TransferReversalOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejections++
	f.lastDecide = r
	if err := f.fail(); err != nil {
		return TransferReversalOutcome{}, err
	}
	return TransferReversalOutcome{View: f.view, Replayed: f.replayed}, nil
}

func (f *fakeTransferReversalsPort) List(_ context.Context, status string, transfer *transfers.TransferID, cursor string, limit int) (TransferReversalProposalPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStatus, f.lastTransfer, f.lastCursor, f.lastLimit = status, transfer, cursor, limit
	return f.page, f.fail()
}

func (f *fakeTransferReversalsPort) Get(_ context.Context, id uuid.UUID) (TransferReversalProposalView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastGet = id
	if err := f.fail(); err != nil {
		return TransferReversalProposalView{}, err
	}
	return f.view, nil
}

// sampleReversalProposal is a proposal by testUserID to reverse the sample
// transfer, in the given derived status.
func sampleReversalProposal(status string) TransferReversalProposalView {
	tr := sampleTransferRecord("bob")
	v := TransferReversalProposalView{
		ID: testReversalProposalID, TransferID: tr.ID, Seq: 1, Status: status, ReasonCode: "FRAUD_CONFIRMED",
		Reason: "sender reported account takeover", ProposedBy: testUserID.String(), ProposedAt: testNow,
	}
	if status != "PENDING" {
		decided := testNow.Add(time.Minute)
		v.DecidedBy, v.DecidedAt = testReversalApprover, &decided
	}
	switch status {
	case "APPROVED":
		jid := ledger.NewTransactionID()
		at := testNow.Add(time.Minute)
		tr.Status, tr.ReversalJournalTransactionID, tr.ReversalReasonCode = transfers.StatusReversed, &jid, "FRAUD_CONFIRMED"
		tr.ReversedByActorID, tr.ReversedAt = testReversalApprover, &at
	case "REJECTED":
		v.DecisionReason = "the sender withdrew the claim"
	}
	v.Transfer = transferViewOf(tr)
	return v
}

func newTransferReversalsHarness(t *testing.T) (*harness, *fakeTransferReversalsPort) {
	t.Helper()
	h := newHarness(t)
	fr := &fakeTransferReversalsPort{view: sampleReversalProposal("PENDING")}
	ports := h.ports.ports()
	ports.TransferReversals = fr
	h.server.opts.Ports = ports
	op := operatorPrincipal()
	h.as(&op)
	return h, fr
}

func reversalProposalPath() string {
	return "/v1/advanced/transfers/" + testP2PTransferID.String() + "/reversal-proposals"
}

func reversalDecisionPath(verb string) string {
	return "/v1/advanced/transfers/reversal-proposals/" + testReversalProposalID.String() + "/" + verb
}

// Each step of a reversal is an operator's, under a recent step-up: a
// customer never reaches it and an operator whose strong authentication is
// stale is told to step up, before anything reaches the service.
func TestTransferReversals_EveryStepIsAnOperatorsUnderStepUp(t *testing.T) {
	t.Parallel()
	proposal := map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": "sender reported account takeover"}
	for name, call := range map[string]struct {
		path string
		body map[string]any
	}{
		"propose": {reversalProposalPath(), proposal},
		"approve": {reversalDecisionPath("approve"), map[string]any{}},
		"reject":  {reversalDecisionPath("reject"), map[string]any{"reason": "the sender withdrew the claim"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, fr := newTransferReversalsHarness(t)
			customer := customerPrincipal()
			h.as(&customer)
			res := h.do(http.MethodPost, call.path, call.body, "Idempotency-Key", "reversal-key-"+name+"-01")
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			stale := operatorPrincipal()
			stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
			h.as(&stale)
			res = h.do(http.MethodPost, call.path, call.body, "Idempotency-Key", "reversal-key-"+name+"-02")
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
			assert.Zero(t, fr.proposes+fr.approvals+fr.rejections, "nothing reaches the service")
		})
	}
}

// Finance holds ledger:post_correction, the authority the service requires of
// every step, without sandbox:operate: the routes admit it, so the two
// operators of a reversal can be any two holders of the correction authority.
func TestTransferReversals_TheCorrectionAuthorityReachesEveryStep(t *testing.T) {
	t.Parallel()
	h, fr := newTransferReversalsHarness(t)
	finance := operatorPrincipal()
	finance.Roles = []security.Role{security.RoleFinance}
	h.as(&finance)
	res := h.do(http.MethodPost, reversalProposalPath(), map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": "x"},
		"Idempotency-Key", "finance-key-00001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodPost, reversalDecisionPath("approve"), map[string]any{}, "Idempotency-Key", "finance-key-00002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodPost, reversalDecisionPath("reject"), map[string]any{"reason": "x"}, "Idempotency-Key", "finance-key-00003")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, fr.proposes)
	assert.Equal(t, 1, fr.approvals)
	assert.Equal(t, 1, fr.rejections)

	// Support reads but holds neither authority.
	support := transferSupportPrincipal()
	h.as(&support)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals", nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

func TestTransferReversals_AProposalCarriesTheOperatorsWordsAndIsReplayedNotReExecuted(t *testing.T) {
	t.Parallel()
	h, fr := newTransferReversalsHarness(t)
	body := map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": "sender reported account takeover"}
	res := h.do(http.MethodPost, reversalProposalPath(), body, "Idempotency-Key", "propose-key-00001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	require.Equal(t, 1, fr.proposes)
	assert.Equal(t, testP2PTransferID, fr.lastPropose.TransferID)
	assert.Equal(t, "FRAUD_CONFIRMED", fr.lastPropose.ReasonCode)
	assert.Equal(t, "sender reported account takeover", fr.lastPropose.Reason)

	raw := res.raw()
	assert.Equal(t, testReversalProposalID.String(), raw["id"])
	assert.Equal(t, "PENDING", raw["status"])
	assert.EqualValues(t, 1, raw["seq"])
	assert.Equal(t, testUserID.String(), raw["proposed_by"])
	assert.NotContains(t, raw, "decided_by")
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	tr, ok := raw["transfer"].(map[string]any)
	require.True(t, ok, "the proposal carries its transfer")
	assert.Equal(t, "COMPLETED", tr["status"])
	assert.Equal(t, "12345678", tr["quantity"], "exact base units")
	assert.Equal(t, testOtherAcct.String(), tr["recipient_account_id"], "an operator sees the whole record")

	again := h.do(http.MethodPost, reversalProposalPath(), body, "Idempotency-Key", "propose-key-00001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, fr.proposes, "a replay must not reach the service")

	body["reason"] = "a different story"
	res = h.do(http.MethodPost, reversalProposalPath(), body, "Idempotency-Key", "propose-key-00001")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)
	assert.Equal(t, 1, fr.proposes)

	// The service's own replay (the same proposal already pending) creates nothing.
	fr.replayed = true
	res = h.do(http.MethodPost, reversalProposalPath(), body, "Idempotency-Key", "propose-key-00002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

func TestTransferReversals_AProposalIsValidatedBeforeTheService(t *testing.T) {
	t.Parallel()
	h, fr := newTransferReversalsHarness(t)
	res := h.do(http.MethodPost, reversalProposalPath(), nil, "Idempotency-Key", "propose-key-bad01")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodPost, "/v1/advanced/transfers/not-a-uuid/reversal-proposals",
		map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": "x"}, "Idempotency-Key", "propose-key-bad02")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodPost, reversalDecisionPath("reject"), nil, "Idempotency-Key", "reject-key-bad001")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Zero(t, fr.proposes+fr.rejections, "a malformed request never reaches the service")
}

// The service's refusals keep their codes: a state the transfer cannot be
// reversed from, a second pending proposal, the proposer's own approval and
// a recipient who no longer holds the funds.
func TestTransferReversals_TheServicesRefusalsKeepTheirCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path string
		body       map[string]any
		err        error
		status     int
		rejection  string
	}{
		{"not completed", reversalProposalPath(), map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": "x"},
			errs.New(errs.CodeInvalidStateTransition, "a REVERSED transfer cannot be reversed"), http.StatusConflict, ""},
		{"pending elsewhere", reversalProposalPath(), map[string]any{"reason_code": "FRAUD_CONFIRMED", "reason": "x"},
			errs.New(errs.CodeConflict, "a proposal is pending").WithField("proposal_id", testReversalProposalID.String()), http.StatusConflict, ""},
		{"self approval", reversalDecisionPath("approve"), map[string]any{},
			errs.New(errs.CodeForbidden, "the proposer cannot approve").WithField("rejection_code", "TRANSFER_REVERSAL_SELF_APPROVAL"),
			http.StatusForbidden, "TRANSFER_REVERSAL_SELF_APPROVAL"},
		{"funds gone", reversalDecisionPath("approve"), map[string]any{"reason": "confirmed"},
			errs.New(errs.CodeConflict, "the recipient no longer holds the funds").WithField("rejection_code", "TRANSFER_REVERSAL_FUNDS_UNAVAILABLE"),
			http.StatusConflict, "TRANSFER_REVERSAL_FUNDS_UNAVAILABLE"},
		{"already decided", reversalDecisionPath("reject"), map[string]any{"reason": "x"},
			errs.New(errs.CodeConflict, "already APPROVED").WithField("decision", "APPROVED"), http.StatusConflict, ""},
		{"unknown proposal", reversalDecisionPath("approve"), map[string]any{},
			errs.New(errs.CodeNotFound, "reversal proposal not found"), http.StatusNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, fr := newTransferReversalsHarness(t)
			fr.err = c.err
			for i := 0; i < 2; i++ {
				res := h.do(http.MethodPost, c.path, c.body, "Idempotency-Key", "refusal-key-0001")
				require.Equal(t, c.status, res.Code, "body=%s", res.Body.String())
				p := res.problem()
				assert.Equal(t, errs.CodeOf(c.err), p.Code)
				if c.rejection != "" {
					assert.Equal(t, c.rejection, p.Fields["rejection_code"])
				}
			}
			assert.Equal(t, 1, fr.proposes+fr.approvals+fr.rejections, "a recorded refusal is replayed, not re-decided")
		})
	}
}

func TestTransferReversals_AnApprovalExecutesOnceAndRendersTheReversedTransfer(t *testing.T) {
	t.Parallel()
	h, fr := newTransferReversalsHarness(t)
	fr.view = sampleReversalProposal("APPROVED")
	approver := operatorPrincipal()
	approver.SubjectID = testReversalApprover
	h.as(&approver)
	res := h.do(http.MethodPost, reversalDecisionPath("approve"), map[string]any{"reason": "evidence reviewed"},
		"Idempotency-Key", "approve-key-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Equal(t, 1, fr.approvals)
	assert.Equal(t, testReversalProposalID, fr.lastDecide.ProposalID)
	assert.Equal(t, "evidence reviewed", fr.lastDecide.Reason)

	var out api.AdvancedTransferReversalDecisionResult
	res.json(&out)
	assert.False(t, out.Replayed)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.Equal(t, api.TransferReversalApproved, out.Proposal.Status)
	require.NotNil(t, out.Proposal.DecidedBy)
	assert.Equal(t, testReversalApprover, *out.Proposal.DecidedBy)
	assert.Equal(t, api.TransferStatusReversed, out.Proposal.Transfer.Status)
	require.NotNil(t, out.Proposal.Transfer.ReversalReasonCode)
	assert.Equal(t, "FRAUD_CONFIRMED", *out.Proposal.Transfer.ReversalReasonCode)
	require.NotNil(t, out.Proposal.Transfer.ReversalJournalTransactionId)

	again := h.do(http.MethodPost, reversalDecisionPath("approve"), map[string]any{"reason": "evidence reviewed"},
		"Idempotency-Key", "approve-key-00001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, fr.approvals, "a replay must not reach the service")

	// An approval without a body is an approval without a note.
	res = h.do(http.MethodPost, reversalDecisionPath("approve"), nil, "Idempotency-Key", "approve-key-00002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Empty(t, fr.lastDecide.Reason)

	// The service's own replay of the same decision is reported as such.
	fr.replayed = true
	res = h.do(http.MethodPost, reversalDecisionPath("approve"), map[string]any{}, "Idempotency-Key", "approve-key-00003")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["replayed"])
}

func TestTransferReversals_ARejectionCarriesItsReasonAndMovesNothing(t *testing.T) {
	t.Parallel()
	h, fr := newTransferReversalsHarness(t)
	fr.view = sampleReversalProposal("REJECTED")
	res := h.do(http.MethodPost, reversalDecisionPath("reject"), map[string]any{"reason": "the sender withdrew the claim"},
		"Idempotency-Key", "reject-key-000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Equal(t, 1, fr.rejections)
	assert.Zero(t, fr.approvals)
	assert.Equal(t, "the sender withdrew the claim", fr.lastDecide.Reason)
	var out api.AdvancedTransferReversalDecisionResult
	res.json(&out)
	assert.Equal(t, api.TransferReversalRejected, out.Proposal.Status)
	assert.Equal(t, api.TransferStatusCompleted, out.Proposal.Transfer.Status, "a rejection reverses nothing")
	require.NotNil(t, out.Proposal.DecisionReason)
	assert.Equal(t, "the sender withdrew the claim", *out.Proposal.DecisionReason)
}

func TestTransferReversals_ProposalsAreAnOperatorRead(t *testing.T) {
	t.Parallel()
	h, fr := newTransferReversalsHarness(t)
	fr.page = TransferReversalProposalPage{
		Items: []TransferReversalProposalView{sampleReversalProposal("PENDING"), sampleReversalProposal("REJECTED")}, NextCursor: "next-1",
	}
	customer := customerPrincipal()
	h.as(&customer)
	res := h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals", nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals?status=PENDING&transfer_id="+
		testP2PTransferID.String()+"&cursor=c-1&limit=7", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "PENDING", fr.lastStatus)
	require.NotNil(t, fr.lastTransfer)
	assert.Equal(t, testP2PTransferID, *fr.lastTransfer)
	assert.Equal(t, "c-1", fr.lastCursor)
	assert.Equal(t, 7, fr.lastLimit)
	var page api.AdvancedTransferReversalProposalPage
	res.json(&page)
	require.Len(t, page.Items, 2)
	assert.Equal(t, api.SIMULATED, page.ValueDomain)
	next, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "next-1", next)

	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Empty(t, fr.lastStatus, "no status lists every status")
	assert.Nil(t, fr.lastTransfer)

	for _, q := range []string{"status=EXECUTED", "transfer_id=not-a-uuid"} {
		res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals?"+q, nil)
		require.Equal(t, http.StatusBadRequest, res.Code, "%s body=%s", q, res.Body.String())
	}

	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals/"+testReversalProposalID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testReversalProposalID, fr.lastGet)

	// Absent and not-readable answer the same.
	fr.err = errs.New(errs.CodeNotFound, "reversal proposal not found")
	res = h.do(http.MethodGet, "/v1/advanced/transfers/reversal-proposals/"+testReversalProposalID.String(), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testReversalProposalID.String(), res.problem().Fields["proposal_id"])
}

// A status the API does not declare is a fault of the adapter, never
// rendered as one it does; a principal who may not see the transfer is told
// the proposal does not exist.
func TestTransferReversals_AnUndeclaredStatusIsNeverRendered(t *testing.T) {
	t.Parallel()
	ctx := security.WithPrincipal(context.Background(), operatorPrincipal())
	v := sampleReversalProposal("PENDING")
	v.Status = "EXECUTED"
	_, err := toAPITransferReversalProposal(ctx, v)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))

	stranger := security.WithPrincipal(context.Background(), transferStrangerPrincipal())
	_, err = toAPITransferReversalProposal(stranger, sampleReversalProposal("PENDING"))
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}
