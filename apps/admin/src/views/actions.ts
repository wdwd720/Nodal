/**
 * The dual-control queue: propose, review, approve, reject, execute.
 *
 * This is the surface Stage 15 exists for, and its whole design rule is that
 * an operator cannot get dual control wrong by accident. Concretely:
 *
 *   - the Approve control on an action you proposed is rendered `disabled`
 *     with "You proposed this action" attached. It is not hidden (which would
 *     look like a bug) and it is not merely styled to look inert (which a
 *     click would defeat) — the element cannot be activated at all;
 *   - every other refusal is shown the same way, with the reason the server
 *     would give, taken from the generated authority model rather than guessed;
 *   - the proposer is named on every row, so "different person" is a fact on
 *     screen and not a rule in a document;
 *   - an approval that needs a break-glass elevation says so before it is
 *     attempted, because obtaining one is itself a two-person process.
 */
import { ApiProblem, decideAction, isUnwired, listActions, proposeAction } from "../api.ts";
import type { AdminAction, DecisionVerb } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { decide, decideProposal } from "../decide.ts";
import type { DecideAction, Decision } from "../decide.ts";
import { actionButton, append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm } from "../forms.ts";
import { formatInstant, kindLabel, reasonText, relativeInstant } from "../format.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const STATUS_FILTERS = ["PROPOSED", "APPROVED", "REJECTED", "EXECUTED", "FAILED", "EXPIRED", "CANCELLED"] as const;

/**
 * The verbs offered against a stored action. `propose` is excluded because it
 * creates a record rather than acting on one; `cancel` is included because the
 * decision logic has a verdict for it and hiding the control would misrepresent
 * a proposer's standing — but it has no HTTP route, which the handler says.
 */
const ROW_VERBS = ["approve", "reject", "execute", "cancel"] as const;
type RowVerb = (typeof ROW_VERBS)[number];

/**
 * The API's AdminAction as the decision logic wants it.
 *
 * `targetId` is not optional in practice. For a kind whose spec sets
 * `approver_is_not_target` — BREAK_GLASS_GRANT is the one that matters — the
 * target names the person the action elevates, and that person may not supply
 * the second signature on their own elevation. `decide.ts` implements that
 * check and `decisions.json` proves it; this mapping is what feeds it, and
 * omitting the field here reproduced F-71 one layer up: the queue rendered a
 * live Approve button to the grantee of their own grant, which the server then
 * refuses. The console must not offer what the server will refuse.
 */
export function toDecideAction(a: AdminAction): DecideAction {
  return {
    kind: a.kind,
    status: a.status,
    requiresDual: a.requires_dual,
    proposedBy: a.proposed_by,
    approvedBy: a.approved_by ?? null,
    targetId: a.target_id,
    expiresAt: a.expires_at,
  };
}

export async function renderActions(ctx: ViewContext, root: HTMLElement): Promise<void> {
  const params = new URLSearchParams(window.location.hash.split("?")[1] ?? "");
  const status = params.get("status") ?? "PROPOSED";
  const kindFilter = params.get("kind") ?? "";

  clear(root);
  append(root, filterBar(ctx, status, kindFilter));

  let page;
  try {
    page = await listActions(status === "ALL" ? "" : status, { limit: 100 });
  } catch (err) {
    append(root, problemNotice(err, "the action queue"));
    return;
  }

  const items = kindFilter ? page.items.filter((a) => a.kind === kindFilter) : page.items;
  append(root, proposePanel(ctx, kindFilter));

  if (items.length === 0) {
    append(
      root,
      panel(
        "Queue",
        `No ${status === "ALL" ? "" : `${status.toLowerCase()} `}actions${kindFilter ? ` of kind ${kindLabel(kindFilter)}` : ""}.`,
        emptyState("Nothing here. This is the queue as the server reports it, not a loading state."),
      ),
    );
    return;
  }

  const rows = items.flatMap((a) => actionRows(ctx, a));
  append(root, panel("Queue", `${items.length} action(s).`, table(
    ["Kind", "Target", "Status", "Proposed by", "Approved by", "Expires", "Available to you"],
    rows,
  )));
}

function filterBar(ctx: ViewContext, status: string, kind: string): HTMLElement {
  const bar = el("div", { class: "filter-bar" });
  append(bar, el("span", { class: "muted" }, "Status"));
  for (const s of ["PROPOSED", ...STATUS_FILTERS.filter((x) => x !== "PROPOSED"), "ALL"]) {
    append(
      bar,
      el(
        "button",
        {
          type: "button",
          class: `chip${s === status ? " on" : ""}`,
          onclick: () => ctx.navigate(`actions?status=${s}${kind ? `&kind=${kind}` : ""}`),
        },
        s,
      ),
    );
  }
  if (kind) {
    append(
      bar,
      el(
        "button",
        { type: "button", class: "chip on", onclick: () => ctx.navigate(`actions?status=${status}`) },
        `kind: ${kindLabel(kind)} x`,
      ),
    );
  }
  return bar;
}

/** The propose form, offering only the kinds this principal may propose. */
function proposePanel(ctx: ViewContext, presetKind: string): HTMLElement {
  const now = ctx.now();
  const proposable = ctx.authority
    .allKinds()
    .map((k) => ({ kind: k, decision: decideProposal(ctx.session.principal, k.kind, now, ctx.authority) }));
  const allowed = proposable.filter((p) => p.decision.allowed);

  if (allowed.length === 0) {
    const blockers = new Map<string, string>();
    for (const p of proposable) blockers.set(p.decision.reason, reasonText(p.decision.reason));
    return panel(
      "Propose an action",
      "None of the controlled action kinds is available to you.",
      el("ul", { class: "reasons" }, ...[...blockers.values()].map((r) => el("li", {}, r))),
    );
  }

  const kinds = allowed.map((p) => p.kind.kind);
  const initial = presetKind && kinds.includes(presetKind) ? presetKind : kinds[0];
  const form = commandForm({
    title: "Propose an action",
    description:
      "The reason is recorded on the action, in its transition history and in the audit stream. It is not a comment.",
    submitLabel: "Propose",
    fields: [
      { name: "kind", label: "Kind", options: kinds, ...(initial ? { value: initial } : {}) },
      { name: "target_type", label: "Target type", placeholder: "kill_switch, capability_gate, user, ..." },
      { name: "target_id", label: "Target id", placeholder: "the exact record this action names" },
      {
        name: "reason",
        label: "Reason",
        multiline: true,
        minLength: ctx.authority.doc.min_reason_length,
        hint: `At least ${ctx.authority.doc.min_reason_length} characters. Recorded permanently.`,
      },
      {
        name: "params",
        label: "Params (JSON object)",
        multiline: true,
        required: false,
        placeholder: "{}",
        hint: "Typed by the kind. Hashed at proposal and re-checked before execution, so it cannot be edited later.",
      },
    ],
    onSubmit: async (values, key) => {
      let parsed: Record<string, unknown> | undefined;
      const raw = values["params"] ?? "";
      if (raw !== "") {
        let candidate: unknown;
        try {
          candidate = JSON.parse(raw);
        } catch {
          throw new Error("Params must be a JSON object.");
        }
        if (typeof candidate !== "object" || candidate === null || Array.isArray(candidate)) {
          throw new Error("Params must be a JSON object.");
        }
        parsed = candidate as Record<string, unknown>;
      }
      const created = await proposeAction(
        {
          kind: values["kind"] ?? "",
          target_type: values["target_type"] ?? "",
          target_id: values["target_id"] ?? "",
          reason: values["reason"] ?? "",
          ...(parsed ? { params: parsed } : {}),
        },
        key,
      );
      ctx.report(
        notice(
          "info",
          `Proposed ${kindLabel(created.kind)} ${created.id}. It now needs a different person to approve it.`,
        ),
      );
      ctx.refresh();
    },
  });

  const unavailable = proposable.filter((p) => !p.decision.allowed);
  return panel(
    "Propose an action",
    "Only the kinds your roles can propose are listed.",
    form,
    unavailable.length > 0
      ? el(
          "details",
          { class: "muted" },
          el("summary", {}, `${unavailable.length} kind(s) you cannot propose`),
          el(
            "ul",
            { class: "reasons" },
            ...unavailable.map((p) => el("li", {}, `${kindLabel(p.kind.kind)} — ${reasonText(p.decision.reason)}`)),
          ),
        )
      : null,
  );
}

function actionRows(ctx: ViewContext, a: AdminAction): HTMLElement[] {
  const now = ctx.now();
  const decideView = toDecideAction(a);
  const me = ctx.session.principal.subjectId;
  const mine = a.proposed_by === me;

  const row = el("tr", { class: mine ? "mine" : "" });
  append(
    row,
    el("td", {}, el("button", {
      type: "button",
      class: "linky",
      onclick: () => ctx.navigate(`actions?status=${a.status}&kind=${a.kind}`),
    }, kindLabel(a.kind)), a.requires_dual ? pill("dual control", "dual", "Two distinct principals are required.") : pill("single", "single")),
    el("td", {}, el("code", {}, `${a.target_type}/${a.target_id}`)),
    el("td", {}, pill(a.status, a.status.toLowerCase())),
    el("td", {}, el("code", {}, a.proposed_by), mine ? pill("you", "you") : null),
    el("td", {}, a.approved_by ? el("code", {}, a.approved_by) : el("span", { class: "muted" }, "—")),
    el("td", { title: formatInstant(a.expires_at) }, relativeInstant(a.expires_at, now)),
  );

  const controls = el("td", { class: "controls" });
  for (const verb of ROW_VERBS) {
    const decision = decide(ctx.session.principal, decideView, verb, now, ctx.authority);
    append(controls, verbButton(ctx, a, verb, decision));
  }
  append(row, controls);

  const detail = el("tr", { class: "detail" });
  const cell = el("td", { colspan: "7" });
  append(
    cell,
    fields(
      field("Reason", a.reason ?? "—"),
      field("Proposed", formatInstant(a.proposed_at)),
      field("Approved", a.approved_at ? formatInstant(a.approved_at) : "not yet"),
      field("Executed", a.executed_at ? formatInstant(a.executed_at) : "not yet"),
      field("Params hash", el("code", {}, a.params_hash ?? "—")),
      a.params ? field("Params", el("pre", {}, JSON.stringify(a.params, null, 2))) : null,
      elevationNote(ctx, a),
    ),
  );
  append(detail, cell);
  return [row, detail];
}

/** Says, before it is attempted, when an approval needs a break-glass grant. */
function elevationNote(ctx: ViewContext, a: AdminAction): HTMLElement | null {
  const spec = ctx.authority.kind(a.kind);
  if (!spec || !spec.requires_break_glass_approval || a.status !== "PROPOSED") return null;
  const held = ctx.session.principal.roles.includes(ctx.authority.doc.break_glass.role);
  return field(
    "Approval authority",
    held
      ? "You hold a break-glass elevation. The server confirms it is still live when you approve."
      : `Approving this needs a live ${ctx.authority.doc.break_glass.role} elevation, which no standing role carries. Request one first.`,
  );
}

function verbButton(ctx: ViewContext, a: AdminAction, verb: RowVerb, decision: Decision): HTMLElement {
  const label = verb.charAt(0).toUpperCase() + verb.slice(1);
  const variant = verb === "approve" ? "primary" : verb === "reject" ? "danger" : "quiet";
  return actionButton({
    label,
    variant,
    allowed: decision.allowed,
    reason: reasonText(decision.reason),
    testId: `${verb}-${a.id}`,
    onClick: () => openDecisionForm(ctx, a, verb, decision),
  });
}

function openDecisionForm(ctx: ViewContext, a: AdminAction, verb: RowVerb, decision: Decision): void {
  if (verb === "cancel") {
    // Cancel is a proposer withdrawing their own request; the API models it as
    // a rejection by the proposer, which internal/admin refuses. There is no
    // cancel route on the HTTP surface, so say so rather than offer a button
    // that cannot work.
    ctx.report(
      notice(
        "warn",
        "Withdrawing your own proposal exists in the domain (admin.Cancel) but has no HTTP route yet, so this console cannot do it. Reject it instead, or let it expire.",
      ),
    );
    return;
  }
  // Bound to a const so the narrowing survives into `onSubmit`: the only three
  // verbs that reach the wire are the three that are routable path segments.
  const wire: DecisionVerb = verb;
  const needsReason = verb === "reject";
  const form = commandForm({
    title: `${verb === "approve" ? "Approve" : verb === "reject" ? "Reject" : "Execute"} ${kindLabel(a.kind)}`,
    description:
      verb === "approve"
        ? "You are the second person on this action. Your subject id is recorded as the approver."
        : verb === "reject"
          ? "The rejection reason is recorded permanently on the action."
          : "Executing applies the action's effect. The stored params are re-hashed first; a mismatch refuses.",
    submitLabel: verb === "approve" ? "Approve" : verb === "reject" ? "Reject" : "Execute",
    variant: verb === "reject" ? "danger" : "primary",
    ...(decision.elevationUnverified
      ? {
          warning:
            "This approval rests on a break-glass elevation whose deadline this console cannot see. The server checks it; if it has expired you will be refused.",
        }
      : {}),
    fields: [
      {
        name: "note",
        label: needsReason ? "Rejection reason" : "Note",
        multiline: true,
        required: needsReason,
        ...(needsReason ? { minLength: ctx.authority.doc.min_reason_length } : {}),
        hint: needsReason
          ? `At least ${ctx.authority.doc.min_reason_length} characters. Recorded on the action.`
          : "Optional. Recorded on the action.",
      },
    ],
    onSubmit: async (values, key) => {
      try {
        const updated = await decideAction(a.id, wire, values["note"] ?? "", key);
        ctx.report(notice("info", `${kindLabel(a.kind)} ${updated.id} is now ${updated.status}.`));
        ctx.refresh();
      } catch (err) {
        if (verb === "execute" && isUnwired(err)) {
          ctx.report(
            notice(
              "warn",
              `This deployment cannot execute ${kindLabel(a.kind)} actions over the API: ${
                err instanceof ApiProblem ? (err.detail ?? err.code) : String(err)
              } The action stays approved until an operator runs its own tool.`,
            ),
          );
          return;
        }
        throw new Error(describeProblem(err));
      }
    },
  });
  ctx.report(form);
}
