//go:build integration

package credit

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/webhook"
)

// ---------------------------------------------------------------------------
// a provider double that behaves the way a real acquirer does
// ---------------------------------------------------------------------------

type fakePurchaseProvider struct {
	mu sync.Mutex
	// byKey is the idempotency contract: the same key is the same payment.
	byKey map[string]PurchaseSnapshot
	byRef map[string]PurchaseSnapshot
	seq   int
	// failCreate, when set, is returned instead of creating.
	failCreate error
	// loseResponse makes CreatePurchase create the payment and then report a
	// transport failure, which is the case that decides whether a lost
	// response can charge twice.
	loseResponse bool
	created      int
}

func newFakeProvider() *fakePurchaseProvider {
	return &fakePurchaseProvider{byKey: map[string]PurchaseSnapshot{}, byRef: map[string]PurchaseSnapshot{}}
}

func (p *fakePurchaseProvider) Name() string { return "fake_credit" }

func (p *fakePurchaseProvider) Capabilities() PurchaseCapabilities {
	return PurchaseCapabilities{
		SupportsHostedPaymentUI: true, SupportsSCA: true, SupportsIdempotentCreate: true,
		SupportsLookup: true, SupportsRefund: true, SupportsDisputeEvents: true,
		Currencies: []string{"USD"},
	}
}

func (p *fakePurchaseProvider) CreatePurchase(_ context.Context, req CreatePurchaseRequest) (PurchaseSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failCreate != nil {
		return PurchaseSession{}, p.failCreate
	}
	if snap, ok := p.byKey[req.IdempotencyKey]; ok {
		return PurchaseSession{
			ProviderReference: snap.ProviderReference, Status: snap.Status,
			RawStatus: snap.RawStatus, ClientSecret: "cs_replay",
		}, nil
	}
	p.seq++
	p.created++
	// Globally unique. credit_fundings has UNIQUE (provider, provider_reference)
	// and the suite shares one database, so a per-fixture counter would collide
	// between tests -- which is the constraint doing its job on a bad fake.
	ref := "pi_fake_" + uuid.NewString()
	snap := PurchaseSnapshot{
		ProviderReference: ref, Status: PurchasePaymentMethodRequired,
		RawStatus: "requires_payment_method", Amount: req.Amount, Currency: req.Currency,
		Metadata: map[string]string{
			"nodal_workstream":         "NODAL",
			"nodal_credit_purchase_id": req.FundingID.String(),
			"nodal_environment":        req.Environment,
		},
	}
	p.byKey[req.IdempotencyKey] = snap
	p.byRef[ref] = snap
	if p.loseResponse {
		return PurchaseSession{}, ErrPurchaseProviderUnavailable
	}
	return PurchaseSession{
		ProviderReference: ref, Status: snap.Status, RawStatus: snap.RawStatus, ClientSecret: "cs_new",
	}, nil
}

func (p *fakePurchaseProvider) GetPurchase(_ context.Context, ref string) (PurchaseSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap, ok := p.byRef[ref]
	if !ok {
		return PurchaseSnapshot{}, errs.New(errs.CodeNotFound, "no such payment")
	}
	return snap, nil
}

func (p *fakePurchaseProvider) ParseWebhook(context.Context, []byte, http.Header) (PurchaseEvent, error) {
	return PurchaseEvent{}, errors.New("not used")
}

// advance sets what the provider will report next for a reference.
func (p *fakePurchaseProvider) advance(ref string, st PurchaseStatus, raw string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := p.byRef[ref]
	snap.Status, snap.RawStatus = st, raw
	p.byRef[ref] = snap
}

// ---------------------------------------------------------------------------
// a gate double
// ---------------------------------------------------------------------------

type fakeGate struct {
	active bool
	asked  []gates.Capability
	mu     sync.Mutex
}

func (g *fakeGate) RequireActive(_ context.Context, _ db.Querier, c gates.Capability) error {
	g.mu.Lock()
	g.asked = append(g.asked, c)
	g.mu.Unlock()
	if g.active {
		return nil
	}
	return errs.Newf(errs.CodeCapabilityNotApproved, "capability %s is not active", c)
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

type purchaseFixture struct {
	*fixture
	svcP      *PurchaseService
	prov      *fakePurchaseProvider
	gate      *fakeGate
	keyPrefix string
}

func newPurchaseFixture(t *testing.T) *purchaseFixture {
	t.Helper()
	f := newFixture(t)
	prov := newFakeProvider()
	gate := &fakeGate{active: true}
	svcP, err := NewPurchaseService(PurchaseServiceConfig{
		Credits: f.svc, Provider: prov, Pricing: DefaultPricingPolicy(),
		Gates: gate, Clock: f.clk, Environment: "TEST",
	})
	require.NoError(t, err)
	return &purchaseFixture{fixture: f, svcP: svcP, prov: prov, gate: gate, keyPrefix: uuid.NewString()}
}

// start buys Credits. The key is namespaced per fixture because the suite
// shares one database across runs and credit_fundings.idempotency_key is
// globally unique -- a fixed key would pass once and then report a reused key
// forever.
func (f *purchaseFixture) start(t *testing.T, key string, minor int64) StartedPurchase {
	t.Helper()
	key = f.keyPrefix + ":" + key
	var out StartedPurchase
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var err error
		out, err = f.svcP.StartPurchase(f.ctx, tx, StartPurchaseRequest{
			AccountID: f.account, Amount: money.USDFromMinor(minor),
			Currency: "USD", IdempotencyKey: key,
		})
		return err
	}))
	return out
}

func (f *purchaseFixture) tx(fn func(pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
}

// deliver applies one provider event, the way the webhook pipeline would.
func (f *purchaseFixture) deliver(t *testing.T, ev PurchaseEvent) webhook.Disposition {
	t.Helper()
	var d webhook.Disposition
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var err error
		d, err = f.svcP.Dispatch(f.ctx, tx, ev)
		return err
	}))
	return d
}

func event(ref string, st PurchaseStatus, eventID string, amountMinor int64) PurchaseEvent {
	return PurchaseEvent{
		Identity: webhook.Identity{Provider: "fake_credit", EventID: eventID, EventType: "test." + string(st)},
		Snapshot: PurchaseSnapshot{
			ProviderReference: ref, Status: st, RawStatus: string(st),
			Amount: money.USDFromMinor(amountMinor), Currency: "USD",
		},
		Recognized: true,
	}
}

func (f *purchaseFixture) funding(t *testing.T, id FundingID) Funding {
	t.Helper()
	got, err := f.svc.Funding(f.ctx, testDB, id)
	require.NoError(t, err)
	return got
}

func (f *purchaseFixture) balances(t *testing.T) Balances {
	t.Helper()
	b, err := f.svc.Balances(f.ctx, testDB, BalanceRequest{
		AccountID: f.account, Policy: valuedomain.DefaultPolicy(),
		Verified: valuedomain.VerificationNone, Now: f.clk.Now(),
	})
	require.NoError(t, err)
	return b
}

// ---------------------------------------------------------------------------
// PAY-001 .. PAY-006
// ---------------------------------------------------------------------------

func TestPAY001_OnePaymentCreatesExactlyOneFundingRecord(t *testing.T) {
	f := newPurchaseFixture(t)

	a := f.start(t, "pay001", 10000)
	b := f.start(t, "pay001", 10000) // the double-clicked buy button

	require.Equal(t, a.Funding.ID, b.Funding.ID, "one funding record")
	require.Equal(t, 1, f.prov.created, "and one payment at the provider")

	var n int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM credit_fundings WHERE account_id = $1`, f.account).Scan(&n))
	require.Equal(t, 1, n)
}

func TestPAY002_CreditAmountIsDerivedServerSide(t *testing.T) {
	f := newPurchaseFixture(t)

	// $100.00 at 100 Credits per dollar.
	got := f.start(t, "pay002", 10000)
	require.Equal(t, "10000", got.CreditQuantity.String())
	require.Equal(t, "10000", got.Funding.CreditQuantity.String())
	require.Equal(t, DefaultPricingVersion, got.PricingVersion)

	// There is no request field that could have asked for more. The only
	// input was the amount, so the only way to get more Credits is to pay
	// more money.
	more := f.start(t, "pay002b", 20000)
	require.Equal(t, "20000", more.CreditQuantity.String())
}

func TestPAY003_DuplicateWebhookCreatesNoDuplicateCredits(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "pay003", 10000)

	// The same capture, delivered four times, with two different event ids --
	// because the inbox deduplicates by event id and the domain must be safe
	// even when it does not.
	for i, id := range []string{"e1", "e1", "e2", "e2"} {
		d := f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, id, 10000))
		require.Equal(t, webhook.Applied, d, "delivery %d", i)
	}

	got := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingReversible, got.State)
	require.NotNil(t, got.LotID)

	b := f.balances(t)
	require.Equal(t, "10000", b.Gross.String(), "four deliveries, one issuance")

	var lots int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM credit_lots WHERE account_id = $1`, f.account).Scan(&lots))
	require.Equal(t, 1, lots)
}

func TestPAY004_RefundIsHandledSafely(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "pay004", 10000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 10000))
	require.Equal(t, "10000", f.balances(t).Gross.String())

	f.deliver(t, event(p.Funding.ProviderReference, PurchaseRefunded, "e2", 10000))

	got := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingRefunded, got.State)

	b := f.balances(t)
	require.Equal(t, "0", b.Spendable.String(), "refunded Credits are not spendable")

	// The ledger must still balance. A refund that leaves the books unequal is
	// worse than a refund that fails.
	requireLedgerBalanced(t, f.ctx)
}

func TestPAY005_DisputeAffectsPayoutEligibilityImmediately(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "pay005", 10000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 10000))

	before := f.balances(t)
	require.Equal(t, "10000", before.Spendable.String())

	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputed, "e2", 10000))

	got := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingDisputed, got.State)

	after := f.balances(t)
	require.Equal(t, "0", after.Spendable.String(),
		"value under dispute must stop being spendable the moment the dispute lands, not when somebody notices")
	require.Equal(t, "10000", after.Frozen.String())
	require.Equal(t, "0", after.PayoutEligible.String())

	// And the lot itself, not merely the funding row, must say so. This is the
	// assertion that would have caught advancing the funding without
	// updating the lot.
	require.NotNil(t, got.LotID)
	lot, err := f.svc.Lot(f.ctx, testDB, *got.LotID)
	require.NoError(t, err)
	require.Equal(t, valuedomain.FinalityDisputed, lot.Finality)
}

func TestPAY006_ChargebackCannotCreateFreeWithdrawableValue(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "pay006", 10000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 10000))

	// Spend most of it, the way a launderer would before charging back.
	_, err := f.spend(8000)
	require.NoError(t, err)

	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputed, "e2", 10000))
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseChargeback, "e3", 10000))

	got := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingReversed, got.State)

	b := f.balances(t)
	require.Equal(t, "0", b.Spendable.String())
	require.Equal(t, "0", b.PayoutEligible.String(),
		"a chargeback must never leave withdrawable value behind")

	// The 8,000 that was already spent is a real hole and must be recorded as
	// one rather than absorbed.
	var deficit string
	err = testDB.QueryRow(f.ctx,
		`SELECT coalesce(sum(e.quantity)::text,'0') FROM journal_entries e
		   JOIN ledger_accounts a ON a.id = e.ledger_account_id
		  WHERE a.owner_id = $1 AND a.code = 'DEFICIT' AND e.side = 'CREDIT'`,
		f.account).Scan(&deficit)
	require.NoError(t, err)
	require.Equal(t, "8000", deficit)

	requireLedgerBalanced(t, f.ctx)
}

// ---------------------------------------------------------------------------
// the shared-account and unknown-status behaviours
// ---------------------------------------------------------------------------

func TestDispatch_ForeignEventIsIgnoredAndChangesNothing(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "foreign", 10000)

	ev := event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 10000)
	ev.Foreign = true
	ev.ForeignReason = "belongs to the other product on this Stripe account"

	require.Equal(t, webhook.Ignored, f.deliver(t, ev))
	require.Equal(t, FundingAuthorizationPending, f.funding(t, p.Funding.ID).State)
	require.Equal(t, "0", f.balances(t).Gross.String(), "a foreign event must mint nothing")
}

func TestDispatch_EventForAnUnknownPaymentIsIgnored(t *testing.T) {
	f := newPurchaseFixture(t)
	f.start(t, "unknown", 10000)
	d := f.deliver(t, event("pi_belongs_to_someone_else", PurchaseSucceeded, "e1", 10000))
	require.Equal(t, webhook.Ignored, d)
	require.Equal(t, "0", f.balances(t).Gross.String())
}

func TestDispatch_AmountDisagreementParksForAPerson(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "amount", 10000)

	// The provider says the charge was 999999. Either our record is wrong or
	// the event is not what it claims; minting against it would issue value
	// for a payment nobody understands.
	d := f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 999999))
	require.Equal(t, webhook.Applied, d)

	got := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingManualReview, got.State)
	require.Nil(t, got.LotID, "nothing was minted")
	require.Equal(t, "0", f.balances(t).Gross.String())
}

func TestDispatch_UnmappedStatusParksRatherThanGuesses(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "unmapped", 10000)

	ev := event(p.Funding.ProviderReference, PurchaseStatus("SOMETHING_NEW"), "e1", 10000)
	require.Equal(t, webhook.Applied, f.deliver(t, ev))

	require.Equal(t, FundingManualReview, f.funding(t, p.Funding.ID).State)
	require.Equal(t, "0", f.balances(t).Gross.String())
}

func TestDispatch_StaleRedeliveryIsIgnoredNotParked(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "stale", 10000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 10000))
	require.Equal(t, FundingReversible, f.funding(t, p.Funding.ID).State)

	// An early event arriving late. A provider that redelivers is not a
	// provider that is wrong, so this must not summon a human.
	d := f.deliver(t, event(p.Funding.ProviderReference, PurchasePaymentMethodRequired, "e2", 10000))
	require.Equal(t, webhook.Applied, d)
	require.Equal(t, FundingReversible, f.funding(t, p.Funding.ID).State)
}

// ---------------------------------------------------------------------------
// the gate, and a lost response
// ---------------------------------------------------------------------------

func TestStartPurchase_RefusedWhenTheCapabilityIsNotActive(t *testing.T) {
	f := newPurchaseFixture(t)
	f.gate.active = false

	err := f.tx(func(tx pgx.Tx) error {
		_, err := f.svcP.StartPurchase(f.ctx, tx, StartPurchaseRequest{
			AccountID: f.account, Amount: money.USDFromMinor(10000),
			Currency: "USD", IdempotencyKey: f.keyPrefix + ":gated",
		})
		return err
	})
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	require.Equal(t, 0, f.prov.created, "the provider must not be called when the gate refuses")

	var n int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM credit_fundings WHERE account_id = $1`, f.account).Scan(&n))
	require.Equal(t, 0, n, "and nothing is persisted")
}

func TestStartPurchase_LostResponseReconcilesWithoutASecondCharge(t *testing.T) {
	f := newPurchaseFixture(t)
	f.prov.loseResponse = true

	// The provider created the payment and we never heard back.
	err := f.tx(func(tx pgx.Tx) error {
		_, err := f.svcP.StartPurchase(f.ctx, tx, StartPurchaseRequest{
			AccountID: f.account, Amount: money.USDFromMinor(10000),
			Currency: "USD", IdempotencyKey: f.keyPrefix + ":lost",
		})
		return err
	})
	require.Error(t, err)
	require.Equal(t, 1, f.prov.created, "the payment exists at the provider")

	// The transaction rolled back, so no funding row survived -- and the
	// idempotency key did. Retrying is safe precisely because the provider
	// honours the key: it returns the same payment rather than making a second.
	f.prov.loseResponse = false
	retry := f.start(t, "lost", 10000)
	require.Equal(t, 1, f.prov.created, "the retry must not charge again")
	require.NotEmpty(t, retry.Funding.ProviderReference)
}

func TestReconcile_AdoptsTheProvidersView(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "recon", 10000)

	// The capture happened and no webhook ever arrived.
	f.prov.advance(p.Funding.ProviderReference, PurchaseSucceeded, "succeeded")

	var got Funding
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var err error
		got, err = f.svcP.Reconcile(f.ctx, tx, p.Funding.ID)
		return err
	}))
	require.Equal(t, FundingReversible, got.State)
	require.Equal(t, "10000", f.balances(t).Gross.String())
}

func TestSettleDue_PromotesTheLotAndNotJustTheRow(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "settle", 10000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "e1", 10000))

	// Nothing is due yet.
	var n int
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var err error
		n, err = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 10)
		return err
	}))
	require.Equal(t, 0, n)

	// Backdate reversible_at rather than advancing the fake clock. SettleDue
	// compares the database's reversible_at against the database's now(), so
	// moving this process's clock proves nothing -- which is exactly the
	// property that made mixing the two a bug.
	_, err := testDB.Exec(f.ctx,
		`UPDATE credit_fundings SET reversible_at = now() - interval '31 days' WHERE id = $1`,
		p.Funding.ID)
	require.NoError(t, err)

	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var err error
		n, err = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 10)
		return err
	}))
	require.Equal(t, 1, n)

	got := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingSettled, got.State)
	require.NotNil(t, got.ReversibleAt, "the funding records when its window opened")

	lot, lerr := f.svc.Lot(f.ctx, testDB, *got.LotID)
	require.NoError(t, lerr)
	require.Equal(t, valuedomain.FinalitySettled, lot.Finality,
		"a SETTLED funding over a REVERSIBLE lot would be permanently unpayable while every screen said otherwise")
	require.True(t, lot.Finality.PayoutEligible())
}

func TestSettleDue_RefusesAZeroWindow(t *testing.T) {
	f := newPurchaseFixture(t)
	err := f.tx(func(tx pgx.Tx) error {
		_, err := f.svcP.SettleDue(f.ctx, tx, 0, 10)
		return err
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "positive")
}

// requireLedgerBalanced asserts the global double-entry invariant.
func requireLedgerBalanced(t *testing.T, ctx context.Context) {
	t.Helper()
	var unbalanced int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM (
		   SELECT transaction_id,
		          sum(CASE WHEN side = 'DEBIT' THEN quantity ELSE -quantity END) AS net
		     FROM journal_entries GROUP BY transaction_id
		 ) t WHERE t.net <> 0`).Scan(&unbalanced))
	require.Zero(t, unbalanced, "every journal transaction must balance")
}

// ---------------------------------------------------------------------------
// SEC-003 and the metadata attack surface
// ---------------------------------------------------------------------------

func TestSEC003_AProviderEventCannotCreditAnotherUsersAccount(t *testing.T) {
	victim := newPurchaseFixture(t)
	attacker := newPurchaseFixture(t)

	vp := victim.start(t, "sec003-v", 10000)
	ap := attacker.start(t, "sec003-a", 10000)

	// The attacker's own event, but claiming the victim's funding id in
	// metadata. The funding is resolved by provider reference -- a column
	// under a unique constraint in a database only this application writes --
	// and the metadata claim is a cross-check, so the two disagree and the
	// event is refused outright rather than resolved in favour of either.
	ev := event(ap.Funding.ProviderReference, PurchaseSucceeded, "sec003-e1", 10000)
	ev.FundingID = vp.Funding.ID

	err := attacker.tx(func(tx pgx.Tx) error {
		_, derr := attacker.svcP.Dispatch(attacker.ctx, tx, ev)
		return derr
	})
	require.Error(t, err, "an object whose two identities disagree must not be resolved automatically")
	require.Contains(t, err.Error(), "names funding")

	require.Equal(t, "0", victim.balances(t).Gross.String(), "the victim gained nothing")
	require.Equal(t, "0", attacker.balances(t).Gross.String(), "and so did the attacker")
}

func TestSEC_MetadataCannotDecideHowManyCreditsAreIssued(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "meta-qty", 10000)
	require.Equal(t, "10000", p.Funding.CreditQuantity.String())

	// Suppose the nodal_credit_quantity metadata on the Stripe object were
	// edited to nine million -- by a compromised dashboard session, or by
	// anyone with provider access. The mint reads the funding row, never the
	// event, so the claim has nowhere to land.
	ev := event(p.Funding.ProviderReference, PurchaseSucceeded, "meta-e1", 10000)
	ev.Snapshot.Metadata = map[string]string{
		"nodal_workstream":      "NODAL",
		"nodal_credit_quantity": "9000000",
	}
	require.Equal(t, webhook.Applied, f.deliver(t, ev))

	require.Equal(t, "10000", f.balances(t).Gross.String(),
		"the Credit quantity comes from the pricing policy at purchase time and from nowhere else")
}

func TestSEC_AnEventCannotRaiseTheAmountThatWasPaid(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "amt-raise", 10000)

	// An attacker who could forge a signature would forge the amount, because
	// that is where the money is. Signature verification stops that at the
	// adapter; this asserts the domain refuses it too, so the guarantee does
	// not rest on one layer.
	require.Equal(t, webhook.Applied,
		f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "amt-e1", 5000000)))

	require.Equal(t, FundingManualReview, f.funding(t, p.Funding.ID).State)
	require.Equal(t, "0", f.balances(t).Gross.String())
}

func TestSEC_TwoFundingsCannotShareAProviderReference(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "ref-unique", 10000)

	// The database is the thing that guarantees a provider object maps to at
	// most one funding. Without it, the resolution in Dispatch would be
	// ambiguous and an attacker could aim an event at whichever row they
	// preferred.
	_, err := testDB.Exec(f.ctx,
		`INSERT INTO credit_fundings (id, account_id, provider, provider_reference, state,
		    credit_quantity, paid_amount_minor, paid_currency, idempotency_key)
		 VALUES ($1,$2,$3,$4,'CREATED',1::numeric,1,'USD',$5)`,
		NewFundingID(), f.account, "fake_credit", p.Funding.ProviderReference, uuid.NewString())
	require.Error(t, err)
	require.Contains(t, err.Error(), "credit_fundings_provider_provider_reference_key")
}
