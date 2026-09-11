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
  // `profile` and `onboarding` were added to `/me` additively and are absent
  // for a principal with no profile row, so they are optional here and checked
  // only for shape. Their contents are validated where they are used, by
  // `userProfileSpec` and `onboardingSpec`: a principal that arrives without a
  // usable profile must still sign in, because the screen that fixes it is
  // behind the session.
  optional: { step_up_valid_until: "timestamp", profile: "object", onboarding: "object" },
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

/* ---------------------------------------------------------------------------
 * The product surfaces: profile, onboarding, legal documents, notifications.
 * ------------------------------------------------------------------------ */

/**
 * The profile is product state, not identity. It carries no e-mail address,
 * legal name or date of birth — those are sealed in `identity_pii` and this
 * surface has no route to them — so nothing here needs redacting.
 */
export const userProfileSpec: Spec = {
  required: { user_id: "uuid", locale: "string", time_zone: "string", avatar_seed: "string", created_at: "timestamp" },
  optional: { display_name: "string", handle: "string", updated_at: "timestamp" },
};

/**
 * Onboarding is timestamps, not a state machine (D-053). The steps are
 * independent, may be done in any order, and cannot be undone, so the client
 * reads `complete` per step rather than deriving a position in a sequence.
 */
export const onboardingSpec: Spec = {
  required: { started_at: "timestamp", complete: "boolean" },
  optional: { completed_at: "timestamp", next_step: "string" },
  arrays: {
    steps: { required: true, spec: { required: { key: "string", complete: "boolean" }, optional: { completed_at: "timestamp" } } },
  },
};

/**
 * One legal document at one version.
 *
 * `body` is optional in the contract and is the thing that matters: the
 * acceptance record hashes the exact bytes shown, so a screen that asks for an
 * acceptance must render `body` and nothing else. A document that arrives
 * without one cannot honestly be accepted, and the terms step says so rather
 * than substituting text of its own.
 */
export const legalDocumentSpec: Spec = {
  required: {
    document_id: "string",
    version: "string",
    title: "string",
    content_hash: "string",
    requirement: "string",
    counsel_review_required: "boolean",
    accepted: "boolean",
  },
  optional: { body: "string", accepted_at: "timestamp" },
};

export const termsStateSpec: Spec = {
  arrays: {
    documents: { required: true, spec: legalDocumentSpec },
  },
};

export const unreadCountSpec: Spec = {
  required: { count: "integer" },
};

/* --------------------------------------------------------------------------
 * The markets surfaces: discovery, the trade screen, charts and the tape
 * (product goal §12–§14, §47).
 *
 * Every price here is `quantity` — an exact integer at the market's own
 * `price_scale` — and every amount is base units. None of them is `usd` and
 * none of them is `decimal`: PART LIV forbids an external value for a Credit,
 * so a response that tried to hand this app a currency figure for a native
 * market would be refused at this boundary rather than rendered.
 *
 * `price_scale` and `asset_decimals` are REQUIRED on every one of these,
 * because they are how the digits are read. A response that carried a price
 * without saying what scale it is on is not a price, it is a number, and the
 * markets page would draw it a factor of ten to the twelve wrong (F-44).
 * ------------------------------------------------------------------------ */

export const nativeMarketSummarySpec: Spec = {
  required: {
    market_id: "uuid",
    asset_id: "uuid",
    credit_asset_id: "uuid",
    name: "string",
    symbol: "string",
    market_status: "string",
    asset_status: "string",
    creator_account_id: "uuid",
    last_price: "quantity",
    price_scale: "integer",
    asset_decimals: "integer",
    virtual_credit_reserve: "quantity",
    initial_asset_reserve: "quantity",
    real_credit_reserve: "quantity",
    asset_reserve: "quantity",
    liquidity_credits: "quantity",
    circulating_supply: "quantity",
    max_supply: "quantity",
    credit_volume_24h: "quantity",
    trades_24h: "integer",
    platform_fee_bps: "integer",
    creator_fee_bps: "integer",
    // Required, and required for a reason: a demo market that arrived without
    // its label would render as an ordinary one. The absence of a flag is not
    // "not a demo" — it is a response this app cannot label honestly.
    demo: "boolean",
    created_at: "timestamp",
  },
  optional: {
    description: "string",
    image_url: "string",
    moderation_state: "string",
    reference_price_24h: "quantity",
    // A signed integer: it is the 24-hour move, and a fall is a real outcome.
    change_24h_bps: "integer",
    has_24h_change: "boolean",
    state_version: "integer",
    activated_at: "timestamp",
  },
};

export const nativeMarketPageSpec: Spec = {
  required: { sort: "string", stable: "boolean" },
  arrays: { markets: { required: true, spec: nativeMarketSummarySpec } },
};

/**
 * The limits in force on one market (goal §47).
 *
 * Only the policy version is required, and that is the contract's own answer
 * rather than a convenience: the compiled-in safety policy ships with the
 * circuit breaker DISARMED (D-065), and a disarmed breaker has no move
 * threshold to report. Rendering a zero there would state a limit of nothing —
 * the strictest possible breaker — where the truth is that there is no breaker.
 * So every limit is optional here and the panel renders each one's absence as
 * an absence.
 */
export const marketSafetyLimitsSpec: Spec = {
  required: { safety_policy_version: "string" },
  optional: {
    max_price_impact_bps: "integer",
    max_slippage_bps: "integer",
    circuit_breaker_move_bps: "integer",
    circuit_breaker_window_seconds: "integer",
    min_opening_liquidity_credits: "quantity",
    creator_may_buy_own_asset: "boolean",
    risk_policy_version: "string",
    max_native_market_concentration_bps: "integer",
    max_creator_concentration_bps: "integer",
  },
};

export const nativeMarketDetailSpec: Spec = {
  required: { market: "object", limits_in_force: "object" },
  arrays: {
    top_holders: { spec: { required: { account_id: "uuid", quantity: "quantity" } } },
  },
};

export const nativeCandlePageSpec: Spec = {
  required: {
    market_id: "uuid",
    interval: "string",
    from: "timestamp",
    to: "timestamp",
    price_scale: "integer",
    asset_decimals: "integer",
  },
  arrays: {
    candles: {
      required: true,
      spec: {
        required: {
          open_time: "timestamp",
          open: "quantity",
          high: "quantity",
          low: "quantity",
          close: "quantity",
          credit_volume: "quantity",
          asset_volume: "quantity",
          trades: "integer",
        },
      },
    },
  },
};

export const nativeTradePageSpec: Spec = {
  required: { market_id: "uuid", price_scale: "integer", asset_decimals: "integer" },
  arrays: {
    trades: {
      required: true,
      spec: {
        required: {
          seq: "integer",
          side: "string",
          effective_price: "quantity",
          spot_price_after: "quantity",
          credit_volume: "quantity",
          asset_volume: "quantity",
          printed_at: "timestamp",
        },
        optional: { spot_price_before: "quantity" },
      },
    },
  },
};

/**
 * The portfolio, read by the trade screen for one market's position.
 *
 * The whole document is validated even though the ticket needs one position:
 * a response whose totals or Credit breakdown are malformed is a response this
 * app cannot trust for the position either, and validating the part it reads
 * while ignoring the rest would be choosing which half of a broken answer to
 * believe.
 *
 * `temperature` is required on the document and on every position, because it
 * is what says whether a figure is closed-loop Credits, money at a provider or
 * a sandbox rehearsal. A position without one cannot be rendered honestly.
 */
export const nativePortfolioSpec: Spec = {
  required: { account_id: "uuid", as_of: "timestamp", temperature: "string", credits: "object", totals: "object" },
  arrays: {
    positions: {
      required: true,
      spec: {
        required: {
          asset_id: "uuid",
          symbol: "string",
          quantity: "quantity",
          cost_basis_credits: "quantity",
          realized_pnl_credits: "quantity",
          fees_paid_credits: "quantity",
          market_value_credits: "quantity",
          unrealized_pnl_credits: "quantity",
          total_pnl_credits: "quantity",
          price_scale: "integer",
          asset_decimals: "integer",
          temperature: "string",
        },
        optional: {
          market_id: "uuid",
          name: "string",
          market_status: "string",
          average_cost_credits: "quantity",
          spot_price: "quantity",
          units_bought_total: "quantity",
          units_sold_total: "quantity",
          allocation_units: "quantity",
          fill_count: "integer",
          demo: "boolean",
          first_acquired_at: "timestamp",
          last_trade_at: "timestamp",
        },
      },
    },
  },
};
