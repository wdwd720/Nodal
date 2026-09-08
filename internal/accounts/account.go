package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

type accountKind struct{}

type userKind struct{}

// AccountID identifies an account.
type AccountID = id.ID[accountKind]

// UserID identifies a user (identity subject holder).
type UserID = id.ID[userKind]

// NewAccountID returns a fresh account id.
func NewAccountID() AccountID { return id.New[accountKind]() }

// NewUserID returns a fresh user id.
func NewUserID() UserID { return id.New[userKind]() }

// ParseAccountID parses the canonical form.
func ParseAccountID(s string) (AccountID, error) { return id.Parse[accountKind](s) }

// ParseUserID parses the canonical form.
func ParseUserID(s string) (UserID, error) { return id.Parse[userKind](s) }

// Kind distinguishes customer, canary, and platform accounts.
type Kind string

// Account kinds.
const (
	KindCustomer Kind = "CUSTOMER"
	KindCanary   Kind = "CANARY"
	KindPlatform Kind = "PLATFORM"
)

// Status is the account state (PART 194).
type Status string

// Account statuses.
const (
	StatusActive     Status = "ACTIVE"
	StatusRestricted Status = "RESTRICTED"
	StatusFrozen     Status = "FROZEN"
	StatusClosed     Status = "CLOSED"
)

// Valid reports whether s is declared.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusRestricted, StatusFrozen, StatusClosed:
		return true
	}
	return false
}

// AllowsNewRisk reports whether the account may open new exposure or funding.
func (s Status) AllowsNewRisk() bool { return s == StatusActive }

// AllowsRiskReduction reports whether existing exposure may be reduced.
func (s Status) AllowsRiskReduction() bool { return s == StatusActive || s == StatusRestricted }

var statusTransitions = map[Status][]Status{
	StatusActive:     {StatusRestricted, StatusFrozen, StatusClosed},
	StatusRestricted: {StatusActive, StatusFrozen, StatusClosed},
	StatusFrozen:     {StatusActive, StatusRestricted, StatusClosed},
	StatusClosed:     {},
}

// CanTransition reports whether from → to is legal.
func CanTransition(from, to Status) bool {
	for _, t := range statusTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// User is an identity subject holder.
type User struct {
	ID         UserID
	IdPIssuer  string
	IdPSubject string
	Status     string
	// EmailHash is sha256(lowercase email), present only when the identity
	// provider asserted a VERIFIED email address. It is a lookup key and an
	// assurance record, never a way back to the address.
	EmailHash []byte
	CreatedAt time.Time
}

// Account is a financial account owned by a user.
type Account struct {
	ID              AccountID
	OwnerUserID     UserID
	Kind            Kind
	Status          Status
	StatusReason    string
	FrozenAt        *time.Time
	CostBasisMethod string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Repository persists users and accounts.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

// CreateUser inserts a user for an identity-provider subject.
func (r *Repository) CreateUser(ctx context.Context, q db.Querier, issuer, subject string, emailHash []byte) (User, error) {
	if issuer == "" || subject == "" {
		return User{}, errs.New(errs.CodeValidationFailed, "issuer and subject required")
	}
	u := User{ID: NewUserID(), IdPIssuer: issuer, IdPSubject: subject, Status: "ACTIVE"}
	err := q.QueryRow(ctx, `INSERT INTO users (id, idp_issuer, idp_subject, email_hash, status) VALUES ($1,$2,$3,$4,'ACTIVE') RETURNING created_at`,
		u.ID, issuer, subject, emailHash).Scan(&u.CreatedAt)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return User{}, errs.Wrap(err, errs.CodeConflict, "user already exists for this identity subject")
		}
		return User{}, fmt.Errorf("accounts: create user: %w", err)
	}
	return u, nil
}

// GetUserBySubject looks a user up by identity-provider issuer and subject.
func (r *Repository) GetUserBySubject(ctx context.Context, q db.Querier, issuer, subject string) (User, error) {
	var u User
	err := q.QueryRow(ctx, `SELECT id, idp_issuer, idp_subject, status, email_hash, created_at FROM users WHERE idp_issuer = $1 AND idp_subject = $2`, issuer, subject).
		Scan(&u.ID, &u.IdPIssuer, &u.IdPSubject, &u.Status, &u.EmailHash, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, errs.New(errs.CodeNotFound, "user not found")
		}
		return User{}, fmt.Errorf("accounts: get user: %w", err)
	}
	return u, nil
}

// SetEmailHash records the identity provider's verified-email assertion for a
// user that has none yet.
//
// It only ever fills an absence: the WHERE clause refuses to overwrite a hash
// that is already there. Following a changed address is a different question
// with different consequences, and this is not the place to answer it.
func (r *Repository) SetEmailHash(ctx context.Context, q db.Querier, userID UserID, hash []byte) error {
	if len(hash) == 0 {
		return errs.New(errs.CodeValidationFailed, "an email hash is required")
	}
	if _, err := q.Exec(ctx,
		`UPDATE users SET email_hash = $2 WHERE id = $1 AND email_hash IS NULL`, userID, hash); err != nil {
		return fmt.Errorf("accounts: set email hash: %w", err)
	}
	return nil
}

const accountColumns = `id, owner_user_id, kind, status, coalesce(status_reason,''), frozen_at, cost_basis_method, created_at, updated_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	if err := row.Scan(&a.ID, &a.OwnerUserID, &a.Kind, &a.Status, &a.StatusReason, &a.FrozenAt, &a.CostBasisMethod, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return Account{}, err
	}
	return a, nil
}

// CreateAccount inserts an ACTIVE account of the given kind for the owner.
func (r *Repository) CreateAccount(ctx context.Context, q db.Querier, owner UserID, kind Kind) (Account, error) {
	switch kind {
	case KindCustomer, KindCanary, KindPlatform:
	default:
		return Account{}, errs.Newf(errs.CodeValidationFailed, "unknown account kind %q", kind)
	}
	row := q.QueryRow(ctx, `INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1,$2,$3,'ACTIVE') RETURNING `+accountColumns, NewAccountID(), owner, kind)
	a, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("accounts: create account: %w", err)
	}
	return a, nil
}

// Get returns an account by id.
func (r *Repository) Get(ctx context.Context, q db.Querier, accountID AccountID) (Account, error) {
	a, err := scanAccount(q.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, accountID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, errs.New(errs.CodeNotFound, "account not found").WithField("account_id", accountID.String())
		}
		return Account{}, fmt.Errorf("accounts: get: %w", err)
	}
	return a, nil
}

// ListByOwner returns the accounts owned by a user.
func (r *Repository) ListByOwner(ctx context.Context, q db.Querier, owner UserID) ([]Account, error) {
	rows, err := q.Query(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_user_id = $1 ORDER BY created_at, id`, owner)
	if err != nil {
		return nil, fmt.Errorf("accounts: list by owner: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("accounts: list scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StatusChange is an audited status transition request.
type StatusChange struct {
	To            Status
	ActorType     string
	ActorID       string
	Reason        string
	CorrelationID string
}

// Transition applies a status change under a row lock and records it.
// AGENT actors are rejected; illegal transitions fail with INVALID_STATE_TRANSITION.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, accountID AccountID, ch StatusChange, now time.Time) (Account, error) {
	if ch.ActorType == "AGENT" || ch.ActorType == "" {
		return Account{}, errs.New(errs.CodeForbidden, "account status can only be changed by a non-agent actor")
	}
	if ch.Reason == "" {
		return Account{}, errs.New(errs.CodeValidationFailed, "reason required")
	}
	if !ch.To.Valid() {
		return Account{}, errs.Newf(errs.CodeValidationFailed, "unknown status %q", ch.To)
	}
	cur, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1 FOR UPDATE`, accountID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, errs.New(errs.CodeNotFound, "account not found")
		}
		return Account{}, fmt.Errorf("accounts: lock: %w", err)
	}
	if !CanTransition(cur.Status, ch.To) {
		return Account{}, errs.Newf(errs.CodeInvalidStateTransition, "account status %s -> %s is not allowed", cur.Status, ch.To).
			WithField("from", string(cur.Status)).WithField("to", string(ch.To))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_status_transitions (id, account_id, from_status, to_status, actor_type, actor_id, reason, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), accountID, cur.Status, ch.To, ch.ActorType, ch.ActorID, ch.Reason, ch.CorrelationID, now.UTC()); err != nil {
		return Account{}, fmt.Errorf("accounts: record transition: %w", err)
	}
	var frozenAt *time.Time
	if ch.To == StatusFrozen {
		t := now.UTC()
		frozenAt = &t
	}
	updated, err := scanAccount(tx.QueryRow(ctx,
		`UPDATE accounts SET status = $2, status_reason = $3, frozen_at = CASE WHEN $2 = 'FROZEN' THEN $4 ELSE frozen_at END WHERE id = $1 RETURNING `+accountColumns,
		accountID, ch.To, ch.Reason, frozenAt))
	if err != nil {
		return Account{}, fmt.Errorf("accounts: update status: %w", err)
	}
	return updated, nil
}
