package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// This file covers the boundary half of the Stage 15 exit criterion: who may
// reach an operator route at all, and the two path/header contracts every
// admin command must satisfy before a domain package is asked anything.
//
// The dual-control half — proposer ≠ approver, the approve permission, a live
// elevation, an approval that belongs to another record — cannot be proved
// here, because these tests run against doubles that would happily say yes.
// It lives in admin_dualcontrol_integration_test.go, which drives the real
// internal/admin service against a real database over the same router.

// adminProbes returns every mounted /v1/admin route with a well-formed path.
func adminProbes(t *testing.T, s *Server) []routeProbe {
	t.Helper()
	var out []routeProbe
	for _, p := range mountedRoutes(t, s) {
		if strings.HasPrefix(p.path, "/v1/admin/") {
			out = append(out, p)
		}
	}
	require.NotEmpty(t, out, "the admin surface must have mounted routes")
	return out
}

// TestCustomerPrincipalCannotReachAnyAdminRoute is the RBAC separation that
// matters most in practice, and it is not the same test as the powerless
// principal in authz_test.go.
//
// A customer is not a principal with no permissions: they hold account:read,
// trade:create, funding:create, withdrawal:create and more, and they arrive
// with a real session and a recent multi-factor sign-in. Every one of those
// facts is true of the attacker who has simply signed up. What must stop them
// is that not one admin route's AnyOf set intersects the customer role — so
// this walks the whole operator surface rather than sampling it, and a future
// route whose floor accidentally admits account:read fails here.
func TestCustomerPrincipalCannotReachAnyAdminRoute(t *testing.T) {
	t.Parallel()
	customer := customerPrincipal()

	h := newHarness(t)
	for _, probe := range adminProbes(t, h.server) {
		name := probe.method + " " + probe.path
		t.Run(name, func(t *testing.T) {
			hh := newHarness(t)
			hh.as(&customer)
			res := hh.do(probe.method, probe.path, anonymousBody(probe.method),
				"Idempotency-Key", "customer-probe-key-0001")
			require.Equal(t, http.StatusForbidden, res.Code,
				"%s must refuse an ordinary customer; body=%s", name, res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)
		})
	}
}

// TestCustomerRoleHoldsNoAdminRoutePermission is the same property stated
// against the matrix instead of the router, so a refusal that happened for an
// incidental reason (a missing body, a bad id) could not be mistaken for the
// authorization one.
func TestCustomerRoleHoldsNoAdminRoutePermission(t *testing.T) {
	t.Parallel()
	customer := customerPrincipal()
	for op, pol := range operationPolicies {
		if !strings.HasPrefix(op, "GetAdmin") && !strings.HasPrefix(op, "PostAdmin") {
			continue
		}
		for _, perm := range pol.AnyOf {
			assert.False(t, customer.Has(perm, testNow),
				"%s admits %s, which the customer role holds", op, perm)
		}
	}
}

// --- canonical path identifiers ---------------------------------------------

// TestNonCanonicalPathIdentifiersAreRefused pins the rule that a resource is
// addressable by exactly one spelling.
//
// google/uuid parses "{01a0…}", "urn:uuid:01a0…" and the 32-character undashed
// form, and the generated binder normalises all of them, so without this
// middleware one action would answer to four different paths. Nothing about
// that is an authorization bypass, which is why it needs a test of its own:
// the damage is to every control that keys off the raw path — per-resource
// rate limit buckets, cache keys, a proxy rule, and the audit trail an
// operator greps — each of which would silently split across the spellings.
func TestNonCanonicalPathIdentifiersAreRefused(t *testing.T) {
	t.Parallel()
	canonical := testSessionID // a canonical UUID this suite already uses
	undashed := strings.ReplaceAll(canonical, "-", "")

	cases := map[string]string{
		"braced":   "{" + canonical + "}",
		"urn":      "urn:uuid:" + canonical,
		"undashed": undashed,
		// The escaped form matters most. Judging r.URL.Path (decoded) rather
		// than the raw target is what closes it: "%7B…%7D" does not parse as a
		// UUID, so a check on the escaped string would wave it through and the
		// router would then decode and bind it anyway.
		"percent-encoded":    "%7B" + canonical + "%7D",
		"undashed uppercase": strings.ToUpper(undashed),
	}

	operator := operatorPrincipal()
	for name, spelling := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.as(&operator)
			res := h.do(http.MethodPost, "/v1/admin/actions/"+spelling+"/approve",
				map[string]any{"note": "second pair of eyes"},
				"Idempotency-Key", "canonical-probe-0001")
			require.Equal(t, http.StatusBadRequest, res.Code,
				"%q must be refused as a non-canonical identifier; body=%s", spelling, res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeValidationFailed, p.Code)
			assert.Contains(t, p.Detail, "canonical UUID")
		})
	}
}

// TestCanonicalPathIdentifiersAreAccepted is the other half: the refusal above
// must be about the spelling and nothing else. Both a canonical UUID and its
// uppercase form address the same resource under every control that lowercases
// hex, so shape is the test and case is not.
func TestCanonicalPathIdentifiersAreAccepted(t *testing.T) {
	t.Parallel()
	operator := operatorPrincipal()
	for name, spelling := range map[string]string{
		"canonical":           testSessionID,
		"canonical uppercase": strings.ToUpper(testSessionID),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.as(&operator)
			res := h.do(http.MethodPost, "/v1/admin/actions/"+spelling+"/approve",
				map[string]any{"note": "second pair of eyes"},
				"Idempotency-Key", "canonical-probe-0002")
			// It reaches the handler, so whatever it answers is not the
			// path-shape refusal.
			if res.Code == http.StatusBadRequest {
				assert.NotContains(t, res.problem().Detail, "canonical UUID",
					"%q is canonical and must not be refused for its shape", spelling)
			}
		})
	}
}

// TestNonUUIDPathSegmentsAreLeftAlone: only segments that genuinely parse as a
// UUID are judged. A capability name is an opaque path value and must pass
// through untouched, or the middleware would break every non-UUID route.
func TestNonUUIDPathSegmentsAreLeftAlone(t *testing.T) {
	t.Parallel()
	operator := operatorPrincipal()
	h := newHarness(t)
	h.as(&operator)
	res := h.do(http.MethodPost, "/v1/admin/gates/LIVE_FUNDING/propose",
		map[string]any{"reason": "quarterly review of the funding gate"},
		"Idempotency-Key", "gate-probe-key-0001")
	if res.Code == http.StatusBadRequest {
		assert.NotContains(t, res.problem().Detail, "canonical UUID",
			"a capability name is not an identifier and must not be judged as one")
	}
}

// --- the Idempotency-Key contract -------------------------------------------

// validProposalBody is a proposal that passes every body check, so a request
// built from it fails on the Idempotency-Key or not at all.
func validProposalBody() map[string]any {
	return map[string]any{
		"kind":        string(admin.KindCapabilityGateApprove),
		"target_type": "capability_gate",
		"target_id":   "LIVE_FUNDING",
		"reason":      "quarterly review of the funding gate",
	}
}

// TestIdempotencyKeyCharsetAndLengthAreEnforced pins
// components.parameters.IdempotencyKey at the edge.
//
// The charset is narrower than printable ASCII on purpose: the key becomes
// part of idempotency_keys' primary key, is echoed back in responses, and is
// written to structured logs and audit records. Validating it here means none
// of those sinks has to be the place that gets quoting right for a value an
// untrusted client chose.
func TestIdempotencyKeyCharsetAndLengthAreEnforced(t *testing.T) {
	t.Parallel()
	operator := operatorPrincipal()

	cases := map[string]struct {
		key  string
		want string
	}{
		"too short": {key: "1234567", want: "8 to 128 characters"},
		// An absent value is caught earlier, by the generated binder refusing
		// to bind a required header, so it never reaches validateIdempotencyKey
		// and does not carry the length message. It is still refused, which is
		// what matters; asserting the length wording here would be asserting a
		// message this path does not produce.
		"empty":            {key: "", want: "Idempotency-Key"},
		"too long":         {key: strings.Repeat("a", 129), want: "8 to 128 characters"},
		"space":            {key: "key with space", want: "letters, digits"},
		"slash":            {key: "key/with/slash", want: "letters, digits"},
		"newline":          {key: "key\nwith-newline", want: "letters, digits"},
		"quote":            {key: `key"with-quote`, want: "letters, digits"},
		"percent":          {key: "key%00nul", want: "letters, digits"},
		"non-ascii":        {key: "kéy-with-accent", want: "letters, digits"},
		"json meta":        {key: "{\"k\":\"v\"}", want: "letters, digits"},
		"sql meta":         {key: "key';DROP--", want: "letters, digits"},
		"128 is inclusive": {key: strings.Repeat("a", 128)},
		"exactly 8":        {key: "12345678"},
		"uuid":             {key: "0193b2e0-0000-7000-8000-000000000002"},
		"dotted token":     {key: "admin.console:propose_1-2024"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.ports.adminActs.action = sampleAdminAction()
			h.as(&operator)
			res := h.do(http.MethodPost, "/v1/admin/actions", validProposalBody(),
				"Idempotency-Key", tc.key)

			if tc.want == "" {
				require.Equal(t, http.StatusCreated, res.Code,
					"%q is a legal key and must be accepted; body=%s", tc.key, res.Body.String())
				return
			}
			require.Equal(t, http.StatusBadRequest, res.Code,
				"%q must be refused; body=%s", tc.key, res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeValidationFailed, p.Code)
			assert.Contains(t, p.Detail, tc.want)
		})
	}
}

// TestIdempotencyKeyIsRequiredOnAdminCommands: a mutating admin route with no
// key at all is refused, so a client cannot opt out of the replay contract by
// omitting the header.
func TestIdempotencyKeyIsRequiredOnAdminCommands(t *testing.T) {
	t.Parallel()
	operator := operatorPrincipal()
	h := newHarness(t)
	h.as(&operator)
	res := h.do(http.MethodPost, "/v1/admin/actions", validProposalBody())
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

// sampleAdminAction is a fully-formed action for the doubles to return, so a
// success path in this file is a real 201 rather than an accident of zero
// values.
func sampleAdminAction() admin.Action {
	return admin.Action{
		ID:               admin.NewActionID(),
		Kind:             admin.KindCapabilityGateApprove,
		TargetType:       "capability_gate",
		TargetID:         "LIVE_FUNDING",
		Params:           json.RawMessage(`{}`),
		ParamsHash:       []byte{0x01, 0x02},
		Reason:           "quarterly review of the funding gate",
		RequiresDual:     true,
		Status:           admin.StatusProposed,
		ProposedBy:       testUserID.String(),
		ProposedAt:       testNow,
		ProposerStepUpAt: testNow,
		ExpiresAt:        testNow.Add(24 * time.Hour),
		UpdatedAt:        testNow,
	}
}

// TestAdminCommandsDeclareStepUp: every admin route that changes state must
// demand a recent strong authentication at the boundary. The domain packages
// demand their own as well; this is the floor, and a new admin command that
// forgets it fails here rather than in production.
func TestAdminCommandsDeclareStepUp(t *testing.T) {
	t.Parallel()
	// Kill-switch activation is the one deliberate exception: stopping new
	// risk must never wait for a re-authentication (POLICY_AUTHORITY §2).
	// Release, which is the dangerous direction, is gated by internal/killswitch
	// with step-up and — for SEVERE switches — an approved admin action.
	exempt := map[string]string{
		"PostAdminKillSwitches": "activation must be fast; release is gated by internal/killswitch",
	}
	for op, pol := range operationPolicies {
		if !strings.HasPrefix(op, "PostAdmin") {
			continue
		}
		if reason, ok := exempt[op]; ok {
			assert.False(t, pol.StepUp, "%s is listed as exempt (%s) but declares step-up", op, reason)
			continue
		}
		assert.True(t, pol.StepUp, "%s changes operator state and must require step-up", op)
		assert.True(t, pol.Mutating, "%s is a command and must be marked mutating", op)
	}
}

// TestNoAdminRouteAdmitsAnAgent restates the containment rule for the operator
// surface specifically. The whole-surface version lives in authz_test.go; this
// one exists so that a future admin route added with AllowAgent set fails a
// test whose name says why it matters.
func TestNoAdminRouteAdmitsAnAgent(t *testing.T) {
	t.Parallel()
	for op, pol := range operationPolicies {
		if !strings.HasPrefix(op, "GetAdmin") && !strings.HasPrefix(op, "PostAdmin") {
			continue
		}
		assert.False(t, pol.AllowAgent,
			"%s must never admit an AGENT principal: an agent may not approve, resolve or hold a key", op)
	}
}

// TestApprovePathFloorIsDeliberatelyCoarse documents, and pins, the fact that
// the decision route admits propose-side permissions.
//
// It has to: a rejection is available to a proposer, and execution is
// available to either side, so a floor of approve-side permissions alone would
// lock the route. The consequence is that holding gate:propose is enough to
// *reach* the approve path — the only thing that refuses the approval itself
// is internal/admin, which is why
// TestIntegration_ApproveRefusesAPrincipalLackingTheApprovePermission exists.
func TestApprovePathFloorIsDeliberatelyCoarse(t *testing.T) {
	t.Parallel()
	pol, ok := policyFor("PostAdminActionsActionIdDecision")
	require.True(t, ok)

	var standing, dual int
	for _, perm := range pol.AnyOf {
		if security.IsDualControl(perm) {
			dual++
			continue
		}
		standing++
	}
	assert.Positive(t, standing,
		"the decision route must be reachable by a standing role, or reject and execute become impossible")
	assert.Positive(t, dual,
		"the decision route must also admit the approve-side permissions")
	assert.True(t, pol.StepUp, "every decision on a controlled action needs a recent strong authentication")
}
