/**
 * Turning a backend refusal into something a customer can act on.
 *
 * The backend answers with a stable machine-readable `code` (PART 37). The app
 * maps that code to a sentence that says what happened and what, if anything,
 * the customer can do. It never degrades a refusal into a zero, a dash or an
 * empty row: "we could not price this" and "this is worth nothing" are
 * different statements, and only one of them is true.
 */
import { ApiProblem } from "@controlplane/generated-client";

import { UNAVAILABLE_COPY } from "../lib/honesty.ts";
import { ContractViolation } from "./contract.ts";

export interface Explanation {
  readonly title: string;
  readonly body: string;
  /** The backend's stable code, always shown so support can act on it. */
  readonly code: string;
  readonly status: number | undefined;
  readonly requestId: string | undefined;
  /** Per-field messages when the backend rejected specific inputs. */
  readonly fields: ReadonlyArray<readonly [string, string]>;
  /** True when trying again unchanged could plausibly work. */
  readonly retryable: boolean;
  /** True when the caller should send the user to sign in again. */
  readonly needsSignIn: boolean;
  /** True when the action needs a stronger sign-in first. */
  readonly needsStepUp: boolean;
}

function fieldsOf(problem: ApiProblem): ReadonlyArray<readonly [string, string]> {
  const raw = problem.fields;
  if (!raw) return [];
  return Object.entries(raw).map(([key, value]) => [
    key,
    typeof value === "string" ? value : JSON.stringify(value),
  ]);
}

/**
 * Explains any thrown value. Unknown failures are reported as unknown rather
 * than dressed up: an interface that invents a reason is worse than one that
 * admits it does not have one.
 */
export function explain(error: unknown): Explanation {
  if (error instanceof ContractViolation) {
    return {
      title: "This response could not be trusted",
      body:
        `${error.message}. Rather than display a figure that does not match the API contract, ` +
        "the app is showing nothing here.",
      code: "CONTRACT_VIOLATION",
      status: undefined,
      requestId: undefined,
      fields: [[error.path, "field failed contract validation"]],
      retryable: true,
      needsSignIn: false,
      needsStepUp: false,
    };
  }

  if (!(error instanceof ApiProblem)) {
    const detail = error instanceof Error ? error.message : String(error);
    return {
      title: "The request did not complete",
      body: `The app could not reach the API or the reply was unreadable: ${detail}`,
      code: "UNREACHABLE",
      status: undefined,
      requestId: undefined,
      fields: [],
      retryable: true,
      needsSignIn: false,
      needsStepUp: false,
    };
  }

  const base = {
    code: error.code,
    status: error.status,
    requestId: error.requestId,
    fields: fieldsOf(error),
  };

  switch (error.code) {
    case "PROVIDER_UNAVAILABLE":
      return {
        ...base,
        ...UNAVAILABLE_COPY.providerUnavailable,
        body: joinDetail(UNAVAILABLE_COPY.providerUnavailable.body, error.detail),
        retryable: true,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "UNSUPPORTED":
      return {
        ...base,
        ...UNAVAILABLE_COPY.unsupported,
        body: joinDetail(UNAVAILABLE_COPY.unsupported.body, error.detail),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "CAPABILITY_NOT_APPROVED":
      return {
        ...base,
        ...UNAVAILABLE_COPY.capabilityNotApproved,
        body: joinDetail(UNAVAILABLE_COPY.capabilityNotApproved.body, error.detail),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "STEP_UP_REQUIRED":
      return {
        ...base,
        ...UNAVAILABLE_COPY.stepUpRequired,
        body: joinDetail(UNAVAILABLE_COPY.stepUpRequired.body, error.detail),
        retryable: false,
        needsSignIn: false,
        needsStepUp: true,
      };
    case "KILL_SWITCH_ACTIVE":
      return {
        ...base,
        ...UNAVAILABLE_COPY.killSwitch,
        body: joinDetail(UNAVAILABLE_COPY.killSwitch.body, error.detail),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "UNAUTHENTICATED":
      return {
        ...base,
        title: "Signed out",
        body: "This session is not signed in. Sign in again to continue.",
        retryable: false,
        needsSignIn: true,
        needsStepUp: false,
      };
    case "FORBIDDEN":
      return {
        ...base,
        title: "Not permitted",
        body: joinDetail(
          "The backend refused this for the signed-in principal. This is an authorisation decision, not an error in what you entered.",
          error.detail,
        ),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "NOT_FOUND":
      return {
        ...base,
        title: "Not found",
        body: joinDetail("The backend has no record with that identifier.", error.detail),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "RATE_LIMITED":
      return {
        ...base,
        title: "Too many requests",
        body: joinDetail(
          error.retryAfterSeconds === undefined
            ? "The backend is rate limiting this session."
            : `The backend is rate limiting this session. It asked to be left alone for ${String(error.retryAfterSeconds)} seconds.`,
          error.detail,
        ),
        retryable: true,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "VALIDATION_FAILED":
      return {
        ...base,
        title: "The request was rejected",
        body: joinDetail("The backend rejected the request as invalid.", error.detail),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "INSUFFICIENT_BUYING_POWER":
      return {
        ...base,
        title: "Not enough buying power",
        body: joinDetail(
          "The backend computed buying power at the moment it evaluated this request and found it short. Buying power is recomputed on every request and is never taken from what this page last displayed.",
          error.detail,
        ),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "QUOTE_EXPIRED":
      return {
        ...base,
        title: "That quote is no longer valid",
        body: joinDetail("The quote passed its freshness window before the request was evaluated. Take a new one.", error.detail),
        retryable: true,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "IDEMPOTENCY_IN_PROGRESS":
      return {
        ...base,
        title: "Still processing",
        body: joinDetail("An identical request is already in flight. It will not be applied twice.", error.detail),
        retryable: true,
        needsSignIn: false,
        needsStepUp: false,
      };
    case "INVALID_IDEMPOTENCY_REUSE":
      return {
        ...base,
        title: "That key was used for a different request",
        body: joinDetail("The idempotency key has already been used with a different body, so the backend refused it rather than guess which one you meant.", error.detail),
        retryable: false,
        needsSignIn: false,
        needsStepUp: false,
      };
    default:
      break;
  }

  if (error.status === 401) {
    return {
      ...base,
      title: "Signed out",
      body: "This session is not signed in. Sign in again to continue.",
      retryable: false,
      needsSignIn: true,
      needsStepUp: false,
    };
  }

  return {
    ...base,
    title: error.isBusinessRejection ? "The backend refused this" : unmappedTitle(error),
    body: joinDetail(
      `The backend answered ${error.code}${error.status ? ` with HTTP ${String(error.status)}` : ""}.`,
      error.detail,
    ),
    retryable: error.status >= 500,
    needsSignIn: false,
    needsStepUp: false,
  };
}

/**
 * The heading for a code this build has no sentence for.
 *
 * `UI_UX_SYSTEM.md` §4 forbids the generic apology by name, and it is right to:
 * it tells the reader nothing, and it is not even true — the backend said
 * exactly what happened, in a `title` this app was throwing away. So the
 * backend's own title is used where it gave one that is not just the code
 * repeated back, and where it did not, the honest fallback says what is known:
 * the request reached the backend and the backend did not complete it. The
 * stable code and the correlation id are on `base` in both cases, which is what
 * support actually acts on.
 */
function unmappedTitle(error: ApiProblem): string {
  const title = error.message.trim();
  if (title !== "" && title !== error.code) return title;
  return "The backend did not complete this";
}

function joinDetail(body: string, detail: string | undefined): string {
  if (detail === undefined || detail === "") return body;
  return `${body} The backend said: ${detail}`;
}

/** True when a failure means the session is gone. */
export function isUnauthenticated(error: unknown): boolean {
  return error instanceof ApiProblem && (error.status === 401 || error.code === "UNAUTHENTICATED");
}
