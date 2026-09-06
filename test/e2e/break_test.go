//go:build integration && e2e

package e2e

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// A test that cannot fail is worse than no test, because it converts an
// unknown into false confidence. Every guard in this package therefore has a
// named negative control: setting CP_E2E_BREAK to the guard's name makes the
// test deliberately violate the property it defends, so the assertion can be
// observed firing.
//
//	CP_E2E_BREAK=sse_no_frame go test -tags=integration,e2e ./test/e2e -run TestE2E_SSE
//
// A run with CP_E2E_BREAK set is EXPECTED TO FAIL. It is evidence, not a
// regression. `make e2e` never sets it, and TestE2EBreaksAreDeclared keeps the
// catalog honest: a break name a test reads must be listed here, and a name
// listed here must be read by some test, so the catalog cannot rot into a
// list of controls nobody runs.
const envE2EBreak = "CP_E2E_BREAK"

// breakNames is the closed catalog of negative controls.
var breakNames = map[string]string{
	"journey_wrong_account": "every account-scoped read in the customer journey is aimed at customer-b's account " +
		"instead of customer-a's, so the seeded 10,000.00 USDC and the SEED journal posting are not there — " +
		"which is what a buying-power assertion that silently accepted the wrong account would look like",
	"logout_not_revoked": "the request made after logout carries a freshly minted session instead of the revoked one, " +
		"so it succeeds — the shape of a server that keeps honoring a revoked cookie",
	"idempotency_two_effects": "the second POST of the same command uses a different Idempotency-Key, " +
		"so two trade intents exist where the contract allows one",
	"error_body_leaks": "a DSN is spliced into the bytes the leakage scan reads, " +
		"so the forbidden-substring check must fire; if it does not, the scan is not looking at the response",
	"cross_tenant_allowed": "the cross-tenant request is made with the owner's own session, " +
		"so it is answered 200 — the shape of a broken tenant check",
	"sse_no_frame": "the stream is given a deadline shorter than the server's heartbeat interval, " +
		"so no frame can arrive in the window and the delivery assertion must fire",
	"concurrency_serialized": "the concurrent callers are run one after another, so nothing contends; " +
		"the precondition that the goroutines really overlapped must fire, because a concurrency test " +
		"that never contends proves nothing",
}

// e2eBreak reports whether the named negative control is active. It fails the
// test on an unknown name so a typo in the environment cannot silently
// produce a green run that the operator believes was a negative control.
func e2eBreak(t *testing.T, name string) bool {
	t.Helper()
	if _, ok := breakNames[name]; !ok {
		t.Fatalf("e2e: unknown break name %q; add it to breakNames with a description", name)
	}
	requested := strings.TrimSpace(os.Getenv(envE2EBreak))
	if requested == "" {
		return false
	}
	if _, ok := breakNames[requested]; !ok {
		t.Fatalf("e2e: %s=%q is not a declared break; declared: %v", envE2EBreak, requested, sortedBreakNames())
	}
	if requested != name {
		return false
	}
	t.Logf("e2e: NEGATIVE CONTROL ACTIVE (%s=%s): %s. This run is EXPECTED TO FAIL.",
		envE2EBreak, name, breakNames[name])
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

// TestE2EBreaksAreDeclared proves the catalog matches the source: every name
// passed to e2eBreak in this package is declared, and every declared name is
// read by at least one test. Without this, a control could be deleted from a
// test while its documentation kept claiming it existed.
func TestE2EBreaksAreDeclared(t *testing.T) {
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
