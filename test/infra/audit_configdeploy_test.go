package infra

// Adversarial audit of the config/deploy surface (goal Section 54).
//
// Every test in this file is a REPRODUCTION of a finding, not a proposed
// invariant. Each one fails on `productization` at 2592b11. Nothing here
// changes product code; the fixes belong to whoever the orchestrator assigns
// them to.
//
// The findings, in the order they appear below:
//
//	F-cfg-1  the blueprint hands the internet-facing API service the
//	         schema-owner DSN under the name NODAL_DB_MIGRATE_URL, which the
//	         existing test does not look for
//	F-cfg-2  RuleAlertDestination and RulePIIKeyring are satisfied by the
//	         env:// REFERENCE, so a STAGING deployment whose dashboard secret
//	         is missing boots, serves, alerts nobody and stores no personal
//	         data -- the opposite of what render.yaml, MASTER_BUILD_STATE.md
//	         and HUMAN_ACTIONS_QUEUE.md items 1 and 2 all claim
//	F-cfg-3  the blueprint's secret scan reads Services[0] only, so anything
//	         at all may be written into the static site's environment
//	F-cfg-4  nothing pins the static site's security headers, its SPA rewrite
//	         or its custom domain: the whole block can be deleted and every
//	         test stays green
//	F-cfg-5  style-src 'self' blocks the two inline style attributes the app
//	         ships, one of which is the width of every segment of the
//	         available/reserved/pending balance bar
//	F-cfg-6  the API service declares no custom domain, while every other
//	         value in the file and in the app assumes api-nodal.actorvia.xyz
//	F-cfg-7  no VERSION build arg reaches build/Dockerfile, so /v1/version
//	         reports build_version "dev" for every build ever deployed
//	F-cfg-8  EventSource is constructed without withCredentials, so the live
//	         stream carries no session cookie on the deployed cross-origin
//	         topology

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"

	"github.com/nodal/controlplane/internal/config"
)

// auditBlueprint is the shape this file needs. renderBlueprint (render_test.go)
// deliberately stops at envVars; the static site's headers, routes, domains and
// build command are not in it at all, which is F-cfg-4.
type auditBlueprint struct {
	Services []struct {
		Type              string   `yaml:"type"`
		Name              string   `yaml:"name"`
		Runtime           string   `yaml:"runtime"`
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

func auditLoadBlueprint(t *testing.T) auditBlueprint {
	t.Helper()
	b, err := os.ReadFile("../../render.yaml")
	require.NoError(t, err)
	var bp auditBlueprint
	require.NoError(t, yaml.Unmarshal(b, &bp))
	require.Len(t, bp.Services, 2)
	return bp
}

// auditLoadRenderBlueprint loads the committed file into the shape render_test.go
// uses, so the shared scan can run over it.
func auditLoadRenderBlueprint(t *testing.T) renderBlueprint {
	t.Helper()
	b, err := os.ReadFile("../../render.yaml")
	require.NoError(t, err)
	var bp renderBlueprint
	require.NoError(t, yaml.Unmarshal(b, &bp))
	return bp
}

func auditBlueprintText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../render.yaml")
	require.NoError(t, err)
	return string(b)
}

// ---------------------------------------------------------------- F-cfg-1 ---

// The credential that can `ALTER TABLE ... DISABLE TRIGGER` must not be in the
// internet-facing process, and render.yaml puts it there.
//
// CP_DATABASE_MIGRATE_URL is deliberately absent (render.yaml:217-228) and
// TestTheWebServiceIsNotGivenTheSchemaOwner asserts that absence. But the
// blueprint still declares `NODAL_DB_MIGRATE_URL` as a prompted secret on
// nodal-api (render.yaml:60-61), so Render puts the cp_migrate DSN -- password
// included -- into the API container's environment. Nothing reads it: a
// repository-wide grep finds the name only in render.yaml and in two tests that
// supply a stand-in. The control is documented, tested, and not in force.
func TestAuditConfigDeploy_TheAPIServiceIsNotHandedTheSchemaOwnerDSN(t *testing.T) {
	t.Parallel()
	api := auditLoadBlueprint(t).Services[0]
	require.Equal(t, "nodal-api", api.Name)

	for _, e := range api.EnvVars {
		assert.NotContainsf(t, strings.ToUpper(e.Key), "MIGRATE",
			"render.yaml declares %s on the internet-facing service. It is the schema owner's DSN; "+
				"an owner can DISABLE TRIGGER, and every state machine in this system is a trigger. "+
				"Nothing in cmd/api reads it -- the only other references in the tree are two test "+
				"stand-ins -- so the credential is in the process's environment for no purpose at all.",
			e.Key)
	}
}

// ---------------------------------------------------------------- F-cfg-2 ---

// A STAGING deployment boots with no alert destination and no PII keyring.
//
// render.yaml:327-329 says of CP_ALERT_WEBHOOK_URL: "config.Validate refuses to
// start in STAGING or PROD when it is empty, so a deployment that forgets it
// does not boot silently unalerted -- it does not boot." render.yaml:352-355
// says the same of the keyring. HUMAN_ACTIONS_QUEUE.md items 1 and 2 repeat it,
// and MASTER_BUILD_STATE.md says the deploy "refuses to start without them, by
// design."
//
// None of that is true. The blueprint sets CP_ALERT_WEBHOOK_URL to the literal
// string "env://NODAL_ALERT_WEBHOOK_URL", which is never empty. RuleAlertDestination
// and RulePIIKeyring see a non-zero SecretRef and pass. The value behind the
// reference is only read at composition time, where cmd/api/alerts.go:59 and
// cmd/api/pii.go:30 log at ERROR and return nil -- deliberately, so an
// unresolvable secret does not cause an outage. The two decisions are each
// defensible and together they mean an operator who skips HUMAN_ACTIONS_QUEUE
// items 1 and 2 gets a healthy, serving STAGING in which a ledger-integrity
// violation reaches nobody (the F-118 state) and no verified e-mail is ever
// stored (the F-47 state).
func TestAuditConfigDeploy_STAGINGRefusesToBootWithoutTheAlertDestinationAndKeyring(t *testing.T) {
	t.Parallel()
	api := auditLoadBlueprint(t).Services[0]

	// Exactly what Render hands the process when the operator has set the
	// database and OIDC secrets but not the two the queue is still waiting on.
	env := map[string]string{}
	for _, e := range api.EnvVars {
		if e.Value != nil {
			env[e.Key] = *e.Value
		}
	}
	for k, v := range map[string]string{
		"NODAL_DB_APP_URL":            "postgresql://u:p@h/d?sslmode=require",
		"NODAL_DB_OPS_URL":            "postgresql://u:p@h/d?sslmode=require",
		"NODAL_OIDC_CLIENT_SECRET":    "stand-in",
		"NODAL_STRIPE_API_KEY":        "stand-in",
		"NODAL_STRIPE_WEBHOOK_SECRET": "stand-in",
	} {
		env[k] = v
	}
	// NODAL_ALERT_WEBHOOK_URL and NODAL_PII_KEYRING are deliberately absent.
	lookup := config.LookupFromMap(env)

	cfg, err := config.Load(context.Background(), config.ServiceAPI, lookup)
	require.Equal(t, "STAGING", env["CP_ENV"], "the blueprint's own environment")

	// Half one: the configuration is accepted.
	assert.Error(t, err,
		"config.Load accepted a STAGING configuration with no alert destination and no PII keyring "+
			"behind its references. render.yaml, MASTER_BUILD_STATE.md and HUMAN_ACTIONS_QUEUE.md all "+
			"state that this cannot boot. It boots.")
	if err != nil {
		return
	}

	// Half two: and the references resolve to nothing, which is what the
	// composition root then degrades on.
	resolver := config.NewResolver(cfg.Env, lookup)
	for _, tc := range []struct {
		name string
		ref  config.SecretRef
	}{
		{"CP_ALERT_WEBHOOK_URL", cfg.Alert.WebhookURL},
		{"CP_PII_KEYRING_REF", cfg.PII.Keyring},
	} {
		_, rerr := resolver.Resolve(context.Background(), tc.ref)
		assert.NoErrorf(t, rerr,
			"%s = %q resolves to nothing. cmd/api logs an ERROR and serves without it: alerts go "+
				"to the log stream only, and personal data is discarded rather than stored.",
			tc.name, string(tc.ref))
	}
}

// ---------------------------------------------------------------- F-cfg-3 ---

// The blueprint's secret scan reads one service out of two.
//
// TestRender_NoSecretIsWrittenIntoTheBlueprint opens with "render.yaml is
// committed. Anything with a literal value in it is public to everyone who can
// read the repository" -- a claim about the FILE -- and then scans
// loadBlueprint(t).Services[0] only. The static site's envVars are never looked
// at, so a live secret key written there is committed and green.
//
// The reproduction injects a live-looking Stripe secret key into the static
// site's env block and runs the REAL scan over the result.
//
// It used to replay the scan's logic locally, which was the right way to
// DEMONSTRATE the finding and the wrong thing to leave behind: a copy of a
// scan passes forever once it is written, whatever the original does
// afterwards. credentialMarkersIn (render_test.go) is the scan
// TestRender_NoSecretIsWrittenIntoTheBlueprint actually uses, so this now fails
// again the day that one narrows.
func TestAuditConfigDeploy_TheSecretScanCoversEveryServiceInTheBlueprint(t *testing.T) {
	t.Parallel()

	raw := auditBlueprintText(t)
	anchor := "      - key: VITE_API_ORIGIN\n"
	require.Contains(t, raw, anchor, "the static site's env block moved; re-anchor this test")
	injected := strings.Replace(raw, anchor,
		"      - key: STRIPE_SECRET_KEY\n        value: sk_live_AUDITINJECTEDNOTAREALKEY0000\n"+anchor, 1)

	var bp renderBlueprint
	require.NoError(t, yaml.Unmarshal([]byte(injected), &bp))
	require.Len(t, bp.Services, 2)

	found := credentialMarkersIn(bp)
	assert.NotEmpty(t, found,
		"a live Stripe secret key written into the static site's environment passes "+
			"TestRender_NoSecretIsWrittenIntoTheBlueprint. That test opens by claiming something about "+
			"the FILE and read Services[0] only; a build-time variable on the static site is embedded "+
			"in the bundle and served to every visitor.")
	assert.Contains(t, strings.Join(found, " "), "nodal-web",
		"the scan found a marker, but not the one injected into the second service")

	// The control: the committed file is clean. The publishable key on the
	// static site is pk_test_, which is public by design and is not a marker.
	assert.Empty(t, credentialMarkersIn(auditLoadRenderBlueprint(t)),
		"render.yaml as committed carries something that looks like a credential")
}

// ---------------------------------------------------------------- F-cfg-4 ---

// Nothing in the repository pins the static site's security headers.
//
// The CSP, HSTS, X-Frame-Options, Referrer-Policy, Permissions-Policy,
// X-Content-Type-Options, the SPA rewrite and the custom domain are all
// declared in render.yaml and asserted nowhere: renderBlueprint does not parse
// `headers`, `routes` or `domains`, and no other file under test/infra mentions
// them. Deleting the entire block leaves `go test ./test/infra/` green -- run
// and confirmed.
//
// test/deployed/surface_test.go pins headers, but only the API's, only behind
// the `deployed` build tag, and only against a live target.
func TestAuditConfigDeploy_TheStaticSitesSecurityHeadersArePinnedSomewhere(t *testing.T) {
	t.Parallel()

	// The blueprint does declare them.
	site := auditLoadBlueprint(t).Services[1]
	require.NotEmpty(t, site.Headers, "the static site's headers block is gone")
	require.NotEmpty(t, site.Routes, "the static site's SPA rewrite is gone")

	var mentions []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.Contains(filepath.Base(path), "audit_") {
			return nil // this file
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // G304: walking the test tree
		if rerr != nil {
			return rerr
		}
		s := string(b)
		// A test that does not read the build tag still counts against us if it
		// pins the site's headers, so `deployed` is excluded by hand below.
		if strings.Contains(s, "//go:build deployed") {
			return nil
		}
		if strings.Contains(s, "Content-Security-Policy") || strings.Contains(s, "frame-ancestors") {
			mentions = append(mentions, path)
		}
		return nil
	})
	require.NoError(t, err)

	assert.NotEmpty(t, mentions,
		"no test that runs without a live deployment asserts anything about the static site's "+
			"Content-Security-Policy. Deleting render.yaml's whole `headers:`, `routes:` and "+
			"`domains:` block leaves `go test ./test/infra/` green; confirmed by doing it.")
}

// ---------------------------------------------------------------- F-cfg-5 ---

// style-src 'self' blocks the inline style attributes the app ships.
//
// CSP Level 3 governs `style=` attributes with style-src-attr, which falls back
// to style-src when it is absent. The deployed policy declares
// `style-src 'self'` and no style-src-attr, so every inline style attribute in
// the bundle is refused by the browser.
//
// The app ships two. apps/web/src/components/Skeleton.tsx:45 sets a loading
// placeholder's width. apps/web/src/components/SegmentedBar.tsx:105 sets
// `flexGrow` from the exact base-unit string of each segment -- and
// SegmentedBar is the available / reserved / pending balance bar its own file
// header calls "the central information-design problem on Home". With the
// attribute blocked every segment keeps flex-grow: 0 and the bar renders empty,
// while the element's aria-label still reads "Available now, about 62%; ...".
//
// (Stripe.js injects its own <style> element into the document head for the
// Payment Element's container; that is a third-party behaviour this test does
// not assert, but it is governed by the same directive.)
func TestAuditConfigDeploy_TheCSPPermitsTheInlineStylesTheAppShips(t *testing.T) {
	t.Parallel()

	csp := ""
	for _, h := range auditLoadBlueprint(t).Services[1].Headers {
		if h.Name == "Content-Security-Policy" {
			csp = h.Value
		}
	}
	require.NotEmpty(t, csp, "the static site declares no CSP")

	directive := func(name string) string {
		for _, part := range strings.Split(csp, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, name+" ") || part == name {
				return part
			}
		}
		return ""
	}
	styleSrc := directive("style-src")
	if attr := directive("style-src-attr"); attr != "" {
		styleSrc = attr
	}
	require.NotEmpty(t, styleSrc, "no style-src; default-src governs and must be checked instead")

	var offenders []string
	err := filepath.WalkDir("../../apps/web/src", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".tsx") || strings.Contains(path, ".test.") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // G304: walking the app source
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "style={{") {
				offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(i+1))
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, offenders, "no inline style attributes remain; this finding is closed")

	assert.Contains(t, styleSrc, "'unsafe-inline'",
		"the deployed policy is %q, and these inline style attributes are refused under it: %v. "+
			"SegmentedBar's is the width of every segment of the available/reserved/pending balance "+
			"bar; blocked, the bar renders empty while its accessible name still states the shares.",
		styleSrc, offenders)
}

// ---------------------------------------------------------------- F-cfg-6 ---

// The API service declares no custom domain.
//
// The static site declares `domains: [app-nodal.actorvia.xyz]`; nodal-api
// declares none, while CP_HTTP_PUBLIC_BASE_URL, CP_AUTH_REDIRECT_URL,
// CP_AUTH_POST_LOGIN_URL, CP_HTTP_CORS_ORIGINS, the app's VITE_API_ORIGIN and
// the static site's CSP connect-src and form-action all name
// api-nodal.actorvia.xyz. A deployment created from this blueprint answers on
// *.onrender.com: the OIDC redirect_uri does not match, the cookie is set on a
// host the app does not call, and every request is a CORS failure.
//
// The live service has the domain attached by hand, which is exactly the drift
// the blueprint exists to remove. HUMAN_ACTIONS_QUEUE.md item 4 covers the
// app's CNAME and explicitly says "Do not touch api-nodal", so re-creating the
// deployment from the file plus the queue reproduces none of it.
func TestAuditConfigDeploy_TheAPIServiceDeclaresItsOwnHostname(t *testing.T) {
	t.Parallel()
	bp := auditLoadBlueprint(t)
	api, site := bp.Services[0], bp.Services[1]

	var base string
	for _, e := range api.EnvVars {
		if e.Key == "CP_HTTP_PUBLIC_BASE_URL" && e.Value != nil {
			base = *e.Value
		}
	}
	require.NotEmpty(t, base)
	host := strings.TrimPrefix(base, "https://")

	require.NotEmpty(t, site.Domains, "the static site declares its domain, so the field is supported here")
	assert.Contains(t, api.Domains, host,
		"nodal-api claims %s in CP_HTTP_PUBLIC_BASE_URL, CP_AUTH_REDIRECT_URL, CP_HTTP_CORS_ORIGINS "+
			"and in the static site's CSP, and declares no domains: at all. A blueprint sync gives it "+
			"an *.onrender.com hostname and nothing in the product can reach it.", host)
}

// ---------------------------------------------------------------- F-cfg-7 ---

// /v1/version cannot say which build is running.
//
// build/Dockerfile stamps config.BuildVersion from the VERSION build arg and
// defaults it to "dev" (build/Dockerfile:15,36). Render passes a service's
// environment variables to the docker build as build args -- which is the sole
// reason CMD is in the file (render.yaml:46-51) -- and no VERSION is declared.
// So every image ever built from this blueprint reports build_version "dev",
// and because BuildVersion is part of Config.Hash, the configuration hash
// cannot distinguish two different builds of the same configuration either.
//
// HUMAN_ACTIONS_QUEUE.md item 3 says the push "unblocks live verification of
// ... /v1/version (build commit 9906c9f, config hash)". It will report "dev".
func TestAuditConfigDeploy_TheBuildStampsAVersion(t *testing.T) {
	t.Parallel()

	b, err := os.ReadFile("../../build/Dockerfile")
	require.NoError(t, err)
	dockerfile := string(b)

	// The link-time stamp still comes from the VERSION build arg, which is what
	// makes everything below about the arg rather than about the ldflags.
	require.Contains(t, dockerfile, "config.BuildVersion=${VERSION}",
		"the Dockerfile no longer stamps config.BuildVersion from VERSION; re-read this finding")

	// The finding: `ARG VERSION=dev` with nothing supplying VERSION meant every
	// image ever built from this blueprint reported build_version "dev".
	assert.NotRegexp(t, regexp.MustCompile(`(?m)^ARG VERSION=dev\s*$`), dockerfile,
		"VERSION still defaults to the constant \"dev\", and render.yaml supplies none, so "+
			"config.BuildVersion is the literal \"dev\" in every deployed image. /v1/version -- the "+
			"endpoint whose whole purpose is to prove which build and which configuration are running "+
			"-- reports \"dev\", and the config hash it reports alongside includes that same constant.")

	// What replaced it (D-088). Render passes a service's environment variables
	// to `docker build` as build args -- which is the sole reason CMD is in
	// render.yaml -- and sets RENDER_GIT_COMMIT itself, at build time and at run
	// time. So the default is the commit when Render is the builder and "dev"
	// everywhere else, and nothing new joins the configuration table.
	//
	// Both stages, because an ARG declared inside a stage is scoped to it: the
	// builder stamps the binary and the runtime stage writes the OCI version
	// label, and a label that says "dev" over a binary that says the commit is
	// the same defect wearing a smaller hat.
	assert.Equal(t, 2, strings.Count(dockerfile, "ARG RENDER_GIT_COMMIT="),
		"RENDER_GIT_COMMIT must be declared in both stages")
	assert.Equal(t, 2, strings.Count(dockerfile, "ARG VERSION=${RENDER_GIT_COMMIT:-dev}"),
		"VERSION must derive from RENDER_GIT_COMMIT in both stages")

	// And the blueprint still declares CMD, which is the mechanism this relies
	// on: if Render stopped passing environment variables as build args, CMD
	// would stop reaching the build too and the image would not build at all --
	// a loud failure rather than a silent "dev".
	var cmd bool
	for _, e := range auditLoadBlueprint(t).Services[0].EnvVars {
		if e.Key == "CMD" {
			cmd = true
		}
	}
	assert.True(t, cmd, "render.yaml no longer passes CMD as a build arg; the VERSION default relies on the same mechanism")
}

// ---------------------------------------------------------------- F-cfg-8 ---

// The live event stream carries no session on the deployed topology.
//
// EventSource's credentials mode is "same-origin" unless withCredentials is
// set. The deployed app is app-nodal.actorvia.xyz and the API is
// api-nodal.actorvia.xyz: same SITE, different ORIGIN. So the session cookie is
// not attached, and GetEventsStream requires PermAccountRead
// (internal/httpapi/authz.go:229).
//
// Everything else in the app is fine -- the generated client sets
// credentials: "include" and probeReady deliberately omits them -- which is why
// this is the one request that breaks. It cannot be caught by the browser suite
// either: vite.config.ts proxies /v1 so the tests run same-origin.
func TestAuditConfigDeploy_TheEventStreamCarriesTheSessionCrossOrigin(t *testing.T) {
	t.Parallel()

	var origin, siteDomain string
	bp := auditLoadBlueprint(t)
	for _, e := range bp.Services[1].EnvVars {
		if e.Key == "VITE_API_ORIGIN" && e.Value != nil {
			origin = *e.Value
		}
	}
	require.NotEmpty(t, origin)
	require.NotEmpty(t, bp.Services[1].Domains)
	siteDomain = bp.Services[1].Domains[0]
	require.NotEqual(t, "https://"+siteDomain, origin,
		"the deployed topology is no longer cross-origin; this finding is closed")

	src, err := os.ReadFile("../../apps/web/src/components/StreamStatus.tsx")
	require.NoError(t, err)
	require.Contains(t, string(src), "new EventSource(",
		"StreamStatus no longer constructs an EventSource; re-read this finding")

	assert.Truef(t, strings.Contains(string(src), "withCredentials"),
		"StreamStatus.tsx constructs `new EventSource(`${API_BASE}/events/stream`)` with no options. "+
			"API_BASE is %s/v1, a different origin from %s, so EventSource's default credentials mode "+
			"(\"same-origin\") sends no cookie and GetEventsStream -- which requires PermAccountRead -- "+
			"refuses it. The bell, the activity feed and every stream-driven invalidation are dead on "+
			"the deployed tier; the badge says \"reconnecting\" forever.",
		origin, siteDomain)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ---------------------------------------------------------------- F-cfg-9 ---

// CP_SEED_ENABLED is declared, validated, set to false in the blueprint, and
// read by nothing.
//
// internal/config declares it required, documents it as "Allow seeding
// clearly-labeled fake users/assets/balances. Must be false in STAGING/PROD",
// and RuleNoSeed enforces that. render.yaml sets it to "false". Then
// cmd/api/marketsurfaces.go seeds the sandbox demo catalogue on every boot,
// keyed on cfg.SandboxTier() alone -- its own docstring says "when the
// deployment is a sandbox tier AND ASKS FOR IT", and there is no asking.
// c.Seed.Enabled is read by internal/config's own validator and by nothing
// else in cmd/ or internal/.
//
// So the deployed STAGING declares that it does not seed and seeds: eight demo
// markets, a demo Credit balance, and an activity feed built out of them.
// Nothing real is at risk -- the rows are SANDBOX-labelled and migration 00774
// forbids a PROD one -- but a stated control is not in force, and the one
// variable an operator would reach for to turn seeding off does nothing.
func TestAuditConfigDeploy_SeedingHonoursTheVariableThatForbidsIt(t *testing.T) {
	t.Parallel()

	// readersOf walks the source tree for files outside internal/config -- where
	// the variable is declared and validated -- that read a configured value.
	//
	// The original walked cmd/ and internal/. scripts/ is here because that is
	// where the readers turned out to belong: CP_SEED_ENABLED governs the
	// developer seed scripts and nothing else, since RuleNoSeed forbids it in
	// STAGING and PROD and so it could never have been the deployed tier's
	// control. The assertion is the one the finding made -- the switch is read
	// by something that can act on it -- and the demo catalogue's own switch is
	// asserted alongside it below, which the finding could not do because there
	// was none.
	readersOf := func(needles ...string) []string {
		var readers []string
		for _, root := range []string{"../../cmd", "../../internal", "../../scripts"} {
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return err
				}
				if strings.Contains(filepath.ToSlash(path), "internal/config/") {
					return nil // where it is declared and validated
				}
				b, rerr := os.ReadFile(path) //nolint:gosec // G304: walking the source tree
				if rerr != nil {
					return rerr
				}
				for _, needle := range needles {
					if strings.Contains(string(b), needle) {
						readers = append(readers, filepath.ToSlash(path))
						return nil
					}
				}
				return nil
			})
			require.NoError(t, err)
		}
		return readers
	}

	env := map[string]string{}
	for _, e := range auditLoadBlueprint(t).Services[0].EnvVars {
		if e.Value != nil {
			env[e.Key] = *e.Value
		}
	}

	// The blueprint still forbids the developer seed scripts.
	require.Equal(t, "false", env["CP_SEED_ENABLED"], "the blueprint no longer forbids seeding; re-read this finding")

	assert.NotEmpty(t, readersOf("SeedScriptsAllowed", "Seed.Enabled"),
		"CP_SEED_ENABLED is required of every binary, validated by RuleNoSeed, set to \"false\" in "+
			"render.yaml -- and read by nothing outside internal/config. An operator who reached for "+
			"the one variable that says whether fake data may be written changed nothing at all.")

	// And the thing that WAS seeding has a declared control that something
	// reads (D-086). cmd/api/marketsurfaces.go used to seed the demo catalogue
	// on cfg.SandboxTier() alone -- its own docstring said "when the deployment
	// is a sandbox tier AND ASKS FOR IT", and there was no asking -- so the
	// deployed STAGING declared that it does not seed and seeded.
	assert.NotEmpty(t, readersOf("API.DemoData"),
		"the sandbox demo catalogue still has no switch: eight demo markets, a demo Credit balance "+
			"and an activity feed built out of them are loaded on every boot with nothing able to stop it")
	demo, declared := env["CP_API_DEMO_DATA"]
	require.True(t, declared,
		"the blueprint does not say whether this deployment seeds demo data, so the answer is the "+
			"variable's default and nobody decided it")
	assert.Equal(t, "true", demo,
		"STAGING is the rehearsal this catalogue exists for; if it is genuinely not wanted, say so "+
			"here and delete this assertion with it")
}

// --------------------------------------------------------------- F-cfg-10 ---

// A misspelled capability passes every configuration check and is dropped in
// silence.
//
// config.Config's own comment on SandboxGates says "each must be a declared
// capability and must also be in EnabledCapabilities". Validate checks the
// second half only: it compares the sandbox list against the enabled list and
// never asks internal/gates whether either name exists. cmd/api then does two
// different things with the same mistake -- parseCapabilities (wire.go:860)
// skips an invalid name without a word, and sandboxGatesAtBoot
// (sandboxtier.go:139) returns an error and refuses to start.
//
// The consequence of the first is the one that matters: CP_API_ENABLED_CAPABILITIES
// is condition 1 of the policy authority, so a typo silently makes a capability
// inactive whatever its gate row says, with a healthy service and a 200 on
// every health check. That is the defect class F-124 is in the register for.
func TestAuditConfigDeploy_AMisspelledCapabilityIsRefusedByTheConfiguration(t *testing.T) {
	t.Parallel()

	env := map[string]string{}
	for _, e := range auditLoadBlueprint(t).Services[0].EnvVars {
		if e.Value != nil {
			env[e.Key] = *e.Value
		}
	}
	for k, v := range map[string]string{
		"NODAL_DB_APP_URL":            "postgresql://u:p@h/d?sslmode=require",
		"NODAL_DB_OPS_URL":            "postgresql://u:p@h/d?sslmode=require",
		"NODAL_OIDC_CLIENT_SECRET":    "stand-in",
		"NODAL_STRIPE_API_KEY":        "stand-in",
		"NODAL_STRIPE_WEBHOOK_SECRET": "stand-in",
		"NODAL_ALERT_WEBHOOK_URL":     "https://ntfy.sh/x",
		"NODAL_PII_KEYRING":           `{"active":1,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}`,
	} {
		env[k] = v
	}
	// One letter, in the list that decides which capabilities this deployment
	// is for. CREDIT_PURCHASE is dropped; PAYOUT_SETTLE is fine.
	env["CP_API_ENABLED_CAPABILITIES"] = "CREDIT_PURCHASEE,NATIVE_ASSET_CREATION,NATIVE_MARKET_TRADING,MARKETPLACE,PAYOUT_RESERVE,PAYOUT_SETTLE"
	env["CP_API_SANDBOX_GATES"] = "NATIVE_ASSET_CREATION"

	_, err := config.Load(context.Background(), config.ServiceAPI, config.LookupFromMap(env))
	assert.Error(t, err,
		"CP_API_ENABLED_CAPABILITIES=CREDIT_PURCHASEE loads clean. cmd/api's parseCapabilities "+
			"drops the name without a word, so the capability that decides whether this deployment "+
			"may sell Credits -- condition 1 of the policy authority -- is off, with a healthy "+
			"service and a 200 on every probe. config.Config's own comment says each name \"must be "+
			"a declared capability\"; nothing checks it.")
}
