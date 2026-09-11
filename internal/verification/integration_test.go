//go:build integration

package verification_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider/verifysandbox"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "verification integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "verification-itest", MaxConns: 8}); err != nil {
		fmt.Fprintln(os.Stderr, "verification integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

// fixedClock is a Clock that does not move, so every timestamp a test writes is
// the one it chose. internal/clock deliberately exports no such helper: the
// system clock is the only one the production code has.
type fixedClock time.Time

func (f fixedClock) Now() time.Time { return time.Time(f).UTC() }

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

func inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return testDB.InTx(ctx, db.TxOptions{}, fn)
}

func newAccount(t *testing.T) (accounts.UserID, accounts.AccountID) {
	t.Helper()
	repo := accounts.NewRepository()
	var user accounts.User
	var acct accounts.Account
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if user, err = repo.CreateUser(ctx, tx, "https://idp.test", "sub-"+uuid.NewString(), nil); err != nil {
			return err
		}
		acct, err = repo.CreateAccount(ctx, tx, user.ID, accounts.KindCustomer)
		return err
	}))
	return user.ID, acct.ID
}

// newService builds the service the sandbox tier runs, on a fixed clock.
func newService(t *testing.T, sandboxTier bool, now func() time.Time) (*verification.Service, *verification.Repository) {
	t.Helper()
	reg := verification.NewRegistry(true)
	p, err := verifysandbox.New(config.EnvStaging, now)
	require.NoError(t, err)
	require.NoError(t, reg.Register(p))
	repo := verification.NewRepository()
	svc, err := verification.NewService(verification.Deps{
		Repo:        repo,
		Compliance:  compliance.NewRepository(audit.NewWriter()),
		Providers:   reg,
		Clock:       fixedClock(now()),
		Environment: "STAGING",
		SandboxTier: sandboxTier,
	})
	require.NoError(t, err)
	return svc, repo
}

func ensureProfile(t *testing.T, userID accounts.UserID) {
	t.Helper()
	repo := compliance.NewRepository(audit.NewWriter())
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Upsert(ctx, tx, compliance.Profile{
			UserID: userID, SanctionsState: compliance.SanctionsUnknown, Restrictions: []string{},
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "itest", Reason: "fixture"})
		return err
	}))
}

// F-42, on the table the product goal calls the financial verification state
// machine. Four properties, each probed rather than assumed.
func TestIntegration_TheVerificationStateIsNotTheApplicationsToWrite(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	userID, _ := newAccount(t)

	t.Run("a profile cannot be born verified", func(t *testing.T) {
		_, err := testDB.Exec(ctx,
			`INSERT INTO compliance_profiles (user_id, identity_state, sanctions_state)
			 VALUES ($1,'VERIFIED','CLEAR')`, userID)
		require.Error(t, err)
		assert.Equal(t, "AD001", db.SQLState(err))
		assert.Contains(t, err.Error(), "COMPLIANCE_PROFILE_BORN_VERIFIED")

		_, err = testDB.Exec(ctx,
			`INSERT INTO compliance_profiles (user_id, identity_state, sanctions_state, verified_at)
			 VALUES ($1,'UNVERIFIED','CLEAR', now())`, userID)
		require.Error(t, err)
		assert.Equal(t, "AD001", db.SQLState(err))
	})

	ensureProfile(t, userID)

	t.Run("the application role may not write the state columns", func(t *testing.T) {
		for _, set := range []string{
			`identity_state = 'VERIFIED'`,
			`verified_at = now()`,
			`expires_at = now()`,
			// The sanctions screen joined them in 00796. It is a screening
			// DECISION that internal/eligibility reads as a payout gate, and
			// 00761 left it in the attribute grant, where one UPDATE changed it
			// with no edge, no actor and no evidence (F-168).
			`sanctions_state = 'CLEAR'`,
		} {
			_, err := testDB.Exec(ctx, `UPDATE compliance_profiles SET `+set+` WHERE user_id = $1`, userID)
			require.Errorf(t, err, "UPDATE ... SET %s must be refused", set)
			assert.Equalf(t, "42501", db.SQLState(err), "%s: expected a privilege refusal", set)
		}
		// And the attribute half it DOES own still works, which is what keeps
		// the row lock available (00744's rule).
		_, err := testDB.Exec(ctx, `UPDATE compliance_profiles SET residency_country = 'US' WHERE user_id = $1`, userID)
		require.NoError(t, err)
	})

	t.Run("a transition row writes the state and an illegal edge is refused", func(t *testing.T) {
		repo := verification.NewRepository()
		now := time.Now().UTC()

		// UNVERIFIED -> VERIFIED is not an edge, and the Go table says so
		// before the database is asked.
		err := inTx(t, func(ctx context.Context, tx pgx.Tx) error {
			_, terr := repo.TransitionProfile(ctx, tx, verification.ProfileTransition{
				UserID: userID, To: verification.StateVerified,
				ActorType: security.ActorSystem, ActorID: "itest", Reason: "forged", OccurredAt: now,
			})
			return terr
		})
		assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err),
			"nobody becomes verified without a provider decision")

		// The legal path does move it, and the trigger writes the column.
		require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
			for _, to := range []verification.State{
				verification.StateRequired, verification.StateStarted, verification.StatePending,
			} {
				if _, terr := repo.TransitionProfile(ctx, tx, verification.ProfileTransition{
					UserID: userID, To: to,
					ActorType: security.ActorSystem, ActorID: "itest", Reason: "walking the machine",
					OccurredAt: now,
				}); terr != nil {
					return terr
				}
			}
			return nil
		}))
		state, _, _, err := repo.ProfileState(ctx, testDB, userID)
		require.NoError(t, err)
		assert.Equal(t, verification.StatePending, state)

		// A raw transition row with a from_state the profile was never in
		// licenses nothing: the edge binding refuses the whole transaction.
		err = inTx(t, func(ctx context.Context, tx pgx.Tx) error {
			_, ierr := tx.Exec(ctx, `INSERT INTO compliance_profile_transitions
				(id, user_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
				VALUES ($1,$2,'STARTED','VERIFIED','SYSTEM','itest','forged origin', now())`,
				uuid.New(), userID)
			return ierr
		})
		require.Error(t, err, "a row claiming an origin the profile was never in must license nothing")
	})

	t.Run("a customer never attests their own compliance state", func(t *testing.T) {
		repo := verification.NewRepository()
		for _, actor := range []security.ActorType{security.ActorUser, security.ActorAgent, security.ActorService} {
			err := inTx(t, func(ctx context.Context, tx pgx.Tx) error {
				_, terr := repo.TransitionProfile(ctx, tx, verification.ProfileTransition{
					UserID: userID, To: verification.StateVerified,
					ActorType: actor, ActorID: "self", Reason: "self-attestation",
					OccurredAt: time.Now().UTC(),
				})
				return terr
			})
			assert.Equalf(t, errs.CodeForbidden, errs.CodeOf(err), "%s", actor)
		}
	})
}

// A rehearsal cannot exist where real value moves. The CHECK is the last of
// three refusals and the only one a redeploy cannot get around.
func TestIntegration_ASandboxOutcomeCannotExistInProd(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	userID, _ := newAccount(t)
	sessionID := uuid.New()

	_, err := testDB.Exec(ctx, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, rules_version, environment, sandbox)
		VALUES ($1,$2,'PAYOUT_KYC','sandbox_verification','CREATED','v1','PROD',true)`, sessionID, userID)
	require.Error(t, err, "a sandbox session in PROD must be refused")
	assert.Contains(t, err.Error(), "verification_sessions_sandbox_never_in_prod")

	// A real session in PROD is fine; it is the SANDBOX label that PROD refuses.
	// This is the positive control: without it, a refusal below would prove
	// nothing about the sandbox flag.
	_, err = testDB.Exec(ctx, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, rules_version, environment, sandbox)
		VALUES ($1,$2,'PAYOUT_KYC','a_real_provider','CREATED','v1','PROD',false)`, sessionID, userID)
	require.NoError(t, err)
	// Evidence attaches only to a session a provider has answered (00806,
	// F-227), so the session is walked to PROCESSING before a check is hung on
	// it. The sandbox CHECK below is what this test is about, and it is a table
	// CHECK: it is evaluated after the BEFORE trigger that asks this question,
	// so both refusals are still distinguishable.
	for _, edge := range [][2]string{{"CREATED", "PENDING_USER_ACTION"}, {"PENDING_USER_ACTION", "PROCESSING"}} {
		_, err = testDB.Exec(ctx, `INSERT INTO verification_session_transitions
			(id, session_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,$3,$4,'SYSTEM','itest','the provider was called and answered')`,
			uuid.New(), sessionID, edge[0], edge[1])
		require.NoError(t, err)
	}
	_, err = testDB.Exec(ctx, `INSERT INTO verification_checks
		(id, session_id, user_id, kind, outcome, provider, rules_version, environment, sandbox)
		VALUES ($1,$2,$3,'AGE','PASS','a_real_provider','v1','PROD',false)`, uuid.New(), sessionID, userID)
	require.NoError(t, err, "a real check in PROD is exactly what PROD is for")

	_, err = testDB.Exec(ctx, `INSERT INTO verification_checks
		(id, session_id, user_id, kind, outcome, provider, rules_version, environment, sandbox)
		VALUES ($1,$2,$3,'AGE','PASS','a_real_provider','v1','PROD',true)`, uuid.New(), sessionID, userID)
	require.Error(t, err, "a sandbox check in PROD must be refused")
	assert.Contains(t, err.Error(), "verification_checks_sandbox_never_in_prod")
}

// A session cannot be born decided, and one person has at most one live
// attempt: two answers arriving in an uncontrolled order is a race, not a
// decision.
func TestIntegration_SessionBirthControlAndOneOpenPerPerson(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	userID, _ := newAccount(t)

	_, err := testDB.Exec(ctx, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, rules_version, environment, sandbox)
		VALUES ($1,$2,'PAYOUT_KYC','p','APPROVED','v1','STAGING',true)`, uuid.New(), userID)
	require.Error(t, err)
	assert.Equal(t, "AD001", db.SQLState(err))
	assert.Contains(t, err.Error(), "VERIFICATION_SESSION_BORN_DECIDED")

	repo := verification.NewRepository()
	var first verification.Session
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		first, cerr = repo.CreateSession(ctx, tx, verification.Session{
			UserID: userID, Purpose: verification.PurposePayoutKYC, Provider: "p",
			RulesVersion: "v1", Environment: "STAGING", Sandbox: true,
		})
		return cerr
	}))
	assert.Equal(t, verification.SessionCreated, first.Status)

	err = inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, cerr := repo.CreateSession(ctx, tx, verification.Session{
			UserID: userID, Purpose: verification.PurposePayoutKYC, Provider: "p",
			RulesVersion: "v1", Environment: "STAGING", Sandbox: true,
		})
		return cerr
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "one live attempt per person")

	// Once it is terminal, a new attempt is permitted.
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, terr := repo.TransitionSession(ctx, tx, first.ID, verification.SessionCancelled, verification.SessionChange{
			ActorType: security.ActorSystem, ActorID: "itest", Reason: "abandoned", OccurredAt: time.Now().UTC(),
		})
		return terr
	}))
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, cerr := repo.CreateSession(ctx, tx, verification.Session{
			UserID: userID, Purpose: verification.PurposePayoutKYC, Provider: "p",
			RulesVersion: "v1", Environment: "STAGING", Sandbox: true,
		})
		return cerr
	}))

	// And the session status is not the application's to write either.
	_, err = testDB.Exec(ctx, `UPDATE verification_sessions SET status = 'APPROVED' WHERE id = $1`, first.ID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
}

// The whole journey on a sandbox tier: start, choose an outcome explicitly,
// and watch the level move from what Nodal can establish by itself to what a
// provider decision plus its evidence supports.
func TestIntegration_TheSandboxJourneyEndToEnd(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc, repo := newService(t, true, func() time.Time { return now })

	cases := []struct {
		outcome   verification.SandboxOutcome
		wantState verification.State
		wantLevel valuedomain.VerificationLevel
	}{
		{verification.SandboxVerified, verification.StateVerified, valuedomain.VerificationEnhanced},
		{verification.SandboxUnderage, verification.StateRejected, valuedomain.VerificationNodalIdentity},
		{verification.SandboxSanctioned, verification.StateRejected, valuedomain.VerificationNodalIdentity},
		{verification.SandboxRejected, verification.StateRejected, valuedomain.VerificationNodalIdentity},
		{verification.SandboxNeedsInformation, verification.StateNeedsInformation, valuedomain.VerificationNodalIdentity},
	}
	for _, c := range cases {
		t.Run(string(c.outcome), func(t *testing.T) {
			userID, accountID := newAccount(t)

			started, err := svc.Start(ctx, testDB, verification.StartRequest{
				AccountID:    accountID,
				Purpose:      verification.PurposeEnhanced,
				Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
			})
			require.NoError(t, err)
			assert.True(t, started.Sandbox)
			assert.Equal(t, verification.SandboxControlPath, started.SandboxControlPath,
				"a rehearsal session says so by carrying the control that decides it")
			assert.NotEmpty(t, started.HostedURL)

			// Before anybody chooses, nothing is decided. This is the property
			// the whole design rests on.
			state, _, _, err := repo.ProfileState(ctx, testDB, userID)
			require.NoError(t, err)
			assert.Equal(t, verification.StateStarted, state)
			snap, err := svc.Snapshot(ctx, testDB, accountID, valuedomain.VerificationNodalIdentity)
			require.NoError(t, err)
			assert.Equal(t, valuedomain.VerificationNodalIdentity, snap.Level,
				"an unanswered session establishes nothing")
			assert.False(t, snap.PayoutReady())

			session, err := svc.SetSandboxOutcome(ctx, testDB, accountID, c.outcome)
			require.NoError(t, err)
			assert.True(t, session.Sandbox)

			state, _, _, err = repo.ProfileState(ctx, testDB, userID)
			require.NoError(t, err)
			assert.Equal(t, c.wantState, state)

			snap, err = svc.Snapshot(ctx, testDB, accountID, valuedomain.VerificationNodalIdentity)
			require.NoError(t, err)
			assert.Equal(t, c.wantLevel, snap.Level)
			assert.True(t, snap.Sandbox, "a rehearsal-derived level is labelled everywhere it is shown")
			assert.Equal(t, rules.Version, snap.RulesVersion)
			assert.True(t, snap.JurisdictionSupported)
			assert.Equal(t, 18, snap.MinimumAge)

			if c.outcome == verification.SandboxVerified {
				assert.True(t, snap.PayoutReady())
				assert.Empty(t, snap.Missing)
				assert.Equal(t, compliance.SanctionsClear, snap.SanctionsState)
				assert.True(t, snap.AgeVerified)
				assert.Len(t, snap.Checks, 5, "all five sub-checks are recorded as evidence")
				for _, ch := range snap.Checks {
					assert.True(t, ch.Sandbox, "%s", ch.Kind)
					assert.Equal(t, "STAGING", ch.Environment)
				}
				return
			}

			assert.False(t, snap.PayoutReady())
			assert.NotEmpty(t, snap.Missing, "a refusal always says what is missing and what to do")
			switch c.outcome {
			case verification.SandboxUnderage:
				assert.Equal(t, verification.OutcomeFail, outcomeOf(snap, verification.CheckAge))
				assert.Equal(t, verification.OutcomePass, outcomeOf(snap, verification.CheckIdentityDocument),
					"the document was fine and the person is too young; two different answers")
			case verification.SandboxSanctioned:
				assert.Equal(t, verification.OutcomeFail, outcomeOf(snap, verification.CheckSanctions))
				assert.Equal(t, compliance.SanctionsHit, snap.SanctionsState)
			}
		})
	}
}

func outcomeOf(s verification.Snapshot, kind verification.CheckKind) verification.Outcome {
	for _, c := range s.Checks {
		if c.Kind == kind {
			return c.Outcome
		}
	}
	return verification.OutcomeUnknown
}

// The sandbox control is refused when the deployment is not a sandbox tier,
// before anything is written. It is the same refusal the handler makes and the
// same one the CHECK makes; all three exist because a fabricated approval is
// the worst thing this system could produce.
func TestIntegration_TheSandboxControlIsRefusedOutsideASandboxTier(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, accountID := newAccount(t)

	sandboxSvc, _ := newService(t, true, func() time.Time { return now })
	_, err := sandboxSvc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)

	strictSvc, _ := newService(t, false, func() time.Time { return now })
	_, err = strictSvc.SetSandboxOutcome(ctx, testDB, accountID, verification.SandboxVerified)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "sandbox tier")

	// And nothing was written: the session is still awaiting an answer.
	snap, err := sandboxSvc.Snapshot(ctx, testDB, accountID, valuedomain.VerificationNodalIdentity)
	require.NoError(t, err)
	assert.Equal(t, verification.StateStarted, snap.State)
	assert.Empty(t, snap.Checks)
}

// A jurisdiction the rule table does not offer is refused before a session is
// opened, so nobody hands over identity documents to a flow that could never
// have finished.
func TestIntegration_AnUnofferedJurisdictionIsRefusedBeforeAnythingStarts(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc, _ := newService(t, true, func() time.Time { return now })
	_, accountID := newAccount(t)

	for _, j := range []rules.Jurisdiction{
		{Country: "GB", Region: "LND"},
		{Country: "IR", Region: "THR"},
		{Country: "US"}, // no subdivision, and the rules depend on one
	} {
		_, err := svc.Start(ctx, testDB, verification.StartRequest{
			AccountID: accountID, Purpose: verification.PurposePayoutKYC, Jurisdiction: j,
		})
		require.Errorf(t, err, "%s", j)
		assert.Equalf(t, errs.CodeEligibilityJurisdiction, errs.CodeOf(err), "%s", j)
	}

	// And with no country at all, the refusal says where the answer has to
	// come from rather than guessing.
	_, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "network address")
}

// The composite resolver, which is what the payout engine actually reads.
func TestIntegration_TheResolverComposesRatherThanReplaces(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc, repo := newService(t, true, func() time.Time { return now })
	_, accountID := newAccount(t)

	resolver, err := verification.NewResolver(
		verification.StaticBase(valuedomain.VerificationNodalIdentity), repo, testDB, fixedClock(now),
	)
	require.NoError(t, err)

	level, err := resolver.Level(ctx, accountID)
	require.NoError(t, err)
	assert.Equal(t, valuedomain.VerificationNodalIdentity, level, "nothing established yet")

	_, err = svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)
	_, err = svc.SetSandboxOutcome(ctx, testDB, accountID, verification.SandboxVerified)
	require.NoError(t, err)

	level, err = resolver.Level(ctx, accountID)
	require.NoError(t, err)
	assert.True(t, level.AtLeast(valuedomain.VerificationPayoutKYC),
		"a provider decision plus its evidence reaches the level the cap could not")

	// A base of NONE caps everything, however good the evidence is.
	capped, err := verification.NewResolver(
		verification.StaticBase(valuedomain.VerificationNone), repo, testDB, fixedClock(now),
	)
	require.NoError(t, err)
	level, err = capped.Level(ctx, accountID)
	require.NoError(t, err)
	assert.Equal(t, valuedomain.VerificationNone, level)

	// An account that does not exist is answered, not errored: the caller is
	// deciding what an account may do, and "nothing" is the right answer.
	level, err = resolver.Level(ctx, accounts.NewAccountID())
	require.NoError(t, err)
	assert.Equal(t, valuedomain.VerificationNone, level)
}

// Polling is idempotent and never trusts a redirect: a second poll of a settled
// session records nothing new.
func TestIntegration_PollingIsIdempotent(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc, _ := newService(t, true, func() time.Time { return now })
	userID, accountID := newAccount(t)

	started, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)

	// A poll before anybody answers leaves everything where it was.
	session, err := svc.Poll(ctx, testDB, accountID, started.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, verification.SessionPendingUserAction, session.Status)

	_, err = svc.SetSandboxOutcome(ctx, testDB, accountID, verification.SandboxVerified)
	require.NoError(t, err)

	var before int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM verification_checks WHERE user_id = $1`, userID).Scan(&before))
	for i := 0; i < 3; i++ {
		session, err = svc.Poll(ctx, testDB, accountID, started.Session.ID)
		require.NoError(t, err)
		assert.Equal(t, verification.SessionApproved, session.Status)
	}
	var after int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM verification_checks WHERE user_id = $1`, userID).Scan(&after))
	assert.Equal(t, before, after, "re-polling a settled session records nothing new")

	// Somebody else's session is NOT_FOUND rather than FORBIDDEN.
	_, otherAccount := newAccount(t)
	_, err = svc.Poll(ctx, testDB, otherAccount, started.Session.ID)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

// Verification changes eligibility and never changes what Credits are. The
// strongest statement of it available without a ledger: after a full
// verification, this package has written to exactly three tables and none of
// them is a Credit table.
func TestIntegration_VerificationNeverTouchesACredit(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc, _ := newService(t, true, func() time.Time { return now })
	userID, accountID := newAccount(t)

	countOf := func(table string) int {
		var n int
		require.NoError(t, testDB.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
		return n
	}
	lotsBefore := countOf("credit_lots")
	eventsBefore := countOf("credit_lot_events")
	journalBefore := countOf("journal_transactions")

	_, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposeEnhanced,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)
	_, err = svc.SetSandboxOutcome(ctx, testDB, accountID, verification.SandboxVerified)
	require.NoError(t, err)

	assert.Equal(t, lotsBefore, countOf("credit_lots"), "verification minted no Credits")
	assert.Equal(t, eventsBefore, countOf("credit_lot_events"), "verification moved no Credits")
	assert.Equal(t, journalBefore, countOf("journal_transactions"), "verification posted nothing")

	var profileState string
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT identity_state FROM compliance_profiles WHERE user_id = $1`, userID).Scan(&profileState))
	assert.Equal(t, "VERIFIED", profileState, "what changed is the person's standing, and only that")
}
