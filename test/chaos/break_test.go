//go:build integration && chaos

package chaos

import (
	"os"
	"strings"
	"testing"
)

// A test that cannot fail is worse than no test, because it converts an
// unknown into false confidence. Every guard in this package therefore has a
// named negative control: setting CP_CHAOS_BREAK to the guard's name makes the
// test deliberately violate the property it defends, so the assertion can be
// observed firing.
//
//	CP_CHAOS_BREAK=relay_reports_stall_as_success go test -tags=integration,chaos ./test/chaos -run TestChaos_BrokerStall
//
// A run with CP_CHAOS_BREAK set is EXPECTED TO FAIL. It is evidence, not a
// regression. `make chaos` never sets it, and TestChaosBreaksAreDeclared keeps
// the catalog honest: a break name a test reads must be listed here, and a
// name listed here must be read by some test, so the catalog cannot rot into
// a list of controls nobody runs.
const envChaosBreak = "CP_CHAOS_BREAK"

// breakNames is the closed catalog of negative controls.
var breakNames = map[string]string{
	"db_partial_write": "the reservation and its ledger posting are written in SEPARATE transactions, " +
		"so a database fault between them leaves a reservation with no posting — the partial financial effect the guard forbids",
	"relay_reports_stall_as_success": "the bus reports a stalled publish as success (the pre-D-034 behavior), " +
		"so the relay marks the outbox row published and the event is lost",
	"inbox_bypassed":                "the effect runs with no inbox guard, so concurrent redeliveries of one provider message each credit the account",
	"rogue_relay_ignores_the_claim": "a second relay publishes rows another instance has claimed, producing the duplicate delivery the row lock exists to prevent",
	"archive_failure_swallowed": "an archive write that the object store refused is recorded as stored, " +
		"so evidence is reported present when it is absent",
	"commerce_partial_write": "the buyer's Credits are moved on the ledger WITHOUT consuming the lots behind them, " +
		"so the ledger says they paid while provenance still claims every unit — the drift VerifyProvenance exists to catch. " +
		"The mirror image (consuming lots with no posting) is not usable as a control: SQLSTATE CR004 makes it unrepresentable",
	"native_trade_partial_write": "the trader's Credits leave their balance in a SEPARATE, already-committed transaction, " +
		"so a fault leaves them having paid for a trade that produced no fill and moved no curve",
	"clock_jump_trusted": "the idempotency KEY is derived from the wall clock, so a clock jump makes a replay look like a new command and the deposit posts twice. " +
		"Deriving only the CONTENT from the clock is NOT a usable control: the ledger refuses that as INVALID_IDEMPOTENCY_REUSE, so the invariant holds either way and the test cannot fail",
}

// chaosBreak reports whether the named negative control is active. It panics
// on an unknown name so a typo in the environment cannot silently produce a
// green run that the operator believes was a negative control.
func chaosBreak(t *testing.T, name string) bool {
	t.Helper()
	if _, ok := breakNames[name]; !ok {
		t.Fatalf("chaos: unknown break name %q; add it to breakNames with a description", name)
	}
	requested := strings.TrimSpace(os.Getenv(envChaosBreak))
	if requested == "" {
		return false
	}
	if _, ok := breakNames[requested]; !ok {
		t.Fatalf("chaos: %s=%q is not a declared break; declared: %v", envChaosBreak, requested, sortedBreakNames())
	}
	if requested != name {
		return false
	}
	t.Logf("chaos: NEGATIVE CONTROL ACTIVE (%s=%s): %s. This run is EXPECTED TO FAIL.",
		envChaosBreak, name, breakNames[name])
	return true
}

func sortedBreakNames() []string {
	out := make([]string, 0, len(breakNames))
	for k := range breakNames {
		out = append(out, k)
	}
	return out
}

// TestChaosBreaksAreDeclared proves the catalog matches the source: every
// name passed to chaosBreak in this package is declared, and every declared
// name is read by at least one test. Without this, a control could be deleted
// from a test while its documentation kept claiming it existed.
func TestChaosBreaksAreDeclared(t *testing.T) {
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
