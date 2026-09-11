package profile

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Restriction is one reason the product is not fully available to somebody,
// written for the person it applies to.
//
// The message is composed here from the STATUS, and deliberately not from
// `accounts.status_reason`. That column holds free text an operator wrote for
// other operators -- a ticket number, a counterparty name, an internal
// classification -- and echoing it to the account holder would publish
// whatever the operator happened to type. The user is told what is restricted
// and what to do; the operator's words stay in the support view, where they
// were written to be read.
type Restriction struct {
	Code      string
	AccountID string
	Message   string
}

// Restriction codes. They are stable identifiers a UI can branch on.
const (
	RestrictionUserSuspended     = "USER_SUSPENDED"
	RestrictionUserClosed        = "USER_CLOSED"
	RestrictionAccountRestricted = "ACCOUNT_RESTRICTED"
	RestrictionAccountFrozen     = "ACCOUNT_FROZEN"
	RestrictionAccountClosed     = "ACCOUNT_CLOSED"
	RestrictionClosurePending    = "CLOSURE_PENDING"
)

// AccountView is what a user is told about the standing of their own account.
type AccountView struct {
	UserID       string
	UserStatus   string
	Accounts     []accounts.Account
	Restrictions []Restriction
	// Closure is the open request, if there is one.
	Closure *ClosureRequest
	// CoolingOff is how long a new request would have to wait, so the surface
	// can say so before the user asks rather than after.
	CoolingOff time.Duration
}

// Account returns the caller's own account standing.
func (s *Service) Account(ctx context.Context, a Actor) (AccountView, error) {
	if err := a.validate(); err != nil {
		return AccountView{}, err
	}
	return s.accountView(ctx, s.d.DB, a.UserID)
}

func (s *Service) accountView(ctx context.Context, q db.Querier, userID string) (AccountView, error) {
	uid, err := accounts.ParseUserID(userID)
	if err != nil {
		return AccountView{}, errs.Wrap(err, errs.CodeInternal, "profile: subject is not a user id")
	}
	var status string
	if err := q.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccountView{}, errs.New(errs.CodeNotFound, "no such user")
		}
		return AccountView{}, fmt.Errorf("profile: read user status: %w", err)
	}
	owned, err := s.d.Accounts.ListByOwner(ctx, q, uid)
	if err != nil {
		return AccountView{}, err
	}
	// The MOST RECENT request, not the open one. A refusal carries the reason an
	// operator gave, and the person it is about has to be able to read it; a
	// view that showed only an open request would answer "no request" to
	// somebody who had just been told no.
	last, hasAny, err := s.d.Repo.LastClosure(ctx, q, userID)
	if err != nil {
		return AccountView{}, err
	}
	view := AccountView{UserID: userID, UserStatus: status, Accounts: owned, CoolingOff: s.d.CoolingOff}
	if hasAny {
		c := last
		view.Closure = &c
	}
	view.Restrictions = restrictionsFor(status, owned, view.Closure)
	return view, nil
}

func restrictionsFor(userStatus string, owned []accounts.Account, closure *ClosureRequest) []Restriction {
	var out []Restriction
	switch userStatus {
	case "SUSPENDED":
		out = append(out, Restriction{
			Code:    RestrictionUserSuspended,
			Message: "This account is suspended. You can sign in and read your records, but you cannot trade, buy Credits or request a payout. Contact support to ask why.",
		})
	case "CLOSED":
		out = append(out, Restriction{
			Code:    RestrictionUserClosed,
			Message: "This account is closed.",
		})
	}
	for _, acct := range owned {
		switch acct.Status {
		case accounts.StatusRestricted:
			out = append(out, Restriction{
				Code: RestrictionAccountRestricted, AccountID: acct.ID.String(),
				Message: "This account is restricted. You can close or reduce what you already hold, but you cannot open anything new.",
			})
		case accounts.StatusFrozen:
			out = append(out, Restriction{
				Code: RestrictionAccountFrozen, AccountID: acct.ID.String(),
				Message: "This account is frozen. No position can be opened or reduced while it is. Contact support.",
			})
		case accounts.StatusClosed:
			out = append(out, Restriction{
				Code: RestrictionAccountClosed, AccountID: acct.ID.String(),
				Message: "This account is closed.",
			})
		}
	}
	// Only an OPEN request restricts anything. A decided one is history.
	if closure != nil && closure.State == ClosurePending {
		out = append(out, Restriction{
			Code:    RestrictionClosurePending,
			Message: "You have asked for this account to be closed. Until it is, everything still works, and you can cancel the request at any time.",
		})
	}
	return out
}

// RequestClosure opens a closure request for the caller's own account.
//
// It does not close anything. It starts a clock the user can stop, and it is
// the only self-service route to closure: nothing here can be reached by an
// operator acting on somebody else, and nothing here deletes a record.
func (s *Service) RequestClosure(ctx context.Context, a Actor, reason string) (AccountView, error) {
	if err := a.validate(); err != nil {
		return AccountView{}, err
	}
	clean, err := ValidateClosureReason(reason)
	if err != nil {
		return AccountView{}, err
	}
	now := s.d.Clock.Now()
	var out AccountView
	err = s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		status, err := s.d.Repo.LockUser(ctx, tx, a.UserID)
		if err != nil {
			return err
		}
		if status == "CLOSED" {
			return errs.New(errs.CodeInvalidStateTransition, "this account is already closed")
		}
		req, err := s.d.Repo.CreateClosure(ctx, tx, ClosureRequest{
			UserID: a.UserID, RequestedReason: clean, RequestedAt: now,
			CoolingOffUntil: now.Add(s.d.CoolingOff), SessionID: a.SessionID,
		})
		if err != nil {
			return err
		}
		if err := s.append(ctx, tx, a, ActionClosureRequested, "account_closure_request", req.ID, map[string]any{
			"cooling_off_until": req.CoolingOffUntil.UTC().Format(time.RFC3339),
			"has_reason":        clean != "",
		}, now); err != nil {
			return err
		}
		out, err = s.accountView(ctx, tx, a.UserID)
		return err
	})
	return out, err
}

// CancelClosure stops the caller's own pending closure request.
//
// It requires no step-up, and that asymmetry is the point: requesting closure is
// the dangerous direction and asks for a recent strong authentication; stopping
// one is the safe direction and must never be harder than starting one, or a
// user who cannot step up would be unable to undo a request made from a session
// that could (D-055).
func (s *Service) CancelClosure(ctx context.Context, a Actor) (AccountView, error) {
	if err := a.validate(); err != nil {
		return AccountView{}, err
	}
	now := s.d.Clock.Now()
	var out AccountView
	err := s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		pending, ok, err := s.d.Repo.PendingClosure(ctx, tx, a.UserID)
		if err != nil {
			return err
		}
		if !ok {
			return errs.New(errs.CodeNotFound, "there is no open closure request on this account")
		}
		locked, err := s.d.Repo.LockClosure(ctx, tx, pending.ID)
		if err != nil {
			return err
		}
		req, err := s.d.Repo.TransitionClosure(ctx, tx, locked, ClosureCancelled,
			string(a.ActorType), a.UserID, "cancelled by the account owner", a.CorrelationID, now)
		if err != nil {
			return err
		}
		if err := s.append(ctx, tx, a, ActionClosureCancelled, "account_closure_request", req.ID, nil, now); err != nil {
			return err
		}
		out, err = s.accountView(ctx, tx, a.UserID)
		return err
	})
	return out, err
}

// ClosureDecision is what an operator may do to a pending closure request.
type ClosureDecision string

// The operator decisions.
const (
	// DecisionCancel stops a request on the user's behalf -- the support case
	// where somebody phones to say they changed their mind.
	DecisionCancel ClosureDecision = "CANCEL"
	// DecisionRefuse declines to close, with a reason the user is shown.
	DecisionRefuse ClosureDecision = "REFUSE"
	// DecisionEffect closes the account. It is refused before the cooling-off
	// period ends, by the database as well as by this code.
	DecisionEffect ClosureDecision = "EFFECT"
)

// AllClosureDecisions returns every declared decision.
func AllClosureDecisions() []ClosureDecision {
	return []ClosureDecision{DecisionCancel, DecisionRefuse, DecisionEffect}
}

// Valid reports whether d is declared.
func (d ClosureDecision) Valid() bool {
	for _, v := range AllClosureDecisions() {
		if v == d {
			return true
		}
	}
	return false
}

// Decide applies an operator's decision to a user's pending closure request.
//
// The operator is identified by their own principal. There is no route by which
// a person decides their own request as an operator: `Decide` refuses when the
// operator's subject is the request's owner, which is the same distinct-principal
// rule the admin plane applies elsewhere and is why this is not simply "an
// operator may close any account".
func (s *Service) Decide(ctx context.Context, op Actor, targetUserID string, d ClosureDecision, reason string) (AdminUserView, error) {
	if err := op.validate(); err != nil {
		return AdminUserView{}, err
	}
	if !d.Valid() {
		return AdminUserView{}, errs.Newf(errs.CodeValidationFailed, "unknown decision %q", d).WithField("field", "decision")
	}
	if len(reason) < 8 {
		return AdminUserView{}, errs.New(errs.CodeValidationFailed, "a reason of at least 8 characters is required").
			WithField("field", "reason")
	}
	if targetUserID == "" {
		return AdminUserView{}, errs.New(errs.CodeValidationFailed, "a user id is required")
	}
	if targetUserID == op.UserID {
		return AdminUserView{}, errs.Wrap(security.ErrSelfApproval, errs.CodeForbidden,
			"an operator may not decide their own closure request; a distinct principal is required")
	}
	now := s.d.Clock.Now()
	var out AdminUserView
	err := s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		pending, ok, err := s.d.Repo.PendingClosure(ctx, tx, targetUserID)
		if err != nil {
			return err
		}
		if !ok {
			return errs.New(errs.CodeNotFound, "there is no open closure request for this user")
		}
		locked, err := s.d.Repo.LockClosure(ctx, tx, pending.ID)
		if err != nil {
			return err
		}
		to, action := ClosureCancelled, ActionClosureCancelled
		switch d {
		case DecisionRefuse:
			to, action = ClosureRefused, ActionClosureRefused
		case DecisionEffect:
			to, action = ClosureEffected, ActionClosureEffected
			if !locked.Effectable(now) {
				return errs.Newf(errs.CodeInvalidStateTransition,
					"this closure request cannot be effected until %s", locked.CoolingOffUntil.UTC().Format(time.RFC3339)).
					WithField("cooling_off_until", locked.CoolingOffUntil.UTC().Format(time.RFC3339))
			}
			// The three facts 00758 and this package both name as the reason
			// REFUSED exists. Effecting past one of them puts value out of
			// reach of the person it belongs to, permanently, and until F-179
			// nothing here consulted any of them.
			//
			// The refusal names the blocker rather than saying "not now": the
			// operator has to be able to tell the person what to do about it,
			// and REFUSE is the decision that carries that reason to them.
			blockers, berr := s.d.Repo.ClosureBlockers(ctx, tx, targetUserID)
			if berr != nil {
				return berr
			}
			if !blockers.Clear() {
				return errs.Newf(errs.CodeInvalidStateTransition,
					"this closure cannot be effected yet: %s", strings.Join(blockers.Reasons(), "; ")).
					WithField("credit_balance", blockers.CreditBalance).
					WithField("open_payout_requests", strconv.Itoa(blockers.OpenPayoutRequests)).
					WithField("open_native_positions", strconv.Itoa(blockers.OpenNativePositions))
			}
		}
		req, err := s.d.Repo.TransitionClosure(ctx, tx, locked, to, string(op.ActorType), op.UserID, reason, op.CorrelationID, now)
		if err != nil {
			return err
		}
		payload := map[string]any{"decision": string(d), "target_user_id": targetUserID}
		if d == DecisionEffect {
			closed, err := s.closeEverything(ctx, tx, op, targetUserID, reason, now)
			if err != nil {
				return err
			}
			payload["accounts_closed"] = closed.accounts
			payload["sessions_revoked"] = closed.sessions
		}
		// An operator's decision is an admin-plane event: it belongs on the
		// admin stream as well as being about one user, and the admin stream is
		// the one an auditor reads to see what staff did.
		if _, err := s.d.Audit.Append(ctx, tx, audit.Event{
			Stream: audit.AdminStream, ActorType: string(op.ActorType), ActorID: op.UserID,
			Action: action, ResourceType: "account_closure_request", ResourceID: req.ID,
			RequestID: op.RequestID, CorrelationID: op.CorrelationID, Reason: reason,
			SourceIP: op.IP, Device: op.UserAgent, Payload: mustJSON(payload), OccurredAt: now,
		}); err != nil {
			return err
		}
		out, err = s.adminUserView(ctx, tx, targetUserID)
		return err
	})
	return out, err
}

type closedWhat struct {
	accounts int
	sessions int
}

// closeEverything performs the closure itself: the user status, every account
// they own, and every session they hold. It deletes nothing.
func (s *Service) closeEverything(ctx context.Context, tx pgx.Tx, op Actor, userID, reason string, now time.Time) (closedWhat, error) {
	var out closedWhat
	status, err := s.d.Repo.LockUser(ctx, tx, userID)
	if err != nil {
		return out, err
	}
	if status != "CLOSED" {
		if err := s.d.Repo.TransitionUserStatus(ctx, tx, userID, status, "CLOSED",
			string(op.ActorType), op.UserID, reason, op.CorrelationID, now); err != nil {
			return out, err
		}
	}
	uid, err := accounts.ParseUserID(userID)
	if err != nil {
		return out, errs.Wrap(err, errs.CodeInternal, "profile: subject is not a user id")
	}
	owned, err := s.d.Accounts.ListByOwner(ctx, tx, uid)
	if err != nil {
		return out, err
	}
	for _, acct := range owned {
		if acct.Status == accounts.StatusClosed {
			continue
		}
		if _, err := s.d.Accounts.Transition(ctx, tx, acct.ID, accounts.StatusChange{
			To: accounts.StatusClosed, ActorType: string(op.ActorType), ActorID: op.UserID,
			Reason: reason, CorrelationID: op.CorrelationID,
		}, now); err != nil {
			return out, err
		}
		out.accounts++
	}
	// Ending the sessions is part of closing, not a courtesy: a closed account
	// whose browser tab still works is a closed account only on paper.
	n, err := s.d.Sessions.RevokeAllForSubject(ctx, tx, userID)
	if err != nil {
		return out, fmt.Errorf("profile: revoke sessions: %w", err)
	}
	out.sessions = n
	return out, nil
}

// AdminUserView is the operator support view of one user (PART 38).
//
// It is a READ. There is no field on it an operator can write, and the only
// mutation this package offers an operator is deciding a closure request the
// user themselves opened. PART 38 is explicit that admin surfaces must not
// provide unauthorized direct financial mutation, and the way to guarantee that
// is not to build one.
type AdminUserView struct {
	UserID     string
	UserStatus string
	IdPIssuer  string
	IdPSubject string
	// EmailVerified reports that the identity provider asserted a verified
	// address. The address itself is not here and cannot be got from here: it is
	// sealed in identity_pii and ADR-0021 decides who may open it.
	EmailVerified bool
	CreatedAt     time.Time

	Profile      *Profile
	Accounts     []accounts.Account
	Restrictions []Restriction
	Closure      *ClosureRequest
	// Blockers is what this person's accounts still hold: a Credit balance, a
	// payout request that has not reached a terminal state, an open native
	// position. Decide refuses EFFECT while any of them stands, and this is the
	// same read, so the surface the operator decides from cannot disagree with
	// the check that refuses the decision (F-179).
	Blockers    ClosureBlockers
	Acceptances []Acceptance
	// ActiveSessions is how many live sessions the user holds right now.
	ActiveSessions int
	// AuditStream names where this user's history is, so a support view links
	// to the evidence rather than restating it.
	AuditStream string
	// Verification is the level Nodal has established for the user's first
	// account. Known is false when this deployment wired no resolver, which is
	// reported as "not known here" and never as NONE.
	Verification      string
	VerificationKnown bool
}

// VerificationResolver reports a verification level for an account. It is a
// function rather than an interface so this package does not depend on the
// verification domain; cmd/api supplies internal/identity's resolver today.
type VerificationResolver func(ctx context.Context, accountID accounts.AccountID) (string, error)

// AdminUser returns the support view of one user.
func (s *Service) AdminUser(ctx context.Context, userID string) (AdminUserView, error) {
	if userID == "" {
		return AdminUserView{}, errs.New(errs.CodeValidationFailed, "a user id is required")
	}
	return s.adminUserView(ctx, s.d.DB, userID)
}

func (s *Service) adminUserView(ctx context.Context, q db.Querier, userID string) (AdminUserView, error) {
	var v AdminUserView
	v.UserID = userID
	err := q.QueryRow(ctx, `SELECT idp_issuer, idp_subject, status, (email_hash IS NOT NULL), created_at
		FROM users WHERE id = $1`, userID).
		Scan(&v.IdPIssuer, &v.IdPSubject, &v.UserStatus, &v.EmailVerified, &v.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AdminUserView{}, errs.New(errs.CodeNotFound, "no such user")
		}
		return AdminUserView{}, fmt.Errorf("profile: admin user: %w", err)
	}
	if p, err := s.d.Repo.Get(ctx, q, userID); err == nil {
		v.Profile = &p
	} else if errs.CodeOf(err) != errs.CodeNotFound {
		return AdminUserView{}, err
	}
	acct, err := s.accountView(ctx, q, userID)
	if err != nil {
		return AdminUserView{}, err
	}
	v.Accounts, v.Restrictions, v.Closure = acct.Accounts, acct.Restrictions, acct.Closure
	if v.Blockers, err = s.d.Repo.ClosureBlockers(ctx, q, userID); err != nil {
		return AdminUserView{}, err
	}
	if v.Acceptances, err = s.d.Repo.Acceptances(ctx, q, userID); err != nil {
		return AdminUserView{}, err
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`, userID).Scan(&v.ActiveSessions); err != nil {
		return AdminUserView{}, fmt.Errorf("profile: session count: %w", err)
	}
	if v.AuditStream, err = s.streamFor(ctx, q, userID); err != nil {
		return AdminUserView{}, err
	}
	if s.d.Verification != nil && len(v.Accounts) > 0 {
		level, err := s.d.Verification(ctx, v.Accounts[0].ID)
		if err != nil {
			return AdminUserView{}, err
		}
		v.Verification, v.VerificationKnown = level, true
	}
	return v, nil
}
