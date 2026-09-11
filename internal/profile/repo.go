package profile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/terms"
)

// Repository is the SQL half of this package. It holds no state and takes a
// db.Querier so the same code runs inside or outside a transaction.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

const profileColumns = `user_id, coalesce(display_name,''), coalesce(handle,''), locale, time_zone, avatar_seed,
	onboarding_started_at, display_name_set_at, terms_accepted_at, onboarding_completed_at, created_at, updated_at`

func scanProfile(row pgx.Row) (Profile, error) {
	var p Profile
	err := row.Scan(&p.UserID, &p.DisplayName, &p.Handle, &p.Locale, &p.TimeZone, &p.AvatarSeed,
		&p.Onboarding.StartedAt, &p.Onboarding.DisplayNameSetAt, &p.Onboarding.TermsAcceptedAt,
		&p.Onboarding.CompletedAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Profile{}, err
	}
	return p, nil
}

// Get returns a user's profile.
func (r *Repository) Get(ctx context.Context, q db.Querier, userID string) (Profile, error) {
	p, err := scanProfile(q.QueryRow(ctx, `SELECT `+profileColumns+` FROM user_profiles WHERE user_id = $1`, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Profile{}, errs.New(errs.CodeNotFound, "no profile for this user")
		}
		return Profile{}, fmt.Errorf("profile: get: %w", err)
	}
	return p, nil
}

// Ensure returns the user's profile, creating it if this is the first time
// anyone has asked. The second return value reports whether it was created.
//
// Lazy creation is deliberate. A profile row written at login would be written
// inside the login transaction, where a failure fails the login -- and there is
// no product reason a person must have a profile before they have looked at
// anything. ON CONFLICT DO NOTHING makes two simultaneous first requests
// produce one row rather than a unique violation.
func (r *Repository) Ensure(ctx context.Context, q db.Querier, userID string, now time.Time) (Profile, bool, error) {
	tag, err := q.Exec(ctx, `INSERT INTO user_profiles (user_id, locale, time_zone, avatar_seed, onboarding_started_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5, $5) ON CONFLICT (user_id) DO NOTHING`,
		userID, DefaultLocale, DefaultTimeZone, AvatarSeedFor(userID), now.UTC())
	if err != nil {
		if db.SQLState(err) == db.SQLStateForeignKeyViolation {
			return Profile{}, false, errs.New(errs.CodeNotFound, "no such user")
		}
		return Profile{}, false, fmt.Errorf("profile: ensure: %w", err)
	}
	p, err := r.Get(ctx, q, userID)
	if err != nil {
		return Profile{}, false, err
	}
	return p, tag.RowsAffected() == 1, nil
}

// updateProfileSQL is one constant statement rather than a clause list joined at
// call time.
//
// A patch has four independently-present fields, which reads as a case for
// building the SET list dynamically -- and that is exactly the shape
// test/security/TestSQLInjection_EveryStatementIsBuiltFromConstants exists to
// refuse, because a statement assembled from strings cannot be proven constant
// by reading it. Each field therefore carries a "present" boolean beside its
// value, and the CASE keeps the column as it was when the boolean is false.
//
// display_name_set_at fills once, in the same statement: it records when the
// user FIRST chose a name, and 00756 refuses to let a later statement move it.
const updateProfileSQL = `UPDATE user_profiles SET
	display_name        = CASE WHEN $3 THEN NULLIF($4, '')                       ELSE display_name END,
	display_name_set_at = CASE WHEN $3 THEN coalesce(display_name_set_at, $2)    ELSE display_name_set_at END,
	handle              = CASE WHEN $5 THEN NULLIF($6, '')                       ELSE handle END,
	locale              = CASE WHEN $7 THEN $8                                   ELSE locale END,
	time_zone           = CASE WHEN $9 THEN $10                                  ELSE time_zone END
	WHERE user_id = $1 RETURNING ` + profileColumns

// Apply writes a validated patch and returns the updated profile.
func (r *Repository) Apply(ctx context.Context, q db.Querier, userID string, p Patch, now time.Time) (Profile, error) {
	if p.Empty() {
		return r.Get(ctx, q, userID)
	}
	deref := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	out, err := scanProfile(q.QueryRow(ctx, updateProfileSQL, userID, now.UTC(),
		p.DisplayName != nil, deref(p.DisplayName),
		p.Handle != nil, deref(p.Handle),
		p.Locale != nil, deref(p.Locale),
		p.TimeZone != nil, deref(p.TimeZone)))
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return Profile{}, errs.New(errs.CodeNotFound, "no profile for this user")
		case db.IsUniqueViolation(err):
			return Profile{}, errs.Wrap(err, errs.CodeConflict, "that handle is already taken").
				WithField("field", "handle")
		}
		return Profile{}, fmt.Errorf("profile: apply: %w", err)
	}
	return out, nil
}

// StampTermsAccepted records that every document required at onboarding has been
// accepted, and completes onboarding if nothing else is outstanding. Both stamps
// are fill-once.
func (r *Repository) StampTermsAccepted(ctx context.Context, q db.Querier, userID string, now time.Time) (Profile, error) {
	out, err := scanProfile(q.QueryRow(ctx, `UPDATE user_profiles
		SET terms_accepted_at = coalesce(terms_accepted_at, $2),
		    onboarding_completed_at = CASE
		        WHEN onboarding_completed_at IS NOT NULL THEN onboarding_completed_at
		        WHEN display_name_set_at IS NOT NULL THEN $2
		        ELSE NULL END
		WHERE user_id = $1 RETURNING `+profileColumns, userID, now.UTC()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Profile{}, errs.New(errs.CodeNotFound, "no profile for this user")
		}
		return Profile{}, fmt.Errorf("profile: stamp terms: %w", err)
	}
	return out, nil
}

// CompleteIfReady stamps onboarding_completed_at when every step is done. It is
// called after a profile edit, so that setting a display name last completes
// onboarding just as accepting the terms last does.
func (r *Repository) CompleteIfReady(ctx context.Context, q db.Querier, userID string, now time.Time) (Profile, error) {
	out, err := scanProfile(q.QueryRow(ctx, `UPDATE user_profiles
		SET onboarding_completed_at = $2
		WHERE user_id = $1 AND onboarding_completed_at IS NULL
		  AND display_name_set_at IS NOT NULL AND terms_accepted_at IS NOT NULL
		RETURNING `+profileColumns, userID, now.UTC()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing to complete; return the row as it stands.
			return r.Get(ctx, q, userID)
		}
		return Profile{}, fmt.Errorf("profile: complete: %w", err)
	}
	return out, nil
}

// Acceptance is one recorded acceptance of one document version.
type Acceptance struct {
	ID          string
	UserID      string
	DocumentID  terms.DocumentID
	Version     string
	ContentHash string
	ActorType   string
	ActorID     string
	SessionID   string
	AcceptedAt  time.Time
	// Replayed reports that this acceptance already existed: the same person
	// accepting the same bytes of the same version twice is one fact, not two,
	// and the caller uses this to avoid writing a second audit event for it.
	Replayed bool
}

// RecordAcceptance appends an acceptance. Re-accepting exactly the same bytes of
// the same version is idempotent: the row that is already there is returned.
func (r *Repository) RecordAcceptance(ctx context.Context, q db.Querier, a Acceptance, ip, userAgent string, now time.Time) (Acceptance, error) {
	a.AcceptedAt = now.UTC()
	newID := id.New[id.Any]().String()
	var sessionArg any
	if a.SessionID != "" {
		sessionArg = a.SessionID
	}
	// DO NOTHING and not DO UPDATE: the table is append-only and its
	// forbid_mutation trigger fires on the UPDATE path of an upsert, so an
	// idempotent re-acceptance would raise instead of being idempotent. The
	// row that is already there is read back instead.
	err := q.QueryRow(ctx, `INSERT INTO terms_acceptances
		(id, user_id, document_id, version, content_hash, actor_type, actor_id, session_id, source_ip, user_agent, accepted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::uuid,$9,NULLIF($10,''),$11)
		ON CONFLICT (user_id, document_id, version, content_hash) DO NOTHING
		RETURNING id, accepted_at`,
		newID, a.UserID, string(a.DocumentID), a.Version, a.ContentHash, a.ActorType, a.ActorID,
		sessionArg, nullIP(ip), userAgent, a.AcceptedAt).Scan(&a.ID, &a.AcceptedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err = q.QueryRow(ctx, `SELECT id, accepted_at FROM terms_acceptances
			WHERE user_id = $1 AND document_id = $2 AND version = $3 AND content_hash = $4`,
			a.UserID, string(a.DocumentID), a.Version, a.ContentHash).Scan(&a.ID, &a.AcceptedAt); err != nil {
			return Acceptance{}, fmt.Errorf("profile: read back acceptance: %w", err)
		}
		a.Replayed = true
	case err != nil:
		return Acceptance{}, fmt.Errorf("profile: record acceptance: %w", err)
	}
	return a, nil
}

func nullIP(ip string) any {
	if strings.TrimSpace(ip) == "" {
		return nil
	}
	return ip
}

// Acceptances returns every acceptance a user has, newest first.
func (r *Repository) Acceptances(ctx context.Context, q db.Querier, userID string) ([]Acceptance, error) {
	rows, err := q.Query(ctx, `SELECT id, user_id, document_id, version, content_hash, actor_type, actor_id,
		coalesce(session_id::text,''), accepted_at
		FROM terms_acceptances WHERE user_id = $1 ORDER BY accepted_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("profile: acceptances: %w", err)
	}
	defer rows.Close()
	var out []Acceptance
	for rows.Next() {
		var a Acceptance
		var doc string
		if err := rows.Scan(&a.ID, &a.UserID, &doc, &a.Version, &a.ContentHash, &a.ActorType, &a.ActorID,
			&a.SessionID, &a.AcceptedAt); err != nil {
			return nil, fmt.Errorf("profile: acceptance scan: %w", err)
		}
		a.DocumentID = terms.DocumentID(doc)
		out = append(out, a)
	}
	return out, rows.Err()
}

const closureColumns = `id, user_id, state, coalesce(requested_reason,''), requested_at, cooling_off_until,
	coalesce(requested_session_id::text,''), decided_at, coalesce(decided_reason,'')`

func scanClosure(row pgx.Row) (ClosureRequest, error) {
	var c ClosureRequest
	err := row.Scan(&c.ID, &c.UserID, &c.State, &c.RequestedReason, &c.RequestedAt, &c.CoolingOffUntil,
		&c.SessionID, &c.DecidedAt, &c.DecidedReason)
	if err != nil {
		return ClosureRequest{}, err
	}
	return c, nil
}

// PendingClosure returns the user's open closure request, if there is one.
func (r *Repository) PendingClosure(ctx context.Context, q db.Querier, userID string) (ClosureRequest, bool, error) {
	c, err := scanClosure(q.QueryRow(ctx,
		`SELECT `+closureColumns+` FROM account_closure_requests WHERE user_id = $1 AND state = 'PENDING'`, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ClosureRequest{}, false, nil
		}
		return ClosureRequest{}, false, fmt.Errorf("profile: pending closure: %w", err)
	}
	return c, true, nil
}

// LastClosure returns the user's most recent closure request, if any.
func (r *Repository) LastClosure(ctx context.Context, q db.Querier, userID string) (ClosureRequest, bool, error) {
	c, err := scanClosure(q.QueryRow(ctx,
		// id breaks a tie on requested_at. Two requests can share an instant --
		// a decided one and its replacement, under a clock that did not move --
		// and "the most recent" has to be an answer, not a coin toss. The id is
		// a UUIDv7, so it orders by creation time.
		`SELECT `+closureColumns+` FROM account_closure_requests WHERE user_id = $1
		 ORDER BY requested_at DESC, id DESC LIMIT 1`, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ClosureRequest{}, false, nil
		}
		return ClosureRequest{}, false, fmt.Errorf("profile: last closure: %w", err)
	}
	return c, true, nil
}

// CreateClosure inserts a PENDING request. A second one while another is open is
// a CONFLICT, refused by the partial unique index rather than by a read-then-write
// that two requests could both pass.
func (r *Repository) CreateClosure(ctx context.Context, q db.Querier, c ClosureRequest) (ClosureRequest, error) {
	// The state is not taken from the argument and not set on it either: the
	// INSERT writes PENDING as a literal and the birth trigger in 00758 refuses
	// anything else, so a caller cannot ask for a request that starts decided.
	c.ID = id.New[id.Any]().String()
	var sessionArg any
	if c.SessionID != "" {
		sessionArg = c.SessionID
	}
	out, err := scanClosure(q.QueryRow(ctx, `INSERT INTO account_closure_requests
		(id, user_id, state, requested_reason, requested_at, cooling_off_until, requested_session_id, created_at, updated_at)
		VALUES ($1,$2,'PENDING',NULLIF($3,''),$4,$5,$6::uuid,$4,$4) RETURNING `+closureColumns,
		c.ID, c.UserID, c.RequestedReason, c.RequestedAt.UTC(), c.CoolingOffUntil.UTC(), sessionArg))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return ClosureRequest{}, errs.Wrap(err, errs.CodeConflict,
				"a closure request is already open for this account")
		}
		return ClosureRequest{}, fmt.Errorf("profile: create closure: %w", err)
	}
	return out, nil
}

// LockClosure reads a request FOR UPDATE so a decision is serialised.
func (r *Repository) LockClosure(ctx context.Context, tx pgx.Tx, requestID string) (ClosureRequest, error) {
	c, err := scanClosure(tx.QueryRow(ctx, `SELECT `+closureColumns+` FROM account_closure_requests WHERE id = $1 FOR UPDATE`, requestID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ClosureRequest{}, errs.New(errs.CodeNotFound, "no such closure request")
		}
		return ClosureRequest{}, fmt.Errorf("profile: lock closure: %w", err)
	}
	return c, nil
}

// TransitionClosure inserts the transition row that IS the state change: the
// trigger installed by 00758 writes state, decided_at and decided_reason from
// it, and refuses an EFFECTED transition before the cooling-off period passes.
func (r *Repository) TransitionClosure(ctx context.Context, tx pgx.Tx, c ClosureRequest, to ClosureState, actorType, actorID, reason, correlationID string, now time.Time) (ClosureRequest, error) {
	if !to.Valid() {
		return ClosureRequest{}, errs.Newf(errs.CodeValidationFailed, "unknown closure state %q", to)
	}
	if !CanCloseTransition(c.State, to) {
		return ClosureRequest{}, errs.Newf(errs.CodeInvalidStateTransition, "closure request %s -> %s is not allowed", c.State, to).
			WithField("from", string(c.State)).WithField("to", string(to))
	}
	if reason == "" {
		return ClosureRequest{}, errs.New(errs.CodeValidationFailed, "reason required")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_request_transitions
		(id, request_id, from_state, to_state, actor_type, actor_id, reason, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), c.ID, string(c.State), string(to), actorType, actorID, reason, correlationID, now.UTC()); err != nil {
		return ClosureRequest{}, fmt.Errorf("profile: closure transition: %w", err)
	}
	return scanClosureErr(tx.QueryRow(ctx, `SELECT `+closureColumns+` FROM account_closure_requests WHERE id = $1`, c.ID))
}

func scanClosureErr(row pgx.Row) (ClosureRequest, error) {
	c, err := scanClosure(row)
	if err != nil {
		return ClosureRequest{}, fmt.Errorf("profile: read back closure: %w", err)
	}
	return c, nil
}

// TransitionUserStatus inserts the row that moves users.status. Migration 00757
// makes it the only way the column moves, and 00798 binds the edge, so an
// illegal move is refused twice: here, with the domain's own error, and there,
// with a constraint violation for anything that reaches the table another way.
func (r *Repository) TransitionUserStatus(ctx context.Context, tx pgx.Tx, userID, from, to, actorType, actorID, reason, correlationID string, now time.Time) error {
	if !CanUserStatusTransition(from, to) {
		return errs.Newf(errs.CodeInvalidStateTransition, "a user may not go %s -> %s", from, to).
			WithField("from", from).WithField("to", to)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_status_transitions
		(id, user_id, from_status, to_status, actor_type, actor_id, reason, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), userID, from, to, actorType, actorID, reason, correlationID, now.UTC()); err != nil {
		return fmt.Errorf("profile: user status transition: %w", err)
	}
	return nil
}

// LockUser reads the user's status FOR UPDATE.
func (r *Repository) LockUser(ctx context.Context, tx pgx.Tx, userID string) (string, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errs.New(errs.CodeNotFound, "no such user")
		}
		return "", fmt.Errorf("profile: lock user: %w", err)
	}
	return status, nil
}

// terminalPayoutStates are the payout states that hold nothing and wait for
// nothing.
//
// It is internal/payout's list, repeated here rather than imported: the payout
// package pulls in the ledger, the gates and the configuration, and this package
// needs four strings. TestClosureBlockers_TheTerminalPayoutStatesAreThePayout
// Packages holds the two lists together, so the copy cannot drift -- which is
// the only thing that made it a copy worth having.
var terminalPayoutStates = []string{"SETTLED", "FAILED", "REJECTED", "REVERSED"}

// closureBlockersSQL reads the three financial facts a closure decision needs,
// across every account the person owns.
//
// One statement rather than three because the operator is shown one answer and
// the decision is refused on one answer: three round trips could report a
// balance from before a payout reserved against it.
//
// The Credit figure is the sum of remaining lot quantities, which is what
// credit.Balances calls Gross and what credit.VerifyProvenance pins to the
// ledger's CREDIT_BALANCE. Gross and not spendable, because a disputed or frozen
// lot is still value that belongs to the person whose account this is.
const closureBlockersSQL = `SELECT
	coalesce((SELECT sum(st.remaining_quantity)
	            FROM credit_lots l
	            JOIN credit_lot_state st ON st.lot_id = l.id
	            JOIN accounts a ON a.id = l.account_id
	           WHERE a.owner_user_id = $1 AND st.remaining_quantity > 0), 0)::text,
	(SELECT count(*) FROM payout_requests p
	   JOIN accounts a ON a.id = p.account_id
	  WHERE a.owner_user_id = $1 AND p.state <> ALL($2::text[])),
	(SELECT count(*) FROM native_positions n
	   JOIN accounts a ON a.id = n.account_id
	  WHERE a.owner_user_id = $1 AND n.quantity > 0)`

// ClosureBlockers reads what the person's accounts still hold.
func (r *Repository) ClosureBlockers(ctx context.Context, q db.Querier, userID string) (ClosureBlockers, error) {
	var b ClosureBlockers
	if err := q.QueryRow(ctx, closureBlockersSQL, userID, terminalPayoutStates).
		Scan(&b.CreditBalance, &b.OpenPayoutRequests, &b.OpenNativePositions); err != nil {
		return ClosureBlockers{}, fmt.Errorf("profile: closure blockers: %w", err)
	}
	return b, nil
}

// lockHoldingsSQL takes an exclusive row lock on every account the person owns.
//
// It exists because the blockers above are an aggregate read, and an aggregate
// read sees a snapshot. Under READ COMMITTED a payout Create committing between
// that read and the closure's own commit is invisible to both: the closure sees
// no open request, the payout sees an ACTIVE account, and the reservation lands
// on an account that is CLOSED a moment later -- value held out of the balance
// of somebody who can no longer sign in to cancel it (F-249).
//
// PostgreSQL will not take a row lock through an aggregate ("FOR UPDATE is not
// allowed with aggregate functions"), so this is a second statement rather than
// a clause on the first. It locks `accounts` because that is the row both sides
// name: `payout.Service.guardWithdraw` takes FOR SHARE on exactly this row
// before it reads the account's status, and FOR SHARE conflicts with FOR
// UPDATE. So a conversion request in flight either commits before the closure
// reads the blockers -- and blocks it, with the reason the operator can give the
// person -- or waits here until the closure has committed, and then meets the
// CLOSED status in its own guard and is refused.
//
// `cp_app` may take it: 00744 revoked table-wide UPDATE on `accounts` and left
// a column grant on `cost_basis_method`, and a column grant is what a row lock
// needs (00744 probed exactly this).
const lockHoldingsSQL = `SELECT id FROM accounts WHERE owner_user_id = $1 ORDER BY id FOR UPDATE`

// LockHoldings takes the lock described above. It is called by the EFFECT path
// only: a read-only view of the blockers must not take locks that make an
// operator's screen contend with a customer's payout.
func (r *Repository) LockHoldings(ctx context.Context, tx pgx.Tx, userID string) error {
	rows, err := tx.Query(ctx, lockHoldingsSQL, userID)
	if err != nil {
		return fmt.Errorf("profile: lock holdings: %w", err)
	}
	defer rows.Close()
	for rows.Next() { //nolint:revive // the rows are locked, not read
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("profile: lock holdings: %w", err)
	}
	return nil
}
