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
export type AdminUserView = Schemas["AdminUserView"];
export type ClosureRequest = Schemas["ClosureRequest"];
export type ClosureDecisionName = Schemas["ClosureDecision"]["decision"];
export type AccountRestriction = Schemas["AccountRestriction"];
export type TermsAcceptance = Schemas["TermsAcceptance"];
export type Agent = Schemas["Agent"];
export type AgentPage = Schemas["AgentPage"];
export type CapabilityGateTransition = Schemas["CapabilityGateTransition"];

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

// --- the person behind an account -------------------------------------------

/**
 * The operator support view of one user (§38).
 *
 * Read-only by construction: `AdminUserView` has no field an operator can
 * write, it carries no e-mail address, legal name or date of birth (those stay
 * sealed in `identity_pii`, which this surface has no route to), and it points
 * at the audit stream rather than restating it. `account:read_any` is the
 * permission, the same one that lets the accounts surface exist.
 */
export async function getAdminUser(userId: string): Promise<AdminUserView> {
  return must(
    await client.GET("/admin/users/{userId}", { params: { path: { userId } } }),
    "the user support view",
  );
}

/**
 * Decides a closure request **the user themselves opened**. It is the only
 * mutation the support surface offers, and the console never originates one:
 * there is no operator route that closes an account nobody asked to close.
 *
 * `account:freeze` and a step-up, which is what the account status change
 * takes, because that is what EFFECT ends up performing. EFFECT is refused
 * before the cooling-off period has passed by the service and again by the
 * database, and an operator may not decide their own request.
 */
export async function decideClosure(
  userId: string,
  body: { decision: ClosureDecisionName; reason: string },
  key: string,
): Promise<AdminUserView> {
  return must(
    await client.POST("/admin/users/{userId}/closure", {
      ...idempotent(key, { path: { userId } }),
      body,
    }),
    "the closure decision",
  );
}

// --- agents ------------------------------------------------------------------

/** Every agent, or one account's, with the authority ladder this build permits. */
export async function listAgents(
  params: { accountId?: string; includeArchived?: boolean; limit?: number } = {},
): Promise<AgentPage> {
  const query: Record<string, unknown> = {};
  if (params.accountId) query["account_id"] = params.accountId;
  if (params.includeArchived !== undefined) query["include_archived"] = params.includeArchived;
  if (params.limit !== undefined) query["limit"] = params.limit;
  return must(await client.GET("/admin/agents", { params: { query } }), "the agents");
}

/**
 * Operator pause of a customer's agent.
 *
 * It writes the same `agent_pauses` row an owner pause does, under the OPERATOR
 * reason code, so the owner's own history shows plainly that somebody else
 * stopped it. Open orders are left alone: stopping an agent is stopping new
 * risk, not unwinding what it already did.
 *
 * The route floor is `kill:activate` with a step-up, and `internal/agents` then
 * demands `agent:pause` and an OPERATOR actor — both halves must hold, which is
 * why the console checks both.
 */
export async function pauseAgent(agentId: string, reason: string, key: string): Promise<Agent> {
  return must(
    await client.POST("/admin/agents/{agentId}/pause", {
      ...idempotent(key, { path: { agentId } }),
      body: { reason },
    }),
    "the agent pause",
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
 * Every recorded transition of one gate, oldest first.
 *
 * These are the `capability_gate_transitions` rows, written by
 * `cp_gate_transition` and `cp_gate_sandbox` in the same statement as the state
 * change they record — so the history cannot disagree with the row, and a
 * transition cannot exist without one. It reads under `gate:read`, the same
 * permission the gates surface itself takes, so anyone who can see a gate can
 * see who moved it.
 */
export async function listGateTransitions(capability: Capability): Promise<CapabilityGateTransition[]> {
  return must(
    await client.GET("/admin/gates/{capability}/history", { params: { path: { capability } } }),
    "the gate's transition history",
  );
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
 * The capability names the contract accepts on the gate path.
 *
 * Typed as `Record<Capability, true>`, so the compiler requires an entry for
 * every member of the generated enum: a capability added to
 * `openapi/openapi.yaml` breaks this file rather than producing a console that
 * silently omits a live-money gate.
 *
 * The console's own capability list comes from the generated authority
 * document, which `internal/adminplane` exports from `internal/gates`, so the
 * two are separately generated from the same Go source and can in principle
 * disagree. They did: the contract's enum was a hand-written restatement and
 * listed ten of the twenty Go declares, including five of the six a sandbox
 * tier activates, and D-078 records what the console did about it. The enum has
 * since been regenerated and the two now agree; `isCapability` is how any
 * future disagreement surfaces — as a failing test in `scan.test.ts`, which
 * holds the two lists equal, and as a visible refusal rather than a 400 on
 * submit.
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
  CREDIT_PURCHASE: true,
  NATIVE_ASSET_CREATION: true,
  NATIVE_MARKET_TRADING: true,
  PAYOUT_RESERVE: true,
  PAYOUT_SETTLE: true,
  HOSTED_TRADING: true,
  HOSTED_FUNDING: true,
  AGENT_BOUNDED_DISCRETION: true,
  AGENT_AUTONOMOUS_SELECTION: true,
  AGENT_AUTONOMOUS_PORTFOLIO: true,
};

/** True when the contract lists this name as an addressable capability. */
export function isCapability(name: string): name is Capability {
  return Object.hasOwn(CAPABILITY_SET, name);
}

/** The capability names the contract knows, for the parity test. */
export function contractCapabilities(): readonly string[] {
  return Object.keys(CAPABILITY_SET);
}

export async function actOnGate(
  capability: Capability,
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
      ...idempotent(key, { path: { capability, action } }),
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
