/**
 * Delegated agent authority (ADR-0020; internal/delegation behind
 * /v1/advanced/delegation), following the pattern of `core.ts`:
 *
 *   - every response is checked against a spec before a component sees it:
 *     sandbox-dollar figures are exact two-decimal strings (`usd`), amounts are
 *     exact base units (`quantity`), statuses, classes, chains and verdicts
 *     are closed sets, and every response that reports value must say
 *     `value_domain: SIMULATED`;
 *   - account-scoped keys sit under `advancedKeys.account(accountId)`, so a
 *     command invalidates everything it may have changed for that account with
 *     one key; the operator's views sit under `delegationKeys.operator`;
 *   - every command carries an idempotency key created when the person
 *     confirmed, reused for every retry of that same attempt, and is never
 *     retried automatically;
 *   - a 4xx answer is the backend's decision and is not retried.
 *
 * Every route here is the account owner's or an operator's. An agent never
 * reaches the API: its swap requests and withdrawal proposals are admitted
 * inside the platform, and this app only shows what they became.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  ACTION_CLASSES,
  CAPABILITY_STANDINGS,
  CAPABILITY_STATUSES,
  CHAINS,
  DECISIONS,
  DELEGATION_MODES,
  DESTINATION_STATUSES,
  EVALUATION_OUTCOMES,
  INSTRUMENT_CLASSES,
  SUBJECT_KINDS,
  SWAP_STATUSES,
  VERDICTS,
  WITHDRAWAL_STATUSES,
  type CapabilityStatus,
  type ConfirmedAttempt,
  type Decision,
  type GrantBody,
  type PolicyBody,
  type SwapStatus,
  type WithdrawalStatus,
} from "../../lib/delegation.ts";
import { api } from "../client.ts";
import { cursorPageSpec, itemsSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";

export type DelegationAgent = Schemas["DelegationAgent"];
export type DelegationCapability = Schemas["DelegationCapability"];
export type DelegationCapabilityDetail = Schemas["DelegationCapabilityDetail"];
export type DelegationCapabilityUse = Schemas["DelegationCapabilityUse"];
export type DelegationDestination = Schemas["DelegationDestination"];
export type DelegationWithdrawalPolicy = Schemas["DelegationWithdrawalPolicy"];
export type DelegationWithdrawalPolicyState = Schemas["DelegationWithdrawalPolicyState"];
export type DelegationSwap = Schemas["DelegationSwap"];
export type DelegationWithdrawal = Schemas["DelegationWithdrawal"];
export type DelegationApproval = Schemas["DelegationApproval"];
export type DelegationInspection = Schemas["DelegationInspection"];
export type DelegationTransition = Schemas["DelegationTransition"];
export type DelegationExecution = Schemas["DelegationExecution"];
export type DelegationSwapDecisionResult = Schemas["DelegationSwapDecisionResult"];
export type DelegationWithdrawalDecisionResult = Schemas["DelegationWithdrawalDecisionResult"];
export type DelegationVenue = Schemas["DelegationVenue"];
export type DelegationVenueList = Schemas["DelegationVenueList"];
export type DelegationPending = Schemas["DelegationPending"];
export type DelegationExecutionRun = Schemas["DelegationExecutionRun"];

/** Rows per page of the capability, signing request and withdrawal lists. */
export const DELEGATION_PAGE_SIZE = 25;
/** The operator's pending view reads at most this many of each kind. */
export const PENDING_LIMIT = 100;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const delegationKeys = {
  agents: (accountId: string) => [...advancedKeys.account(accountId), "delegation", "agents"] as const,
  capabilities: (accountId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "delegation", "capabilities", cursor] as const,
  capability: (accountId: string, capabilityId: string) =>
    [...advancedKeys.account(accountId), "delegation", "capability", capabilityId] as const,
  destinations: (accountId: string) => [...advancedKeys.account(accountId), "delegation", "destinations"] as const,
  policy: (accountId: string) => [...advancedKeys.account(accountId), "delegation", "policy"] as const,
  swaps: (accountId: string, status: string, cursor: string) =>
    [...advancedKeys.account(accountId), "delegation", "swaps", status, cursor] as const,
  swap: (accountId: string, swapId: string) => [...advancedKeys.account(accountId), "delegation", "swap", swapId] as const,
  withdrawals: (accountId: string, status: string, cursor: string) =>
    [...advancedKeys.account(accountId), "delegation", "withdrawals", status, cursor] as const,
  withdrawal: (accountId: string, withdrawalId: string) =>
    [...advancedKeys.account(accountId), "delegation", "withdrawal", withdrawalId] as const,
  /** Prefix of every read that is not one account's. */
  operator: ["advanced", "delegation"] as const,
  venues: ["advanced", "delegation", "venues"] as const,
  opsCapabilities: (status: string, cursor: string) => ["advanced", "delegation", "ops-capabilities", status, cursor] as const,
  pending: ["advanced", "delegation", "pending"] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const MODE = oneOf(DELEGATION_MODES);

export const actorSpec: Spec = { required: { actor_type: "string", actor_id: "string" } };

export const agentSpec: Spec = {
  required: { id: "uuid", name: "string", stage: "string", state: "string", strategy_id: "uuid", created_at: "timestamp" },
};

export const agentListSpec: Spec = itemsSpec(agentSpec);

export const capabilityUsageSpec: Spec = {
  required: {
    day: "string",
    uses: "integer",
    used_today_usd: "usd",
    used_total_usd: "usd",
    remaining_today_usd: "usd",
    remaining_total_usd: "usd",
  },
};

export const capabilitySpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    agent_id: "uuid",
    strategy_id: "uuid",
    environment: "string",
    mode: MODE,
    max_notional_usd: "usd",
    max_daily_notional_usd: "usd",
    max_single_trade_usd: "usd",
    granted_at: "timestamp",
    expires_at: "timestamp",
    requires_human_approval: "boolean",
    status: oneOf(CAPABILITY_STATUSES),
    standing: oneOf(CAPABILITY_STANDINGS),
    content_hash: "string",
    as_of: "timestamp",
    value_domain: SIMULATED,
  },
  optional: { agent_name: "string", revocation_reason: "string", revoked_at: "timestamp" },
  lists: {
    instrument_classes: { required: true, kind: oneOf(INSTRUMENT_CLASSES) },
    action_classes: { required: true, kind: oneOf(ACTION_CLASSES) },
    destination_allowlist: { required: true, kind: "string" },
    chain_allowlist: { required: true, kind: oneOf(CHAINS) },
  },
  objects: {
    granted_by: { required: true, spec: actorSpec },
    revoked_by: { spec: actorSpec },
    usage: { required: true, spec: capabilityUsageSpec },
  },
};

export const capabilityUseSpec: Spec = {
  required: {
    id: "uuid",
    subject_kind: oneOf(SUBJECT_KINDS),
    subject_id: "uuid",
    action_class: oneOf(ACTION_CLASSES),
    notional_usd: "usd",
    used_at: "timestamp",
  },
};

export const capabilityDetailSpec: Spec = {
  ...capabilitySpec,
  arrays: { uses: { required: true, spec: capabilityUseSpec } },
};

export const capabilityPageSpec: Spec = { ...cursorPageSpec(capabilitySpec), required: { value_domain: SIMULATED } };

export const destinationSpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    chain: oneOf(CHAINS),
    address: "string",
    label: "string",
    status: oneOf(DESTINATION_STATUSES),
    added_at: "timestamp",
  },
  optional: { removed_at: "timestamp" },
  objects: { added_by: { required: true, spec: actorSpec }, removed_by: { spec: actorSpec } },
};

export const destinationListSpec: Spec = itemsSpec(destinationSpec);

export const policySpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    version: "integer",
    auto_approve_enabled: "boolean",
    auto_approve_max_usd: "usd",
    auto_approve_max_risk_score: "integer",
    max_risk_score: "integer",
    per_withdrawal_limit_usd: "usd",
    daily_limit_usd: "usd",
    velocity_max_count: "integer",
    min_destination_age_seconds: "integer",
    min_account_age_seconds: "integer",
    cooling_period_seconds: "integer",
    strong_auth_required: "boolean",
    strong_auth_max_age_seconds: "integer",
    policy_hash: "string",
    effective_at: "timestamp",
    created_at: "timestamp",
    value_domain: SIMULATED,
  },
  objects: { created_by: { required: true, spec: actorSpec } },
};

export const policyStateSpec: Spec = {
  required: { account_id: "uuid", value_domain: SIMULATED },
  objects: { current: { spec: policySpec } },
  arrays: { versions: { required: true, spec: policySpec } },
};

const assetRefSpec: Spec = { required: { id: "uuid", symbol: "string", decimals: "integer" } };

const planPriceSpec: Spec = {
  required: { mantissa: "string", scale: "string", source: "string", observed_at: "timestamp", par: "boolean" },
};

export const approvalSpec: Spec = {
  required: { decision: oneOf(DECISIONS), plan_hash: "string", decided_at: "timestamp", reason: "string" },
  optional: { authenticated_at: "timestamp" },
  objects: { approver: { required: true, spec: actorSpec } },
};

export const inspectionSpec: Spec = {
  required: {
    id: "uuid",
    verdict: oneOf(VERDICTS),
    signer_public_key: "string",
    capability_hash: "string",
    plan_hash: "string",
    message_sha256: "string",
    policy_version: "string",
    policy_hash: "string",
    inspector_version: "string",
    decided_at: "timestamp",
  },
  lists: { reason_codes: { required: true, kind: "string" } },
};

export const transitionSpec: Spec = {
  required: { from_status: "string", to_status: "string", reason: "string", occurred_at: "timestamp" },
  objects: { actor: { required: true, spec: actorSpec } },
};

export const swapPlanSpec: Spec = {
  required: {
    version: "string",
    swap_id: "string",
    capability_hash: "string",
    request_nonce: "string",
    action_class: "string",
    chain: "string",
    wallet: "string",
    venue_address: "string",
    pay_mint: "string",
    receive_mint: "string",
    amount_in: "quantity",
    amount_out: "quantity",
    min_amount_out: "quantity",
    spread_bps: "string",
    max_slippage_bps: "string",
    notional_usd: "usd",
    fee_per_tx: "quantity",
    requested_at: "timestamp",
    quote_expires_at: "timestamp",
  },
  objects: { pay_price: { required: true, spec: planPriceSpec }, receive_price: { required: true, spec: planPriceSpec } },
};

export const swapSpec: Spec = {
  required: {
    id: "uuid",
    capability_id: "uuid",
    account_id: "uuid",
    agent_id: "uuid",
    request_nonce: "string",
    intent_id: "uuid",
    reservation_id: "uuid",
    mode: MODE,
    action_class: oneOf(ACTION_CLASSES),
    chain: oneOf(CHAINS),
    wallet: "string",
    amount_in: "quantity",
    amount_out: "quantity",
    min_amount_out: "quantity",
    notional_usd: "usd",
    requested_at: "timestamp",
    quote_expires_at: "timestamp",
    requires_approval: "boolean",
    plan_hash: "string",
    status: oneOf(SWAP_STATUSES),
    created_at: "timestamp",
    updated_at: "timestamp",
    value_domain: SIMULATED,
  },
  optional: {
    signed_at: "timestamp",
    tx_id: "string",
    effect_id: "uuid",
    journal_transaction_id: "uuid",
    failure_code: "string",
  },
  objects: {
    venue: { required: true, spec: { required: { id: "uuid", name: "string", address: "string" } } },
    pay_asset: { required: true, spec: assetRefSpec },
    receive_asset: { required: true, spec: assetRefSpec },
    plan: { required: true, spec: swapPlanSpec },
    approval: { spec: approvalSpec },
    inspection: { spec: inspectionSpec },
  },
  arrays: { transitions: { required: true, spec: transitionSpec } },
};

export const swapPageSpec: Spec = { ...cursorPageSpec(swapSpec), required: { value_domain: SIMULATED } };

export const evaluationSpec: Spec = {
  required: {
    outcome: oneOf(EVALUATION_OUTCOMES),
    risk_score: "integer",
    destination_age_seconds: "integer",
    account_age_seconds: "integer",
    day_total_before_usd: "usd",
    count_last_24h: "integer",
    policy_version: "string",
    policy_hash: "string",
    environment: "string",
    compliance_clear: "boolean",
  },
  lists: { reason_codes: { required: true, kind: "string" }, anomalies: { required: true, kind: "string" } },
};

export const withdrawalPlanSpec: Spec = {
  required: {
    version: "string",
    withdrawal_id: "string",
    capability_hash: "string",
    proposal_nonce: "string",
    chain: "string",
    wallet: "string",
    destination_address: "string",
    mint: "string",
    amount: "quantity",
    notional_usd: "usd",
    policy_version: "string",
    policy_hash: "string",
    evaluation_outcome: "string",
    risk_score: "string",
    fee_per_tx: "quantity",
    proposed_at: "timestamp",
    executable_at: "timestamp",
    approval_expires_at: "timestamp",
  },
  objects: { price: { required: true, spec: planPriceSpec } },
};

export const withdrawalSpec: Spec = {
  required: {
    id: "uuid",
    capability_id: "uuid",
    account_id: "uuid",
    agent_id: "uuid",
    proposal_nonce: "string",
    intent_id: "uuid",
    reservation_id: "uuid",
    policy_id: "uuid",
    environment: "string",
    mode: MODE,
    chain: oneOf(CHAINS),
    wallet: "string",
    destination_address: "string",
    amount: "quantity",
    notional_usd: "usd",
    proposed_at: "timestamp",
    approval_expires_at: "timestamp",
    executable_at: "timestamp",
    plan_hash: "string",
    status: oneOf(WITHDRAWAL_STATUSES),
    created_at: "timestamp",
    updated_at: "timestamp",
    value_domain: SIMULATED,
  },
  optional: {
    signed_at: "timestamp",
    tx_id: "string",
    effect_id: "uuid",
    journal_transaction_id: "uuid",
    failure_code: "string",
  },
  objects: {
    destination: {
      required: true,
      spec: { required: { id: "uuid", label: "string", status: oneOf(DESTINATION_STATUSES) } },
    },
    asset: { required: true, spec: assetRefSpec },
    evaluation: { required: true, spec: evaluationSpec },
    plan: { required: true, spec: withdrawalPlanSpec },
    approval: { spec: approvalSpec },
    inspection: { spec: inspectionSpec },
  },
  arrays: { transitions: { required: true, spec: transitionSpec } },
};

export const withdrawalPageSpec: Spec = { ...cursorPageSpec(withdrawalSpec), required: { value_domain: SIMULATED } };

export const executionErrorSpec: Spec = { required: { code: "string", detail: "string" }, optional: { reason: "string" } };

export const executionSpec: Spec = {
  required: { attempted: "boolean" },
  objects: { error: { spec: executionErrorSpec } },
};

export const swapDecisionResultSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: { swap: { required: true, spec: swapSpec }, execution: { required: true, spec: executionSpec } },
};

export const withdrawalDecisionResultSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: { withdrawal: { required: true, spec: withdrawalSpec }, execution: { required: true, spec: executionSpec } },
};

export const venueSpec: Spec = {
  required: {
    id: "uuid",
    name: "string",
    chain: oneOf(CHAINS),
    address: "string",
    house_account_id: "uuid",
    spread_bps: "integer",
    co_signer: "boolean",
    created_at: "timestamp",
  },
  objects: { created_by: { required: true, spec: actorSpec } },
};

export const venueListSpec: Spec = {
  arrays: {
    items: { required: true, spec: venueSpec },
    co_signers: {
      required: true,
      spec: { required: { name: "string", chain: oneOf(CHAINS), address: "string", registered: "boolean" } },
    },
  },
};

export const pendingSpec: Spec = {
  required: { as_of: "timestamp", value_domain: SIMULATED },
  arrays: { swaps: { required: true, spec: swapSpec }, withdrawals: { required: true, spec: withdrawalSpec } },
};

export const executionRunSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: {
    items: {
      required: true,
      spec: {
        required: { kind: oneOf(SUBJECT_KINDS), id: "uuid", account_id: "uuid", status_before: "string", status: "string" },
        objects: { error: { spec: executionErrorSpec } },
      },
    },
  },
};

/* --------------------------------------------------------------------------
 * Reads of one account
 * ------------------------------------------------------------------------ */

export function useDelegationAgents(accountId: string | undefined): UseQueryResult<readonly DelegationAgent[]> {
  return useQuery({
    queryKey: delegationKeys.agents(accountId ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/agents", { params: { query: { account_id: accountId ?? "" } } });
      return validated<Schemas["DelegationAgentList"]>(data, agentListSpec, "/advanced/delegation/agents").items;
    },
  });
}

export function useDelegationCapabilities(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<DelegationCapability>> {
  return useQuery({
    queryKey: delegationKeys.capabilities(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/capabilities", {
        params: {
          query: { account_id: accountId ?? "", limit: DELEGATION_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
        },
      });
      const page = validated<Schemas["DelegationCapabilityPage"]>(data, capabilityPageSpec, "/advanced/delegation/capabilities");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One capability with its uses. The backend answers NOT_FOUND both for an id it
 * never recorded and for a capability this session may not read.
 */
export function useDelegationCapability(
  accountId: string | undefined,
  capabilityId: string | undefined,
): UseQueryResult<DelegationCapabilityDetail> {
  return useQuery({
    queryKey: delegationKeys.capability(accountId ?? "", capabilityId ?? ""),
    enabled: capabilityId !== undefined && capabilityId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/capabilities/{capabilityId}", {
        params: { path: { capabilityId: capabilityId ?? "" } },
      });
      return validated<DelegationCapabilityDetail>(data, capabilityDetailSpec, "/advanced/delegation/capabilities/{id}");
    },
  });
}

export function useDelegationDestinations(accountId: string | undefined): UseQueryResult<readonly DelegationDestination[]> {
  return useQuery({
    queryKey: delegationKeys.destinations(accountId ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/destinations", { params: { query: { account_id: accountId ?? "" } } });
      return validated<Schemas["DelegationDestinationList"]>(data, destinationListSpec, "/advanced/delegation/destinations").items;
    },
  });
}

export function useDelegationPolicy(accountId: string | undefined): UseQueryResult<DelegationWithdrawalPolicyState> {
  return useQuery({
    queryKey: delegationKeys.policy(accountId ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/withdrawal-policy", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validated<DelegationWithdrawalPolicyState>(data, policyStateSpec, "/advanced/delegation/withdrawal-policy");
    },
  });
}

export function useDelegationSwaps(
  accountId: string | undefined,
  status: SwapStatus | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<DelegationSwap>> {
  return useQuery({
    queryKey: delegationKeys.swaps(accountId ?? "", status ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/swaps", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: DELEGATION_PAGE_SIZE,
            ...(status === undefined ? {} : { status }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["DelegationSwapPage"]>(data, swapPageSpec, "/advanced/delegation/swaps");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useDelegationSwap(accountId: string | undefined, swapId: string | undefined): UseQueryResult<DelegationSwap> {
  return useQuery({
    queryKey: delegationKeys.swap(accountId ?? "", swapId ?? ""),
    enabled: swapId !== undefined && swapId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/swaps/{swapId}", { params: { path: { swapId: swapId ?? "" } } });
      return validated<DelegationSwap>(data, swapSpec, "/advanced/delegation/swaps/{id}");
    },
  });
}

export function useDelegationWithdrawals(
  accountId: string | undefined,
  status: WithdrawalStatus | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<DelegationWithdrawal>> {
  return useQuery({
    queryKey: delegationKeys.withdrawals(accountId ?? "", status ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/withdrawals", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: DELEGATION_PAGE_SIZE,
            ...(status === undefined ? {} : { status }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["DelegationWithdrawalPage"]>(data, withdrawalPageSpec, "/advanced/delegation/withdrawals");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useDelegationWithdrawal(
  accountId: string | undefined,
  withdrawalId: string | undefined,
): UseQueryResult<DelegationWithdrawal> {
  return useQuery({
    queryKey: delegationKeys.withdrawal(accountId ?? "", withdrawalId ?? ""),
    enabled: withdrawalId !== undefined && withdrawalId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/withdrawals/{withdrawalId}", {
        params: { path: { withdrawalId: withdrawalId ?? "" } },
      });
      return validated<DelegationWithdrawal>(data, withdrawalSpec, "/advanced/delegation/withdrawals/{id}");
    },
  });
}

/* --------------------------------------------------------------------------
 * Reads that are not one account's
 * ------------------------------------------------------------------------ */

export function useDelegationVenues(): UseQueryResult<DelegationVenueList> {
  return useQuery({
    queryKey: delegationKeys.venues,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/venues");
      return validated<DelegationVenueList>(data, venueListSpec, "/advanced/delegation/venues");
    },
  });
}

export function useOperatorCapabilities(
  status: CapabilityStatus | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<DelegationCapability>> {
  return useQuery({
    queryKey: delegationKeys.opsCapabilities(status ?? "", cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/ops/capabilities", {
        params: {
          query: {
            limit: DELEGATION_PAGE_SIZE,
            ...(status === undefined ? {} : { status }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["DelegationCapabilityPage"]>(data, capabilityPageSpec, "/advanced/delegation/ops/capabilities");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useDelegationPending(): UseQueryResult<DelegationPending> {
  return useQuery({
    queryKey: delegationKeys.pending,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/delegation/ops/pending", { params: { query: { limit: PENDING_LIMIT } } });
      return validated<DelegationPending>(data, pendingSpec, "/advanced/delegation/ops/pending");
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

/** A mutation of one confirmed attempt; the attempt carries its idempotency key. */
export type DelegationCommand<R, T> = UseMutationResult<T, unknown, ConfirmedAttempt<R>>;

/** Every command changes what the area reads -- the account's lists and the operator's views -- answered or refused. */
function useDelegationCommand<R, T>(run: (attempt: ConfirmedAttempt<R>) => Promise<T>): DelegationCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function useGrantCapability(): DelegationCommand<GrantBody, DelegationCapability> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/capabilities", { ...idempotent(idempotencyKey), body: request });
    return validated<DelegationCapability>(data, capabilitySpec, "/advanced/delegation/capabilities");
  });
}

export interface ReasonRequest {
  readonly id: string;
  readonly reason: string;
}

export function useRevokeCapability(): DelegationCommand<ReasonRequest, DelegationCapability> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/capabilities/{capabilityId}/revoke", {
      ...idempotent(idempotencyKey, { path: { capabilityId: request.id } }),
      body: { reason: request.reason },
    });
    return validated<DelegationCapability>(data, capabilitySpec, "/advanced/delegation/capabilities/{id}/revoke");
  });
}

export type DestinationRequest = Schemas["DelegationDestinationRequest"];

export function useAddDestination(): DelegationCommand<DestinationRequest, DelegationDestination> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/destinations", { ...idempotent(idempotencyKey), body: request });
    return validated<DelegationDestination>(data, destinationSpec, "/advanced/delegation/destinations");
  });
}

export function useRemoveDestination(): DelegationCommand<ReasonRequest, DelegationDestination> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/destinations/{destinationId}/remove", {
      ...idempotent(idempotencyKey, { path: { destinationId: request.id } }),
      body: { reason: request.reason },
    });
    return validated<DelegationDestination>(data, destinationSpec, "/advanced/delegation/destinations/{id}/remove");
  });
}

export function useSetWithdrawalPolicy(): DelegationCommand<PolicyBody, DelegationWithdrawalPolicy> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/withdrawal-policy", { ...idempotent(idempotencyKey), body: request });
    return validated<DelegationWithdrawalPolicy>(data, policySpec, "/advanced/delegation/withdrawal-policy");
  });
}

export interface DecisionRequest {
  readonly id: string;
  readonly decision: Decision;
  /** The plan hash the owner was shown; the backend refuses any other plan. */
  readonly planHash: string;
  readonly reason: string;
}

export function useDecideSwap(): DelegationCommand<DecisionRequest, DelegationSwapDecisionResult> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/swaps/{swapId}/decision", {
      ...idempotent(idempotencyKey, { path: { swapId: request.id } }),
      body: { decision: request.decision, plan_hash: request.planHash, reason: request.reason },
    });
    return validated<DelegationSwapDecisionResult>(data, swapDecisionResultSpec, "/advanced/delegation/swaps/{id}/decision");
  });
}

export function useDecideWithdrawal(): DelegationCommand<DecisionRequest, DelegationWithdrawalDecisionResult> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/withdrawals/{withdrawalId}/decision", {
      ...idempotent(idempotencyKey, { path: { withdrawalId: request.id } }),
      body: { decision: request.decision, plan_hash: request.planHash, reason: request.reason },
    });
    return validated<DelegationWithdrawalDecisionResult>(
      data,
      withdrawalDecisionResultSpec,
      "/advanced/delegation/withdrawals/{id}/decision",
    );
  });
}

export function useCancelWithdrawal(): DelegationCommand<ReasonRequest, DelegationWithdrawal> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/withdrawals/{withdrawalId}/cancel", {
      ...idempotent(idempotencyKey, { path: { withdrawalId: request.id } }),
      body: { reason: request.reason },
    });
    return validated<DelegationWithdrawal>(data, withdrawalSpec, "/advanced/delegation/withdrawals/{id}/cancel");
  });
}

export type VenueRequest = Schemas["DelegationVenueRequest"];

export function useRegisterVenue(): DelegationCommand<VenueRequest, DelegationVenue> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/ops/venues", { ...idempotent(idempotencyKey), body: request });
    return validated<DelegationVenue>(data, venueSpec, "/advanced/delegation/ops/venues");
  });
}

export interface ExecuteRequest {
  readonly limit: number;
}

export function useExecutePending(): DelegationCommand<ExecuteRequest, DelegationExecutionRun> {
  return useDelegationCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/delegation/ops/execute-pending", {
      ...idempotent(idempotencyKey),
      body: { limit: request.limit },
    });
    return validated<DelegationExecutionRun>(data, executionRunSpec, "/advanced/delegation/ops/execute-pending");
  });
}
