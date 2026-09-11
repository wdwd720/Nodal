//go:build integration

package eligibility

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Shared with internal/db and test/integration/migrations: schema-mutating
// suites hold pg_advisory_lock(424242) exclusively; this DML-only suite holds
// it shared. Run against an isolated database:
//
//	eval "$(go run ./scripts/testdb -name risk -export)"
//	go test -count=1 -race -tags=integration ./internal/eligibility/
const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eligibility integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "eligibility integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "eligibility integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "eligibility-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "eligibility integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

// isolatedEra returns a random instant in 1990-2020 so policies recorded by
// one test (always with an expiry) never fall inside another test's window;
// cp_app cannot delete rows, so isolation is by time rather than cleanup.
func isolatedEra(t *testing.T) time.Time {
	t.Helper()
	var b [8]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	offset := time.Duration(binary.LittleEndian.Uint64(b[:])%uint64(30*365*24)) * time.Hour
	return time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
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
		user, err = repo.CreateUser(ctx, tx, "https://idp.test", "sub-"+uuid.NewString(), nil)
		if err != nil {
			return err
		}
		acct, err = repo.CreateAccount(ctx, tx, user.ID, accounts.KindCustomer)
		return err
	}))
	return user.ID, acct.ID
}

func TestIntegration_RecordPolicy_RejectsAgentsAndUsers(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	era := isolatedEra(t)
	rules := fixtureRules(t, "us_baseline")
	base := PolicyRecord{Version: "v-" + uuid.NewString(), Rules: rules, EffectiveAt: era, ActorID: "op-1", Reason: "test"}

	err := inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		rec := base
		rec.ActorType = security.ActorAgent
		_, err := s.RecordPolicy(ctx, tx, rec)
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "AGENT actor type")

	err = inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		rec := base
		rec.ActorType = security.ActorOperator
		ctx = security.WithPrincipal(ctx, security.AgentPrincipal("agent-1", "acct-1"))
		_, err := s.RecordPolicy(ctx, tx, rec)
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "AGENT principal on the context")

	err = inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		rec := base
		rec.ActorType = security.ActorUser
		_, err := s.RecordPolicy(ctx, tx, rec)
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "USER actor type")

	err = inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		rec := base
		rec.ActorType = security.ActorOperator
		rec.Rules = mutateRules(t, rules, []string{"bogus"}, 1)
		_, err := s.RecordPolicy(ctx, tx, rec)
		return err
	})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "unknown rule key fails closed before any insert")
}

func TestIntegration_PolicyRoundTripAndCurrent(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	era := isolatedEra(t)
	expires := era.Add(3 * time.Hour)
	rules := fixtureRules(t, "us_baseline")
	v1, v2, v3 := "v1-"+uuid.NewString(), "v2-"+uuid.NewString(), "v3-"+uuid.NewString()

	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		for _, rec := range []PolicyRecord{
			{Version: v1, Rules: rules, EffectiveAt: era, ExpiresAt: &expires, ActorType: security.ActorOperator, ActorID: "op-1", Reason: "initial"},
			{Version: v2, Rules: mutateRules(t, rules, []string{"minimum_age"}, 21), EffectiveAt: era.Add(time.Hour), ExpiresAt: &expires, ActorType: security.ActorSystem, ActorID: "seed", Reason: "raise age"},
			{Version: v3, Rules: rules, EffectiveAt: era.Add(2 * time.Hour), ExpiresAt: &expires, ActorType: security.ActorOperator, ActorID: "op-2", Reason: "future"},
		} {
			p, err := s.RecordPolicy(ctx, tx, rec)
			if err != nil {
				return err
			}
			if p.Version != rec.Version {
				return fmt.Errorf("version %q not set", p.Version)
			}
		}
		return nil
	}))

	ctx := context.Background()
	p, version, err := s.CurrentPolicy(ctx, testDB, era.Add(90*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, v2, version)
	assert.Equal(t, v2, p.Version)
	assert.Equal(t, 21, p.MinimumAge)
	assert.Equal(t, []string{"CA", "KR", "US"}, p.AllowedCountries)

	p, version, err = s.CurrentPolicy(ctx, testDB, era.Add(30*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, v1, version)
	assert.Equal(t, 18, p.MinimumAge)

	_, version, err = s.CurrentPolicy(ctx, testDB, era.Add(2*time.Hour+time.Minute))
	require.NoError(t, err)
	assert.Equal(t, v3, version)

	_, _, err = s.CurrentPolicy(ctx, testDB, era.Add(3*time.Hour))
	assert.ErrorIs(t, err, ErrNoPolicy, "expired at exactly expires_at")
	_, _, err = s.CurrentPolicy(ctx, testDB, era.Add(-time.Second))
	assert.ErrorIs(t, err, ErrNoPolicy, "nothing effective before the first version")

	err = inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.RecordPolicy(ctx, tx, PolicyRecord{Version: v1, Rules: rules, EffectiveAt: era, ExpiresAt: &expires, ActorType: security.ActorOperator, ActorID: "op-1", Reason: "dup"})
		return err
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "versions are unique")
}

func TestIntegration_CurrentPolicy_TamperedHashFailsClosed(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	era := isolatedEra(t)
	expires := era.Add(time.Hour)
	p := MustParsePolicy(fixtureRules(t, "us_baseline"))
	canon, err := p.CanonicalJSON()
	require.NoError(t, err)
	_, err = testDB.Exec(context.Background(), `INSERT INTO eligibility_policies
		(id, version, rules, rules_hash, effective_at, expires_at, created_by_actor_type, created_by_actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,'OPERATOR','op-x','tamper test')`,
		uuid.New(), "tampered-"+uuid.NewString(), canon, []byte("not-the-hash"), era, expires)
	require.NoError(t, err)

	_, _, err = s.CurrentPolicy(context.Background(), testDB, era.Add(time.Minute))
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "hash mismatch")
}

func TestIntegration_RecordDecision_RoundTrip(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	userID, accountID := newAccount(t)
	p := loadPolicyFixture(t, "us_baseline")
	cases := loadCases(t)
	in := caseInput(t, cases[0])
	in.AccountID = accountID.String()
	in.UserID = userID.String()
	in.JurisdictionRegion = "NY"
	in.Restrictions = []string{"NO_TRADING"}
	in.Now = time.Date(2026, 9, 5, 12, 0, 0, 123456000, time.UTC)
	d := Evaluate(p, in)
	require.False(t, d.Eligible)

	var decisionID DecisionID
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decisionID, err = s.RecordDecision(ctx, tx, in, d, "corr-1")
		return err
	}))

	var (
		gotAccount, gotUser            uuid.UUID
		gotIntent, gotInstrument       *uuid.UUID
		contextKind, venue, assetClass string
		provider, correlation          *string
		eligible                       bool
		policyVersion                  string
		codes                          []string
		contextHash                    []byte
		evaluatedAt                    time.Time
	)
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT account_id, user_id, intent_id, instrument_id, context_kind, venue, asset_class, provider,
		eligible, policy_version, reason_codes, context_hash, evaluated_at, correlation_id FROM eligibility_decisions WHERE id = $1`, decisionID).
		Scan(&gotAccount, &gotUser, &gotIntent, &gotInstrument, &contextKind, &venue, &assetClass, &provider,
			&eligible, &policyVersion, &codes, &contextHash, &evaluatedAt, &correlation))
	assert.Equal(t, accountID.String(), gotAccount.String())
	assert.Equal(t, userID.String(), gotUser.String())
	assert.Nil(t, gotIntent)
	assert.Nil(t, gotInstrument)
	assert.Equal(t, "TRADE", contextKind)
	assert.Equal(t, "JUPITER", venue)
	assert.Equal(t, "CRYPTO_SPOT", assetClass)
	assert.Nil(t, provider)
	assert.False(t, eligible)
	assert.Equal(t, p.Version, policyVersion)
	assert.Equal(t, []string{ReasonAccountRestriction, ReasonRegionBlocked}, codes)
	assert.Equal(t, d.ContextHash, hex.EncodeToString(contextHash))
	assert.True(t, evaluatedAt.Equal(in.Now), "evaluated_at keeps microsecond precision: %s vs %s", evaluatedAt, in.Now)
	require.NotNil(t, correlation)
	assert.Equal(t, "corr-1", *correlation)

	tampered := d
	tampered.Eligible = true
	err := inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.RecordDecision(ctx, tx, in, tampered, "")
		return err
	})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a modified decision is refused")

	missing := Evaluate(Policy{}, in)
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.RecordDecision(ctx, tx, in, missing, "")
		return err
	}), "a POLICY_MISSING decision is persisted with an empty policy version")
}

func TestIntegration_LoadInputFromProfile(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	userID, accountID := newAccount(t)
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	in, err := s.LoadInputFromProfile(ctx, testDB, accountID, at)
	require.NoError(t, err)
	assert.Equal(t, IdentityUnverified, in.IdentityState, "no profile is unverified")
	assert.Equal(t, SanctionsUnknown, in.SanctionsState)
	assert.Equal(t, accounts.StatusActive, in.AccountStatus)
	assert.Equal(t, accountID.String(), in.AccountID)
	assert.Equal(t, userID.String(), in.UserID)
	assert.Equal(t, []string{}, in.Restrictions)
	assert.Equal(t, at, in.Now)

	expires := at.Add(24 * time.Hour)
	// A profile is born UNVERIFIED and reaches VERIFIED through transition rows
	// (migration 00761): identity_state, verified_at and expires_at are written
	// by a trigger and cp_app has no UPDATE privilege on any of them. So the
	// fixture inserts the attribute half and then walks the state machine, which
	// is also the only path a real verification takes.
	_, err = testDB.Exec(ctx, `INSERT INTO compliance_profiles
		(user_id, identity_state, age_verified, jurisdiction_country, jurisdiction_region, residency_country, sanctions_state, restrictions)
		VALUES ($1,'UNVERIFIED',true,'us','ny','US','CLEAR','["NO_WITHDRAWALS","NO_TRADING"]')`, userID)
	require.NoError(t, err)
	verifyProfile(t, userID, at.Add(-time.Hour), expires)

	in, err = s.LoadInputFromProfile(ctx, testDB, accountID, at)
	require.NoError(t, err)
	assert.Equal(t, IdentityVerified, in.IdentityState)
	assert.True(t, in.AgeVerified)
	assert.Equal(t, "US", in.JurisdictionCountry, "codes are upper-cased")
	assert.Equal(t, "NY", in.JurisdictionRegion)
	assert.Equal(t, "US", in.ResidencyCountry)
	assert.Equal(t, SanctionsClear, in.SanctionsState)
	assert.Equal(t, []string{"NO_TRADING", "NO_WITHDRAWALS"}, in.Restrictions, "restrictions are sorted")

	in, err = s.LoadInputFromProfile(ctx, testDB, accountID, expires)
	require.NoError(t, err)
	assert.Equal(t, IdentityExpired, in.IdentityState, "a verification past expires_at is EXPIRED")

	_, err = s.LoadInputFromProfile(ctx, testDB, accounts.NewAccountID(), at)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	in.Context = ContextTrade
	in.InstrumentRiskClass = "MAJOR"
	in.InstrumentStatus = "ACTIVE"
	in.AssetClass = "CRYPTO_SPOT"
	in.Venue = "JUPITER"
	in.Capabilities = map[string]bool{"LIVE_MANUAL_TRADING": true}
	d := Evaluate(loadPolicyFixture(t, "us_baseline"), in)
	assert.True(t, errors.Is(nil, nil))
	assert.Equal(t, []string{ReasonAccountRestriction, ReasonIdentityState, ReasonRegionBlocked}, d.ReasonCodes)
}

// verifyProfile walks a profile from UNVERIFIED to VERIFIED the way the
// verification service does: one transition row per edge, in one transaction,
// with the trigger writing the state each time.
func verifyProfile(t *testing.T, userID accounts.UserID, verifiedAt, expiresAt time.Time) {
	t.Helper()
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		edges := [][2]string{
			{"UNVERIFIED", "REQUIRED"},
			{"REQUIRED", "STARTED"},
			{"STARTED", "PENDING"},
			{"PENDING", "VERIFIED"},
		}
		for _, e := range edges {
			var verified, expires any
			if e[1] == "VERIFIED" {
				verified, expires = verifiedAt, expiresAt
			}
			if _, err := tx.Exec(ctx, `INSERT INTO compliance_profile_transitions
				(id, user_id, from_state, to_state, actor_type, actor_id, reason, verified_at, expires_at, occurred_at)
				VALUES ($1,$2,$3,$4,'SYSTEM','eligibility-itest','fixture',$5,$6,now())`,
				uuid.New(), userID, e[0], e[1], verified, expires); err != nil {
				return err
			}
		}
		return nil
	}))
}
