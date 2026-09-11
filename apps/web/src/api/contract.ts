/**
 * Runtime validation at the API boundary (PART 114).
 *
 * Types from the generated client describe what the contract *says*; this
 * checks what actually arrived. It matters most for money: a USD field that
 * does not match `^-?\d+\.\d{2}$` is refused here, so a malformed figure
 * surfaces as an explicit error rather than being rendered next to a dollar
 * sign. The UI would rather say "this response is not usable" than show a
 * customer a number that is not their money.
 *
 * Validation is deliberately shallow-but-strict: it covers the fields the
 * interface renders, and it never repairs anything.
 */
import { DECIMAL_PATTERN, QUANTITY_PATTERN, USD_PATTERN } from "../lib/money.ts";

export class ContractViolation extends Error {
  readonly path: string;
  constructor(path: string, detail: string) {
    super(`the backend response did not match the API contract at ${path}: ${detail}`);
    this.name = "ContractViolation";
    this.path = path;
  }
}

export type FieldKind =
  | "string"
  | "number"
  | "integer"
  | "boolean"
  | "usd"
  | "quantity"
  | "decimal"
  | "timestamp"
  | "uuid"
  | "object"
  | "any";

const UUID_PATTERN = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

function checkField(value: unknown, kind: FieldKind, path: string): void {
  switch (kind) {
    case "any":
      return;
    case "string":
      if (typeof value !== "string") throw new ContractViolation(path, "expected a string");
      return;
    case "number":
      if (typeof value !== "number" || !Number.isFinite(value)) {
        throw new ContractViolation(path, "expected a finite number");
      }
      return;
    case "integer":
      if (typeof value !== "number" || !Number.isInteger(value)) {
        throw new ContractViolation(path, "expected an integer");
      }
      return;
    case "boolean":
      if (typeof value !== "boolean") throw new ContractViolation(path, "expected a boolean");
      return;
    case "usd":
      if (typeof value !== "string" || !USD_PATTERN.test(value)) {
        throw new ContractViolation(path, "expected a USD decimal string with two fraction digits");
      }
      return;
    case "quantity":
      if (typeof value !== "string" || !QUANTITY_PATTERN.test(value)) {
        throw new ContractViolation(path, "expected exact base units as an integer string");
      }
      return;
    case "decimal":
      if (typeof value !== "string" || !DECIMAL_PATTERN.test(value)) {
        throw new ContractViolation(path, "expected a decimal string");
      }
      return;
    case "timestamp":
      if (typeof value !== "string" || value.length < 20) {
        throw new ContractViolation(path, "expected an RFC 3339 timestamp");
      }
      return;
    case "uuid":
      if (typeof value !== "string" || !UUID_PATTERN.test(value)) {
        throw new ContractViolation(path, "expected a UUID");
      }
      return;
    case "object":
      if (typeof value !== "object" || value === null || Array.isArray(value)) {
        throw new ContractViolation(path, "expected an object");
      }
      return;
  }
}

export interface Spec {
  /** Fields that must be present and well-formed. */
  readonly required?: Readonly<Record<string, FieldKind>>;
  /** Fields checked only when present and not null. */
  readonly optional?: Readonly<Record<string, FieldKind>>;
  /** Nested arrays of objects, each element validated against its own spec. */
  readonly arrays?: Readonly<Record<string, { readonly required?: boolean; readonly spec: Spec }>>;
}

function checkObject(value: unknown, spec: Spec, path: string): void {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new ContractViolation(path, "expected an object");
  }
  const record = value as Record<string, unknown>;
  for (const [field, kind] of Object.entries(spec.required ?? {})) {
    if (!(field in record) || record[field] === undefined || record[field] === null) {
      throw new ContractViolation(`${path}.${field}`, "required field is missing");
    }
    checkField(record[field], kind, `${path}.${field}`);
  }
  for (const [field, kind] of Object.entries(spec.optional ?? {})) {
    const present = record[field];
    if (present === undefined || present === null || present === "") continue;
    checkField(present, kind, `${path}.${field}`);
  }
  for (const [field, entry] of Object.entries(spec.arrays ?? {})) {
    const list = record[field];
    if (list === undefined || list === null) {
      if (entry.required === true) {
        throw new ContractViolation(`${path}.${field}`, "required array is missing");
      }
      continue;
    }
    if (!Array.isArray(list)) throw new ContractViolation(`${path}.${field}`, "expected an array");
    list.forEach((item, index) => {
      checkObject(item, entry.spec, `${path}.${field}[${String(index)}]`);
    });
  }
}

/**
 * Validates a decoded response and returns it under its generated type.
 * The cast is the point where a checked shape becomes a typed one, and it only
 * happens after the check.
 */
export function validated<T>(value: unknown, spec: Spec, path: string): T {
  checkObject(value, spec, path);
  return value as T;
}

/**
 * Validates every element of a top-level array response.
 *
 * The parameter is `readonly unknown[] | undefined` and not `unknown`, which is
 * the whole point. The generated client already knows that `/native-assets`
 * answers `{ items: NativeAsset[] }` and `/accounts` answers `Account[]`; when
 * this took `unknown` it threw that knowledge away, and calling it on a paged
 * response compiled cleanly and threw at runtime on every page load. That was
 * F-32, and it reached the browser tests, which could not see it either.
 *
 * With the parameter typed, `validatedList(data, ...)` on a paged endpoint is a
 * compile error, and the failure moves from a customer's screen to `tsc`.
 */
export function validatedList<T>(value: readonly unknown[] | undefined, spec: Spec, path: string): T[] {
  if (!Array.isArray(value)) throw new ContractViolation(path, "expected an array");
  value.forEach((item, index) => {
    checkObject(item, spec, `${path}[${String(index)}]`);
  });
  return value as T[];
}

/* ---------------------------------------------------------------------------
 * Specs for the responses this app renders.
 * ------------------------------------------------------------------------ */

export const principalSpec: Spec = {
  required: { subject_id: "uuid", actor_type: "string", auth_time: "timestamp" },
  optional: { step_up_valid_until: "timestamp" },
};

export const accountSpec: Spec = {
  required: { id: "uuid", kind: "string", status: "string", created_at: "timestamp" },
  optional: { status_reason: "string" },
};

export const buyingPowerSpec: Spec = {
  required: {
    portfolio_value: "usd",
    buying_power: "usd",
    available_now: "usd",
    reserved: "usd",
    pending: "usd",
    withdrawable: "usd",
    policy_version: "string",
    as_of: "timestamp",
    purpose: "string",
  },
  arrays: {
    underlying_balances: {
      required: true,
      spec: {
        required: {
          asset: "uuid",
          symbol: "string",
          decimals: "integer",
          quantity: "quantity",
          usd_value: "usd",
          price_ref: "string",
          status: "string",
        },
      },
    },
    haircuts: {
      required: true,
      spec: { required: { asset: "uuid", factor_bps: "integer", reason: "string" } },
    },
    restrictions: {
      required: true,
      spec: { required: { code: "string", detail: "string", scope: "string", blocking: "boolean" } },
    },
  },
};

export const holdingsSpec: Spec = {
  required: { as_of: "timestamp" },
  arrays: {
    holdings: {
      required: true,
      spec: {
        required: {
          asset: "uuid",
          symbol: "string",
          decimals: "integer",
          quantity: "quantity",
          usd_mark: "usd",
          cost_basis_usd: "usd",
          unrealized_pnl_usd: "usd",
        },
        optional: {
          chain: "string",
          mint_address: "string",
          price_ref: "string",
          realized_pnl_usd: "usd",
          location: "string",
        },
      },
    },
  },
};

export const assetSpec: Spec = {
  required: {
    id: "uuid",
    chain: "string",
    mint_address: "string",
    kind: "string",
    symbol: "string",
    name: "string",
    decimals: "integer",
    is_stablecoin: "boolean",
    risk_class: "string",
    status: "string",
  },
  optional: { peg_currency: "string" },
};

export const instrumentSpec: Spec = {
  required: {
    id: "uuid",
    type: "string",
    canonical_name: "string",
    settlement_asset: "uuid",
    risk_class: "string",
    status: "string",
    active_from: "timestamp",
  },
  optional: { base_asset: "uuid", quote_asset: "uuid", active_until: "timestamp" },
};

export const instrumentDetailSpec: Spec = {
  ...instrumentSpec,
  arrays: {
    listings: {
      required: true,
      spec: {
        required: {
          id: "uuid",
          venue: "string",
          venue_status: "string",
          status: "string",
          network: "string",
          base_precision: "integer",
          quote_precision: "integer",
          min_notional_quote: "quantity",
        },
        optional: { base_mint: "string", quote_mint: "string", max_notional_quote: "quantity" },
      },
    },
  },
};

export const quoteDisclosureSpec: Spec = {
  required: {
    quote_id: "uuid",
    provider: "string",
    venue: "string",
    input_asset: "uuid",
    input_quantity: "quantity",
    output_asset: "uuid",
    expected_output: "quantity",
    minimum_output: "quantity",
    price_impact_bps: "integer",
    slippage_bps: "integer",
    venue_fee: "quantity",
    network_fee_estimate: "quantity",
    platform_fee: "quantity",
    platform_fee_bps: "integer",
    total_estimated_cost_usd: "usd",
    received_at: "timestamp",
    expires_at: "timestamp",
  },
  optional: {
    effective_price: "decimal",
    venue_fee_asset: "uuid",
    network_fee_asset: "uuid",
    fee_policy_version: "string",
  },
};

export const intentSpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    actor_type: "string",
    action: "string",
    instrument_id: "uuid",
    status: "string",
    mode: "string",
    requested_at: "timestamp",
    received_at: "timestamp",
    correlation_id: "string",
  },
  optional: {
    agent_id: "uuid",
    notional_usd: "usd",
    target_exposure_usd: "usd",
    quantity: "quantity",
    rejection_code: "string",
    deadline: "timestamp",
    terminal_at: "timestamp",
    order_id: "uuid",
    plan_id: "uuid",
  },
};

export const orderSpec: Spec = {
  required: {
    id: "uuid",
    intent_id: "uuid",
    account_id: "uuid",
    instrument_id: "uuid",
    side: "string",
    mode: "string",
    status: "string",
    input_asset: "uuid",
    input_quantity: "quantity",
    output_asset: "uuid",
    min_output_quantity: "quantity",
    filled_input_quantity: "quantity",
    filled_output_quantity: "quantity",
    created_at: "timestamp",
  },
  optional: { rejection_code: "string", terminal_at: "timestamp" },
};

export const depositSpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    provider: "string",
    status: "string",
    expected_asset: "uuid",
    buying_power_eligible: "boolean",
    withdrawal_eligible: "boolean",
    created_at: "timestamp",
  },
  optional: {
    provider_session_id: "string",
    client_secret_ref: "string",
    fiat_amount: "string",
    fiat_currency: "string",
    expected_quantity: "quantity",
    observed_quantity: "quantity",
    tx_signature: "string",
    reversible_until: "timestamp",
    available_at: "timestamp",
  },
};

export const activityItemSpec: Spec = {
  required: { id: "string", kind: "string", occurred_at: "timestamp", summary: "string" },
  optional: { correlation_id: "string", references: "object", detail: "object" },
};

export const journalTransactionSpec: Spec = {
  required: {
    id: "uuid",
    kind: "string",
    effective_at: "timestamp",
    posted_at: "timestamp",
    content_hash: "string",
  },
  optional: { description: "string", reason_code: "string", reversal_of: "uuid" },
  arrays: {
    entries: {
      required: true,
      spec: {
        required: { seq: "integer", account_code: "string", asset: "uuid", side: "string", quantity: "quantity" },
        optional: { usd_value: "usd" },
      },
    },
  },
};

export const sessionSpec: Spec = {
  required: { id: "uuid", created_at: "timestamp", last_seen_at: "timestamp", expires_at: "timestamp" },
  optional: { ip: "string", user_agent: "string", device_label: "string", revoked_at: "timestamp", current: "boolean" },
};

/** Spec for `{ items, next_cursor }` pages, given the spec of one item. */
export function pageSpec(item: Spec): Spec {
  return { arrays: { items: { required: true, spec: item } } };
}

/* --------------------------------------------------------------------------
 * The Nodal-native economy (gola.md PARTS XII-XXI, LII-LIV)
 *
 * Every quantity here is `quantity` — exact base units as an integer string —
 * and not one of them is `usd`. That is the contract enforcing PART LIV: there
 * is no approved external value for a Credit, so a response that tried to hand
 * this app a dollar figure for one would be refused at the boundary rather
 * than rendered.
 * ------------------------------------------------------------------------ */

export const creditBalanceSpec: Spec = {
  required: {
    account_id: "uuid",
    gross: "quantity",
    spendable: "quantity",
    frozen: "quantity",
    payout_eligible: "quantity",
    ineligible: "quantity",
    policy_version: "string",
  },
  optional: { reversed: "quantity", policy_hash: "string", by_origin: "object", by_finality: "object" },
};

export const nativeAssetSpec: Spec = {
  // supply and policy are REQUIRED by the contract and are checked here,
  // because the create flow shows a creator the exact economics they are about
  // to make permanent. A response missing them must be an error, not a page
  // that renders blanks where the numbers a creator agreed to should be.
  required: {
    asset_id: "uuid",
    creator_account_id: "uuid",
    name: "string",
    symbol: "string",
    status: "string",
    moderation_state: "string",
    supply: "object",
    policy: "object",
  },
  optional: {
    description: "string",
    image_url: "string",
    decimals: "integer",
    moderation_notes: "string",
    economics_locked_at: "timestamp",
    activated_at: "timestamp",
    created_at: "timestamp",
  },
};

export const nativeMarketSpec: Spec = {
  required: {
    market_id: "uuid",
    asset_id: "uuid",
    status: "string",
    real_credit_reserve: "quantity",
    asset_reserve: "quantity",
    state_version: "integer",
    platform_fee_bps: "integer",
    creator_fee_bps: "integer",
  },
  optional: {
    virtual_credit_reserve: "quantity",
    initial_asset_reserve: "quantity",
    circulating_supply: "quantity",
    spot_price: "quantity",
    price_scale: "integer",
    asset_decimals: "integer",
  },
  arrays: {
    top_holders: {
      spec: { optional: { account_id: "uuid", quantity: "quantity" } },
    },
  },
};

export const nativeQuoteSpec: Spec = {
  required: {
    quote_id: "uuid",
    market_id: "uuid",
    side: "string",
    input_amount: "quantity",
    expected_output: "quantity",
    state_version: "integer",
    expires_at: "timestamp",
  },
  optional: {
    platform_fee: "quantity",
    creator_fee: "quantity",
    spot_price_before: "quantity",
    effective_price: "quantity",
    price_scale: "integer",
    asset_decimals: "integer",
    slippage_bps: "integer",
  },
};

export const nativeFillSpec: Spec = {
  required: { fill_id: "uuid", market_id: "uuid", side: "string", state_version_after: "integer" },
  optional: {
    credits_in: "quantity",
    credits_out: "quantity",
    assets_in: "quantity",
    assets_out: "quantity",
    platform_fee: "quantity",
    creator_fee: "quantity",
    effective_price: "quantity",
    price_scale: "integer",
    asset_decimals: "integer",
    slippage_bps: "integer",
    real_credit_reserve_after: "quantity",
    asset_reserve_after: "quantity",
  },
  arrays: {
    alerts: { spec: { optional: { kind: "string", severity: "string", reason: "string" } } },
  },
};

export const internalProductSpec: Spec = {
  required: {
    product_id: "uuid",
    seller_account_id: "uuid",
    kind: "string",
    title: "string",
    price: "quantity",
    version: "integer",
    status: "string",
    earning_origin: "string",
  },
  optional: {
    description: "string",
    platform_fee: "quantity",
    seller_proceeds: "quantity",
    platform_fee_bps: "integer",
    terms_frozen: "boolean",
    published_at: "timestamp",
    created_at: "timestamp",
  },
};

export const internalSellerSpec: Spec = {
  required: { account_id: "uuid", display_name: "string", status: "string" },
  optional: { payout_account_id: "uuid", suspended_reason: "string", created_at: "timestamp" },
};

export const internalOrderSpec: Spec = {
  required: {
    order_id: "uuid",
    product_id: "uuid",
    product_version: "integer",
    buyer_account_id: "uuid",
    seller_account_id: "uuid",
    price: "quantity",
    platform_fee: "quantity",
    seller_proceeds: "quantity",
    earning_origin: "string",
  },
  optional: {
    earning_account_id: "uuid",
    journal_transaction_id: "uuid",
    created_at: "timestamp",
  },
};

export const payoutRequestSpec: Spec = {
  required: {
    payout_id: "uuid",
    account_id: "uuid",
    state: "string",
    requested_quantity: "quantity",
    reserved_quantity: "quantity",
    policy_version: "string",
  },
  optional: {
    destination_id: "uuid",
    settled_quantity: "quantity",
    eligible_quantity: "quantity",
    verification_would_suffice: "boolean",
    required_verification: "string",
    policy_hash: "string",
    failure_reason: "string",
    created_at: "timestamp",
  },
};

/** Spec for `{ items }` collections that carry no cursor. */
export function itemsSpec(item: Spec): Spec {
  return { arrays: { items: { required: true, spec: item } } };
}

/* --------------------------------------------------------------------------
 * Agents and strategies (goal §17, §18; openapi tags: agents)
 *
 * Two shapes here that the specs above never had to describe.
 *
 * The first is a bare array of strings — an agent's allowed universe, a
 * compiled version's effect set. `Spec.arrays` validates arrays OF OBJECTS,
 * which is what every paged response is, so a list of identifiers would have
 * passed through unchecked. `validatedStrings` closes that.
 *
 * The second is a nested object with money inside it. `checkObject` descends
 * into arrays but treats a nested object as opaque, and an agent's limits and
 * budget are exactly that: objects whose fields are exact Credit base units. A
 * budget that arrived as a float would have been rendered rather than refused.
 * `validatedAgent` therefore validates the nested parts explicitly, and it is
 * here rather than in a page so no caller can forget.
 * ------------------------------------------------------------------------ */

/** Validates an array of plain strings and returns it. */
export function validatedStrings(value: unknown, path: string): string[] {
  if (!Array.isArray(value)) throw new ContractViolation(path, "expected an array of strings");
  value.forEach((item, index) => {
    if (typeof item !== "string") {
      throw new ContractViolation(`${path}[${String(index)}]`, "expected a string");
    }
  });
  return value as string[];
}

export const strategyVersionSpec: Spec = {
  required: { id: "uuid", version: "integer", status: "string", ir_hash: "string", human_readable: "string" },
  optional: { built_at: "timestamp" },
};

export const strategySpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    name: "string",
    description: "string",
    source_kind: "string",
    status: "string",
    compiler_configured: "boolean",
    created_at: "timestamp",
  },
  optional: { updated_at: "timestamp", current_version: "object" },
};

export const compileResultSpec: Spec = {
  required: {
    strategy_id: "uuid",
    attempt_id: "uuid",
    attempt_no: "integer",
    outcome: "string",
    detail: "string",
  },
  optional: { version: "object" },
};

export const authorityLevelSpec: Spec = {
  required: { level: "integer", name: "string", summary: "string", enabled: "boolean" },
  optional: { required_capability: "string" },
};

export const agentScheduleSpec: Spec = {
  required: { kind: "string" },
  optional: { interval_minutes: "integer" },
};

export const agentLimitsSpec: Spec = {
  required: {
    budget_credits: "quantity",
    per_trade_cap_credits: "quantity",
    daily_loss_stop_credits: "quantity",
    max_position_share_bps: "integer",
    schedule: "object",
  },
};

export const agentBudgetSpec: Spec = {
  required: { granted_credits: "quantity", used_credits: "quantity", source: "string" },
};

export const agentRuntimeSpec: Spec = {
  required: { evaluator: "string", executor: "string", detail: "string" },
  optional: { last_heartbeat: "timestamp" },
};

export const agentPauseSpec: Spec = {
  required: { reason_code: "string", reason: "string", paused_by_actor_type: "string", paused_at: "timestamp" },
  optional: { open_orders_policy: "string" },
};

export const agentSpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    strategy_id: "uuid",
    strategy_version_id: "uuid",
    name: "string",
    stage: "string",
    state: "string",
    status: "string",
    authority: "object",
    limits: "object",
    budget: "object",
    runtime: "object",
    archived: "boolean",
    created_at: "timestamp",
  },
  optional: {
    mode: "string",
    pause: "object",
    runs_total: "integer",
    last_run_at: "timestamp",
    last_run_status: "string",
    granted_by_user_id: "uuid",
    granted_at: "timestamp",
    updated_at: "timestamp",
  },
};

/** The compiled version inside a strategy or a compile attempt, when there is one. */
function checkStrategyVersion(raw: unknown, path: string): void {
  if (raw === undefined || raw === null) return;
  validated<unknown>(raw, strategyVersionSpec, path);
  validatedStrings((raw as Record<string, unknown>)["effect_set"], `${path}.effect_set`);
}

/** A strategy, with its compiled version checked rather than assumed. */
export function validatedStrategy<T>(raw: unknown, path: string): T {
  const value = validated<T>(raw, strategySpec, path);
  checkStrategyVersion((raw as Record<string, unknown>)["current_version"], `${path}.current_version`);
  return value;
}

/** A compile attempt. Every outcome is a real answer, including "nothing was produced". */
export function validatedCompileResult<T>(raw: unknown, path: string): T {
  const value = validated<T>(raw, compileResultSpec, path);
  const record = raw as Record<string, unknown>;
  checkStrategyVersion(record["version"], `${path}.version`);
  for (const field of ["failure_codes", "clarifications"]) {
    const list = record[field];
    if (list !== undefined && list !== null) validatedStrings(list, `${path}.${field}`);
  }
  return value;
}

/**
 * An agent, including the Credit figures inside its limits and its budget.
 *
 * Those live one level down from the fields `checkObject` walks, so without
 * this they would reach a page unvalidated. A budget is a ceiling on value at
 * risk; it is not a field this app is willing to render on trust.
 */
export function validatedAgent<T>(raw: unknown, path: string): T {
  const value = validated<T>(raw, agentSpec, path);
  const record = raw as Record<string, unknown>;
  validated<unknown>(record["authority"], authorityLevelSpec, `${path}.authority`);
  const limits = validated<Record<string, unknown>>(record["limits"], agentLimitsSpec, `${path}.limits`);
  validated<unknown>(limits["schedule"], agentScheduleSpec, `${path}.limits.schedule`);
  validatedStrings(limits["allowed_asset_ids"], `${path}.limits.allowed_asset_ids`);
  validated<unknown>(record["budget"], agentBudgetSpec, `${path}.budget`);
  validated<unknown>(record["runtime"], agentRuntimeSpec, `${path}.runtime`);
  if (record["pause"] !== undefined && record["pause"] !== null) {
    validated<unknown>(record["pause"], agentPauseSpec, `${path}.pause`);
  }
  return value;
}
