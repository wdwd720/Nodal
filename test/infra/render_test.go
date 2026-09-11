package infra

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/provider/payoutsandbox"
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
		// The static site's half of the file, which this shape stopped at
		// `envVars` and therefore asserted nothing about: its build, its
		// publish directory, its custom domain, its SPA rewrite and its
		// security headers could all be deleted with every test still green
		// (F-138).
		BuildCommand      string   `yaml:"buildCommand"`
		StaticPublishPath string   `yaml:"staticPublishPath"`
		Domains           []string `yaml:"domains"`
		EnvVars           []struct {
			Key   string  `yaml:"key"`
			Value *string `yaml:"value"`
			Sync  *bool   `yaml:"sync"`
		} `yaml:"envVars"`
		Routes []struct {
			Type        string `yaml:"type"`
			Source      string `yaml:"source"`
			Destination string `yaml:"destination"`
		} `yaml:"routes"`
		Headers []struct {
			Path  string `yaml:"path"`
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"headers"`
	} `yaml:"services"`
}

func loadBlueprint(t *testing.T) renderBlueprint {
	t.Helper()
	b, err := os.ReadFile("../../render.yaml")
	require.NoError(t, err, "render.yaml must exist; it is what deploys the launch tier")
	var bp renderBlueprint
	require.NoError(t, yaml.Unmarshal(b, &bp))
	// Two entries: the API, first, and the static site that serves the web
	// app. A static site is free and consumes no instance-hours; a third
	// entry, or a second entry that is not a static site, is how a $0 tier
	// stops being one. The rest of this file reads Services[0] as the API.
	require.Len(t, bp.Services, 2, "the launch tier is one free web service and one static site; anything else costs money")
	require.Equal(t, "nodal-api", bp.Services[0].Name, "the API must stay first: every check below reads Services[0]")
	require.Equal(t, "static", bp.Services[1].Runtime, "the second service must be a static site, the only other kind that is free")
	require.Empty(t, bp.Services[1].Plan, "a static site has no plan; one here means a paid kind was added")
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
	bp := loadBlueprint(t)

	prompted := map[string]bool{}
	for _, svc := range bp.Services {
		for _, e := range svc.EnvVars {
			if e.Sync != nil && !*e.Sync {
				assert.Nil(t, e.Value, "%s is prompted and must carry no value", e.Key)
				prompted[e.Key] = true
			}
		}
	}
	assert.NotEmpty(t, prompted, "a deployment with no prompted secrets is one with its secrets in the file")

	// Which variables are secrets is internal/config's answer, not a guess
	// from the name. A suffix rule read CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF
	// as key material because it ends in _REF -- it is a Stripe account id,
	// and it belongs in the file precisely so it can be reviewed. Asking the
	// configuration means this cannot be wrong in either direction.
	secret := map[string]bool{}
	for _, v := range config.Vars() {
		if v.Secret {
			secret[v.Name] = true
		}
	}
	// The two connection strings carry a password in the value itself rather
	// than as a SecretRef, so they are secrets that the table cannot mark.
	secret["CP_DATABASE_APP_URL"] = true
	secret["CP_DATABASE_MIGRATE_URL"] = true

	for _, svc := range bp.Services {
		for _, e := range svc.EnvVars {
			if e.Value == nil || !secret[e.Key] {
				continue
			}
			v := *e.Value
			require.True(t, strings.HasPrefix(v, "env://"),
				"%s on %s is a secret and must reference a variable, not carry a value", e.Key, svc.Name)
			assert.True(t, prompted[strings.TrimPrefix(v, "env://")],
				"%s points at %q, which is not declared as a prompted secret", e.Key, v)
		}
	}

	// And nothing anywhere in the file may look like key material.
	assert.Empty(t, credentialMarkersIn(bp),
		"render.yaml carries something that looks like a credential")
}

// credentialMarkersIn reports every value in the blueprint that looks like key
// material, as "<service>.<key>: <marker>".
//
// It is a function rather than three lines inside the test above because the
// scan was the finding. TestRender_NoSecretIsWrittenIntoTheBlueprint opened
// with "render.yaml is committed. Anything with a literal value in it is public
// to everyone who can read the repository" -- a claim about the FILE -- and
// then read `Services[0]` only. The static site's envVars were never looked at,
// so a live Stripe secret key written there, where a build-time variable is
// embedded in the bundle and served to every visitor, was committed and green
// (F-138).
//
// The early `continue` on a known-secret key went with it: a name being on the
// secret list is a reason to REQUIRE an env:// reference, not a reason to stop
// looking for key material under it.
//
// Sharing the function is what lets the audit reproduction run the real scan
// over an injected document instead of a copy of the scan, which is the only
// version of that test worth keeping: a copy passes forever once it is written,
// whatever the original does afterwards.
func credentialMarkersIn(bp renderBlueprint) []string {
	var found []string
	for _, svc := range bp.Services {
		for _, e := range svc.EnvVars {
			if e.Value == nil {
				continue
			}
			for _, marker := range []string{"sk_live_", "sk_test_", "whsec_", "-----BEGIN"} {
				if strings.Contains(*e.Value, marker) {
					found = append(found, svc.Name+"."+e.Key+": "+marker)
				}
			}
		}
	}
	return found
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
	//
	// NODAL_ALERT_WEBHOOK_URL and NODAL_PII_KEYRING are here because the
	// loader now RESOLVES the two references that name them rather than only
	// parsing them. Without that, a STAGING whose operator never set either
	// one loaded clean, served, alerted nobody and stored no personal data --
	// and this test was one of the three places that said otherwise (F-137).
	// NODAL_DB_MIGRATE_URL is deliberately NOT here: the blueprint no longer
	// hands the schema owner to the internet-facing service and nothing in
	// cmd/api ever read it (F-136).
	for k, v := range map[string]string{
		"NODAL_DB_APP_URL":            "postgresql://u:p@h/d?sslmode=require",
		"NODAL_DB_OPS_URL":            "postgresql://u:p@h/d?sslmode=require",
		"NODAL_OIDC_CLIENT_SECRET":    "stand-in",
		"NODAL_STRIPE_API_KEY":        "stand-in",
		"NODAL_STRIPE_WEBHOOK_SECRET": "stand-in",
		"NODAL_ALERT_WEBHOOK_URL":     "https://ntfy.sh/stand-in",
		"NODAL_PII_KEYRING":           `{"active":1,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}`,
	} {
		env[k] = v
	}

	cfg, err := config.Load(context.Background(), config.ServiceAPI, config.LookupFromMap(env))
	require.NoError(t, err, "the blueprint must be a configuration cmd/api can start with")

	// And the other direction: take either value away and it must NOT start.
	// A reference with nothing behind it is exactly what the deployment had.
	for _, name := range []string{"NODAL_ALERT_WEBHOOK_URL", "NODAL_PII_KEYRING"} {
		without := map[string]string{}
		for k, v := range env {
			if k != name {
				without[k] = v
			}
		}
		_, werr := config.Load(context.Background(), config.ServiceAPI, config.LookupFromMap(without))
		assert.Errorf(t, werr,
			"the blueprint loads with %s unset. render.yaml, HUMAN_ACTIONS_QUEUE.md items 1-2 and "+
				"MASTER_BUILD_STATE.md all say a deployment that forgets it does not boot", name)
	}

	// The references resolve to what the blueprint says they name, rather than
	// to whatever happened to be in the process environment.
	resolver := config.NewResolver(cfg.Env, config.LookupFromMap(env))
	for name, ref := range map[string]config.SecretRef{
		"CP_ALERT_WEBHOOK_URL": cfg.Alert.WebhookURL,
		"CP_PII_KEYRING_REF":   cfg.PII.Keyring,
	} {
		v, rerr := resolver.Resolve(context.Background(), ref)
		require.NoErrorf(t, rerr, "%s = %q does not resolve", name, string(ref))
		assert.NotEmptyf(t, v, "%s resolves to nothing", name)
	}
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
	// A sandbox tier (ADR-0023) constructs the sandbox payout provider in that
	// slot instead, and nothing else may name it: config refuses the
	// declaration in PROD and the adapter refuses to build there.
	sandboxTier := false
	for _, e := range svc.EnvVars {
		if e.Key == "CP_API_LEGAL_POLICY" && e.Value != nil && strings.EqualFold(*e.Value, "SANDBOX") {
			sandboxTier = true
		}
	}
	if sandboxTier {
		wantBySlot["CP_PROVIDER_PAYOUT_NAME"] = payoutsandbox.Name
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
		if v == payoutsandbox.Name {
			assert.True(t, sandboxTier, "%s names the sandbox payout provider on a deployment that is not a sandbox tier; it would refuse to build", key)
			assert.Equal(t, "CP_PROVIDER_PAYOUT_NAME", key, "the sandbox payout provider answers only the payout slot")
		}
	}
}

// The webhook path the contract publishes is the one the service registers
// (F-124).
//
// openapi.yaml declared `enum: [stripe]`; cmd/api registers the port under
// stripecredit.ProviderName, which is "stripe_credit". Nothing enforced the
// enum at runtime -- no OpenAPI request validator is wired anywhere in this
// repository -- so a delivery to the DOCUMENTED path reached the handler, found
// no provider in the map, and answered 404.
//
// Confirmed against the deployment on 2026-09-10: POST /v1/webhooks/stripe
// returns 404, byte-identical to POST /v1/webhooks/definitely-not-a-provider.
// Stripe retries a 404 for a few days and then gives up, which is exactly the
// failure cmd/api/wire.go's own comment says the mounting exists to avoid.
//
// Two strings that have to agree, in two languages, is the defect class this
// register keeps recording. This is the test that makes them agree.
// TestTheWebServiceIsNotGivenTheSchemaOwner: the credential that can turn off
// every trigger in this system must not be in the internet-facing process.
//
// Since 00743-00753 the state machines are enforced by triggers rather than by
// convention: the transition bindings, forbid_mutation on fifty-three
// append-only tables, and the eleven that write state columns the application
// cannot. `cp_migrate` owns those tables and an owner can
// `ALTER TABLE ... DISABLE TRIGGER`, so a compromise of the web process holding
// that credential would undo all of it in one statement.
//
// F-93 recorded that CP_DATABASE_MIGRATE_URL was declared unconditionally
// required, which forced the blueprint to supply it. Nothing that loads
// configuration reads it -- cmd/migrate takes it from the environment itself --
// so the requirement bought nothing and cost the credential.
//
// This asserts both halves: the blueprint does not hand it over, and the
// configuration does not ask cmd/api for it. Either alone would let the other
// drift back.
func TestTheWebServiceIsNotGivenTheSchemaOwner(t *testing.T) {
	t.Parallel()
	bp := loadBlueprint(t)

	// Any variable at all, under any name.
	//
	// The test used to name CP_DATABASE_MIGRATE_URL and nothing else, so the
	// blueprint handed the same credential over as the prompted secret
	// NODAL_DB_MIGRATE_URL -- a name the check did not know -- and stayed
	// green for as long as that was true. Nothing in cmd/api read it: the only
	// other references in the tree were two test stand-ins. The credential was
	// in the internet-facing container's environment for no purpose at all
	// (F-136). A name test is only as good as the names it knows, so this one
	// asks about the ROLE instead, on the key and on the value.
	for _, e := range bp.Services[0].EnvVars {
		assert.NotContainsf(t, strings.ToUpper(e.Key), "MIGRATE",
			"render.yaml declares %s on the internet-facing service. The schema owner can "+
				"ALTER TABLE ... DISABLE TRIGGER, and every state machine in this system is a trigger",
			e.Key)
		if e.Value == nil {
			continue
		}
		assert.NotContainsf(t, strings.ToLower(*e.Value), "cp_migrate",
			"%s carries the schema owner's role name in its value", e.Key)
		assert.NotContainsf(t, strings.ToUpper(*e.Value), "MIGRATE_URL",
			"%s references the schema owner's connection string", e.Key)
	}

	// And the configuration does not ask cmd/api for it, so the blueprint is not
	// the only thing standing between the web process and the owner credential.
	var found bool
	for _, v := range config.Vars() {
		if v.Name != "CP_DATABASE_MIGRATE_URL" {
			continue
		}
		found = true
		assert.Equalf(t, config.ServiceTooling, v.Svc,
			"CP_DATABASE_MIGRATE_URL is required of %q; it is the schema owner and only tooling may be asked for it", v.Svc)
		assert.True(t, v.Required, "it should still be required OF tooling; an operator running a migration needs it")
	}
	require.True(t, found, "CP_DATABASE_MIGRATE_URL is no longer declared at all; if it was deleted this test should be too")
}

func TestWebhookPathIsTheOneTheContractPublishes(t *testing.T) {
	t.Parallel()
	spec, err := os.ReadFile(filepath.Join("..", "..", "openapi", "openapi.yaml"))
	require.NoError(t, err)
	m := regexp.MustCompile(`name: provider,[^}]*enum: \[([^\]]+)\]`).FindSubmatch(spec)
	require.NotNil(t, m, "the provider path parameter's enum is no longer where this test looks for it")

	var declared []string
	for _, v := range strings.Split(string(m[1]), ",") {
		declared = append(declared, strings.TrimSpace(v))
	}
	sort.Strings(declared)

	// What the composition root actually registers. Read from source rather
	// than imported, because test/infra deliberately does not link cmd/api.
	wire, err := os.ReadFile(filepath.Join("..", "..", "internal", "provider", "stripecredit", "wire.go"))
	require.NoError(t, err)
	pm := regexp.MustCompile(`ProviderName\s*=\s*"([^"]+)"`).FindSubmatch(wire)
	require.NotNil(t, pm, "stripecredit.ProviderName is no longer a string constant")

	assert.Equal(t, []string{string(pm[1])}, declared,
		"the contract publishes a webhook provider the service does not register; a delivery to the "+
			"documented path answers 404 and the provider eventually gives up")
}

// ---- the static site ------------------------------------------------------

// staticSite is the second service, named so the helpers below can take it.
type staticSite = struct {
	Type              string   `yaml:"type"`
	Name              string   `yaml:"name"`
	Runtime           string   `yaml:"runtime"`
	Plan              string   `yaml:"plan"`
	Region            string   `yaml:"region"`
	DockerfilePath    string   `yaml:"dockerfilePath"`
	HealthCheckPath   string   `yaml:"healthCheckPath"`
	BuildCommand      string   `yaml:"buildCommand"`
	StaticPublishPath string   `yaml:"staticPublishPath"`
	Domains           []string `yaml:"domains"`
	EnvVars           []struct {
		Key   string  `yaml:"key"`
		Value *string `yaml:"value"`
		Sync  *bool   `yaml:"sync"`
	} `yaml:"envVars"`
	Routes []struct {
		Type        string `yaml:"type"`
		Source      string `yaml:"source"`
		Destination string `yaml:"destination"`
	} `yaml:"routes"`
	Headers []struct {
		Path  string `yaml:"path"`
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"headers"`
}

// siteHeaders indexes the site's `headers:` block by header name.
//
// Every assertion below exists because none of them did (F-138): renderBlueprint
// stopped at `envVars`, no other file under test/infra mentioned a header, and
// deleting render.yaml's whole `headers:`, `routes:` and `domains:` block left
// `go test ./test/infra/` green -- confirmed by doing it. test/deployed pins the
// API's headers, but only behind the `deployed` build tag and only against a
// live target, so nothing in the ordinary suite could tell.
func siteHeaders(t *testing.T, site staticSite) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, h := range site.Headers {
		require.Equal(t, "/*", h.Path,
			"%s is scoped to %q; a header that does not cover every path covers the one that matters least", h.Name, h.Path)
		require.NotContains(t, out, h.Name, "%s is declared twice", h.Name)
		out[h.Name] = h.Value
	}
	return out
}

// cspDirectives splits a policy into directive name -> source list.
func cspDirectives(t *testing.T, policy string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, part := range strings.Split(policy, ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		require.NotContains(t, out, fields[0],
			"%s is declared twice; the first wins and the second reads as if it did", fields[0])
		out[fields[0]] = fields[1:]
	}
	return out
}

// TestRender_TheStaticSiteIsBuiltAndPublishedFromThisRepository.
func TestRender_TheStaticSiteIsBuiltAndPublishedFromThisRepository(t *testing.T) {
	t.Parallel()
	site := loadBlueprint(t).Services[1]

	assert.Equal(t, "apps/web/dist", site.StaticPublishPath,
		"the published directory is not the one `pnpm --filter @controlplane/web build` writes")
	assert.Contains(t, site.BuildCommand, "pnpm install --frozen-lockfile",
		"a build that resolves its own dependency versions is not the build this repository tested")
	assert.Contains(t, site.BuildCommand, "generated-client",
		"the app imports the generated client; without generating it the build fails or ships a stale contract")
	assert.Contains(t, site.BuildCommand, "@controlplane/web build")
}

// TestRender_TheSPARewriteExists: every route is the single-page app, so an
// unknown path must reach index.html. Without it a deep link or a reload on
// /portfolio is a 404 from the CDN and the router never runs.
func TestRender_TheSPARewriteExists(t *testing.T) {
	t.Parallel()
	site := loadBlueprint(t).Services[1]

	require.Len(t, site.Routes, 1,
		"exactly one route: a second rule ahead of the catch-all is how a path stops reaching the app")
	assert.Equal(t, "rewrite", site.Routes[0].Type,
		"a redirect changes the address bar and loses the path the router was going to read")
	assert.Equal(t, "/*", site.Routes[0].Source)
	assert.Equal(t, "/index.html", site.Routes[0].Destination)
}

// TestRender_EveryServiceDeclaresItsOwnHostname, and the app and the API agree
// about which hostname is which.
func TestRender_EveryServiceDeclaresItsOwnHostname(t *testing.T) {
	t.Parallel()
	bp := loadBlueprint(t)
	api, site := bp.Services[0], bp.Services[1]

	apiEnv := map[string]string{}
	for _, e := range api.EnvVars {
		if e.Value != nil {
			apiEnv[e.Key] = *e.Value
		}
	}
	base := apiEnv["CP_HTTP_PUBLIC_BASE_URL"]
	require.NotEmpty(t, base)
	u, err := url.Parse(base)
	require.NoError(t, err)
	require.NotEmpty(t, u.Host)

	// The API declared no domains at all, while CP_HTTP_PUBLIC_BASE_URL,
	// CP_AUTH_REDIRECT_URL, CP_HTTP_CORS_ORIGINS, the app's VITE_API_ORIGIN and
	// the static site's CSP all named api-nodal.actorvia.xyz. A deployment
	// created from this file answers on *.onrender.com: the OIDC redirect_uri
	// does not match, the cookie is set on a host the app does not call, and
	// every request is a CORS failure. The live service had the domain attached
	// by hand, which is the drift the blueprint exists to remove (F-141).
	assert.Containsf(t, api.Domains, u.Host,
		"nodal-api claims %s in CP_HTTP_PUBLIC_BASE_URL and declares domains: %v", u.Host, api.Domains)

	require.NotEmpty(t, site.Domains,
		"the static site declares no hostname, so HUMAN_ACTIONS_QUEUE item 4 points at nothing")
	assert.NotContains(t, api.Domains, site.Domains[0], "the two services claim the same hostname")

	// And the app is built to call the API's hostname, not its own.
	var origin string
	for _, e := range site.EnvVars {
		if e.Key == "VITE_API_ORIGIN" && e.Value != nil {
			origin = *e.Value
		}
	}
	assert.Equal(t, "https://"+u.Host, origin, "the app is built to call an origin the API does not answer on")
	assert.Contains(t, apiEnv["CP_HTTP_CORS_ORIGINS"], "https://"+site.Domains[0],
		"the API does not allow the origin the app is served from; every request is a CORS failure")
}

// TestRender_TheStaticSitesSecurityHeadersAreWhatTheAppNeeds.
//
// Six headers, and each one's value rather than its presence. A header list
// that is only checked for names is a list a wrong value passes.
func TestRender_TheStaticSitesSecurityHeadersAreWhatTheAppNeeds(t *testing.T) {
	t.Parallel()
	site := loadBlueprint(t).Services[1]
	headers := siteHeaders(t, site)

	for _, name := range []string{
		"Content-Security-Policy",
		"X-Content-Type-Options",
		"X-Frame-Options",
		"Referrer-Policy",
		"Permissions-Policy",
		"Strict-Transport-Security",
	} {
		require.Containsf(t, headers, name,
			"the static site emits no %s; a separately hosted page gets none of the API's", name)
	}
	assert.Equal(t, "nosniff", headers["X-Content-Type-Options"])
	assert.Equal(t, "DENY", headers["X-Frame-Options"])
	assert.Equal(t, "strict-origin-when-cross-origin", headers["Referrer-Policy"])

	// HSTS for at least a year, which is the floor every preload list states.
	hsts := headers["Strict-Transport-Security"]
	m := regexp.MustCompile(`max-age=(\d+)`).FindStringSubmatch(hsts)
	require.NotNil(t, m, "Strict-Transport-Security carries no max-age: %q", hsts)
	age, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	assert.GreaterOrEqual(t, age, 31536000, "HSTS shorter than a year is a window an attacker only has to wait for")
	assert.Contains(t, hsts, "includeSubDomains")

	// Permissions-Policy: everything denied EXCEPT payment, which is delegated
	// to Stripe's own origin. `payment=()` denies the Payment Request API to
	// every frame including the js.stripe.com iframe, so the Payment Element's
	// wallet rows -- Apple Pay, Google Pay, Link -- vanish from the one page
	// this tier exists to exercise (F-150).
	pp := headers["Permissions-Policy"]
	for _, denied := range []string{"camera=()", "microphone=()", "geolocation=()", "usb=()", "interest-cohort=()"} {
		assert.Containsf(t, pp, denied, "Permissions-Policy does not deny %s", denied)
	}
	assert.Contains(t, pp, `payment=(self "https://js.stripe.com")`,
		"payment is denied outright, which takes the wallets out of Stripe's Payment Element")

	// ---- the policy itself ------------------------------------------------
	csp := cspDirectives(t, headers["Content-Security-Policy"])

	apiOrigin := ""
	for _, e := range site.EnvVars {
		if e.Key == "VITE_API_ORIGIN" && e.Value != nil {
			apiOrigin = *e.Value
		}
	}
	require.NotEmpty(t, apiOrigin)

	// connect-src is EXACTLY the three origins the app talks to. An extra one
	// here is an exfiltration destination, and a missing one is a dead page.
	assert.ElementsMatch(t, []string{"'self'", apiOrigin, "https://api.stripe.com"}, csp["connect-src"],
		"connect-src is not exactly self + the API + Stripe's API")

	assert.Equal(t, []string{"'none'"}, csp["frame-ancestors"],
		"the app may be framed, so a clickjacked confirm is a confirm")
	assert.Equal(t, []string{"'none'"}, csp["object-src"])
	assert.Equal(t, []string{"'self'"}, csp["base-uri"],
		"a <base> tag could re-point every relative URL in the document")
	assert.Equal(t, []string{"'self'"}, csp["default-src"])
	assert.Equal(t, []string{"'self'", "https://js.stripe.com"}, csp["script-src"],
		"script-src is not exactly self + Stripe.js")
	assert.NotContains(t, csp["script-src"], "'unsafe-inline'", "an injected <script> would execute")
	assert.NotContains(t, csp["script-src"], "'unsafe-eval'")

	// style-src admits 'unsafe-inline', and that is D-087 rather than an
	// oversight. CSP Level 3 governs a `style=` ATTRIBUTE with style-src-attr,
	// which falls back to style-src when absent, so `style-src 'self'` refused
	// every inline style attribute in the bundle -- including SegmentedBar's,
	// which is the width of each segment of the available/reserved/pending
	// balance bar. Blocked, the bar rendered empty while its accessible name
	// still stated the shares (F-139).
	require.Contains(t, csp, "style-src")
	assert.Contains(t, csp["style-src"], "'unsafe-inline'",
		"the app ships inline style attributes and this policy refuses them")
	assert.Contains(t, csp["style-src"], "'self'")

	// The relaxation is bounded by what surrounds it, so the boundary is
	// asserted rather than described: no inline script, and the directives that
	// keep an injected style from becoming navigation are above.
	assert.NotContains(t, csp, "style-src-attr",
		"a style-src-attr here is ignored by browsers that do not implement it, which then fall back to style-src")
}

// TestRender_TheStripeKeyAndTheStripeAccountAreTheSameAccount.
//
// A publishable key embeds the account it belongs to. The API refuses to sell
// Credits if its SECRET key answers for a different account than
// CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF names -- and nothing compared the key
// the BROWSER is given against the same value. A mismatch there is silent in
// exactly the way that finding describes.
func TestRender_TheStripeKeyAndTheStripeAccountAreTheSameAccount(t *testing.T) {
	t.Parallel()
	bp := loadBlueprint(t)

	var account, mode string
	for _, e := range bp.Services[0].EnvVars {
		if e.Value == nil {
			continue
		}
		switch e.Key {
		case "CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF":
			account = *e.Value
		case "CP_PROVIDER_CREDIT_PURCHASE_MODE":
			mode = *e.Value
		}
	}
	require.NotEmpty(t, account, "the API names no Stripe account, so the adapter cannot refuse a rotated key")
	require.True(t, strings.HasPrefix(account, "acct_"),
		"CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF is not an account id: %q", account)

	var key string
	for _, e := range bp.Services[1].EnvVars {
		if e.Key == "VITE_STRIPE_PUBLISHABLE_KEY" && e.Value != nil {
			key = *e.Value
		}
	}
	require.NotEmpty(t, key, "the app is given no publishable key, so Buy Credits renders 'Payments unavailable'")
	require.Regexp(t, `^pk_(test|live)_`, key, "a key that is neither pk_test_ nor pk_live_ is refused by the app")

	// The account id, with its "acct_" prefix removed, is embedded verbatim in
	// the publishable key.
	assert.Containsf(t, key, strings.TrimPrefix(account, "acct_"),
		"VITE_STRIPE_PUBLISHABLE_KEY belongs to a different Stripe account than "+
			"CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF (%s) names", account)

	// And the mode matches: a live key here would pair with a sandbox API.
	if mode == "live" {
		assert.True(t, strings.HasPrefix(key, "pk_live_"), "a test publishable key on a live provider")
	} else {
		assert.True(t, strings.HasPrefix(key, "pk_test_"), "a live publishable key on a %s provider", mode)
	}
}

// TestRender_EveryCapabilityNamedInTheBlueprintExists (F-145).
//
// CP_API_ENABLED_CAPABILITIES is condition 1 of the policy authority: a
// capability absent from it is inactive whatever its gate row says. One
// misspelled letter therefore turns a capability off, and cmd/api's
// parseCapabilities used to drop the name without a word -- a healthy service
// and a 200 on every probe, with the Credit purchase path dark. The names come
// from internal/gates rather than a list written here, so a renamed capability
// fails to compile instead of failing quietly in a deployment.
func TestRender_EveryCapabilityNamedInTheBlueprintExists(t *testing.T) {
	t.Parallel()
	svc := loadBlueprint(t).Services[0]

	checked := 0
	for _, e := range svc.EnvVars {
		if e.Value == nil || (e.Key != "CP_API_ENABLED_CAPABILITIES" && e.Key != "CP_API_SANDBOX_GATES") {
			continue
		}
		for _, raw := range strings.Split(*e.Value, ",") {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			checked++
			assert.Truef(t, gates.Capability(name).Valid(),
				"%s names %q, which is not a declared capability. A name nothing answers to is dropped "+
					"from condition 1 of the policy authority, so the capability is off with a healthy "+
					"service and a 200 on every health check", e.Key, name)
		}
	}
	assert.Positive(t, checked, "the blueprint names no capability at all, which disables every one of them")
}
