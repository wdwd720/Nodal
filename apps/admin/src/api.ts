/**
 * The console's single HTTP surface.
 *
 * It wraps `@controlplane/generated-client`, which is generated from
 * `openapi/openapi.yaml` and already handles problem+json, request-id
 * correlation and idempotency keys. There is no second HTTP client in this
 * app and no bare `fetch` to the API: every call goes through here, so every
 * refusal arrives as a typed `ApiProblem` with the server's own code.
 *
 * The console is served same-origin with the API (see server.mjs), so the base
 * URL is a path, cookies ride along by themselves and the Fetch Metadata CSRF
 * guard sees a first-party request.
 */
import { ApiProblem, createApiClient, idempotent, newIdempotencyKey } from "@controlplane/generated-client";
import type { Schemas } from "@controlplane/generated-client";

export { ApiProblem, newIdempotencyKey };

export type Account = Schemas["Account"];
export type AccountPage = Schemas["AccountPage"];
export type Capability = Schemas["Capability"];
export type AdminAction = Schemas["AdminAction"];
export type AdminActionPage = Schemas["AdminActionPage"];
export type CapabilityGate = Schemas["CapabilityGate"];
export type KillSwitch = Schemas["KillSwitch"];
export type ProviderStatus = Schemas["ProviderStatus"];
export type ReconciliationRecord = Schemas["ReconciliationRecord"];
export type ReconciliationRecordPage = Schemas["ReconciliationRecordPage"];
export type ActivityItem = Schemas["ActivityItem"];
export type ActivityPage = Schemas["ActivityPage"];
export type Principal = Schemas["Principal"];
export type CreditBalance = Schemas["CreditBalance"];

const client = createApiClient({ baseUrl: "/v1" });

/**
 * The middleware in the generated client throws `ApiProblem` for every
 * non-2xx response, so a returned envelope without `data` means the server
 * answered 2xx with nothing. That is a contract violation rather than an
 * operator-visible condition, and it must not be rendered as an empty list.
 */
function must<T>(result: { data?: T }, what: string): T {
  if (result.data === undefined) {
    throw new Error(`the API returned no body for ${what}`);
  }
  return result.data;
}

export interface PageParams {
  cursor?: string;
  limit?: number;
}

function pageQuery(params: PageParams): Record<string, unknown> {
  const query: Record<string, unknown> = {};
  if (params.cursor) query["cursor"] = params.cursor;
  if (params.limit !== undefined) query["limit"] = params.limit;
  return query;
}

// --- identity ---------------------------------------------------------------

export async function getMe(): Promise<Principal> {
  return must(await client.GET("/me"), "the current principal");
}

// --- accounts ---------------------------------------------------------------

export async function searchAccounts(q: string, params: PageParams = {}): Promise<AccountPage> {
  const query = pageQuery(params);
  if (q) query["q"] = q;
  return must(await client.GET("/admin/accounts", { params: { query } }), "the account search");
}

export async function getAccount(accountId: string): Promise<Account> {
  return must(
    await client.GET("/accounts/{accountId}", { params: { path: { accountId } } }),
    "the account",
  );
}

export async function getAccountActivity(accountId: string, params: PageParams = {}): Promise<ActivityPage> {
  return must(
    await client.GET("/accounts/{accountId}/activity", {
      params: { path: { accountId }, query: pageQuery(params) },
    }),
    "the account activity",
  );
}

export async function changeAccountStatus(
  accountId: string,
  body: { to: Account["status"]; reason: string },
  key: string,
): Promise<Account> {
  return must(
    await client.POST("/admin/accounts/{accountId}/status", {
      ...idempotent(key, { path: { accountId } }),
      body,
    }),
    "the account status change",
  );
}

/**
 * One account's Credit balance, broken into the buckets PART XX requires.
 *
 * There is no `/v1/admin/credits/...` route. This is the customer route, and an
 * operator reaches it by the same rule every other read on this console uses:
 * `security.RequireAccount` admits the account's owner *or* a principal holding
 * `account:read_any`, which is exactly the permission that already gates this
 * surface (`internal/security/authz.go`). So this is not a customer endpoint
 * being borrowed — it is the read path an operator is authorised for, and the
 * write path (`RequireAccountOwner`) has no operator override at all, which is
 * why no balance on this console is editable.
 *
 * It answers 422 UNSUPPORTED where the Nodal-native economy is not wired. That
 * is a state of the deployment and is rendered as one, never as a zero.
 */
export async function getCreditBalance(accountId: string): Promise<CreditBalance> {
  return must(
    await client.GET("/credits/balance", { params: { query: { account_id: accountId } } }),
    "the Credit balance",
  );
}

// --- controlled administrative actions --------------------------------------

export async function listActions(status: string, params: PageParams = {}): Promise<AdminActionPage> {
  const query = pageQuery(params);
  if (status) query["status"] = status;
  return must(await client.GET("/admin/actions", { params: { query } }), "the action queue");
}

export async function proposeAction(
  body: {
    kind: string;
    target_type: string;
    target_id: string;
    reason: string;
    params?: Record<string, unknown>;
  },
  key: string,
): Promise<AdminAction> {
  return must(await client.POST("/admin/actions", { ...idempotent(key), body }), "the proposal");
}

/**
 * The three decisions that are routable path segments on
 * `POST /v1/admin/actions/{actionId}/{decision}`.
 *
 * `propose` is not one of them (it is the collection POST) and neither is
 * `cancel` (`admin.Cancel` exists in the domain but has no HTTP route), so the
 * type is deliberately narrower than the console's own verb list. Anything the
 * console can offer but not send has to be refused in the view rather than
 * assembled into a URL that does not exist.
 */
export type DecisionVerb = "approve" | "reject" | "execute";

export async function decideAction(
  actionId: string,
  decision: DecisionVerb,
  note: string,
  key: string,
): Promise<AdminAction> {
  return must(
    await client.POST("/admin/actions/{actionId}/{decision}", {
      ...idempotent(key, { path: { actionId, decision } }),
      body: { note },
    }),
    "the decision",
  );
}

// --- capability gates -------------------------------------------------------

export async function listGates(): Promise<CapabilityGate[]> {
  return must(await client.GET("/admin/gates"), "the capability gates");
}

/**
 * Every step of the gate path, including the two that exist only on a sandbox
 * tier. `sandbox` and `unsandbox` are ordinary path segments on the same
 * route; what makes them sandbox-only is the server, which answers 403
 * FORBIDDEN with its own reason anywhere else (internal/gates.Admin.sandboxOp).
 * The console offers them and renders that refusal — it does not decide on the
 * server's behalf which deployments are sandbox tiers, because it has no
 * endpoint that would tell it.
 */
export type GateActionName =
  | "propose"
  | "approve"
  | "activate"
  | "suspend"
  | "resume"
  | "revoke"
  | "sandbox"
  | "unsandbox";

/** The two steps ADR-0023 adds, which exist only on a sandbox tier. */
export const SANDBOX_GATE_ACTIONS: readonly GateActionName[] = ["sandbox", "unsandbox"];

/**
 * The capability names the *OpenAPI contract* lists on the gate path.
 *
 * Typed as `Record<Capability, true>`, so the compiler requires an entry for
 * every member of the generated enum: adding a capability to
 * `openapi/openapi.yaml` breaks this file rather than leaving the console
 * quietly out of step.
 *
 * The contract is not the authority here, and this predicate is no longer a
 * gate on what the console will address. `internal/gates.Capability.Valid` is
 * what the server actually checks, `internal/adminplane` exports that same list
 * into the authority document, and the OpenAPI enum is a hand-written
 * restatement that has fallen behind it: the contract lists ten capabilities
 * while Go declares twenty, and the six a sandbox tier activates
 * (CREDIT_PURCHASE, NATIVE_ASSET_CREATION, NATIVE_MARKET_TRADING, MARKETPLACE,
 * PAYOUT_RESERVE, PAYOUT_SETTLE) include five the enum has never heard of.
 *
 * Nothing validates the enum on the wire — the generated server binds the path
 * segment as a plain string and hands it to `Capability.Valid` — so refusing to
 * address a gate the authority document declares would leave an operator unable
 * to suspend or revoke a live capability for no reason but a stale document.
 * The console therefore addresses every declared capability and *reports* the
 * drift where it is visible (`views/gates.ts`), rather than acting on it.
 */
const CAPABILITY_SET: Readonly<Record<Capability, true>> = {
  LIVE_FUNDING: true,
  LIVE_MANUAL_TRADING: true,
  LIVE_AGENT_TRADING: true,
  WITHDRAWALS: true,
  SOCIAL_DATA_PERSISTENCE: true,
  MARKETPLACE: true,
  CROSS_CHAIN: true,
  PREDICTION_MARKETS: true,
  SECURITIES: true,
  CEX_TRADING: true,
};

/** True when the OpenAPI contract's Capability enum also lists this name. */
export function inApiContract(name: string): name is Capability {
  return Object.hasOwn(CAPABILITY_SET, name);
}

/** The capability names the OpenAPI contract knows, for a drift report. */
export function contractCapabilities(): readonly string[] {
  return Object.keys(CAPABILITY_SET);
}

export async function actOnGate(
  capability: string,
  action: GateActionName,
  body: {
    reason: string;
    legal_review_ref?: string;
    provider_contract_ref?: string;
    risk_approval_ref?: string;
    security_approval_ref?: string;
    note?: string;
  },
  key: string,
): Promise<CapabilityGate> {
  return must(
    await client.POST("/admin/gates/{capability}/{action}", {
      // The path parameter is typed from the OpenAPI enum, which is narrower
      // than the capability list the server validates against (see
      // CAPABILITY_SET). The cast is the drift, made visible in one place
      // instead of silently narrowing the console's reach; remove it when the
      // contract's enum is regenerated from internal/gates.
      ...idempotent(key, { path: { capability: capability as Capability, action } }),
      body,
    }),
    "the gate transition",
  );
}

// --- kill switches ----------------------------------------------------------

export async function listKillSwitches(): Promise<KillSwitch[]> {
  return must(await client.GET("/admin/kill-switches"), "the kill switches");
}

/**
 * `scope_id` is required by the contract (it defaults to `*`, the whole
 * platform, only in the schema's documentation — the field itself must be
 * sent). Requiring it here keeps the caller from omitting a scope and
 * accidentally arming a switch wider than it meant to.
 */
export async function actOnKillSwitch(
  body: {
    kind: string;
    scope_id: string;
    action: "activate" | "release";
    reason: string;
    approval_id?: string;
  },
  key: string,
): Promise<KillSwitch> {
  return must(await client.POST("/admin/kill-switches", { ...idempotent(key), body }), "the kill switch");
}

// --- reconciliation ---------------------------------------------------------

export async function listReconciliationRecords(
  status: string,
  accountId: string,
  params: PageParams = {},
): Promise<ReconciliationRecordPage> {
  const query = pageQuery(params);
  if (status) query["status"] = status;
  if (accountId) query["account_id"] = accountId;
  return must(
    await client.GET("/admin/reconciliation/records", { params: { query } }),
    "the reconciliation records",
  );
}

export async function resolveReconciliationRecord(
  recordId: string,
  body: { reason: string; evidence_ref: string; approval_id?: string },
  key: string,
): Promise<ReconciliationRecord> {
  return must(
    await client.POST("/admin/reconciliation/records/{recordId}/resolve", {
      ...idempotent(key, { path: { recordId } }),
      body,
    }),
    "the resolution",
  );
}

// --- providers --------------------------------------------------------------

export async function listProviders(): Promise<ProviderStatus[]> {
  return must(await client.GET("/admin/providers"), "the provider status");
}

/**
 * Classifies a failure as "this deployment has not wired that capability"
 * rather than "you may not". The API answers 422 UNSUPPORTED for an unwired
 * port (for example `cmd/api` starts with no reconciliation engine and no
 * admin executors) and 503 PROVIDER_UNAVAILABLE for an upstream that is down.
 * Both are truthful states the console shows as such — never as an empty list.
 */
export function isUnwired(err: unknown): err is ApiProblem {
  return err instanceof ApiProblem && (err.code === "UNSUPPORTED" || err.code === "PROVIDER_UNAVAILABLE");
}
