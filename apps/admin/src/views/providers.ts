/**
 * Provider status: health, verification label and mode.
 *
 * `mode` is the one an operator must never misread. `fake` means the adapter
 * is a test double and no external system is involved at all; `sandbox` means
 * a provider's test environment; only `live` touches real money. The console
 * puts the mode next to the health so "HEALTHY" can never be read as "working
 * in production".
 *
 * `verification` is the evidence ladder from the build state: CODE_COMPLETE,
 * CONTRACT_TESTED, SANDBOX_VERIFIED, CANARY_VERIFIED, LIVE_VERIFIED, and
 * BLOCKED_EXTERNAL for an integration waiting on someone else.
 */
import { listProviders } from "../api.ts";
import type { ProviderStatus } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { append, clear, el, emptyState, notice, panel, pill, table } from "../dom.ts";
import { formatInstant } from "../format.ts";
import { problemNotice } from "../problem.ts";

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

  const live = providers.filter((p) => p.mode === "live");
  append(
    root,
    panel(
      "Providers",
      "Every configured provider slot, its health, and whether it is a test double or the real thing.",
      live.length === 0
        ? notice("info", "No provider is in live mode. Nothing here can move real money.")
        : notice("warn", `${live.length} provider(s) are in live mode: ${live.map((p) => p.name).join(", ")}.`),
      providers.length === 0
        ? emptyState("No provider slots are configured.")
        : table(
            ["Provider", "Role", "Mode", "Health", "Verification", "Error rate", "p95", "Last success"],
            providers.map((p) =>
              el(
                "tr",
                {},
                el("td", {}, el("code", {}, p.name)),
                el("td", {}, p.role),
                el("td", {}, pill(p.mode ?? "unknown", p.mode === "live" ? "blocking" : "inactive")),
                el("td", {}, pill(p.health, p.health.toLowerCase())),
                el("td", {}, p.verification),
                // Basis points are an integer count, not money: the API sends
                // them as a JSON integer and they are rendered, never summed.
                el("td", { class: "muted" }, p.error_rate_bps === undefined ? "—" : `${String(p.error_rate_bps)} bps`),
                el("td", { class: "muted" }, p.p95_ms === undefined ? "—" : `${String(p.p95_ms)} ms`),
                el("td", { class: "muted" }, p.last_success_at ? formatInstant(p.last_success_at) : "never"),
              ),
            ),
          ),
      el(
        "p",
        { class: "muted" },
        "A DISABLED provider in fake mode is the default for an unconfigured slot. It is not an incident.",
      ),
    ),
  );
}
