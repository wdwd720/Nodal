package verification

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Repository reads and writes verification_sessions, verification_checks and
// the state half of compliance_profiles.
//
// It never writes the state column directly: migration 00761 and 00762 revoke
// that privilege, and a transition row is the only way either state moves. The
// SQL below therefore INSERTs a transition and reads the row back rather than
// UPDATEing anything.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

const sessionColumns = `id, user_id, purpose, provider, coalesce(provider_ref,''), status,
	coalesce(jurisdiction_country,''), coalesce(jurisdiction_region,''), rules_version,
	environment, sandbox, coalesce(failure_reason,''), expires_at, provider_polled_at,
	created_at, updated_at`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	if err := row.Scan(&s.ID, &s.UserID, &s.Purpose, &s.Provider, &s.ProviderRef, &s.Status,
		&s.JurisdictionCountry, &s.JurisdictionRegion, &s.RulesVersion,
		&s.Environment, &s.Sandbox, &s.FailureReason, &s.ExpiresAt, &s.ProviderPolledAt,
		&s.CreatedAt, &s.UpdatedAt); err != nil {
		return Session{}, err
	}
	return s, nil
}

// MarkProviderPolled records that the provider was asked about this session
// just now.
//
// It is its own small write rather than a field on an Ingest, because the poll
// that finds NOTHING CHANGED writes nothing else at all -- and that is exactly
// the poll the interval exists to stop repeating (00818, D-133).
func (r *Repository) MarkProviderPolled(ctx context.Context, q db.Querier, id SessionID, at time.Time) error {
	if _, err := q.Exec(ctx,
		`UPDATE verification_sessions SET provider_polled_at = $2 WHERE id = $1`, id, at.UTC()); err != nil {
		return mapError(err)
	}
	return nil
}

// OwnerOf returns the user who owns an account. Verification is a property of a
// PERSON, not of an account: a person with three accounts verifies once.
func (r *Repository) OwnerOf(ctx context.Context, q db.Querier, accountID accounts.AccountID) (accounts.UserID, error) {
	var owner accounts.UserID
	err := q.QueryRow(ctx, `SELECT owner_user_id FROM accounts WHERE id = $1`, accountID).Scan(&owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Deliberately NOT_FOUND rather than a distinguishable refusal: a
		// caller asking about somebody else's account must not learn whether
		// it exists.
		return accounts.UserID{}, errs.New(errs.CodeNotFound, "no such account")
	case err != nil:
		return accounts.UserID{}, errs.Wrap(err, errs.CodeInternal, "verification: read account owner")
	}
	return owner, nil
}

// CreateSession writes a session row in status CREATED, before the provider is
// called. A crash between this write and the call leaves a row to reconcile
// against rather than a provider session nobody knows about.
func (r *Repository) CreateSession(ctx context.Context, tx pgx.Tx, s Session) (Session, error) {
	if s.ID.IsZero() {
		s.ID = NewSessionID()
	}
	row := tx.QueryRow(ctx, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, jurisdiction_country, jurisdiction_region,
		 rules_version, environment, sandbox, expires_at)
		VALUES ($1,$2,$3,$4,'CREATED',NULLIF($5,''),NULLIF($6,''),$7,$8,$9,$10)
		RETURNING `+sessionColumns,
		s.ID, s.UserID, string(s.Purpose), s.Provider,
		s.JurisdictionCountry, s.JurisdictionRegion, s.RulesVersion, s.Environment, s.Sandbox, s.ExpiresAt)
	out, err := scanSession(row)
	if err != nil {
		return Session{}, mapError(err)
	}
	return out, nil
}

// Session reads one session.
func (r *Repository) Session(ctx context.Context, q db.Querier, id SessionID) (Session, error) {
	s, err := scanSession(q.QueryRow(ctx, `SELECT `+sessionColumns+` FROM verification_sessions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, errs.New(errs.CodeNotFound, "no such verification session")
	}
	if err != nil {
		return Session{}, mapError(err)
	}
	return s, nil
}

// SessionByProviderRef finds the session a provider is talking about. It is how
// a webhook and a poll agree on which attempt they concern.
func (r *Repository) SessionByProviderRef(ctx context.Context, q db.Querier, provider, ref string) (Session, error) {
	s, err := scanSession(q.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM verification_sessions WHERE provider = $1 AND provider_ref = $2`, provider, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, errs.New(errs.CodeNotFound, "no verification session for that provider reference")
	}
	if err != nil {
		return Session{}, mapError(err)
	}
	return s, nil
}

// OpenSession returns the person's live attempt, if there is one. Migration
// 00762 permits at most one, so this can return at most one row.
func (r *Repository) OpenSession(ctx context.Context, q db.Querier, userID accounts.UserID) (Session, bool, error) {
	s, err := scanSession(q.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM verification_sessions
		  WHERE user_id = $1
		    AND status IN ('CREATED','PENDING_USER_ACTION','PROCESSING','REQUIRES_INPUT','MANUAL_REVIEW')`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, mapError(err)
	}
	return s, true, nil
}

// LockSession takes the row lock a status change needs before it checks that
// the change is legal. Without it two concurrent results produce a trail whose
// from_status was already superseded.
func (r *Repository) LockSession(ctx context.Context, tx pgx.Tx, id SessionID) (Session, error) {
	s, err := scanSession(tx.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM verification_sessions WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, errs.New(errs.CodeNotFound, "no such verification session")
	}
	if err != nil {
		return Session{}, mapError(err)
	}
	return s, nil
}

// SetProviderReference records the provider's identifier and the hosted link's
// expiry. It never records the link itself.
func (r *Repository) SetProviderReference(ctx context.Context, tx pgx.Tx, id SessionID, ref string, expiresAt *time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE verification_sessions SET provider_ref = NULLIF($2,''), expires_at = $3 WHERE id = $1`,
		id, ref, expiresAt)
	return mapError(err)
}

// SessionChange describes who moved a session and why.
type SessionChange struct {
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	ProviderEvent string
	CorrelationID string
	FailureReason string
	OccurredAt    time.Time
}

// TransitionSession moves a session by inserting the transition row that
// licenses the move. The trigger writes the status; nothing here does.
func (r *Repository) TransitionSession(ctx context.Context, tx pgx.Tx, id SessionID, to SessionStatus, ch SessionChange) (Session, error) {
	if !to.Valid() {
		return Session{}, errs.Newf(errs.CodeValidationFailed, "unknown verification session status %q", to)
	}
	if err := requireNonCustomerActor(ch.ActorType); err != nil {
		return Session{}, err
	}
	current, err := r.LockSession(ctx, tx, id)
	if err != nil {
		return Session{}, err
	}
	if current.Status == to {
		return current, nil
	}
	if !CanTransitionSession(current.Status, to) {
		return Session{}, errs.Newf(errs.CodeInvalidStateTransition,
			"a verification session cannot go %s -> %s", current.Status, to).
			WithField("session_id", id.String()).
			WithField("from", string(current.Status)).
			WithField("to", string(to))
	}
	if ch.FailureReason != "" {
		if _, uerr := tx.Exec(ctx,
			`UPDATE verification_sessions SET failure_reason = $2 WHERE id = $1`, id, ch.FailureReason); uerr != nil {
			return Session{}, mapError(uerr)
		}
	}
	occurred := ch.OccurredAt
	if occurred.IsZero() {
		return Session{}, errs.New(errs.CodeValidationFailed, "a verification transition needs a time")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO verification_session_transitions
		(id, session_id, from_status, to_status, actor_type, actor_id, reason, provider_event, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10)`,
		NewTransitionID(), id, string(current.Status), string(to),
		string(ch.ActorType), ch.ActorID, ch.Reason, ch.ProviderEvent, ch.CorrelationID, occurred); err != nil {
		return Session{}, mapError(err)
	}
	return r.Session(ctx, tx, id)
}

// RecordCheck appends one sub-check answer. The unique constraint on
// (session, kind, outcome) makes a redelivered webhook a no-op rather than a
// second row: Persona retries up to eight times (PROVIDER_BOUNDARY §3).
func (r *Repository) RecordCheck(ctx context.Context, tx pgx.Tx, c Check) error {
	if !c.Kind.Valid() || !c.Outcome.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "verification: check %q/%q is not declared", c.Kind, c.Outcome)
	}
	if c.ID.IsZero() {
		c.ID = NewCheckID()
	}
	_, err := tx.Exec(ctx, `INSERT INTO verification_checks
		(id, session_id, user_id, kind, outcome, provider, provider_ref, rules_version, environment, sandbox, detail, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12)
		ON CONFLICT (session_id, kind, outcome) DO NOTHING`,
		c.ID, c.SessionID, c.UserID, string(c.Kind), string(c.Outcome), c.Provider, c.ProviderRef,
		c.RulesVersion, c.Environment, c.Sandbox, c.Detail, c.RecordedAt)
	return mapError(err)
}

const checkColumns = `id, session_id, user_id, kind, outcome, provider, coalesce(provider_ref,''),
	rules_version, environment, sandbox, detail, recorded_at`

func scanChecks(rows pgx.Rows) ([]Check, error) {
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.ID, &c.SessionID, &c.UserID, &c.Kind, &c.Outcome, &c.Provider, &c.ProviderRef,
			&c.RulesVersion, &c.Environment, &c.Sandbox, &c.Detail, &c.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChecksForUser returns every sub-check recorded for a person, oldest first.
func (r *Repository) ChecksForUser(ctx context.Context, q db.Querier, userID accounts.UserID) ([]Check, error) {
	rows, err := q.Query(ctx,
		`SELECT `+checkColumns+` FROM verification_checks WHERE user_id = $1 ORDER BY recorded_at, id`, userID)
	if err != nil {
		return nil, mapError(err)
	}
	out, err := scanChecks(rows)
	if err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// ChecksForSession returns the sub-checks of one attempt, oldest first.
func (r *Repository) ChecksForSession(ctx context.Context, q db.Querier, id SessionID) ([]Check, error) {
	rows, err := q.Query(ctx,
		`SELECT `+checkColumns+` FROM verification_checks WHERE session_id = $1 ORDER BY recorded_at, id`, id)
	if err != nil {
		return nil, mapError(err)
	}
	out, err := scanChecks(rows)
	if err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// ProfileTransition is one move of a person's verification state.
type ProfileTransition struct {
	UserID        accounts.UserID
	To            State
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	Provider      string
	ProviderRef   string
	SessionID     *SessionID
	VerifiedAt    *time.Time
	ExpiresAt     *time.Time
	CorrelationID string
	OccurredAt    time.Time
}

// ProfileState reads a person's current verification state and its window.
// A person with no profile row is UNVERIFIED: no profile is not an error here,
// because "nothing has been established" is the right answer for somebody who
// has never been asked.
func (r *Repository) ProfileState(ctx context.Context, q db.Querier, userID accounts.UserID) (State, *time.Time, *time.Time, error) {
	var (
		state      State
		verifiedAt *time.Time
		expiresAt  *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT identity_state, verified_at, expires_at FROM compliance_profiles WHERE user_id = $1`, userID).
		Scan(&state, &verifiedAt, &expiresAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return StateUnverified, nil, nil, nil
	case err != nil:
		return "", nil, nil, mapError(err)
	}
	return state, verifiedAt, expiresAt, nil
}

// TransitionProfile moves a person's verification state by inserting the row
// that licenses the move. The trigger in migration 00761 writes the state,
// verified_at and expires_at; nothing here does.
//
// A customer never attests their own compliance state, so USER, AGENT and
// SERVICE actors are refused before the statement is built. The transitions
// table's own CHECK says the same thing, in the place that cannot be
// redeployed around.
func (r *Repository) TransitionProfile(ctx context.Context, tx pgx.Tx, t ProfileTransition) (State, error) {
	if !t.To.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown verification state %q", t.To)
	}
	if err := requireNonCustomerActor(t.ActorType); err != nil {
		return "", err
	}
	if t.OccurredAt.IsZero() {
		return "", errs.New(errs.CodeValidationFailed, "a verification transition needs a time")
	}
	var current State
	err := tx.QueryRow(ctx,
		`SELECT identity_state FROM compliance_profiles WHERE user_id = $1 FOR UPDATE`, t.UserID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errs.New(errs.CodeNotFound, "that person has no compliance profile")
	}
	if err != nil {
		return "", mapError(err)
	}
	if current == t.To {
		return current, nil
	}
	if !CanTransition(current, t.To) {
		return "", errs.Newf(errs.CodeInvalidStateTransition,
			"a verification cannot go %s -> %s", current, t.To).
			WithField("from", string(current)).
			WithField("to", string(t.To))
	}
	var sessionID any
	if t.SessionID != nil && !t.SessionID.IsZero() {
		sessionID = *t.SessionID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason, provider, provider_ref,
		 session_id, verified_at, expires_at, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,NULLIF($13,''),$14)`,
		NewTransitionID(), t.UserID, string(current), string(t.To), string(t.ActorType), t.ActorID, t.Reason,
		t.Provider, t.ProviderRef, sessionID, t.VerifiedAt, t.ExpiresAt, t.CorrelationID, t.OccurredAt); err != nil {
		return "", mapError(err)
	}
	return t.To, nil
}

// requireNonCustomerActor refuses the actors that can never decide a
// compliance state. It is the application half of the CHECK on
// compliance_profile_transitions.actor_type.
func requireNonCustomerActor(actor security.ActorType) error {
	if actor != security.ActorSystem && actor != security.ActorOperator {
		return errs.New(errs.CodeForbidden,
			"verification decisions are recorded by SYSTEM or OPERATOR actors only; a customer never attests their own")
	}
	return nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errs.CodeOf(err) != "" && errs.CodeOf(err) != errs.CodeInternal {
		return err
	}
	switch db.SQLState(err) {
	case "AU001", "AD001":
		return errs.Wrap(err, errs.CodeInvalidStateTransition, "that verification change is not one the schema licenses")
	case "23505":
		return errs.Wrap(err, errs.CodeConflict, "that verification record already exists")
	case "42501":
		return errs.Wrap(err, errs.CodeInternal, "verification: the application role may not write that column")
	}
	return fmt.Errorf("verification: %w", err)
}
