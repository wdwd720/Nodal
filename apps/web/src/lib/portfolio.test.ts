/**
 * The portfolio's pure helpers (src/lib/portfolio.ts) and its runtime
 * contract (src/api/advanced/portfolio.ts).
 *
 * The contract is shown a real answer of the backend -- captured from
 * TestIntegration_PortfolioIsTheServicesOwnReads, trimmed, with an equities
 * section added -- which it must accept unchanged, and then that answer
 * broken one way at a time, which it must refuse with a ContractViolation
 * naming the field. A refused answer becomes an explicit error state on the
 * page; an accepted malformed one would become a figure.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { advancedKeys } from "../api/advanced/core.ts";
import { checkPortfolio, portfolioKeys, type Portfolio } from "../api/advanced/portfolio.ts";
import { ContractViolation } from "../api/contract.ts";
import {
  LEDGER_CODE_COPY,
  PORTFOLIO_LEDGER_CODES,
  PORTFOLIO_SECTION_STATES,
  PURPOSE_COPY,
  RESERVATION_PURPOSES,
  formatLeverageBps,
  isZeroQuantity,
  ledgerCodeLabel,
  pendingCapital,
  purposeLabel,
  sectionStateCopy,
  sectionStateTone,
} from "./portfolio.ts";

const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const ACME = "01a11b2c-4f10-7000-8000-00000000ac3e";
const MARKET = "01a11b2c-4e06-7da0-8448-ae100a658c3f";

const ANSWER = {
  account_id: "01a11b2c-4dc6-76f9-a46e-d1a9405d14b5", as_of: "2026-10-08T11:00:50Z",
  balances: [
    {
      asset_id: "01a11b2c-4e09-705d-8882-cc71448e4563", available: "3000000", chain: "nodal-sandbox", decimals: 6,
      held_elsewhere: [], name: "Prediction share PFPM-841DE2B1.NO (simulated)", owed: [], reservations: [],
      reserved: "0", symbol: "PFPM-841DE2B1.NO", total: "3000000",
      valuation: {
        problem: {
          code: "STALE_MARKET_DATA",
          detail: "no mark of PFPM-841DE2B1.NO in the sandbox dollar within the maximum age (1m0s)",
        },
        status: "UNVALUED",
      },
      value_domain: "SIMULATED", wallet: "3000000", withdrawal_holds: "0",
    },
    {
      asset_id: "01a11b2c-4e07-7df2-8a38-81b7624ef347", available: "3000000", chain: "nodal-sandbox", decimals: 6,
      held_elsewhere: [], name: "Prediction share PFPM-841DE2B1.YES (simulated)", owed: [], reservations: [],
      reserved: "0", symbol: "PFPM-841DE2B1.YES", total: "3000000",
      valuation: {
        problem: {
          code: "STALE_MARKET_DATA",
          detail: "no mark of PFPM-841DE2B1.YES in the sandbox dollar within the maximum age (1m0s)",
        },
        status: "UNVALUED",
      },
      value_domain: "SIMULATED", wallet: "3000000", withdrawal_holds: "0",
    },
    {
      asset_id: "01a11b2c-4dc4-72bf-938e-0f7b772ad847", available: "0", chain: "nodal-sandbox", decimals: 9,
      held_elsewhere: [{ code: "MARGIN_COLLATERAL", quantity: "5000000000" }], name: "portfolio itest SOL (sandbox)",
      owed: [], reservations: [], reserved: "0", symbol: "PFSOL9335B9C9", total: "5000000000",
      valuation: {
        mark: {
          age_ms: 0, mantissa: "100", observed_at: "2026-10-08T11:00:50Z", price: "100", scale: 0,
          source: "portfolio-itest-oracle",
        },
        status: "MARKED", value: "500000000",
      },
      value_domain: "SIMULATED", wallet: "0", withdrawal_holds: "0",
    },
    {
      asset_id: "01920000-0000-7000-8000-5342582d5553", available: "95705770000", chain: "nodal-sandbox", decimals: 6,
      held_elsewhere: [{ code: "MARGIN_COLLATERAL", quantity: "1000000000" }, { code: "DERIVATIVES_MARGIN", quantity: "796250000" }],
      name: "Sandbox US dollar (simulated, no economic value)", owed: [{ code: "MARGIN_LOAN", quantity: "500500000" }],
      reservations: [{ capability: "INTERNAL_MATCHING", purpose: "OPEN_ORDERS", quantity: "2494980000", reservations: 1 }],
      reserved: "2494980000", symbol: "SBX-USD", total: "99997000000",
      valuation: { owed_value: "500500000", status: "PAR", value: "99997000000" }, value_domain: "SIMULATED",
      wallet: "98200750000", withdrawal_holds: "0",
    },
    {
      asset_id: "01a11b2c-4f10-7000-8000-00000000ac3e", available: "3000000", chain: "nodal-sandbox", decimals: 6,
      held_elsewhere: [{ code: "UNSETTLED_RECEIVABLE", quantity: "2000000" }], name: "Acme (simulated share)", owed: [],
      reservations: [], reserved: "0", symbol: "ACME", total: "5000000",
      valuation: {
        mark: {
          age_ms: 30000, mantissa: "15000", observed_at: "2026-10-08T11:00:20Z", price: "150.00", scale: 2,
          source: "sandbox-feed",
        },
        status: "MARKED", value: "750000000",
      },
      value_domain: "SIMULATED", wallet: "3000000", withdrawal_holds: "0",
    },
  ],
  buying_power: {
    buying_power: {
      as_of: "2026-10-08T11:00:50Z", borrow_liabilities: "500.50", eligible_collateral: "100397.00",
      eligible_equity: "99890.00", gross_equity: "99990.00", haircuts: "100.00", initial_margin_used: "896.25",
      lines: [
        {
          amount_usd: "796.25", asset_id: "01920000-0000-7000-8000-5342582d5553", component: "DERIVATIVES_MARGIN",
          detail: "SBX-USD 796250000 marked 796.25 at factor 100.00%", field: "eligible_collateral",
        },
        {
          amount_usd: "1000.00", asset_id: "01920000-0000-7000-8000-5342582d5553", component: "MARGIN_COLLATERAL",
          detail: "SBX-USD 1000000000 marked 1000.00 at factor 100.00%", field: "eligible_collateral",
        },
      ],
      maintenance_margin: "375.00", margin_available: "96498.77", open_order_reservations: "2494.98",
      pending_bridge_haircut: "0.00", pending_transfer_reservations: "0.00", pending_withdrawal_reservations: "0.00",
      policy_version: "buying-power/2|bp2-sandbox-v1", quote_asset_id: "01920000-0000-7000-8000-5342582d5553",
      quote_symbol: "SBX-USD",
      reasons: [
        {
          amount: "2494.98", code: "CAPITAL_RESERVED",
          detail: "open orders, pending transfers and withdrawals, strategy reservations",
        },
        {
          amount: "100.00", code: "HAIRCUT_APPLIED",
          detail: "MARGIN_COLLATERAL PFSOL9335B9C9: MAJOR risk class haircut 20%",
        },
      ],
      risk_increasing_buying_power: "482493.85", strategy_reservations: "0.00", trade_buying_power: "95705.77",
      value_domain: "SIMULATED", withdrawable: "95705.77",
    },
    state: "PRESENT", value_domain: "SIMULATED",
  },
  max_mark_age_ms: 60000,
  positions: {
    derivatives: {
      positions: {
        book: {
          as_of: "2026-10-08T11:00:50Z", equity_adjustment: "-6.50", gross_notional: "6500.00",
          initial_margin: "796.25", maintenance_margin: "325.00", max_initial_bps: 1000,
          policy_hash: "9e23f8fc886f2acbe1bd8894f16da89aaf21a1ae2b27a79a968810f2e5151ff1",
          policy_version: "derivatives-sandbox-v2", positions: 1,
          quote_asset: { decimals: 6, id: "01920000-0000-7000-8000-5342582d5553", symbol: "SBX-USD" },
          unrealized_pnl: "-6.50",
        },
        book_status: "VALUED",
        items: [
          {
            account_id: "01a11b2c-4dc6-76f9-a46e-d1a9405d14b5", cost_value: "6506500000", funding_pnl: "0",
            id: "01a11b2c-4dfc-7ae1-8ccc-4cfd76323216", instrument_id: "01a11b2c-4dd8-7b23-9a7b-004a0a0f489c",
            kind: "PERPETUAL", margin_posted: "796250000", opened_at: "2026-10-08T11:00:50Z", price_scale: 2,
            quantity: "10", realized_pnl: "0", settled_value: "6506500000",
            settlement_asset: { decimals: 6, id: "01920000-0000-7000-8000-5342582d5553", symbol: "SBX-USD" },
            shortfall: "0", side: "LONG", status: "OPEN", symbol: "PFPERP564061B6",
            valuation: {
              average_entry_price_mantissa: "6506500", equity_adjustment: "-6500000",
              mark_id: "01a11b2c-4de9-7318-aa67-c906324dbd57", mark_mantissa: "6500000",
              mark_observed_at: "2026-10-08T11:00:50Z",
              requirement: {
                funding_buffer: "146250000", initial: "796250000", maintenance: "325000000", notional: "6500000000",
                scenario_loss: "0",
              },
              unrealized_pnl: "-6500000",
            },
            valuation_status: "VALUED", version: 1,
          },
        ],
        next_cursor: null, value_domain: "SIMULATED",
      },
      state: "PRESENT", value_domain: "SIMULATED",
    },
    equities: {
      holdings: [
        {
          listing: {
            exchange_mic: "XNAS", instrument_id: "01a11b2c-4f10-7000-8000-00000000ac31", kind: "EQUITY", lot_size: "1",
            mark: { observed_at: "2026-10-08T11:00:20Z", price: "150.00", source: "sandbox-feed" }, min_quantity: "1",
            name: "Acme (simulated share)", oracle_max_age_ms: 60000, price_scale: 2,
            settlement_asset_id: "01920000-0000-7000-8000-5342582d5553", settlement_decimals: 6,
            settlement_symbol: "SBX-USD", share_asset_id: "01a11b2c-4f10-7000-8000-00000000ac3e", share_decimals: 6,
            share_symbol: "ACME", status: "ACTIVE", symbol: "ACME", terms_hash: "0a0b", terms_version: 1,
            tick_price: "0.01", trading_calendar: "XNYS", value_domain: "SIMULATED", venue: "nodal-paper-equities",
          },
          settled: "3000000", unsettled: "2000000",
        },
      ],
      listings_truncated: false,
      pending_settlements: {
        items: [
          {
            account_id: "01a11b2c-4dc6-76f9-a46e-d1a9405d14b5", asset_decimals: 6,
            asset_id: "01a11b2c-4f10-7000-8000-00000000ac3e", asset_symbol: "ACME", due_date: "2026-10-08",
            fill_id: "01a11b2c-4f10-7000-8000-00000000ac36", id: "01a11b2c-4f10-7000-8000-00000000ac35",
            instrument_id: "01a11b2c-4f10-7000-8000-00000000ac31", order_id: "01a11b2c-4f10-7000-8000-00000000ac37",
            quantity: "2000000", state: "PENDING", trade_date: "2026-10-07", value_domain: "SIMULATED",
          },
        ],
        next_cursor: null, value_domain: "SIMULATED",
      },
      state: "PRESENT", value_domain: "SIMULATED",
    },
    margin: {
      account: {
        account_id: "01a11b2c-4dc6-76f9-a46e-d1a9405d14b5",
        evaluation: {
          accrued_fees: "0", accrued_interest: "0", at: "2026-10-08T11:00:50Z", borrow_liabilities: "500500000",
          capacity: "849450000", effective_leverage_bps: 5267, eligible_collateral: "1449950000", equity: "949450000",
          gross_exposure: "500000000", hash: "2383805559d36f166ef17584352f20fd89bff0c9f074f5ca9bf5272657aea4a6",
          health: "NORMAL",
          holdings: [
            {
              asset_id: "01920000-0000-7000-8000-5342582d5553", collateral_value: "1000000000",
              concentration_share_bps: 0, exposure: "0", fresh: true,
              haircut: {
                concentration_bps: 0, cross_chain_bps: 0, liquidity_bps: 0, oracle_bps: 0, settlement_bps: 0,
                total_bps: 0, venue_bps: 0, volatility_bps: 0, zero_value: false,
              },
              initial_margin: "0", maintenance_margin: "0", market_value: "1000000000", numeraire: true, priced: true,
              quantity: "1000000000",
            },
          ],
          initial_margin: "100000000", leverage_cap_bps: 50000, leverage_unbounded: false, levered: true,
          liquidation_buffer: "899450000", liquidation_eligible: false, maintenance_margin: "50000000",
          margin_available: "849450000", market_value: "1500000000", missing_marks: false, net_asset_value: "999500000",
          net_exposure: "500000000", newest_mark_at: "2026-10-08T11:00:50Z",
          numeraire_asset_id: "01920000-0000-7000-8000-5342582d5553", numeraire_decimals: 6,
          oldest_mark_at: "2026-10-08T11:00:50Z",
          policy_hash: "aa6fd563a2fd0e2353e4a6ce51e153d4eebd36de0f12fd87bfd9a197f7076130",
          policy_version: "portfolio-itest-0a4ef69f-0279-4806-a498-7dfbe169402d", prices_fresh: true, realized_pnl: "0",
          stale_assets: [], unrealized_pnl: "0",
          usd: {
            accrued_interest: "0.00", borrow_liabilities: "500.50", eligible_collateral: "1449.95", equity: "949.45",
            gross_exposure: "500.00", initial_margin: "100.00", liquidation_buffer: "899.45",
            maintenance_margin: "50.00", margin_available: "849.45", market_value: "1500.00",
            warning_threshold: "62.50",
          },
          warning_threshold: "62500000",
        },
        id: "01a11b2c-4e56-7d71-ba28-56c28c4fd20c", insurance_fund_id: "01a11b2c-4e4f-736e-909f-f9e21d171d71",
        lending_pool_id: "01a11b2c-4e4f-7173-aea5-f07abd0f7a37",
        loans: [
          {
            accrued_through: "2026-10-08T11:00:50Z", interest: "0", lot_id: "01a11b2c-4e65-7d8c-adec-302917138b85",
            principal: "500500000", rate_bps: 800,
          },
        ],
        margin_mode: "CROSS", mode: "PAPER", numeraire_asset_id: "01920000-0000-7000-8000-5342582d5553",
        opened_at: "2026-10-08T11:00:50Z", policy_version: "portfolio-itest-0a4ef69f-0279-4806-a498-7dfbe169402d",
        status: "NORMAL", value_domain: "SIMULATED",
      },
      state: "PRESENT", value_domain: "SIMULATED",
    },
    order_books: {
      orders: {
        items: [
          {
            accepted_command_seq: 1, account_id: "01a11b2c-4dc6-76f9-a46e-d1a9405d14b5", base_decimals: 8,
            base_symbol: "PFOBB808ED563", book_id: "01a11b2c-4e31-7cc5-b75f-0d9153ad911c", cancelled_quantity: "0",
            client_order_id: "portfolio-itest-1", created_at: "2026-10-08T11:00:50.8694Z", filled_quantity: "0",
            id: "01a11b2c-4e35-7a06-a60f-1e6a92193d02", instrument_id: "01a11b2c-4e2f-7e6b-9275-c468fe174d2c",
            intent_id: "01a11b2c-4e35-7a06-a60f-1e6a92193d02", last_command_seq: 1,
            liquidity_intent: "PROVIDE_LIQUIDITY", mode: "PAPER", order_type: "LIMIT", post_only: false,
            price: "2490.00", price_ticks: 249000, quantity: "100000000", quote_decimals: 6, quote_symbol: "SBX-USD",
            reduce_only: false, remaining_quantity: "100000000", reservation_id: "01a11b2c-4e3f-7612-ac8e-d1d574c39c31",
            side: "BUY", status: "OPEN", stp: "CANCEL_TAKER", strategy_originated: false, time_in_force: "GTC",
            updated_at: "2026-10-08T11:00:50.882877Z", value_domain: "SIMULATED",
            venue_label: "SIMULATED_INTERNAL_BOOK",
          },
        ],
        next_cursor: null, value_domain: "SIMULATED",
      },
      state: "PRESENT", value_domain: "SIMULATED",
    },
    prediction_markets: {
      markets: [
        {
          market: {
            close_at: "2026-10-08T12:00:50Z", collateral_asset_id: "01920000-0000-7000-8000-5342582d5553",
            collateral_decimals: 6, collateral_symbol: "SBX-USD", created_at: "2026-10-08T11:00:50.822024Z",
            created_by: { actor_id: "d102114a-ffd6-444f-afac-59fb93b6f29f", actor_type: "OPERATOR" },
            definition_hash: "1b5933b79a93d22a3121b734acd170a77f27842b3e543b1b6a8952bca31c7957", dispute_window_ms: 0,
            facility_id: "01a11b2c-4e06-7da4-9c6c-cee802fac0e3", id: "01a11b2c-4e06-7da0-8448-ae100a658c3f",
            instrument_id: "01a11b2c-4e07-7294-aa0b-a886b90ccaff", mode: "SANDBOX", open_at: "2026-10-08T11:00:50Z",
            oracle_public_key: "d18846a93796f5196e0f6306f23722633d63353384fc1d43ea991884823cb138",
            oracle_source: "portfolio-itest-board",
            outcomes: [
              {
                index: 0, instrument_id: "01a11b2c-4e08-7395-abb3-5e288c150c6d",
                share_asset_id: "01a11b2c-4e07-7df2-8a38-81b7624ef347", symbol: "YES",
              },
              {
                index: 1, instrument_id: "01a11b2c-4e09-7641-8ae1-f12660069242",
                share_asset_id: "01a11b2c-4e09-705d-8882-cc71448e4563", symbol: "NO",
              },
            ],
            question: "Which outcome will the sandbox election board certify?", share_decimals: 6, status: "OPEN",
            symbol: "PFPM-841DE2B1", unit_quantity: "1000000", updated_at: "2026-10-08T11:00:50.830478Z",
            value_domain: "SIMULATED",
          },
          positions: {
            account_id: "01a11b2c-4dc6-76f9-a46e-d1a9405d14b5",
            items: [
              {
                as_of: "2026-10-08T11:00:50.837619Z", outcome_index: 0, outcome_symbol: "YES", quantity: "3000000",
                share_asset_id: "01a11b2c-4e07-7df2-8a38-81b7624ef347",
              },
              {
                as_of: "2026-10-08T11:00:50.837619Z", outcome_index: 1, outcome_symbol: "NO", quantity: "3000000",
                share_asset_id: "01a11b2c-4e09-705d-8882-cc71448e4563",
              },
            ],
            market_id: "01a11b2c-4e06-7da0-8448-ae100a658c3f", share_decimals: 6, value_domain: "SIMULATED",
          },
        },
      ],
      state: "PRESENT", truncated: false, value_domain: "SIMULATED",
    },
  },
  // The world the answer was captured in runs no agent authority service and no fee ledger.
  delegation: { state: "NOT_CONFIGURED", value_domain: "SIMULATED" },
  performance_fees: { state: "NOT_CONFIGURED", value_domain: "SIMULATED" },
  quote_asset_id: "01920000-0000-7000-8000-5342582d5553", quote_decimals: 6, quote_provisioned: true,
  quote_symbol: "SBX-USD",
  risk: {
    account_status: "ACTIVE", blocks_new_risk: true,
    decisions: {
      items: [
        {
          decision_hash: "b4d4cfa5b6da9ba7e5d6b1afd04a01c381c762f55acb222f1460223f5848215d",
          evaluated_at: "2026-10-08T11:00:50Z", evaluator_version: "risk-kernel/2",
          id: "01a11b2c-4e66-7a8f-b649-809d61216043",
          input_hash: "cf441e78c567903ace1359795e3e3a5698df44d7ff1f482bad206ae2b8ebc258",
          intent_id: "01a11b2c-4e64-7121-bdb7-bd8f78673300", intent_kind: "ACQUIRE_EXPOSURE",
          policy_hash: "0a383d197cdcc5e8d8946801d6feafcec807ac4ef485b29d9bb5e44133ee3351",
          policy_version: "advanced-sandbox-v1+portfolio-itest-0a4ef69f-0279-4806-a498-7dfbe169402d/CROSS",
          reason_codes: [], stage: "PRE_TRADE", verdict: "ALLOW",
        },
        {
          decision_hash: "30f80659b36a4aae8be70aac6ea0755726e03aa44b2e6259fc6c375d99ff2d00",
          evaluated_at: "2026-10-08T11:00:50Z", evaluator_version: "risk-kernel/2",
          id: "01a11b2c-4e5b-7e53-b173-9a69d7bbeced",
          input_hash: "5e3436ec90ea4c1d62d192dbc71b2b48a38c16e241f97c3e5c9beeb4eb5a78fe",
          intent_id: "01a11b2c-4e58-7b27-adf4-d700a889678f", intent_kind: "TRANSFER_VALUE",
          policy_hash: "0a383d197cdcc5e8d8946801d6feafcec807ac4ef485b29d9bb5e44133ee3351",
          policy_version: "advanced-sandbox-v1+portfolio-itest-0a4ef69f-0279-4806-a498-7dfbe169402d/CROSS",
          reason_codes: [], stage: "PRE_TRADE", verdict: "ALLOW",
        },
      ],
      state: "PRESENT",
    },
    reconciliation: [
      {
        asset_id: "01920000-0000-7000-8000-5342582d5553", blocks_new_risk: true,
        id: "01a11b2c-4e76-7046-bb02-858547cae6d3", kind: "ECONOMIC_EFFECT", material: true,
        opened_at: "2026-10-08T11:00:50Z", scope_type: "ECONOMIC_EFFECT", status: "MISMATCH",
      },
    ],
    reconciliation_truncated: false,
    unknown_effects: [
      {
        account_id: "01a11b2c-4dc6-76f9-a46e-d1a9405d14b5", created_at: "2026-10-08T11:00:50Z",
        effect_key: "portfolio-itest:e4146694-e0d3-400d-a580-357d58a129ac", effect_kind: "BRIDGE_SOURCE_FINALIZED",
        external_ref: "devnet-tx-portfolio", id: "01a11b2c-4e74-7dc3-8290-cbc57b1618df", posts_ledger: true,
        status: "UNKNOWN", subsystem: "BRIDGE", updated_at: "2026-10-08T11:00:50.932871Z",
      },
    ],
    unknown_effects_truncated: false,
  },
  value_domain: "SIMULATED",
};

type Mutable = Record<string, unknown>;

/** A deep copy of the answer with one change applied, so every case starts from the valid one. */
function broken(change: (copy: Mutable) => void): unknown {
  const copy = structuredClone(ANSWER) as Mutable;
  change(copy);
  return copy;
}

/** The value at a path of keys and indexes in a decoded answer. */
function at(doc: unknown, ...path: Array<string | number>): Mutable {
  let cur: unknown = doc;
  for (const step of path) cur = (cur as Record<string | number, unknown>)[step];
  return cur as Mutable;
}

function refuses(value: unknown, path: RegExp): void {
  assert.throws(
    () => checkPortfolio(value),
    (error: unknown) => {
      assert.ok(error instanceof ContractViolation, `expected a ContractViolation, got ${String(error)}`);
      assert.match(error.path, path);
      return true;
    },
  );
}

const answer = (): Portfolio => checkPortfolio(structuredClone(ANSWER));

/* --------------------------------------------------------------------------
 * The runtime contract
 * ------------------------------------------------------------------------ */

test("the backend's own answer is accepted unchanged", () => {
  assert.deepEqual(checkPortfolio(structuredClone(ANSWER)), ANSWER);
});

test("a section carries its answer exactly when PRESENT and its problem exactly when UNAVAILABLE", () => {
  const problem = { code: "STALE_MARKET_DATA", detail: "no mark" };
  refuses(broken((p) => { delete at(p, "buying_power").buying_power; }), /\.buying_power\.state$/);
  refuses(broken((p) => { at(p, "positions", "margin").state = "UNAVAILABLE"; }), /\.positions\.margin\.state$/);
  refuses(
    broken((p) => {
      const m = at(p, "positions", "margin");
      m.state = "UNAVAILABLE";
      delete m.account;
    }),
    /\.positions\.margin\.state$/,
  );
  refuses(broken((p) => { at(p, "positions", "derivatives").state = "NOT_CONFIGURED"; }), /\.positions\.derivatives\.state$/);
  refuses(
    broken((p) => {
      const o = at(p, "positions", "order_books");
      o.state = "NONE";
      delete o.orders;
    }),
    /\.positions\.order_books\.state$/,
  );
  refuses(
    broken((p) => {
      const pm = at(p, "positions", "prediction_markets");
      pm.state = "NOT_CONFIGURED";
    }),
    /\.positions\.prediction_markets\.markets$/,
  );
  refuses(
    broken((p) => {
      const eq = at(p, "positions", "equities");
      eq.state = "UNAVAILABLE";
      eq.problem = problem;
      delete eq.pending_settlements;
    }),
    /\.positions\.equities\.holdings$/,
  );
  refuses(broken((p) => { at(p, "risk", "decisions").state = "NOT_CONFIGURED"; }), /\.risk\.decisions\.items$/);

  // The shapes that are allowed: no margin account, a refusal with its problem.
  assert.doesNotThrow(() =>
    checkPortfolio(
      broken((p) => {
        const m = at(p, "positions", "margin");
        m.state = "NONE";
        delete m.account;
      }),
    ),
  );
  assert.doesNotThrow(() =>
    checkPortfolio(
      broken((p) => {
        const d = at(p, "positions", "derivatives");
        d.state = "UNAVAILABLE";
        d.problem = problem;
        delete d.positions;
      }),
    ),
  );
});

test("the delegated authority carries all of its pages or none, and performance fees their periods exactly when PRESENT", () => {
  const empty = { items: [], next_cursor: null, value_domain: "SIMULATED" };
  const present = (p: Mutable): void => {
    Object.assign(at(p, "delegation"), {
      state: "PRESENT", capabilities: empty, swaps_awaiting_approval: empty, withdrawals_awaiting_approval: empty,
    });
  };
  assert.doesNotThrow(() => checkPortfolio(broken(present)), "PRESENT with every page");
  refuses(broken((p) => { at(p, "delegation").state = "PRESENT"; }), /\.delegation\.state$/);
  refuses(
    broken((p) => {
      present(p);
      delete at(p, "delegation").withdrawals_awaiting_approval;
    }),
    /\.delegation$/,
  );
  refuses(broken((p) => { at(p, "delegation").capabilities = empty; }), /\.delegation$/);
  // An embedded page is held to the agent authority section's own spec.
  refuses(
    broken((p) => {
      present(p);
      at(p, "delegation").capabilities = { items: [], value_domain: "SIMULATED" };
    }),
    /\.delegation\.capabilities\.next_cursor$/,
  );

  refuses(broken((p) => { at(p, "performance_fees").state = "UNAVAILABLE"; }), /\.performance_fees\.state$/);
  assert.doesNotThrow(() =>
    checkPortfolio(
      broken((p) => {
        Object.assign(at(p, "performance_fees"), { state: "UNAVAILABLE", problem: { code: "UNSUPPORTED", detail: "no fee ledger" } });
      }),
    ),
  );
  assert.doesNotThrow(() =>
    checkPortfolio(broken((p) => { Object.assign(at(p, "performance_fees"), { state: "PRESENT", periods: empty }); })),
  );
});

test("a valuation carries a value unless UNVALUED and a mark exactly when MARKED", () => {
  refuses(broken((p) => { delete at(p, "balances", 2, "valuation").mark; }), /\.balances\[2\]\.valuation\.status$/);
  refuses(broken((p) => { at(p, "balances", 0, "valuation").value = "1"; }), /\.balances\[0\]\.valuation\.status$/);
  refuses(broken((p) => { delete at(p, "balances", 0, "valuation").problem; }), /\.balances\[0\]\.valuation\.status$/);
  refuses(
    broken((p) => { at(p, "balances", 3, "valuation").mark = at(p, "balances", 2, "valuation").mark; }),
    /\.balances\[3\]\.valuation\.status$/,
  );
  refuses(broken((p) => { at(p, "balances", 0, "valuation").owed_value = "1"; }), /\.balances\[0\]\.valuation\.status$/);
});

test("a figure that is not exact base units, or not in the simulated domain, is refused", () => {
  refuses(broken((p) => { at(p, "balances", 3).wallet = "98200.75"; }), /\.balances\[3\]\.wallet$/);
  refuses(broken((p) => { at(p, "balances", 3).value_domain = "REAL"; }), /\.balances\[3\]\.value_domain$/);
  refuses(broken((p) => { (at(p, "balances", 3).reservations as Mutable[])[0] = { purpose: "LOANS", quantity: "1", reservations: 1 }; }), /\.reservations\[0\]\.purpose$/);
  refuses(broken((p) => { at(p, "balances", 2, "valuation", "mark").price = "1e2"; }), /\.valuation\.mark\.price$/);
  refuses(broken((p) => { at(p).value_domain = "LIVE"; }), /\.value_domain$/);
  // The embedded reads are held to their own subsystems' specs.
  refuses(broken((p) => { at(p, "buying_power", "buying_power").gross_equity = "99990"; }), /\.buying_power\.buying_power\.gross_equity$/);
  refuses(broken((p) => { at(p, "positions", "margin", "account", "evaluation").equity = "949.45"; }), /\.evaluation\.equity$/);
  refuses(broken((p) => { at(p, "positions", "order_books", "orders", "items", 0).remaining_quantity = "1.0"; }), /\.remaining_quantity$/);
});

test("the embedded reads are held to their subsystems' own rules", () => {
  // A derivatives page whose book status contradicts what it carries.
  refuses(broken((p) => { delete at(p, "positions", "derivatives", "positions").book; }), /\.derivatives\.positions\.book_status$/);
  // A pending settlement that says when it settled.
  refuses(
    broken((p) => { at(p, "positions", "equities", "pending_settlements", "items", 0).settled_at = "2026-10-08T12:00:00Z"; }),
    /\.pending_settlements\.items\[0\]\.settled_at$/,
  );
  // Positions of another market than the one they are shown under.
  refuses(
    broken((p) => { at(p, "positions", "prediction_markets", "markets", 0, "positions").market_id = SANDBOX_DOLLAR; }),
    /\.markets\[0\]\.positions\.market_id$/,
  );
});

test("the portfolio's query sits with the account's other rows", () => {
  const key = portfolioKeys.portfolio("acct-1");
  assert.deepEqual(key.slice(0, advancedKeys.account("acct-1").length), [...advancedKeys.account("acct-1")]);
});

/* --------------------------------------------------------------------------
 * Pending capital
 * ------------------------------------------------------------------------ */

test("pending capital is every reservation and every balance still on its way, row by row", () => {
  const p = answer();
  const rows = pendingCapital(p.balances, p.positions.prediction_markets.markets);
  assert.deepEqual(
    rows.map((r) => [r.symbol, r.kind, r.label, r.code, r.quantity, r.route.join("/")]),
    [
      ["SBX-USD", "RESERVED", "Open orders", "INTERNAL_MATCHING", "2494980000", "order-books/orders"],
      ["ACME", "HELD", "Awaiting settlement", "UNSETTLED_RECEIVABLE", "2000000", "equities/settlements"],
    ],
    "collateral and debt are not pending capital; a reservation and an unsettled balance are",
  );
});

test("withdrawal holds, value in flight and an unclaimed held payout are pending; a claimed payout is not", () => {
  const p = answer();
  const usd = p.balances[3];
  assert.ok(usd !== undefined);
  const balances = [
    {
      ...usd,
      withdrawal_holds: "1500000",
      held_elsewhere: [...usd.held_elsewhere, { code: "BRIDGE_IN_FLIGHT" as const, quantity: "7000000" }],
    },
  ];
  const market = p.positions.prediction_markets.markets[0];
  assert.ok(market !== undefined);
  const held = {
    id: "01a11b2c-4f20-7000-8000-0000000000a1", market_id: MARKET, settlement_id: "01a11b2c-4f20-7000-8000-0000000000a2",
    account_id: p.account_id, kind: "PAYOUT" as const, amount: "3000000", reason_codes: ["RISK_ACCOUNT_STATUS"],
    refused_intent_id: "01a11b2c-4f20-7000-8000-0000000000a3", journal_transaction_id: "01a11b2c-4f20-7000-8000-0000000000a4",
    held_at: "2026-10-08T12:00:00Z", claimed: false,
  };
  const unclaimed = [{ ...market, positions: { ...market.positions, held_payout: held } }];
  const rows = pendingCapital(balances, unclaimed);
  assert.deepEqual(
    rows.map((r) => [r.kind, r.label, r.quantity]),
    [
      ["RESERVED", "Open orders", "2494980000"],
      ["WITHDRAWAL_HOLD", "Withdrawal holds", "1500000"],
      ["HELD", "Bridge in flight", "7000000"],
      ["HELD_PAYOUT", "Held payout of PFPM-841DE2B1", "3000000"],
    ],
  );
  assert.deepEqual(rows[3]?.route, ["prediction-markets", MARKET]);
  assert.equal(rows[3]?.symbol, "SBX-USD", "a held payout is in the market's collateral");

  const claimed = [{ ...market, positions: { ...market.positions, held_payout: { ...held, claimed: true } } }];
  assert.deepEqual(
    pendingCapital(balances, claimed).map((r) => r.kind),
    ["RESERVED", "WITHDRAWAL_HOLD", "HELD"],
    "a claimed payout has been delivered",
  );
});

test("a balance with nothing pending contributes no row", () => {
  const p = answer();
  const shares = p.balances.filter((b) => b.symbol.startsWith("PFPM"));
  assert.equal(shares.length, 2);
  assert.deepEqual(pendingCapital(shares, []), []);
});

/* --------------------------------------------------------------------------
 * Names, states and leverage
 * ------------------------------------------------------------------------ */

test("every purpose and every ledger code the contract declares has words and a page", () => {
  for (const purpose of RESERVATION_PURPOSES) {
    assert.ok(PURPOSE_COPY[purpose].label.length > 0, purpose);
    assert.ok(PURPOSE_COPY[purpose].route.length > 0, purpose);
  }
  for (const code of PORTFOLIO_LEDGER_CODES) {
    assert.ok(LEDGER_CODE_COPY[code].label.length > 0, code);
    assert.ok(LEDGER_CODE_COPY[code].route.length > 0, code);
  }
  assert.equal(purposeLabel("OPEN_ORDERS"), "Open orders");
  assert.equal(ledgerCodeLabel("BRIDGE_IN_FLIGHT"), "Bridge in flight");
  assert.equal(purposeLabel("SOMETHING_NEW"), "SOMETHING_NEW", "an unknown purpose is shown as the backend's own code");
  assert.equal(ledgerCodeLabel("SOMETHING_NEW"), "SOMETHING_NEW");
});

test("a section without an answer says why, and never looks like an empty answer", () => {
  assert.deepEqual([...PORTFOLIO_SECTION_STATES], ["PRESENT", "NONE", "NOT_CONFIGURED", "UNAVAILABLE"]);
  assert.match(sectionStateCopy("NOT_CONFIGURED", "Equities").title, /not configured in this deployment/);
  assert.match(sectionStateCopy("NOT_CONFIGURED", "Equities").body, /not the same as holding nothing/);
  assert.match(sectionStateCopy("NONE", "Margin account").title, /^No margin account/);
  assert.match(sectionStateCopy("UNAVAILABLE", "Derivatives").title, /could not be read/);
  assert.equal(sectionStateTone("PRESENT"), "good");
  assert.equal(sectionStateTone("UNAVAILABLE"), "bad");
  assert.equal(sectionStateTone("NOT_CONFIGURED"), "warn");
  assert.equal(sectionStateTone("NONE"), "neutral");
});

test("a leverage in basis points is written by moving the decimal point, never by dividing", () => {
  assert.equal(formatLeverageBps(28654), "2.8654x");
  assert.equal(formatLeverageBps(5267), "0.5267x", "the captured margin account's leverage");
  assert.equal(formatLeverageBps(10000), "1x");
  assert.equal(formatLeverageBps(50000), "5x");
  assert.equal(formatLeverageBps(12340), "1.234x");
  assert.equal(formatLeverageBps(5), "0.0005x");
  assert.equal(formatLeverageBps(0), "0x");
  assert.equal(formatLeverageBps(-1), "unreadable leverage");
});

test("zero is judged on the digits of the exact string", () => {
  assert.equal(isZeroQuantity("0"), true);
  assert.equal(isZeroQuantity("000"), true);
  assert.equal(isZeroQuantity("-0"), true);
  assert.equal(isZeroQuantity("10"), false);
  assert.equal(isZeroQuantity("1"), false);
  assert.equal(at(ANSWER, "balances", 4).asset_id, ACME, "the fixture's equity share is the holding's");
});
