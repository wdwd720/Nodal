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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxfund"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/treasury"
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

		// Migration 00731 (advanced capabilities). Every enum it introduced
		// is paired, and so are the four it widened that had been unpaired
		// until then: the chart, the owner types, the journal kinds and the
		// reconciliation kinds.
		{table: "ledger_accounts", constraint: "ledger_accounts_code_check", source: "ledger.AllCodes()", values: str(ledger.AllCodes())},
		{table: "ledger_accounts", constraint: "ledger_accounts_owner_type_check", source: "ledger.AllOwnerTypes()", values: str(ledger.AllOwnerTypes())},
		{table: "journal_transactions", constraint: "journal_transactions_kind_check", source: "ledger.AllKinds()", values: str(ledger.AllKinds())},
		{table: "reconciliation_records", constraint: "reconciliation_records_kind_check", source: "reconciliation.AllKinds()", values: str(reconciliation.AllKinds())},
		{table: "facilities", constraint: "facilities_kind_check", source: "ledger.AllFacilityKinds()", values: str(ledger.AllFacilityKinds())},
		{table: "facilities", constraint: "facilities_status_check", source: "econ.AllFacilityStatuses()", values: str(econ.AllFacilityStatuses())},
		{table: "facilities", constraint: "facilities_created_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "house_accounts", constraint: "house_accounts_status_check", source: "econ.AllHouseStatuses()", values: str(econ.AllHouseStatuses())},
		{table: "house_accounts", constraint: "house_accounts_strategy_check", source: "econ.AllHouseStrategies()", values: str(econ.AllHouseStrategies())},
		{table: "house_accounts", constraint: "house_accounts_created_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "economic_intents", constraint: "economic_intents_kind_check", source: "econ.AllIntentKinds()", values: str(econ.AllIntentKinds())},
		{table: "economic_intents", constraint: "economic_intents_mode_check", source: "econ.PersistableModes()", values: str(econ.PersistableModes())},
		{table: "economic_intents", constraint: "economic_intents_capability_check", source: "econ.AdvancedCapabilities()", values: str(econ.AdvancedCapabilities())},
		{table: "economic_intents", constraint: "economic_intents_status_check", source: "econ.AllIntentStatuses()", values: str(econ.AllIntentStatuses())},
		{table: "economic_intents", constraint: "economic_intents_actor_type_check", source: "security.AllActorTypes()", values: str(security.AllActorTypes())},
		{table: "economic_effects", constraint: "economic_effects_subsystem_check", source: "econ.AllSubsystems()", values: str(econ.AllSubsystems())},
		{table: "economic_effects", constraint: "economic_effects_status_check", source: "econ.AllEffectStatuses()", values: str(econ.AllEffectStatuses())},
		{table: "advanced_risk_decisions", constraint: "advanced_risk_decisions_stage_check", source: "risk.AllStages()", values: str(risk.AllStages())},
		{table: "advanced_risk_decisions", constraint: "advanced_risk_decisions_verdict_check", source: "risk.AllVerdicts()", values: str(risk.AllVerdicts())},

		// Migration 00732 (canonical instruments), which also pairs the
		// instrument type list that had been unpaired since 00200.
		{table: "instruments", constraint: "instruments_type_check", source: "instruments.AllTypes()", values: str(instruments.AllTypes())},
		{table: "instrument_terms", constraint: "instrument_terms_kind_check", source: "instruments.AllTypes()", values: str(instruments.AllTypes())},
		{table: "instrument_terms", constraint: "instrument_terms_option_right_check", source: "instruments.AllOptionRights()", values: str(instruments.AllOptionRights())},
		{table: "instrument_terms", constraint: "instrument_terms_exercise_style_check", source: "instruments.AllExerciseStyles()", values: str(instruments.AllExerciseStyles())},
		{table: "instrument_terms", constraint: "instrument_terms_settlement_style_check", source: "instruments.AllSettlementStyles()", values: str(instruments.AllSettlementStyles())},
		{table: "instrument_terms", constraint: "instrument_terms_margin_model_check", source: "instruments.AllMarginModels()", values: str(instruments.AllMarginModels())},
		{table: "instrument_terms", constraint: "instrument_terms_corporate_action_behavior_check", source: "instruments.AllCorporateActionBehaviors()", values: str(instruments.AllCorporateActionBehaviors())},
		{table: "instrument_terms", constraint: "instrument_terms_transferability_check", source: "instruments.AllTransferabilities()", values: str(instruments.AllTransferabilities())},

		// Migration 00733 (kernel amendments), which pairs the notification
		// kind list that had been unpaired since 00640.
		{table: "notifications", constraint: "notifications_kind_check", source: "notification.AllKinds()", values: str(notification.AllKinds())},

		// Migration 00744 (customer-to-customer transfers and the faucet).
		{table: "internal_transfers", constraint: "internal_transfers_status_check", source: "transfers.AllStatuses()", values: str(transfers.AllStatuses())},
		{table: "internal_transfers", constraint: "internal_transfers_mode_check", source: "transfers.SupportedModes()", values: str(transfers.SupportedModes())},
		{table: "transfer_recipient_rules", constraint: "transfer_recipient_rules_rule_check", source: "transfers.AllRuleKinds()", values: str(transfers.AllRuleKinds())},
		{table: "transfer_sender_modes", constraint: "transfer_sender_modes_mode_check", source: "transfers.AllSenderModes()", values: str(transfers.AllSenderModes())},
		{table: "sandbox_fundings", constraint: "sandbox_fundings_environment_check", source: "sandboxfund.AllEnvironments()", values: str(sandboxfund.AllEnvironments())},
		{table: "sandbox_fundings", constraint: "sandbox_fundings_actor_type_check", source: "sandboxfund.AllowedActorTypes()", values: str(sandboxfund.AllowedActorTypes())},

		// Migration 00740 (margin, leverage, liquidation).
		{table: "margin_accounts", constraint: "margin_accounts_margin_mode_check", source: "margin.AllMarginModes()", values: str(margin.AllMarginModes())},
		{table: "margin_accounts", constraint: "margin_accounts_mode_check", source: "margin.RunModes()", values: str(margin.RunModes())},
		{table: "margin_accounts", constraint: "margin_accounts_status_check", source: "margin.AllAccountStatuses()", values: str(margin.AllAccountStatuses())},
		{table: "margin_accounts", constraint: "margin_accounts_opened_by_actor_type_check", source: "margin.OpenerActorTypes()", values: str(margin.OpenerActorTypes())},
		{table: "margin_borrow_lots", constraint: "margin_borrow_lots_close_reason_check", source: "margin.AllLotCloseReasons()", values: str(margin.AllLotCloseReasons())},
		{table: "margin_collateral_moves", constraint: "margin_collateral_moves_direction_check", source: "margin.AllMoveDirections()", values: str(margin.AllMoveDirections())},
		{table: "margin_calls", constraint: "margin_calls_status_check", source: "margin.AllCallStatuses()", values: str(margin.AllCallStatuses())},
		{table: "margin_liquidation_cases", constraint: "margin_liquidation_cases_status_check", source: "margin.AllCaseStatuses()", values: str(margin.AllCaseStatuses())},
		{table: "margin_liquidation_cases", constraint: "margin_liquidation_cases_target_check", source: "margin.AllLiquidationTargets()", values: str(margin.AllLiquidationTargets())},
		{table: "margin_liquidation_steps", constraint: "margin_liquidation_steps_action_check", source: "margin.AllStepActions()", values: str(margin.AllStepActions())},
		// 00754: a MANUAL_REVIEW case leaves only through a resolution one
		// operator proposes and a second approves.
		{table: "margin_case_resolutions", constraint: "margin_case_resolutions_outcome_check", source: "margin.AllResolutionOutcomes()", values: str(margin.AllResolutionOutcomes())},
		{table: "margin_case_resolution_decisions", constraint: "margin_case_resolution_decisions_decision_check", source: "margin.AllResolutionDecisions()", values: str(margin.AllResolutionDecisions())},
		// 00745 treasury and 00746 fee ledger.
		{table: "treasury_books", constraint: "treasury_books_class_check", source: "treasury.AllClasses()", values: str(treasury.AllClasses())},
		{table: "treasury_risk_decisions", constraint: "treasury_risk_decisions_intent_kind_check", source: "treasury.AllEvaluatedIntentKinds()", values: str(treasury.AllEvaluatedIntentKinds())},
		{table: "treasury_risk_decisions", constraint: "treasury_risk_decisions_stage_check", source: "risk.AllStages()", values: str(risk.AllStages())},
		{table: "treasury_risk_decisions", constraint: "treasury_risk_decisions_verdict_check", source: "risk.AllVerdicts()", values: str(risk.AllVerdicts())},
		{table: "treasury_paper_fills", constraint: "treasury_paper_fills_activity_check", source: "treasury.AllActivities()", values: str(treasury.AllActivities())},
		{table: "treasury_paper_fills", constraint: "treasury_paper_fills_side_check", source: "orders.AllSides()", values: str(orders.AllSides())},
		{table: "treasury_position_events", constraint: "treasury_position_events_kind_check", source: "treasury.AllPositionEventKinds()", values: str(treasury.AllPositionEventKinds())},
		{table: "treasury_position_events", constraint: "treasury_position_events_direction_check", source: "treasury.AllDirections()", values: str(treasury.AllDirections())},
		{table: "treasury_actions", constraint: "treasury_actions_kind_check", source: "treasury.AllActionKinds()", values: str(treasury.AllActionKinds())},
		{table: "treasury_actions", constraint: "treasury_actions_activity_check", source: "treasury.AllActivities()", values: str(treasury.AllActivities())},
		{table: "treasury_actions", constraint: "treasury_actions_outcome_check", source: "treasury.AllOutcomes()", values: str(treasury.AllOutcomes())},
		{table: "treasury_actions", constraint: "treasury_actions_actor_type_check", source: "treasury.AuthorizedActorTypes()", values: str(treasury.AuthorizedActorTypes())},
		{table: "fee_schedules", constraint: "fee_schedules_created_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "fee_quote_disclosures", constraint: "fee_quote_disclosures_subsystem_check", source: "econ.AllSubsystems()", values: str(econ.AllSubsystems())},
		{table: "fee_quote_disclosures", constraint: "fee_quote_disclosures_side_check", source: "feeledger.AllSides()", values: str(feeledger.AllSides())},
		{table: "fee_bookings", constraint: "fee_bookings_mode_check", source: "feeledger.SupportedModes()", values: str(feeledger.SupportedModes())},
		{table: "fee_bookings", constraint: "fee_bookings_booking_mode_check", source: "feeledger.AllBookingModes()", values: str(feeledger.AllBookingModes())},
		{table: "fee_bookings", constraint: "fee_bookings_journal_kind_check", source: "feeledger.AllBookingKinds()", values: str(feeledger.AllBookingKinds())},
		{table: "fee_bookings", constraint: "fee_bookings_status_check", source: "feeledger.AllBookingStatuses()", values: str(feeledger.AllBookingStatuses())},
		{table: "fee_bookings", constraint: "fee_bookings_reversed_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "fee_booking_transitions", constraint: "fee_booking_transitions_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "performance_fee_periods", constraint: "performance_fee_periods_mode_check", source: "feeledger.SupportedModes()", values: str(feeledger.SupportedModes())},
		{table: "performance_fee_periods", constraint: "performance_fee_periods_status_check", source: "feeledger.AllPeriodStatuses()", values: str(feeledger.AllPeriodStatuses())},
		{table: "performance_fee_periods", constraint: "performance_fee_periods_opened_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "performance_fee_period_transitions", constraint: "performance_fee_period_transitions_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "performance_fee_flows", constraint: "performance_fee_flows_recorded_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "performance_fee_lines", constraint: "performance_fee_lines_kind_check", source: "feeledger.AllLineKinds()", values: str(feeledger.AllLineKinds())},

		// Migration 00749 (prediction markets).
		{table: "prediction_markets", constraint: "prediction_markets_status_check", source: "predmarket.AllMarketStatuses()", values: str(predmarket.AllMarketStatuses())},
		{table: "prediction_markets", constraint: "prediction_markets_mode_check", source: "predmarket.SupportedModes()", values: str(predmarket.SupportedModes())},
		{table: "prediction_market_transitions", constraint: "prediction_market_transitions_actor_type_check", source: "predmarket.LifecycleActorTypes()", values: str(predmarket.LifecycleActorTypes())},
		{table: "prediction_resolutions", constraint: "prediction_resolutions_kind_check", source: "predmarket.AllResolutionKinds()", values: str(predmarket.AllResolutionKinds())},
		{table: "prediction_settlements", constraint: "prediction_settlements_kind_check", source: "predmarket.AllSettlementKinds()", values: str(predmarket.AllSettlementKinds())},
		{table: "prediction_settlements", constraint: "prediction_settlements_rounding_policy_check", source: "predmarket.AllRoundingPolicies()", values: str(predmarket.AllRoundingPolicies())},

		// Migration 00741 (futures, perpetuals and options).
		{table: "derivative_positions", constraint: "derivative_positions_status_check", source: "derivatives.AllPositionStatuses()", values: str(derivatives.AllPositionStatuses())},
		{table: "derivative_position_transitions", constraint: "derivative_position_transitions_from_status_check", source: "derivatives.AllPositionStatuses()", values: str(derivatives.AllPositionStatuses())},
		{table: "derivative_position_transitions", constraint: "derivative_position_transitions_to_status_check", source: "derivatives.AllPositionStatuses()", values: str(derivatives.AllPositionStatuses())},
		{table: "derivative_fills", constraint: "derivative_fills_source_check", source: "derivatives.AllFillSources()", values: str(derivatives.AllFillSources())},
		{table: "derivative_fills", constraint: "derivative_fills_side_check", source: "derivatives.AllFillSides()", values: str(derivatives.AllFillSides())},
		{table: "derivative_cash_flows", constraint: "derivative_cash_flows_kind_check", source: "derivatives.AllCashFlowKinds()", values: str(derivatives.AllCashFlowKinds())},
		{table: "derivative_settlements", constraint: "derivative_settlements_kind_check", source: "derivatives.AllSettlementKinds()", values: str(derivatives.AllSettlementKinds())},
		{table: "funding_accruals", constraint: "funding_accruals_direction_check", source: "derivatives.AllFundingDirections()", values: str(derivatives.AllFundingDirections())},
		{table: "option_exercises", constraint: "option_exercises_kind_check", source: "derivatives.AllExerciseKinds()", values: str(derivatives.AllExerciseKinds())},

		// Migration 00742 (the persistent internal order book).
		{table: "book_states", constraint: "book_states_mode_check", source: "orders.AllBookModes()", values: str(orders.AllBookModes())},
		{table: "book_states", constraint: "book_states_status_check", source: "orders.AllBookStatuses()", values: str(orders.AllBookStatuses())},
		{table: "book_states", constraint: "book_states_instrument_kind_check", source: "orders.AllBookInstrumentKinds()", values: str(orders.AllBookInstrumentKinds())},
		{table: "book_states", constraint: "book_states_terms_source_check", source: "orders.AllTermsSources()", values: str(orders.AllTermsSources())},
		{table: "book_states", constraint: "book_states_created_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "book_state_transitions", constraint: "book_state_transitions_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "book_orders", constraint: "book_orders_owner_type_check", source: "orders.AllOwnerTypes()", values: str(orders.AllOwnerTypes())},
		{table: "book_orders", constraint: "book_orders_side_check", source: "orders.AllSides()", values: str(orders.AllSides())},
		{table: "book_orders", constraint: "book_orders_order_type_check", source: "orders.AllOrderTypes()", values: str(orders.AllOrderTypes())},
		{table: "book_orders", constraint: "book_orders_time_in_force_check", source: "orders.AllTimesInForce()", values: str(orders.AllTimesInForce())},
		{table: "book_orders", constraint: "book_orders_stp_mode_check", source: "orders.AllSTPModes()", values: str(orders.AllSTPModes())},
		{table: "book_orders", constraint: "book_orders_liquidity_intent_check", source: "orders.AllLiquidityIntents()", values: str(orders.AllLiquidityIntents())},
		{table: "book_orders", constraint: "book_orders_mode_check", source: "orders.AllBookModes()", values: str(orders.AllBookModes())},
		{table: "book_orders", constraint: "book_orders_status_check", source: "orders.AllStatuses()", values: str(orders.AllStatuses())},
		{table: "book_order_transitions", constraint: "book_order_transitions_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "book_commands", constraint: "book_commands_kind_check", source: "orders.AllCommandKinds()", values: str(orders.AllCommandKinds())},
		{table: "book_commands", constraint: "book_commands_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "book_events", constraint: "book_events_event_type_check", source: "orders.AllEventTypes()", values: str(orders.AllEventTypes())},
		{table: "book_trades", constraint: "book_trades_maker_owner_type_check", source: "orders.AllOwnerTypes()", values: str(orders.AllOwnerTypes())},
		{table: "book_trades", constraint: "book_trades_taker_owner_type_check", source: "orders.AllOwnerTypes()", values: str(orders.AllOwnerTypes())},
		{table: "book_trades", constraint: "book_trades_aggressor_side_check", source: "orders.AllSides()", values: str(orders.AllSides())},
		{table: "book_crossing_rules", constraint: "book_crossing_rules_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "book_crossing_decisions", constraint: "book_crossing_decisions_command_kind_check", source: "orders.AllCrossingCommandKinds()", values: str(orders.AllCrossingCommandKinds())},
		{table: "book_crossing_decisions", constraint: "book_crossing_decisions_taker_owner_type_check", source: "orders.AllOwnerTypes()", values: str(orders.AllOwnerTypes())},
		{table: "book_house_reservations", constraint: "book_house_reservations_status_check", source: "orders.AllHouseReservationStatuses()", values: str(orders.AllHouseReservationStatuses())},
		{table: "book_house_reservation_transitions", constraint: "book_house_reservation_transitions_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "paper_fills", constraint: "paper_fills_side_check", source: "orders.AllSides()", values: str(orders.AllSides())},

		// Migration 00743 (paper and sandbox equities).
		{table: "corporate_action_applications", constraint: "corporate_action_applications_rounding_policy_check", source: "equities.AllCorporateRoundings()", values: str(equities.AllCorporateRoundings())},
		{table: "equity_calendar_holidays", constraint: "equity_calendar_holidays_created_by_actor_type_check", source: "equities.AllTransitionActorTypes()", values: str(equities.AllTransitionActorTypes())},
		{table: "equity_corporate_action_transitions", constraint: "equity_corporate_action_transitions_actor_type_check", source: "equities.AllTransitionActorTypes()", values: str(equities.AllTransitionActorTypes())},
		{table: "equity_corporate_actions", constraint: "equity_corporate_actions_action_type_check", source: "equities.AllCorporateActionTypes()", values: str(equities.AllCorporateActionTypes())},
		{table: "equity_corporate_actions", constraint: "equity_corporate_actions_announced_by_actor_type_check", source: "equities.AllTransitionActorTypes()", values: str(equities.AllTransitionActorTypes())},
		{table: "equity_corporate_actions", constraint: "equity_corporate_actions_rounding_policy_check", source: "equities.AllCorporateRoundings()", values: str(equities.AllCorporateRoundings())},
		{table: "equity_corporate_actions", constraint: "equity_corporate_actions_status_check", source: "equities.AllCorporateActionStatuses()", values: str(equities.AllCorporateActionStatuses())},
		{table: "equity_fills", constraint: "equity_fills_side_check", source: "equities.AllSides()", values: str(equities.AllSides())},
		{table: "equity_order_transitions", constraint: "equity_order_transitions_actor_type_check", source: "equities.AllTransitionActorTypes()", values: str(equities.AllTransitionActorTypes())},
		{table: "equity_orders", constraint: "equity_orders_actor_type_check", source: "security.AllActorTypes()", values: str(security.AllActorTypes())},
		{table: "equity_orders", constraint: "equity_orders_mode_check", source: "equities.AllOrderModes()", values: str(equities.AllOrderModes())},
		{table: "equity_orders", constraint: "equity_orders_order_type_check", source: "equities.AllOrderTypes()", values: str(equities.AllOrderTypes())},
		{table: "equity_orders", constraint: "equity_orders_rejection_code_check", source: "equities.AllRejectionCodes()", values: str(equities.AllRejectionCodes())},
		{table: "equity_orders", constraint: "equity_orders_session_check", source: "equities.AllSessions()", values: str(equities.AllSessions())},
		{table: "equity_orders", constraint: "equity_orders_side_check", source: "equities.AllSides()", values: str(equities.AllSides())},
		{table: "equity_orders", constraint: "equity_orders_status_check", source: "equities.AllOrderStatuses()", values: str(equities.AllOrderStatuses())},

		// Migrations 00747 and 00748 (devnet host, stable assets, bridges).
		{table: "devnet_events", constraint: "devnet_events_kind_check", source: "devnethost.AllEventKinds()", values: str(devnethost.AllEventKinds())},
		{table: "devnet_events", constraint: "devnet_events_outcome_check", source: "devnethost.AllOutcomes()", values: str(devnethost.AllOutcomes())},
		{table: "stable_assets", constraint: "stable_assets_status_check", source: "stableasset.AllStatuses()", values: str(stableasset.AllStatuses())},
		{table: "stable_assets", constraint: "stable_assets_redemption_mode_check", source: "stableasset.AllRedemptionModes()", values: str(stableasset.AllRedemptionModes())},
		{table: "stable_assets", constraint: "stable_assets_created_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "stable_reserve_assets", constraint: "stable_reserve_assets_reserve_kind_check", source: "stableasset.AllReserveKinds()", values: str(stableasset.AllReserveKinds())},
		{table: "stable_mint_requests", constraint: "stable_mint_requests_mode_check", source: "stableasset.RequestModes()", values: str(stableasset.RequestModes())},
		{table: "stable_mint_requests", constraint: "stable_mint_requests_status_check", source: "stableasset.AllMintStatuses()", values: str(stableasset.AllMintStatuses())},
		{table: "stable_burn_requests", constraint: "stable_burn_requests_mode_check", source: "stableasset.RequestModes()", values: str(stableasset.RequestModes())},
		{table: "stable_burn_requests", constraint: "stable_burn_requests_status_check", source: "stableasset.AllBurnStatuses()", values: str(stableasset.AllBurnStatuses())},
		{table: "stable_peg_alerts", constraint: "stable_peg_alerts_kind_check", source: "stableasset.AllAlertKinds()", values: str(stableasset.AllAlertKinds())},
		{table: "stable_peg_alerts", constraint: "stable_peg_alerts_severity_check", source: "stableasset.AllSeverities()", values: str(stableasset.AllSeverities())},
		{table: "bridge_routes", constraint: "bridge_routes_status_check", source: "bridge.AllRouteStatuses()", values: str(bridge.AllRouteStatuses())},
		{table: "bridge_routes", constraint: "bridge_routes_created_by_actor_type_check", source: "econ.CreatorActorTypes()", values: str(econ.CreatorActorTypes())},
		{table: "bridge_legs", constraint: "bridge_legs_mode_check", source: "bridge.LegModes()", values: str(bridge.LegModes())},
		{table: "bridge_legs", constraint: "bridge_legs_status_check", source: "bridge.AllStates()", values: str(bridge.AllStates())},
		{table: "bridge_release_attempts", constraint: "bridge_release_attempts_kind_check", source: "bridge.AllAttemptKinds()", values: str(bridge.AllAttemptKinds())},

		// Migrations 00750 and 00751 (strategy marketplace, sandboxed runs).
		{table: "marketplace_evaluations", constraint: "marketplace_evaluations_actor_type_check", source: "marketplace.TransitionActorTypes()", values: str(marketplace.TransitionActorTypes())},
		{table: "marketplace_follow_intents", constraint: "marketplace_follow_intents_outcome_check", source: "marketplace.AllFollowOutcomes()", values: str(marketplace.AllFollowOutcomes())},
		{table: "marketplace_follow_runs", constraint: "marketplace_follow_runs_actor_type_check", source: "marketplace.TransitionActorTypes()", values: str(marketplace.TransitionActorTypes())},
		{table: "marketplace_follow_runs", constraint: "marketplace_follow_runs_trigger_check", source: "marketplace.AllFollowTriggers()", values: str(marketplace.AllFollowTriggers())},
		{table: "marketplace_install_transitions", constraint: "marketplace_install_transitions_actor_type_check", source: "marketplace.TransitionActorTypes()", values: str(marketplace.TransitionActorTypes())},
		{table: "marketplace_installs", constraint: "marketplace_installs_installed_by_actor_type_check", source: "marketplace.CreatorActorTypes()", values: str(marketplace.CreatorActorTypes())},
		{table: "marketplace_installs", constraint: "marketplace_installs_status_check", source: "marketplace.AllInstallStatuses()", values: str(marketplace.AllInstallStatuses())},
		{table: "marketplace_listing_versions", constraint: "marketplace_listing_versions_created_by_actor_type_check", source: "marketplace.CreatorActorTypes()", values: str(marketplace.CreatorActorTypes())},
		{table: "marketplace_listing_versions", constraint: "marketplace_listing_versions_moderation_state_check", source: "marketplace.AllModerationStates()", values: str(marketplace.AllModerationStates())},
		{table: "marketplace_listing_versions", constraint: "marketplace_listing_versions_risk_class_check", source: "marketplace.AllRiskClasses()", values: str(marketplace.AllRiskClasses())},
		{table: "marketplace_listing_versions", constraint: "marketplace_listing_versions_status_check", source: "marketplace.AllStatuses()", values: str(marketplace.AllStatuses())},
		{table: "marketplace_listings", constraint: "marketplace_listings_creator_actor_type_check", source: "marketplace.CreatorActorTypes()", values: str(marketplace.CreatorActorTypes())},
		{table: "marketplace_moderation_transitions", constraint: "marketplace_moderation_transitions_actor_type_check", source: "marketplace.TransitionActorTypes()", values: str(marketplace.TransitionActorTypes())},
		{table: "marketplace_reviews", constraint: "marketplace_reviews_decision_check", source: "marketplace.AllReviewDecisions()", values: str(marketplace.AllReviewDecisions())},
		{table: "marketplace_version_transitions", constraint: "marketplace_version_transitions_actor_type_check", source: "marketplace.TransitionActorTypes()", values: str(marketplace.TransitionActorTypes())},
		{table: "marketplace_version_validations", constraint: "marketplace_version_validations_failure_reason_check", source: "marketplace.AllValidationFailureReasons()", values: str(marketplace.AllValidationFailureReasons())},
		{table: "marketplace_version_validations", constraint: "marketplace_version_validations_outcome_check", source: "marketplace.AllValidationOutcomes()", values: str(marketplace.AllValidationOutcomes())},
		{table: "marketplace_version_validations", constraint: "marketplace_version_validations_validated_by_actor_type_check", source: "marketplace.TransitionActorTypes()", values: str(marketplace.TransitionActorTypes())},
		{table: "sandbox_run_violations", constraint: "sandbox_run_violations_code_check", source: "sandboxrt.AllViolationCodes()", values: str(sandboxrt.AllViolationCodes())},
		{table: "sandbox_runs", constraint: "sandbox_runs_actor_type_check", source: "sandboxrt.RecordingActorTypes()", values: str(sandboxrt.RecordingActorTypes())},
		{table: "sandbox_runs", constraint: "sandbox_runs_termination_reason_check", source: "sandboxrt.AllTerminations()", values: str(sandboxrt.AllTerminations())},
		{table: "strategy_artifact_sources", constraint: "strategy_artifact_sources_created_by_actor_type_check", source: "sandboxrt.RecordingActorTypes()", values: str(sandboxrt.RecordingActorTypes())},
		{table: "strategy_artifacts", constraint: "strategy_artifacts_created_by_actor_type_check", source: "sandboxrt.RecordingActorTypes()", values: str(sandboxrt.RecordingActorTypes())},
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
	// Not a list of values but a rule: the house book and every facility hold
	// SIMULATED value only. internal/econ's
	// TestIntegration_HouseAndFacilityBooksCannotHoldRealValue drives it.
	"ledger_accounts.ledger_accounts_segregated_books_are_simulated",
	// Not a list of values but a rule: a liquidation case carries closed_at
	// exactly when its status is terminal. internal/margin's
	// TestIntegration_DatabaseRefusesInvalidMarginStates drives it.
	"margin_liquidation_cases.margin_liquidation_cases_closed",
	// Rules, not lists of values: a MARKET order is IOC or FOK, and an
	// order's quantity is conserved across fills, cancels and its remainder.
	// internal/orders' TestIntegration_TheDatabaseRefusesOverfillAndIllegalOrderStates
	// drives both.
	"book_orders.book_orders_market_tif",
	"book_orders.book_orders_quantity_conserved",
	// Not a list of values but a rule: a leg past RESERVED carries the lock
	// transaction it was requested with. internal/bridge's
	// TestIntegration_ARequestedLegCarriesItsLock drives it.
	"bridge_legs.bridge_legs_requested_has_lock",
	// Not a list of values but a rule: a SUBMIT event's outcome is ACCEPTED or
	// TIMEOUT and no other kind has one of those. internal/devnethost's
	// TestIntegration_TheDatabaseRefusesAGapInTheHistory drives it.
	"devnet_events.devnet_events_submit_outcome",
	// Rules, not lists of values: a corporate action carries only its own
	// type's fields (a ratio for splits, cash in lieu for splits).
	// internal/equities' TestIntegration_DatabaseGuardsTheEquitiesRegisters
	// drives both.
	"equity_corporate_actions.equity_corporate_actions_cash_in_lieu_for_splits",
	"equity_corporate_actions.equity_corporate_actions_ratio_shape",
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
		`SELECT conrelid::regclass::text || '.' || conname
			FROM pg_constraint
			WHERE contype = 'c' AND connamespace = 'public'::regnamespace
			  AND pg_get_constraintdef(oid) LIKE '%= ANY (ARRAY[%'
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
	"data_sources.data_sources_retention_class_check",
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
	"internal_commerce_orders.internal_commerce_orders_earning_origin_check",
	"internal_sellers.internal_sellers_status_check",
	"journal_entries.journal_entries_side_check",
	"kill_switch_transitions.kill_switch_transitions_actor_type_check",
	"kill_switches.kill_switches_severity_check",
	"ledger_accounts.ledger_accounts_normal_side_check",
	"ledger_accounts.ledger_accounts_status_check",
	"ledger_accounts.ledger_accounts_value_domain_check",
	"login_attempts.login_attempts_outcome_check",
	"model_calls.model_calls_parse_result_check",
	"model_calls.model_calls_purpose_check",
	"native_assets.native_assets_content_moderation_state_check",
	"native_assets.native_assets_status_check",
	"native_market_alerts.native_market_alerts_kind_check",
	"native_market_alerts.native_market_alerts_severity_check",
	"native_market_fills.native_market_fills_side_check",
	"native_market_quotes.native_market_quotes_side_check",
	"native_markets.native_markets_status_check",
	"notifications.notifications_severity_check",
	"orders.orders_side_check",
	"payout_destinations.payout_destinations_kind_check",
	"payout_destinations.payout_destinations_status_check",
	"payout_provider_events.payout_provider_events_direction_check",
	"payout_requests.payout_requests_state_check",
	"performance_snapshots.performance_snapshots_scope_kind_check",
	"position_lots.position_lots_status_check",
	"prediction_outcomes.prediction_outcomes_realized_direction_check",
	"predictions.predictions_direction_check",
	"provider_events.provider_events_processing_status_check",
	"provider_health_samples.provider_health_samples_role_check",
	"provider_health_samples.provider_health_samples_state_check",
	"quotes.quotes_side_check",
	"raw_archive_objects.raw_archive_objects_retention_class_check",
	"reconciliation_records.reconciliation_records_mode_check",
	"reconciliation_records.reconciliation_records_status_check",
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
