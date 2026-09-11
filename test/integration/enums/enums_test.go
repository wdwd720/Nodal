//go:build integration

// Package enums_test compares every enum list declared in Go against the CHECK
// constraint that holds the same list in the schema.
//
// A list kept in two languages diverges. This repository has produced that
// defect repeatedly, and had three tests named for the comparison --
// TestSkipReasonsMirrorTheDatabaseCheck, TestPauseReasonsMirrorTheDatabaseCheck
// and internal/prediction's TestModesMirrorTheDatabaseCheck -- none of which
// opened a database. Each asserted a hardcoded length and then that every
// member of a list is a member of that list, which holds however far the CHECK
// has drifted. They forced a human to bump a number; they could not detect what
// their names promised (F-74).
//
// The failure they were meant to catch arrives at the worst possible moment. A
// value declared in Go and absent from the CHECK is a row that cannot be
// written at all, and the constraint violation lands exactly when the thing
// being recorded has already gone wrong: a skipped run, a raised pause, a
// halted market. The other direction is quieter and worse -- a value the
// database accepts that no switch in the code handles.
package enums_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/operatorroles"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/reality"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/terms"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "enums: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "enums-itest", MaxConns: 4}); err != nil {
		fmt.Fprintln(os.Stderr, "enums: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL to run the enum agreement suite")
	}
}

// str widens any string-kinded enum to []string so one comparison serves all of
// them.
func str[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}

// pair is one Go declaration and the CHECK constraint that repeats it.
type pair struct {
	table      string
	constraint string
	// source is the Go expression, named so a failure says where to look.
	source string
	values []string
}

// registry is the set of enum lists that exist in both languages. It is
// deliberately explicit: pairing by "these two sets happen to be equal" would
// pass by construction and prove nothing.
func registry() []pair {
	// One list, twelve tables. agent.Modes, intent.Modes and prediction.Modes
	// are three Go types over the same six values, which TestTheThreeModeLists
	// AgreeWithEachOther keeps in step.
	modes := func(table string) pair {
		return pair{table: table, constraint: table + "_mode_check", source: "agent.Modes()", values: str(agent.Modes())}
	}
	out := []pair{
		{table: "admin_actions", constraint: "admin_actions_status_check", source: "admin.AllStatuses()", values: str(admin.AllStatuses())},
		{table: "agent_pauses", constraint: "agent_pauses_reason_code_check", source: "agent.PauseReasons()", values: str(agent.PauseReasons())},
		{table: "agent_runs", constraint: "agent_runs_skip_reason_check", source: "agent.SkipReasons()", values: str(agent.SkipReasons())},
		{table: "agent_runs", constraint: "agent_runs_status_check", source: "agent.RunStatuses()", values: str(agent.RunStatuses())},
		{table: "agents", constraint: "agents_stage_check", source: "agent.Stages()", values: str(agent.Stages())},
		{table: "agents", constraint: "agents_state_check", source: "agent.States()", values: str(agent.States())},
		{table: "assets", constraint: "assets_kind_check", source: "assets.AllKinds()", values: str(assets.AllKinds())},
		{table: "capability_gates", constraint: "capability_gates_capability_check", source: "gates.AllCapabilities()", values: str(gates.AllCapabilities())},
		{table: "capability_gates", constraint: "capability_gates_state_check", source: "gates.AllStates()", values: str(gates.AllStates())},
		{table: "credit_fundings", constraint: "credit_fundings_state_check", source: "credit.AllFundingStates()", values: str(credit.AllFundingStates())},
		{table: "deposits", constraint: "deposits_status_check", source: "funding.AllStatuses()", values: str(funding.AllStatuses())},
		{table: "execution_attempts", constraint: "execution_attempts_finality_check", source: "execution.AllFinalityLevels()", values: str(execution.AllFinalityLevels())},
		{table: "execution_attempts", constraint: "execution_attempts_status_check", source: "execution.AllAttemptStatuses()", values: str(execution.AllAttemptStatuses())},
		{table: "internal_products", constraint: "internal_products_kind_check", source: "commerce.AllKinds()", values: str(commerce.AllKinds())},
		{table: "internal_products", constraint: "internal_products_status_check", source: "commerce.AllStatuses()", values: str(commerce.AllStatuses())},
		{table: "kill_switches", constraint: "kill_switches_kind_check", source: "killswitch.AllKinds()", values: str(killswitch.AllKinds())},
		{table: "orders", constraint: "orders_status_check", source: "execution.AllOrderStatuses()", values: str(execution.AllOrderStatuses())},
		{table: "tools", constraint: "tools_status_check", source: "agent.ToolStatuses()", values: str(agent.ToolStatuses())},
		{table: "trade_intents", constraint: "trade_intents_action_check", source: "intent.Actions()", values: str(intent.Actions())},
		{table: "trade_intents", constraint: "trade_intents_status_check", source: "intent.Statuses()", values: str(intent.Statuses())},

		// Paired 2026-09-09 while working through the unpaired inventory. Each
		// is a domain match, not a set match: the Go list named is the one that
		// DECLARES the values the column holds.
		//
		// Three constraints were deliberately left unpaired in the same pass.
		// data_sources, venues and venue_listings all hold ACTIVE/DEGRADED/
		// DISABLED, which is exactly agent.ToolStatuses() -- and pairing them
		// with it would be the "these two sets happen to be equal" mistake this
		// registry exists to avoid. A tool's health and a venue's listing
		// status are different facts that agree today by coincidence.
		{table: "ledger_accounts", constraint: "ledger_accounts_code_check", source: "ledger.AllCodes()", values: str(ledger.AllCodes())},
		{table: "native_assets", constraint: "native_assets_status_check", source: "nativeasset.AllStatuses()", values: str(nativeasset.AllStatuses())},
		{table: "native_markets", constraint: "native_markets_status_check", source: "nativemarket.AllStatuses()", values: str(nativemarket.AllStatuses())},
		{table: "payout_requests", constraint: "payout_requests_state_check", source: "payout.AllStates()", values: str(payout.AllStates())},
		{table: "payout_destinations", constraint: "payout_destinations_kind_check", source: "payout.AllDestinationKinds()", values: str(payout.AllDestinationKinds())},
		{table: "reconciliation_records", constraint: "reconciliation_records_kind_check", source: "reconciliation.AllKinds()", values: str(reconciliation.AllKinds())},
		{table: "reconciliation_records", constraint: "reconciliation_records_status_check", source: "reconciliation.AllStatuses()", values: str(reconciliation.AllStatuses())},
		{table: "data_sources", constraint: "data_sources_retention_class_check", source: "reality.RetentionClasses()", values: str(reality.RetentionClasses())},
		{table: "raw_archive_objects", constraint: "raw_archive_objects_retention_class_check", source: "reality.RetentionClasses()", values: str(reality.RetentionClasses())},

		// Paired with the product surfaces (00756-00760). Each is a domain
		// match: the Go list named is the one that DECLARES the values.
		//
		// operator_roles is the one worth reading twice. Its CHECK is
		// security.AllRoles() MINUS break-glass, and operatorroles.Directory()
		// computes exactly that subtraction -- so a role added to the matrix
		// and not to the CHECK fails here, and so does a CHECK that quietly
		// re-admits BREAK_GLASS as a standing role.
		{table: "terms_acceptances", constraint: "terms_acceptances_document_id_check", source: "terms.AllDocumentIDs()", values: str(terms.AllDocumentIDs())},
		{table: "account_closure_requests", constraint: "account_closure_requests_state_check", source: "profile.AllClosureStates()", values: str(profile.AllClosureStates())},
		{table: "operator_roles", constraint: "operator_roles_role_check", source: "operatorroles.Directory()", values: str(operatorroles.Directory())},
	}
	for _, table := range []string{
		"agent_runs", "agents", "calibration_snapshots", "cost_accounting", "counterfactuals",
		"model_calls", "orders", "performance_snapshots", "prediction_outcomes", "predictions",
		"tool_invocations", "trade_intents",
	} {
		out = append(out, modes(table))
	}
	return out
}

var literal = regexp.MustCompile(`'([^']*)'::text`)

// checkLiterals returns the values a CHECK constraint admits, and fails if the
// constraint does not exist: a renamed or dropped constraint must not read as
// agreement.
func checkLiterals(t *testing.T, table, constraint string) []string {
	t.Helper()
	var def string
	err := testDB.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = $1::regclass AND conname = $2`, table, constraint).Scan(&def)
	require.NoErrorf(t, err, "constraint %s.%s is registered here but not in the schema", table, constraint)
	found := literal.FindAllStringSubmatch(def, -1)
	require.NotEmptyf(t, found, "no literals in %q: %s is not an enum CHECK", def, constraint)
	out := make([]string, 0, len(found))
	for _, m := range found {
		out = append(out, m[1])
	}
	return out
}

// TestIntegration_EveryDeclaredEnumMatchesItsCheck is the comparison the three
// misnamed unit tests never made.
func TestIntegration_EveryDeclaredEnumMatchesItsCheck(t *testing.T) {
	requireEnv(t)
	for _, p := range registry() {
		t.Run(p.constraint, func(t *testing.T) {
			got := checkLiterals(t, p.table, p.constraint)
			want := append([]string(nil), p.values...)
			sort.Strings(got)
			sort.Strings(want)
			assert.Equalf(t, want, got,
				"%s and %s.%s have diverged: a value in Go and not in the CHECK cannot be written at all, "+
					"and a value in the CHECK and not in Go is one that no switch in the code handles",
				p.source, p.table, p.constraint)
		})
	}
}

// TestTheThreeModeListsAgreeWithEachOther: agent.Modes, intent.Modes and
// prediction.Modes are three Go types over one set of values, and twelve tables
// repeat it. Comparing each against the schema separately would not catch two
// of them drifting together.
func TestTheThreeModeListsAgreeWithEachOther(t *testing.T) {
	a, i, p := str(agent.Modes()), str(intent.Modes()), str(prediction.Modes())
	sort.Strings(a)
	sort.Strings(i)
	sort.Strings(p)
	assert.Equal(t, a, i, "agent.Modes and intent.Modes have diverged")
	assert.Equal(t, a, p, "agent.Modes and prediction.Modes have diverged")
}

// bespoke names the constraints compared by a test of their own because their
// shape is not a flat list of values.
var bespoke = []string{
	"agents.agents_check3",
	// 00750's mirror of agents_check3 onto the transition row. Compared with
	// the original by TestIntegration_TheStageModeMappingMatchesTheDatabase,
	// which is the strongest pairing available: not against a Go list, but
	// against the constraint it is a copy of.
	"agent_lifecycle_transitions.agent_lifecycle_transitions_destination_is_legal",
}

var (
	stageNullMode = regexp.MustCompile(`\(stage = ANY \(ARRAY\[([^\]]*)\]\)\) AND \(mode IS NULL\)`)
	stageModeAny  = regexp.MustCompile(`\(stage = '([A-Z_]+)'::text\) AND \(mode = ANY \(ARRAY\[([^\]]*)\]\)\)`)
	stageModeOne  = regexp.MustCompile(`\(stage = '([A-Z_]+)'::text\) AND \(mode = '([A-Z_]+)'::text\)`)
)

// TestIntegration_TheStageModeMappingMatchesTheDatabase covers agents_check3,
// which is not a list but the stage-to-mode mapping itself:
//
//	((stage = ANY (ARRAY['DRAFT','COMPILED','VALIDATED'])) AND (mode IS NULL))
//	 OR ((stage = 'BACKTEST_ELIGIBLE') AND (mode = ANY (ARRAY['BACKTEST','PAPER'])))
//	 OR ((stage = 'SHADOW') AND (mode = 'SHADOW')) ...
//
// internal/agent's ModesForStage is the same mapping in Go, and
// TestModeMappingMirrorsTheDatabaseCheck asserts it against a table typed out
// by hand in that test file -- a third copy, compared with the second, while
// the constraint that actually refuses the write went unread. A stage allowed
// one more mode in Go than in the CHECK is a promotion that fails at the
// database with a bare constraint violation.
func TestIntegration_TheStageModeMappingMatchesTheDatabase(t *testing.T) {
	requireEnv(t)
	var def string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = 'agents'::regclass AND conname = 'agents_check3'`).Scan(&def))

	inDB := map[string][]string{}
	for _, m := range stageNullMode.FindAllStringSubmatch(def, -1) {
		for _, st := range literal.FindAllStringSubmatch(m[1], -1) {
			inDB[st[1]] = nil
		}
	}
	for _, m := range stageModeAny.FindAllStringSubmatch(def, -1) {
		for _, md := range literal.FindAllStringSubmatch(m[2], -1) {
			inDB[m[1]] = append(inDB[m[1]], md[1])
		}
	}
	for _, m := range stageModeOne.FindAllStringSubmatch(def, -1) {
		inDB[m[1]] = append(inDB[m[1]], m[2])
	}
	require.Len(t, inDB, len(agent.Stages()),
		"the parse found %d stages in %q; every stage must appear or the comparison below is partial", len(inDB), def)

	for _, st := range agent.Stages() {
		t.Run(string(st), func(t *testing.T) {
			// Both sides are normalised to a non-nil empty slice: DRAFT,
			// COMPILED and VALIDATED carry no mode at all, and "no modes" must
			// compare equal however each side spells it.
			got := append([]string{}, inDB[string(st)]...)
			want := append([]string{}, str(agent.ModesForStage(st))...)
			sort.Strings(got)
			sort.Strings(want)
			assert.Equal(t, want, got, "ModesForStage(%s) and agents_check3 have diverged", st)
		})
	}

	// The third copy, and the reason this test grew rather than the unpaired
	// list.
	//
	// 00750 mirrors agents_check3 onto agent_lifecycle_transitions, against
	// to_stage and to_mode, so that a transition row which does not describe a
	// legal agent is refused where it is WRITTEN instead of surfacing as a
	// violation on a table the caller never touched. That migration says the two
	// "cannot drift unnoticed, because the agent's own CHECKs still stand" --
	// which is true of correctness and not of maintenance: a mirror that
	// permitted LESS would refuse legitimate promotions, and nothing would say
	// why.
	//
	// So the mirror is compared with the original, mapping to_stage/to_mode back
	// onto stage/mode, and this assertion is what makes that sentence in 00750
	// true rather than hopeful.
	var mirror string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = 'agent_lifecycle_transitions'::regclass
			  AND conname = 'agent_lifecycle_transitions_destination_is_legal'`).Scan(&mirror))
	normalised := strings.NewReplacer("to_stage", "stage", "to_mode", "mode").Replace(mirror)

	inMirror := map[string][]string{}
	for _, m := range stageNullMode.FindAllStringSubmatch(normalised, -1) {
		for _, st := range literal.FindAllStringSubmatch(m[1], -1) {
			inMirror[st[1]] = nil
		}
	}
	for _, m := range stageModeAny.FindAllStringSubmatch(normalised, -1) {
		for _, md := range literal.FindAllStringSubmatch(m[2], -1) {
			inMirror[m[1]] = append(inMirror[m[1]], md[1])
		}
	}
	for _, m := range stageModeOne.FindAllStringSubmatch(normalised, -1) {
		inMirror[m[1]] = append(inMirror[m[1]], m[2])
	}
	require.Len(t, inMirror, len(agent.Stages()),
		"the parse found %d stages in the mirrored CHECK; every stage must appear or this comparison is partial", len(inMirror))

	for _, st := range agent.Stages() {
		a := append([]string{}, inDB[string(st)]...)
		b := append([]string{}, inMirror[string(st)]...)
		sort.Strings(a)
		sort.Strings(b)
		assert.Equal(t, a, b,
			"%s: agents_check3 and its mirror on agent_lifecycle_transitions have diverged; "+
				"a mirror that permits less refuses legitimate promotions and a mirror that permits more lets an illegal row through", st)
	}
}

// TestIntegration_NoEnumCheckAppearsUnnoticed enumerates the schema's enum
// CHECKs and names every one the registry does not cover. A new enum column
// therefore fails this test until somebody decides whether it has a Go
// counterpart worth comparing it against -- the decision that was never made
// for the ones already listed below.
//
// The list is expected to SHRINK. Adding a name to it is admitting a second
// copy of a list with nothing keeping the two in step.
func TestIntegration_NoEnumCheckAppearsUnnoticed(t *testing.T) {
	requireEnv(t)
	rows, err := testDB.Query(context.Background(),
		// Partitions are excluded, and that is not a convenience. A partition
		// carries a copy of every CHECK its parent declares -- PostgreSQL puts
		// it there, nobody wrote it -- so counting them would make this
		// inventory grow by thirteen every time a table is partitioned and
		// shrink every time a month is dropped. The decision this test tracks
		// is "someone declared an enum in SQL", and that happens once, on the
		// parent (00740).
		`SELECT c.conrelid::regclass::text || '.' || c.conname
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			WHERE c.contype = 'c' AND c.connamespace = 'public'::regnamespace
			  AND NOT t.relispartition
			  AND pg_get_constraintdef(c.oid) LIKE '%= ANY (ARRAY[%'
			ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var all []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		all = append(all, s)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, all, "no enum CHECK constraints found at all: the query is wrong, not the schema")

	known := map[string]bool{}
	for _, p := range registry() {
		known[p.table+"."+p.constraint] = true
	}
	// Constraints whose shape is not a flat list and which therefore have a
	// test of their own rather than a registry entry.
	for _, c := range bespoke {
		known[c] = true
	}
	unregistered := []string{}
	for _, s := range all {
		if !known[s] {
			unregistered = append(unregistered, s)
		}
	}
	assert.Equal(t, unpaired, unregistered,
		"an enum CHECK appeared or disappeared without a decision about its Go counterpart")
}

// unpaired names the enum CHECK constraints that no Go list is compared
// against today. Each is a list the schema holds and nothing verifies against
// the code. It is an inventory, not an allow-list: the right number is zero.
// intent_transitions_rejection_code_check names the six terminal statuses, and
// it is listed here rather than paired because the list it repeats is
// intent.terminalStatuses, which is UNEXPORTED. Pairing it would mean exporting
// a Go internal purely so a test could read it, which trades a real
// encapsulation for a check the CHECK itself already enforces on every write.
// The rule is asserted from the outside instead, by
// internal/intent's own tests driving a rejection code onto a non-terminal
// transition and watching it refused (00749).
var unpaired = []string{
	"accounts.accounts_kind_check",
	"accounts.accounts_status_check",
	"agent_lifecycle_transitions.agent_lifecycle_transitions_actor_type_check",
	"agent_pauses.agent_pauses_open_orders_policy_check",
	"agent_pauses.agent_pauses_paused_by_actor_type_check",
	"agent_pauses.agent_pauses_resumed_by_actor_type_check",
	"agent_runs.agent_runs_trigger_kind_check",
	"agents.agents_check",
	"asset_policies.asset_policies_stablecoin_status_check",
	"asset_policies.asset_policies_status_check",
	"asset_reservations.asset_reservations_status_check",
	"assets.assets_internal_chain_check",
	"assets.assets_kind_domain_agree",
	"assets.assets_risk_class_check",
	"assets.assets_status_check",
	"assets.assets_value_domain_check",
	"audit_checkpoints.audit_checkpoints_signer_check",
	"backtests.backtests_check2",
	"backtests.backtests_model_mode_check",
	"backtests.backtests_pit_validity_check",
	"backtests.backtests_status_check",
	"capability_gate_transitions.capability_gate_transitions_actor_type_check",
	"capability_gates.capability_gates_environment_check",
	"capital_envelopes.capital_envelopes_status_check",
	"compile_attempts.compile_attempts_outcome_check",
	"compile_attempts.compile_attempts_parse_result_check",
	"compile_attempts.compile_attempts_source_kind_check",
	"compile_attempts.compile_attempts_stage_reached_check",
	"compliance_profiles.compliance_profiles_identity_state_check",
	"compliance_profiles.compliance_profiles_sanctions_state_check",
	"cost_accounting.cost_accounting_kind_check",
	"counterfactuals.counterfactuals_branch_check",
	"counterfactuals.counterfactuals_subject_kind_check",
	"credit_lot_events.credit_lot_events_check1",
	"credit_lot_events.credit_lot_events_kind_check",
	"credit_lot_events.credit_lot_events_to_finality_check",
	"credit_lots.credit_lots_initial_finality_check",
	"credit_lots.credit_lots_origin_check",
	"data_sources.data_sources_dedup_strategy_check",
	"data_sources.data_sources_historical_use_permitted_check",
	"data_sources.data_sources_kind_check",
	"data_sources.data_sources_persistence_capability_check",
	"data_sources.data_sources_redistribution_policy_check",
	"data_sources.data_sources_status_check",
	"deposits.deposits_fraud_state_check",
	"economic_exposures.economic_exposures_kind_check",
	"eligibility_decisions.eligibility_decisions_context_kind_check",
	"eligibility_policies.eligibility_policies_created_by_actor_type_check",
	"execution_plan_steps.execution_plan_steps_retry_class_check",
	"execution_plan_steps.execution_plan_steps_state_check",
	"execution_plan_steps.execution_plan_steps_type_check",
	"execution_plans.execution_plans_status_check",
	"external_identifiers.external_identifiers_entity_type_check",
	"fills.fills_finality_check",
	"fills.fills_source_check",
	"funding_sources.funding_sources_kind_check",
	"funding_sources.funding_sources_status_check",
	"idempotency_keys.idempotency_keys_status_check",
	"inbox_messages.inbox_messages_status_check",
	"ingest_checkpoints.ingest_checkpoints_status_check",
	"instruments.instruments_risk_class_check",
	"instruments.instruments_status_check",
	"instruments.instruments_type_check",
	"intent_transitions.intent_transitions_rejection_code_check",
	"internal_commerce_orders.internal_commerce_orders_earning_origin_check",
	"internal_sellers.internal_sellers_status_check",
	"journal_entries.journal_entries_side_check",
	"journal_transactions.journal_transactions_kind_check",
	"kill_switch_transitions.kill_switch_transitions_actor_type_check",
	"kill_switches.kill_switches_severity_check",
	"ledger_accounts.ledger_accounts_normal_side_check",
	"ledger_accounts.ledger_accounts_owner_type_check",
	"ledger_accounts.ledger_accounts_status_check",
	"ledger_accounts.ledger_accounts_value_domain_check",
	"login_attempts.login_attempts_outcome_check",
	"model_calls.model_calls_parse_result_check",
	"model_calls.model_calls_purpose_check",
	"native_assets.native_assets_content_moderation_state_check",
	"native_market_alerts.native_market_alerts_kind_check",
	"native_market_alerts.native_market_alerts_severity_check",
	"native_market_fills.native_market_fills_side_check",
	"native_market_quotes.native_market_quotes_side_check",
	"notifications.notifications_kind_check",
	"notifications.notifications_severity_check",
	"orders.orders_side_check",
	"payout_destinations.payout_destinations_status_check",
	"payout_provider_events.payout_provider_events_direction_check",
	"performance_snapshots.performance_snapshots_scope_kind_check",
	"position_lots.position_lots_status_check",
	"prediction_outcomes.prediction_outcomes_realized_direction_check",
	"predictions.predictions_direction_check",
	"provider_events.provider_events_processing_status_check",
	"provider_health_samples.provider_health_samples_role_check",
	"provider_health_samples.provider_health_samples_state_check",
	"quotes.quotes_side_check",
	"reconciliation_records.reconciliation_records_mode_check",
	"risk_decisions.risk_decisions_decision_check",
	"risk_decisions.risk_decisions_stage_check",
	"risk_policies.risk_policies_created_by_actor_type_check",
	"risk_policies.risk_policies_scope_check",
	"security_events.security_events_severity_check",
	"sessions.sessions_actor_type_check",
	"signing_decisions.signing_decisions_decision_check",
	"signing_results.signing_results_retry_class_check",
	"strategies.strategies_source_kind_check",
	"strategies.strategies_status_check",
	"strategy_dependencies.strategy_dependencies_effect_check",
	"strategy_dependencies.strategy_dependencies_kind_check",
	"strategy_versions.strategy_versions_source_kind_check",
	"strategy_versions.strategy_versions_status_check",
	"stream_gaps.stream_gaps_kind_check",
	"stream_gaps.stream_gaps_resolution_check",
	"stream_gaps.stream_gaps_resolved_by_actor_type_check",
	// Unpaired for the reason sessions_actor_type_check is: the column holds a
	// deliberate SUBSET of security.AllActorTypes() -- USER and OPERATOR, never
	// SERVICE, AGENT or SYSTEM -- so pairing it with the full list would fail,
	// and pairing it with a hand-written subset would be the coincidence this
	// registry exists to avoid.
	"terms_acceptances.terms_acceptances_actor_type_check",
	"tools.tools_effect_check",
	"trade_intents.trade_intents_actor_type_check",
	"users.users_status_check",
	"venue_listings.venue_listings_status_check",
	"venues.venues_kind_check",
	"venues.venues_status_check",
	"wallet_status_transitions.wallet_status_transitions_to_status_check",
	"wallets.wallets_status_check",
	"withdrawals.withdrawals_status_check",
}
