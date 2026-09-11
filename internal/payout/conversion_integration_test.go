//go:build integration

package payout_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The conversion-request surface of goal §19, §22, §23 and §25, against the
// real schema: destinations that the application cannot decide about, a quote
// that stands until it expires, and provenance a person can read.

// quotableProvider gives the fixture's sandbox provider the two things a quote
// needs: an availability that says it can be used at all, and a PUBLISHED fee
// model. The default sandbox in payouttest has neither, deliberately -- an
// adapter nobody has read against a contract reports nothing, which
// TestCapabilities_ZeroValueSupportsNothing asserts on the type itself.
//
// newFixture calls this, because a payout now names a quote and a quote needs a
// provider that can be quoted from (D-119). The explicit calls below stay: they
// say what the test depends on rather than inheriting it.
func quotableProvider(f *fixture) {
	f.provider.WithCapabilities(payout.Capabilities{
		SupportsBankPayout: true,
		SupportsFiatWallet: true,
		SupportsLookup:     true,
		SupportsWebhooks:   true,
		Currencies:         []string{"USD"},
		// The provider is asked about the whole recipient now, not just the
		// kind and the currency (D-122), so it has to publish what it pays.
		SupportedCountries: []string{"US"},
		RecipientKinds:     []string{"individual"},
		Availability:       payout.AvailabilitySandbox,
		FeeModelPublished:  true,
		FeeFlat:            money.USDFromMinor(25),
		FeeBasisPoints:     money.BPS(25),
		FeeModelVersion:    "ITEST-PLACEHOLDER-NOT-A-PRICE",
		MinimumAmount:      money.USDFromMinor(100),
	})
}

// twoOriginPolicy permits two earning origins so the provenance test has an
// order to report. Everything else stays shut, as the default does.
func twoOriginPolicy() valuedomain.Policy {
	p := creatorPayoutPolicy()
	p.Version = "itest-two-origins-v1"
	p.Rules[valuedomain.OriginDataSaleEarning] = valuedomain.OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   creatorCap,
		RequiredVerification: valuedomain.VerificationPayoutKYC,
	}
	return p
}

// createWithQuote makes a payout against a quote, which is what the HTTP
// surface does.
func (f *fixture) createWithQuote(amount int64, quoteID payout.QuoteID, in payout.EligibilityInput) (payout.Request, payout.Decision, error) {
	var (
		req payout.Request
		dec payout.Decision
	)
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			dest := f.destination
			req, dec, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quoteID, Quantity: q(amount),
				ProviderTerms:      f.terms(),
				Environment:        "TEST",
				DisclosureAccepted: true,
				IdempotencyKey:     "payout-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
			}, in)
			return cerr
		})
	return req, dec, err
}

func quoteRequest(f *fixture, amount int64, key string) payout.QuoteRequest {
	return payout.QuoteRequest{
		AccountID:              f.account,
		DestinationID:          f.destination,
		Quantity:               q(amount),
		CreditsPerMajorUnit:    100,
		MinorUnitsPerMajorUnit: 100,
		CreditDecimals:         6,
		PricingVersion:         "credit-pricing-v1",
		PolicyVersion:          "payout-policy-itest",
		Currency:               "USD",
		Environment:            "TEST",
		DisclosureAccepted:     true,
		IdempotencyKey:         key,
		Now:                    f.clk.Now(),
	}
}

func quoteIt(t *testing.T, f *fixture, r payout.QuoteRequest) (payout.Quote, error) {
	t.Helper()
	var out payout.Quote
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		dest, derr := f.svc.Destination(ctx, tx, f.destination)
		if derr != nil {
			return derr
		}
		var qerr error
		out, qerr = f.svc.Quote(ctx, tx, r, dest)
		return qerr
	})
	return out, err
}

// F-42 on payout_destinations: whether value may leave to a given account is
// the provider's decision recorded as a transition, not an UPDATE.
func TestIntegration_ADestinationStatusIsNotTheApplicationsToWrite(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx

	// The fixture's destination is VERIFIED, and it got there through a
	// transition row. The row itself exists.
	var transitions int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM payout_destination_transitions WHERE destination_id = $1`, f.destination).Scan(&transitions))
	assert.Equal(t, 1, transitions, "the status moved exactly once, through one row")

	for _, set := range []string{`status = 'DISABLED'`, `verified_at = now()`} {
		_, err := testDB.Exec(ctx, `UPDATE payout_destinations SET `+set+` WHERE id = $1`, f.destination)
		require.Errorf(t, err, "UPDATE ... SET %s must be refused", set)
		assert.Equalf(t, "42501", db.SQLState(err), "%s", set)
	}
	// The one field a person renames still works, which is also what keeps the
	// row lock available.
	_, err := testDB.Exec(ctx, `UPDATE payout_destinations SET display_label = 'Renamed' WHERE id = $1`, f.destination)
	require.NoError(t, err)

	// A destination cannot be born usable.
	_, err = testDB.Exec(ctx, `INSERT INTO payout_destinations
		(id, account_id, kind, provider, provider_reference, status)
		VALUES ($1,$2,'BANK','sandbox',$3,'VERIFIED')`, uuid.New(), f.account, "dest-"+uuid.NewString())
	require.Error(t, err)
	assert.Equal(t, "AD001", db.SQLState(err))
	assert.Contains(t, err.Error(), "DESTINATION_BORN_VERIFIED")
}

// A destination never comes back, and a person may turn off their own but not
// decide that it may receive value.
func TestIntegration_DestinationLifecycleAndWhoMayMoveIt(t *testing.T) {
	f := newFixture(t)
	now := f.clk.Now()

	userChange := payout.DestinationChange{
		ActorType: security.ActorUser, ActorID: f.account.String(),
		Reason: "the account holder stopped using this destination", OccurredAt: now,
	}
	// A person may disable their own.
	var disabled payout.Destination
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			disabled, err = f.svc.TransitionDestination(ctx, tx, f.destination, payout.DestinationDisabled, userChange)
			return err
		}))
	assert.Equal(t, payout.DestinationDisabled, disabled.Status)
	assert.False(t, disabled.Status.Usable())
	assert.Nil(t, disabled.VerifiedAt, "a destination that is no longer usable does not keep a verified_at")

	// And it never comes back.
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, f.destination, payout.DestinationVerified,
				payout.DestinationChange{
					ActorType: security.ActorSystem, ActorID: "itest",
					Reason: "undo", OccurredAt: now,
				})
			return terr
		})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// A person may not decide that a destination may receive value; that is the
	// provider's decision.
	var fresh payout.Destination
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			fresh, cerr = f.svc.CreateDestination(ctx, tx, payout.Destination{
				AccountID: f.account, Kind: payout.DestinationBank, Provider: "sandbox",
				ProviderReference: "dest-" + uuid.NewString(), Currency: "USD", Country: "US",
				MaskedDisplay: "****4242",
			})
			return cerr
		}))
	assert.Equal(t, payout.DestinationUnverified, fresh.Status)
	assert.Equal(t, "US", fresh.Country)
	assert.Equal(t, "****4242", fresh.MaskedDisplay)

	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, fresh.ID, payout.DestinationVerified, userChange)
			return terr
		})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// The listing shows the disabled ones too: a person who removed one and
	// cannot see that it is gone will add it again.
	list, err := f.svc.DestinationsByAccount(f.ctx, testDB, f.account, 50)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	// The token validation is enforced by the domain, not only by the handler.
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, cerr := f.svc.CreateDestination(ctx, tx, payout.Destination{
				AccountID: f.account, Kind: payout.DestinationBank, Provider: "sandbox",
				ProviderReference: "4242424242424242",
			})
			return cerr
		})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err),
		"Nodal never stores the thing the token replaces")
}

// A quote stands until it expires, funds exactly one payout, and says what
// would leave before anybody commits.
func TestIntegration_AQuoteStandsAndIsSpentOnce(t *testing.T) {
	f := newFixture(t)
	quotableProvider(f)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000_000_000)

	quote, err := quoteIt(t, f, quoteRequest(f, 500_000_000, "quote-"+uuid.NewString()))
	require.NoError(t, err)
	assert.Equal(t, "500000000", quote.GrossQuantity.String())
	assert.Equal(t, int64(500), quote.GrossAmountMinor, "5000 Credits at 100 per dollar is $5.00")
	assert.True(t, quote.MinimumOK)
	assert.Equal(t, quote.GrossAmountMinor, quote.FeeAmountMinor+quote.NetAmountMinor)
	assert.Equal(t, quote.GrossQuantity.String(),
		quote.FeeQuantity.Add(quote.NetQuantity).String(), "the two sides of a quote agree")
	assert.False(t, quote.Expired(f.clk.Now()))
	assert.True(t, quote.Expired(f.clk.Now().Add(payout.QuoteTTL)))

	// The same key replays the quote the customer was actually shown, rather
	// than producing a second one at a different price.
	key := "quote-" + uuid.NewString()
	first, err := quoteIt(t, f, quoteRequest(f, 100_000_000, key))
	require.NoError(t, err)
	again, err := quoteIt(t, f, quoteRequest(f, 900_000_000, key))
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, first.GrossQuantity.String(), again.GrossQuantity.String(),
		"a replay is what was shown, not what was asked for the second time")

	// A quote is what the customer was shown, and the schema says so.
	_, err = testDB.Exec(f.ctx, `UPDATE payout_quotes SET fee_amount_minor = 0 WHERE id = $1`, quote.ID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err), "cp_app holds UPDATE on consumed_at alone")

	// It funds exactly one payout, and the request records which.
	req, _, err := f.createWithQuote(500_000_000, quote.ID, f.input())
	require.NoError(t, err)
	require.NotNil(t, req.QuoteID)
	assert.Equal(t, quote.ID, *req.QuoteID)

	_, _, err = f.createWithQuote(500_000_000, quote.ID, f.input())
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "a quote is spent once")
}

// An expired quote refuses the request rather than being silently re-priced: a
// person who saw a number and pressed the button later is told the number
// moved, not charged a different one.
func TestIntegration_AnExpiredQuoteRefusesThePayout(t *testing.T) {
	f := newFixture(t)
	quotableProvider(f)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000_000_000)

	quote, err := quoteIt(t, f, quoteRequest(f, 500_000_000, "quote-"+uuid.NewString()))
	require.NoError(t, err)

	f.clk.Advance(payout.QuoteTTL + time.Second)
	_, _, err = f.createWithQuote(500_000_000, quote.ID, f.input())
	require.Error(t, err)
	assert.Equal(t, errs.CodeQuoteExpired, errs.CodeOf(err))

	// And nothing was reserved by the attempt.
	var consumed *time.Time
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT consumed_at FROM payout_quotes WHERE id = $1`, quote.ID).Scan(&consumed))
	assert.Nil(t, consumed)
}

// A quote whose amount or destination does not match the request is refused.
// The customer was shown one number for one destination; a request that quietly
// changed either is not the thing they agreed to.
func TestIntegration_AQuoteMustMatchTheRequestItFunds(t *testing.T) {
	f := newFixture(t)
	quotableProvider(f)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000_000_000)

	quote, err := quoteIt(t, f, quoteRequest(f, 500_000_000, "quote-"+uuid.NewString()))
	require.NoError(t, err)

	_, _, err = f.createWithQuote(400_000_000, quote.ID, f.input())
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "different amount")
}

// Provenance through trading (§23): a payout does not take "500 Credits", it
// takes specific units from specific provenance lots, and the read model says
// which, in the order they leave.
func TestIntegration_APayoutSaysWhatValueIsLeaving(t *testing.T) {
	f := newFixture(t)
	// Two eligible origins plus one that can never leave. The consumption
	// order is the credit package's: most restricted first among the ELIGIBLE
	// ones, and the promotional grant is never selected at all.
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 900_000_000)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 300_000_000)
	f.issue(valuedomain.OriginDataSaleEarning, valuedomain.FinalitySettled, 300_000_000)

	in := f.input()
	in.Policy = twoOriginPolicy()
	req, decision, err := f.create(500_000_000, in)
	require.NoError(t, err)
	require.True(t, decision.Sufficient())

	slices, err := f.svc.Provenance(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.NotEmpty(t, slices)

	var total int64
	for i, s := range slices {
		assert.NotEqual(t, valuedomain.OriginPromotional, s.Origin,
			"a promotional grant is never selected, so it can never appear in what left")
		assert.False(t, s.Returned)
		if i > 0 {
			assert.LessOrEqual(t, slices[i-1].ConsumptionRank, s.ConsumptionRank,
				"provenance is reported in consumption order")
		}
		n, perr := s.Quantity.Int64()
		require.NoError(t, perr)
		total += n
	}
	assert.Equal(t, int64(500_000_000), total, "the slices add up to what the payout reserved")

	// Cancelling returns exactly what it took, to the exact lots, and the read
	// model says so rather than reporting the value as still leaving.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, cerr := f.svc.Cancel(ctx, tx, req.ID, "changed my mind")
			return cerr
		}))
	slices, err = f.svc.Provenance(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.NotEmpty(t, slices)
	for _, s := range slices {
		assert.True(t, s.Returned, "a cancelled payout's provenance is what came back")
	}
}
