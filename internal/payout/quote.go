package payout

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type quoteKind struct{}

// QuoteID identifies one pre-commitment quote.
type QuoteID = id.ID[quoteKind]

// NewQuoteID returns a fresh quote id.
func NewQuoteID() QuoteID { return id.New[quoteKind]() }

// ParseQuoteID parses the canonical form.
func ParseQuoteID(s string) (QuoteID, error) { return id.Parse[quoteKind](s) }

// QuoteTTL is how long a quote stands.
//
// Five minutes, chosen rather than inherited: a provider's fee schedule does
// not move minute to minute, and the reason a quote expires at all is that the
// eligibility behind it can — a dispute can arrive, a gate can be pulled, a
// capability can be revoked. Long enough for a person to read the number and
// press the button; short enough that the eligibility it rests on is still the
// eligibility that was checked (D-062).
const QuoteTTL = 5 * time.Minute

// Quote is what a provider said a payout would cost, before anybody committed.
//
// Both sides are exact integers. The Credit side is base units; the money side
// is minor units of Currency. There is no rate field holding a decimal: the
// Credit price is a pricing-policy VERSION, recorded here, and the arithmetic
// is redone from that version if anybody needs to check it.
type Quote struct {
	ID            QuoteID
	AccountID     accounts.AccountID
	DestinationID DestinationID
	Provider      string

	GrossQuantity money.Quantity
	FeeQuantity   money.Quantity
	NetQuantity   money.Quantity

	Currency         string
	GrossAmountMinor int64
	FeeAmountMinor   int64
	NetAmountMinor   int64

	PricingVersion  string
	FeeModelVersion string
	PolicyVersion   string

	// MinimumOK is judged NET of fees, because sub-minimum dust is destroyed
	// rather than returned (PROVIDER_BOUNDARY §3). False is recorded rather
	// than rounded away: the customer is told the provider will not send this.
	MinimumOK          bool
	MinimumAmountMinor int64

	Environment string
	Sandbox     bool

	IdempotencyKey string
	ExpiresAt      time.Time
	ConsumedAt     *time.Time
	CreatedAt      time.Time
}

// Expired reports whether the quote has stopped standing.
func (q Quote) Expired(at time.Time) bool { return !at.Before(q.ExpiresAt) }

// Usable reports whether a payout may be created against this quote.
func (q Quote) Usable(at time.Time) bool {
	return q.ConsumedAt == nil && !q.Expired(at) && q.MinimumOK
}

// QuoteRequest asks what a payout of this many Credits would cost.
type QuoteRequest struct {
	AccountID     accounts.AccountID
	DestinationID DestinationID
	// Quantity is the GROSS amount of Credits the customer would give up. The
	// fee comes out of it, so the net is always smaller — which is the way
	// round a customer expects and the way round that cannot overdraw them.
	Quantity money.Quantity
	// CreditsPerMajorUnit and MinorUnitsPerMajorUnit come from the deployment's
	// Credit pricing policy, so the payout side and the purchase side use one
	// rate. They are supplied rather than read here for the same reason
	// everything else in this package is: the same inputs must always produce
	// the same quote.
	CreditsPerMajorUnit    int64
	MinorUnitsPerMajorUnit int64
	CreditDecimals         uint8
	PricingVersion         string
	PolicyVersion          string
	Currency               string
	Environment            string
	Sandbox                bool
	// DisclosureAccepted says whether the person has accepted the current
	// WITHDRAWAL_DISCLOSURE. See CreateRequest for why it is an input, and
	// ErrDisclosureNotAccepted for why a quote is refused rather than priced.
	DisclosureAccepted bool
	IdempotencyKey     string
	Now                time.Time
}

// Validate checks the request without touching a database or a provider.
func (r QuoteRequest) Validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a quote needs an account")
	}
	if r.DestinationID.IsZero() {
		return errs.New(errs.CodeValidationFailed,
			"a quote needs a destination; the fee depends on where the value is going")
	}
	if r.Quantity.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "a quote needs a positive amount")
	}
	if r.CreditsPerMajorUnit <= 0 || r.MinorUnitsPerMajorUnit <= 0 {
		return errs.New(errs.CodeValidationFailed, "a quote needs the deployment's Credit pricing policy")
	}
	if strings.TrimSpace(r.Currency) == "" {
		return errs.New(errs.CodeValidationFailed, "a quote needs a currency")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "a quote needs an idempotency key")
	}
	if r.Now.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a quote needs the current time")
	}
	return nil
}

// QuoteFee computes the provider's fee for a gross amount of money, from the
// fee model the adapter reports.
//
// A provider whose adapter has not published a fee model produces an error
// rather than a fee of zero. That distinction is the point of
// FeeModelPublished: a zero fee that means "unknown" is how a customer is
// promised a net amount nobody agreed to.
func QuoteFee(c Capabilities, gross money.USD) (money.USD, error) {
	if !c.FeeModelPublished {
		return money.USD{}, errs.Newf(errs.CodeProviderUnavailable,
			"this payout provider has not published a fee model, so no net amount can be quoted").
			WithField("fee_model", "UNPUBLISHED")
	}
	proportional, err := c.FeeBasisPoints.ApplyUSD(gross, money.RoundHalfUp)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeInternal, "payout: fee arithmetic")
	}
	fee, err := proportional.Add(c.FeeFlat)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeInternal, "payout: fee arithmetic")
	}
	if fee.Cmp(gross) > 0 {
		// The fee swallows the payout. It is not an error — it is the answer,
		// and `MinimumOK` carries it — but the fee is capped at the gross so
		// that a net amount is never negative.
		return gross, nil
	}
	return fee, nil
}

// Quote computes and persists a pre-commitment quote.
//
// It reserves nothing, moves nothing and writes no ledger row. A quote is a
// statement about what WOULD happen; the reservation happens when a payout is
// created against it.
func (s *Service) Quote(ctx context.Context, tx pgx.Tx, r QuoteRequest, dest Destination) (Quote, error) {
	if err := r.Validate(); err != nil {
		return Quote{}, err
	}
	// Before the price, not after it. A quote is the moment §48 puts the
	// withdrawal disclosure at -- the person is asking what it would cost to
	// take value out -- and quoting first and refusing at the commit would show
	// somebody a number and then tell them they may not have it.
	if !r.DisclosureAccepted {
		return Quote{}, disclosureRefusal()
	}
	if dest.AccountID != r.AccountID {
		// NOT_FOUND, not FORBIDDEN: a distinguishable refusal is a membership
		// oracle, and every sibling on this resource answers NOT_FOUND
		// (F-233, F-41's rule).
		return Quote{}, errs.New(errs.CodeNotFound, "no such payout destination")
	}
	provider, err := s.providers.Get(dest.Provider)
	if err != nil {
		return Quote{}, err
	}
	caps := provider.Capabilities()
	if !caps.Availability.Usable() {
		return Quote{}, errs.Newf(errs.CodeProviderUnavailable,
			"the payout provider %q reports availability %q", provider.Name(), caps.Availability)
	}
	if !caps.Supports(dest.Kind) {
		return Quote{}, errs.Newf(errs.CodeProviderUnavailable,
			"the payout provider %q does not pay a %s destination", provider.Name(), dest.Kind)
	}
	if !caps.SupportsCurrency(r.Currency) {
		return Quote{}, errs.Newf(errs.CodeProviderUnavailable,
			"the payout provider %q does not pay in %s", provider.Name(), r.Currency)
	}
	// And whether it can pay this RECIPIENT, from the country and subdivision
	// the destination stored.
	//
	// It used to ask three questions -- availability, the destination kind, the
	// currency -- and not the fourth, which is the one D-122 added because "a
	// question skipped when the answer is missing is not asking early, it is not
	// asking". So a provider that narrowed its ExcludedRegions after a
	// destination was registered priced a payout it would refuse, the person was
	// shown a gross, a fee, a net and the lots that would leave, and POST
	// /v1/payouts then refused the commit AND consumed the quote on the way out
	// -- so asking again cost another quote and was refused again (F-269).
	//
	// The same question the destination route asks, in the same words, so the
	// two cannot disagree about one provider fact. The refusal is here rather
	// than at the commit because a quote is a statement about what would happen,
	// and a price for something that cannot happen is not one.
	if ok, refusals := caps.CanPayRecipient(RecipientProfile{
		Kind: RecipientKindIndividual, Country: dest.Country, Region: dest.Region,
	}); !ok {
		code := errs.CodeProviderUnavailable
		detail := "the payout provider " + provider.Name() +
			" does not pay recipients in " + dest.Country
		for _, refusal := range refusals {
			switch refusal {
			case RefusalProfileIncomplete, RefusalRegionUnknown:
				// Not the provider's fault and not a refusal of this person:
				// something the destination does not say. A destination
				// registered before the provider published exclusions for its
				// country has no region, and "we do not know which state" is
				// not "any state".
				code = errs.CodeValidationFailed
				detail = "this payout provider excludes some subdivisions of " + dest.Country +
					", so a destination there has to say which one it pays into; " +
					"register the destination again with its subdivision"
			}
		}
		return Quote{}, errs.New(code, detail).
			WithField("destination_id", dest.ID.String()).
			WithField("provider", provider.Name()).
			WithField("refusals", RefusalCodes(refusals))
	}

	gross, err := creditsToMoney(r.Quantity, r.CreditDecimals, r.CreditsPerMajorUnit, r.MinorUnitsPerMajorUnit)
	if err != nil {
		return Quote{}, err
	}
	feeUSD, err := QuoteFee(caps, gross)
	if err != nil {
		return Quote{}, err
	}
	netUSD, err := gross.Sub(feeUSD)
	if err != nil {
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "payout: fee arithmetic")
	}
	feeQty, err := moneyToCredits(feeUSD, r.CreditDecimals, r.CreditsPerMajorUnit, r.MinorUnitsPerMajorUnit)
	if err != nil {
		return Quote{}, err
	}
	if feeQty.Cmp(r.Quantity) > 0 {
		feeQty = r.Quantity
	}

	q := Quote{
		ID:                 NewQuoteID(),
		AccountID:          r.AccountID,
		DestinationID:      dest.ID,
		Provider:           provider.Name(),
		GrossQuantity:      r.Quantity,
		FeeQuantity:        feeQty,
		NetQuantity:        r.Quantity.Sub(feeQty),
		Currency:           strings.ToUpper(strings.TrimSpace(r.Currency)),
		GrossAmountMinor:   gross.Minor(),
		FeeAmountMinor:     feeUSD.Minor(),
		NetAmountMinor:     netUSD.Minor(),
		PricingVersion:     r.PricingVersion,
		FeeModelVersion:    caps.FeeModelVersion,
		PolicyVersion:      r.PolicyVersion,
		MinimumAmountMinor: caps.MinimumAmount.Minor(),
		Environment:        r.Environment,
		// A quote is a rehearsal when the deployment is, or when the provider
		// is one. Either is enough, and the label is never dropped.
		Sandbox:        r.Sandbox || caps.Availability == AvailabilitySandbox,
		IdempotencyKey: r.IdempotencyKey,
		ExpiresAt:      r.Now.UTC().Add(QuoteTTL),
		CreatedAt:      r.Now.UTC(),
	}
	// Judged NET of fees. A provider minimum of one dollar refuses a payout
	// that nets ninety cents, however large the gross was.
	q.MinimumOK = netUSD.Minor() >= caps.MinimumAmount.Minor() && netUSD.Minor() > 0

	if err := s.insertQuote(ctx, tx, &q); err != nil {
		return Quote{}, err
	}
	return q, nil
}

const quoteColumns = `id, account_id, destination_id, provider,
	gross_quantity, fee_quantity, net_quantity,
	currency, gross_amount_minor, fee_amount_minor, net_amount_minor,
	pricing_version, fee_model_version, policy_version,
	minimum_ok, minimum_amount_minor, environment, sandbox,
	idempotency_key, expires_at, consumed_at, created_at`

func scanQuote(row pgx.Row) (Quote, error) {
	var q Quote
	if err := row.Scan(&q.ID, &q.AccountID, &q.DestinationID, &q.Provider,
		&q.GrossQuantity, &q.FeeQuantity, &q.NetQuantity,
		&q.Currency, &q.GrossAmountMinor, &q.FeeAmountMinor, &q.NetAmountMinor,
		&q.PricingVersion, &q.FeeModelVersion, &q.PolicyVersion,
		&q.MinimumOK, &q.MinimumAmountMinor, &q.Environment, &q.Sandbox,
		&q.IdempotencyKey, &q.ExpiresAt, &q.ConsumedAt, &q.CreatedAt); err != nil {
		return Quote{}, err
	}
	return q, nil
}

func (s *Service) insertQuote(ctx context.Context, tx pgx.Tx, q *Quote) error {
	row := tx.QueryRow(ctx, `INSERT INTO payout_quotes (`+quoteColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING
		RETURNING `+quoteColumns,
		q.ID, q.AccountID, q.DestinationID, q.Provider,
		q.GrossQuantity, q.FeeQuantity, q.NetQuantity,
		q.Currency, q.GrossAmountMinor, q.FeeAmountMinor, q.NetAmountMinor,
		q.PricingVersion, q.FeeModelVersion, q.PolicyVersion,
		q.MinimumOK, q.MinimumAmountMinor, q.Environment, q.Sandbox,
		q.IdempotencyKey, q.ExpiresAt, q.ConsumedAt, q.CreatedAt)
	out, err := scanQuote(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// This account already used the key. Replaying it returns what the
		// customer was actually shown, which is the whole point of storing it.
		// Another account's identical key is a different row and never collides
		// here, because the uniqueness is (account_id, idempotency_key).
		existing, gerr := s.QuoteByIdempotencyKey(ctx, tx, q.AccountID, q.IdempotencyKey)
		if gerr != nil {
			return gerr
		}
		*q = existing
		return nil
	}
	if err != nil {
		return mapError(err)
	}
	*q = out
	return nil
}

// QuoteByID reads one quote.
func (s *Service) QuoteByID(ctx context.Context, q db.Querier, id QuoteID) (Quote, error) {
	out, err := scanQuote(q.QueryRow(ctx, `SELECT `+quoteColumns+` FROM payout_quotes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, errs.New(errs.CodeNotFound, "no such payout quote")
	}
	if err != nil {
		return Quote{}, mapError(err)
	}
	return out, nil
}

// QuoteByIdempotencyKey reads the quote an account's key produced. The account
// is part of the lookup, not a check after it: the uniqueness is scoped by
// account, so there is no row another caller's identical key could return.
func (s *Service) QuoteByIdempotencyKey(ctx context.Context, q db.Querier, accountID accounts.AccountID, key string) (Quote, error) {
	out, err := scanQuote(q.QueryRow(ctx,
		`SELECT `+quoteColumns+` FROM payout_quotes WHERE account_id = $1 AND idempotency_key = $2`, accountID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, errs.New(errs.CodeNotFound, "no such payout quote")
	}
	if err != nil {
		return Quote{}, mapError(err)
	}
	return out, nil
}

// consumeQuote marks a quote as spent by a payout request, refusing an expired
// one, a spent one and one the provider would not honour.
//
// The refusal is deliberately a CONFLICT or QUOTE_EXPIRED rather than a silent
// re-quote: a customer who saw a number and pressed the button a quarter of an
// hour later is told the number has moved, not charged a different one.
//
// It is unexported because Create is its only legitimate caller. Consuming a
// quote outside the transaction that reserves the value would let a quote be
// spent by nothing, and test/reachability is right to ask who calls an exported
// mutator: the answer here is "one function in this file", and that is what an
// unexported method says.
func (s *Service) consumeQuote(ctx context.Context, tx pgx.Tx, id QuoteID, accountID accounts.AccountID, at time.Time) (Quote, error) {
	q, err := scanQuote(tx.QueryRow(ctx,
		`SELECT `+quoteColumns+` FROM payout_quotes WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, errs.New(errs.CodeNotFound, "no such payout quote")
	}
	if err != nil {
		return Quote{}, mapError(err)
	}
	if q.AccountID != accountID {
		return Quote{}, errs.New(errs.CodeNotFound, "no such payout quote")
	}
	if q.ConsumedAt != nil {
		return Quote{}, errs.New(errs.CodeConflict, "that quote has already been used")
	}
	if q.Expired(at) {
		return Quote{}, errs.New(errs.CodeQuoteExpired,
			"that quote has expired; ask for a new one").
			WithField("quote_id", id.String()).
			WithField("expired_at", q.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if !q.MinimumOK {
		return Quote{}, errs.New(errs.CodeValidationFailed,
			"that quote nets less than the provider's minimum, so it cannot be sent")
	}
	used := at.UTC()
	if _, err := tx.Exec(ctx, `UPDATE payout_quotes SET consumed_at = $2 WHERE id = $1`, id, used); err != nil {
		return Quote{}, mapError(err)
	}
	q.ConsumedAt = &used
	return q, nil
}

// creditsToMoney converts base-unit Credits into money, exactly.
//
// Credits are integers scaled by CreditDecimals; the pricing policy says how
// many WHOLE Credits one major unit of money buys. So:
//
//	minor = quantity * minorPerMajor / (creditsPerMajor * 10^decimals)
//
// computed with MulDiv on big integers, rounded down, so a conversion can never
// produce more money than the Credits are worth.
func creditsToMoney(qty money.Quantity, decimals uint8, creditsPerMajor, minorPerMajor int64) (money.USD, error) {
	scale := money.QuantityFromInt64(1).ScaleUp(decimals)
	den := money.QuantityFromInt64(creditsPerMajor).Mul(scale)
	minor, err := qty.MulDiv(money.QuantityFromInt64(minorPerMajor), den, money.RoundDown)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeInternal, "payout: credit to money conversion")
	}
	n, err := minor.Int64()
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "payout: that amount does not fit a money value")
	}
	return money.USDFromMinor(n), nil
}

// moneyToCredits is the inverse, rounded UP so a fee expressed in Credits is
// never less than the fee expressed in money. Rounding a fee down would make
// the platform absorb a fraction of every payout, which is a subsidy nobody
// decided on.
func moneyToCredits(amount money.USD, decimals uint8, creditsPerMajor, minorPerMajor int64) (money.Quantity, error) {
	scale := money.QuantityFromInt64(1).ScaleUp(decimals)
	num := money.QuantityFromInt64(creditsPerMajor).Mul(scale)
	qty, err := money.QuantityFromInt64(amount.Minor()).
		MulDiv(num, money.QuantityFromInt64(minorPerMajor), money.RoundUp)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "payout: money to credit conversion")
	}
	return qty, nil
}
