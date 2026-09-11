package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/webhook"
	"github.com/nodal/controlplane/internal/withdrawal"
)

// The doubles below stand in for the domain services so the real router, the
// real middleware chain and the real error contract can be exercised without a
// database. Each one can be told to fail with a specific errs.Code, which is
// how the business-rejection table test drives every code to the wire.

type stubErr struct{ err error }

func (s *stubErr) fail() error { return s.err }

type fakeIdentity struct {
	stubErr
	begun     identity.BeginResult
	complete  identity.Completed
	loggedIn  bool
	loggedOut bool
	lastBegin identity.BeginRequest
}

func (f *fakeIdentity) Begin(_ context.Context, req identity.BeginRequest) (identity.BeginResult, error) {
	if err := f.fail(); err != nil {
		return identity.BeginResult{}, err
	}
	f.lastBegin = req
	return f.begun, nil
}

func (f *fakeIdentity) Complete(context.Context, identity.CompleteRequest) (identity.Completed, error) {
	if err := f.fail(); err != nil {
		return identity.Completed{}, err
	}
	f.loggedIn = true
	return f.complete, nil
}

func (f *fakeIdentity) Logout(context.Context, auth.Session, identity.CompleteRequest) error {
	if err := f.fail(); err != nil {
		return err
	}
	f.loggedOut = true
	return nil
}

type fakeSessions struct {
	stubErr
	items   []auth.Summary
	revoked []string
}

func (f *fakeSessions) ListForSubject(context.Context, string) ([]auth.Summary, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.items, nil
}

func (f *fakeSessions) Revoke(_ context.Context, id string) error {
	if err := f.fail(); err != nil {
		return err
	}
	f.revoked = append(f.revoked, id)
	return nil
}

type fakeAccounts struct {
	stubErr
	account     accounts.Account
	owned       []accounts.Account
	page        AccountPage
	transitions []accounts.StatusChange
}

func (f *fakeAccounts) Get(context.Context, accounts.AccountID) (accounts.Account, error) {
	if err := f.fail(); err != nil {
		return accounts.Account{}, err
	}
	return f.account, nil
}

func (f *fakeAccounts) ListByOwner(context.Context, accounts.UserID) ([]accounts.Account, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.owned, nil
}

func (f *fakeAccounts) Search(context.Context, string, string, int) (AccountPage, error) {
	if err := f.fail(); err != nil {
		return AccountPage{}, err
	}
	return f.page, nil
}

func (f *fakeAccounts) Transition(_ context.Context, _ accounts.AccountID, ch accounts.StatusChange) (accounts.Account, error) {
	if err := f.fail(); err != nil {
		return accounts.Account{}, err
	}
	f.transitions = append(f.transitions, ch)
	out := f.account
	out.Status = ch.To
	out.StatusReason = ch.Reason
	return out, nil
}

type fakeBuyingPower struct {
	stubErr
	value buyingpower.BuyingPower
}

func (f *fakeBuyingPower) Compute(context.Context, accounts.AccountID, buyingpower.Purpose) (buyingpower.BuyingPower, error) {
	if err := f.fail(); err != nil {
		return buyingpower.BuyingPower{}, err
	}
	return f.value, nil
}

type fakeHoldings struct {
	stubErr
	value HoldingsView
}

func (f *fakeHoldings) Holdings(context.Context, accounts.AccountID) (HoldingsView, error) {
	if err := f.fail(); err != nil {
		return HoldingsView{}, err
	}
	return f.value, nil
}

type fakeLedger struct {
	stubErr
	txs  []ledger.Transaction
	next string
}

func (f *fakeLedger) ListTransactions(context.Context, ledger.TransactionFilter, string, int) ([]ledger.Transaction, string, error) {
	if err := f.fail(); err != nil {
		return nil, "", err
	}
	return f.txs, f.next, nil
}

type fakeActivity struct {
	stubErr
	page ActivityPage
}

func (f *fakeActivity) Activity(context.Context, accounts.AccountID, string, int) (ActivityPage, error) {
	if err := f.fail(); err != nil {
		return ActivityPage{}, err
	}
	return f.page, nil
}

type fakeExport struct {
	stubErr
	doc ExportDocument
}

func (f *fakeExport) Export(context.Context, accounts.AccountID, time.Time, time.Time) (ExportDocument, error) {
	if err := f.fail(); err != nil {
		return ExportDocument{}, err
	}
	return f.doc, nil
}

type fakeAssets struct {
	stubErr
	items []assets.Asset
}

func (f *fakeAssets) List(context.Context, int) ([]assets.Asset, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.items, nil
}

type fakeInstruments struct {
	stubErr
	items  []instruments.Instrument
	detail InstrumentDetail
	last   instruments.StatusChange
}

func (f *fakeInstruments) List(context.Context, int) ([]instruments.Instrument, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.items, nil
}

func (f *fakeInstruments) Detail(context.Context, instruments.InstrumentID) (InstrumentDetail, error) {
	if err := f.fail(); err != nil {
		return InstrumentDetail{}, err
	}
	return f.detail, nil
}

func (f *fakeInstruments) Transition(_ context.Context, _ instruments.InstrumentID, ch instruments.StatusChange) (instruments.Instrument, error) {
	if err := f.fail(); err != nil {
		return instruments.Instrument{}, err
	}
	f.last = ch
	out := f.detail.Instrument
	out.Status = ch.To
	return out, nil
}

type fakeQuotes struct {
	stubErr
	view QuoteView
}

func (f *fakeQuotes) Preview(context.Context, QuotePreview) (QuoteView, error) {
	if err := f.fail(); err != nil {
		return QuoteView{}, err
	}
	return f.view, nil
}

type fakeIntents struct {
	stubErr
	value   intent.TradeIntent
	detail  IntentDetail
	page    intent.Page
	submits int
	cancels int
	lastReq intent.SubmitRequest
	mu      sync.Mutex
}

func (f *fakeIntents) Submit(_ context.Context, _ security.Principal, req intent.SubmitRequest) (intent.TradeIntent, error) {
	if err := f.fail(); err != nil {
		return intent.TradeIntent{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	f.lastReq = req
	return f.value, nil
}

func (f *fakeIntents) Get(context.Context, intent.IntentID) (intent.TradeIntent, error) {
	if err := f.fail(); err != nil {
		return intent.TradeIntent{}, err
	}
	return f.value, nil
}

func (f *fakeIntents) ListForAccount(context.Context, accounts.AccountID, string, int) (intent.Page, error) {
	if err := f.fail(); err != nil {
		return intent.Page{}, err
	}
	return f.page, nil
}

func (f *fakeIntents) Detail(context.Context, intent.IntentID) (IntentDetail, error) {
	if err := f.fail(); err != nil {
		return IntentDetail{}, err
	}
	return f.detail, nil
}

func (f *fakeIntents) RequestCancel(context.Context, security.Principal, intent.IntentID, string) (intent.TradeIntent, error) {
	if err := f.fail(); err != nil {
		return intent.TradeIntent{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return f.value, nil
}

func (f *fakeIntents) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submits
}

func (f *fakeIntents) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

type fakeOrders struct {
	stubErr
	page   OrderPage
	detail OrderDetail
}

func (f *fakeOrders) ListForAccount(context.Context, accounts.AccountID, string, int) (OrderPage, error) {
	if err := f.fail(); err != nil {
		return OrderPage{}, err
	}
	return f.page, nil
}

func (f *fakeOrders) Detail(context.Context, execution.OrderID) (OrderDetail, error) {
	if err := f.fail(); err != nil {
		return OrderDetail{}, err
	}
	return f.detail, nil
}

type fakeFunding struct {
	stubErr
	result funding.StartResult
	items  []funding.Deposit
	detail DepositDetail
	starts int
}

func (f *fakeFunding) Start(context.Context, security.Principal, StartDeposit) (funding.StartResult, error) {
	if err := f.fail(); err != nil {
		return funding.StartResult{}, err
	}
	f.starts++
	return f.result, nil
}

func (f *fakeFunding) Get(context.Context, funding.DepositID) (funding.Deposit, error) {
	if err := f.fail(); err != nil {
		return funding.Deposit{}, err
	}
	return f.detail.Deposit, nil
}

func (f *fakeFunding) List(context.Context, accounts.AccountID, string, int) ([]funding.Deposit, string, error) {
	if err := f.fail(); err != nil {
		return nil, "", err
	}
	return f.items, "", nil
}

func (f *fakeFunding) Detail(context.Context, funding.DepositID) (DepositDetail, error) {
	if err := f.fail(); err != nil {
		return DepositDetail{}, err
	}
	return f.detail, nil
}

type fakeWithdrawals struct {
	stubErr
	value withdrawal.Withdrawal
}

func (f *fakeWithdrawals) Request(context.Context, security.Principal, WithdrawalRequest) (withdrawal.Withdrawal, error) {
	if err := f.fail(); err != nil {
		return withdrawal.Withdrawal{}, err
	}
	return f.value, nil
}

type fakeGates struct {
	stubErr
	items      []GateView
	lastAction GateAction
	history    []gates.Transition
}

func (f *fakeGates) History(_ context.Context, _ gates.Capability) ([]gates.Transition, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.history, nil
}

func (f *fakeGates) List(context.Context) ([]GateView, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.items, nil
}

func (f *fakeGates) Act(_ context.Context, _ gates.Capability, action GateAction, _ gates.Proposal, _ string) (GateView, error) {
	if err := f.fail(); err != nil {
		return GateView{}, err
	}
	f.lastAction = action
	if len(f.items) == 0 {
		return GateView{}, errs.New(errs.CodeNotFound, "no such gate")
	}
	return f.items[0], nil
}

type fakeKillSwitches struct {
	stubErr
	items      []killswitch.Switch
	activated  int
	released   int
	lastReason string
}

func (f *fakeKillSwitches) List(context.Context) ([]killswitch.Switch, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.items, nil
}

func (f *fakeKillSwitches) Activate(_ context.Context, kind killswitch.Kind, scope, reason string) (killswitch.Switch, error) {
	if err := f.fail(); err != nil {
		return killswitch.Switch{}, err
	}
	f.activated++
	f.lastReason = reason
	return killswitch.Switch{Kind: kind, ScopeID: scope, Active: true, Severity: kind.Severity(), Reason: reason}, nil
}

func (f *fakeKillSwitches) Release(_ context.Context, kind killswitch.Kind, scope, reason string, _ *string) (killswitch.Switch, error) {
	if err := f.fail(); err != nil {
		return killswitch.Switch{}, err
	}
	f.released++
	return killswitch.Switch{Kind: kind, ScopeID: scope, Active: false, Severity: kind.Severity(), Reason: reason}, nil
}

type fakeAdminActions struct {
	stubErr
	page   AdminActionPage
	action admin.Action
}

func (f *fakeAdminActions) List(context.Context, string, string, int) (AdminActionPage, error) {
	if err := f.fail(); err != nil {
		return AdminActionPage{}, err
	}
	return f.page, nil
}

func (f *fakeAdminActions) Propose(context.Context, admin.Proposal) (admin.Action, error) {
	if err := f.fail(); err != nil {
		return admin.Action{}, err
	}
	return f.action, nil
}

func (f *fakeAdminActions) Approve(context.Context, string, string) (admin.Action, error) {
	if err := f.fail(); err != nil {
		return admin.Action{}, err
	}
	return f.action, nil
}

func (f *fakeAdminActions) Reject(context.Context, string, string) (admin.Action, error) {
	if err := f.fail(); err != nil {
		return admin.Action{}, err
	}
	return f.action, nil
}

func (f *fakeAdminActions) Execute(context.Context, string) (admin.Action, error) {
	if err := f.fail(); err != nil {
		return admin.Action{}, err
	}
	return f.action, nil
}

type fakeProviders struct {
	stubErr
	items []ProviderView
}

func (f *fakeProviders) List(context.Context) ([]ProviderView, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.items, nil
}

type fakeReconciliation struct {
	stubErr
	page   ReconciliationPage
	record ReconciliationRecord
}

func (f *fakeReconciliation) List(context.Context, string, *accounts.AccountID, string, int) (ReconciliationPage, error) {
	if err := f.fail(); err != nil {
		return ReconciliationPage{}, err
	}
	return f.page, nil
}

func (f *fakeReconciliation) Resolve(context.Context, string, ReconciliationResolution) (ReconciliationRecord, error) {
	if err := f.fail(); err != nil {
		return ReconciliationRecord{}, err
	}
	return f.record, nil
}

type fakeHealth struct{ stubErr }

func (f *fakeHealth) Ready(context.Context) error { return f.fail() }

type fakeWebhook struct {
	status int
	raw    []byte
	seen   int
}

func (f *fakeWebhook) Handle(_ context.Context, raw []byte, _ http.Header, _ webhook.RequestMeta) webhook.Result {
	f.seen++
	f.raw = append([]byte(nil), raw...)
	return webhook.Result{Status: f.status, Outcome: webhook.OutcomeProcessed}
}

// fakeIdempotency is a faithful in-memory model of the persisted contract:
// the same key with the same request replays, the same key with a different
// request conflicts, and a command that concluded is never re-executed.
type fakeIdempotency struct {
	mu      sync.Mutex
	records map[string]fakeIdemRecord
	calls   int
}

type fakeIdemRecord struct {
	hash   string
	result CommandResult
}

func newFakeIdempotency() *fakeIdempotency {
	return &fakeIdempotency{records: map[string]fakeIdemRecord{}}
}

func (f *fakeIdempotency) Run(ctx context.Context, cmd IdempotentCommand, fn func(context.Context) (CommandResult, error)) (CommandResult, error) {
	f.mu.Lock()
	key := cmd.ActorID + "|" + cmd.Endpoint + "|" + cmd.Key
	if rec, ok := f.records[key]; ok {
		f.mu.Unlock()
		if rec.hash != cmd.RequestHash {
			return CommandResult{}, errs.New(errs.CodeInvalidIdempotencyReuse,
				"this Idempotency-Key was already used with a different request body")
		}
		out := rec.result
		out.Replayed = true
		if out.Status >= 400 {
			return out, nil
		}
		return out, nil
	}
	f.calls++
	f.mu.Unlock()

	res, err := fn(ctx)
	if err != nil {
		if idempotencyOutcomeIsConclusion(res.Status, errs.CodeOf(err)) {
			f.mu.Lock()
			f.records[key] = fakeIdemRecord{hash: cmd.RequestHash, result: res}
			f.mu.Unlock()
		}
		return CommandResult{}, err
	}
	f.mu.Lock()
	f.records[key] = fakeIdemRecord{hash: cmd.RequestHash, result: res}
	f.mu.Unlock()
	return res, nil
}

// marshalJSON is a helper for building fake stored bodies.
func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// usd parses a decimal string in tests; a bad literal is a test bug.
func usd(s string) money.USD {
	v, err := money.ParseUSD(s)
	if err != nil {
		panic(err)
	}
	return v
}

// qty parses an exact base-unit string in tests.
func qty(s string) money.Quantity {
	v, err := money.ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return v
}
