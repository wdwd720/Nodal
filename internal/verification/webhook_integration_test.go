//go:build integration

package verification_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// A provider shaped like a real one: hosted, contracted, and NOT a sandbox.
//
// It exists so the service is exercised against something other than the
// rehearsal provider. The two paths differ in exactly one way that matters —
// whether the result carries the sandbox label — and a service that only ever
// saw the sandbox one would not prove that a contracted provider's result is
// accepted on a deployment that is not a sandbox tier.
type contractedProvider struct {
	ref     string
	result  verification.Result
	webhook verification.Result
	err     error
}

func (p *contractedProvider) Name() string { return "contracted_identity_provider" }

func (p *contractedProvider) Capabilities() verification.Capabilities {
	return verification.Capabilities{
		PerformsIdentityDocument:   true,
		PerformsAgeCheck:           true,
		AttestsAgeAtLeast:          18,
		PerformsSanctionsScreening: true,
		PerformsPEPScreening:       true,
		HostedFlow:                 true,
		SupportsWebhooks:           true,
		SupportedCountries:         []string{"US"},
		Availability:               verification.AvailabilityLive,
		// Not a real contract; a string that makes the registry treat this as
		// a contracted provider so the non-sandbox path is reachable in a test.
		// Nothing outside this file constructs it.
		ContractReference: "ITEST-NOT-A-REAL-CONTRACT",
	}
}

func (p *contractedProvider) Start(context.Context, verification.SessionRequest) (verification.StartedSession, error) {
	return verification.StartedSession{
		ProviderRef: p.ref,
		HostedURL:   "https://identity.example.test/session/" + p.ref,
		ExpiresAt:   time.Now().Add(10 * time.Minute),
		Environment: "LIVE",
	}, p.err
}

func (p *contractedProvider) Get(context.Context, string) (verification.Result, error) {
	return p.result, p.err
}

func (p *contractedProvider) Resume(context.Context, string) (verification.StartedSession, error) {
	return p.Start(context.Background(), verification.SessionRequest{})
}

func (p *contractedProvider) ParseWebhook(http.Header, []byte) (verification.Result, error) {
	return p.webhook, p.err
}

func approvedResult(ref string, sandbox bool) verification.Result {
	pass := func(k verification.CheckKind) verification.CheckResult {
		return verification.CheckResult{Kind: k, Outcome: verification.OutcomePass, Detail: "OK"}
	}
	return verification.Result{
		ProviderRef: ref, Status: verification.SessionApproved, RawStatus: "approved",
		AgeAtLeast: 18, Sandbox: sandbox,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
		Checks: []verification.CheckResult{
			pass(verification.CheckIdentityDocument), pass(verification.CheckAge),
			pass(verification.CheckJurisdiction), pass(verification.CheckSanctions),
			pass(verification.CheckPEP),
		},
	}
}

func newContractedService(t *testing.T, p *contractedProvider, now time.Time) *verification.Service {
	t.Helper()
	// allowSandbox = false: a contracted provider must register on a registry
	// that refuses rehearsals, which is the state of a production deployment.
	reg := verification.NewRegistry(false)
	require.NoError(t, reg.Register(p))
	svc, err := verification.NewService(verification.Deps{
		Repo:        verification.NewRepository(),
		Compliance:  compliance.NewRepository(audit.NewWriter()),
		Providers:   reg,
		Clock:       fixedClock(now),
		Environment: "STAGING",
		SandboxTier: false,
	})
	require.NoError(t, err)
	return svc
}

// A webhook is verified by the adapter, matched to its session by the
// provider's reference, and applied exactly once however many times it is
// redelivered — Persona retries up to eight times (PROVIDER_BOUNDARY §3).
func TestIntegration_AWebhookIsIngestedOnceHoweverOftenItArrives(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	userID, accountID := newAccount(t)

	p := &contractedProvider{ref: "prov-ref-" + accountID.String()}
	svc := newContractedService(t, p, now)

	started, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)
	assert.False(t, started.Sandbox, "a contracted provider's session is not a rehearsal")
	assert.Empty(t, started.SandboxControlPath, "and it carries no control that decides it")
	assert.Equal(t, "https://identity.example.test/session/"+p.ref, started.HostedURL)

	// The hosted URL is returned and stored nowhere.
	var stored int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM verification_sessions WHERE id = $1 AND provider_ref = $2`,
		started.Session.ID, p.ref).Scan(&stored))
	assert.Equal(t, 1, stored)

	p.webhook = approvedResult(p.ref, false)
	body := []byte(`{"status":"approved"}`)
	for i := 0; i < 4; i++ {
		session, werr := svc.IngestWebhook(ctx, testDB, p.Name(), http.Header{}, body)
		require.NoErrorf(t, werr, "delivery %d", i)
		assert.Equal(t, verification.SessionApproved, session.Status)
	}

	var checks int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM verification_checks WHERE user_id = $1`, userID).Scan(&checks))
	assert.Equal(t, 5, checks, "four deliveries of the same decision are one decision")

	snap, err := svc.Snapshot(ctx, testDB, accountID, valuedomain.VerificationNodalIdentity)
	require.NoError(t, err)
	assert.Equal(t, verification.StateVerified, snap.State)
	assert.Equal(t, valuedomain.VerificationEnhanced, snap.Level)
	assert.False(t, snap.Sandbox, "a contracted decision is not labelled a rehearsal")
	assert.True(t, snap.PayoutReady())
	require.NotNil(t, snap.VerifiedAt)
	require.NotNil(t, snap.ExpiresAt)
	assert.Equal(t, verification.ValidityWindow, snap.ExpiresAt.Sub(*snap.VerifiedAt),
		"a decision carries the window it stands for")
}

// A provider that reports a SANDBOX result on a deployment that is not a
// sandbox tier is refused outright, before anything is written. A rehearsal
// that reached a real deployment is a defect and not a row.
func TestIntegration_ASandboxResultIsRefusedOnARealDeployment(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	userID, accountID := newAccount(t)

	p := &contractedProvider{ref: "prov-ref-sandbox-" + accountID.String()}
	svc := newContractedService(t, p, now)

	_, err := svc.Start(ctx, testDB, verification.StartRequest{
		AccountID: accountID, Purpose: verification.PurposePayoutKYC,
		Jurisdiction: rules.Jurisdiction{Country: "US", Region: "CA"},
	})
	require.NoError(t, err)

	p.webhook = approvedResult(p.ref, true)
	_, err = svc.IngestWebhook(ctx, testDB, p.Name(), http.Header{}, []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "sandbox tier")

	var checks int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM verification_checks WHERE user_id = $1`, userID).Scan(&checks))
	assert.Zero(t, checks, "the refusal happens before anything is written")

	// And the poll path refuses it the same way, for the same reason.
	p.result = approvedResult(p.ref, true)
	open, found, oerr := verification.NewRepository().OpenSession(ctx, testDB, userID)
	require.NoError(t, oerr)
	require.True(t, found)
	_, err = svc.Poll(ctx, testDB, accountID, open.ID)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}
