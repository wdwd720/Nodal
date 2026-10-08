/**
 * The strategy marketplace and its sandbox runs (internal/marketplace,
 * internal/sandboxrt), modelled on core.ts:
 *
 *   - every response is checked against a spec before a component sees it:
 *     decimals are exact strings (`decimal`), basis points and counts are
 *     integers, enums are closed, and a response that reports value says
 *     `value_domain: SIMULATED`;
 *   - account-scoped keys start with `advancedKeys.account(accountId)`, the
 *     rest with `strategyKeys.all`, and every write invalidates both for its
 *     account, so a refusal or a success is followed by the backend's own
 *     state rather than an edited copy of it;
 *   - every write carries an idempotency key the page creates once, when the
 *     person confirms, and reuses on every retry of that same action;
 *   - a 4xx answer is the backend's decision and is not retried.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { INSTRUMENT_KINDS, VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  BUILD_FAILURES,
  EVALUATION_KINDS,
  FOLLOW_OUTCOMES,
  FOLLOW_TRIGGERS,
  INSTALL_STATUSES,
  REVIEW_DECISIONS,
  RUN_SOURCES,
  RUN_TERMINATIONS,
  STRATEGY_INTENT_KINDS,
  STRATEGY_MODERATIONS,
  STRATEGY_RISK_CLASSES,
  STRATEGY_STATUSES,
  VALIDATION_OUTCOMES,
  type Snapshot,
  type StrategyModeration,
  type StrategyStatus,
  type VersionInput,
} from "../../lib/marketplace.ts";
import { api } from "../client.ts";
import { cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, riskDecisionSpec, type CursorPage } from "./core.ts";

export type StrategyListing = Schemas["StrategyListing"];
export type StrategyVersionSummary = Schemas["StrategyVersionSummary"];
export type StrategyVersion = Schemas["StrategyVersion"];
export type StrategyVersionDetail = Schemas["StrategyVersionDetail"];
export type StrategyListingDetail = Schemas["StrategyListingDetail"];
export type StrategyManifest = Schemas["StrategyManifest"];
export type StrategyValidation = Schemas["StrategyValidation"];
export type StrategyValidationResult = Schemas["StrategyValidationResult"];
export type StrategyReview = Schemas["StrategyReview"];
export type StrategyProvenance = Schemas["StrategyProvenance"];
export type StrategyEvaluation = Schemas["StrategyEvaluation"];
export type StrategyPerformance = Schemas["StrategyPerformanceSnapshot"];
export type StrategyReviewQueueItem = Schemas["StrategyReviewQueueItem"];
export type StrategyInstall = Schemas["StrategyInstall"];
export type StrategyFollowRun = Schemas["StrategyFollowRun"];
export type StrategyFollowIntent = Schemas["StrategyFollowIntent"];
export type StrategySandboxRun = Schemas["StrategySandboxRun"];

/** Rows per page of every marketplace list. */
export const STRATEGY_PAGE_SIZE = 25;
/** The review queue is read as a whole page of the contract's maximum. */
export const REVIEW_QUEUE_LIMIT = 100;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const strategyKeys = {
  all: ["advanced", "strategies"] as const,
  catalog: (cursor: string) => ["advanced", "strategies", "catalog", cursor] as const,
  listing: (listingId: string) => ["advanced", "strategies", "listing", listingId] as const,
  reviewQueue: (status: string, cursor: string) => ["advanced", "strategies", "review-queue", status, cursor] as const,
  accountListings: (accountId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "strategies", "listings", cursor] as const,
  installs: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "strategies", "installs", cursor] as const,
  follows: (accountId: string, installId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "strategies", "follows", installId, cursor] as const,
  runs: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "strategies", "runs", cursor] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const STATUS = oneOf(STRATEGY_STATUSES);
const MODERATION = oneOf(STRATEGY_MODERATIONS);
const RISK_CLASS = oneOf(STRATEGY_RISK_CLASSES);
const TERMINATION = oneOf(RUN_TERMINATIONS);

export const versionSummarySpec: Spec = {
  required: {
    id: "uuid",
    listing_id: "uuid",
    number: "integer",
    status: STATUS,
    moderation: MODERATION,
    risk_class: RISK_CLASS,
    description: "string",
    strategy_ref: "string",
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: { rejection_code: "string" },
};

export const listingSpec: Spec = {
  required: { id: "uuid", title: "string", created_at: "timestamp" },
  optional: { forked_from_version_id: "uuid", creator_account_id: "uuid" },
  objects: { latest_version: { required: true, spec: versionSummarySpec } },
};

export const manifestSpec: Spec = {
  required: {
    schema: "string",
    max_notional_per_intent: "decimal",
    max_intents_per_run: "integer",
    min_interval_seconds: "integer",
  },
  lists: {
    allowed_intent_kinds: { required: true, kind: oneOf(STRATEGY_INTENT_KINDS) },
    allowed_instrument_ids: { required: true, kind: "uuid" },
    allowed_instrument_kinds: { required: true, kind: oneOf(INSTRUMENT_KINDS) },
  },
};

export const versionSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    listing_id: "uuid",
    number: "integer",
    status: STATUS,
    moderation: MODERATION,
    description: "string",
    risk_class: RISK_CLASS,
    manifest_hash: "string",
    source_digest: "string",
    source_commit: "string",
    backtest_evidence_ref: "string",
    config: "object",
    strategy_ref: "string",
    created_by_actor_type: "string",
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: { rejection_code: "string", created_by_actor_id: "string" },
  objects: {
    manifest: { required: true, spec: manifestSpec },
    fees: {
      required: true,
      spec: { required: { performance_fee_bps: "integer", management_fee_bps: "integer", high_water_mark: "boolean" } },
    },
  },
  lists: {
    supported_instrument_ids: { required: true, kind: "uuid" },
    data_dependencies: { required: true, kind: "string" },
  },
};

export const validationSpec: Spec = {
  required: {
    outcome: oneOf(VALIDATION_OUTCOMES),
    toolchain_version: "string",
    sdk_version: "string",
    runtime_version: "string",
  },
  optional: { artifact_digest: "string", failure_reason: oneOf(BUILD_FAILURES), build_log: "string" },
  lists: { scan_codes: { required: true, kind: "string" } },
};

export const reviewSpec: Spec = {
  required: {
    decision: oneOf(REVIEW_DECISIONS),
    reason_code: "string",
    note: "string",
    reviewer_actor_type: "string",
    reviewed_at: "timestamp",
  },
  optional: { reviewer_actor_id: "string" },
};

export const provenanceSpec: Spec = {
  required: { strategy_ref: "string", source_commit: "string", source_digest: "string", manifest_hash: "string" },
  optional: {
    artifact_digest: "string",
    toolchain_version: "string",
    sdk_version: "string",
    runtime_version: "string",
    dataset_version: "string",
    evaluation_id: "uuid",
  },
  arrays: {
    forked_from: {
      required: true,
      spec: {
        required: {
          version_id: "uuid",
          listing_id: "uuid",
          number: "integer",
          source_digest: "string",
          manifest_hash: "string",
        },
      },
    },
  },
};

export const evaluationSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    version_id: "uuid",
    kind: oneOf(EVALUATION_KINDS),
    dataset_version: "string",
    snapshots_count: "integer",
    snapshots_digest: "string",
    results_digest: "string",
    runs_ok: "integer",
    proposals_count: "integer",
    accepted_count: "integer",
    violations_count: "integer",
    evidence_ref: "string",
    evidence_sha256: "string",
  },
  optional: { created_at: "timestamp" },
  arrays: {
    runs: {
      spec: {
        required: {
          seq: "integer",
          sandbox_run_id: "uuid",
          as_of: "timestamp",
          input_digest: "string",
          run_digest: "string",
          termination: TERMINATION,
          proposals: "integer",
          accepted: "integer",
          violations: "integer",
        },
      },
    },
  },
};

export const performanceSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    version_id: "uuid",
    follow_runs_count: "integer",
    first_as_of: "timestamp",
    last_as_of: "timestamp",
    history_digest: "string",
    return_bps: "integer",
    max_drawdown_bps: "integer",
    volatility_bps: "integer",
    turnover_bps: "integer",
    computed_at: "timestamp",
  },
  optional: { install_id: "uuid" },
};

export const versionDetailSpec: Spec = {
  ...versionSpec,
  objects: {
    ...versionSpec.objects,
    validation: { spec: validationSpec },
    provenance: { required: true, spec: provenanceSpec },
  },
  arrays: {
    reviews: { required: true, spec: reviewSpec },
    evaluations: { required: true, spec: evaluationSpec },
    performance: { required: true, spec: performanceSpec },
  },
};

export const listingDetailSpec: Spec = {
  required: { value_domain: SIMULATED, id: "uuid", title: "string", created_at: "timestamp" },
  optional: { forked_from_version_id: "uuid", creator_account_id: "uuid" },
  arrays: { versions: { required: true, spec: versionDetailSpec } },
};

export const validationResultSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: {
    version: { required: true, spec: versionSpec },
    validation: { required: true, spec: validationSpec },
  },
};

export const reviewQueueItemSpec: Spec = {
  required: { listing_id: "uuid", listing_title: "string", creator_account_id: "uuid" },
  objects: { version: { required: true, spec: versionSummarySpec } },
};

export const installSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    account_id: "uuid",
    version_id: "uuid",
    listing_id: "uuid",
    listing_title: "string",
    strategy_ref: "string",
    version_number: "integer",
    version_status: STATUS,
    version_moderation: MODERATION,
    // PAPER and nothing else: an install that says otherwise is refused here
    // rather than drawn under a notice saying nothing here is real.
    mode: oneOf(["PAPER"]),
    status: oneOf(INSTALL_STATUSES),
    installed_by_actor_type: "string",
    paper_capital: "decimal",
    created_at: "timestamp",
  },
  objects: { latest_performance: { spec: performanceSpec } },
};

export const followIntentSpec: Spec = {
  required: {
    proposal_seq: "integer",
    intent_id: "uuid",
    kind: "string",
    outcome: oneOf(FOLLOW_OUTCOMES),
    notional: "decimal",
  },
  optional: {
    reference_price: "decimal",
    decision_hash: "string",
    effect_id: "uuid",
    effect_status: "string",
    effect_failure_code: "string",
    external_ref: "string",
  },
  objects: { risk_decision: { spec: riskDecisionSpec } },
};

export const followMarkSpec: Spec = {
  required: { instrument_id: "uuid", price_id: "uuid", mark: "decimal", observed_at: "timestamp" },
};

export const followRunSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    install_id: "uuid",
    version_id: "uuid",
    account_id: "uuid",
    sandbox_run_id: "uuid",
    trigger: oneOf(FOLLOW_TRIGGERS),
    as_of: "timestamp",
    input_digest: "string",
    run_digest: "string",
    termination: TERMINATION,
    proposals_count: "integer",
    accepted_count: "integer",
    violations_count: "integer",
    intents_submitted: "integer",
    intents_dispatched: "integer",
    intents_risk_rejected: "integer",
    intents_failed: "integer",
    equity: "decimal",
    turnover: "decimal",
    evidence_ref: "string",
    evidence_sha256: "string",
    created_at: "timestamp",
  },
  optional: { replayed: "boolean", paper_cash: "decimal" },
  arrays: {
    intents: { required: true, spec: followIntentSpec },
    marks: { required: true, spec: followMarkSpec },
  },
};

export const sandboxRunSpec: Spec = {
  required: {
    id: "uuid",
    source: oneOf(RUN_SOURCES),
    strategy_ref: "string",
    termination: TERMINATION,
    exit_code: "integer",
    artifact_verified: "boolean",
    wall_ms: "integer",
    output_bytes: "integer",
    artifact_digest: "string",
    input_digest: "string",
    output_digest: "string",
    run_digest: "string",
    limits_version: "string",
    proposals_count: "integer",
    accepted_count: "integer",
    violations_count: "integer",
    created_at: "timestamp",
  },
  optional: { follow_run_id: "uuid", evaluation_id: "uuid" },
  arrays: {
    violations: { required: true, spec: { required: { record_seq: "integer", code: "string", detail: "string" } } },
  },
};

const listingPageSpec = cursorPageSpec(listingSpec);
const installPageSpec: Spec = { ...cursorPageSpec(installSpec), required: { value_domain: SIMULATED } };
const followPageSpec: Spec = { ...cursorPageSpec(followRunSpec), required: { value_domain: SIMULATED } };
const runPageSpec = cursorPageSpec(sandboxRunSpec);
const reviewQueuePageSpec = cursorPageSpec(reviewQueueItemSpec);

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function useStrategyCatalog(cursor: string | undefined): UseQueryResult<CursorPage<StrategyListing>> {
  return useQuery({
    queryKey: strategyKeys.catalog(cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/strategies/catalog", {
        params: { query: { limit: STRATEGY_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) } },
      });
      const page = validated<Schemas["StrategyListingPage"]>(data, listingPageSpec, "/advanced/strategies/catalog");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useAccountListings(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<StrategyListing>> {
  return useQuery({
    queryKey: strategyKeys.accountListings(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/strategies/listings", {
        params: {
          query: { account_id: accountId ?? "", limit: STRATEGY_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
        },
      });
      const page = validated<Schemas["StrategyListingPage"]>(data, listingPageSpec, "/advanced/strategies/listings");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One listing. The backend decides from the session what of it may be read,
 * and answers NOT_FOUND when nothing may.
 */
export function useStrategyListing(listingId: string | undefined): UseQueryResult<StrategyListingDetail> {
  return useQuery({
    queryKey: strategyKeys.listing(listingId ?? ""),
    enabled: listingId !== undefined && listingId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/strategies/listings/{listingId}", {
        params: { path: { listingId: listingId ?? "" } },
      });
      return validated<StrategyListingDetail>(data, listingDetailSpec, "/advanced/strategies/listings/{id}");
    },
  });
}

export function useReviewQueue(
  status: StrategyStatus | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<StrategyReviewQueueItem>> {
  return useQuery({
    queryKey: strategyKeys.reviewQueue(status ?? "", cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/strategies/review-queue", {
        params: {
          query: {
            limit: REVIEW_QUEUE_LIMIT,
            ...(status === undefined ? {} : { status }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["StrategyReviewQueuePage"]>(data, reviewQueuePageSpec, "/advanced/strategies/review-queue");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useStrategyInstalls(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<StrategyInstall>> {
  return useQuery({
    queryKey: strategyKeys.installs(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/strategies/installs", {
        params: {
          query: { account_id: accountId ?? "", limit: STRATEGY_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
        },
      });
      const page = validated<Schemas["StrategyInstallPage"]>(data, installPageSpec, "/advanced/strategies/installs");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useStrategyFollows(
  accountId: string | undefined,
  installId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<StrategyFollowRun>> {
  return useQuery({
    queryKey: strategyKeys.follows(accountId ?? "", installId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/strategies/follows", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: STRATEGY_PAGE_SIZE,
            ...(installId === undefined ? {} : { install_id: installId }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["StrategyFollowRunPage"]>(data, followPageSpec, "/advanced/strategies/follows");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useStrategyRuns(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<StrategySandboxRun>> {
  return useQuery({
    queryKey: strategyKeys.runs(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/strategies/runs", {
        params: {
          query: { account_id: accountId ?? "", limit: STRATEGY_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
        },
      });
      const page = validated<Schemas["StrategySandboxRunPage"]>(data, runPageSpec, "/advanced/strategies/runs");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/* --------------------------------------------------------------------------
 * Writes
 * ------------------------------------------------------------------------ */

/** Everything a marketplace write can have changed, for the account it acted for. */
function useInvalidate(): (accountId: string | undefined) => void {
  const queryClient = useQueryClient();
  return (accountId) => {
    void queryClient.invalidateQueries({ queryKey: strategyKeys.all });
    if (accountId !== undefined) {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(accountId) });
    }
  };
}

interface Keyed {
  /** Created once when the person confirmed, reused for every retry of this action. */
  readonly idempotencyKey: string;
}

export interface CreateListingInput extends Keyed {
  readonly accountId: string;
  readonly title: string;
  readonly version: VersionInput;
}

export function useCreateListing(): UseMutationResult<StrategyListingDetail, unknown, CreateListingInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: CreateListingInput) => {
      const { data } = await api.POST("/advanced/strategies/listings", {
        ...idempotent(input.idempotencyKey),
        body: { account_id: input.accountId, title: input.title, version: input.version },
      });
      return validated<StrategyListingDetail>(data, listingDetailSpec, "/advanced/strategies/listings");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

export interface NewVersionInput extends Keyed {
  readonly accountId: string;
  readonly listingId: string;
  readonly version: VersionInput;
}

export function useNewVersion(): UseMutationResult<StrategyVersion, unknown, NewVersionInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: NewVersionInput) => {
      const { data } = await api.POST("/advanced/strategies/listings/{listingId}/versions", {
        ...idempotent(input.idempotencyKey, { path: { listingId: input.listingId } }),
        body: { account_id: input.accountId, version: input.version },
      });
      return validated<StrategyVersion>(data, versionSpec, "/advanced/strategies/listings/{id}/versions");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

export interface ForkInput extends Keyed {
  readonly accountId: string;
  readonly versionId: string;
  readonly title: string;
  readonly version: VersionInput;
}

export function useForkVersion(): UseMutationResult<StrategyListingDetail, unknown, ForkInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: ForkInput) => {
      const { data } = await api.POST("/advanced/strategies/versions/{versionId}/forks", {
        ...idempotent(input.idempotencyKey, { path: { versionId: input.versionId } }),
        body: { account_id: input.accountId, title: input.title, version: input.version },
      });
      return validated<StrategyListingDetail>(data, listingDetailSpec, "/advanced/strategies/versions/{id}/forks");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

export interface VersionAccountInput extends Keyed {
  readonly accountId: string;
  readonly versionId: string;
}

export function useSubmitVersion(): UseMutationResult<StrategyVersion, unknown, VersionAccountInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: VersionAccountInput) => {
      const { data } = await api.POST("/advanced/strategies/versions/{versionId}/submit", {
        ...idempotent(input.idempotencyKey, { path: { versionId: input.versionId } }),
        body: { account_id: input.accountId },
      });
      return validated<StrategyVersion>(data, versionSpec, "/advanced/strategies/versions/{id}/submit");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

export interface SimulateInput extends Keyed {
  readonly accountId: string;
  readonly versionId: string;
  readonly datasetVersion: string;
  readonly snapshots: readonly Snapshot[];
}

export function useSimulateVersion(): UseMutationResult<StrategyEvaluation, unknown, SimulateInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: SimulateInput) => {
      const { data } = await api.POST("/advanced/strategies/versions/{versionId}/simulations", {
        ...idempotent(input.idempotencyKey, { path: { versionId: input.versionId } }),
        body: { account_id: input.accountId, dataset_version: input.datasetVersion, snapshots: [...input.snapshots] },
      });
      return validated<StrategyEvaluation>(data, evaluationSpec, "/advanced/strategies/versions/{id}/simulations");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

export interface InstallInput extends Keyed {
  readonly accountId: string;
  readonly versionId: string;
}

/** Installs in PAPER, the only mode an install can have. */
export function useInstallVersion(): UseMutationResult<StrategyInstall, unknown, InstallInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: InstallInput) => {
      const { data } = await api.POST("/advanced/strategies/installs", {
        ...idempotent(input.idempotencyKey),
        body: { account_id: input.accountId, version_id: input.versionId, mode: "PAPER" },
      });
      return validated<StrategyInstall>(data, installSpec, "/advanced/strategies/installs");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

export interface InstallActionInput extends Keyed {
  readonly accountId: string;
  readonly installId: string;
}

export function useRemoveInstall(): UseMutationResult<StrategyInstall, unknown, InstallActionInput & { readonly reason: string }> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: InstallActionInput & { readonly reason: string }) => {
      const { data } = await api.POST("/advanced/strategies/installs/{installId}/remove", {
        ...idempotent(input.idempotencyKey, { path: { installId: input.installId } }),
        body: { account_id: input.accountId, ...(input.reason === "" ? {} : { reason: input.reason }) },
      });
      return validated<StrategyInstall>(data, installSpec, "/advanced/strategies/installs/{id}/remove");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

/**
 * A paper follow names the install and the account, and nothing about the
 * market or the portfolio: the backend builds the run's input itself from
 * the install's paper book and the reference prices it reads.
 */
export type FollowInput = InstallActionInput;

export function useFollowInstall(): UseMutationResult<StrategyFollowRun, unknown, FollowInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: FollowInput) => {
      const { data } = await api.POST("/advanced/strategies/installs/{installId}/follows", {
        ...idempotent(input.idempotencyKey, { path: { installId: input.installId } }),
        body: { account_id: input.accountId },
      });
      return validated<StrategyFollowRun>(data, followRunSpec, "/advanced/strategies/installs/{id}/follows");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

export function useRecordPerformance(): UseMutationResult<StrategyPerformance, unknown, InstallActionInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: InstallActionInput) => {
      const { data } = await api.POST("/advanced/strategies/installs/{installId}/performance", {
        ...idempotent(input.idempotencyKey, { path: { installId: input.installId } }),
        body: { account_id: input.accountId },
      });
      return validated<StrategyPerformance>(data, performanceSpec, "/advanced/strategies/installs/{id}/performance");
    },
    onSettled: (_data, _error, input) => {
      invalidate(input.accountId);
    },
  });
}

/* --------------------------------------------------------------------------
 * Operator decisions
 * ------------------------------------------------------------------------ */

export interface DecisionInput extends Keyed {
  readonly versionId: string;
}

export function useValidateVersion(): UseMutationResult<StrategyValidationResult, unknown, DecisionInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: DecisionInput) => {
      const { data } = await api.POST("/advanced/strategies/versions/{versionId}/validate", {
        ...idempotent(input.idempotencyKey, { path: { versionId: input.versionId } }),
      });
      return validated<StrategyValidationResult>(data, validationResultSpec, "/advanced/strategies/versions/{id}/validate");
    },
    onSettled: () => {
      invalidate(undefined);
    },
  });
}

export function useStartReview(): UseMutationResult<StrategyVersion, unknown, DecisionInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: DecisionInput) => {
      const { data } = await api.POST("/advanced/strategies/versions/{versionId}/review", {
        ...idempotent(input.idempotencyKey, { path: { versionId: input.versionId } }),
      });
      return validated<StrategyVersion>(data, versionSpec, "/advanced/strategies/versions/{id}/review");
    },
    onSettled: () => {
      invalidate(undefined);
    },
  });
}

export interface ReviewDecisionInput extends DecisionInput {
  readonly decision: "approve" | "reject";
  readonly reasonCode: string;
  readonly note: string;
}

export function useReviewDecision(): UseMutationResult<StrategyVersion, unknown, ReviewDecisionInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: ReviewDecisionInput) => {
      const params = idempotent(input.idempotencyKey, { path: { versionId: input.versionId } });
      const body = { reason_code: input.reasonCode, ...(input.note === "" ? {} : { note: input.note }) };
      const { data } =
        input.decision === "approve"
          ? await api.POST("/advanced/strategies/versions/{versionId}/approve", { ...params, body })
          : await api.POST("/advanced/strategies/versions/{versionId}/reject", { ...params, body });
      return validated<StrategyVersion>(data, versionSpec, `/advanced/strategies/versions/{id}/${input.decision}`);
    },
    onSettled: () => {
      invalidate(undefined);
    },
  });
}

export interface ModerateInput extends DecisionInput {
  readonly to: StrategyModeration;
  readonly reason: string;
}

export function useModerateVersion(): UseMutationResult<StrategyVersion, unknown, ModerateInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: ModerateInput) => {
      const { data } = await api.POST("/advanced/strategies/versions/{versionId}/moderation", {
        ...idempotent(input.idempotencyKey, { path: { versionId: input.versionId } }),
        body: { to: input.to, reason: input.reason },
      });
      return validated<StrategyVersion>(data, versionSpec, "/advanced/strategies/versions/{id}/moderation");
    },
    onSettled: () => {
      invalidate(undefined);
    },
  });
}

export interface DeprecateInput extends DecisionInput {
  readonly reason: string;
}

export function useDeprecateVersion(): UseMutationResult<StrategyVersion, unknown, DeprecateInput> {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (input: DeprecateInput) => {
      const { data } = await api.POST("/advanced/strategies/versions/{versionId}/deprecate", {
        ...idempotent(input.idempotencyKey, { path: { versionId: input.versionId } }),
        body: { reason: input.reason },
      });
      return validated<StrategyVersion>(data, versionSpec, "/advanced/strategies/versions/{id}/deprecate");
    },
    onSettled: () => {
      invalidate(undefined);
    },
  });
}
