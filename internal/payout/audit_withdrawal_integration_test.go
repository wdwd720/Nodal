//go:build integration

package payout_test

// Reproductions for the withdrawal-verification audit (goal §54, findings
// F-wv-1 … F-wv-4). Every test in this file is expected to FAIL on the tree it
// was written against; each one asserts the invariant the product documents
// claim, so that the fix makes it pass rather than the fix making it moot.
//
// Nothing here changes product code.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider/verifysandbox"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// --- a provider that records what it was actually asked to do ---------------

// recordingProvider is payouttest.Sandbox with two differences: it publishes a
// minimum and a fee model (so the quote has something to refuse on), and it
// keeps the SubmitRequest it was handed, which is the thing F-wv-2 is about.
type recordingProvider struct {
	name string
	caps payout.Capabilities
	last payout.SubmitRequest
	seen map[string]payout.SubmitResult
}

func newRecordingProvider(name string) *recordingProvider {
	return &recordingProvider{
		name: name,
		seen: map[string]payout.SubmitResult{},
		caps: payout.Capabilities{
			SupportsBankPayout: true,
			SupportsFiatWallet: true,
			SupportsLookup:     true,
			Currencies:         []string{"USD"},
			RecipientKinds:     []string{"individual"},
			SupportedCountries: []string{"US"},
			Availability:       payout.AvailabilitySandbox,
			// The same shape internal/provider/payoutsandbox publishes.
			FeeModelPublished: true,
			FeeFlat:           money.USDFromMinor(25),
			FeeBasisPoints:    money.BPS(25),
			FeeModelVersion:   "AUDIT-PLACEHOLDER-NOT-A-PRICE",
			MinimumAmount:     money.USDFromMinor(100),
		},
	}
}

func (p *recordingProvider) Name() string                      { return p.name }
func (p *recordingProvider) Capabilities() payout.Capabilities { return p.caps }

func (p *recordingProvider) Submit(_ context.Context, req payout.SubmitRequest) (payout.SubmitResult, error) {
	p.last = req
	if res, ok := p.seen[req.IdempotencyKey]; ok {
		return res, nil
	}
	now := time.Now().UTC()
	res := payout.SubmitResult{
		Status: payout.ProviderSettled, ProviderReference: "audit-" + req.IdempotencyKey,
		RawStatus: "settled", SettledAt: &now,
	}
	p.seen[req.IdempotencyKey] = res
	return res, nil
}

func (p *recordingProvider) Lookup(_ context.Context, key string) (payout.SubmitResult, error) {
	if res, ok := p.seen[key]; ok {
		return res, nil
	}
	return payout.SubmitResult{Status: payout.ProviderFailed, RawStatus: "not_found"}, nil
}

// auditFixture is the payout service wired against recordingProvider.
type auditFixture struct {
	t           *testing.T
	ctx         context.Context
	clk         *clock.Fake
	credits     *credit.Service
	svc         *payout.Service
	provider    *recordingProvider
	user        accounts.UserID
	account     accounts.AccountID
	creditAsset assets.AssetID
	destination payout.DestinationID
}

func newAuditFixture(t *testing.T) *auditFixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	led := ledger.NewService(clk, "payout-audit-itest")
	led.SetCapabilityResolver(payoutCaps{
		valuedomain.CapPayoutReserve: true,
		valuedomain.CapPayoutSettle:  true,
	})
	credits := credit.NewService(led, clk)

	registry := payout.NewRegistry(true)
	provider := newRecordingProvider("audit_sandbox")
	require.NoError(t, registry.Register(provider))

	repo := accounts.NewRepository()
	user, err := repo.CreateUser(ctx, testDB, "payout-audit-itest", uuid.NewString(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(ctx, testDB, user.ID, accounts.KindCustomer)
	require.NoError(t, err)

	f := &auditFixture{
		t: t, ctx: ctx, clk: clk, credits: credits, provider: provider,
		user: user.ID, account: acct.ID, creditAsset: creditAsset(t),
		svc: payout.NewService(led, credits, payout.NewEngine(credits), registry, clk,
			killswitch.NewChecker(killswitch.Policy{}), repo),
	}

	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			d, err := f.svc.CreateDestination(ctx, tx, payout.Destination{
				AccountID: f.account, Kind: payout.DestinationBank,
				Provider: provider.Name(), ProviderReference: "audit-dest-" + uuid.NewString(),
				DisplayLabel: "Audit bank", Currency: "USD", Country: "US",
			})
			if err != nil {
				return err
			}
			if _, err := f.svc.SetDestinationStatus(ctx, tx, d.ID, payout.DestinationVerified); err != nil {
				return err
			}
			f.destination = d.ID
			return nil
		}))
	return f
}

// sandboxInput is the eligibility input a sandbox tier composes: the sandbox
// payout policy, PAYOUT_KYC, the two payout capabilities active, and the
// compliance facts httpapi reads from the same repository the eligibility page
// reads (D-120). They are stated here because none of them has a permissive
// zero value: an input that left them out would block, which is the direction
// F-226 made this fall.
func (f *auditFixture) sandboxInput() payout.EligibilityInput {
	return payout.EligibilityInput{
		Policy:                valuedomain.SandboxPolicy(),
		Verified:              valuedomain.VerificationPayoutKYC,
		ActiveCaps:            map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:                   f.clk.Now(),
		DestinationVerified:   true,
		ProviderSupports:      true,
		SanctionsState:        compliance.SanctionsClear,
		JurisdictionSupported: true,
	}
}

// quote is the pre-commitment quote a payout now names (D-119). The pricing is
// the deployment's: one hundred Credits to the dollar, six decimals.
func (f *auditFixture) quote(qty int64) (payout.Quote, error) {
	var out payout.Quote
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			dest, derr := f.svc.Destination(ctx, tx, f.destination)
			if derr != nil {
				return derr
			}
			var qerr error
			out, qerr = f.svc.Quote(ctx, tx, payout.QuoteRequest{
				AccountID: f.account, DestinationID: f.destination,
				Quantity:               money.QuantityFromInt64(qty),
				CreditsPerMajorUnit:    100,
				MinorUnitsPerMajorUnit: 100,
				CreditDecimals:         6,
				PricingVersion:         "audit-pricing-v1",
				PolicyVersion:          valuedomain.SandboxPolicyVersion,
				Currency:               "USD",
				Environment:            "TEST",
				Sandbox:                true,
				DisclosureAccepted:     true,
				IdempotencyKey:         "audit-quote-" + uuid.NewString(),
				Now:                    f.clk.Now(),
			}, dest)
			return qerr
		})
	return out, err
}

// terms are what the recording provider publishes: a 25c + 25bp fee and a
// $1.00 minimum.
func (f *auditFixture) terms() payout.ProviderTerms {
	return payout.TermsFrom(f.provider.Capabilities())
}

func (f *auditFixture) issue(origin valuedomain.CreditOrigin, fin valuedomain.FundingFinality, qty int64) credit.Lot {
	f.t.Helper()
	var lot credit.Lot
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			lot, err = f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: f.account, Quantity: money.QuantityFromInt64(qty), Origin: origin, Finality: fin,
				Reference:      credit.Reference{Type: "audit_issue", ID: uuid.NewString()},
				IdempotencyKey: "audit-issue-" + uuid.NewString(),
				Reason:         "withdrawal audit fixture", EffectiveAt: f.clk.Now(),
			})
			return err
		}))
	return lot
}

// ---------------------------------------------------------------------------
// F-wv-1 — the provider's minimum and its fee are enforced only by the quote,
// and POST /v1/payouts does not require one.
//
// docs/product/VERIFICATION_AND_WITHDRAWAL.md §3 lists MINIMUM_NOT_MET as one
// of the four refusals a payout meets ("Will a provider actually send it?") and
// PROVIDER_BOUNDARY.md §3 says the minimum is judged net of fees "because
// sub-minimum dust is destroyed, not returned". The quote refuses it. The
// conversion request never asks.
// ---------------------------------------------------------------------------

func TestAuditWV_APayoutBelowTheProviderMinimumIsReservedAndSettledWithoutAQuote(t *testing.T) {
	f := newAuditFixture(t)
	// 50 Credits at 6 decimals = 50,000,000 base units = $0.50 gross, which
	// nets $0.25 after the 25c + 25bp fee: below the provider's $1.00 minimum.
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 50_000_000)

	// The quote says so, in as many words.
	quote, err := f.quote(50_000_000)
	require.NoError(t, err)
	require.False(t, quote.MinimumOK,
		"fixture check: this amount must be below the provider minimum for the test to mean anything")
	require.Equal(t, int64(100), quote.MinimumAmountMinor)
	require.Equal(t, int64(25), quote.NetAmountMinor)

	// The same amount, requested without naming the quote. It used to reach
	// VERIFIED with the value reserved and then SETTLED, because the whole
	// minimum-and-fee branch of Create sat inside `if r.QuoteID != nil`; the
	// quote is required now and there is no such branch (F-224, D-119).
	dest := f.destination
	var noQuoteErr error
	require.Error(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, _, noQuoteErr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest,
				Quantity:           money.QuantityFromInt64(50_000_000),
				ProviderTerms:      f.terms(),
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-noquote-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return noQuoteErr
		}))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(noQuoteErr))
	assert.Contains(t, noQuoteErr.Error(), "quote",
		"F-224: a payout that named no quote met neither the minimum nor the fee")

	// And naming the quote does not get round it either: the quote itself is
	// refused at consumption, so nothing is reserved and nothing is created.
	var withQuoteErr error
	require.Error(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, _, withQuoteErr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(50_000_000),
				ProviderTerms:      f.terms(),
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-subminimum-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return withQuoteErr
		}))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(withQuoteErr))

	var rows int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM payout_requests WHERE account_id = $1`, f.account).Scan(&rows))
	assert.Zero(t, rows,
		"F-224: a sub-minimum payout reached VERIFIED with the value reserved and then SETTLED, "+
			"and the fee the quote priced was never taken")

	// The other direction, which is why the minimum is an input to Create and
	// not a number copied onto the quote: a quote that was above the minimum
	// when it was given, against a provider that has since raised it.
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)
	ok, err := f.quote(200_000_000) // $2.00 gross, $1.70 net after the fee
	require.NoError(t, err)
	require.True(t, ok.MinimumOK, "fixture check: this one is above the published minimum")

	raised := f.terms()
	raised.MinimumAmount = money.USDFromMinor(500)
	var raisedErr error
	require.Error(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, _, raisedErr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &ok.ID,
				Quantity:           money.QuantityFromInt64(200_000_000),
				ProviderTerms:      raised,
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-raised-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return raisedErr
		}))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(raisedErr))
	raisedDetail, ok2 := errs.As(raisedErr)
	require.True(t, ok2)
	assert.Equal(t, string(payout.ReasonMinimumNotMet), raisedDetail.Fields["refusal"])
}

// ---------------------------------------------------------------------------
// F-wv-2 — the provider is handed no amount, no destination, no currency and
// no destination kind.
//
// payout.Service.Submit builds a SubmitRequest with `Amount: money.USD{}` and
// leaves DestinationReference, DestinationKind and Currency at their zero
// values, yet the request then reaches SETTLED and the ledger records the whole
// reserved quantity as EXTERNAL_SETTLED.
// ---------------------------------------------------------------------------

func TestAuditWV_TheProviderIsToldTheAmountAndTheDestination(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)

	quote, err := f.quote(500_000_000) // 500 Credits = $5.00 gross
	require.NoError(t, err)
	require.True(t, quote.MinimumOK)
	require.Positive(t, quote.NetAmountMinor)

	var req payout.Request
	dest := f.destination
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(500_000_000),
				ProviderTerms:      f.terms(),
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-submit-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return cerr
		}))
	require.Equal(t, payout.StateVerified, req.State)
	// The GROSS is reserved: the fee comes out of what leaves rather than being
	// added to it, so the units the customer gives up are the units they asked
	// to convert (D-119).
	assert.Equal(t, "500000000", req.ReservedQuantity.String())

	settled, err := f.svc.Submit(f.ctx, testDB, req.ID, f.provider.Name())
	require.NoError(t, err)
	require.Equal(t, payout.StateSettled, settled.State,
		"fixture check: the recording provider settles immediately")

	got := f.provider.last
	assert.Equal(t, quote.NetAmountMinor, got.Amount.Minor(),
		"F-225: the provider is told the quote's NET -- what the customer was told would reach them")
	assert.Equal(t, "USD", got.Currency)
	assert.NotEmpty(t, got.DestinationReference,
		"F-225: the provider was given no destination reference, so it was told to pay nobody")
	assert.Equal(t, payout.DestinationBank, got.DestinationKind)
	assert.Equal(t, req.ID.String(), got.Reference)

	// And the instruction is refused at the type, so no adapter can be handed
	// an empty one however it was assembled.
	require.Error(t, payout.SubmitRequest{IdempotencyKey: "k", Reference: "r"}.Validate())

	// The ledger recorded the value as having left, which is what makes the
	// instruction above the thing that had to be right.
	var external string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='PLATFORM' AND la.code=$1 AND la.asset_id=$2), 0)::text`,
		string(ledger.CodePayoutSettled), f.creditAsset).Scan(&external))
	assert.Equal(t, "500000000", external)
}

// ---------------------------------------------------------------------------
// F-wv-3 — no earned Credit can ever reach a payout-eligible funding finality.
//
// internal/commerce and internal/nativemarket mint every earning as
// FinalityReversible. Only credit.Service.SettleFunding promotes a lot to
// SETTLED, and it reads credit_fundings.lot_id — a row an earning never has. So
// five of the six origins valuedomain.SandboxPolicy marks withdrawable can
// never be withdrawn, and GET /v1/me/eligibility reports FUNDING_NOT_SETTLED,
// which internal/eligibility documents as "Waiting fixes it".
// ---------------------------------------------------------------------------

func TestAuditWV_AnEarnedCreditCanReachAPayoutEligibleFinality(t *testing.T) {
	f := newAuditFixture(t)
	// Exactly what internal/nativemarket.moveCredits and
	// internal/commerce.Service mint for a sale: an earning, REVERSIBLE.
	lot := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalityReversible, 1_000_000_000)

	// Nothing funds it, so nothing can ever settle it.
	var fundings int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM credit_fundings WHERE lot_id = $1`, lot.ID).Scan(&fundings))
	assert.NotZero(t, fundings,
		"F-wv-3: an earned lot has no credit_fundings row, and SettleFunding is the only writer that "+
			"promotes a lot to SETTLED, so this value can never become payout-eligible")

	// The payout engine refuses it, under the policy that says the origin is
	// withdrawable.
	policy := valuedomain.SandboxPolicy()
	require.True(t, policy.Rule(valuedomain.OriginCreatorEarning).PayoutAllowed,
		"fixture check: the sandbox policy says this origin may be withdrawn")

	var dec payout.Decision
	dest := f.destination
	// 500 Credits: $5.00 gross, which clears the provider's $1.00 minimum, so
	// the refusal under test is the finality's and not the minimum's.
	quote, qerr := f.quote(500_000_000)
	require.NoError(t, qerr)
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			_, dec, err = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(500_000_000),
				ProviderTerms:      f.terms(),
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-earning-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return err
		}))
	assert.True(t, dec.Sufficient(),
		"F-wv-3: a fully verified account holding an origin the policy permits cannot withdraw it, "+
			"because the finality it was minted with has no path to SETTLED: %v", dec.Reasons)

	// And the explanation tells the person to wait for something that will
	// never happen.
	exp := eligibility.ExplainWithdrawal(eligibility.WithdrawalInput{
		Policy:      policy,
		Verified:    valuedomain.VerificationPayoutKYC,
		ActiveCaps:  map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		PolicyValid: policy.Validate() == nil,
		Holdings: []eligibility.OriginHolding{{
			Origin:   valuedomain.OriginCreatorEarning,
			Quantity: money.QuantityFromInt64(1_000_000_000),
			Finality: valuedomain.FinalityReversible,
			HeldDays: 365,
		}},
		Gross:                 money.QuantityFromInt64(1_000_000_000),
		Spendable:             money.QuantityFromInt64(1_000_000_000),
		JurisdictionSupported: true,
		ProviderAvailable:     true,
		DestinationConfigured: true,
		DisclosureAccepted:    true,
	})
	assert.NotContains(t, exp.Reasons, eligibility.WithdrawalFundingNotSettled,
		"F-wv-3: the eligibility explanation reports FUNDING_NOT_SETTLED, which it documents as a "+
			"reason waiting fixes, on value whose finality nothing can ever move")
}

// ---------------------------------------------------------------------------
// F-wv-4 — the compliance facts GET /v1/me/eligibility refuses on are read by
// nothing on the conversion path.
//
// ADR-0025 names "verified, and sanctions is under review" as the state a real
// screening produces most often after clear, and
// VERIFICATION_AND_WITHDRAWAL.md §3 lists ACCOUNT_RESTRICTED ("a freeze, a
// compliance hold, a sanctions review") among the seven decisions between
// Credits and money. payout.EligibilityInput has no field for any of them and
// payout.Service.Create reads none of those columns.
// ---------------------------------------------------------------------------

func TestAuditWV_AnOpenSanctionsReviewStopsAConversionRequest(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)

	// A provider answer of the shape ADR-0025 describes: the document, the age
	// and the jurisdiction pass, the sanctions screen is clear, and the
	// political-exposure screen is a hit. sanctionsStateFrom maps that to
	// REVIEW; EvidenceSatisfies(PAYOUT_KYC) is satisfied because PEP is not one
	// of the four, so the profile reaches VERIFIED.
	svc, repo := newAuditVerificationService(t, f.clk)
	session := newAuditSession(t, repo, f.user)
	ingested, err := svc.Ingest(f.ctx, testDB, session, verification.Result{
		ProviderRef: session.ProviderRef,
		Status:      verification.SessionApproved,
		RawStatus:   "audit_approved",
		Sandbox:     true,
		AgeAtLeast:  verifysandbox.AttestsAgeAtLeast,
		Jurisdiction: rules.Jurisdiction{
			Country: session.JurisdictionCountry, Region: session.JurisdictionRegion,
		},
		Checks: []verification.CheckResult{
			{Kind: verification.CheckIdentityDocument, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckAge, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckJurisdiction, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckSanctions, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckPEP, Outcome: verification.OutcomeFail, Detail: "AUDIT_PEP_HIT"},
		},
	}, "audit fixture: a provider decision with a political-exposure hit", "audit")
	require.NoError(t, err)
	require.Equal(t, verification.SessionApproved, ingested.Status)

	profile, err := compliance.NewRepository(audit.NewWriter()).Get(f.ctx, testDB, f.user)
	require.NoError(t, err)
	require.Equal(t, compliance.SanctionsReview, profile.SanctionsState,
		"fixture check: the screen is under review")
	state, _, _, err := repo.ProfileState(f.ctx, testDB, f.user)
	require.NoError(t, err)
	require.Equal(t, verification.StateVerified, state,
		"fixture check: the identity state is VERIFIED, which is the point")

	// The level the payout path resolves, from the same resolver cmd/api wires.
	resolver, err := verification.NewResolver(
		verification.StaticBase(valuedomain.VerificationNodalIdentity), repo, testDB, f.clk,
	)
	require.NoError(t, err)
	level, err := resolver.Level(f.ctx, f.account)
	require.NoError(t, err)
	require.Equal(t, valuedomain.VerificationPayoutKYC, level,
		"fixture check: the resolver reports PAYOUT_KYC, because PEP is not required for it")

	// What the person is told.
	exp := eligibility.ExplainWithdrawal(eligibility.WithdrawalInput{
		Policy:      valuedomain.SandboxPolicy(),
		Verified:    level,
		ActiveCaps:  map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		PolicyValid: true,
		Holdings: []eligibility.OriginHolding{{
			Origin:   valuedomain.OriginPurchased,
			Quantity: money.QuantityFromInt64(1_000_000_000),
			Finality: valuedomain.FinalitySettled,
		}},
		Gross:                 money.QuantityFromInt64(1_000_000_000),
		Spendable:             money.QuantityFromInt64(1_000_000_000),
		JurisdictionSupported: true,
		ProviderAvailable:     true,
		DestinationConfigured: true,
		DisclosureAccepted:    true,
		// Exactly what httpapi.eligibilityAdapter.applyAccountFacts appends.
		AccountRestrictions: []string{"SANCTIONS_" + string(profile.SanctionsState)},
	})
	require.Contains(t, exp.Reasons, eligibility.WithdrawalAccountRestricted)
	require.False(t, exp.Eligible)
	require.Equal(t, "0", exp.WithdrawableNow.String(),
		"fixture check: the eligibility page says nothing may leave")

	// What the conversion request does, with the same facts the eligibility
	// page answered from -- read from the same compliance profile, in the
	// transaction that would reserve the value (D-120).
	quote, err := f.quote(1_000_000_000)
	require.NoError(t, err)

	var (
		req payout.Request
		dec payout.Decision
	)
	in := f.sandboxInput()
	in.Verified = level
	in.SanctionsState = profile.SanctionsState
	in.AccountRestrictions = append([]string(nil), profile.Restrictions...)
	dest := f.destination
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, dec, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(1_000_000_000),
				ProviderTerms:      f.terms(),
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-sanctions-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, in)
			return cerr
		}))
	assert.NotEqual(t, payout.StateVerified, req.State,
		"F-226: an account under an open sanctions review reserved %s Credits for payout, "+
			"while GET /v1/me/eligibility reported ACCOUNT_RESTRICTED and 0 withdrawable",
		req.ReservedQuantity.String())
	assert.Equal(t, "0", req.ReservedQuantity.String())
	assert.Contains(t, dec.ReasonStrings(), string(payout.ReasonAccountRestricted),
		"the conversion path gives the reason the eligibility page gives")

	// And the two other facts on the same input, each on its own, because §21
	// says they must be able to refuse independently.
	for _, tc := range []struct {
		name   string
		mutate func(*payout.EligibilityInput)
		reason valuedomain.PermitReason
	}{
		{"an unsupported jurisdiction", func(i *payout.EligibilityInput) {
			i.JurisdictionSupported = false
		}, payout.ReasonJurisdictionRestricted},
		{"a restriction recorded against the account", func(i *payout.EligibilityInput) {
			i.AccountRestrictions = []string{"OPERATOR_HOLD"}
		}, payout.ReasonAccountRestricted},
		{"a screen nobody has answered", func(i *payout.EligibilityInput) {
			i.SanctionsState = ""
		}, payout.ReasonAccountRestricted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := f.sandboxInput()
			probe.Verified = level
			probe.AccountID = f.account
			probe.Requested = money.QuantityFromInt64(1_000_000)
			tc.mutate(&probe)
			d, eerr := payout.NewEngine(f.credits).Evaluate(f.ctx, testDB, probe)
			require.NoError(t, eerr)
			assert.False(t, d.Sufficient())
			assert.Contains(t, d.ReasonStrings(), string(tc.reason))
		})
	}
}

// newAuditVerificationService builds the verification service a sandbox tier
// runs, on the fixture's clock.
func newAuditVerificationService(t *testing.T, clk clock.Clock) (*verification.Service, *verification.Repository) {
	t.Helper()
	reg := verification.NewRegistry(true)
	p, err := verifysandbox.New(config.EnvStaging, clk.Now)
	require.NoError(t, err)
	require.NoError(t, reg.Register(p))
	repo := verification.NewRepository()
	svc, err := verification.NewService(verification.Deps{
		Repo:        repo,
		Compliance:  compliance.NewRepository(audit.NewWriter()),
		Providers:   reg,
		Clock:       clk,
		Environment: "TEST",
		SandboxTier: true,
	})
	require.NoError(t, err)
	return svc, repo
}

// newAuditSession creates the profile and the session a provider answer needs
// to land on, through the same repository the service uses.
func newAuditSession(t *testing.T, repo *verification.Repository, user accounts.UserID) verification.Session {
	t.Helper()
	ctx := context.Background()
	creg := compliance.NewRepository(audit.NewWriter())
	var session verification.Session
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := creg.Upsert(ctx, tx, compliance.Profile{
				UserID: user, SanctionsState: compliance.SanctionsUnknown,
				JurisdictionCountry: "US", JurisdictionRegion: "CA",
				ResidencyCountry: "US", Restrictions: []string{},
			}, compliance.Change{
				ActorType: security.ActorSystem, ActorID: "audit-fixture", Reason: "withdrawal audit",
			}); err != nil {
				return err
			}
			s, err := repo.CreateSession(ctx, tx, verification.Session{
				UserID: user, Purpose: verification.PurposePayoutKYC,
				Provider: verifysandbox.Name, JurisdictionCountry: "US", JurisdictionRegion: "CA",
				RulesVersion: rules.Version, Environment: "TEST", Sandbox: true,
			})
			if err != nil {
				return err
			}
			ref := "audit-verif-" + s.ID.String()
			if err := repo.SetProviderReference(ctx, tx, s.ID, ref, nil); err != nil {
				return err
			}
			s, err = repo.TransitionSession(ctx, tx, s.ID, verification.SessionPendingUserAction,
				verification.SessionChange{
					ActorType: security.ActorSystem, ActorID: "audit-fixture",
					Reason: "the audit fixture issued a hosted session", OccurredAt: time.Now().UTC(),
				})
			if err != nil {
				return err
			}
			s.ProviderRef = ref
			session = s
			return nil
		}))
	return session
}

// ---------------------------------------------------------------------------
// F-wv-6 — payout_requests.state is bound to a transition row and to no legal
// edge table, and cp_app keeps a plain UPDATE on the row.
//
// The neighbouring state columns in this area were given the F-42 treatment in
// this goal's wave: 00761 (compliance_profiles.identity_state), 00762
// (verification_sessions.status), 00763 (payout_destinations.status) each
// REVOKE UPDATE from cp_app and have a SECURITY DEFINER trigger write the
// column. payout_requests — the ConversionRequest itself, the row that says
// whether somebody's money left — still holds `GRANT SELECT, INSERT, UPDATE ON
// payout_requests TO cp_app` from 00713, with only 00731's edge binding in
// front of it. The binding checks that a transition row for exactly this edge
// exists; it does not ask whether the edge is one internal/payout.CanTransition
// has.
// ---------------------------------------------------------------------------

func TestAuditWV_TheConversionRequestStateMachineIsEnforcedByTheDatabase(t *testing.T) {
	f := newAuditFixture(t)
	// A promotional grant: an origin the sandbox policy forbids at every
	// verification level, so the decision is REJECTED rather than
	// VERIFICATION_REQUIRED.
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 1_000_000_000)

	// A request the engine REJECTED: nothing reserved, nothing consumed, no
	// allocations, no ledger movement. REJECTED is terminal in Go.
	var req payout.Request
	dest := f.destination
	in := f.sandboxInput()
	quote, qerr := f.quote(500_000_000)
	require.NoError(t, qerr)
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(500_000_000),
				ProviderTerms:      f.terms(),
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-sqledge-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, in)
			return cerr
		}))
	require.Equal(t, payout.StateRejected, req.State)
	require.False(t, payout.CanTransition(payout.StateRejected, payout.StateSettled),
		"fixture check: REJECTED is terminal in internal/payout")

	// One transaction as cp_app: an honest transition row naming an edge the Go
	// table does not have, and the money columns written beside it.
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, ierr := tx.Exec(ctx, `INSERT INTO payout_request_transitions
				(id, request_id, from_state, to_state, actor_type, actor_id, reason)
				VALUES ($1,$2,'REJECTED','SETTLED','SYSTEM','audit-probe','an edge CanTransition forbids')`,
				uuid.New(), req.ID); ierr != nil {
				return ierr
			}
			_, uerr := tx.Exec(ctx, `UPDATE payout_requests
				   SET state = 'SETTLED', reserved_quantity = requested_quantity,
				       settled_quantity = requested_quantity, settled_at = now(),
				       provider = $2, provider_reference = 'forged-by-cp_app'
				 WHERE id = $1`, req.ID, f.provider.Name())
			return uerr
		})
	require.Error(t, err,
		"F-229: cp_app moved a REJECTED conversion request to SETTLED with a forged provider "+
			"reference and a settled quantity, with no ledger posting and no allocation")
	assert.Equal(t, "AD001", db.SQLState(err), "got %v", err)
	assert.Contains(t, err.Error(), "PAYOUT_TRANSITION_ILLEGAL_EDGE")

	// The edge is half of it. The money columns were writable beside a LAWFUL
	// move too, which is what 00733's header describes and could not stop for
	// the columns it left granted: each of these is now refused on privilege,
	// with no transition row anywhere in sight.
	for _, forgery := range []struct {
		what string
		sql  string
	}{
		{"the state", `UPDATE payout_requests SET state = 'VERIFIED' WHERE id = $1`},
		{"the reservation", `UPDATE payout_requests SET reserved_quantity = requested_quantity WHERE id = $1`},
		{"the settlement", `UPDATE payout_requests SET settled_quantity = requested_quantity WHERE id = $1`},
		{"the settled instant", `UPDATE payout_requests SET settled_at = now() WHERE id = $1`},
		{"the provider's reference", `UPDATE payout_requests SET provider_reference = 'forged' WHERE id = $1`},
	} {
		_, ferr := testDB.Exec(f.ctx, forgery.sql, req.ID)
		require.Error(t, ferr, "cp_app rewrote %s of a conversion request", forgery.what)
		assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(ferr), "%s: got %v", forgery.what, ferr)
	}

	after, gerr := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, gerr)
	assert.Equal(t, payout.StateRejected, after.State)
	assert.Equal(t, "0", after.SettledQuantity.String())
	assert.Empty(t, after.ProviderReference)
	assert.Zero(t, countAllocations(t, req.ID))

	// The positive control: the service's own legal move still commits, money
	// and all, through the trigger that now owns those columns.
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)
	var lawful payout.Request
	dest2 := f.destination
	okQuote, oerr := f.quote(500_000_000)
	require.NoError(t, oerr)
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			lawful, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest2, QuoteID: &okQuote.ID,
				Quantity:           money.QuantityFromInt64(500_000_000),
				ProviderTerms:      f.terms(),
				DisclosureAccepted: true,
				IdempotencyKey:     "audit-sqledge-ok-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return cerr
		}))
	require.Equal(t, payout.StateVerified, lawful.State,
		"the trigger that writes the state refused a move the state machine has")
	assert.Equal(t, "500000000", lawful.ReservedQuantity.String(),
		"the reservation must reach the row through the transition that carries it")
}

func countAllocations(t *testing.T, id payout.RequestID) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM payout_allocations WHERE request_id = $1`, id).Scan(&n))
	return n
}
