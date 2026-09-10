package infra

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/provider/stripe"
	"github.com/nodal/controlplane/internal/provider/stripecredit"
	"github.com/nodal/controlplane/internal/provider/stripepayout"
)

// renderBlueprint is the shape this test cares about. Render's schema is much
// larger; everything not named here is deliberately not this test's business.
type renderBlueprint struct {
	Services []struct {
		Type            string `yaml:"type"`
		Name            string `yaml:"name"`
		Runtime         string `yaml:"runtime"`
		Plan            string `yaml:"plan"`
		Region          string `yaml:"region"`
		DockerfilePath  string `yaml:"dockerfilePath"`
		HealthCheckPath string `yaml:"healthCheckPath"`
		EnvVars         []struct {
			Key   string  `yaml:"key"`
			Value *string `yaml:"value"`
			Sync  *bool   `yaml:"sync"`
		} `yaml:"envVars"`
	} `yaml:"services"`
}

func loadBlueprint(t *testing.T) renderBlueprint {
	t.Helper()
	b, err := os.ReadFile("../../render.yaml")
	require.NoError(t, err, "render.yaml must exist; it is what deploys the launch tier")
	var bp renderBlueprint
	require.NoError(t, yaml.Unmarshal(b, &bp))
	require.Len(t, bp.Services, 1, "the launch tier is one service; anything else costs money")
	return bp
}

// TestRender_TheBlueprintCostsNothingFixed is the constraint the whole tier
// exists to satisfy, checked rather than trusted.
//
// On Render, every service type except a free web service, a static site and
// the two expiring datastores carries a fixed monthly charge -- a background
// worker is $7 a month and a cron job has a $1 minimum. A second service added
// to this file without noticing is how a $0 tier stops being one.
func TestRender_TheBlueprintCostsNothingFixed(t *testing.T) {
	t.Parallel()
	bp := loadBlueprint(t)
	svc := bp.Services[0]

	assert.Equal(t, "web", svc.Type, "only a web service has a free plan on Render")
	assert.Equal(t, "free", svc.Plan, "any other plan is a fixed monthly charge")
	assert.Equal(t, "docker", svc.Runtime)
	assert.Equal(t, "./build/Dockerfile", svc.DockerfilePath)
}

// TestRender_HealthCheckIsLivenessNotReadiness.
//
// /v1/healthz is answered by the process alone. /v1/readyz touches the
// database, and the free tier's database scales to zero after five minutes --
// so pointing Render's health check at readiness would take the service out of
// rotation for being idle, which is most of the time.
func TestRender_HealthCheckIsLivenessNotReadiness(t *testing.T) {
	t.Parallel()
	svc := loadBlueprint(t).Services[0]
	assert.Equal(t, "/v1/healthz", svc.HealthCheckPath)
}

// TestRender_NoSecretIsWrittenIntoTheBlueprint.
//
// render.yaml is committed. Anything with a literal value in it is public to
// everyone who can read the repository, so every secret must arrive as a
// prompted variable (`sync: false`) and every CP_* reference to one must be an
// env:// indirection rather than the value itself.
func TestRender_NoSecretIsWrittenIntoTheBlueprint(t *testing.T) {
	t.Parallel()
	svc := loadBlueprint(t).Services[0]

	prompted := map[string]bool{}
	for _, e := range svc.EnvVars {
		if e.Sync != nil && !*e.Sync {
			assert.Nil(t, e.Value, "%s is prompted and must carry no value", e.Key)
			prompted[e.Key] = true
		}
	}
	assert.NotEmpty(t, prompted, "a deployment with no prompted secrets is one with its secrets in the file")

	for _, e := range svc.EnvVars {
		if e.Value == nil {
			continue
		}
		v := *e.Value
		// Every *_REF and *_URL that names a credential must be an indirection.
		if strings.HasSuffix(e.Key, "_REF") || e.Key == "CP_DATABASE_APP_URL" || e.Key == "CP_DATABASE_MIGRATE_URL" {
			require.True(t, strings.HasPrefix(v, "env://"),
				"%s must reference a variable, not carry a value", e.Key)
			assert.True(t, prompted[strings.TrimPrefix(v, "env://")],
				"%s points at %q, which is not declared as a prompted secret", e.Key, v)
			continue
		}
		// And nothing else may look like key material.
		for _, marker := range []string{"sk_live_", "sk_test_", "whsec_", "-----BEGIN"} {
			assert.NotContains(t, v, marker, "%s carries something that looks like a credential", e.Key)
		}
	}
}

// TestRender_TheBlueprintActuallyStartsTheAPI runs the real configuration
// loader over the blueprint's own variables.
//
// This is the check worth having. Every other assertion here is about the
// file's shape; this one asks internal/config whether cmd/api would start with
// exactly what Render will hand it. It catches a renamed variable, a dropped
// requirement and a typo in a duration, none of which the shape checks would.
func TestRender_TheBlueprintActuallyStartsTheAPI(t *testing.T) {
	t.Parallel()
	svc := loadBlueprint(t).Services[0]

	env := map[string]string{}
	for _, e := range svc.EnvVars {
		if e.Value != nil {
			env[e.Key] = *e.Value
		}
	}
	// The prompted secrets, filled with syntactically valid stand-ins. Their
	// VALUES are never in the repository; their shapes are what the loader
	// checks.
	for k, v := range map[string]string{
		"NODAL_DB_APP_URL":            "postgresql://u:p@h/d?sslmode=require",
		"NODAL_DB_MIGRATE_URL":        "postgresql://u:p@h/d?sslmode=require",
		"NODAL_OIDC_CLIENT_SECRET":    "stand-in",
		"NODAL_STRIPE_API_KEY":        "stand-in",
		"NODAL_STRIPE_WEBHOOK_SECRET": "stand-in",
	} {
		env[k] = v
	}

	_, err := config.Load(context.Background(), config.ServiceAPI, config.LookupFromMap(env))
	require.NoError(t, err, "the blueprint must be a configuration cmd/api can start with")
}

// TestRender_OneProcessIsDeclaredAndMeant.
//
// The blueprint runs a single free instance, and process-local rate-limit
// counters enforce the configured limit only at one process. CP_HTTP_REPLICAS
// is what internal/config believes, and nothing inside the container can check
// it -- so the number in this file and the instance count Render actually runs
// are the same claim, and this test at least stops the file disagreeing with
// itself.
func TestRender_OneProcessIsDeclaredAndMeant(t *testing.T) {
	t.Parallel()
	svc := loadBlueprint(t).Services[0]

	var replicas, backend string
	for _, e := range svc.EnvVars {
		if e.Value == nil {
			continue
		}
		switch e.Key {
		case "CP_HTTP_REPLICAS":
			replicas = *e.Value
		case "CP_RATELIMIT_BACKEND":
			backend = *e.Value
		}
	}
	assert.Equal(t, "memory", backend, "there is no free Redis in this tier")
	assert.Equal(t, "1", replicas,
		"the free plan runs one instance; any other number here would loosen every rate limit by that factor")
}

// TestRender_EveryCapacityCeilingIsStated: internal/capacity refuses a budget
// in which every ceiling is zero, and internal/config refuses the same. This
// checks the deployment states them rather than relying on either refusal.
func TestRender_EveryCapacityCeilingIsStated(t *testing.T) {
	t.Parallel()
	svc := loadBlueprint(t).Services[0]

	want := []string{
		"CP_CAPACITY_MAX_ACCOUNTS",
		"CP_CAPACITY_MAX_PURCHASES_PER_DAY",
		"CP_CAPACITY_MAX_AT_RISK_MINOR",
		"CP_CAPACITY_MAX_DATABASE_BYTES",
	}
	got := map[string]string{}
	for _, e := range svc.EnvVars {
		if e.Value != nil {
			got[e.Key] = *e.Value
		}
	}
	for _, k := range want {
		v, ok := got[k]
		require.True(t, ok, "%s is not stated", k)
		assert.NotEqual(t, "0", v, "%s is disabled, which is not a launch-tier ceiling", k)
	}
}

// TestRender_EveryProviderNameMatchesAnAdapter.
//
// A provider slot's name is not decoration: the adapter compares it against its
// own ProviderName and refuses to build if they differ. The refusal is a WARN
// at startup and a silently disabled capability -- so the whole Credit purchase
// path can be dark in production with a healthy service and a 200 on every
// health check.
//
// That is not hypothetical. This test was written because it happened: the
// blueprint said "stripe-credit" and the adapter is "stripe_credit", and the
// only symptom was the webhook route returning 404. The shipped AWS tfvars
// example had the same defect independently, naming "stripe-onramp" for an
// adapter called "stripe".
//
// The expected names are imported from the adapters, so this cannot drift: a
// renamed adapter fails to compile here rather than failing quietly in a
// deployment.
func TestRender_EveryProviderNameMatchesAnAdapter(t *testing.T) {
	t.Parallel()
	svc := loadBlueprint(t).Services[0]

	// Only slots that a binary actually constructs an adapter for. The rest may
	// carry any name, because nothing ever compares it to anything.
	wantBySlot := map[string]string{
		"CP_PROVIDER_FUNDING_NAME":         stripe.ProviderName,
		"CP_PROVIDER_CREDIT_PURCHASE_NAME": stripecredit.ProviderName,
		"CP_PROVIDER_PAYOUT_NAME":          stripepayout.ProviderName,
	}

	got := map[string]string{}
	for _, e := range svc.EnvVars {
		if e.Value != nil && strings.HasPrefix(e.Key, "CP_PROVIDER_") && strings.HasSuffix(e.Key, "_NAME") {
			got[e.Key] = *e.Value
		}
	}

	for key, want := range wantBySlot {
		have, ok := got[key]
		require.True(t, ok, "%s is not set; the adapter would take the empty name and build, which is worse than refusing", key)
		assert.Equal(t, want, have,
			"%s must equal the adapter's own ProviderName, or the adapter refuses to build and the capability is silently disabled", key)
	}

	// And nothing else claims to be a Stripe adapter under a name no Stripe
	// adapter answers to.
	real := map[string]bool{
		stripe.ProviderName:       true,
		stripecredit.ProviderName: true,
		stripepayout.ProviderName: true,
	}
	for key, v := range got {
		if strings.HasPrefix(v, "stripe") {
			assert.True(t, real[v], "%s names %q, which no Stripe adapter answers to", key, v)
		}
	}
}
