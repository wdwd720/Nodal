/**
 * The three approval flows that have their own dedicated approve permission —
 * withdrawals, capital envelopes and agent promotion — rendered as what they
 * are: filtered views of the one dual-control queue.
 *
 * There is no separate withdrawal-review endpoint, no envelope endpoint and no
 * agent-promotion endpoint on the HTTP surface. Each flow is expressed as a
 * controlled action of its own kind, with its own propose and approve
 * permission, and the console links to the queue rather than inventing a
 * parallel one. That is not a limitation being worked around: it is why every
 * one of these gets the same reason, the same expiry, the same distinct
 * approver and the same audit trail.
 */
import { listActions } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { decideProposal } from "../decide.ts";
import { append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { formatDuration, kindLabel, reasonText, relativeInstant } from "../format.ts";
import { problemNotice } from "../problem.ts";

/** Builds a renderer for one surface that reviews a fixed set of kinds. */
export function approvalSurface(surfaceId: string, heading: string, blurb: string) {
  return async (ctx: ViewContext, root: HTMLElement): Promise<void> => {
    clear(root);
    const surface = ctx.authority.surface(surfaceId);
    const kinds = surface?.action_kinds ?? [];

    append(root, panel(heading, blurb, policyTable(ctx, kinds)));

    if (kinds.length === 0) {
      append(root, emptyState("This surface reviews no action kinds."));
      return;
    }

    let page;
    try {
      page = await listActions("", { limit: 100 });
    } catch (err) {
      append(root, problemNotice(err, "the approval queue"));
      return;
    }
    const items = page.items.filter((a) => kinds.includes(a.kind));
    append(
      root,
      panel(
        "Queue",
        `${items.length} action(s) of ${kinds.length === 1 ? "this kind" : "these kinds"}.`,
        items.length === 0
          ? emptyState("Nothing is awaiting a decision here.")
          : table(
              ["Kind", "Target", "Status", "Proposed by", "Approved by", "Expires", ""],
              items.map((a) =>
                el(
                  "tr",
                  {},
                  el("td", {}, kindLabel(a.kind)),
                  el("td", {}, el("code", {}, `${a.target_type}/${a.target_id}`)),
                  el("td", {}, pill(a.status, a.status.toLowerCase())),
                  el("td", {}, el("code", {}, a.proposed_by)),
                  el(
                    "td",
                    {},
                    a.approved_by ? el("code", {}, a.approved_by) : el("span", { class: "muted" }, "not yet"),
                  ),
                  el("td", {}, relativeInstant(a.expires_at, ctx.now())),
                  el(
                    "td",
                    {},
                    el(
                      "button",
                      {
                        type: "button",
                        class: "button quiet",
                        onclick: () => ctx.navigate(`actions?status=${a.status}&kind=${a.kind}`),
                      },
                      "Decide in queue",
                    ),
                  ),
                ),
              ),
            ),
      ),
    );
  };
}

/** What each kind on this surface demands, read from the authority document. */
function policyTable(ctx: ViewContext, kinds: readonly string[]): HTMLElement {
  const now = ctx.now();
  const rows = kinds.map((name) => {
    const spec = ctx.authority.kind(name);
    if (!spec) {
      return el("tr", {}, el("td", { colspan: "5" }, `${name} is not a declared kind.`));
    }
    const proposal = decideProposal(ctx.session.principal, name, now, ctx.authority);
    return el(
      "tr",
      {},
      el("td", {}, kindLabel(name)),
      el("td", {}, el("code", {}, spec.propose_permission)),
      el(
        "td",
        {},
        spec.approve_permission ? el("code", {}, spec.approve_permission) : el("span", { class: "muted" }, "single control"),
        spec.requires_break_glass_approval ? pill("break-glass only", "dual") : null,
      ),
      el("td", {}, formatDuration(spec.step_up_max_age_seconds)),
      el(
        "td",
        {},
        proposal.allowed
          ? pill("you can propose", "active")
          : el("span", { class: "muted", title: reasonText(proposal.reason) }, reasonText(proposal.reason)),
      ),
    );
  });
  return el(
    "div",
    {},
    table(["Kind", "Propose permission", "Approve permission", "Step-up window", "You"], rows),
    notice(
      "info",
      "These flows are controlled actions like any other: propose, then a different principal approves, then execute. The console links each one to the queue rather than offering a second, parallel path.",
    ),
    fields(
      field(
        "Where the effect happens",
        "Approving records authority. The effect is applied by the domain that owns it, which verifies the approval id names this exact kind and target before acting.",
      ),
    ),
  );
}
