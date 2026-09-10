/**
 * Provider status: health, verification label and mode.
 *
 * `mode` is the one an operator must never misread, and the three values are
 * three different things rather than degrees of the same one. `fake` means the
 * adapter is a test double and no external system is involved at all;
 * `sandbox` means a provider's own test environment, where requests are real
 * requests and no value moves; only `live` touches real money. The console
 * gives each its own pill — sandbox in the sandbox hue, never the amber a fake
 * slot gets and never the red a live one gets — so "HEALTHY" can never be read
 * as "working in production" and "sandbox" can never be skimmed as "fake".
 *
 * `verification` is the evidence ladder from the build state: CODE_COMPLETE,
 * CONTRACT_TESTED, SANDBOX_VERIFIED, CANARY_VERIFIED, LIVE_VERIFIED, and
 * BLOCKED_EXTERNAL for an integration waiting on someone else. It is a claim
 * about how well an adapter has been proven, not about what it is wired to, so
 * it is rendered next to the mode and never instead of it.
 *
 * One provider is constrained by its own code rather than by configuration:
 * `internal/provider/payoutsandbox` ("sandbox_payout") accepts a payout,
 * settles it ten seconds later, moves nothing, and refuses to be constructed in
 * PROD at all. If a response ever labelled it `live`, that would be a broken
 * report rather than a live payout rail, and this view says so instead of
 * rendering the word.
 */
import { listProviders } from "../api.ts";
import type { ProviderStatus } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { formatInstant } from "../format.ts";
import { problemNotice } from "../problem.ts";

/**
 * Providers whose adapter cannot move value however it is configured.
 *
 * `payoutsandbox.Name` is the only member: it is a first-class provider (a
 * production wiring may not import a test double) that settles without moving
 * anything and refuses to construct in PROD (ADR-0023). A `live` label on one
 * of these contradicts the adapter's own constructor.
 */
const CANNOT_BE_LIVE: readonly string[] = ["sandbox_payout"];

/**
 * Provider roles `cmd/api`'s catalogue does not describe, so no row for them
 * can appear above however they are configured. The payout slot is real
 * configuration — `CP_PROVIDER_PAYOUT_NAME` / `CP_PROVIDER_PAYOUT_MODE`, and
 * STAGING sets both — but `providerCatalog` in cmd/api/wire.go lists twelve
 * slots and not this one. An operator looking for the payout rail here would
 * otherwise conclude none is configured.
 */
const UNCATALOGUED_SLOTS: ReadonlyArray<{ role: string; config: string }> = [
  { role: "PayoutProvider", config: "CP_PROVIDER_PAYOUT_NAME / CP_PROVIDER_PAYOUT_MODE" },
];

export async function renderProviders(ctx: ViewContext, root: HTMLElement): Promise<void> {
  clear(root);
  void ctx;

  let providers: ProviderStatus[];
  try {
    providers = await listProviders();
  } catch (err) {
    append(root, problemNotice(err, "provider status"));
    return;
  }

  // A provider that cannot be live is not counted as live, whatever the label
  // says: the contradiction is reported separately and loudly.
  const live = providers.filter((p) => p.mode === "live" && !CANNOT_BE_LIVE.includes(p.name));
  const contradictory = providers.filter((p) => p.mode === "live" && CANNOT_BE_LIVE.includes(p.name));
  const sandbox = providers.filter((p) => effectiveMode(p) === "sandbox");

  append(
    root,
    panel(
      "Providers",
      "Every configured provider slot, its health, and whether it is a test double, a provider's sandbox, or the real thing.",
      live.length === 0
        ? notice("info", "No provider is in live mode. Nothing here can move real money.")
        : notice("warn", `${live.length} provider(s) are in live mode: ${live.map((p) => p.name).join(", ")}.`),
      sandbox.length > 0
        ? notice(
            "info",
            `${sandbox.length} provider(s) are in sandbox mode: ${sandbox.map((p) => p.name).join(", ")}. Requests are real requests against a provider's test environment; no value moves.`,
          )
        : null,
      ...contradictory.map(contradictionNotice),
      providers.length === 0
        ? emptyState("No provider slots are configured.")
        : table(
            ["Provider", "Role", "Mode", "Health", "Verification", "Error rate", "p95", "Last success"],
            providers.flatMap((p) => providerRows(p)),
          ),
      el(
        "p",
        { class: "muted" },
        "A DISABLED provider in fake mode is the default for an unconfigured slot. It is not an incident.",
      ),
      uncataloguedNotice(),
    ),
  );
}

/**
 * The mode this console will render, which is the configured one except where
 * the adapter's own code contradicts it. A provider that refuses to exist in
 * PROD and moves nothing is a sandbox whatever the catalogue says.
 */
function effectiveMode(p: ProviderStatus): "fake" | "sandbox" | "live" | "unknown" {
  if (CANNOT_BE_LIVE.includes(p.name)) return "sandbox";
  return p.mode ?? "unknown";
}

function providerRows(p: ProviderStatus): HTMLElement[] {
  const mode = effectiveMode(p);
  const row = el(
    "tr",
    { class: mode === "sandbox" ? "sandboxed" : "" },
    el("td", {}, el("code", {}, p.name)),
    el("td", {}, p.role),
    el("td", {}, modePill(p)),
    el("td", {}, pill(p.health, p.health.toLowerCase())),
    el("td", {}, p.verification),
    // Basis points are an integer count, not money: the API sends
    // them as a JSON integer and they are rendered, never summed.
    el("td", { class: "muted" }, p.error_rate_bps === undefined ? "—" : `${String(p.error_rate_bps)} bps`),
    el("td", { class: "muted" }, p.p95_ms === undefined ? "—" : `${String(p.p95_ms)} ms`),
    el("td", { class: "muted" }, p.last_success_at ? formatInstant(p.last_success_at) : "never"),
  );
  if (!p.disable_reason) return [row];
  // A disabled provider always has a reason on the wire and it was never
  // rendered. "DISABLED" without it tells an operator that something is off
  // and nothing about what.
  const detail = el("tr", { class: "detail" });
  const cell = el("td", { colspan: "8" });
  append(cell, fields(field("Why this slot is disabled", p.disable_reason)));
  append(detail, cell);
  return [row, detail];
}

function modePill(p: ProviderStatus): HTMLElement {
  const mode = effectiveMode(p);
  switch (mode) {
    case "live":
      return pill("live", "blocking", "This slot is wired to a production provider and can move real value.");
    case "sandbox":
      return pill(
        "sandbox",
        "sandbox",
        CANNOT_BE_LIVE.includes(p.name)
          ? "This adapter settles without moving anything and refuses to be constructed in PROD."
          : "A provider's own test environment. Real requests, no value.",
      );
    case "fake":
      return pill("fake", "inactive", "A test double. No external system is involved at all.");
    default:
      return pill("unknown", "inactive", "The API did not report a mode for this slot.");
  }
}

/** A `live` label on a provider whose code refuses to be live. */
function contradictionNotice(p: ProviderStatus): HTMLElement {
  return notice(
    "error",
    el("strong", {}, `${p.name} is reported as live, and it cannot be.`),
    el(
      "p",
      {},
      "This adapter settles without moving value and refuses to be constructed in PROD (internal/provider/payoutsandbox). It is shown as sandbox above because that is what it is; the report is what is wrong. Treat this as a configuration or wiring defect and check which adapter the payout slot actually holds.",
    ),
  );
}

/** Slots that are configurable but that the API's catalogue never describes. */
function uncataloguedNotice(): HTMLElement {
  return notice(
    "info",
    el("strong", {}, "Not every configured provider slot appears above."),
    el(
      "ul",
      { class: "reasons" },
      ...UNCATALOGUED_SLOTS.map((s) => el("li", {}, `${s.role} — configured by ${s.config}, absent from cmd/api's provider catalogue.`)),
    ),
    el(
      "p",
      { class: "muted" },
      "The absence of a row here is not evidence that a slot is unconfigured. Adding the slot to providerCatalog in cmd/api/wire.go is what would make it visible.",
    ),
  );
}
