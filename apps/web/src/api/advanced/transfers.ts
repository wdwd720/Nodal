/**
 * Customer-to-customer transfers of simulated value and the sandbox faucet
 * (ADR-0020), following the pattern of `core.ts`:
 *
 *   - every response is checked against a spec before a component sees it:
 *     amounts are exact base units (`quantity`), the valuation is an exact
 *     USD string (`usd`), statuses and modes are closed sets, and every
 *     response that reports value must say `value_domain: SIMULATED`;
 *   - account-scoped keys sit under `advancedKeys.account(accountId)`, so a
 *     command invalidates everything it may have changed for that account with
 *     one key;
 *   - every command carries an idempotency key the page created when the
 *     person confirmed, reused for every retry of that same attempt, and is
 *     never retried automatically;
 *   - a 4xx answer is the backend's decision and is not retried.
 *
 * A send the backend refuses after accepting it is still a write: the
 * REJECTED transfer is recorded. So a send invalidates the account's lists
 * whether it succeeded or not.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import { TRANSFER_MODES, TRANSFER_STATUSES } from "../../lib/transfers.ts";
import { api } from "../client.ts";
import { cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";

export type AdvancedTransfer = Schemas["AdvancedTransfer"];
export type AdvancedTransferHandle = Schemas["AdvancedTransferHandle"];
export type AdvancedFaucetGrant = Schemas["AdvancedFaucetGrant"];
export type AdvancedFaucetAllowance = Schemas["AdvancedFaucetAllowance"];
export type TransferMode = (typeof TRANSFER_MODES)[number];

/** Rows per page of the transfer history. */
export const TRANSFER_PAGE_SIZE = 25;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const transferKeys = {
  list: (accountId: string, cursor: string, limit: number) =>
    [...advancedKeys.account(accountId), "transfers", cursor, limit] as const,
  one: (accountId: string, transferId: string) => [...advancedKeys.account(accountId), "transfer", transferId] as const,
  allowance: (accountId: string) => [...advancedKeys.account(accountId), "faucet-allowance"] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);

export const transferSpec: Spec = {
  required: {
    id: "uuid",
    intent_id: "uuid",
    status: oneOf(TRANSFER_STATUSES),
    sender_account_id: "uuid",
    asset_id: "uuid",
    asset_symbol: "string",
    asset_decimals: "integer",
    quantity: "quantity",
    notional_usd: "usd",
    // Not every AdvancedMode: a transfer exists only in the simulated modes
    // its capability implements, so one that says otherwise is refused.
    mode: oneOf(TRANSFER_MODES),
    policy_version: "string",
    policy_hash: "string",
    created_at: "timestamp",
    updated_at: "timestamp",
    value_domain: SIMULATED,
  },
  optional: {
    recipient_account_id: "uuid",
    recipient_handle: "string",
    memo: "string",
    rejection_code: "string",
    risk_decision_id: "uuid",
    effect_id: "uuid",
    journal_transaction_id: "uuid",
    evidence_ref: "string",
    evidence_sha256: "string",
    decided_at: "timestamp",
    reversal_journal_transaction_id: "uuid",
    reversal_reason_code: "string",
    reversed_at: "timestamp",
  },
  lists: { reason_codes: { required: true, kind: "string" } },
};

/** A page of transfers, which states the value domain of the figures it carries. */
export const transferPageSpec: Spec = {
  ...cursorPageSpec(transferSpec),
  required: { value_domain: SIMULATED },
};

export const handleSpec: Spec = {
  required: { id: "uuid", account_id: "uuid", handle: "string", created_at: "timestamp" },
};

export const faucetGrantSpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    asset_id: "uuid",
    asset_symbol: "string",
    asset_decimals: "integer",
    quantity: "quantity",
    policy_version: "string",
    policy_hash: "string",
    environment: "string",
    effect_id: "uuid",
    journal_transaction_id: "uuid",
    granted_at: "timestamp",
    value_domain: SIMULATED,
  },
  optional: { evidence_ref: "string", evidence_sha256: "string" },
};

export const faucetAllowanceSpec: Spec = {
  required: {
    account_id: "uuid",
    asset_id: "uuid",
    asset_symbol: "string",
    asset_decimals: "integer",
    cap: "quantity",
    used: "quantity",
    remaining: "quantity",
    window_ms: "integer",
    policy_version: "string",
    policy_hash: "string",
    environment: "string",
    value_domain: SIMULATED,
  },
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

/** The transfers an account sent or received, newest first, exactly as the backend pages them. */
export function useTransfers(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<AdvancedTransfer>> {
  return useQuery({
    queryKey: transferKeys.list(accountId ?? "", cursor ?? "", TRANSFER_PAGE_SIZE),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/transfers", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: TRANSFER_PAGE_SIZE,
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["AdvancedTransferPage"]>(data, transferPageSpec, "/advanced/transfers");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One transfer. The backend decides from the session whether it may be read,
 * and answers NOT_FOUND -- the answer an unknown id gets -- when it may not.
 */
export function useTransfer(
  accountId: string | undefined,
  transferId: string | undefined,
): UseQueryResult<AdvancedTransfer> {
  return useQuery({
    queryKey: transferKeys.one(accountId ?? "", transferId ?? ""),
    enabled: transferId !== undefined && transferId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/transfers/{transferId}", {
        params: { path: { transferId: transferId ?? "" } },
      });
      return validated<AdvancedTransfer>(data, transferSpec, "/advanced/transfers/{id}");
    },
  });
}

/** What the sandbox faucet would still grant the account in the current window. */
export function useFaucetAllowance(accountId: string | undefined): UseQueryResult<AdvancedFaucetAllowance> {
  return useQuery({
    queryKey: transferKeys.allowance(accountId ?? ""),
    enabled: accountId !== undefined,
    // Computed by the backend on every call: a figure is refetched, not reused.
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/transfers/faucet/allowance", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validated<AdvancedFaucetAllowance>(data, faucetAllowanceSpec, "/advanced/transfers/faucet/allowance");
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

/** A send, exactly as the person confirmed it. The recipient is named by exactly one of handle or account id. */
export interface SendTransferInput {
  /** The sender: always an account the session owns. */
  readonly accountId: string;
  readonly recipientHandle?: string;
  readonly recipientAccountId?: string;
  /** A plain decimal in whole units of the sandbox dollar, as typed. */
  readonly amount: string;
  readonly mode: TransferMode;
  readonly memo?: string;
  /** Created once when the person confirmed, reused for every retry of this attempt. */
  readonly idempotencyKey: string;
}

export function useSendTransfer(): UseMutationResult<AdvancedTransfer, unknown, SendTransferInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SendTransferInput) => {
      const { data } = await api.POST("/advanced/transfers", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          amount: input.amount,
          mode: input.mode,
          ...(input.recipientHandle === undefined ? {} : { recipient_handle: input.recipientHandle }),
          ...(input.recipientAccountId === undefined ? {} : { recipient_account_id: input.recipientAccountId }),
          ...(input.memo === undefined ? {} : { memo: input.memo }),
        },
      });
      return validated<AdvancedTransfer>(data, transferSpec, "/advanced/transfers");
    },
    // A refusal recorded a REJECTED transfer, so the lists change either way.
    onSettled: (_transfer, _error, input) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(input.accountId) });
    },
  });
}

export interface FaucetGrantInput {
  /** Always an account the session owns. */
  readonly accountId: string;
  /** A plain decimal in whole sandbox dollars, as typed. */
  readonly amount: string;
  readonly idempotencyKey: string;
}

export function useRequestFaucetGrant(): UseMutationResult<AdvancedFaucetGrant, unknown, FaucetGrantInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: FaucetGrantInput) => {
      const { data } = await api.POST("/advanced/transfers/faucet/grants", {
        ...idempotent(input.idempotencyKey),
        body: { account_id: input.accountId, amount: input.amount },
      });
      return validated<AdvancedFaucetGrant>(data, faucetGrantSpec, "/advanced/transfers/faucet/grants");
    },
    onSettled: (_grant, _error, input) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(input.accountId) });
    },
  });
}

export interface RegisterHandleInput {
  readonly accountId: string;
  readonly handle: string;
  readonly idempotencyKey: string;
}

export function useRegisterHandle(): UseMutationResult<AdvancedTransferHandle, unknown, RegisterHandleInput> {
  return useMutation({
    mutationFn: async (input: RegisterHandleInput) => {
      const { data } = await api.POST("/advanced/transfers/handles", {
        ...idempotent(input.idempotencyKey),
        body: { account_id: input.accountId, handle: input.handle },
      });
      return validated<AdvancedTransferHandle>(data, handleSpec, "/advanced/transfers/handles");
    },
  });
}

export interface ReverseTransferInput {
  readonly transferId: string;
  readonly reasonCode: string;
  readonly reason: string;
  readonly idempotencyKey: string;
}

/** The operator correction. It changes two accounts, so every advanced read is refetched. */
export function useReverseTransfer(): UseMutationResult<AdvancedTransfer, unknown, ReverseTransferInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ReverseTransferInput) => {
      const { data } = await api.POST("/advanced/transfers/{transferId}/reverse", {
        ...idempotent(input.idempotencyKey, { path: { transferId: input.transferId } }),
        body: { reason_code: input.reasonCode, reason: input.reason },
      });
      return validated<AdvancedTransfer>(data, transferSpec, "/advanced/transfers/{id}/reverse");
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}
