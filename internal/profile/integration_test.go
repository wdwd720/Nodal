//go:build integration

package profile_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/terms"
)

const testCoolingOff = 10 * time.Minute

type fixture struct {
	svc      *profile.Service
	db       *db.DB
	clk      *clock.Fake
	repo     *accounts.Repository
	sessions *auth.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 4, MinConns: 1, AppName: "profile-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	mgr, err := auth.NewManager(pgstore.New(), auth.ManagerConfig{Now: clk.Now})
	require.NoError(t, err)
	repo := accounts.NewRepository()
	svc, err := profile.New(profile.Deps{
		DB: d, Repo: profile.NewRepository(), Accounts: repo, Audit: audit.NewWriter(),
		Clock: clk, Sessions: mgr, CoolingOff: testCoolingOff,
	})
	require.NoError(t, err)
	return &fixture{svc: svc, db: d, clk: clk, repo: repo, sessions: mgr}
}

// newUser creates a user, their CUSTOMER account and one live session, the way
// a first login does, and returns the actor a handler would build.
func (f *fixture) newUser(t *testing.T) (profile.Actor, accounts.User, accounts.Account) {
	t.Helper()
	ctx := context.Background()
	var (
		user   accounts.User
		acct   accounts.Account
		issued auth.Issued
	)
	subject := "sub-" + id.New[id.Any]().String()
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if user, err = f.repo.CreateUser(ctx, tx, "profile-test", subject, nil); err != nil {
			return err
		}
		if acct, err = f.repo.CreateAccount(ctx, tx, user.ID, accounts.KindCustomer); err != nil {
			return err
		}
		issued, err = f.sessions.Issue(ctx, tx, auth.IssueParams{
			SubjectID: user.ID.String(), ActorType: security.ActorUser,
			Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{acct.ID.String()},
			AuthTime: f.clk.Now(), AMR: []string{"pwd", "mfa"},
		})
		return err
	}))
	return profile.Actor{
		UserID: user.ID.String(), ActorType: security.ActorUser,
		SessionID: issued.Session.ID, RequestID: "req-" + subject, IP: "203.0.113.7", UserAgent: "ua",
	}, user, acct
}

func (f *fixture) operator(t *testing.T) profile.Actor {
	t.Helper()
	a, _, _ := f.newUser(t)
	a.ActorType = security.ActorOperator
	return a
}

func (f *fixture) acceptEverythingRequiredAtOnboarding(t *testing.T, a profile.Actor) profile.TermsView {
	t.Helper()
	var ids []terms.DocumentID
	for _, d := range terms.RequiredAt(terms.AtOnboarding) {
		ids = append(ids, d.ID)
	}
	view, err := f.svc.Accept(context.Background(), a, ids)
	require.NoError(t, err)
	return view
}

func (f *fixture) countAudit(t *testing.T, action, resourceID string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = $1 AND resource_id = $2`, action, resourceID).Scan(&n))
	return n
}

// ------------------------------------------------------------------ profile

func TestIntegration_Profile_IsCreatedLazilyAndOnlyOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)

	first, err := f.svc.Me(ctx, actor)
	require.NoError(t, err)
	assert.Equal(t, user.ID.String(), first.Profile.UserID)
	assert.Equal(t, profile.DefaultLocale, first.Profile.Locale)
	assert.Equal(t, profile.DefaultTimeZone, first.Profile.TimeZone)
	assert.Equal(t, profile.AvatarSeedFor(user.ID.String()), first.Profile.AvatarSeed)
	assert.Empty(t, first.Profile.DisplayName)
	assert.False(t, first.Onboarding.Complete())
	assert.Equal(t, profile.StepProfile, first.Onboarding.NextStep())

	// Creation is the product's signup moment, and it is audited exactly once.
	assert.Equal(t, 1, f.countAudit(t, profile.ActionProfileCreated, user.ID.String()))

	second, err := f.svc.Me(ctx, actor)
	require.NoError(t, err)
	assert.Equal(t, first.Profile.CreatedAt, second.Profile.CreatedAt)
	assert.Equal(t, 1, f.countAudit(t, profile.ActionProfileCreated, user.ID.String()),
		"a second read wrote a second signup event")
}

func TestIntegration_Profile_UpdateStampsTheStepOnceAndIsAudited(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)

	name, handle, tz := "Ada Lovelace", "AdaL", "Europe/London"
	p, err := f.svc.Update(ctx, actor, profile.Patch{DisplayName: &name, Handle: &handle, TimeZone: &tz})
	require.NoError(t, err)
	assert.Equal(t, "Ada Lovelace", p.DisplayName)
	assert.Equal(t, "adal", p.Handle, "the handle is stored lower case")
	assert.Equal(t, "Europe/London", p.TimeZone)
	require.NotNil(t, p.Onboarding.DisplayNameSetAt)
	firstStamp := *p.Onboarding.DisplayNameSetAt
	assert.Equal(t, 1, f.countAudit(t, profile.ActionProfileUpdated, user.ID.String()))

	// Changing the name later does not move the record of when a name was first
	// chosen: the trigger in 00756 refuses a restamp and the statement never
	// tries one.
	f.clk.Advance(time.Hour)
	renamed := "Ada L"
	p, err = f.svc.Update(ctx, actor, profile.Patch{DisplayName: &renamed})
	require.NoError(t, err)
	require.NotNil(t, p.Onboarding.DisplayNameSetAt)
	assert.Equal(t, firstStamp, *p.Onboarding.DisplayNameSetAt)
	assert.Equal(t, "Ada L", p.DisplayName)

	// Clearing the handle frees it; the display-name stamp is untouched.
	empty := ""
	p, err = f.svc.Update(ctx, actor, profile.Patch{Handle: &empty})
	require.NoError(t, err)
	assert.Empty(t, p.Handle)

	// A request that changes nothing is a validation failure rather than a
	// silent no-op that writes an audit event.
	_, err = f.svc.Update(ctx, actor, profile.Patch{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestIntegration_Profile_AHandleIsGloballyUnique(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, _, _ := f.newUser(t)
	second, _, _ := f.newUser(t)

	handle := "ada" + id.New[id.Any]().String()[:8]
	_, err := f.svc.Update(ctx, first, profile.Patch{Handle: &handle})
	require.NoError(t, err)

	// The same handle in a different case is the same handle.
	upper := "ADA" + handle[3:]
	_, err = f.svc.Update(ctx, second, profile.Patch{Handle: &upper})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
}

// A profile is product state and nothing else: the table has no column for the
// personal data identity_pii holds, asserted against the schema rather than
// against a comment.
func TestIntegration_Profile_HasNoPersonalDataColumn(t *testing.T) {
	f := newFixture(t)
	rows, err := f.db.Pool().Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'user_profiles'`)
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, cols)
	for _, forbidden := range []string{"email", "phone", "legal_name", "dob", "date_of_birth", "address", "ssn", "country_code"} {
		assert.NotContainsf(t, cols, forbidden,
			"user_profiles gained a %q column; personal data belongs in identity_pii, sealed (ADR-0021)", forbidden)
	}
}

// F-42: the onboarding stamps are not the application's to rewrite.
func TestIntegration_Profile_AStepCannotBeRestampedOrBornFinished(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)
	_, err := f.svc.Me(ctx, actor)
	require.NoError(t, err)

	name := "Ada"
	_, err = f.svc.Update(ctx, actor, profile.Patch{DisplayName: &name})
	require.NoError(t, err)

	_, err = f.db.Pool().Exec(ctx,
		`UPDATE user_profiles SET display_name_set_at = now() + interval '1 day' WHERE user_id = $1`, user.ID)
	require.Error(t, err)
	assert.Equal(t, "AD001", db.SQLState(err))
	assert.Contains(t, err.Error(), "PROFILE_STEP_RESTAMPED")

	// Clearing a stamp is a restamp too.
	_, err = f.db.Pool().Exec(ctx,
		`UPDATE user_profiles SET display_name_set_at = NULL WHERE user_id = $1`, user.ID)
	require.Error(t, err)
	assert.Equal(t, "AD001", db.SQLState(err))

	// And nothing is born finished.
	other, _, _ := f.newUser(t)
	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO user_profiles (user_id, avatar_seed, onboarding_completed_at) VALUES ($1, '0123456789abcdef', now())`,
		other.UserID)
	require.Error(t, err)
	assert.Equal(t, "AD001", db.SQLState(err))
	assert.Contains(t, err.Error(), "PROFILE_BORN_ONBOARDED")
}

// -------------------------------------------------------------------- terms

func TestIntegration_Terms_AcceptanceCompletesOnboardingAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)

	before, err := f.svc.Terms(ctx, actor)
	require.NoError(t, err)
	assert.Len(t, before.Outstanding, len(terms.RequiredAt(terms.AtOnboarding)))

	name := "Ada"
	_, err = f.svc.Update(ctx, actor, profile.Patch{DisplayName: &name})
	require.NoError(t, err)

	after := f.acceptEverythingRequiredAtOnboarding(t, actor)
	assert.Empty(t, after.Outstanding)
	for _, d := range after.Documents {
		if d.Document.Requirement == terms.AtOnboarding {
			assert.Truef(t, d.Accepted, "%s", d.Document.ID)
			require.NotNil(t, d.AcceptedAt)
		}
	}

	me, err := f.svc.Me(ctx, actor)
	require.NoError(t, err)
	require.NotNil(t, me.Onboarding.TermsAcceptedAt)
	assert.True(t, me.Onboarding.Complete(), "both steps are done and onboarding is not complete")
	assert.Equal(t, 1, f.countAudit(t, profile.ActionOnboardingDone, user.ID.String()))

	// Accepting the same bytes again is one fact, not two: no new row and no
	// new audit event.
	var rows int
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM terms_acceptances WHERE user_id = $1`, user.ID).Scan(&rows))
	f.acceptEverythingRequiredAtOnboarding(t, actor)
	var again int
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM terms_acceptances WHERE user_id = $1`, user.ID).Scan(&again))
	assert.Equal(t, rows, again)

	// The acceptance names the version and the exact bytes, and says which
	// session it came from.
	var version, hash, actorType string
	var sessionID *string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT version, content_hash, actor_type, session_id::text FROM terms_acceptances
		 WHERE user_id = $1 AND document_id = 'TERMS_OF_SERVICE'`, user.ID).
		Scan(&version, &hash, &actorType, &sessionID))
	doc, ok := terms.Get(terms.TermsOfService)
	require.True(t, ok)
	assert.Equal(t, doc.Version, version)
	assert.Equal(t, doc.ContentHash, hash)
	assert.Equal(t, string(security.ActorUser), actorType)
	require.NotNil(t, sessionID)
	assert.Equal(t, actor.SessionID, *sessionID)
}

// An acceptance is evidence, and evidence is append-only.
func TestIntegration_Terms_AnAcceptanceCannotBeEditedOrDeleted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)
	f.acceptEverythingRequiredAtOnboarding(t, actor)

	_, err := f.db.Pool().Exec(ctx, `UPDATE terms_acceptances SET version = 'forged' WHERE user_id = $1`, user.ID)
	require.Error(t, err)
	// cp_app holds SELECT and INSERT and nothing else, so the refusal arrives as
	// a privilege error before the append-only trigger is even reached. Both are
	// accepted: what matters is that the row cannot be rewritten, not which of
	// the two controls said so first.
	assert.Contains(t, []string{db.SQLStateRaiseException, db.SQLStateInsufficientPrivilege},
		db.SQLState(err), "got %v", err)

	// cp_app holds no DELETE on any table, which is the stronger statement.
	_, err = f.db.Pool().Exec(ctx, `DELETE FROM terms_acceptances WHERE user_id = $1`, user.ID)
	require.Error(t, err)
}

// D-053: a document whose bytes changed without its version being bumped is a
// document nobody has agreed to, and the API asks again rather than treating a
// record of different text as consent.
func TestIntegration_Terms_AStaleHashIsOutstandingAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)

	// The only acceptance this user has names the current version and bytes the
	// registry no longer serves.
	_, err := f.db.Pool().Exec(ctx,
		`INSERT INTO terms_acceptances (id, user_id, document_id, version, content_hash, actor_type, actor_id)
		 VALUES ($1, $2, 'TERMS_OF_SERVICE', $3, repeat('a', 64), 'USER', $4)`,
		id.New[id.Any](), user.ID, terms.Version, user.ID.String())
	require.NoError(t, err)

	view, err := f.svc.Terms(ctx, actor)
	require.NoError(t, err)
	assert.Contains(t, view.Outstanding, terms.TermsOfService,
		"an acceptance of different bytes counted as consent to what is served now")
	for _, d := range view.Documents {
		if d.Document.ID == terms.TermsOfService {
			assert.False(t, d.Accepted)
		}
	}
	// The row itself is still there: it is a record of something that happened.
	var rows int
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM terms_acceptances WHERE user_id = $1`, user.ID).Scan(&rows))
	assert.Equal(t, 1, rows)

	// Accepting properly clears it, and the stale row stays beside the new one.
	f.acceptEverythingRequiredAtOnboarding(t, actor)
	view, err = f.svc.Terms(ctx, actor)
	require.NoError(t, err)
	assert.Empty(t, view.Outstanding)
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM terms_acceptances WHERE user_id = $1 AND document_id = 'TERMS_OF_SERVICE'`,
		user.ID).Scan(&rows))
	assert.Equal(t, 2, rows, "the two acceptances are two facts and both are kept")
}

// ---------------------------------------------------------------- lifecycle

func TestIntegration_Closure_RequestWaitsAndCanBeCancelled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)

	view, err := f.svc.RequestClosure(ctx, actor, "  too many emails  ")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)
	assert.Equal(t, profile.ClosurePending, view.Closure.State)
	assert.Equal(t, f.clk.Now().Add(testCoolingOff), view.Closure.CoolingOffUntil.UTC())
	assert.False(t, view.Closure.Effectable(f.clk.Now()))
	assert.Equal(t, "too many emails", view.Closure.RequestedReason)
	assert.Equal(t, 1, f.countAudit(t, profile.ActionClosureRequested, view.Closure.ID))

	// The user is told the request is open, and that everything still works.
	var codes []string
	for _, r := range view.Restrictions {
		codes = append(codes, r.Code)
	}
	assert.Contains(t, codes, profile.RestrictionClosurePending)

	// A second request while one is open is a conflict, not a second clock.
	_, err = f.svc.RequestClosure(ctx, actor, "")
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	// The user can stop it, with no step-up and no operator.
	cancelled, err := f.svc.CancelClosure(ctx, actor)
	require.NoError(t, err)
	require.NotNil(t, cancelled.Closure, "the decided request must stay visible to the person it is about")
	assert.Equal(t, profile.ClosureCancelled, cancelled.Closure.State)
	assert.Empty(t, cancelled.Restrictions, "a cancelled request still restricted something")
	assert.Equal(t, 1, f.countAudit(t, profile.ActionClosureCancelled, view.Closure.ID))

	// Cancelling twice is a NOT_FOUND rather than a second cancellation.
	_, err = f.svc.CancelClosure(ctx, actor)
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	// The user is still ACTIVE: a closure request closes nothing.
	var status string
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, user.ID).Scan(&status))
	assert.Equal(t, "ACTIVE", status)

	// A new request after a cancellation starts a fresh clock.
	f.clk.Advance(time.Minute)
	again, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, again.Closure)
	assert.Equal(t, f.clk.Now().Add(testCoolingOff), again.Closure.CoolingOffUntil.UTC())
}

func TestIntegration_Closure_CannotBeEffectedBeforeTheCoolingOffPeriod(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, _, _ := f.newUser(t)
	op := f.operator(t)

	view, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)

	_, err = f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the customer asked")
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// And the database refuses it too, so the wait survives a bug in code that
	// is in a hurry: the transition row is written directly, bypassing the
	// service entirely.
	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO account_closure_request_transitions
		 (id, request_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'PENDING', 'EFFECTED', 'OPERATOR', 'forged', 'in a hurry', $3)`,
		id.New[id.Any](), view.Closure.ID, f.clk.Now())
	require.Error(t, err)
	assert.Equal(t, "AD001", db.SQLState(err))
	assert.Contains(t, err.Error(), "CLOSURE_STILL_COOLING")
}

func TestIntegration_Closure_EffectingClosesTheUserTheAccountsAndTheSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, acct := f.newUser(t)
	op := f.operator(t)

	view, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)
	requestID := view.Closure.ID

	f.clk.Advance(testCoolingOff + time.Minute)
	admin, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.NoError(t, err)
	assert.Equal(t, "CLOSED", admin.UserStatus)
	require.NotNil(t, admin.Closure)
	assert.Equal(t, profile.ClosureEffected, admin.Closure.State)

	var userStatus, acctStatus string
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, user.ID).Scan(&userStatus))
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, acct.ID).Scan(&acctStatus))
	assert.Equal(t, "CLOSED", userStatus)
	assert.Equal(t, "CLOSED", acctStatus)

	// A closed account whose session still works is closed only on paper.
	var live int
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, user.ID).Scan(&live))
	assert.Zero(t, live)

	// Nothing was deleted: the user, the account and the transitions are all
	// still there, which is what ADR-0020 requires.
	var users, accountsRows, transitions int
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, user.ID).Scan(&users))
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT count(*) FROM accounts WHERE owner_user_id = $1`, user.ID).Scan(&accountsRows))
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT count(*) FROM user_status_transitions WHERE user_id = $1`, user.ID).Scan(&transitions))
	assert.Equal(t, 1, users)
	assert.Equal(t, 1, accountsRows)
	assert.Equal(t, 1, transitions)

	// The decision is on the admin stream, with the operator as the actor.
	var actorID string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT actor_id FROM audit_events WHERE action = $1 AND resource_id = $2`,
		profile.ActionClosureEffected, requestID).Scan(&actorID))
	assert.Equal(t, op.UserID, actorID)

	// A decided request cannot be decided again.
	_, err = f.svc.Decide(ctx, op, actor.UserID, profile.DecisionCancel, "changed my mind")
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestIntegration_Closure_RefusalCarriesItsReasonAndClosesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)

	admin, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionRefuse, "an unsettled payout is in flight")
	require.NoError(t, err)
	require.NotNil(t, admin.Closure)
	assert.Equal(t, profile.ClosureRefused, admin.Closure.State)
	assert.Equal(t, "an unsettled payout is in flight", admin.Closure.DecidedReason)

	var status string
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, user.ID).Scan(&status))
	assert.Equal(t, "ACTIVE", status)

	// The user can ask again, and the clock restarts.
	f.clk.Advance(time.Minute)
	view, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)
	assert.Equal(t, profile.ClosurePending, view.Closure.State)
}

// An operator is not allowed to decide a request of their own: it is the same
// distinct-principal rule the admin plane applies everywhere else.
func TestIntegration_Closure_AnOperatorCannotDecideTheirOwnRequest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, op, "")
	require.NoError(t, err)

	_, err = f.svc.Decide(ctx, op, op.UserID, profile.DecisionEffect, "closing my own account")
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.ErrorIs(t, err, security.ErrSelfApproval)
}

func TestIntegration_Closure_ARequiredReasonIsActuallyRequired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, _, _ := f.newUser(t)
	op := f.operator(t)
	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)

	_, err = f.svc.Decide(ctx, op, actor.UserID, profile.DecisionRefuse, "short")
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	_, err = f.svc.Decide(ctx, op, actor.UserID, profile.ClosureDecision("DELETE"), "a long enough reason")
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// F-42 on users.status: the column moves only through a transition row, and the
// row must describe the edge it licenses.
func TestIntegration_UserStatus_MovesOnlyThroughATransitionRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, user, _ := f.newUser(t)

	_, err := f.db.Pool().Exec(ctx, `UPDATE users SET status = 'SUSPENDED' WHERE id = $1`, user.ID)
	require.Error(t, err, "cp_app updated users.status directly")
	assert.Contains(t, []string{"AU001", db.SQLStateInsufficientPrivilege}, db.SQLState(err), "got %v", err)

	// A transition row describing a different edge does not license this one.
	err = f.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, terr := tx.Exec(ctx, `INSERT INTO user_status_transitions (id, user_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1, $2, 'SUSPENDED', 'CLOSED', 'OPERATOR', 'op', 'wrong edge')`, id.New[id.Any](), user.ID)
		return terr
	})
	require.Error(t, err, "a transition from a status the user was never in was accepted")

	// The right edge works, and writes the column.
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, terr := tx.Exec(ctx, `INSERT INTO user_status_transitions (id, user_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1, $2, 'ACTIVE', 'SUSPENDED', 'OPERATOR', 'op', 'compliance hold')`, id.New[id.Any](), user.ID)
		return terr
	}))
	var status string
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, user.ID).Scan(&status))
	assert.Equal(t, "SUSPENDED", status)

	// And a suspended user is told so, in words written for them.
	actor := profile.Actor{UserID: user.ID.String(), ActorType: security.ActorUser}
	view, err := f.svc.Account(ctx, actor)
	require.NoError(t, err)
	assert.Equal(t, "SUSPENDED", view.UserStatus)
	var codes []string
	for _, r := range view.Restrictions {
		codes = append(codes, r.Code)
	}
	assert.Contains(t, codes, profile.RestrictionUserSuspended)
}

// ------------------------------------------------------------- support view

func TestIntegration_AdminUserView_IsAReadWithNoPersonalData(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, acct := f.newUser(t)

	name := "Ada"
	_, err := f.svc.Update(ctx, actor, profile.Patch{DisplayName: &name})
	require.NoError(t, err)
	f.acceptEverythingRequiredAtOnboarding(t, actor)

	view, err := f.svc.AdminUser(ctx, user.ID.String())
	require.NoError(t, err)
	assert.Equal(t, user.ID.String(), view.UserID)
	assert.Equal(t, "ACTIVE", view.UserStatus)
	assert.Equal(t, "profile-test", view.IdPIssuer)
	assert.False(t, view.EmailVerified, "no verified address was asserted for this user")
	require.NotNil(t, view.Profile)
	assert.Equal(t, "Ada", view.Profile.DisplayName)
	require.Len(t, view.Accounts, 1)
	assert.Equal(t, acct.ID, view.Accounts[0].ID)
	assert.NotEmpty(t, view.Acceptances)
	assert.Equal(t, 1, view.ActiveSessions)
	assert.Equal(t, "account:"+acct.ID.String(), view.AuditStream)
	assert.False(t, view.VerificationKnown, "no resolver is wired in this fixture and a level was reported anyway")

	_, err = f.svc.AdminUser(ctx, id.New[id.Any]().String())
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

// An agent has no profile and may not act on one, in the domain as well as at
// the boundary.
func TestIntegration_AnAgentIsRefusedEverySurface(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, _, _ := f.newUser(t)
	actor.ActorType = security.ActorAgent

	name := "Ada"
	for _, call := range []func() error{
		func() error { _, err := f.svc.Me(ctx, actor); return err },
		func() error { _, err := f.svc.Update(ctx, actor, profile.Patch{DisplayName: &name}); return err },
		func() error { _, err := f.svc.Terms(ctx, actor); return err },
		func() error { _, err := f.svc.Accept(ctx, actor, []terms.DocumentID{terms.TermsOfService}); return err },
		func() error { _, err := f.svc.Account(ctx, actor); return err },
		func() error { _, err := f.svc.RequestClosure(ctx, actor, ""); return err },
		func() error { _, err := f.svc.CancelClosure(ctx, actor); return err },
	} {
		err := call()
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	}
}
