package bridge

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
)

func states(xs ...CrossChainCapitalState) map[CrossChainCapitalState]bool {
	out := map[CrossChainCapitalState]bool{}
	for _, x := range xs {
		out[x] = true
	}
	return out
}

// TestStateMachine_FinalityNeverReverts: no edge leads from a state whose
// source lock is final back to a state before it, nor from a final release
// back to one in flight; terminal states have no successors; every state is
// reachable from AVAILABLE.
func TestStateMachine_FinalityNeverReverts(t *testing.T) {
	beforeSource := states(StateAvailable, StateReserved, StateBridgeRequested)
	afterSource := states(StateSourceFinalized, StateMessageInFlight, StateDestinationObserved, StateDestinationFinalized,
		StateReconciled, StateManualReview)
	releasedFinal := states(StateDestinationFinalized, StateReconciled)
	for _, from := range AllStates() {
		assert.False(t, CanTransition(from, from), "%s has a self-loop", from)
		for _, to := range AllStates() {
			if !CanTransition(from, to) {
				continue
			}
			if afterSource[from] {
				assert.False(t, beforeSource[to], "%s -> %s reverts a final lock", from, to)
			}
			if releasedFinal[from] {
				assert.True(t, to == StateReconciled || to == StateManualReview, "%s -> %s reverts a final release", from, to)
			}
			if from.Terminal() {
				t.Errorf("terminal %s has a successor %s", from, to)
			}
		}
	}
	reached := map[CrossChainCapitalState]bool{StateAvailable: true}
	frontier := []CrossChainCapitalState{StateAvailable}
	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		for _, to := range AllStates() {
			if CanTransition(s, to) && !reached[to] {
				reached[to] = true
				frontier = append(frontier, to)
			}
		}
	}
	assert.Len(t, reached, len(AllStates()), "every state is reachable")
	assert.False(t, CanTransition(StateBridgeRequested, StateManualReview), "a lock that is not final never needs review")
	assert.True(t, CanTransition(StateDestinationObserved, StateMessageInFlight), "an observed release may be reorged away")
	assert.False(t, CanTransition(StateDestinationFinalized, StateMessageInFlight), "a final release never is")
	assert.True(t, CanTransitionRoute(RouteActive, RoutePaused))
	assert.False(t, CanTransitionRoute(RouteClosed, RouteActive), "CLOSED is terminal")
}

// TestLegHolding_NamesExactlyOneLedgerAccount: a leg's value is held in one
// place at every stage.
func TestLegHolding_NamesExactlyOneLedgerAccount(t *testing.T) {
	tx := func() *ledger.TransactionID { v := ledger.NewTransactionID(); return &v }
	cases := []struct {
		leg  Leg
		want LedgerHolding
	}{
		{Leg{Status: StateReserved}, HoldingSourceReserved},
		{Leg{Status: StateBridgeRequested}, HoldingSourceReserved},
		{Leg{Status: StateFailed}, HoldingNone},
		{Leg{Status: StateMessageInFlight, SourceJournal: tx()}, HoldingInFlight},
		{Leg{Status: StateManualReview, SourceJournal: tx()}, HoldingInFlight},
		{Leg{Status: StateReconciled, SourceJournal: tx(), DestinationJournal: tx()}, HoldingDestination},
		{Leg{Status: StateFailed, SourceJournal: tx(), RefundJournal: tx()}, HoldingNone},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.leg.Holding(), "%s", c.leg.Status)
	}
}

func TestDeriveNonce_IsDeterministicAndPerLeg(t *testing.T) {
	a, b := id.New[legKind](), id.New[legKind]()
	assert.Equal(t, deriveNonce(a), deriveNonce(a))
	assert.NotEqual(t, deriveNonce(a), deriveNonce(b))
}

func TestEnumLists_AreUnique(t *testing.T) {
	check := func(name string, xs []string) {
		seen := map[string]bool{}
		for _, x := range xs {
			require.False(t, seen[x], "%s lists %s twice", name, x)
			seen[x] = true
		}
	}
	check("states", strs(AllStates()))
	check("route statuses", strs(AllRouteStatuses()))
	check("attempt kinds", strs(AllAttemptKinds()))
	check("leg modes", strs(LegModes()))
}

func strs[T ~string](xs []T) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, string(x))
	}
	return out
}

func readMigration(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../migrations/00748_bridges.sql")
	require.NoError(t, err)
	return string(b)
}

// tableBody returns the column list of CREATE TABLE name.
func tableBody(t *testing.T, sql, name string) string {
	t.Helper()
	start := strings.Index(sql, "CREATE TABLE "+name+" (")
	require.GreaterOrEqual(t, start, 0, "table %s", name)
	end := strings.Index(sql[start:], "\n);")
	require.Greater(t, end, 0)
	return sql[start : start+end]
}

var quoted = regexp.MustCompile(`'([A-Z_]+)'`)

// checkIn returns the IN list of column's CHECK in table.
func checkIn(t *testing.T, sql, table, column string) []string {
	t.Helper()
	body := tableBody(t, sql, table)
	at := regexp.MustCompile(`\n\s+` + column + `\s+text NOT NULL CHECK \(` + column + ` IN \(`).FindStringIndex(body)
	require.NotNil(t, at, "%s.%s has a CHECK IN list", table, column)
	rest := body[at[1]:]
	list := rest[:strings.Index(rest, ")")]
	var out []string
	for _, m := range quoted.FindAllStringSubmatch(list, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func sorted(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

var pair = regexp.MustCompile(`\('([A-Z_]+)',\s*'([A-Z_]+)'\)`)

// edges returns the (from, to) pairs a transition function of the migration
// allows.
func edges(t *testing.T, sql, fn string) map[[2]string]bool {
	t.Helper()
	start := strings.Index(sql, "CREATE FUNCTION "+fn+"(")
	require.GreaterOrEqual(t, start, 0, "function %s", fn)
	end := strings.Index(sql[start:], "$$;")
	out := map[[2]string]bool{}
	for _, m := range pair.FindAllStringSubmatch(sql[start:start+end], -1) {
		out[[2]string{m[1], m[2]}] = true
	}
	require.NotEmpty(t, out)
	return out
}

// TestEnums_MatchTheMigrationsCheckLists pairs every CHECK IN list of
// migration 00748 with its Go twin, and the transition functions with the Go
// transition tables.
func TestEnums_MatchTheMigrationsCheckLists(t *testing.T) {
	sql := readMigration(t)
	assert.Equal(t, sorted(strs(AllStates())), checkIn(t, sql, "bridge_legs", "status"), "bridge_legs_status_check")
	assert.Equal(t, sorted(strs(LegModes())), checkIn(t, sql, "bridge_legs", "mode"), "bridge_legs_mode_check")
	assert.Equal(t, sorted(strs(AllRouteStatuses())), checkIn(t, sql, "bridge_routes", "status"), "bridge_routes_status_check")
	assert.Equal(t, sorted(strs(econ.CreatorActorTypes())), checkIn(t, sql, "bridge_routes", "created_by_actor_type"),
		"bridge_routes_created_by_actor_type_check")
	assert.Equal(t, sorted(strs(AllAttemptKinds())), checkIn(t, sql, "bridge_release_attempts", "kind"),
		"bridge_release_attempts_kind_check")

	legs := edges(t, sql, "cp_bridge_leg_transition_legal")
	for _, from := range AllStates() {
		for _, to := range AllStates() {
			assert.Equal(t, CanTransition(from, to), legs[[2]string{string(from), string(to)}], "leg %s -> %s", from, to)
		}
	}
	routes := edges(t, sql, "cp_bridge_route_transition_legal")
	for _, from := range AllRouteStatuses() {
		for _, to := range AllRouteStatuses() {
			assert.Equal(t, CanTransitionRoute(from, to), routes[[2]string{string(from), string(to)}], "route %s -> %s", from, to)
		}
	}
}

// TestSQLStates_AreDeclaredRaisedAndUnique: every custom SQLSTATE of the
// migration is listed in its header, raised by it, and five characters with
// the BR prefix.
func TestSQLStates_AreDeclaredRaisedAndUnique(t *testing.T) {
	sql := readMigration(t)
	header := sql[:strings.Index(sql, "CREATE TABLE")]
	raised := map[string]bool{}
	for _, m := range regexp.MustCompile(`ERRCODE = '([A-Z0-9]{5})'`).FindAllStringSubmatch(sql, -1) {
		raised[m[1]] = true
	}
	for _, code := range []string{"BR001", "BR002", "BR003", "BR004", "BR005"} {
		assert.Contains(t, header, "--   "+code+" ", "%s is documented in the header", code)
		assert.True(t, raised[code], "%s is raised", code)
	}
	for code := range raised {
		assert.True(t, strings.HasPrefix(code, "BR"), "%s uses the package prefix", code)
	}
	assert.Len(t, raised, 5)
}

// TestNoFloatingPointInSource enforces the "no floating-point financial
// values" non-negotiable mechanically, as internal/money does.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{
		"float32", "float64",
		"big.Float", "big.Rat",
		"ParseFloat", "FormatFloat",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc",
	}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- this package's own sources
		require.NoError(t, err)
		checked++
		text := string(src)
		for _, tok := range banned {
			if idx := strings.Index(text, tok); idx >= 0 {
				line := 1 + strings.Count(text[:idx], "\n")
				t.Errorf("%s:%d: forbidden token %q in non-test bridge source", name, line, tok)
			}
		}
	}
	require.Greater(t, checked, 8, "expected to scan the package's non-test sources")
}
