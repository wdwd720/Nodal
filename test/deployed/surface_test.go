//go:build deployed

package deployed

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"

	"github.com/nodal/controlplane/internal/config"
)

// What the deployment exposes to the internet, checked from the internet.
//
// test/security and test/reachability check the same properties against a
// server this repository starts. That leaves out everything between the binary
// and the public name: the TLS termination, the CDN in front of it, the
// platform's own headers, and whether the configuration the running process
// loaded is the one in the repository at all.

// TestDeployed_TheCertificateIsValidForTheNameAndTLSIsModern.
//
// The database connection demands verify-full, and the API's own front door
// should be held to the same standard rather than to whatever the platform
// happened to provision.
func TestDeployed_TheCertificateIsValidForTheNameAndTLSIsModern(t *testing.T) {
	target := requireTarget(t)
	u, err := url.Parse(target)
	require.NoError(t, err)
	require.Equal(t, "https", u.Scheme, "the deployment must be reached over TLS")

	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}

	// A default dialer verifies the chain and the name; a failure here IS the
	// assertion.
	conn, err := tls.Dial("tcp", host, &tls.Config{MinVersion: tls.VersionTLS12}) //nolint:gosec // G402: MinVersion is set
	require.NoError(t, err, "the certificate must be valid for %s", u.Host)
	defer conn.Close()

	state := conn.ConnectionState()
	assert.GreaterOrEqual(t, state.Version, uint16(tls.VersionTLS12), "TLS below 1.2 is negotiable")
	assert.NotEmpty(t, state.PeerCertificates, "no certificate was presented")
	assert.NoError(t, state.PeerCertificates[0].VerifyHostname(u.Hostname()))
	assert.True(t, state.PeerCertificates[0].NotAfter.After(time.Now().Add(7*24*time.Hour)),
		"the certificate expires within a week; renewal is not happening")
}

// TestDeployed_TheRunningProcessLoadedTheConfigurationInThisRepository.
//
// /v1/version reports the SHA-256 of every non-secret configuration field the
// process actually loaded. Every SecretRef is blanked before hashing, so the
// same hash can be computed here from render.yaml without any secret being
// involved.
//
// This is the check that catches drift: a value edited in the platform's
// dashboard and never written back, which is exactly how this deployment came
// to be running CP_ENV=PROD while render.yaml said something else.
func TestDeployed_TheRunningProcessLoadedTheConfigurationInThisRepository(t *testing.T) {
	target := requireTarget(t)

	var reported struct {
		BuildVersion string `json:"build_version"`
		ConfigHash   string `json:"config_hash"`
		Environment  string `json:"environment"`
	}
	getJSON(t, target+"/v1/version", &reported)

	assert.Equal(t, "STAGING", reported.Environment,
		"the deployment is not the environment the blueprint declares")
	require.NotEmpty(t, reported.ConfigHash)

	env := blueprintEnv(t)
	// The secrets are blanked before hashing, so any syntactically valid
	// stand-in produces the same hash as the real value.
	//
	// NODAL_DB_MIGRATE_URL is gone with the blueprint entry that demanded it:
	// the schema owner is not handed to the internet-facing service (F-136).
	// The alert destination and the keyring are here because the loader now
	// resolves those two references rather than only parsing them (F-137).
	for k, v := range map[string]string{
		"NODAL_DB_APP_URL":            "postgresql://u:p@h/d?sslmode=verify-full",
		"NODAL_DB_OPS_URL":            "postgresql://u:p@h/d?sslmode=verify-full",
		"NODAL_OIDC_CLIENT_SECRET":    "stand-in",
		"NODAL_STRIPE_API_KEY":        "stand-in",
		"NODAL_STRIPE_WEBHOOK_SECRET": "stand-in",
		"NODAL_ALERT_WEBHOOK_URL":     "https://ntfy.sh/stand-in",
		"NODAL_PII_KEYRING":           `{"active":1,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}`,
	} {
		env[k] = v
	}

	cfg, err := config.Load(context.Background(), config.ServiceAPI, config.LookupFromMap(env))
	require.NoError(t, err, "render.yaml must be a configuration cmd/api can load")
	cfg.BuildVersion = reported.BuildVersion

	assert.Equal(t, cfg.Hash(), reported.ConfigHash,
		"the deployment is running a configuration that is not the one in render.yaml")
}

// TestDeployed_HealthAndReadinessAnswerSeparately.
//
// Liveness is the process alone; readiness touches the database. They must not
// be the same answer, or a database outage either takes the service out of
// rotation for being idle or is invisible.
func TestDeployed_HealthAndReadinessAnswerSeparately(t *testing.T) {
	target := requireTarget(t)
	assert.Equal(t, http.StatusOK, statusOf(t, target+"/v1/healthz"))
	assert.Equal(t, http.StatusOK, statusOf(t, target+"/v1/readyz"),
		"readiness is failing, which means the database is not reachable from the deployment")
}

// TestDeployed_EveryMoneyRouteRefusesAnUnauthenticatedCaller.
//
// A 401 is the assertion. A 500 would mean the route is reachable and failing
// somewhere past authentication, and a 404 would mean it is not wired at all --
// both of which have happened here.
func TestDeployed_EveryMoneyRouteRefusesAnUnauthenticatedCaller(t *testing.T) {
	target := requireTarget(t)

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"start a credit purchase", http.MethodPost, "/v1/payments", `{"account_id":"00000000-0000-0000-0000-000000000000","amount_minor":1000}`},
		{"read a credit balance", http.MethodGet, "/v1/credits/balance?account_id=00000000-0000-0000-0000-000000000000", ""},
		{"request a payout", http.MethodPost, "/v1/payouts", `{"account_id":"00000000-0000-0000-0000-000000000000","amount_minor":1000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			var reader *strings.Reader
			if tc.body != "" {
				reader = strings.NewReader(tc.body)
			} else {
				reader = strings.NewReader("")
			}
			req, err := http.NewRequestWithContext(ctx, tc.method, target+tc.path, reader)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "deployed-verification-"+tc.method)

			res, err := client.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()

			assert.Equal(t, http.StatusUnauthorized, res.StatusCode,
				"%s %s answered %d without a session", tc.method, tc.path, res.StatusCode)
		})
	}
}

// TestDeployed_TheBrowserProtectionsArePresentOnEveryAnswer.
//
// These are set by the application rather than by the platform, so a
// deployment behind a proxy that strips or fails to add them is a deployment
// with different security properties from the one the tests in this repository
// describe. Checking them from outside is the only way to know.
func TestDeployed_TheBrowserProtectionsArePresentOnEveryAnswer(t *testing.T) {
	target := requireTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/v1/healthz", nil)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	for header, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "strict-origin-when-cross-origin",
		"Content-Security-Policy":    "frame-ancestors 'none'",
		"Cross-Origin-Opener-Policy": "same-origin",
		"Cache-Control":              "no-store",
	} {
		assert.Equal(t, want, res.Header.Get(header), "%s", header)
	}

	// HSTS has to survive the CDN, and a short max-age is not HSTS.
	hsts := res.Header.Get("Strict-Transport-Security")
	require.NotEmpty(t, hsts, "no HSTS header reaches a browser")
	assert.Contains(t, hsts, "includeSubDomains")
	assert.Contains(t, hsts, "max-age=31536000")
}

// TestDeployed_TheRateLimitInForceIsTheOneTheBlueprintDeclares.
//
// The counters are process-local at one declared replica, and nothing inside
// the container can check that the platform runs one process. What can be
// checked is that the limit the deployment enforces is the number the
// blueprint states -- if those disagree, the declared replica count is not the
// one being run.
func TestDeployed_TheRateLimitInForceIsTheOneTheBlueprintDeclares(t *testing.T) {
	target := requireTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/v1/healthz", nil)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	limit := res.Header.Get("RateLimit-Limit")
	require.NotEmpty(t, limit, "no rate limit is being advertised, so none is being enforced")
	assert.NotEmpty(t, res.Header.Get("RateLimit-Remaining"))
	// "600/1m" declared, "600" advertised: the header carries the count.
	declared := blueprintEnv(t)["CP_API_RATE_LIMIT_GENERAL"]
	require.NotEmpty(t, declared, "the blueprint declares no general transport budget")
	count, _, ok := strings.Cut(declared, "/")
	require.True(t, ok, "CP_API_RATE_LIMIT_GENERAL is %q, not <requests>/<window>", declared)
	assert.Equal(t, count, limit, "the enforced limit is not the declared one")
}

// TestDeployed_CORSAnswersOnlyTheOriginsItWasGiven.
//
// The API is credentialed by cookie, so an origin it echoes back is an origin
// that can read a signed-in user's answers.
func TestDeployed_CORSAnswersOnlyTheOriginsItWasGiven(t *testing.T) {
	target := requireTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/v1/healthz", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://an-origin-nobody-configured.example")
	res, err := client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	got := res.Header.Get("Access-Control-Allow-Origin")
	assert.NotEqual(t, "*", got, "the API is credentialed; a wildcard origin exposes every signed-in answer")
	assert.NotEqual(t, "https://an-origin-nobody-configured.example", got,
		"an unconfigured origin was echoed back, which is the same thing as a wildcard")
}

// TestDeployed_NothingDiagnosticIsExposed.
//
// A profiler or a metrics endpoint on the public name is a description of the
// process handed to anyone who asks.
func TestDeployed_NothingDiagnosticIsExposed(t *testing.T) {
	target := requireTarget(t)
	for _, path := range []string{
		"/debug/pprof/", "/debug/vars", "/metrics", "/v1/debug", "/.env", "/v1/config",
	} {
		t.Run(path, func(t *testing.T) {
			assert.NotEqual(t, http.StatusOK, statusOf(t, target+path), "%s answered", path)
		})
	}
}

// TestDeployed_TheLoginRedirectsToTheIssuerWithPKCE.
//
// The whole authentication story rests on this one redirect being right: the
// issuer it names, the challenge method it uses, and the state and nonce it
// carries. A deployment pointed at the wrong issuer, or downgraded to a plain
// verifier, looks identical until someone reads the query string.
func TestDeployed_TheLoginRedirectsToTheIssuerWithPKCE(t *testing.T) {
	target := requireTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Do not follow the redirect: the Location is the assertion.
	noFollow := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/v1/auth/login", nil)
	require.NoError(t, err)
	res, err := noFollow.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusFound, res.StatusCode, "login must redirect to the identity provider")
	loc, err := url.Parse(res.Header.Get("Location"))
	require.NoError(t, err)

	issuer := blueprintEnv(t)["CP_AUTH_ISSUER"]
	require.NotEmpty(t, issuer)
	assert.True(t, strings.HasPrefix(loc.String(), strings.TrimRight(issuer, "/")),
		"login redirects to %q, which is not the configured issuer %q", loc.Host, issuer)

	q := loc.Query()
	assert.Equal(t, "S256", q.Get("code_challenge_method"),
		"PKCE must be S256; plain is a verifier anyone who sees the request can reuse")
	assert.NotEmpty(t, q.Get("code_challenge"))
	assert.NotEmpty(t, q.Get("state"), "no state parameter is no CSRF protection on the callback")
	assert.NotEmpty(t, q.Get("nonce"), "no nonce means a replayed id token cannot be detected")
	assert.Equal(t, "code", q.Get("response_type"), "any implicit flow puts a token in a URL")
	assert.Equal(t, blueprintEnv(t)["CP_AUTH_CLIENT_ID"], q.Get("client_id"))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func statusOf(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	return res.StatusCode
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode, url)
	require.NoError(t, json.NewDecoder(res.Body).Decode(into))
}

// blueprintEnv reads the literal environment out of render.yaml, which is what
// the deployment is supposed to be running.
func blueprintEnv(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../render.yaml")
	require.NoError(t, err)

	var bp struct {
		Services []struct {
			EnvVars []struct {
				Key   string  `yaml:"key"`
				Value *string `yaml:"value"`
			} `yaml:"envVars"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(b, &bp))
	require.Len(t, bp.Services, 1)

	env := map[string]string{}
	for _, e := range bp.Services[0].EnvVars {
		if e.Value != nil {
			env[e.Key] = *e.Value
		}
	}
	return env
}
