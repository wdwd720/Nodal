/**
 * Reconciliation (PART 163): list open and material mismatches, inspect the
 * evidence behind each, resolve one, escalate one.
 *
 * Three facts shape this surface, and the console states all three rather than
 * implying otherwise:
 *
 *   1. An agent may never resolve a reconciliation. It is refused in Go
 *      (reconciliation.Engine.humanActor) and again by the database
 *      (reconciliation_records.resolved_by_actor_type). This console is an
 *      operator tool and shows no agent path at all.
 *   2. A material mismatch needs an approved RECONCILIATION_RESOLVE_MATERIAL
 *      admin action naming this exact record, approved by someone other than
 *      the resolver. The form demands the approval id up front and links to
 *      the queue, rather than letting the operator write a resolution that is
 *      going to be refused.
 *   3. Investigate and Escalate exist in the domain (reconciliation.Resolver)
 *      but have no HTTP route. They are shown as unavailable with that reason,
 *      not as buttons that fail.
 *
 * Nothing here edits a balance. A repair is a compensating journal transaction
 * posted by internal/ledger and referenced from the record.
 *
 * `ReconciliationResolution` also accepts a `compensation` object — a
 * reason-coded set of double-entry postings. This console deliberately does not
 * offer a form for it. A grid of account codes, sides and quantities is a
 * balance-edit-shaped control however it is labelled, and the one thing this
 * surface must never look like is a place where an operator types a number and
 * a balance changes. A correction that needs postings is composed by the
 * ledger's own tooling, under `ledger:post_correction` and
 * `ledger:approve_correction`, and shows up here as the compensating
 * transaction id on the record.
 */
import { listReconciliationRecords, resolveReconciliationRecord } from "../api.ts";
import type { ReconciliationRecord } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { allowedWrites } from "../decide.ts";
import { actionButton, append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm, refusedCommand } from "../forms.ts";
import { formatInstant, reasonText, titleCase } from "../format.ts";
import { renderEvidenceValue } from "../money.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const OPEN_STATUSES = ["OPEN", "MISMATCH", "INVESTIGATING", "ESCALATED"] as const;

export async function renderReconciliation(ctx: ViewContext, root: HTMLElement): Promise<void> {
  const params = new URLSearchParams(window.location.hash.split("?")[1] ?? "");
  const status = params.get("status") ?? "";
  const accountId = params.get("account_id") ?? "";

  clear(root);
  append(root, filterBar(ctx, status, accountId));

  let page;
  try {
    page = await listReconciliationRecords(status, accountId, { limit: 100 });
  } catch (err) {
    append(
      root,
      problemNotice(err, "reconciliation records"),
      notice(
        "info",
        "This is the deployment's state, not an empty result set: cmd/api starts with no reconciliation engine wired (Reconcile: nil), so the endpoint answers 422 UNSUPPORTED. Mismatches are still being recorded by the reconciliation worker; they are simply not readable over the API yet.",
      ),
    );
    return;
  }

  const records = page.items;
  const material = records.filter((r) => r.material);
  const blocking = records.filter((r) => r.blocks_new_risk);

  append(
    root,
    panel(
      "Open mismatches",
      `${records.length} record(s); ${material.length} material; ${blocking.length} currently blocking new risk.`,
      records.length === 0
        ? emptyState("No records match this filter. The server returned an empty page.")
        : table(
            ["Kind", "Scope", "Status", "Material", "Blocks new risk", "Opened", ""],
            records.flatMap((r) => recordRows(ctx, r)),
          ),
    ),
  );
}

function filterBar(ctx: ViewContext, status: string, accountId: string): HTMLElement {
  const bar = el("div", { class: "filter-bar" });
  append(bar, el("span", { class: "muted" }, "Status"));
  for (const s of ["", ...OPEN_STATUSES, "RESOLVED_MANUAL", "RESOLVED_AUTOMATIC", "MATCHED"]) {
    append(
      bar,
      el(
        "button",
        {
          type: "button",
          class: `chip${s === status ? " on" : ""}`,
          onclick: () => ctx.navigate(`reconciliation?status=${s}${accountId ? `&account_id=${accountId}` : ""}`),
        },
        s === "" ? "ANY" : s,
      ),
    );
  }
  return bar;
}

function recordRows(ctx: ViewContext, r: ReconciliationRecord): HTMLElement[] {
  const row = el(
    "tr",
    { class: r.material ? "material" : "" },
    el("td", {}, r.kind, el("br"), el("span", { class: "muted" }, r.mode)),
    el("td", {}, el("code", {}, `${r.scope_type}/${r.scope_id}`)),
    el("td", {}, pill(r.status, r.status.toLowerCase())),
    el("td", {}, r.material ? pill("material", "material", "A human must resolve this, under dual control.") : el("span", { class: "muted" }, "no")),
    el("td", {}, r.blocks_new_risk ? pill("blocking", "blocking") : el("span", { class: "muted" }, "no")),
    el("td", { title: formatInstant(r.opened_at) }, formatInstant(r.opened_at)),
    el("td", {}),
  );

  const detail = el("tr", { class: "detail" });
  const cell = el("td", { colspan: "7" });
  append(cell, evidencePanel(r), resolutionPanel(ctx, r), unroutedActions());
  append(detail, cell);
  return [row, detail];
}

/**
 * The evidence: internal expected truth, external observed truth, and the
 * difference the engine computed. Values are shown as they arrived. A value
 * that reached the console as a JSON number rather than an exact decimal
 * string is marked, because it may already have lost digits before this code
 * ever saw it.
 */
function evidencePanel(r: ReconciliationRecord): HTMLElement {
  const documents: Array<[string, Record<string, unknown> | undefined]> = [
    ["Expected (internal)", r.expected as Record<string, unknown> | undefined],
    ["Observed (external)", r.observed as Record<string, unknown> | undefined],
    ["Difference", r.difference as Record<string, unknown> | undefined],
  ];
  const columns = documents.map(([title, doc]) => {
    if (!doc || Object.keys(doc).length === 0) {
      return el("div", { class: "evidence" }, el("h4", {}, title), emptyState("Not recorded."));
    }
    const rows = Object.entries(doc).map(([key, value]) => {
      const rendered = renderEvidenceValue(value);
      return field(key, rendered.text, { exact: rendered.exact, raw: rendered.text });
    });
    return el("div", { class: "evidence" }, el("h4", {}, title), fields(...rows));
  });
  return panel(
    "Evidence",
    "Exactly what the engine compared. Nothing here is recomputed by this console.",
    el("div", { class: "evidence-grid" }, ...columns),
    fields(
      field("Record", el("code", {}, r.id)),
      field("Account", r.account_id ? el("code", {}, r.account_id) : "—"),
      field("Asset", r.asset_id ? el("code", {}, r.asset_id) : "—"),
      field("Resolved", r.resolved_at ? formatInstant(r.resolved_at) : "not yet"),
      field("Resolution reason", r.resolution_reason ?? "—"),
      field("Evidence reference", r.resolution_evidence_ref ?? "—"),
      field(
        "Compensating journal transaction",
        r.compensating_journal_transaction_id
          ? el("code", {}, r.compensating_journal_transaction_id)
          : "none — no financial repair was posted",
      ),
    ),
  );
}

function resolutionPanel(ctx: ViewContext, r: ReconciliationRecord): HTMLElement {
  const now = ctx.now();
  const writes = allowedWrites(ctx.session.principal, "reconciliation", now, ctx.authority);
  const surface = ctx.authority.surface("reconciliation");
  const write = surface?.writes?.find((w) => w.id === "reconciliation.resolve");
  const terminal = r.status.startsWith("RESOLVED") || r.status === "MATCHED";

  if (terminal) {
    return panel("Resolution", "This record is already resolved.", emptyState("No further action is available."));
  }
  if (!writes.includes("reconciliation.resolve")) {
    const missing = write?.any_of.join(", ") ?? "reconciliation:resolve";
    return refusedCommand(
      "Resolve",
      `Resolving needs ${missing}${write?.step_up_max_age_seconds ? " and a recent multi-factor sign-in" : ""}. ${reasonText("MISSING_PERMISSION")}`,
    );
  }

  const materialFields = r.material
    ? [
        {
          name: "approval_id",
          label: "Approval id",
          placeholder: "the APPROVED RECONCILIATION_RESOLVE_MATERIAL action naming this record",
          hint: "A material mismatch needs a dual-controlled approval that names this record id, approved by someone other than you.",
        },
      ]
    : [];

  return panel(
    "Resolution",
    r.material
      ? "Material: a second person must have approved a RECONCILIATION_RESOLVE_MATERIAL action naming this record."
      : "Not material: a reason and an evidence reference are enough.",
    r.material
      ? notice(
          "warn",
          `Propose the approval first from the action queue, with target type "reconciliation_record" and target id ${r.id}.`,
        )
      : null,
    commandForm({
      title: "Resolve this mismatch",
      description:
        "Resolution records who, why and on what evidence. It never edits a balance: a financial repair is a compensating journal transaction posted by the ledger.",
      submitLabel: "Resolve",
      fields: [
        {
          name: "reason",
          label: "Reason",
          multiline: true,
          minLength: ctx.authority.doc.min_reason_length,
          hint: `At least ${ctx.authority.doc.min_reason_length} characters. Recorded on the record and in the audit stream.`,
        },
        {
          name: "evidence_ref",
          label: "Evidence reference",
          placeholder: "incident id, statement, explorer link",
          hint: "Required. What a later reader would need to check your conclusion.",
        },
        ...materialFields,
      ],
      onSubmit: async (values, key) => {
        try {
          const approvalId = values["approval_id"] ?? "";
          const updated = await resolveReconciliationRecord(
            r.id,
            {
              reason: values["reason"] ?? "",
              evidence_ref: values["evidence_ref"] ?? "",
              ...(approvalId ? { approval_id: approvalId } : {}),
            },
            key,
          );
          ctx.report(notice("info", `Record ${updated.id} is now ${updated.status}.`));
          ctx.refresh();
        } catch (err) {
          throw new Error(describeProblem(err));
        }
      },
    }),
  );
}

/**
 * Investigate and Escalate are real domain operations with no HTTP route. A
 * console that rendered them would be rendering dead buttons.
 */
function unroutedActions(): HTMLElement {
  const reason =
    "The domain supports this (reconciliation.Resolver.Investigate / .Escalate) but the API exposes no route for it, so this console cannot perform it.";
  return panel(
    "Investigate and escalate",
    "Not reachable from this console.",
    el(
      "div",
      { class: "controls" },
      actionButton({ label: "Investigate", onClick: () => undefined, allowed: false, reason }),
      actionButton({ label: "Escalate", onClick: () => undefined, allowed: false, reason }),
    ),
    el("p", { class: "muted" }, `${titleCase("api gap")}: ${reason}`),
  );
}
