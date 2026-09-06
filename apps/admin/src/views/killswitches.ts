/**
 * Kill switches: state, activation, release.
 *
 * The asymmetry is the point, and the console makes it visible rather than
 * uniform:
 *
 *   - **Activation is the fast path.** One operator holding `kill:activate`,
 *     no step-up, no approval. Stopping new risk must never wait on a second
 *     factor or a second person, so the console never adds a confirmation
 *     step that the server does not require.
 *   - **Release is the slow path.** `kill:release` plus a recent multi-factor
 *     sign-in, and for a SEVERE switch an APPROVED `KILL_SWITCH_RELEASE`
 *     admin action naming this exact switch. The severity comes from the
 *     generated authority document, so the form asks for the approval id
 *     exactly when the domain will demand it.
 *
 * A kill switch never stops reconciliation, settlement, ledger posting or
 * audit. It stops *new risk*. The console says so, because an operator who
 * believes a kill switch froze the ledger will make the wrong next decision.
 */
import { actOnKillSwitch, listKillSwitches } from "../api.ts";
import type { KillSwitch } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { allowedWrites } from "../decide.ts";
import { actionButton, append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm } from "../forms.ts";
import { formatInstant, reasonText } from "../format.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const GLOBAL_SCOPE = "*";

export async function renderKillSwitches(ctx: ViewContext, root: HTMLElement): Promise<void> {
  clear(root);

  let switches: KillSwitch[];
  try {
    switches = await listKillSwitches();
  } catch (err) {
    append(root, problemNotice(err, "kill switches"));
    return;
  }

  const active = switches.filter((s) => s.active);
  append(
    root,
    panel(
      "Kill switches",
      "Kill switches stop new risk. They never stop reconciliation, settlement, ledger posting or audit.",
      active.length > 0
        ? notice(
            "warn",
            `${active.length} switch(es) are active: ${active.map((s) => `${s.kind}/${s.scope_id}`).join(", ")}.`,
          )
        : notice("info", "No kill switch is currently active."),
      switches.length === 0
        ? emptyState("No kill switch rows exist yet. A switch row appears the first time one is activated.")
        : table(
            ["Kind", "Scope", "State", "Severity", "Activated", "Released", ""],
            switches.flatMap((s) => switchRows(ctx, s)),
          ),
    ),
    activatePanel(ctx),
  );
}

function switchRows(ctx: ViewContext, s: KillSwitch): HTMLElement[] {
  const severity = ctx.authority.killSwitchKind(s.kind);
  const needsApproval = severity?.release_needs_approval ?? s.severity === "SEVERE";
  const row = el(
    "tr",
    { class: s.active ? "blocking" : "" },
    el("td", {}, el("code", {}, s.kind)),
    el("td", {}, el("code", {}, s.scope_id)),
    el("td", {}, s.active ? pill("ACTIVE", "blocking") : pill("released", "inactive")),
    el(
      "td",
      {},
      pill(
        s.severity,
        s.severity.toLowerCase(),
        needsApproval ? "Releasing this needs an approved KILL_SWITCH_RELEASE action." : undefined,
      ),
    ),
    el("td", { class: "muted" }, s.activated_at ? formatInstant(s.activated_at) : "—"),
    el("td", { class: "muted" }, s.released_at ? formatInstant(s.released_at) : "—"),
    el("td", {}, releaseControl(ctx, s, needsApproval)),
  );

  const detail = el("tr", { class: "detail" });
  const cell = el("td", { colspan: "7" });
  append(
    cell,
    fields(
      field("Reason on record", s.reason ?? "—"),
      field(
        "Release policy",
        needsApproval
          ? "SEVERE: kill:release, a recent multi-factor sign-in, and an APPROVED KILL_SWITCH_RELEASE action naming this switch."
          : "STANDARD: kill:release and a recent multi-factor sign-in.",
      ),
    ),
  );
  append(detail, cell);
  return [row, detail];
}

function releaseControl(ctx: ViewContext, s: KillSwitch, needsApproval: boolean): HTMLElement {
  if (!s.active) {
    return actionButton({
      label: "Release",
      onClick: () => undefined,
      allowed: false,
      reason: "This switch is not active.",
    });
  }
  const permitted = allowedWrites(ctx.session.principal, "kill_switches", ctx.now(), ctx.authority);
  const allowed = permitted.includes("kill.release");
  return actionButton({
    label: "Release",
    variant: "primary",
    allowed,
    ...(allowed
      ? {}
      : { reason: `${reasonText("MISSING_PERMISSION")} Release needs kill:release and a recent multi-factor sign-in.` }),
    onClick: () => openReleaseForm(ctx, s, needsApproval),
  });
}

function openReleaseForm(ctx: ViewContext, s: KillSwitch, needsApproval: boolean): void {
  ctx.report(
    commandForm({
      title: `Release ${s.kind} / ${s.scope_id}`,
      description: "Releasing lets new risk resume for this scope.",
      submitLabel: "Release",
      variant: "primary",
      ...(needsApproval
        ? {
            warning: `This switch is SEVERE. The server requires an APPROVED KILL_SWITCH_RELEASE admin action whose target id is exactly "${s.kind}:${s.scope_id}", approved by someone other than its proposer.`,
          }
        : {}),
      fields: [
        {
          name: "reason",
          label: "Reason",
          multiline: true,
          minLength: ctx.authority.doc.min_reason_length,
          hint: `At least ${ctx.authority.doc.min_reason_length} characters. Recorded on the switch and in the audit stream.`,
        },
        ...(needsApproval
          ? [
              {
                name: "approval_id",
                label: "Approval id",
                hint: "The APPROVED KILL_SWITCH_RELEASE action naming this switch.",
              },
            ]
          : []),
      ],
      onSubmit: async (values, key) => {
        try {
          const approvalId = values["approval_id"] ?? "";
          const updated = await actOnKillSwitch(
            {
              kind: s.kind,
              scope_id: s.scope_id,
              action: "release",
              reason: values["reason"] ?? "",
              ...(approvalId ? { approval_id: approvalId } : {}),
            },
            key,
          );
          ctx.report(
            notice("info", `${updated.kind}/${updated.scope_id} is now ${updated.active ? "still ACTIVE" : "released"}.`),
          );
          ctx.refresh();
        } catch (err) {
          throw new Error(describeProblem(err));
        }
      },
    }),
  );
}

function activatePanel(ctx: ViewContext): HTMLElement {
  const permitted = allowedWrites(ctx.session.principal, "kill_switches", ctx.now(), ctx.authority);
  if (!permitted.includes("kill.activate")) {
    return panel(
      "Activate a kill switch",
      "Not available to you.",
      notice("info", `${reasonText("MISSING_PERMISSION")} Activation needs kill:activate.`),
    );
  }
  const kinds = ctx.authority.doc.kill_switch_kinds.map((k) => k.kind);
  return panel(
    "Activate a kill switch",
    "One operator, no second factor, no approval. Stopping new risk never waits.",
    commandForm({
      title: "Activate",
      description:
        "Takes effect immediately for the chosen scope. Reconciliation, settlement, ledger posting and audit continue regardless.",
      submitLabel: "Activate",
      variant: "danger",
      fields: [
        { name: "kind", label: "Kind", options: kinds, ...(kinds[0] ? { value: kinds[0] } : {}) },
        {
          name: "scope_id",
          label: "Scope",
          value: GLOBAL_SCOPE,
          hint: `"${GLOBAL_SCOPE}" is the whole platform for kinds that take a global scope; otherwise the exact account, agent, venue, chain or provider id.`,
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
        try {
          const updated = await actOnKillSwitch(
            {
              kind: values["kind"] ?? "",
              scope_id: values["scope_id"] ?? GLOBAL_SCOPE,
              action: "activate",
              reason: values["reason"] ?? "",
            },
            key,
          );
          ctx.report(
            notice("warn", `${updated.kind}/${updated.scope_id} is ACTIVE. New risk is stopped for that scope.`),
          );
          ctx.refresh();
        } catch (err) {
          throw new Error(describeProblem(err));
        }
      },
    }),
  );
}
