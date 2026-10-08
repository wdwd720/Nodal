//go:build integration

package transfers

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var checkLiteral = regexp.MustCompile(`'([^']*)'::text`)

// checkValues returns the values an IN-list CHECK admits; a missing
// constraint is a failure, never agreement.
func checkValues(t *testing.T, table, constraint string) []string {
	t.Helper()
	var def string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = $1::regclass AND conname = $2`, table, constraint).Scan(&def), "%s.%s", table, constraint)
	var out []string
	for _, m := range checkLiteral.FindAllStringSubmatch(def, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func sortedStrings[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	sort.Strings(out)
	return out
}

// TestIntegration_EveryTransferEnumMatchesItsCheck: each Go list and the
// CHECK constraint holding the same list agree exactly (the pairs to register
// in test/integration/enums).
func TestIntegration_EveryTransferEnumMatchesItsCheck(t *testing.T) {
	requireEnv(t)
	for _, p := range []struct {
		table, constraint string
		values            []string
	}{
		{"internal_transfers", "internal_transfers_status_check", sortedStrings(AllStatuses())},
		{"internal_transfers", "internal_transfers_mode_check", sortedStrings(SupportedModes())},
		{"transfer_recipient_rules", "transfer_recipient_rules_rule_check", sortedStrings(AllRuleKinds())},
		{"transfer_sender_modes", "transfer_sender_modes_mode_check", sortedStrings(AllSenderModes())},
		// Migration 00758 (reversal under dual control).
		{"transfer_reversal_decisions", "transfer_reversal_decisions_decision_check", sortedStrings(AllReversalDecisions())},
	} {
		assert.Equal(t, p.values, checkValues(t, p.table, p.constraint), p.constraint)
	}

	// The IN-list CHECKs on the tables of migrations 00744 and 00758 are
	// exactly the seven that have a Go twin (five above, two in
	// internal/sandboxfund): nothing for test/integration/enums to find
	// unregistered.
	rows, err := testDB.Query(context.Background(), `SELECT conrelid::regclass::text || '.' || conname FROM pg_constraint
		WHERE contype = 'c' AND connamespace = 'public'::regnamespace
		  AND conrelid::regclass::text IN ('internal_transfers','internal_transfer_transitions','transfer_handles',
		                                   'transfer_recipient_rules','transfer_sender_modes','sandbox_fundings',
		                                   'transfer_reversal_proposals','transfer_reversal_decisions')
		  AND pg_get_constraintdef(oid) LIKE '%= ANY (ARRAY[%' ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var lists []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		lists = append(lists, s)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"internal_transfers.internal_transfers_mode_check",
		"internal_transfers.internal_transfers_status_check",
		"sandbox_fundings.sandbox_fundings_actor_type_check",
		"sandbox_fundings.sandbox_fundings_environment_check",
		"transfer_recipient_rules.transfer_recipient_rules_rule_check",
		"transfer_reversal_decisions.transfer_reversal_decisions_decision_check",
		"transfer_sender_modes.transfer_sender_modes_mode_check",
	}, lists)
}
