package payout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// A provider with no published fee model produces no quote at all, rather than
// a quote of zero. A zero fee that means "we do not know" is how a customer is
// promised a net amount nobody agreed to.
func TestQuoteFee_AnUnpublishedModelIsNotAFreeOne(t *testing.T) {
	t.Parallel()
	_, err := QuoteFee(Capabilities{}, money.USDFromMinor(10_000))
	require.Error(t, err)
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))

	caps := Capabilities{FeeModelPublished: true, FeeFlat: money.USDFromMinor(25), FeeBasisPoints: money.BPS(25)}
	fee, err := QuoteFee(caps, money.USDFromMinor(10_000))
	require.NoError(t, err)
	// 25 cents flat plus 25 bps of $100.00 = 25c + 25c.
	assert.Equal(t, int64(50), fee.Minor())

	// A fee larger than the payout is capped at the gross, so a net amount is
	// never negative. MinimumOK is what carries the refusal.
	fee, err = QuoteFee(caps, money.USDFromMinor(10))
	require.NoError(t, err)
	assert.Equal(t, int64(10), fee.Minor())
}

// The Credit/money conversion is exact integer arithmetic in both directions,
// and each direction rounds the way that cannot cost anybody money by
// accident: value out of the system rounds DOWN, a fee rounds UP.
func TestCreditMoneyConversion_RoundsConservatively(t *testing.T) {
	t.Parallel()
	const (
		creditsPerMajor = 100 // 100 Credits per dollar
		minorPerMajor   = 100 // cents
		decimals        = 6
	)
	// 100 Credits at six decimals = 100_000_000 base units = $1.00.
	usd, err := creditsToMoney(money.QuantityFromInt64(100_000_000), decimals, creditsPerMajor, minorPerMajor)
	require.NoError(t, err)
	assert.Equal(t, int64(100), usd.Minor())

	// A fraction of a cent's worth of Credits is worth zero cents, rounded
	// down: converting up would mint money out of rounding.
	usd, err = creditsToMoney(money.QuantityFromInt64(999_999), decimals, creditsPerMajor, minorPerMajor)
	require.NoError(t, err)
	assert.Equal(t, int64(0), usd.Minor())

	// And back: one cent is one Credit's worth, and a fee rounds up so the
	// platform never absorbs a fraction of every payout.
	qty, err := moneyToCredits(money.USDFromMinor(1), decimals, creditsPerMajor, minorPerMajor)
	require.NoError(t, err)
	assert.Equal(t, "1000000", qty.String())
}

// Expiry and consumption are properties of the row, and both are checked
// before a payout may use it.
func TestQuote_ExpiryAndUsability(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	q := Quote{ExpiresAt: now.Add(QuoteTTL), MinimumOK: true}

	assert.False(t, q.Expired(now))
	assert.True(t, q.Usable(now))
	assert.False(t, q.Expired(now.Add(QuoteTTL-time.Nanosecond)))
	assert.True(t, q.Expired(now.Add(QuoteTTL)), "a quote stops standing exactly at its expiry")
	assert.False(t, q.Usable(now.Add(QuoteTTL)))

	consumed := q
	at := now
	consumed.ConsumedAt = &at
	assert.False(t, consumed.Usable(now), "a quote funds exactly one payout")

	belowMinimum := q
	belowMinimum.MinimumOK = false
	assert.False(t, belowMinimum.Usable(now),
		"a quote the provider would not honour is not usable, however fresh it is")
}

func TestQuoteRequest_Validate(t *testing.T) {
	t.Parallel()
	valid := QuoteRequest{
		AccountID: accounts.NewAccountID(), DestinationID: NewDestinationID(),
		Quantity: money.QuantityFromInt64(1), CreditsPerMajorUnit: 100, MinorUnitsPerMajorUnit: 100,
		Currency: "USD", IdempotencyKey: "k-12345678", Now: time.Now(),
	}
	require.NoError(t, valid.Validate())

	cases := map[string]func(*QuoteRequest){
		"no account":     func(r *QuoteRequest) { r.AccountID = accounts.AccountID{} },
		"no destination": func(r *QuoteRequest) { r.DestinationID = DestinationID{} },
		"zero amount":    func(r *QuoteRequest) { r.Quantity = money.Quantity{} },
		"no pricing":     func(r *QuoteRequest) { r.CreditsPerMajorUnit = 0 },
		"no currency":    func(r *QuoteRequest) { r.Currency = " " },
		"no key":         func(r *QuoteRequest) { r.IdempotencyKey = "" },
		"no clock":       func(r *QuoteRequest) { r.Now = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := valid
			mutate(&r)
			err := r.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}
