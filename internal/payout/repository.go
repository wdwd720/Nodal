package payout

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// assetsAssetID is the registry asset identifier. It is aliased so the Credits
// interface in service.go can name it without every caller importing the
// registry package.
type assetsAssetID = assets.AssetID

// accountsAccountID is the account identifier, aliased for the same reason.
type accountsAccountID = accounts.AccountID

// accountsAccount and the two statuses the WITHDRAW guard compares against,
// aliased for the same reason: service.go names them in an interface and in one
// refusal, and nothing else in this package needs the accounts package.
type accountsAccount = accounts.Account

const (
	accountsStatusActive = accounts.StatusActive
	accountsStatusFrozen = accounts.StatusFrozen
)

const requestColumns = `id, account_id, destination_id, credit_asset_id, state,
	requested_quantity::text, reserved_quantity::text, settled_quantity::text,
	policy_version, policy_hash, eligibility_reasons, verification_level,
	coalesce(provider,''), coalesce(provider_idempotency_key,''), coalesce(provider_reference,''),
	coalesce(provider_status,''), idempotency_key, quote_id,
	coalesce(quote_gross_amount_minor,0), coalesce(quote_fee_amount_minor,0),
	coalesce(quote_net_amount_minor,0), coalesce(quote_currency,''),
	coalesce(sandbox,true), coalesce(environment,''),
	reserved_at, submitted_at, settled_at, coalesce(failure_reason,''), created_at, updated_at`

func scanRequest(row pgx.Row) (Request, error) {
	var (
		r                            Request
		state, verification          string
		requested, reserved, settled string
		reasons                      []byte
	)
	if err := row.Scan(&r.ID, &r.AccountID, &r.DestinationID, &r.CreditAssetID, &state,
		&requested, &reserved, &settled,
		&r.PolicyVersion, &r.PolicyHash, &reasons, &verification,
		&r.Provider, &r.ProviderIdempotencyKey, &r.ProviderReference, &r.ProviderStatus, &r.IdempotencyKey, &r.QuoteID,
		&r.QuoteGrossAmountMinor, &r.QuoteFeeAmountMinor, &r.QuoteNetAmountMinor, &r.QuoteCurrency,
		&r.Sandbox, &r.Environment,
		&r.ReservedAt, &r.SubmittedAt, &r.SettledAt, &r.FailureReason,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return Request{}, err
	}
	r.State = State(state)
	r.VerificationLevel = valuedomain.VerificationLevel(verification)
	var err error
	if r.RequestedQuantity, err = money.ParseQuantity(requested); err != nil {
		return Request{}, errs.Wrap(err, errs.CodeInternal, "payout: requested quantity is not an integer")
	}
	if r.ReservedQuantity, err = money.ParseQuantity(reserved); err != nil {
		return Request{}, errs.Wrap(err, errs.CodeInternal, "payout: reserved quantity is not an integer")
	}
	if r.SettledQuantity, err = money.ParseQuantity(settled); err != nil {
		return Request{}, errs.Wrap(err, errs.CodeInternal, "payout: settled quantity is not an integer")
	}
	if len(reasons) > 0 {
		_ = json.Unmarshal(reasons, &r.EligibilityReasons)
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

// Get returns one payout request.
func (s *Service) Get(ctx context.Context, q db.Querier, id RequestID) (Request, error) {
	r, err := scanRequest(q.QueryRow(ctx, `SELECT `+requestColumns+` FROM payout_requests WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Request{}, errs.New(errs.CodeNotFound, "payout request not found").
				WithField("payout_id", id.String())
		}
		return Request{}, mapError(err)
	}
	return r, nil
}

func (s *Service) forUpdate(ctx context.Context, tx pgx.Tx, id RequestID) (Request, error) {
	r, err := scanRequest(tx.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM payout_requests WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Request{}, errs.New(errs.CodeNotFound, "payout request not found").
				WithField("payout_id", id.String())
		}
		return Request{}, mapError(err)
	}
	return r, nil
}

func (s *Service) byIdempotencyKey(ctx context.Context, q db.Querier, key string) (Request, bool, error) {
	r, err := scanRequest(q.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM payout_requests WHERE idempotency_key = $1`, key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Request{}, false, nil
		}
		return Request{}, false, mapError(err)
	}
	return r, true, nil
}

// ListByAccount returns an account's payouts, newest first.
func (s *Service) ListByAccount(ctx context.Context, q db.Querier, account accounts.AccountID, limit int) ([]Request, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx,
		`SELECT `+requestColumns+` FROM payout_requests WHERE account_id = $1
		  ORDER BY created_at DESC LIMIT $2`, account, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, r)
	}
	return out, mapError(rows.Err())
}

// allocations returns a request's lot slices.
func (s *Service) allocations(ctx context.Context, q db.Querier, id RequestID, includeReturned bool) ([]Allocation, error) {
	where := `request_id = $1`
	if !includeReturned {
		where += ` AND NOT returned`
	}
	rows, err := q.Query(ctx,
		`SELECT id, request_id, lot_id, origin, quantity::text, returned, created_at
		   FROM payout_allocations WHERE `+where+` ORDER BY created_at, id`, id)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		var (
			a      Allocation
			origin string
			qty    string
		)
		if err := rows.Scan(&a.ID, &a.RequestID, &a.LotID, &origin, &qty, &a.Returned, &a.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		a.Origin = valuedomain.CreditOrigin(origin)
		v, perr := money.ParseQuantity(qty)
		if perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "payout: allocation quantity is not an integer")
		}
		a.Quantity = v
		out = append(out, a)
	}
	return out, mapError(rows.Err())
}

// Allocations returns the lot slices a payout reserved, for operators and for
// the evidence bundle.
func (s *Service) Allocations(ctx context.Context, q db.Querier, id RequestID) ([]Allocation, error) {
	return s.allocations(ctx, q, id, true)
}

const destinationColumns = `id, account_id, kind, provider, provider_reference, display_label,
	coalesce(currency,''), status, coalesce(country,''), coalesce(region,''), masked_display, sandbox,
	verified_at, created_at, updated_at`

func scanDestination(row pgx.Row) (Destination, error) {
	var (
		d           Destination
		kind, state string
	)
	if err := row.Scan(&d.ID, &d.AccountID, &kind, &d.Provider, &d.ProviderReference, &d.DisplayLabel,
		&d.Currency, &state, &d.Country, &d.Region, &d.MaskedDisplay, &d.Sandbox,
		&d.VerifiedAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return Destination{}, err
	}
	d.Kind, d.Status = DestinationKind(kind), DestinationStatus(state)
	return d, nil
}

// CreateDestination records where a user wants value sent.
//
// The provider reference is validated to BE a provider token: a string that
// looks like a bank account, card number or IBAN is refused rather than stored
// (see destination.go). Nodal holds a token and a mask, and nothing else.
func (s *Service) CreateDestination(ctx context.Context, tx pgx.Tx, d Destination) (Destination, error) {
	if d.AccountID.IsZero() {
		return Destination{}, errs.New(errs.CodeValidationFailed, "a destination needs an account")
	}
	if !d.Kind.Valid() {
		return Destination{}, errs.Newf(errs.CodeValidationFailed, "unknown destination kind %q", d.Kind)
	}
	if strings.TrimSpace(d.Provider) == "" {
		return Destination{}, errs.New(errs.CodeValidationFailed,
			"a destination is identified by its provider and that provider's reference")
	}
	if err := ValidateDestinationToken(d.ProviderReference); err != nil {
		return Destination{}, err
	}
	if err := ValidateMaskedDisplay(d.MaskedDisplay); err != nil {
		return Destination{}, err
	}
	if !countryCode.MatchString(d.Country) {
		// Required since D-122: `CanPayRecipient` is asked unconditionally, and
		// `RecipientProfile.Validate` has always refused an empty country with
		// RECIPIENT_PROFILE_INCOMPLETE. The adapter simply never asked it, so a
		// destination the provider had said it could not pay was accepted and
		// marked VERIFIED whenever the client omitted the field (F-228).
		return Destination{}, errs.Newf(errs.CodeValidationFailed,
			"country code %q must be ISO 3166-1 alpha-2 upper case", d.Country).
			WithField("field", "country")
	}
	if d.Region != "" && !regionCode.MatchString(d.Region) {
		return Destination{}, errs.Newf(errs.CodeValidationFailed,
			"region code %q must be the subdivision code without its country prefix", d.Region).
			WithField("field", "region")
	}
	if d.ID.IsZero() {
		d.ID = NewDestinationID()
	}
	// A destination is born UNVERIFIED whatever the caller asked for. Owning an
	// account and controlling a bank account are different facts, and the
	// second is established by the provider, not by the request that created
	// the row. Migration 00763 refuses any other birth state outright.
	var currency any
	if d.Currency != "" {
		currency = d.Currency
	}
	out, err := scanDestination(tx.QueryRow(ctx,
		`INSERT INTO payout_destinations
		   (id, account_id, kind, provider, provider_reference, display_label, currency, status,
		    country, region, masked_display, sandbox)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,'UNVERIFIED',NULLIF($8,''),NULLIF($9,''),$10,$11)
		 RETURNING `+destinationColumns,
		d.ID, d.AccountID, string(d.Kind), d.Provider, strings.TrimSpace(d.ProviderReference),
		d.DisplayLabel, currency, d.Country, d.Region, d.MaskedDisplay, d.Sandbox))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Destination{}, errs.New(errs.CodeConflict, "this destination is already registered")
		}
		return Destination{}, mapError(err)
	}
	return out, nil
}

// SetDestinationStatus records a verification outcome.
//
// It is a thin wrapper over TransitionDestination that names the provider as
// the actor, because that is who decides whether a destination may receive
// value. Migration 00763 revoked the application's UPDATE on the status column,
// so the transition row is the only way it moves.
func (s *Service) SetDestinationStatus(ctx context.Context, tx pgx.Tx, id DestinationID, status DestinationStatus) (Destination, error) {
	return s.TransitionDestination(ctx, tx, id, status, DestinationChange{
		ActorType:  security.ActorSystem,
		ActorID:    "payout-service",
		Reason:     "the provider decided whether this destination may receive value",
		OccurredAt: s.clk.Now().UTC(),
	})
}

// Destination returns one destination.
func (s *Service) Destination(ctx context.Context, q db.Querier, id DestinationID) (Destination, error) {
	d, err := scanDestination(q.QueryRow(ctx,
		`SELECT `+destinationColumns+` FROM payout_destinations WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Destination{}, errs.New(errs.CodeNotFound, "payout destination not found")
		}
		return Destination{}, mapError(err)
	}
	return d, nil
}

// OpenRequestsForDestination lists the conversion requests that still point at
// a destination and have not finished, oldest first.
//
// It exists so that disabling a destination can TELL the person what it has
// just stranded. A reserved request whose destination is no longer usable is
// refused at Submit and stays in VERIFIED with its value held out of their
// balance (F-263); the holder can cancel it, and cannot be expected to guess
// that they need to.
//
// "Not finished" is State.Terminal(), read from Go rather than repeated as a
// SQL literal list, so a state added to the machine cannot quietly drop out of
// this answer.
func (s *Service) OpenRequestsForDestination(ctx context.Context, q db.Querier, id DestinationID) ([]RequestID, error) {
	if id.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "payout: a destination id is required")
	}
	terminal := make([]string, 0, 4)
	for _, st := range AllStates() {
		if st.Terminal() {
			terminal = append(terminal, string(st))
		}
	}
	rows, err := q.Query(ctx,
		`SELECT id FROM payout_requests
		  WHERE destination_id = $1 AND NOT (state = ANY($2))
		  ORDER BY created_at, id`, id, terminal)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []RequestID
	for rows.Next() {
		var reqID RequestID
		if err := rows.Scan(&reqID); err != nil {
			return nil, mapError(err)
		}
		out = append(out, reqID)
	}
	return out, mapError(rows.Err())
}

// lockDestination takes the row lock a status change needs before it checks
// that the change is legal. Without it two concurrent transitions produce a
// trail whose from_status was already superseded (00744's rule, and the reason
// a column-level UPDATE grant remains).
func (s *Service) lockDestination(ctx context.Context, tx pgx.Tx, id DestinationID) (Destination, error) {
	d, err := scanDestination(tx.QueryRow(ctx,
		`SELECT `+destinationColumns+` FROM payout_destinations WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Destination{}, errs.New(errs.CodeNotFound, "payout destination not found")
		}
		return Destination{}, mapError(err)
	}
	return d, nil
}

// DestinationsByAccount lists an account's destinations, newest first.
//
// It includes the disabled and rejected ones. A person who removed a
// destination and cannot see that it is gone will add it again, and a person
// whose destination the provider refused needs to see the refusal rather than
// an empty list.
func (s *Service) DestinationsByAccount(ctx context.Context, q db.Querier, accountID accounts.AccountID, limit int) ([]Destination, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx,
		`SELECT `+destinationColumns+` FROM payout_destinations
		  WHERE account_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		d, serr := scanDestination(rows)
		if serr != nil {
			return nil, mapError(serr)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

// regionCode is the subdivision code without its country prefix: "CA", not
// "US-CA". It is the same shape compliance_profiles.jurisdiction_region and
// verification_sessions.jurisdiction_region already hold.
var regionCode = regexp.MustCompile(`^[A-Z0-9]{1,6}$`)

// VerifyReservations checks the invariant that ties this package to the ledger:
// the Credits sitting in every account's PAYOUT_RESERVED must equal the sum of
// what open payout requests say they reserved.
//
// A divergence means value was reserved without a request, or a request claims
// value the ledger does not hold. Either is a reconciliation incident.
func (s *Service) VerifyReservations(ctx context.Context, q db.Querier, creditAssetID assets.AssetID) error {
	var requestSum, ledgerSum string
	err := q.QueryRow(ctx,
		`SELECT
		   coalesce((SELECT sum(reserved_quantity) FROM payout_requests
		              WHERE credit_asset_id = $1
		                AND state IN ('VERIFICATION_REQUIRED','VERIFICATION_PENDING','VERIFIED',
		                              'SUBMITTED','PROVIDER_PENDING','PAYOUT_STATUS_UNKNOWN','MANUAL_REVIEW')), 0)::text,
		   coalesce((SELECT sum(b.balance) FROM ledger_accounts la
		               JOIN ledger_balances b ON b.ledger_account_id = la.id
		              WHERE la.code = 'PAYOUT_RESERVED' AND la.asset_id = $1), 0)::text`,
		creditAssetID).Scan(&requestSum, &ledgerSum)
	if err != nil {
		return mapError(err)
	}
	if requestSum != ledgerSum {
		return errs.Newf(errs.CodeReconciliationRequired,
			"payout reservations do not reconcile: open requests claim %s, the ledger holds %s",
			requestSum, ledgerSum).
			WithField("request_sum", requestSum).
			WithField("ledger_sum", ledgerSum)
	}
	return nil
}
