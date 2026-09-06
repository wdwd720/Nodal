/**
 * Rendering a refusal.
 *
 * The API answers every failure as application/problem+json with a stable
 * machine-readable code and a request id, and the generated client turns that
 * into an `ApiProblem`. This module renders it without editorialising: the
 * server's own `detail` is shown when there is one, the code is always shown,
 * and the request id is always shown so an operator can quote it.
 *
 * Two refusals are states of the deployment rather than of the operator, and
 * are rendered as such instead of as an error:
 *
 *   422 UNSUPPORTED           the capability is not wired in this deployment
 *   503 PROVIDER_UNAVAILABLE  an upstream is down and the server refused to guess
 *
 * Neither is ever rendered as an empty list. An empty list means the server
 * said there is nothing; these mean the server could not say.
 */
import { ApiProblem } from "./api.ts";
import { append, el, notice } from "./dom.ts";
import { codeText } from "./format.ts";

/** A notice describing why `what` could not be shown or done. */
export function problemNotice(err: unknown, what: string): HTMLElement {
  if (!(err instanceof ApiProblem)) {
    return notice("error", `Could not load ${what}: ${err instanceof Error ? err.message : String(err)}`);
  }
  const kind = err.code === "UNSUPPORTED" || err.code === "PROVIDER_UNAVAILABLE" ? "warn" : "error";
  const box = notice(kind);
  append(
    box,
    el("strong", {}, headline(err, what)),
    el("p", {}, err.detail ?? codeText(err.code)),
    el(
      "p",
      { class: "muted" },
      `HTTP ${String(err.status)} ${err.code}`,
      err.requestId ? ` — request ${err.requestId}` : "",
    ),
  );
  if (err.retryAfterSeconds !== undefined) {
    append(box, el("p", { class: "muted" }, `Retry after ${String(err.retryAfterSeconds)}s.`));
  }
  return box;
}

function headline(err: ApiProblem, what: string): string {
  switch (err.code) {
    case "UNSUPPORTED":
      return `${capitalise(what)} is not wired in this deployment.`;
    case "PROVIDER_UNAVAILABLE":
      return `${capitalise(what)} is unavailable upstream.`;
    case "STEP_UP_REQUIRED":
      return "A recent multi-factor sign-in is required.";
    case "FORBIDDEN":
      return "Refused.";
    case "UNAUTHENTICATED":
      return "Your session has ended.";
    default:
      return `Could not load ${what}.`;
  }
}

function capitalise(value: string): string {
  return value.length === 0 ? value : `${value.charAt(0).toUpperCase()}${value.slice(1)}`;
}

/** One-line description of a failure, for inline reporting. */
export function describeProblem(err: unknown): string {
  if (err instanceof ApiProblem) {
    return `${err.code}: ${err.detail ?? err.message}${err.requestId ? ` (request ${err.requestId})` : ""}`;
  }
  return err instanceof Error ? err.message : String(err);
}
