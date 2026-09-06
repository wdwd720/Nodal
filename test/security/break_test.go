package security

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// A test that cannot fail is worse than no test, because it converts an
// unknown into false confidence. Every adversarial guard in this package
// therefore has a named negative control: setting CP_SEC_BREAK to the guard's
// name makes the test deliberately violate the property it defends, so the
// assertion can be observed firing.
//
//	CP_SEC_BREAK=idor_targets_own_account go test -tags=integration ./test/security -run TestIDOR
//
// A run with CP_SEC_BREAK set is EXPECTED TO FAIL. It is evidence, not a
// regression. `make security` never sets it, and TestSecurityBreaksAreDeclared
// keeps the catalog honest: a break name a test reads must be listed here,
// and a name listed here must be read by some test, so the catalog cannot
// rot into a list of controls nobody runs.
//
// The pattern, and this comment's debt, are owed to test/chaos/break_test.go.
const envSecBreak = "CP_SEC_BREAK"

// breakNames is the closed catalog of negative controls: name → what the
// break does and which assertion it is expected to make fire.
var breakNames = map[string]string{
	"idor_targets_own_account": "every cross-tenant probe is pointed at the caller's OWN account instead of the foreign one, " +
		"so the 403 assertions see 200 — proving the probes distinguish permitted from refused rather than failing for an unrelated reason",
	"idor_absent_id_is_own_account": "the 'account that does not exist' probe uses the caller's own account id, " +
		"so the existing-but-foreign and non-existent answers differ — proving the account-enumeration-oracle comparison is live",
	"admin_probe_uses_privileged_session": "the 'a customer must not reach /v1/admin/*' probes are sent with the ADMIN session, " +
		"so the 403 assertions see 200 — proving the admin refusal is measured and not assumed",
	"idem_reuses_fresh_key": "the 'same key, different body' probe sends a FRESH key, so no INVALID_IDEMPOTENCY_REUSE is produced " +
		"— proving the 409 comes from key reuse and not from the body being rejected on its own merits",
	"idem_cross_tenant_replay_by_same_actor": "customer-a, not customer-b, replays the key, so the stored result IS returned " +
		"— proving the cross-tenant assertion measures per-principal scoping and not merely 'the second call differs'",
	"idem_concurrency_uses_distinct_keys": "each of the concurrent callers uses its own key, so N effects appear instead of one " +
		"— proving the 'exactly one effect' count is a real count and not an artifact of a single caller",
	"replay_uses_a_fresh_state": "the replayed callback fetches a NEW state instead of re-sending the consumed one, so a second session is created " +
		"— proving the replay assertion and the session-count assertion both bite",
	"logout_probe_uses_a_new_cookie": "after logout the probe presents a freshly issued cookie instead of the revoked one, " +
		"so the request succeeds — proving the 'revoked cookie is refused' assertion is measuring the revoked cookie",
	"ledger_cursor_targets_the_owning_account": "the hostile cursor is replayed against the account that DOES own the referenced journal row, " +
		"so the row is returned — proving the cross-tenant cursor assertion can see a leak",
	"injection_probes_unauthenticated": "the payload sweep drops the session cookie, so every probe is answered 401 before reaching a handler " +
		"— proving the sweep's 'the request reached the handler' precondition is what makes 'never a 500' meaningful",
	"redaction_handler_removed": "the logger is built from the bare JSON handler with no observability.NewRedactHandler, " +
		"so secret material reaches the output — proving the redaction assertions are not satisfied by the test's own inputs",
	"problem_body_renders_the_cause": "the HTTP error path writes the raw error text instead of errs.ToProblem's constant INTERNAL detail, " +
		"so the DSN, password and SQL leak into application/problem+json — proving the body assertions bite",
	"sqlscan_treats_parameters_as_constant": "the SQL source scanner counts a function parameter as constant-derived, " +
		"so the planted concatenation fixture is not reported — proving the scanner is sensitive enough to find one",
	"prompt_user_text_into_system_policy": "the compiled prompt request has the user's text spliced into SystemPolicy, " +
		"so untrusted text sits in the instruction channel — proving the separation assertion is live",
	"admin_self_approval_uses_a_second_operator": "the self-approval attempt is made by the second operator rather than the proposer, " +
		"so it succeeds — proving the dual-control assertion measures 'approver != proposer'",
	"admin_step_up_probe_uses_a_stepped_up_session": "the 'approve without step-up' probe uses the MFA session, " +
		"so it is not refused — proving the STEP_UP_REQUIRED assertion measures step-up and not something else",
}

// secBreak reports whether the named negative control is active. It fails the
// test on an unknown name so a typo in the environment cannot silently produce
// a green run that the operator believes was a negative control.
func secBreak(t *testing.T, name string) bool {
	t.Helper()
	if _, ok := breakNames[name]; !ok {
		t.Fatalf("security: unknown break name %q; add it to breakNames with a description", name)
	}
	requested := strings.TrimSpace(os.Getenv(envSecBreak))
	if requested == "" {
		return false
	}
	if _, ok := breakNames[requested]; !ok {
		t.Fatalf("security: %s=%q is not a declared break; declared: %v", envSecBreak, requested, sortedBreakNames())
	}
	if requested != name {
		return false
	}
	t.Logf("security: NEGATIVE CONTROL ACTIVE (%s=%s): %s. This run is EXPECTED TO FAIL.",
		envSecBreak, name, breakNames[name])
	return true
}

func sortedBreakNames() []string {
	out := make([]string, 0, len(breakNames))
	for k := range breakNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSecurityBreaksAreDeclared proves the catalog matches the source: every
// name passed to secBreak in this package is declared, and every declared name
// is read by at least one test. Without it a control could be deleted from a
// test while the catalog kept claiming it existed.
//
// It carries no build tag on purpose: the catalog must be checked in the
// untagged build too, where only part of the suite compiles but the whole
// package source is still on disk to be parsed.
func TestSecurityBreaksAreDeclared(t *testing.T) {
	used := breakNamesUsedInSource(t)
	for name := range breakNames {
		if _, ok := used[name]; !ok {
			t.Errorf("break %q is declared but no test reads it; delete it or write the control", name)
		}
	}
	for name, file := range used {
		if _, ok := breakNames[name]; !ok {
			t.Errorf("%s reads undeclared break %q; add it to breakNames", file, name)
		}
	}
}
