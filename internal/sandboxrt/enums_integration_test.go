//go:build integration

package sandboxrt

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var checkLiteral = regexp.MustCompile(`'([^']*)'::text`)

// checkValues returns the string literals of a CHECK constraint, sorted.
func checkValues(t *testing.T, constraint string) []string {
	t.Helper()
	var def string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint).Scan(&def),
		"constraint %s", constraint)
	var out []string
	for _, m := range checkLiteral.FindAllStringSubmatch(def, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func sortedStrings[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

// TestIntegration_EveryCheckListHasItsGoTwin pairs each CHECK (col IN (...))
// of migration 00751 with the Go function that lists the same values.
func TestIntegration_EveryCheckListHasItsGoTwin(t *testing.T) {
	requireDB(t)
	pairs := map[string][]string{
		"sandbox_runs_termination_reason_check":                 sortedStrings(AllTerminations()),
		"sandbox_run_violations_code_check":                     sortedStrings(AllViolationCodes()),
		"sandbox_runs_actor_type_check":                         sortedStrings(RecordingActorTypes()),
		"strategy_artifacts_created_by_actor_type_check":        sortedStrings(RecordingActorTypes()),
		"strategy_artifact_sources_created_by_actor_type_check": sortedStrings(RecordingActorTypes()),
	}
	for constraint, goList := range pairs {
		assert.Equal(t, goList, checkValues(t, constraint), constraint)
	}
}
