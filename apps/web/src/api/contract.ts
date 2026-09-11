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
    // The scale the quantities below are expressed in. Required, because a
    // response without it leaves the browser to assume one -- and the browser
    // assumed six, in four separate files, while the server issued Credits at
    // a scale nothing had ever compared against the asset (F-151).
    credit_decimals: "integer",
    gross: "quantity",
    spendable: "quantity",
    frozen: "quantity",
    // Removed after a payment was reversed. Required, because a bucket the API
    // returns and no page renders is a number that can be wrong forever
    // (F-156).
    reversed: "quantity",
    payout_eligible: "quantity",
    ineligible: "quantity",
    policy_version: "string",
  },
  optional: { policy_hash: "string", by_origin: "object", by_finality: "object" },
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
    // The same shape the trade screen's holder list has, and it names nobody
    // either: this read takes no account, so not even the caller's own row is
    // marked (D-111).
    top_holders: {
      spec: {
        required: { rank: "integer", quantity: "quantity", share_bps: "integer" },
      },
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
 * Buying Credits, the account's own standing, and the notification centre.
 *
 * Three things are worth pointing at here.
 *
 * `creditPurchaseSpec` marks `sandbox` optional rather than required. The
 * backend populates it on every purchase, but the schema declares it optional
 * and an older API answers without it — and "this deployment did not say"
 * is a different fact from "this is real money". An absent flag labels
 * nothing; it never silently means live.
 *
 * `creditPricingSpec` checks the bounds as integers. They are minor units of
 * the pricing currency, which is the one place in this application where a
 * money-adjacent value legitimately arrives as a JSON number, because a count
 * of cents is a count.
 *
 * `notificationSpec` does NOT validate `data`, beyond it being an object. The
 * contract says it carries identifiers and state names and never a figure, so
 * there is no money field in it to protect; validating an open map would be
 * this file inventing a shape the API does not promise.
 * ------------------------------------------------------------------------ */

export const creditPricingSpec: Spec = {
  required: {
    version: "string",
    currency: "string",
    credits_per_major_unit: "integer",
    // The rest of the conversion. A page holding only the rate cannot compute
    // what a customer is about to be charged for: the quantity the server
    // issues is amount_minor * credits_per_major_unit * 10^decimals /
    // minor_units_per_major_unit. The Buy Credits page rendered "100 Credits
    // per 1 USD" from the rate alone and then rendered the result at a scale it
    // had hardcoded, and the two were a factor of a million apart (F-151).
    minor_units_per_major_unit: "integer",
    decimals: "integer",
    rounding: "string",
    min_amount_minor: "integer",
    max_amount_minor: "integer",
  },
};

export const creditPurchaseSpec: Spec = {
  required: {
    purchase_id: "uuid",
    account_id: "uuid",
    state: "string",
    credit_quantity: "quantity",
    amount_minor: "integer",
    currency: "string",
    pricing_version: "string",
    provider: "string",
  },
  optional: {
    sandbox: "boolean",
    // The mode that opened THIS payment. Absent for a purchase made before the
    // mode was recorded, which the API renders as sandbox (F-158).
    provider_mode: "string",
    client_secret: "string",
    reversible_at: "timestamp",
    settled_at: "timestamp",
    failure_reason: "string",
    created_at: "timestamp",
  },
};

export const notificationSpec: Spec = {
  required: {
    id: "uuid",
    kind: "string",
    severity: "string",
    title: "string",
    body: "string",
    occurred_at: "timestamp",
    sandbox: "boolean",
  },
  optional: {
    account_id: "uuid",
    resource_type: "string",
    resource_id: "string",
    data: "object",
    read_at: "timestamp",
  },
};

export const notificationPreferenceSpec: Spec = {
  required: { kind: "string", channel: "string", enabled: "boolean", enforced: "boolean" },
};

export const markedReadSpec: Spec = { required: { updated: "integer" } };

export const accountRestrictionSpec: Spec = {
  required: { code: "string", message: "string" },
  optional: { account_id: "uuid" },
};

export const closureRequestSpec: Spec = {
  required: { id: "uuid", state: "string", requested_at: "timestamp", cooling_off_until: "timestamp" },
  optional: { effectable: "boolean", decided_at: "timestamp", decided_reason: "string" },
};

export const myAccountSpec: Spec = {
  required: { user_id: "uuid", user_status: "string", cooling_off_days: "integer" },
  optional: { closure_request: "object" },
  arrays: {
    accounts: { required: true, spec: accountSpec },
    restrictions: { required: true, spec: accountRestrictionSpec },
  },
};

export const securitySummarySpec: Spec = {
  required: {
    active_sessions: "integer",
    mfa_present: "boolean",
    step_up_max_age_seconds: "integer",
  },
  optional: {
    last_login_at: "timestamp",
    last_step_up_at: "timestamp",
    step_up_valid_until: "timestamp",
    current_session_id: "uuid",
  },
};

export const meAuditEntrySpec: Spec = {
  // `id` is a string and not a uuid: the trail is stitched from two tables and
  // the identifiers are theirs, not this surface's to reshape.
  required: { id: "string", source: "string", action: "string", occurred_at: "timestamp" },
  optional: {
    severity: "string",
    resource_type: "string",
    resource_id: "string",
    actor_type: "string",
    ip: "string",
    user_agent: "string",
  },
};

/* --------------------------------------------------------------------------
 * Portfolio, the activity feed, and market discovery (goal §15, §16, §12).
 *
 * Every figure in these three responses is `quantity` — exact base units as an
 * integer string — and not one is `usd`, with a single exception noted below.
 * That is the contract enforcing PART LIV at the boundary: there is no approved
 * external value for a Credit, so a response that tried to hand this app a
 * dollar figure for a position would be refused here rather than rendered.
 *
 * The exception is `ActivityAmount` with `unit: "MONEY_MINOR"`, which is the
 * money side of a Credit purchase. It arrives as minor units of `currency` in
 * the same integer-string form, so it is `quantity` here too and is scaled by
 * the currency's minor units where it is rendered, never here.
 *
 * P&L is `quantity` rather than a separate kind because `SignedQuantity` has
 * exactly the same pattern — an optional sign and digits. A loss is a real
 * outcome, so P&L is signed where a balance is not, and the sign is carried
 * through to the glyph rather than being dropped into a colour.
 */

export const portfolioPositionSpec: Spec = {
  required: {
    asset_id: "uuid",
    symbol: "string",
    asset_decimals: "integer",
    price_scale: "integer",
    quantity: "quantity",
    cost_basis_credits: "quantity",
    market_value_credits: "quantity",
    fees_paid_credits: "quantity",
    realized_pnl_credits: "quantity",
    unrealized_pnl_credits: "quantity",
    total_pnl_credits: "quantity",
    temperature: "string",
  },
  optional: {
    name: "string",
    market_id: "uuid",
    market_status: "string",
    average_cost_credits: "quantity",
    spot_price: "quantity",
    allocation_units: "quantity",
    units_bought_total: "quantity",
    units_sold_total: "quantity",
    fill_count: "integer",
    first_acquired_at: "timestamp",
    last_trade_at: "timestamp",
    demo: "boolean",
  },
};

export const portfolioTotalsSpec: Spec = {
  required: {
    cost_basis_credits: "quantity",
    market_value_credits: "quantity",
    fees_paid_credits: "quantity",
    realized_pnl_credits: "quantity",
    unrealized_pnl_credits: "quantity",
    total_pnl_credits: "quantity",
    position_count: "integer",
    open_position_count: "integer",
  },
};

/**
 * The portfolio envelope.
 *
 * `credits` and `totals` are checked as objects here and then validated against
 * their own specs where the response is decoded: `Spec` describes arrays of
 * objects and flat fields, and bolting a nested-object form onto it to save two
 * lines at the call site would make every other spec in this file harder to
 * read.
 */
export const portfolioSpec: Spec = {
  required: {
    account_id: "uuid",
    as_of: "timestamp",
    credits: "object",
    totals: "object",
    temperature: "string",
  },
  arrays: { positions: { required: true, spec: portfolioPositionSpec } },
};

export const activityAmountSpec: Spec = {
  required: { unit: "string", value: "quantity", temperature: "string" },
  optional: { currency: "string", symbol: "string", origin: "string" },
};

export const activityFeedItemSpec: Spec = {
  required: {
    id: "string",
    kind: "string",
    occurred_at: "timestamp",
    summary: "string",
    simulated: "boolean",
    reference: "object",
  },
  optional: { status: "string" },
  arrays: { amounts: { required: true, spec: activityAmountSpec } },
};

export const nativeMarketSummarySpec: Spec = {
  required: {
    market_id: "uuid",
    asset_id: "uuid",
    credit_asset_id: "uuid",
    symbol: "string",
    name: "string",
    market_status: "string",
    asset_status: "string",
    asset_decimals: "integer",
    price_scale: "integer",
    last_price: "quantity",
    liquidity_credits: "quantity",
    credit_volume_24h: "quantity",
    circulating_supply: "quantity",
    max_supply: "quantity",
    real_credit_reserve: "quantity",
    virtual_credit_reserve: "quantity",
    asset_reserve: "quantity",
    initial_asset_reserve: "quantity",
    platform_fee_bps: "integer",
    creator_fee_bps: "integer",
    trades_24h: "integer",
    created_at: "timestamp",
    demo: "boolean",
  },
  optional: {
    description: "string",
    image_url: "string",
    moderation_state: "string",
    activated_at: "timestamp",
    state_version: "integer",
    change_24h_bps: "integer",
    has_24h_change: "boolean",
    reference_price_24h: "quantity",
  },
};

/* ---------------------------------------------------------------------------
 * The public reads (D-080): the legal registry and market discovery.
 * ------------------------------------------------------------------------ */

/**
 * A served legal document with no acceptance state.
 *
 * `body` is REQUIRED here, unlike on `LegalDocument`, and that difference is
 * the point of the endpoint: the public site renders the bytes rather than an
 * explainer, so a document that arrived without them would leave the page with
 * nothing honest to show. `content_hash` is the sha256 of exactly those bytes
 * and is the same value `/me/terms-acceptances` serves, so a visitor can check
 * that what they read before signing up is what they were later asked to
 * accept.
 */
export const publicLegalDocumentSpec: Spec = {
  required: {
    document_id: "string",
    version: "string",
    title: "string",
    content_hash: "string",
    requirement: "string",
    counsel_review_required: "boolean",
    body: "string",
  },
};

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

/* --------------------------------------------------------------------------
 * Verification, eligibility, payout destinations and payout quotes
 * (goal §19-§21, §23-§25; openapi tags: verification, eligibility, payouts)
 *
 * Two rules shape every spec below.
 *
 * NOT ONE OF THEM CARRIES PERSONAL DATA, and that is a property of the API
 * rather than of this file: there is no document identifier, no government
 * number and no date of birth anywhere in the verification surface, because a
 * provider holds the evidence and Nodal holds only the conclusion. A spec that
 * declared such a field would be describing a response this system never sends.
 *
 * EVERY MONEY FIGURE IS AN EXACT INTEGER OF BASE UNITS. A payout quote has a
 * gross, a fee and a net on the Credit side and the same three in the
 * provider's minor units, and there is no rate field holding a decimal between
 * them. `integer` on the minor-unit fields is not a float creeping in: they are
 * whole counts of the smallest unit of a currency, which is what int64 is for,
 * and the Credit side beside them is validated as `quantity`.
 * ------------------------------------------------------------------------ */

export const verificationSessionSpec: Spec = {
  required: { session_id: "uuid", status: "string", purpose: "string", provider: "string", sandbox: "boolean", created_at: "timestamp" },
  optional: {
    provider_ref: "string",
    jurisdiction_country: "string",
    jurisdiction_region: "string",
    rules_version: "string",
    failure_reason: "string",
    expires_at: "timestamp",
  },
};

export const verificationCheckSpec: Spec = {
  required: { kind: "string", outcome: "string", provider: "string", rules_version: "string", sandbox: "boolean", recorded_at: "timestamp" },
  optional: { provider_ref: "string", detail: "string" },
};

export const verificationRequirementSpec: Spec = {
  required: { code: "string", detail: "string", action: "string" },
};

export const verificationProfileSpec: Spec = {
  required: {
    account_id: "uuid",
    state: "string",
    level: "string",
    jurisdiction_supported: "boolean",
    minimum_age: "integer",
    age_verified: "boolean",
    sanctions_state: "string",
    sandbox: "boolean",
    rules_version: "string",
    payout_ready: "boolean",
  },
  optional: {
    jurisdiction_country: "string",
    jurisdiction_region: "string",
    verified_at: "timestamp",
    expires_at: "timestamp",
    session: "object",
    provider: "string",
    provider_availability: "string",
  },
  arrays: {
    checks: { spec: verificationCheckSpec },
    missing: { spec: verificationRequirementSpec },
  },
};

/** The profile, with its open session and its string lists checked too. */
export function validatedVerificationProfile<T>(raw: unknown, path: string): T {
  const value = validated<T>(raw, verificationProfileSpec, path);
  const record = raw as Record<string, unknown>;
  if (record["session"] !== undefined && record["session"] !== null) {
    validated<unknown>(record["session"], verificationSessionSpec, `${path}.session`);
  }
  for (const field of ["jurisdiction_refusals", "restrictions"]) {
    const list = record[field];
    if (list !== undefined && list !== null) validatedStrings(list, `${path}.${field}`);
  }
  return value;
}

export const startedVerificationSpec: Spec = {
  required: { session: "object", sandbox: "boolean" },
  optional: { hosted_url: "string", resume: "string", expires_at: "timestamp", sandbox_control_path: "string" },
};

export function validatedStartedVerification<T>(raw: unknown, path: string): T {
  const value = validated<T>(raw, startedVerificationSpec, path);
  validated<unknown>((raw as Record<string, unknown>)["session"], verificationSessionSpec, `${path}.session`);
  return value;
}

export const withdrawalOriginBucketSpec: Spec = {
  required: {
    origin: "string",
    quantity: "quantity",
    withdrawable: "quantity",
    payout_allowed: "boolean",
    consumption_rank: "integer",
  },
  optional: {
    required_verification: "string",
    required_capability: "string",
    min_hold_days: "integer",
    verification_would_suffice: "boolean",
  },
};

export const withdrawalEligibilitySpec: Spec = {
  required: {
    account_id: "uuid",
    eligible: "boolean",
    withdrawable_now: "quantity",
    gross: "quantity",
    spendable: "quantity",
    frozen: "quantity",
    payout_eligible: "quantity",
    ineligible: "quantity",
    policy_version: "string",
    current_verification: "string",
    required_verification: "string",
    sandbox: "boolean",
  },
  optional: {
    verification_would_suffice: "boolean",
    minimum_quantity: "quantity",
    provider: "string",
    provider_available: "boolean",
    destination_configured: "boolean",
    jurisdiction_supported: "boolean",
    policy_hash: "string",
  },
  arrays: {
    buckets: { required: true, spec: withdrawalOriginBucketSpec },
  },
};

/** Eligibility, with the reason lists on the answer and on every bucket. */
export function validatedEligibility<T>(raw: unknown, path: string): T {
  const value = validated<T>(raw, withdrawalEligibilitySpec, path);
  const record = raw as Record<string, unknown>;
  validatedStrings(record["reasons"], `${path}.reasons`);
  const buckets = record["buckets"];
  if (Array.isArray(buckets)) {
    buckets.forEach((bucket, index) => {
      const reasons = (bucket as Record<string, unknown>)["reasons"];
      validatedStrings(reasons, `${path}.buckets[${String(index)}].reasons`);
    });
  }
  return value;
}

export const payoutDestinationSpec: Spec = {
  required: {
    destination_id: "uuid",
    account_id: "uuid",
    kind: "string",
    provider: "string",
    status: "string",
    sandbox: "boolean",
    created_at: "timestamp",
  },
  optional: {
    display_label: "string",
    masked_display: "string",
    currency: "string",
    country: "string",
    region: "string",
    usable: "boolean",
    verified_at: "timestamp",
  },
};

export const payoutProvenanceSliceSpec: Spec = {
  required: { origin: "string", quantity: "quantity", consumption_rank: "integer" },
  optional: { returned: "boolean" },
};

/**
 * A payout quote.
 *
 * The minor-unit fields are `integer` because that is what they are: whole
 * counts of the smallest unit of the provider's currency. The Credit side of
 * the same quote is `quantity` — an exact integer string of base units — and
 * there is deliberately no field between them holding a rate, because a rate is
 * where a float would enter.
 */
export const payoutQuoteSpec: Spec = {
  required: {
    quote_id: "uuid",
    account_id: "uuid",
    destination_id: "uuid",
    provider: "string",
    gross_quantity: "quantity",
    fee_quantity: "quantity",
    net_quantity: "quantity",
    currency: "string",
    gross_amount_minor: "integer",
    fee_amount_minor: "integer",
    net_amount_minor: "integer",
    minimum_ok: "boolean",
    expires_at: "timestamp",
    sandbox: "boolean",
  },
  optional: {
    minimum_amount_minor: "integer",
    pricing_version: "string",
    fee_model_version: "string",
    policy_version: "string",
    consumed_at: "timestamp",
    created_at: "timestamp",
  },
  arrays: {
    provenance: { spec: payoutProvenanceSliceSpec },
  },
};

/**
 * A payout request, including the provenance slices the merged contract added.
 *
 * `payoutRequestSpec` above predates them and is left alone: it is what the
 * list endpoint's page spec is built from, and widening it would make a
 * response without provenance a contract violation when the API says the field
 * is optional. This walks the extra fields where they matter.
 */
export function validatedPayout<T>(raw: unknown, path: string): T {
  const value = validated<T>(raw, payoutRequestSpec, path);
  const record = raw as Record<string, unknown>;
  const slices = record["provenance"];
  if (slices !== undefined && slices !== null) {
    if (!Array.isArray(slices)) throw new ContractViolation(`${path}.provenance`, "expected an array");
    slices.forEach((slice, index) => {
      validated<unknown>(slice, payoutProvenanceSliceSpec, `${path}.provenance[${String(index)}]`);
    });
  }
  const reasons = record["eligibility_reasons"];
  if (reasons !== undefined && reasons !== null) {
    validatedStrings(reasons, `${path}.eligibility_reasons`);
  }
  return value;
}

/* --------------------------------------------------------------------------
 * The rest of the markets surfaces: the page envelope, the trade screen, the
 * charts and the tape (product goal §12–§14, §47).
 *
 * `nativeMarketSummarySpec` above is the row these all carry. Every price in it
 * is `quantity` — an exact integer at the market's own `price_scale` — and
 * every amount is base units. None of them is `usd` and none of them is
 * `decimal`: PART LIV forbids an external value for a Credit, so a response
 * that tried to hand this app a currency figure for a native market would be
 * refused at this boundary rather than rendered.
 *
 * `price_scale` and `asset_decimals` are required on every one of these,
 * because they are how the digits are read. A response that carried a price
 * without saying what scale it is on is not a price, it is a number, and the
 * markets page would draw it a factor of ten to the twelve wrong (F-44).
 * ------------------------------------------------------------------------ */

export const nativeMarketPageSpec: Spec = {
  required: { sort: "string", stable: "boolean" },
  arrays: { markets: { required: true, spec: nativeMarketSummarySpec } },
};

/**
 * The limits in force on one market (goal §47).
 *
 * Only the policy version is required, and that is the contract's own answer
 * rather than a convenience: the compiled-in safety policy ships with the
 * circuit breaker DISARMED (D-065), and the risk fields are absent entirely
 * until a GLOBAL risk policy is recorded. Rendering an absent limit as a zero
 * would state the strictest possible limit where the truth is that there is
 * none, so every limit is optional here and the panel renders each absence as
 * an absence.
 *
 * Note what this does NOT make optional-by-accident: a DISARMED breaker arrives
 * as a literal `0`, not as an absent field, because the server distinguishes
 * "unset", which it refuses, from "zero", which is a decision. The panel reads
 * those as two different answers.
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

/**
 * The trade screen's read.
 *
 * `market` and `limits_in_force` are checked as objects here and validated
 * against their own specs where the response is decoded, which is the same
 * shape `portfolioSpec` uses for `credits` and `totals`.
 */
export const nativeMarketDetailSpec: Spec = {
  required: { market: "object", limits_in_force: "object" },
  arrays: {
    // No account id, on purpose: a holder list that names people is a list of
    // other people's positions (D-111).
    top_holders: {
      spec: {
        required: { rank: "integer", quantity: "quantity", share_bps: "integer" },
        optional: { is_you: "boolean" },
      },
    },
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
