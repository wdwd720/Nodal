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

/** Validates every element of a top-level array response. */
export function validatedList<T>(value: unknown, spec: Spec, path: string): T[] {
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
