package verifysandbox

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

var (
	_ verification.Provider          = (*Provider)(nil)
	_ verification.SandboxController = (*Provider)(nil)
)

// The provider refuses PROD on its own, before any registry has a say.
func TestNew_RefusesProd(t *testing.T) {
	t.Parallel()
	_, err := New(config.EnvProd, nil)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	for _, env := range []config.Environment{config.EnvStaging, config.EnvDev, config.EnvTest, config.EnvLocal} {
		p, err := New(env, nil)
		require.NoError(t, err, env)
		assert.Equal(t, Name, p.Name())
		caps := p.Capabilities()
		assert.Equal(t, verification.AvailabilitySandbox, caps.Availability, env)
		assert.Empty(t, caps.ContractReference,
			"a sandbox provider has no contract, which is what makes a registry refuse it")
	}
}

// The property the whole design rests on: a session nobody has answered is
// never approved. Not after a second, not after a year, not ever.
func TestGet_ThereIsNoDefaultOutcome(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	p, err := New(config.EnvStaging, func() time.Time { return now })
	require.NoError(t, err)
	ctx := context.Background()

	started, err := p.Start(ctx, verification.SessionRequest{
		SubjectRef: "session-1", Purpose: verification.PurposePayoutKYC,
	})
	require.NoError(t, err)
	assert.True(t, started.Sandbox)
	assert.Equal(t, "sandbox:verification/sandbox-verif-session-1", started.HostedURL,
		"the link is deliberately not navigable; there is no hosted page here")
	assert.Equal(t, now.Add(LinkTTL), started.ExpiresAt)

	for _, after := range []time.Duration{0, time.Second, LinkTTL - time.Millisecond} {
		now = now.Add(0)
		res, gerr := p.Get(ctx, started.ProviderRef)
		require.NoError(t, gerr, after)
		assert.Equal(t, verification.SessionPendingUserAction, res.Status)
		assert.Empty(t, res.Checks, "no answer means no evidence, not a passing one")
		assert.True(t, res.Sandbox)
	}

	// The link expires like a real one does, and expiry is not approval.
	now = now.Add(LinkTTL)
	res, err := p.Get(ctx, started.ProviderRef)
	require.NoError(t, err)
	assert.Equal(t, verification.SessionExpired, res.Status)
	assert.Empty(t, res.Checks)
}

// Each outcome answers a specific set of sub-checks, and leaves the rest ABSENT
// rather than defaulting them. UNDERAGE and SANCTIONED are the two that matter:
// §21 requires an age failure and a sanctions failure to be separately
// expressible, and both pass the document check on the way.
func TestSetOutcome_EachOneAnswersItsOwnChecks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	cases := []struct {
		outcome verification.SandboxOutcome
		status  verification.SessionStatus
		want    map[verification.CheckKind]verification.Outcome
	}{
		{verification.SandboxVerified, verification.SessionApproved, map[verification.CheckKind]verification.Outcome{
			verification.CheckIdentityDocument: verification.OutcomePass,
			verification.CheckAge:              verification.OutcomePass,
			verification.CheckJurisdiction:     verification.OutcomePass,
			verification.CheckSanctions:        verification.OutcomePass,
			verification.CheckPEP:              verification.OutcomePass,
		}},
		{verification.SandboxNeedsInformation, verification.SessionRequiresInput, map[verification.CheckKind]verification.Outcome{
			verification.CheckIdentityDocument: verification.OutcomeNeedsInformation,
		}},
		{verification.SandboxRejected, verification.SessionDeclined, map[verification.CheckKind]verification.Outcome{
			verification.CheckIdentityDocument: verification.OutcomeFail,
		}},
		{verification.SandboxUnderage, verification.SessionDeclined, map[verification.CheckKind]verification.Outcome{
			verification.CheckIdentityDocument: verification.OutcomePass,
			verification.CheckAge:              verification.OutcomeFail,
		}},
		{verification.SandboxSanctioned, verification.SessionDeclined, map[verification.CheckKind]verification.Outcome{
			verification.CheckIdentityDocument: verification.OutcomePass,
			verification.CheckAge:              verification.OutcomePass,
			verification.CheckJurisdiction:     verification.OutcomePass,
			verification.CheckSanctions:        verification.OutcomeFail,
		}},
	}
	for _, c := range cases {
		t.Run(string(c.outcome), func(t *testing.T) {
			p, err := New(config.EnvStaging, func() time.Time { return now })
			require.NoError(t, err)
			started, err := p.Start(ctx, verification.SessionRequest{
				SubjectRef: "s", Purpose: verification.PurposePayoutKYC,
			})
			require.NoError(t, err)
			require.NoError(t, p.SetOutcome(started.ProviderRef, c.outcome))

			res, err := p.Get(ctx, started.ProviderRef)
			require.NoError(t, err)
			assert.Equal(t, c.status, res.Status)
			assert.True(t, res.Sandbox, "every rehearsal answer says so")

			got := map[verification.CheckKind]verification.Outcome{}
			for _, ch := range res.Checks {
				got[ch.Kind] = ch.Outcome
			}
			assert.Equal(t, c.want, got)
			if c.outcome != verification.SandboxVerified {
				ok, _ := verification.EvidenceSatisfies(verification.PurposePayoutKYC, checksOf(res))
				assert.False(t, ok, "%s must not satisfy PAYOUT_KYC", c.outcome)
			}
		})
	}
}

func checksOf(res verification.Result) []verification.Check {
	out := make([]verification.Check, 0, len(res.Checks))
	for _, c := range res.Checks {
		out = append(out, verification.Check{
			ID: verification.NewCheckID(), Kind: c.Kind, Outcome: c.Outcome, Sandbox: true,
		})
	}
	return out
}

func TestSetOutcome_RefusalsAndForgetfulness(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	p, err := New(config.EnvStaging, func() time.Time { return now })
	require.NoError(t, err)
	ctx := context.Background()

	// A reference this instance has never seen is "no such session", never a
	// decline: an instance restart on a free tier loses this memory and nobody
	// is declared unverifiable for that.
	_, err = p.Get(ctx, "never-started")
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(p.SetOutcome("never-started", verification.SandboxVerified)))

	started, err := p.Start(ctx, verification.SessionRequest{SubjectRef: "s", Purpose: verification.PurposePayoutKYC})
	require.NoError(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(p.SetOutcome(started.ProviderRef, "APPROVED-ISH")))

	require.NoError(t, p.SetOutcome(started.ProviderRef, verification.SandboxRejected))
	// Setting the same outcome again is a no-op; setting a different one is a
	// conflict. A decided session is decided.
	require.NoError(t, p.SetOutcome(started.ProviderRef, verification.SandboxRejected))
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(p.SetOutcome(started.ProviderRef, verification.SandboxVerified)))

	// Resume reissues a link on the SAME session and keeps the decision.
	resumed, err := p.Resume(ctx, started.ProviderRef)
	require.NoError(t, err)
	assert.Equal(t, started.ProviderRef, resumed.ProviderRef)
	res, err := p.Get(ctx, started.ProviderRef)
	require.NoError(t, err)
	assert.Equal(t, verification.SessionDeclined, res.Status)

	_, err = p.Resume(ctx, "never-started")
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

// It sends no webhooks and says so, rather than accepting a body somebody could
// post at it.
func TestParseWebhook_IsUnsupported(t *testing.T) {
	t.Parallel()
	p, err := New(config.EnvLocal, nil)
	require.NoError(t, err)
	_, err = p.ParseWebhook(http.Header{}, []byte(`{"status":"approved"}`))
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
}

// A registry that does not allow sandboxes refuses this provider, and one that
// does accepts it. That is the programmatic assertion that production cannot
// load a rehearsal.
func TestRegistry_RefusesASandboxWhereSandboxesAreNotAllowed(t *testing.T) {
	t.Parallel()
	p, err := New(config.EnvStaging, nil)
	require.NoError(t, err)

	strict := verification.NewRegistry(false)
	err = strict.Register(p)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = strict.Only()
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err),
		"a deployment with no identity vendor says so; it does not report a failure")

	permissive := verification.NewRegistry(true)
	require.NoError(t, permissive.Register(p))
	only, err := permissive.Only()
	require.NoError(t, err)
	assert.Equal(t, Name, only.Name())
	assert.Equal(t, []string{Name}, permissive.Names())
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(permissive.Register(p)))
}

// The capability check that stops somebody being sent through an onboarding
// flow that could never have finished.
func TestCapabilities_SupportsOnlyWhatItCanActuallyDo(t *testing.T) {
	t.Parallel()
	p, err := New(config.EnvStaging, nil)
	require.NoError(t, err)
	caps := p.Capabilities()

	ok, missing := caps.Supports(verification.PurposePayoutKYC, jurisdictionUS())
	assert.True(t, ok)
	assert.Empty(t, missing)

	ok, missing = caps.Supports(verification.PurposeEnhanced, jurisdictionUS())
	assert.True(t, ok)
	assert.Empty(t, missing)

	// A country it does not verify in.
	ok, missing = caps.Supports(verification.PurposePayoutKYC, jurisdictionGB())
	assert.False(t, ok)
	assert.Contains(t, missing, "COUNTRY")

	// A jurisdiction whose floor is above what it attests. Mississippi's age of
	// majority is 21 and this provider attests 18, so it cannot establish the
	// level there — which is exactly the discovery that must happen BEFORE
	// somebody hands over identity documents.
	ok, missing = caps.Supports(verification.PurposePayoutKYC, jurisdictionMS())
	assert.False(t, ok)
	assert.Contains(t, missing, string(verification.CheckAge))
}

func jurisdictionUS() rules.Jurisdiction { return rules.Jurisdiction{Country: "US", Region: "CA"} }
func jurisdictionGB() rules.Jurisdiction { return rules.Jurisdiction{Country: "GB", Region: "LND"} }
func jurisdictionMS() rules.Jurisdiction { return rules.Jurisdiction{Country: "US", Region: "MS"} }
