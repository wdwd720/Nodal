package payout

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// assetsAssetID is the registry asset identifier. It is aliased so the Credits
// interface in service.go can name it without every caller importing the
// registry package.
type assetsAssetID = assets.AssetID

// accountsAccountID is the account identifier, aliased for the same reason.
type accountsAccountID = accounts.AccountID

const requestColumns = `id, account_id, destination_id, credit_asset_id, state,
	requested_quantity::text, reserved_quantity::text, settled_quantity::text,
	policy_version, policy_hash, eligibility_reasons, verification_level,
	coalesce(provider,''), coalesce(provider_idempotency_key,''), coalesce(provider_reference,''),
	coalesce(provider_status,''), idempotency_key,
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
		&r.Provider, &r.ProviderIdempotencyKey, &r.ProviderReference, &r.ProviderStatus, &r.IdempotencyKey,
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

// CreateDestination records where a user wants value sent.
func (s *Service) CreateDestination(ctx context.Context, tx pgx.Tx, d Destination) (Destination, error) {
	if d.AccountID.IsZero() {
		return Destination{}, errs.New(errs.CodeValidationFailed, "a destination needs an account")
	}
	if !d.Kind.Valid() {
		return Destination{}, errs.Newf(errs.CodeValidationFailed, "unknown destination kind %q", d.Kind)
	}
	if d.Provider == "" || d.ProviderReference == "" {
		return Destination{}, errs.New(errs.CodeValidationFailed,
			"a destination is identified by its provider and that provider's reference")
	}
	if d.ID.IsZero() {
		d.ID = NewDestinationID()
	}
	// A destination is born UNVERIFIED whatever the caller asked for. Owning an
	// account and controlling a bank account are different facts, and the
	// second is established by the provider, not by the request that created
	// the row.
	var currency any
	if d.Currency != "" {
		currency = d.Currency
	}
	err := tx.QueryRow(ctx,
		`INSERT INTO payout_destinations
		   (id, account_id, kind, provider, provider_reference, display_label, currency, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,'UNVERIFIED')
		 RETURNING created_at, updated_at`,
		d.ID, d.AccountID, string(d.Kind), d.Provider, d.ProviderReference,
		d.DisplayLabel, currency).Scan(&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Destination{}, errs.New(errs.CodeConflict, "this destination is already registered")
		}
		return Destination{}, mapError(err)
	}
	d.Status = DestinationUnverified
	return d, nil
}

// SetDestinationStatus records a verification outcome.
func (s *Service) SetDestinationStatus(ctx context.Context, tx pgx.Tx, id DestinationID, status DestinationStatus) (Destination, error) {
	if !status.Valid() {
		return Destination{}, errs.Newf(errs.CodeValidationFailed, "unknown destination status %q", status)
	}
	set := `status = $2`
	if status == DestinationVerified {
		set += `, verified_at = now()`
	}
	var (
		d           Destination
		kind, state string
		currency    *string
	)
	err := tx.QueryRow(ctx,
		`UPDATE payout_destinations SET `+set+` WHERE id = $1
		 RETURNING id, account_id, kind, provider, provider_reference, display_label, currency, status,
		           verified_at, created_at, updated_at`,
		id, string(status)).
		Scan(&d.ID, &d.AccountID, &kind, &d.Provider, &d.ProviderReference, &d.DisplayLabel,
			&currency, &state, &d.VerifiedAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Destination{}, errs.New(errs.CodeNotFound, "payout destination not found")
		}
		return Destination{}, mapError(err)
	}
	d.Kind, d.Status = DestinationKind(kind), DestinationStatus(state)
	if currency != nil {
		d.Currency = *currency
	}
	return d, nil
}

// Destination returns one destination.
func (s *Service) Destination(ctx context.Context, q db.Querier, id DestinationID) (Destination, error) {
	var (
		d           Destination
		kind, state string
		currency    *string
	)
	err := q.QueryRow(ctx,
		`SELECT id, account_id, kind, provider, provider_reference, display_label, currency, status,
		        verified_at, created_at, updated_at
		   FROM payout_destinations WHERE id = $1`, id).
		Scan(&d.ID, &d.AccountID, &kind, &d.Provider, &d.ProviderReference, &d.DisplayLabel,
			&currency, &state, &d.VerifiedAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Destination{}, errs.New(errs.CodeNotFound, "payout destination not found")
		}
		return Destination{}, mapError(err)
	}
	d.Kind, d.Status = DestinationKind(kind), DestinationStatus(state)
	if currency != nil {
		d.Currency = *currency
	}
	return d, nil
}

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
