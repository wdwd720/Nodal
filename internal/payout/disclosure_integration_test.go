//go:build integration

package payout_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The withdrawal disclosure, against the real ledger (D-083).
//
// The unit tests say the refusal exists and names the document. This says the
// refusal happens BEFORE anything is written: no payout_requests row, no
// reservation, no quote. A refusal that left a request row standing would be a
// record of an ask that should never have been taken.

// TestIntegration_APayoutIsRefusedUntilTheDisclosureIsAccepted.
func TestIntegration_APayoutIsRefusedUntilTheDisclosureIsAccepted(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1000)

	before := payoutRowCount(t, f)

	var err error
	require.Error(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			dest := f.destination
			_, _, err = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, Quantity: q(400),
				// Not accepted. This is the state of everybody who has never
				// withdrawn: §48 puts the document at the moment value leaves
				// and deliberately not at signup.
				DisclosureAccepted: false,
				IdempotencyKey:     "payout-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
			}, f.input())
			return err
		}))
	require.Error(t, err)
	assert.Equal(t, errs.CodeTermsAcceptanceRequired, errs.CodeOf(err))

	assert.Equal(t, before, payoutRowCount(t, f),
		"a refused payout leaves no request row; an ask that was never permitted is not a record")
	assert.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String(),
		"and nothing was reserved")

	// Accepted, and the same request goes through on the same inputs. That is
	// what makes this a step rather than a denial.
	req, dec, err := f.create(400, f.input())
	require.NoError(t, err)
	assert.Equal(t, "400", dec.Eligible.String())
	assert.Equal(t, "400", req.ReservedQuantity.String())
}

// TestIntegration_AQuoteIsRefusedBeforeItIsPriced.
//
// Before the price and not after it. A quote is the moment somebody asks what
// it would cost to take value out; quoting first and refusing at the commit
// would show them a number and then tell them they may not have it.
func TestIntegration_AQuoteIsRefusedBeforeItIsPriced(t *testing.T) {
	f := newFixture(t)
	// A provider that can actually be quoted against, so the refusal under test
	// is the disclosure's and not the provider's.
	quotableProvider(f)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1000)

	r := payout.QuoteRequest{
		AccountID:              f.account,
		DestinationID:          f.destination,
		Quantity:               q(500),
		CreditsPerMajorUnit:    100,
		MinorUnitsPerMajorUnit: 100,
		CreditDecimals:         6,
		PricingVersion:         "credit-pricing-v1",
		PolicyVersion:          "payout-policy-itest",
		Currency:               "USD",
		Environment:            "TEST",
		DisclosureAccepted:     false,
		IdempotencyKey:         "quote-" + uuid.NewString(),
		Now:                    f.clk.Now(),
	}

	var qerr error
	require.Error(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			dest, derr := f.svc.Destination(ctx, tx, f.destination)
			if derr != nil {
				return derr
			}
			_, qerr = f.svc.Quote(ctx, tx, r, dest)
			return qerr
		}))
	require.Error(t, qerr)
	assert.Equal(t, errs.CodeTermsAcceptanceRequired, errs.CodeOf(qerr))
	assert.Zero(t, quoteRowCount(t, f), "a refused quote is not persisted")

	// And with the document accepted, the same request prices.
	r.DisclosureAccepted = true
	var quote payout.Quote
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			dest, derr := f.svc.Destination(ctx, tx, f.destination)
			if derr != nil {
				return derr
			}
			var err error
			quote, err = f.svc.Quote(ctx, tx, r, dest)
			return err
		}))
	assert.Equal(t, "500", quote.GrossQuantity.String())
	assert.Equal(t, 1, quoteRowCount(t, f))
}

func payoutRowCount(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM payout_requests WHERE account_id = $1`, f.account).Scan(&n))
	return n
}

func quoteRowCount(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM payout_quotes WHERE account_id = $1`, f.account).Scan(&n))
	return n
}
