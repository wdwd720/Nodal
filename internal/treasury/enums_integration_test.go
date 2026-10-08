//go:build integration

package treasury

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
)

func strs[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}

var checkLiteral = regexp.MustCompile(`'([^']*)'::text`)

// TestIntegration_EveryEnumCheckMatchesItsGoList: each list a treasury table
// repeats in a CHECK is the Go list the code switches on, value for value,
// and no enum CHECK on a treasury table goes without one. A value in Go and
// not in the CHECK cannot be written; a value in the CHECK and not in Go is
// one no code handles.
func TestIntegration_EveryEnumCheckMatchesItsGoList(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pairs := map[string][]string{
		"treasury_books.treasury_books_class_check":                         strs(AllClasses()),
		"treasury_risk_decisions.treasury_risk_decisions_intent_kind_check": strs(AllEvaluatedIntentKinds()),
		"treasury_risk_decisions.treasury_risk_decisions_stage_check":       strs(risk.AllStages()),
		"treasury_risk_decisions.treasury_risk_decisions_verdict_check":     strs(risk.AllVerdicts()),
		"treasury_paper_fills.treasury_paper_fills_activity_check":          strs(AllActivities()),
		"treasury_paper_fills.treasury_paper_fills_side_check":              strs(orders.AllSides()),
		"treasury_position_events.treasury_position_events_kind_check":      strs(AllPositionEventKinds()),
		"treasury_position_events.treasury_position_events_direction_check": strs(AllDirections()),
		"treasury_actions.treasury_actions_kind_check":                      strs(AllActionKinds()),
		"treasury_actions.treasury_actions_activity_check":                  strs(AllActivities()),
		"treasury_actions.treasury_actions_outcome_check":                   strs(AllOutcomes()),
		"treasury_actions.treasury_actions_actor_type_check":                strs(AuthorizedActorTypes()),
	}
	rows, err := testDB.Query(ctx, `SELECT conrelid::regclass::text || '.' || conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE contype = 'c' AND connamespace = 'public'::regnamespace
		  AND conrelid::regclass::text LIKE 'treasury\_%' AND pg_get_constraintdef(oid) LIKE '%= ANY (ARRAY[%'`)
	require.NoError(t, err)
	defs := map[string]string{}
	for rows.Next() {
		var name, def string
		require.NoError(t, rows.Scan(&name, &def))
		defs[name] = def
	}
	rows.Close()
	require.NoError(t, rows.Err())
	for name := range defs {
		_, ok := pairs[name]
		assert.True(t, ok, "%s is an enum CHECK with no Go list compared against it", name)
	}
	for name, want := range pairs {
		def, ok := defs[name]
		if !assert.True(t, ok, "%s is paired here but is not an enum CHECK in the schema", name) {
			continue
		}
		var got []string
		for _, m := range checkLiteral.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		want = append([]string(nil), want...)
		sort.Strings(got)
		sort.Strings(want)
		assert.Equal(t, want, got, "%s has diverged from its Go list", name)
	}
}
