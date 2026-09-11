//go:build integration

package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/terms"
)

// The seam between the legal registry and the withdrawal surfaces (D-083).
//
// internal/payout refuses without the disclosure and internal/eligibility
// reports it; both are unit-tested against a boolean. The question a database
// answers is where that boolean comes from: the owner of THIS account, the
// documents required at THIS point in the journey, and the version and bytes
// served NOW.

func newDisclosureDeps(t *testing.T, pool *db.DB) (WithdrawalDeps, accounts.AccountID, accounts.UserID) {
	t.Helper()
	ctx := context.Background()
	repo := accounts.NewRepository()
	user, err := repo.CreateUser(ctx, pool, "terms-itest", "sub-"+randomSubject(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(ctx, pool, user.ID, accounts.KindCustomer)
	require.NoError(t, err)

	svc, err := profile.New(profile.Deps{
		DB: pool, Repo: profile.NewRepository(), Accounts: repo,
		Audit: audit.NewWriter(), Clock: clock.NewFake(time.Now().UTC().Truncate(time.Microsecond)),
		// Nothing here closes an account, so the session ender is the one that
		// ends none. A nil is refused by profile.New deliberately, because a
		// deployment that closes accounts and leaves their sessions live is the
		// failure that refusal exists for.
		Sessions: noSessions{},
	})
	require.NoError(t, err)
	return WithdrawalDeps{Terms: svc}, acct.ID, user.ID
}

type noSessions struct{}

func (noSessions) RevokeAllForSubject(context.Context, auth.Querier, string) (int, error) {
	return 0, nil
}

func randomSubject() string {
	return id.New[id.Any]().String()
}

// TestIntegration_TheDisclosureIsReadForTheAccountsOwner.
func TestIntegration_TheDisclosureIsReadForTheAccountsOwner(t *testing.T) {
	appURL := testAppDSN(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "terms-itest", MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	deps, acct, user := newDisclosureDeps(t, pool)

	accepted, err := deps.disclosureAccepted(ctx, pool, acct)
	require.NoError(t, err)
	assert.False(t, accepted,
		"a person who has never withdrawn has not accepted the withdrawal disclosure; §48 does not ask at signup")

	// Accepting the ONBOARDING documents changes nothing here. That separation
	// is the whole reason terms.Requirement is not a boolean.
	actor := profile.Actor{UserID: user.String(), ActorType: "USER"}
	onboarding := make([]terms.DocumentID, 0, 4)
	for _, d := range terms.RequiredAt(terms.AtOnboarding) {
		onboarding = append(onboarding, d.ID)
	}
	require.NotEmpty(t, onboarding)
	_, err = deps.Terms.(*profile.Service).Accept(ctx, actor, onboarding)
	require.NoError(t, err)

	accepted, err = deps.disclosureAccepted(ctx, pool, acct)
	require.NoError(t, err)
	assert.False(t, accepted, "the onboarding documents are not the withdrawal disclosure")

	// And the document itself.
	_, err = deps.Terms.(*profile.Service).Accept(ctx, actor, []terms.DocumentID{terms.WithdrawalDisclosure})
	require.NoError(t, err)
	accepted, err = deps.disclosureAccepted(ctx, pool, acct)
	require.NoError(t, err)
	assert.True(t, accepted)
}

// TestIntegration_AnAcceptanceOfOtherBytesDoesNotCount.
//
// D-053: an acceptance counts only when the version AND the content hash match
// what is served now, so a disclosure amended without its version being bumped
// is a document nobody has agreed to. This writes an acceptance carrying the
// right version and the wrong bytes, which is the shape that failure takes.
func TestIntegration_AnAcceptanceOfOtherBytesDoesNotCount(t *testing.T) {
	appURL := testAppDSN(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "terms-bytes-itest", MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	deps, acct, user := newDisclosureDeps(t, pool)
	doc, ok := terms.Get(terms.WithdrawalDisclosure)
	require.True(t, ok)

	_, err = pool.Exec(ctx, `INSERT INTO terms_acceptances
		(id, user_id, document_id, version, content_hash, actor_type, actor_id)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'USER', $5)`,
		user, string(terms.WithdrawalDisclosure), doc.Version,
		"0000000000000000000000000000000000000000000000000000000000000000", user.String())
	require.NoError(t, err)

	accepted, err := deps.disclosureAccepted(ctx, pool, acct)
	require.NoError(t, err)
	assert.False(t, accepted,
		"a signature on bytes nobody serves is not a signature on the document")
}

// TestIntegration_AnUnwiredRegistryRefusesRatherThanPermits.
//
// A deployment that has not wired the legal registry has not obtained anybody's
// agreement to anything, and the safe reading of "I cannot tell" is "not
// accepted".
func TestIntegration_AnUnwiredRegistryRefusesRatherThanPermits(t *testing.T) {
	appURL := testAppDSN(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "terms-unwired-itest", MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	_, acct, _ := newDisclosureDeps(t, pool)
	accepted, err := WithdrawalDeps{}.disclosureAccepted(ctx, pool, acct)
	require.NoError(t, err)
	assert.False(t, accepted)
}

// TestIntegration_TheEligibilityExplanationReportsItAsAStep.
//
// The reason reaches the response before the quote step, and the withdrawable
// figures beside it stay true. Composed here from the real acceptance state, so
// the reason is the one a person would actually see.
func TestIntegration_TheEligibilityExplanationReportsItAsAStep(t *testing.T) {
	appURL := testAppDSN(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "terms-eligibility-itest", MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	deps, acct, user := newDisclosureDeps(t, pool)
	accepted, err := deps.disclosureAccepted(ctx, pool, acct)
	require.NoError(t, err)

	in := eligibility.WithdrawalInput{
		PolicyValid: true, JurisdictionSupported: true, ProviderAvailable: true,
		DestinationConfigured: true, DisclosureAccepted: accepted,
	}
	e := eligibility.ExplainWithdrawal(in)
	assert.False(t, e.Eligible)
	assert.Contains(t, reasonCodes(e.Reasons), string(eligibility.WithdrawalTermsNotAccepted),
		"the eligibility page names the step before anybody reaches the quote")

	_, err = deps.Terms.(*profile.Service).Accept(ctx,
		profile.Actor{UserID: user.String(), ActorType: "USER"},
		[]terms.DocumentID{terms.WithdrawalDisclosure})
	require.NoError(t, err)

	in.DisclosureAccepted, err = deps.disclosureAccepted(ctx, pool, acct)
	require.NoError(t, err)
	assert.NotContains(t, reasonCodes(eligibility.ExplainWithdrawal(in).Reasons),
		string(eligibility.WithdrawalTermsNotAccepted))
}

func reasonCodes(rs []eligibility.WithdrawalReason) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}
