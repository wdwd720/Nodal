package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/pii"
	"github.com/nodal/controlplane/internal/security"
)

// DefaultAttemptTTL bounds how long a login redirect stays redeemable.
const DefaultAttemptTTL = 10 * time.Minute

// Deps are the collaborators of the login service.
type Deps struct {
	IdP        auth.IdentityProvider
	DB         *db.DB
	Accounts   *accounts.Repository
	Sessions   *auth.Manager
	Audit      audit.Writer
	Clock      clock.Clock
	AttemptTTL time.Duration
	// AdmitAccount is asked before a first login provisions an account, and
	// refusing is how the launch-cohort ceiling is enforced. It is a function
	// rather than a capacity.Guard so this package keeps no dependency on the
	// ceiling's implementation.
	//
	// Nil admits, which is right for a deployment that declares no ceilings --
	// and is why cmd/api always supplies it and TestIdentityIsGivenTheAccount
	// Ceiling asserts that it does. An optional control is only safe when
	// something checks that it was not accidentally left out.
	AdmitAccount func(ctx context.Context, tx pgx.Tx) error
	// PII keeps the verified e-mail address, encrypted, beside the hash that
	// finds it (F-47). Nil keeps nothing, which is the LOCAL/TEST default --
	// and, as with AdmitAccount, cmd/api always supplies one and
	// TestIdentityIsGivenThePIIStore asserts that it does.
	PII *pii.Store
	// Operators writes the operator-directory rows a deployment declared for
	// this identity, before the directory is read. It is how a deployment with
	// no operator gets its first one (ADR-0024, D-056); nil declares none.
	//
	// This is NOT taking a role from a provider claim, which this package must
	// never do: the declaration comes from the deployment's own configuration
	// and lands in operator_roles, which stays the only thing the role decision
	// below reads.
	Operators *OperatorBootstrap
}

// Service implements login/logout.
type Service struct{ d Deps }

// New validates the dependencies and returns a Service.
func New(d Deps) (*Service, error) {
	switch {
	case d.IdP == nil, d.DB == nil, d.Accounts == nil, d.Sessions == nil, d.Audit == nil, d.Clock == nil:
		return nil, errors.New("identity: missing dependency")
	}
	if d.AttemptTTL <= 0 {
		d.AttemptTTL = DefaultAttemptTTL
	}
	return &Service{d: d}, nil
}

// BeginRequest describes a login start.
type BeginRequest struct {
	StepUp    bool
	ReturnTo  string // relative path the UI wants to return to; validated to be a local path
	IP        string
	UserAgent string
}

// BeginResult is what the API redirects the browser to.
type BeginResult struct {
	RedirectURL string
	State       string
	ExpiresAt   time.Time
}

// Begin creates a single-use login attempt and returns the provider redirect.
func (s *Service) Begin(ctx context.Context, req BeginRequest) (BeginResult, error) {
	if req.ReturnTo != "" && (!strings.HasPrefix(req.ReturnTo, "/") || strings.HasPrefix(req.ReturnTo, "//")) {
		return BeginResult{}, errs.New(errs.CodeValidationFailed, "return_to must be a local path")
	}
	state, err := randomToken(32)
	if err != nil {
		return BeginResult{}, err
	}
	nonce, err := randomToken(32)
	if err != nil {
		return BeginResult{}, err
	}
	verifier, err := randomToken(64) // 86 chars, within RFC 7636's 43..128
	if err != nil {
		return BeginResult{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	now := s.d.Clock.Now()
	expires := now.Add(s.d.AttemptTTL)
	if _, err := s.d.DB.Pool().Exec(ctx, `INSERT INTO login_attempts (state, nonce, code_verifier, step_up, return_to, ip, user_agent, created_at, expires_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,NULLIF($7,''),$8,$9)`,
		state, nonce, verifier, req.StepUp, req.ReturnTo, parseIP(req.IP), req.UserAgent, now, expires); err != nil {
		return BeginResult{}, fmt.Errorf("identity: record login attempt: %w", err)
	}
	return BeginResult{RedirectURL: s.d.IdP.AuthCodeURL(state, nonce, challenge, req.StepUp), State: state, ExpiresAt: expires}, nil
}

// CompleteRequest is the provider callback.
type CompleteRequest struct {
	Code      string
	State     string
	IP        string
	UserAgent string
	RequestID string
	// Current is the session the browser already holds, when it holds one.
	// The callback is a public route and the session middleware attaches
	// whatever the cookie resolved to, so this is present for a step-up
	// started from inside the product and absent for a cold sign-in.
	//
	// It is a session and not a subject id on purpose: Rotate re-reads it
	// from the store inside the transaction and refuses a stale copy, and a
	// caller that could only name a subject could ask for somebody else's
	// sessions to be replaced.
	Current *auth.Session
}

// Completed is a successful login.
type Completed struct {
	Issued   auth.Issued
	User     accounts.User
	Accounts []accounts.Account
	Created  bool // first login: user and first account were created
	StepUp   bool
	ReturnTo string
}

type attempt struct {
	nonce, verifier, returnTo string
	stepUp                    bool
}

// Complete consumes the login attempt, exchanges the code, maps the identity
// to a user, decides roles from the operator directory, and issues a session.
func (s *Service) Complete(ctx context.Context, req CompleteRequest) (Completed, error) {
	if req.Code == "" || req.State == "" {
		return Completed{}, errs.New(errs.CodeValidationFailed, "code and state required")
	}
	now := s.d.Clock.Now()

	// 1. Claim the attempt exactly once.
	var at attempt
	err := s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var consumedAt *time.Time
		var expiresAt time.Time
		var returnTo *string
		err := tx.QueryRow(ctx, `SELECT nonce, code_verifier, step_up, return_to, expires_at, consumed_at FROM login_attempts WHERE state = $1 FOR UPDATE`, req.State).
			Scan(&at.nonce, &at.verifier, &at.stepUp, &returnTo, &expiresAt, &consumedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errs.New(errs.CodeUnauthenticated, "unknown login state")
			}
			return err
		}
		if consumedAt != nil {
			return errs.New(errs.CodeUnauthenticated, "login state already used")
		}
		if !now.Before(expiresAt) {
			return errs.New(errs.CodeUnauthenticated, "login attempt expired")
		}
		if returnTo != nil {
			at.returnTo = *returnTo
		}
		_, err = tx.Exec(ctx, `UPDATE login_attempts SET consumed_at = $2 WHERE state = $1`, req.State, now)
		return err
	})
	if err != nil {
		s.recordSecurityEvent(ctx, "login_failed", "WARN", nil, nil, req, map[string]any{"reason": errs.CodeOf(err)})
		return Completed{}, err
	}

	fail := func(cause error, reason string) (Completed, error) {
		_, _ = s.d.DB.Pool().Exec(ctx, `UPDATE login_attempts SET outcome = 'FAILED' WHERE state = $1`, req.State)
		s.recordSecurityEvent(ctx, "login_failed", "WARN", nil, nil, req, map[string]any{"reason": reason})
		return Completed{}, cause
	}

	// 2. Exchange the code (network call outside any transaction).
	ident, err := s.d.IdP.Exchange(ctx, req.Code, at.verifier, at.nonce)
	if err != nil {
		return fail(errs.Wrap(err, errs.CodeUnauthenticated, "identity provider rejected the login"), "exchange_failed")
	}
	if err := ident.Validate(); err != nil {
		return fail(errs.Wrap(err, errs.CodeUnauthenticated, "invalid identity"), "invalid_identity")
	}
	if at.stepUp && !security.HasStrongAMR(ident.AMR) {
		return fail(auth.ErrStepUpNotSatisfied, "step_up_not_satisfied")
	}

	// 3. Map to a user, decide roles, issue the session.
	var out Completed
	err = s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		issuer := s.d.IdP.Name()
		user, err := s.d.Accounts.GetUserBySubject(ctx, tx, issuer, ident.Subject)
		created := false
		switch {
		case errs.CodeOf(err) == errs.CodeNotFound:
			var emailHash []byte
			if ident.EmailVerified && ident.Email != "" {
				h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(ident.Email))))
				emailHash = h[:]
			}
			if user, err = s.d.Accounts.CreateUser(ctx, tx, issuer, ident.Subject, emailHash); err != nil {
				return err
			}
			// The launch cohort is a ceiling on how many accounts exist, and
			// this is the only place one is created for a person. It is asked
			// before the account rather than after, so a refusal leaves no
			// half-provisioned user (F-91).
			if s.d.AdmitAccount != nil {
				if err := s.d.AdmitAccount(ctx, tx); err != nil {
					return err
				}
			}
			if _, err := s.d.Accounts.CreateAccount(ctx, tx, user.ID, accounts.KindCustomer); err != nil {
				return err
			}
			created = true
		case err != nil:
			return err
		case user.EmailHash == nil && ident.EmailVerified && ident.Email != "":
			// The assertion arrived later than the user did. email_hash was
			// only ever written at creation, so an account created before its
			// email was verified could never reach NODAL_IDENTITY afterwards,
			// however many times the provider asserted it since.
			//
			// It is written once and never rewritten: this fills an absence,
			// it does not follow a changing address, and a changed address is
			// a different question with different consequences.
			h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(ident.Email))))
			if err := s.d.Accounts.SetEmailHash(ctx, tx, user.ID, h[:]); err != nil {
				return err
			}
			user.EmailHash = h[:]
		}
		if user.Status != "ACTIVE" {
			return errs.New(errs.CodeForbidden, "user is not active").WithField("status", user.Status)
		}
		// The address itself, encrypted, for the deployments that keep one.
		// Filling an absence on every login rather than only at creation, for
		// the same reason email_hash learned to: a user created before this
		// existed, or before the provider asserted the address, would
		// otherwise never have it. A row that has one is left alone.
		if err := s.storeEmail(ctx, tx, user.ID, ident); err != nil {
			return err
		}
		owned, err := s.d.Accounts.ListByOwner(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		accountIDs := make([]string, 0, len(owned))
		for _, a := range owned {
			if a.Status != accounts.StatusClosed {
				accountIDs = append(accountIDs, a.ID.String())
			}
		}
		// A declared bootstrap grant is written before the directory is read,
		// so the session that first carries the role and the row that grants it
		// commit together. It is idempotent and it never revives a revoked row.
		var bootstrapped []security.Role
		if s.d.Operators != nil {
			if bootstrapped, err = s.d.Operators.Ensure(ctx, tx, issuer, ident.Subject, user.ID.String(), now); err != nil {
				return err
			}
		}
		roles, err := operatorRoles(ctx, tx, user.ID, now)
		if err != nil {
			return err
		}
		actor := security.ActorUser
		if len(roles) > 0 {
			actor = security.ActorOperator
		} else {
			roles = []security.Role{security.RoleCustomer}
		}
		issued, rotatedFrom, err := s.issueOrRotate(ctx, tx, req, auth.IssueParams{
			SubjectID: user.ID.String(), ActorType: actor, Roles: roles, AccountIDs: accountIDs,
			AuthTime: ident.AuthTime, AMR: ident.AMR, IP: req.IP, UserAgent: req.UserAgent,
		}, at.stepUp)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE login_attempts SET outcome = 'SUCCESS' WHERE state = $1`, req.State); err != nil {
			return err
		}
		stream := audit.SystemStream
		if len(accountIDs) > 0 {
			stream = audit.AccountStream(accountIDs[0])
		}
		payload, _ := json.Marshal(map[string]any{"session_id": issued.Session.ID, "actor_type": actor, "roles": roles, "amr": ident.AMR, "step_up": at.stepUp, "created": created, "bootstrapped_roles": bootstrapped, "rotated_from": rotatedFrom})
		if _, err := s.d.Audit.Append(ctx, tx, audit.Event{
			Stream: stream, ActorType: string(actor), ActorID: user.ID.String(), Action: "auth.login",
			ResourceType: "session", ResourceID: issued.Session.ID, RequestID: req.RequestID, SourceIP: req.IP, Device: req.UserAgent,
			Payload: payload, OccurredAt: now,
		}); err != nil {
			return err
		}
		if err := insertSecurityEvent(ctx, tx, "login", "INFO", &user.ID, &issued.Session.ID, req,
			map[string]any{"actor_type": actor, "step_up": at.stepUp, "created": created, "rotated_from": rotatedFrom}, now); err != nil {
			return err
		}
		if rotatedFrom != "" {
			// The replaced session is revoked, and a revocation the user did
			// not ask for belongs on the security page beside the ones they
			// did.
			prior := rotatedFrom
			if err := insertSecurityEvent(ctx, tx, "session_revoked", "INFO", &user.ID, &prior, req,
				map[string]any{"by": "step_up_rotation", "replaced_by": issued.Session.ID}, now); err != nil {
				return err
			}
		}
		out = Completed{Issued: issued, User: user, Accounts: owned, Created: created, StepUp: at.stepUp, ReturnTo: at.returnTo}
		return nil
	})
	if err != nil {
		return fail(err, "session_issue_failed")
	}
	return out, nil
}

// issueOrRotate creates the session a completed login hands to the browser, and
// returns the id of the session it replaced when it replaced one.
//
// PART 192 requires session rotation on privilege change, and a step-up IS a
// privilege change: it raises the session's AuthTime and AMR, and it is what a
// person is asked to do before closing their account or registering a payout
// destination. auth.Manager.Rotate is the mechanism for it and had no caller
// anywhere in the repository -- identity.Complete always called Issue -- so the
// pre-step-up session stayed live, kept its own full absolute lifetime, and was
// still a usable credential carrying the WEAKER authentication. A stolen cookie
// survived the step-up the product asked for to defend against it, and the
// user's own security page counted one browser as two devices (F-177).
//
// Rotate is chosen only when the replacement really is the same login moving
// forward: the same subject, the same actor type, and a session the store still
// accepts. It revokes the old session and keeps its absolute ExpiresAt, so a
// rotation can never extend a login -- which is why a cold step-up, where there
// is nothing to rotate from, still Issues.
//
// An actor type that has changed between the two logins means the directory
// decided something different about this person in between; that is not this
// session moving forward, so it Issues a new one and revokes the old rather than
// carrying an OPERATOR actor type onto a principal the directory no longer names.
func (s *Service) issueOrRotate(ctx context.Context, tx pgx.Tx, req CompleteRequest, p auth.IssueParams, stepUp bool) (auth.Issued, string, error) {
	cur := req.Current
	if !stepUp || cur == nil || cur.SubjectID != p.SubjectID || cur.ActorType == security.ActorAgent {
		issued, err := s.d.Sessions.Issue(ctx, tx, p)
		return issued, "", err
	}
	if cur.ActorType != p.ActorType {
		issued, err := s.d.Sessions.Issue(ctx, tx, p)
		if err != nil {
			return auth.Issued{}, "", err
		}
		if err := s.d.Sessions.Revoke(ctx, tx, cur.ID); err != nil {
			return auth.Issued{}, "", err
		}
		return issued, cur.ID, nil
	}
	r := auth.Rotation{
		Roles: append([]security.Role(nil), p.Roles...), AccountIDs: p.AccountIDs,
		AuthTime: p.AuthTime, AMR: p.AMR,
	}
	// A live break-glass elevation survives the step-up, with its role, because
	// it is bounded by the clock rather than by the session and ending an
	// emergency because somebody re-authenticated more strongly would be the
	// wrong way round. Both halves move together: security.Principal refuses
	// the role without the expiry.
	if cur.BreakGlassUntil != nil {
		r.BreakGlassUntil = cur.BreakGlassUntil
		for _, role := range cur.Roles {
			if role == security.RoleBreakGlass {
				r.Roles = append(r.Roles, security.RoleBreakGlass)
				break
			}
		}
	}
	issued, err := s.d.Sessions.Rotate(ctx, tx, *cur, r)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidSession) {
			// The cookie the browser sent is no longer usable -- revoked
			// elsewhere, expired, or idled out between the redirect and the
			// callback. There is nothing to rotate from, so this is a cold
			// step-up and nothing is left live by treating it as one.
			issued, ierr := s.d.Sessions.Issue(ctx, tx, p)
			return issued, "", ierr
		}
		return auth.Issued{}, "", err
	}
	return issued, cur.ID, nil
}

// Logout revokes the session and records the security event.
func (s *Service) Logout(ctx context.Context, sess auth.Session, req CompleteRequest) error {
	now := s.d.Clock.Now()
	return s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.d.Sessions.Revoke(ctx, tx, sess.ID); err != nil {
			return err
		}
		uid, err := accounts.ParseUserID(sess.SubjectID)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "session subject is not a user id")
		}
		if _, err := s.d.Audit.Append(ctx, tx, audit.Event{
			Stream: audit.SystemStream, ActorType: string(sess.ActorType), ActorID: sess.SubjectID, Action: "auth.logout",
			ResourceType: "session", ResourceID: sess.ID, RequestID: req.RequestID, SourceIP: req.IP, Device: req.UserAgent, OccurredAt: now,
		}); err != nil {
			return err
		}
		return insertSecurityEvent(ctx, tx, "session_revoked", "INFO", &uid, &sess.ID, req, map[string]any{"by": "logout"}, now)
	})
}

// operatorRoles returns active, unexpired operator roles for the user.
func operatorRoles(ctx context.Context, q db.Querier, userID accounts.UserID, now time.Time) ([]security.Role, error) {
	rows, err := q.Query(ctx, `SELECT role FROM operator_roles WHERE user_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2) ORDER BY role`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("identity: operator roles: %w", err)
	}
	defer rows.Close()
	var roles []security.Role
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		roles = append(roles, security.Role(r))
	}
	return roles, rows.Err()
}

func insertSecurityEvent(ctx context.Context, q db.Querier, kind, severity string, userID *accounts.UserID, sessionID *string, req CompleteRequest, detail map[string]any, now time.Time) error {
	b, _ := json.Marshal(detail)
	var sid any
	if sessionID != nil {
		sid = *sessionID
	}
	_, err := q.Exec(ctx, `INSERT INTO security_events (id, kind, severity, user_id, session_id, detail, ip, user_agent, request_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5::uuid,$6,$7,NULLIF($8,''),NULLIF($9,''),$10)`,
		id.New[id.Any](), kind, severity, userID, sid, b, parseIP(req.IP), req.UserAgent, req.RequestID, now)
	if err != nil {
		return fmt.Errorf("identity: security event: %w", err)
	}
	return nil
}

func (s *Service) recordSecurityEvent(ctx context.Context, kind, severity string, userID *accounts.UserID, sessionID *string, req CompleteRequest, detail map[string]any) {
	_ = insertSecurityEvent(ctx, s.d.DB.Pool(), kind, severity, userID, sessionID, req, detail, s.d.Clock.Now())
}

func parseIP(s string) *netip.Addr {
	if s == "" {
		return nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	return &a
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// storeEmail keeps the verified address, encrypted, when a PII store is
// configured. Only a verified address: an unverified claim is somebody's
// assertion about somebody else's mailbox, and the hash does not record it
// either.
func (s *Service) storeEmail(ctx context.Context, tx pgx.Tx, userID accounts.UserID, ident auth.Identity) error {
	if s.d.PII == nil || !ident.EmailVerified || ident.Email == "" {
		return nil
	}
	return s.d.PII.EnsureEmail(ctx, tx, userID.String(), strings.TrimSpace(ident.Email))
}
