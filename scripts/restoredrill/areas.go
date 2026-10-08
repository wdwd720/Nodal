package main

import (
	"fmt"
	"strings"
)

// advancedArea is one advanced capability and the tables it keeps. The areas
// below list every table migrations 00730 to 00763 create -- the evidence
// archive, the financial foundation of ADR-0020 and every capability built
// on it -- each under exactly one area. A table a later migration adds is in
// the drill's row-count comparison regardless, and belongs here too.
type advancedArea struct {
	name string
	// seeded says the drill's seed writes the area, so the source must carry
	// rows in it. An unseeded area is still reported, with what it holds.
	seeded bool
	tables []string
}

var advancedAreas = []advancedArea{
	{"evidence archive", true, []string{"provider_evidence"}},
	{"economic intents and effects", true, []string{
		"economic_intents", "economic_intent_transitions", "economic_effects", "economic_effect_transitions",
		"advanced_risk_decisions",
	}},
	{"facilities and house accounts", true, []string{"facilities", "house_accounts"}},
	{"canonical instruments", true, []string{"instrument_versions", "instrument_terms"}},
	{"sandbox funding", true, []string{"sandbox_fundings"}},
	{"transfers", true, []string{
		"transfer_handles", "transfer_recipient_rules", "transfer_sender_modes", "internal_transfers",
		"internal_transfer_transitions", "transfer_reversal_proposals", "transfer_reversal_decisions",
	}},
	{"order books and trades", true, []string{
		"book_states", "book_state_transitions", "book_orders", "book_order_transitions", "book_commands",
		"book_events", "book_trades", "book_snapshots", "book_crossing_rules", "book_crossing_decisions",
		"book_house_reservations", "book_house_reservation_transitions", "paper_fills",
	}},
	{"fee ledger", true, []string{
		"fee_schedules", "fee_quote_disclosures", "fee_bookings", "fee_booking_transitions",
		"performance_fee_periods", "performance_fee_period_transitions", "performance_fee_flows",
		"performance_fee_lines", "performance_fee_attributions",
	}},
	{"margin", true, []string{
		"margin_policies", "margin_accounts", "margin_account_transitions", "margin_borrow_lots",
		"margin_interest_accruals", "margin_collateral_moves", "margin_calls", "margin_call_transitions",
		"margin_liquidation_cases", "margin_liquidation_case_transitions", "margin_liquidation_steps",
		"margin_stress_runs", "margin_case_resolutions", "margin_case_resolution_decisions",
		"margin_deficit_repayments",
	}},
	{"stable assets", true, []string{
		"stable_assets", "stable_asset_transitions", "stable_reserve_assets", "stable_mint_requests",
		"stable_mint_request_transitions", "stable_burn_requests", "stable_burn_request_transitions",
		"stable_peg_alerts", "stable_reserve_snapshots",
	}},
	{"simulated chain", true, []string{"devnet_events"}},
	{"prediction markets", true, []string{
		"prediction_markets", "prediction_market_transitions", "prediction_outcomes_registry",
		"prediction_resolutions", "prediction_disputes", "prediction_settlements",
		"prediction_oracle_observations", "prediction_void_proposals", "prediction_void_approvals",
		"prediction_held_payouts", "prediction_held_payout_claims",
	}},
	{"derivatives", false, []string{
		"derivative_marks", "derivative_positions", "derivative_position_transitions", "derivative_fills",
		"derivative_cash_flows", "variation_margin_runs", "derivative_settlements", "funding_rates",
		"funding_accruals", "option_exercises", "option_assignments", "derivative_settlement_overrides",
		"derivative_settlement_override_decisions",
	}},
	{"equities", false, []string{
		"equity_orders", "equity_order_transitions", "equity_fills", "equity_settlements",
		"equity_corporate_actions", "equity_corporate_action_transitions", "corporate_action_applications",
		"equity_calendar_holidays",
	}},
	{"treasury", false, []string{
		"treasury_books", "treasury_signing_policies", "treasury_policies", "treasury_risk_decisions",
		"treasury_fundings", "treasury_paper_fills", "treasury_position_events", "treasury_pnl_snapshots",
		"treasury_actions", "treasury_crossing_disclosures",
	}},
	{"bridges", false, []string{
		"bridge_routes", "bridge_route_transitions", "bridge_legs", "bridge_leg_transitions",
		"bridge_release_attempts",
	}},
	{"strategy marketplace", false, []string{
		"marketplace_listings", "marketplace_listing_versions", "marketplace_version_sources",
		"marketplace_version_validations", "marketplace_reviews", "marketplace_version_transitions",
		"marketplace_moderation_transitions", "marketplace_installs", "marketplace_install_transitions",
		"marketplace_follow_runs", "marketplace_follow_intents", "marketplace_evaluations",
		"marketplace_evaluation_runs", "marketplace_performance_snapshots", "marketplace_follow_marks",
		"marketplace_paper_fills", "marketplace_validation_claims", "marketplace_validation_claim_releases",
	}},
	{"strategy sandbox", false, []string{
		"strategy_artifacts", "strategy_artifact_sources", "sandbox_runs", "sandbox_run_violations",
		"sandbox_usage",
	}},
	{"delegation", false, []string{
		"delegated_capabilities", "delegated_capability_transitions", "delegated_signer_keys",
		"delegation_venues", "delegated_capability_usage", "delegated_swaps", "delegated_swap_transitions",
		"delegated_swap_approvals", "delegated_signing_decisions", "delegated_withdrawal_destinations",
		"delegated_withdrawal_destination_transitions", "delegated_withdrawal_policies",
		"delegated_withdrawals", "delegated_withdrawal_transitions", "delegated_withdrawal_approvals",
	}},
	{"scheduled upkeep", false, []string{"upkeep_failures"}},
}

// areaReport is one advanced area on both sides of the restore.
type areaReport struct {
	Area         string `json:"area"`
	Seeded       bool   `json:"seeded"`
	SourceRows   int    `json:"source_rows"`
	RestoredRows int    `json:"restored_rows"`
	// Tables maps each of the area's tables to its row count in the source
	// and in the restored copy.
	Tables   map[string][2]int `json:"tables"`
	Problems []string          `json:"problems,omitempty"`
}

// compareAreas reports every advanced area's rows on both sides. An area
// fails when it names a table either database lacks or another area also
// names, when any of its tables restored with a different count, or when the
// seed writes it and the source holds no row in it at all. The last is the
// point: without it a backup that lost every advanced row, taken of a source
// that never had any, would pass as zero equal to zero.
func compareAreas(src, dst map[string]int) ([]areaReport, bool) {
	ok := true
	owner := map[string]string{}
	out := make([]areaReport, 0, len(advancedAreas))
	for _, a := range advancedAreas {
		r := areaReport{Area: a.name, Seeded: a.seeded, Tables: map[string][2]int{}}
		for _, t := range a.tables {
			if prev, dup := owner[t]; dup {
				r.Problems = append(r.Problems, fmt.Sprintf("%s is also listed under %s", t, prev))
				continue
			}
			owner[t] = a.name
			n, inSource := src[t]
			m, inRestored := dst[t]
			switch {
			case !inSource:
				r.Problems = append(r.Problems, fmt.Sprintf("the source has no table %s", t))
			case !inRestored:
				r.Problems = append(r.Problems, fmt.Sprintf("the restored copy has no table %s", t))
			case n != m:
				r.Problems = append(r.Problems, fmt.Sprintf("%s: %d rows in the source, %d restored", t, n, m))
			}
			r.Tables[t] = [2]int{n, m}
			r.SourceRows += n
			r.RestoredRows += m
		}
		if a.seeded && r.SourceRows == 0 {
			r.Problems = append(r.Problems, "the seed writes this area and the source holds no row in it")
		}
		if len(r.Problems) > 0 {
			ok = false
		}
		out = append(out, r)
	}
	return out, ok
}

// printAreas prints one line per area -- rows in the source, then restored --
// and every problem under the area that has it.
func printAreas(areas []areaReport) {
	fmt.Println("restoredrill: advanced areas (rows in the source -> restored):")
	for _, a := range areas {
		seeded := "seeded"
		if !a.Seeded {
			seeded = "-"
		}
		fmt.Printf("restoredrill:   %-6s  %-30s %5d -> %d\n", seeded, a.Area, a.SourceRows, a.RestoredRows)
		for _, p := range a.Problems {
			fmt.Printf("restoredrill:           FAILED: %s\n", p)
		}
	}
}

// areaSummary is the drill's one-line account of the advanced areas.
func areaSummary(areas []areaReport, ok bool) string {
	var seeded, seededRows, tables int
	var failed []string
	for _, a := range areas {
		tables += len(a.Tables)
		if a.Seeded {
			seeded++
			seededRows += a.SourceRows
		}
		if len(a.Problems) > 0 {
			failed = append(failed, a.Area)
		}
	}
	if !ok {
		return "advanced areas: FAILED in " + strings.Join(failed, ", ")
	}
	return fmt.Sprintf("advanced areas: %d areas over %d tables; the %d seeded areas hold %d rows in the source, "+
		"none empty; every advanced table restored with its count", len(areas), tables, seeded, seededRows)
}
