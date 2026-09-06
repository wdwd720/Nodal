/**
 * Capability gates: view state, propose activation, approve, activate,
 * suspend, resume, revoke.
 *
 * The property this surface must not obscure: **no single environment
 * variable enables live money.** Configuration is condition 1 of the five
 * `internal/gates.Checker` evaluates; the other four are the persisted row's
 * state, the effective window, the required evidence references, and two
 * distinct approvers neither of whom proposed. The console therefore renders
 * `active` (the verdict) separately from `state` (the row), and shows the
 * `inactive_reason` the server computed rather than inferring one.
 *
 * All live capability gates default DISABLED. An empty list is not an error
 * and not a failure to load: it means no gate row exists yet, which is the
 * safest possible state and is labelled as such.
 */
import { actOnGate, isCapability, listGates } from "../api.ts";
import type { Capability, CapabilityGate, GateActionName } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { allowedWrites } from "../decide.ts";
import { actionButton, append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm } from "../forms.ts";
import { formatInstant, reasonText } from "../format.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const ACTIONS: readonly GateActionName[] = ["propose", "approve", "activate", "suspend", "resume", "revoke"];

export async function renderGates(ctx: ViewContext, root: HTMLElement): Promise<void> {
  clear(root);

  let gates: CapabilityGate[];
  try {
    gates = await listGates();
  } catch (err) {
    append(root, problemNotice(err, "capability gates"));
    return;
  }

  // Keyed by plain string: the declared list comes from the authority
  // document, so a name the API contract does not know must still be able to
  // miss this map rather than fail to compile against it.
  const known = new Map<string, CapabilityGate>(gates.map((g) => [g.capability, g] as const));
  const declared = ctx.authority.doc.capabilities;

  append(
    root,
    panel(
      "Capability gates",
      "A capability is live only when all five conditions hold. Configuration alone is one of them, so no environment variable can enable live money on its own.",
      notice(
        "info",
        `${gates.length} of ${declared.length} declared capabilities have a stored gate row. A capability with no row is DISABLED, which is the default and the safe state.`,
      ),
      table(
        ["Capability", "Row state", "Active now", "Why not", "Approval version", "Window", ""],
        declared.flatMap((capability) => gateRows(ctx, capability, known.get(capability))),
      ),
    ),
  );
}

function gateRows(ctx: ViewContext, capability: string, gate: CapabilityGate | undefined): HTMLElement[] {
  if (!gate) {
    return [
      el(
        "tr",
        {},
        el("td", {}, el("code", {}, capability)),
        el("td", {}, pill("DISABLED", "disabled", "No stored gate row exists.")),
        el("td", {}, pill("no", "inactive")),
        el("td", { class: "muted" }, "No gate row has ever been created for this capability."),
        el("td", { class: "muted" }, "—"),
        el("td", { class: "muted" }, "—"),
        el("td", {}, controls(ctx, capability, undefined)),
      ),
    ];
  }
  const row = el(
    "tr",
    {},
    el("td", {}, el("code", {}, capability)),
    el("td", {}, pill(gate.state, gate.state.toLowerCase())),
    el("td", {}, gate.active ? pill("ACTIVE", "active") : pill("no", "inactive")),
    el("td", { class: "muted" }, gate.inactive_reason ?? (gate.active ? "—" : "not reported")),
    el("td", {}, String(gate.approval_version)),
    el(
      "td",
      { class: "muted" },
      `${gate.effective_at ? formatInstant(gate.effective_at) : "—"} → ${gate.expires_at ? formatInstant(gate.expires_at) : "—"}`,
    ),
    el("td", {}, controls(ctx, capability, gate)),
  );

  const detail = el("tr", { class: "detail" });
  const cell = el("td", { colspan: "7" });
  append(
    cell,
    fields(
      field("Environment", gate.environment),
      field("Legal review", gate.legal_review_ref ?? "not recorded"),
      field("Provider contract", gate.provider_contract_ref ?? "not recorded"),
      field("Risk approval", gate.risk_approval_ref ?? "not recorded"),
      field("Security approval", gate.security_approval_ref ?? "not recorded"),
      field(
        "Approvers",
        gate.approvers && gate.approvers.length > 0
          ? el("pre", {}, JSON.stringify(gate.approvers, null, 2))
          : "none recorded — activation needs two distinct principals, neither of whom proposed",
      ),
    ),
  );
  append(detail, cell);
  return [row, detail];
}

function controls(ctx: ViewContext, capability: string, gate: CapabilityGate | undefined): HTMLElement {
  const now = ctx.now();
  const permitted = new Set(allowedWrites(ctx.session.principal, "gates", now, ctx.authority));
  const box = el("div", { class: "controls" });
  // The authority document and the OpenAPI contract are generated from the
  // same Go source, so this normally holds. If it ever does not, the honest
  // answer is a refused control naming the drift, not a request the server
  // would reject with 400.
  const routable = isCapability(capability);
  for (const action of ACTIONS) {
    const writeId = `gate.${action}`;
    const spec = ctx.authority.gateAction(action);
    const allowed = routable && permitted.has(writeId);
    const needsElevation = spec ? ctx.authority.isDualControl(spec.permission) : false;
    const refusal = !routable
      ? `This console's API contract does not list ${capability} as a gated capability, so it cannot address this gate. The console and the API are out of step; regenerate the client.`
      : `${reasonText("MISSING_PERMISSION")} This step needs ${spec?.permission ?? "an unknown permission"}${
          needsElevation ? ", which only a live break-glass elevation carries" : ""
        }.`;
    append(
      box,
      actionButton({
        label: action,
        variant: action === "revoke" || action === "suspend" ? "danger" : "quiet",
        allowed,
        ...(allowed ? {} : { reason: refusal }),
        onClick: () => {
          if (routable) openGateForm(ctx, capability, action, gate);
        },
      }),
    );
  }
  return box;
}

function openGateForm(
  ctx: ViewContext,
  capability: Capability,
  action: GateActionName,
  gate: CapabilityGate | undefined,
): void {
  const evidence = action === "propose";
  ctx.report(
    commandForm({
      title: `${action} ${capability}`,
      description:
        action === "propose"
          ? "A proposal carries the evidence references the activation check requires. Missing evidence keeps the gate inactive however many people approve it."
          : "The reason is recorded on the gate transition and in the audit stream.",
      submitLabel: action,
      variant: action === "revoke" || action === "suspend" ? "danger" : "primary",
      ...(action === "activate"
        ? {
            warning:
              "Activating a live-money capability is the last step before real funds can move. The server still refuses unless all five conditions hold.",
          }
        : {}),
      fields: [
        {
          name: "reason",
          label: "Reason",
          multiline: true,
          minLength: ctx.authority.doc.min_reason_length,
          hint: `At least ${ctx.authority.doc.min_reason_length} characters.`,
        },
        ...(evidence
          ? [
              { name: "legal_review_ref", label: "Legal review reference", required: false, value: gate?.legal_review_ref ?? "" },
              {
                name: "provider_contract_ref",
                label: "Provider contract reference",
                required: false,
                value: gate?.provider_contract_ref ?? "",
              },
              { name: "risk_approval_ref", label: "Risk approval reference", required: false, value: gate?.risk_approval_ref ?? "" },
              {
                name: "security_approval_ref",
                label: "Security approval reference",
                required: false,
                value: gate?.security_approval_ref ?? "",
              },
            ]
          : []),
      ],
      onSubmit: async (values, key) => {
        try {
          const body = {
            reason: values["reason"] ?? "",
            ...(values["legal_review_ref"] ? { legal_review_ref: values["legal_review_ref"] } : {}),
            ...(values["provider_contract_ref"] ? { provider_contract_ref: values["provider_contract_ref"] } : {}),
            ...(values["risk_approval_ref"] ? { risk_approval_ref: values["risk_approval_ref"] } : {}),
            ...(values["security_approval_ref"] ? { security_approval_ref: values["security_approval_ref"] } : {}),
          };
          const updated = await actOnGate(capability, action, body, key);
          ctx.report(
            notice(
              "info",
              `${capability} is now ${updated.state}${updated.active ? " and ACTIVE" : " and not active"}${
                updated.inactive_reason ? `: ${updated.inactive_reason}` : "."
              }`,
            ),
          );
          ctx.refresh();
        } catch (err) {
          throw new Error(describeProblem(err));
        }
      },
    }),
  );
}

/** Exposed for the shell's empty-state copy. */
export function gatesEmptyState(): HTMLElement {
  return emptyState("No capability gate rows exist. Every capability is therefore DISABLED.");
}
