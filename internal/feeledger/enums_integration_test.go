//go:build integration

package feeledger

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
)

func strs[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}

var checkLiteral = regexp.MustCompile(`'([^']*)'::text`)

// TestIntegration_EveryEnumCheckMatchesItsGoList: each list a fee ledger
// table repeats in a CHECK is the Go list the code switches on, value for
// value, and no enum CHECK on a fee ledger table goes without one. A value in
// Go and not in the CHECK cannot be written; a value in the CHECK and not in
// Go is one no code handles.
func TestIntegration_EveryEnumCheckMatchesItsGoList(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	creators := strs(econ.CreatorActorTypes())
	pairs := map[string][]string{
		"fee_schedules.fee_schedules_created_by_actor_type_check":                                creators,
		"fee_quote_disclosures.fee_quote_disclosures_subsystem_check":                            strs(econ.AllSubsystems()),
		"fee_quote_disclosures.fee_quote_disclosures_side_check":                                 strs(AllSides()),
		"fee_bookings.fee_bookings_mode_check":                                                   strs(SupportedModes()),
		"fee_bookings.fee_bookings_booking_mode_check":                                           strs(AllBookingModes()),
		"fee_bookings.fee_bookings_journal_kind_check":                                           strs(AllBookingKinds()),
		"fee_bookings.fee_bookings_status_check":                                                 strs(AllBookingStatuses()),
		"fee_bookings.fee_bookings_reversed_by_actor_type_check":                                 creators,
		"fee_booking_transitions.fee_booking_transitions_actor_type_check":                       creators,
		"performance_fee_periods.performance_fee_periods_mode_check":                             strs(SupportedModes()),
		"performance_fee_periods.performance_fee_periods_status_check":                           strs(AllPeriodStatuses()),
		"performance_fee_periods.performance_fee_periods_opened_by_actor_type_check":             creators,
		"performance_fee_period_transitions.performance_fee_period_transitions_actor_type_check": creators,
		"performance_fee_flows.performance_fee_flows_recorded_by_actor_type_check":               creators,
		"performance_fee_lines.performance_fee_lines_kind_check":                                 strs(AllLineKinds()),
	}
	rows, err := testDB.Query(ctx, `SELECT conrelid::regclass::text || '.' || conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE contype = 'c' AND connamespace = 'public'::regnamespace
		  AND (conrelid::regclass::text LIKE 'fee\_%' OR conrelid::regclass::text LIKE 'performance\_fee\_%')
		  AND pg_get_constraintdef(oid) LIKE '%= ANY (ARRAY[%'`)
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
