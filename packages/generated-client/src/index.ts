/**
 * Typed API client generated from openapi/openapi.yaml (PART 179, PART 186).
 *
 * The client understands the three things the goal document requires of it:
 *   - idempotency: every command carries an Idempotency-Key that the caller creates once per
 *     user action and keeps stable across retries, reconnects and double clicks (PART 173);
 *   - correlation: every request carries X-Request-Id / X-Correlation-Id for tracing;
 *   - structured errors: application/problem+json bodies are surfaced as typed ApiProblem values,
 *     never as bare strings.
 *
 * It never re-implements financial math: amounts are strings from the backend and are only
 * formatted for display by the caller.
 */
import createClient, { type Client, type Middleware } from "openapi-fetch";

import type { components, paths } from "./schema.d.ts";

export type Paths = paths;
export type Schemas = components["schemas"];
export type Problem = Schemas["Problem"];
export type USD = Schemas["USD"];
export type Quantity = Schemas["Quantity"];

/** A structured API failure. `code` is the stable machine-readable code from the backend. */
export class ApiProblem extends Error {
  readonly status: number;
  readonly code: string;
  readonly detail: string | undefined;
  readonly fields: Record<string, unknown> | undefined;
  readonly requestId: string | undefined;
  readonly retryAfterSeconds: number | undefined;

  constructor(problem: Problem, retryAfterSeconds?: number) {
    super(problem.title ?? problem.code);
    this.name = "ApiProblem";
    this.status = problem.status;
    this.code = problem.code;
    this.detail = problem.detail;
    this.fields = problem.fields;
    this.requestId = problem.request_id;
    this.retryAfterSeconds = retryAfterSeconds;
  }

  /** True for business rejections the UI should explain rather than retry. */
  get isBusinessRejection(): boolean {
    return this.status === 422 || this.status === 409 || this.status === 403;
  }
}

export interface ClientOptions {
  /** Base URL including the /v1 prefix, e.g. "https://app.example.test/v1". */
  baseUrl: string;
  /** Source of request ids; defaults to crypto.randomUUID. */
  requestId?: () => string;
  /** Optional fetch implementation (tests, server-side rendering). */
  fetch?: typeof fetch;
}

const problemContentType = "application/problem+json";

function isProblem(value: unknown): value is Problem {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as { code?: unknown }).code === "string" &&
    typeof (value as { status?: unknown }).status === "number"
  );
}

/**
 * Creates the typed client. Errors are thrown as ApiProblem; callers `await` normally and catch.
 * Idempotency keys are supplied per call through `headers` (see `idempotent()`), never generated
 * implicitly, so a retry of the same user action reuses the same key.
 */
export function createApiClient(options: ClientOptions): Client<paths> {
  const requestId = options.requestId ?? (() => crypto.randomUUID());
  const client = createClient<paths>({
    baseUrl: options.baseUrl,
    credentials: "include",
    ...(options.fetch ? { fetch: options.fetch } : {}),
  });

  const tracing: Middleware = {
    onRequest({ request }) {
      if (!request.headers.has("X-Request-Id")) {
        request.headers.set("X-Request-Id", requestId());
      }
      if (!request.headers.has("Accept")) {
        request.headers.set("Accept", "application/json, application/problem+json");
      }
      return request;
    },
    async onResponse({ response }) {
      if (response.ok) {
        return response;
      }
      const contentType = response.headers.get("content-type") ?? "";
      if (contentType.startsWith(problemContentType) || contentType.startsWith("application/json")) {
        let body: unknown;
        try {
          body = await response.clone().json();
        } catch {
          body = undefined;
        }
        if (isProblem(body)) {
          const retryAfter = response.headers.get("Retry-After");
          throw new ApiProblem(body, retryAfter ? Number(retryAfter) : undefined);
        }
      }
      throw new ApiProblem({
        type: "urn:problem:internal",
        title: response.statusText || "request failed",
        status: response.status,
        code: response.status >= 500 ? "INTERNAL" : "VALIDATION_FAILED",
      });
    },
  };
  client.use(tracing);
  return client;
}

/**
 * Request parameters for a money-affecting command. Create the key once when the user confirms
 * the action and reuse it for every retry of that same action (PART 173). Merge additional
 * path/query params into the returned `params` object.
 */
export function idempotent<P extends { path?: object; query?: object } = Record<never, never>>(
  key: string,
  params?: P,
): { params: P & { header: { "Idempotency-Key": string } } } {
  if (key.length < 8 || key.length > 128) {
    throw new Error("idempotency key must be 8–128 characters");
  }
  return { params: { ...(params ?? ({} as P)), header: { "Idempotency-Key": key } } };
}

/** A fresh idempotency key for a new user action. */
export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}
