/**
 * Break-glass: request an elevation, grant one, and see what a live elevation
 * authorises.
 *
 * A break-glass grant is not a special mechanism bolted on beside dual
 * control — it *is* a controlled action (`BREAK_GLASS_GRANT`), proposed by one
 * principal holding `break_glass:request` and approved by a different one
 * holding `break_glass:approve`, with the same reason, expiry and audit trail
 * as any other. This surface therefore drives the ordinary action queue and
 * says so, rather than presenting a private back door.
 *
 * Two properties the console must not blur:
 *
 *   - the elevation is time-boxed, at most `break_glass.max_duration_seconds`,
 *     and it dies of the clock. There is no revoke call; expiry is the
 *     revocation;
 *   - a grant elevates exactly the user it names. Holding the grant record, or
 *     seeing it in the queue, elevates nobody.
 */
import { listActions, proposeAction } from "../api.ts";
import type { AdminAction } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { decideProposal } from "../decide.ts";
import { append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm, refusedCommand } from "../forms.ts";
import { formatDuration, formatInstant, reasonText, relativeInstant } from "../format.ts";
import { describeProblem, problemNotice } from "../problem.ts";

export async function renderBreakGlass(ctx: ViewContext, root: HTMLElement): Promise<void> {
  clear(root);
  const policy = ctx.authority.doc.break_glass;

  append(root, currentElevationPanel(ctx));

  append(
    root,
    panel(
      "What an elevation grants",
      `The ${policy.role} role carries exactly the approve side of every dual-control action, and nothing else. No standing role carries any of them.`,
      table(
        ["Permission", "Kind it approves"],
        policy.grants.map((permission) => {
          const kinds = ctx.authority
            .allKinds()
            .filter((k) => k.approve_permission === permission)
            .map((k) => k.kind);
          return el(
            "tr",
            {},
            el("td", {}, el("code", {}, permission)),
            el("td", {}, kinds.length > 0 ? kinds.join(", ") : el("span", { class: "muted" }, "—")),
          );
        }),
      ),
      notice(
        "info",
        `An elevation lasts at most ${formatDuration(policy.max_duration_seconds)} and expires on the clock. There is no revoke call: the deadline is the revocation, and every use of the elevation is audited by the package that consumes it.`,
      ),
    ),
  );

  append(root, requestPanel(ctx));
  await renderGrantQueue(ctx, root);
}

/**
 * What this session's elevation is. `GET /v1/me` reports the role but not the
 * deadline, so the console says exactly that instead of inventing a countdown.
 */
function currentElevationPanel(ctx: ViewContext): HTMLElement {
  const policy = ctx.authority.doc.break_glass;
  const held = ctx.session.principal.roles.includes(policy.role);
  if (!held) {
    return panel(
      "Your elevation",
      "You hold no break-glass elevation.",
      emptyState(
        `Without one you cannot approve any dual-control action. Ask a second operator to propose a ${policy.kind} naming you.`,
      ),
    );
  }
  return panel(
    "Your elevation",
    "This session carries a break-glass elevation.",
    notice(
      "warn",
      "The API reports the elevation but not its deadline (GET /v1/me does not return break_glass_until), so this console cannot show you how long is left. The server re-checks it on every approval and refuses once it has lapsed.",
    ),
    fields(
      field("Role", pill(policy.role, "dual")),
      field("Maximum duration of any single grant", formatDuration(policy.max_duration_seconds)),
      field("Grants", policy.grants.join(", ")),
    ),
  );
}

function requestPanel(ctx: ViewContext): HTMLElement {
  const policy = ctx.authority.doc.break_glass;
  const decision = decideProposal(ctx.session.principal, policy.kind, ctx.now(), ctx.authority);
  if (!decision.allowed) {
    return refusedCommand(
      "Request an elevation",
      `${reasonText(decision.reason)} Requesting one needs ${decision.permission ?? "the request permission"}${
        decision.stepUpMaxAgeSeconds
          ? ` and a multi-factor sign-in within the last ${formatDuration(decision.stepUpMaxAgeSeconds)}`
          : ""
      }.`,
    );
  }
  const spec = ctx.authority.kind(policy.kind);
  return panel(
    "Request an elevation",
    "This proposes an ordinary controlled action. A different principal must approve it, and a third step executes it to mint the grant.",
    commandForm({
      title: `Propose ${policy.kind}`,
      description:
        "The elevation goes to the user you name here, not to you. Naming yourself is allowed and still needs someone else to approve it.",
      submitLabel: "Propose",
      warning: `The proposal itself expires after ${spec ? formatDuration(spec.expiry_seconds) : "its configured window"} if nobody approves it.`,
      fields: [
        {
          name: "user_id",
          label: "User to elevate",
          placeholder: "users.id UUID",
          hint: "The grant elevates exactly this user. Nobody else is affected by it.",
        },
        {
          name: "scope",
          label: "Scope",
          placeholder: "incident id, kill switch, correction",
          hint: "Recorded verbatim on the grant. It is evidence, not a filter: the elevation is not narrowed by it.",
        },
        {
          name: "duration_seconds",
          label: "Duration (seconds)",
          value: String(Math.min(3600, policy.max_duration_seconds)),
          hint: `At most ${String(policy.max_duration_seconds)} (${formatDuration(policy.max_duration_seconds)}).`,
        },
        {
          name: "reason",
          label: "Reason",
          multiline: true,
          minLength: ctx.authority.doc.min_reason_length,
          hint: `At least ${ctx.authority.doc.min_reason_length} characters.`,
        },
      ],
      onSubmit: async (values, key) => {
        const duration = values["duration_seconds"] ?? "";
        if (!/^[0-9]+$/.test(duration)) {
          throw new Error("Duration must be a whole number of seconds.");
        }
        // Compared as digits, never parsed: the bound is a policy value and a
        // string comparison of equal-length digit strings is exact.
        if (duration.length > String(policy.max_duration_seconds).length ||
          (duration.length === String(policy.max_duration_seconds).length && duration > String(policy.max_duration_seconds))) {
          throw new Error(`Duration must be at most ${String(policy.max_duration_seconds)} seconds.`);
        }
        try {
          const created = await proposeAction(
            {
              kind: policy.kind,
              target_type: "user",
              target_id: values["user_id"] ?? "",
              reason: values["reason"] ?? "",
              params: {
                user_id: values["user_id"] ?? "",
                scope: values["scope"] ?? "",
                // The API takes a JSON number here; it is a duration in whole
                // seconds, not money, and the domain re-validates the bound.
                duration_seconds: durationAsJsonNumber(duration),
              },
            },
            key,
          );
          ctx.report(
            notice("info", `Proposed ${created.id}. A different principal must now approve it, and then execute it.`),
          );
          ctx.refresh();
        } catch (err) {
          throw new Error(describeProblem(err));
        }
      },
    }),
  );
}

/**
 * Durations are counts of seconds bounded by policy at four hours, so they are
 * exactly representable. This is the one place a JSON number is produced, and
 * it is deliberately not a money path: `money.ts` documents why financial
 * values never take this route.
 */
function durationAsJsonNumber(digits: string): number {
  let value = 0;
  for (const ch of digits) value = value * 10 + (ch.charCodeAt(0) - 48);
  return value;
}

async function renderGrantQueue(ctx: ViewContext, root: HTMLElement): Promise<void> {
  const policy = ctx.authority.doc.break_glass;
  let page;
  try {
    page = await listActions("", { limit: 100 });
  } catch (err) {
    append(root, problemNotice(err, "break-glass requests"));
    return;
  }
  const grants = page.items.filter((a: AdminAction) => a.kind === policy.kind);
  append(
    root,
    panel(
      "Elevation requests",
      "Every break-glass grant this deployment has recorded, in the same queue as any other controlled action.",
      grants.length === 0
        ? emptyState("No break-glass grants have been requested.")
        : table(
            ["Target user", "Status", "Requested by", "Approved by", "Expires", ""],
            grants.map((a) =>
              el(
                "tr",
                {},
                el("td", {}, el("code", {}, a.target_id)),
                el("td", {}, pill(a.status, a.status.toLowerCase())),
                el("td", {}, el("code", {}, a.proposed_by)),
                el(
                  "td",
                  {},
                  a.approved_by ? el("code", {}, a.approved_by) : el("span", { class: "muted" }, "not yet"),
                ),
                el("td", { title: formatInstant(a.expires_at) }, relativeInstant(a.expires_at, ctx.now())),
                el(
                  "td",
                  {},
                  el(
                    "button",
                    {
                      type: "button",
                      class: "button quiet",
                      onclick: () => ctx.navigate(`actions?status=${a.status}&kind=${policy.kind}`),
                    },
                    "Review in queue",
                  ),
                ),
              ),
            ),
          ),
    ),
  );
}
