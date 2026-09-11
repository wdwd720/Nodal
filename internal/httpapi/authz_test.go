package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/security"
)

// generatedOperations returns every operation the generated strict server
// declares. The method names of api.StrictServerInterface are the operation
// ids the strict middleware receives, so this is the authoritative list.
func generatedOperations() []string {
	t := reflect.TypeOf((*api.StrictServerInterface)(nil)).Elem()
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	sort.Strings(out)
	return out
}

// TestEveryGeneratedOperationHasAnExplicitPolicy is the deny-by-default proof.
// Authorization is a table keyed by operation id; an operation with no entry is
// refused at runtime, and this test makes that refusal visible at build time.
// Regenerating the server with a new route fails here until someone writes the
// route's permission requirement down.
func TestEveryGeneratedOperationHasAnExplicitPolicy(t *testing.T) {
	t.Parallel()
	ops := generatedOperations()
	require.NotEmpty(t, ops)

	var missing []string
	for _, op := range ops {
		if _, ok := policyFor(op); !ok {
			missing = append(missing, op)
		}
	}
	assert.Empty(t, missing,
		"every generated operation needs an entry in operationPolicies; missing: %v", missing)

	known := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		known[op] = struct{}{}
	}
	var stale []string
	for op := range operationPolicies {
		if _, ok := known[op]; !ok {
			stale = append(stale, op)
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale, "operationPolicies names operations the server does not declare: %v", stale)
}

// TestNonPublicOperationsDeclareAPermission: a route that is not explicitly
// public must name at least one permission. authorize refuses an empty
// requirement at runtime as well; this catches it earlier.
func TestNonPublicOperationsDeclareAPermission(t *testing.T) {
	t.Parallel()
	for _, op := range generatedOperations() {
		pol, ok := policyFor(op)
		require.True(t, ok, op)
		if pol.Public {
			continue
		}
		assert.NotEmpty(t, pol.AnyOf, "%s is not public and must name a permission", op)
		for _, p := range pol.AnyOf {
			assert.True(t, p.Valid(), "%s names an unknown permission %q", op, p)
		}
	}
}

// TestPublicOperationsAreExactlyTheExpectedSet freezes the unauthenticated
// surface. Anything added to it has to be added here too, deliberately.
func TestPublicOperationsAreExactlyTheExpectedSet(t *testing.T) {
	t.Parallel()
	want := []string{
		"GetAuthCallback",      // OIDC callback; the state row is the credential
		"GetAuthLogin",         // OIDC entry point
		"GetHealthz",           // liveness
		"GetReadyz",            // readiness
		"GetVersion",           // build version and non-secret config hash
		"PostWebhooksProvider", // authority is the provider signature over raw bytes
	}
	var got []string
	for op, pol := range operationPolicies {
		if pol.Public {
			got = append(got, op)
		}
	}
	sort.Strings(got)
	assert.Equal(t, want, got)
}

// TestNoOperationAdmitsAnAgent: the REST surface is for humans and operators.
// An agent proposes intents through internal/intent, never over HTTP, and can
// never reach signing, withdrawal, capital or risk here.
func TestNoOperationAdmitsAnAgent(t *testing.T) {
	t.Parallel()
	for op, pol := range operationPolicies {
		assert.False(t, pol.AllowAgent, "%s must not admit AGENT principals", op)
	}
}

// TestNoOperationGrantsADualControlPermissionAlone: an approve-side permission
// is never a standing role's, so a route floor built only from approve-side
// permissions would be unreachable. Every mutating route must therefore also
// admit a propose-side or standing permission.
func TestMutatingOperationsAreReachableByAStandingRole(t *testing.T) {
	t.Parallel()
	for op, pol := range operationPolicies {
		if pol.Public || !pol.Mutating {
			continue
		}
		reachable := false
		for _, perm := range pol.AnyOf {
			if !security.IsDualControl(perm) {
				reachable = true
				break
			}
		}
		assert.True(t, reachable,
			"%s only admits dual-control permissions, which no standing role holds", op)
	}
}

// routeProbe is one mounted route with a concrete, well-formed path.
type routeProbe struct {
	method string
	path   string
}

// mountedRoutes walks the real router and returns a well-formed request for
// every mounted route. Path parameters are filled with valid values so the
// generated binder succeeds and the request reaches the authorization gate.
func mountedRoutes(t *testing.T, s *Server) []routeProbe {
	t.Helper()
	routes, ok := s.Router().(chi.Routes)
	require.True(t, ok, "the router must be walkable")

	replacements := map[string]string{
		"{accountId}":      testAccountID.String(),
		"{instrumentId}":   testInstrument.String(),
		"{intentId}":       testIntentID.String(),
		"{orderId}":        testOrderID.String(),
		"{depositId}":      testDepositID.String(),
		"{sessionId}":      testSessionID,
		"{actionId}":       testSessionID,
		"{recordId}":       testSessionID,
		"{capability}":     "LIVE_FUNDING",
		"{action}":         "propose",
		"{decision}":       "approve",
		"{provider}":       "stripe_credit",
		"{assetId}":        testInstrument.String(),
		"{marketId}":       testOrderID.String(),
		"{payoutId}":       testSessionID,
		"{paymentId}":      testSessionID,
		"{productId}":      testOrderID.String(),
		"{userId}":         testUserID.String(),
		"{notificationId}": testOrderID.String(),
		// The agent surface. `{action}` is shared with the gate route above and
		// is already mapped; an agent action that is not one of the five is
		// refused by the handler, and these probes never reach a handler --
		// authorization answers first, which is what they measure.
		"{agentId}":    testSessionID,
		"{strategyId}": testSessionID,
	}

	requiredQuery := map[string]string{
		"/v1/intents":          "account_id=" + testAccountID.String(),
		"/v1/orders":           "account_id=" + testAccountID.String(),
		"/v1/funding/deposits": "account_id=" + testAccountID.String(),
		"/v1/credits/balance":  "account_id=" + testAccountID.String(),
		"/v1/payouts":          "account_id=" + testAccountID.String(),
		"/v1/internal-orders":  "account_id=" + testAccountID.String(),
		"/v1/me/portfolio":     "account_id=" + testAccountID.String(),
		"/v1/me/activity":      "account_id=" + testAccountID.String(),
		// The candle window is bounded, so from/to are required and there is
		// no default worth guessing: a chart that asks for "everything" on a
		// market with a year of prints is a table scan a client can request by
		// typing a date.
		"/v1/native-markets/{marketId}/candles": "interval=1m&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z",
		"/v1/agents":                            "account_id=" + testAccountID.String(),
		"/v1/strategies":                        "account_id=" + testAccountID.String(),
	}

	var out []routeProbe
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := route
		for placeholder, value := range replacements {
			path = strings.ReplaceAll(path, placeholder, value)
		}
		require.NotContains(t, path, "{", "route %s has an unmapped path parameter", route)
		// Routes with a required query parameter are probed with it, so the
		// generated binder succeeds and the request reaches the
		// authorization gate rather than stopping at VALIDATION_FAILED.
		if q, ok := requiredQuery[route]; ok {
			path += "?" + q
		}
		out = append(out, routeProbe{method: method, path: path})
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, out)
	return out
}

// publicPaths are the concrete paths of the operations declared public.
func publicPaths() map[string]struct{} {
	return map[string]struct{}{
		"GET /v1/auth/login":              {},
		"GET /v1/auth/callback":           {},
		"GET /v1/healthz":                 {},
		"GET /v1/readyz":                  {},
		"GET /v1/version":                 {},
		"POST /v1/webhooks/stripe_credit": {},
	}
}

// TestNoRouteIsUnintentionallyUnauthenticated walks every route the generated
// server mounts and proves that an anonymous, otherwise well-formed request is
// refused with UNAUTHENTICATED unless the route is on the explicit public list.
// A newly generated route that nobody wrote a policy for lands in the default
// branch and fails here.
func TestNoRouteIsUnintentionallyUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	public := publicPaths()

	for _, probe := range mountedRoutes(t, h.server) {
		name := probe.method + " " + probe.path
		t.Run(name, func(t *testing.T) {
			hh := newHarness(t)
			hh.as(nil)
			res := hh.do(probe.method, probe.path, anonymousBody(probe.method),
				"Idempotency-Key", "probe-key-000000")

			key := probe.method + " " + routeKeyFor(probe)
			if _, isPublic := public[key]; isPublic {
				assert.NotEqual(t, http.StatusUnauthorized, res.Code,
					"%s is declared public but refused anonymous access", key)
				// And the probe must have REACHED the route. A 404 satisfies
				// "not 401" while proving nothing, and this assertion existed
				// in that weaker form: `{provider}` was substituted with
				// "stripe", which is not a registered provider (F-124 corrected
				// the contract to `stripe_credit`), so the webhook probe was
				// answered 404 by the provider lookup and the public claim was
				// never measured.
				assert.NotEqual(t, http.StatusNotFound, res.Code,
					"%s is declared public but the probe never reached it: a 404 makes the assertion above vacuous", key)
				return
			}
			require.Equal(t, http.StatusUnauthorized, res.Code,
				"%s must refuse an anonymous request; body=%s", key, res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeUnauthenticated, p.Code)
		})
	}
}

// routeKeyFor renders the probe back to its templated form for the public-set
// lookup (only the webhook route has a concrete segment in the public set).
func routeKeyFor(p routeProbe) string {
	path := p.path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.Replace(path, testAccountID.String(), "{accountId}", 1)
	path = strings.Replace(path, testInstrument.String(), "{instrumentId}", 1)
	path = strings.Replace(path, testIntentID.String(), "{intentId}", 1)
	path = strings.Replace(path, testOrderID.String(), "{orderId}", 1)
	path = strings.Replace(path, testDepositID.String(), "{depositId}", 1)
	path = strings.Replace(path, testSessionID, "{sessionId}", 1)
	return path
}

func anonymousBody(method string) any {
	// PUT as well as POST: the generated binder decodes a required body BEFORE
	// authorization runs (D-042), so a probe with no body is refused with 400
	// and never measures what these tests are about.
	if method == http.MethodPost || method == http.MethodPut {
		return "{}"
	}
	return nil
}

// TestEveryRouteRefusesAPrincipalWithoutPermissions is the per-route
// authorization-denial test: a fully authenticated principal that holds no
// permission at all is refused everywhere that is not public.
func TestEveryRouteRefusesAPrincipalWithoutPermissions(t *testing.T) {
	t.Parallel()
	public := publicPaths()
	powerless := security.Principal{
		SubjectID: testUserID.String(),
		ActorType: security.ActorUser,
		Roles:     nil, // no role, therefore no permission
		SessionID: testSessionID,
		AuthTime:  testNow,
		AMR:       []string{"pwd", "mfa"},
	}

	h := newHarness(t)
	for _, probe := range mountedRoutes(t, h.server) {
		key := probe.method + " " + routeKeyFor(probe)
		if _, isPublic := public[key]; isPublic {
			continue
		}
		t.Run(key, func(t *testing.T) {
			hh := newHarness(t)
			hh.as(&powerless)
			res := hh.do(probe.method, probe.path, anonymousBody(probe.method),
				"Idempotency-Key", "probe-key-000000")
			require.Equal(t, http.StatusForbidden, res.Code,
				"%s must refuse a principal with no permissions; body=%s", key, res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)
		})
	}
}

// TestAgentPrincipalsAreRefusedEverywhere proves the containment rule at the
// transport: an AGENT principal cannot reach any authenticated route, so no
// endpoint lets an agent sign, withdraw, hold a key, or change risk or capital.
func TestAgentPrincipalsAreRefusedEverywhere(t *testing.T) {
	t.Parallel()
	public := publicPaths()
	agent := agentPrincipal()

	h := newHarness(t)
	for _, probe := range mountedRoutes(t, h.server) {
		key := probe.method + " " + routeKeyFor(probe)
		if _, isPublic := public[key]; isPublic {
			continue
		}
		t.Run(key, func(t *testing.T) {
			hh := newHarness(t)
			hh.as(&agent)
			res := hh.do(probe.method, probe.path, anonymousBody(probe.method),
				"Idempotency-Key", "probe-key-000000")
			require.Equal(t, http.StatusForbidden, res.Code,
				"%s must refuse an AGENT principal; body=%s", key, res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeForbidden, p.Code)
			assert.Contains(t, p.Detail, "agents")
		})
	}
}

// TestAuthorizeFailsClosedForAnUnknownOperation: the runtime half of
// deny-by-default. An operation id with no policy is refused even though the
// principal is a full administrator.
// TestTheConfiguredStepUpAgeTightensButNeverWidens (F-89).
//
// CP_AUTH_STEP_UP_MAX_AGE was loaded, validated as positive, and read by
// nothing: every window in the process was a hard-coded constant, so the
// deployment's 5 minutes meant 15 and tightening it changed nothing.
func TestTheConfiguredStepUpAgeTightensButNeverWidens(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 5*time.Minute, effectiveStepUpMaxAge(5*time.Minute), "a tighter value must be used")
	assert.Equal(t, stepUpMaxAge, effectiveStepUpMaxAge(24*time.Hour),
		"a deployment may not widen a window the code chose")
	assert.Equal(t, stepUpMaxAge, effectiveStepUpMaxAge(0), "unset leaves the constant in force")
	assert.Equal(t, stepUpMaxAge, effectiveStepUpMaxAge(-time.Hour), "a negative value cannot disable step-up")
}

func TestAuthorizeFailsClosedForAnUnknownOperation(t *testing.T) {
	t.Parallel()
	p := operatorPrincipal()
	ctx := security.WithPrincipal(t.Context(), p)
	err := authorize(ctx, "SomeOperationNobodyWroteAPolicyFor", func() time.Time { return testNow }, stepUpMaxAge)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// TestStepUpIsRequiredWhereDeclared: an operation that demands recent strong
// authentication refuses a stale session even when the permission is held.
func TestStepUpIsRequiredWhereDeclared(t *testing.T) {
	t.Parallel()
	stale := operatorPrincipal()
	stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)

	h := newHarness(t)
	h.as(&stale)
	res := h.do(http.MethodPost, "/v1/admin/accounts/"+testAccountID.String()+"/status",
		map[string]any{"to": "FROZEN", "reason": "compliance hold"},
		"Idempotency-Key", "step-up-key-0001")
	require.Equal(t, http.StatusForbidden, res.Code)
	assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
}

func TestOperationPolicyCountMatchesRouteCount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	assert.Equal(t, len(generatedOperations()), len(mountedRoutes(t, h.server)),
		"every generated operation must be mounted exactly once")
	assert.Equal(t, len(generatedOperations()), len(operationPolicies))
}

// The window /v1/me reports is the window authorize enforces (F-104).
//
// They used to disagree. authorize takes effectiveStepUpMaxAge(configured),
// which is the MINIMUM of the deployment's CP_AUTH_STEP_UP_MAX_AGE and the
// package's own ceiling; the response was built from the ceiling alone. Under
// the deployed 5m the API told an operator their step-up was good for fifteen
// minutes while the boundary refused after five -- and the operations it gates
// are the gate ceremony and the admin plane, where a client that trusts the
// field submits an approval it is about to be refused for.
func TestStepUp_TheReportedWindowIsTheEnforcedOne(t *testing.T) {
	t.Parallel()
	const configured = 5 * time.Minute
	require.Less(t, configured, stepUpMaxAge, "the fixture must actually tighten, or this proves nothing")

	h := newHarness(t)
	srv, err := New(Options{
		Env: h.server.opts.Env, Clock: h.server.clk, StepUpMaxAge: configured,
		Authenticator: h.server.opts.Authenticator, Ports: h.ports.ports(),
	})
	require.NoError(t, err)
	h.server = srv

	p := customerPrincipal()
	h.as(&p)
	res := h.do(http.MethodGet, "/v1/me", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out api.Principal
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	require.NotNil(t, out.StepUpValidUntil)
	assert.Equal(t, p.AuthTime.Add(configured).UTC(), out.StepUpValidUntil.UTC(),
		"the reported window is the package ceiling, not the tighter one the boundary applies")

	// The control: with nothing configured, the ceiling is what is reported and
	// what is enforced, so the field does not silently become zero.
	h2 := newHarness(t)
	h2.as(&p)
	var plain api.Principal
	r2 := h2.do(http.MethodGet, "/v1/me", nil)
	require.NoError(t, json.Unmarshal(r2.Body.Bytes(), &plain))
	require.NotNil(t, plain.StepUpValidUntil)
	assert.Equal(t, p.AuthTime.Add(stepUpMaxAge).UTC(), plain.StepUpValidUntil.UTC())
}
