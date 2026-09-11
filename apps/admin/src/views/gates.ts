/**
 * Capability gates: view state, propose activation, approve, activate,
 * suspend, resume, revoke — and, on a sandbox tier, sandbox and unsandbox.
 *
 * The property this surface must not obscure: **no single environment
 * variable enables live money.** Configuration is condition 1 of the five
 * `internal/gates.Checker` evaluates; the other four are the persisted row's
 * state, the effective window, the required evidence references, and two
 * distinct approvers neither of whom proposed. The console therefore renders
 * `active` (the verdict) separately from `state` (the row), and shows the
 * `inactive_reason` the server computed rather than inferring one.
 *
 * SANDBOX (ADR-0023) is the state that most needs not to be misread, so it is
 * rendered as what it is and never as what it resembles:
 *
 *   - it is **not on the path to ACTIVE**. A gate enters it from DISABLED,
 *     REVOKED or EXPIRED and leaves it only to DISABLED or REVOKED. The real
 *     ceremony still starts from DISABLED;
 *   - it carries **no approval chain, no evidence references and no validity
 *     window**, by construction: `cp_gate_sandbox` writes its own history row
 *     and touches none of them;
 *   - it is active **only for this deployment**. Anywhere that is not a
 *     sandbox tier the same row evaluates inactive with the server's own
 *     reason, and PROD cannot hold such a row at all.
 *
 * So a sandbox verdict is never painted in the colour an approval gets. It has
 * its own pill, reading "sandbox — not an approval", and the word "ACTIVE" is
 * reserved for a gate that came through dual control. When the API reports
 * `active` on a SANDBOX row without the `sandbox` flag, the console fails
 * towards "not an approval" and says the flag was missing, because the only
 * mistake worth preventing here is showing a rehearsal as an authorisation.
 *
 * All live capability gates default DISABLED. An empty list is not an error
 * and not a failure to load: it means no gate row exists yet, which is the
 * safest possible state and is labelled as such.
 */
import { actOnGate, isCapability, listGates, SANDBOX_GATE_ACTIONS } from "../api.ts";
import type { Capability, CapabilityGate, GateActionName } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { allowedWrites } from "../decide.ts";
import { actionButton, append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm } from "../forms.ts";
import { formatInstant, reasonText } from "../format.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const ACTIONS: readonly GateActionName[] = [
  "propose",
  "approve",
  "activate",
  "suspend",
  "resume",
  "revoke",
  ...SANDBOX_GATE_ACTIONS,
];

/** The row states `cp_gate_sandbox` accepts as a source of SANDBOX. */
const SANDBOX_SOURCES: readonly string[] = ["DISABLED", "REVOKED", "EXPIRED"];

/**
 * The SYSTEM actor a boot-time sandbox activation is recorded under
 * (`internal/gates.bootstrapActorID`). There is no person to name: the
 * authority is the blueprint line listing the capability in
 * `CP_API_SANDBOX_GATES`, reviewed like every other line there.
 */
const BOOTSTRAP_ACTOR = "config:CP_API_SANDBOX_GATES";

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
  const sandboxed = gates.filter((g) => g.state === "SANDBOX");

  append(
    root,
    panel(
      "Capability gates",
      "A capability is live only when all five conditions hold. Configuration alone is one of them, so no environment variable can enable live money on its own.",
      notice(
        "info",
        `${gates.length} of ${declared.length} declared capabilities have a stored gate row. A capability with no row is DISABLED, which is the default and the safe state.`,
      ),
      sandboxed.length > 0 ? sandboxBanner(sandboxed) : null,
      table(
        ["Capability", "Row state", "Active now", "Why not", "Approval version", "Window", ""],
        declared.flatMap((capability) => gateRows(ctx, capability, known.get(capability))),
      ),
    ),
  );
}

/**
 * Says, once and at the top, that this deployment is exercising capabilities it
 * has not approved. It is a statement about the deployment, not a warning about
 * an incident, so it is phrased as one.
 */
function sandboxBanner(sandboxed: readonly CapabilityGate[]): HTMLElement {
  const names = sandboxed.map((g) => g.capability).join(", ");
  const live = sandboxed.filter((g) => g.active);
  return notice(
    "warn",
    el("strong", {}, `${sandboxed.length} gate(s) are SANDBOX: ${names}.`),
    el(
      "p",
      {},
      live.length > 0
        ? `${live.length} of them evaluate active here, which means this deployment declared itself a sandbox tier (CP_API_LEGAL_POLICY=SANDBOX). Nothing they permit can move real value: a sandbox tier cannot be PROD and cannot hold a live provider.`
        : "None of them evaluates active here, so this deployment is not a sandbox tier and the rows are inert.",
    ),
    el(
      "p",
      { class: "muted" },
      "A SANDBOX gate carries no approval, no evidence reference and no approver. It is not a step towards ACTIVE and it never becomes one: the real ceremony starts from DISABLED.",
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
    { class: gate.state === "SANDBOX" ? "sandboxed" : "" },
    el("td", {}, el("code", {}, capability)),
    el("td", {}, statePill(gate)),
    el("td", {}, activePill(gate)),
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
      field("Legal review", evidenceValue(gate, gate.legal_review_ref)),
      field("Provider contract", evidenceValue(gate, gate.provider_contract_ref)),
      field("Risk approval", evidenceValue(gate, gate.risk_approval_ref)),
      field("Security approval", evidenceValue(gate, gate.security_approval_ref)),
      field("Approvers", approversValue(gate)),
    ),
    gate.state === "SANDBOX" ? sandboxProvenance(gate) : null,
  );
  append(detail, cell);
  return [row, detail];
}

/** The row state, with SANDBOX given its own state and its own words. */
function statePill(gate: CapabilityGate): HTMLElement {
  if (gate.state === "SANDBOX") {
    return pill(
      "SANDBOX",
      "sandbox",
      "Entered by one operator (or by deployment configuration) on a sandbox tier. It carries no approval chain and is not a step towards ACTIVE.",
    );
  }
  return pill(gate.state, gate.state.toLowerCase());
}

/**
 * The verdict. "ACTIVE" is reserved for a gate that came through dual control:
 * a sandbox verdict gets its own pill and says so in words, and an `active`
 * SANDBOX row that arrived without the flag is treated as sandbox anyway.
 */
function activePill(gate: CapabilityGate): HTMLElement {
  if (!gate.active) return pill("no", "inactive");
  if (gate.sandbox === true) {
    return pill(
      "sandbox — not an approval",
      "sandbox",
      "Active for this deployment only, because it declared itself a sandbox tier. Nobody approved this capability and no evidence was recorded.",
    );
  }
  if (gate.state === "SANDBOX") {
    // Fail towards "not an approval": the row says SANDBOX, so the verdict
    // cannot have come from the approval chain whatever the flag says.
    return pill(
      "sandbox — not an approval (flag missing)",
      "sandbox",
      "The row is SANDBOX but the response omitted the sandbox flag. It is shown as a sandbox verdict regardless: a SANDBOX row is never an approval.",
    );
  }
  return pill("ACTIVE", "active");
}

/** Evidence references are meaningless on a SANDBOX row, and say so. */
function evidenceValue(gate: CapabilityGate, value: string | undefined): string {
  if (value) return value;
  if (gate.state === "SANDBOX") return "none — a sandbox gate records no evidence";
  return "not recorded";
}

function approversValue(gate: CapabilityGate): HTMLElement | string {
  if (gate.approvers && gate.approvers.length > 0) {
    return el("pre", {}, JSON.stringify(gate.approvers, null, 2));
  }
  if (gate.state === "SANDBOX") {
    return "none, and none is expected — cp_gate_sandbox never touches the approval chain";
  }
  return "none recorded — activation needs two distinct principals, neither of whom proposed";
}

/**
 * Where a SANDBOX row came from, as far as the API will say.
 *
 * `cp_gate_sandbox` writes a row into `capability_gate_transitions` naming the
 * actor: either the operator who ran the step, or the SYSTEM actor
 * `config:CP_API_SANDBOX_GATES` when the capability was listed in the
 * deployment blueprint and activated at boot. **No `/v1/admin` route exposes
 * that table** — `GET /v1/admin/gates` returns the row and the verdict, and the
 * `approvers` array is the approval chain, which a sandbox transition
 * deliberately does not write to. So this console can name the two legal
 * sources and cannot say which of them moved this particular row, and it says
 * exactly that rather than picking one.
 */
function sandboxProvenance(gate: CapabilityGate): HTMLElement {
  return panel(
    "How this gate reached SANDBOX",
    "The transition history is recorded; this console cannot read it.",
    fields(
      field(
        "Legal sources",
        `One operator holding gate:propose with a recent multi-factor sign-in, or the SYSTEM actor ${BOOTSTRAP_ACTOR} when ${gate.capability} is listed in the deployment's CP_API_SANDBOX_GATES and was activated at boot.`,
      ),
      field(
        "Actor on record",
        el(
          "span",
          {},
          "recorded in capability_gate_transitions and in the audit stream, both of which the admin API does not expose. ",
          el("code", {}, BOOTSTRAP_ACTOR),
          " is the actor id for a boot-time activation; an operator-run step names that operator's users.id.",
        ),
      ),
      field(
        "Reason on record",
        "A boot-time activation records: “sandbox tier: activated from deployment configuration (CP_API_SANDBOX_GATES); carries no approval”. An operator-run step records the reason they typed.",
      ),
      field(
        "What it is not",
        "Not an approval, not a proposal, and not a step towards ACTIVE. The approval chain, the evidence references and the validity window are untouched by this transition, and the row is refused outright in PROD by both the function and a table CHECK.",
      ),
    ),
  );
}

function controls(ctx: ViewContext, capability: string, gate: CapabilityGate | undefined): HTMLElement {
  const now = ctx.now();
  const permitted = new Set(allowedWrites(ctx.session.principal, "gates", now, ctx.authority));
  const box = el("div", { class: "controls" });
  // The authority document and the OpenAPI contract are generated from the same
  // Go source, and `scan.test.ts` holds their capability lists equal, so this
  // normally passes for every declared name. If it ever does not, the honest
  // answer is a refused control naming the drift, not a request assembled
  // against a path this console cannot type.
  const routable = isCapability(capability);
  for (const action of ACTIONS) {
    const spec = gateWrite(ctx.authority, action);
    const allowed = routable && permitted.has(spec.writeId) && transitionOffered(action, gate);
    const refusal = !routable
      ? `This console's API contract does not list ${capability} as a gated capability, so it cannot address this gate. The console and the API are out of step; regenerate the client.`
      : !permitted.has(spec.writeId)
        ? `${reasonText("MISSING_PERMISSION")} This step needs ${spec.permission}${
            spec.elevationOnly ? ", which only a live break-glass elevation carries" : ""
          }.`
        : transitionRefusal(action, gate);
    append(
      box,
      actionButton({
        label: action,
        variant: buttonVariant(action),
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

function buttonVariant(action: GateActionName): "primary" | "danger" | "quiet" {
  return action === "revoke" || action === "suspend" ? "danger" : "quiet";
}

/**
 * The permission and step-up window for one gate step.
 *
 * Six of the eight are declared in the generated authority document as
 * `gate.<action>` writes. `sandbox` and `unsandbox` are not: `internal/
 * adminplane` has not grown them yet, and this console does not get to invent
 * an authority the document does not carry. What it can do honestly is read
 * the permission from the enforcing code rather than guess one:
 * `internal/gates.Admin.sandboxOp` requires exactly `gate:propose` and exactly
 * the same `gates.StepUpMaxAge` step-up as the first step of the real
 * ceremony, so the two sandbox steps are gated on the console's side by the
 * `gate.propose` write.
 *
 * That is deliberately no more permissive than the server: an operator who
 * cannot propose a gate cannot sandbox one either. The moment adminplane
 * exports `gate.sandbox` / `gate.unsandbox`, the lookup below finds them and
 * this fallback stops being used — which is why it prefers the document.
 */
interface GateWrite {
  readonly writeId: string;
  readonly permission: string;
  readonly elevationOnly: boolean;
}

function gateWrite(authority: ViewContext["authority"], action: GateActionName): GateWrite {
  const declared = authorityGateAction(authority, action);
  if (declared) return declared;
  const propose = authorityGateAction(authority, "propose");
  return {
    writeId: "gate.propose",
    permission: propose?.permission ?? "gate:propose",
    elevationOnly: propose?.elevationOnly ?? false,
  };
}

function authorityGateAction(authority: ViewContext["authority"], action: GateActionName): GateWrite | null {
  const spec = authority.gateAction(action);
  if (!spec) return null;
  return {
    writeId: `gate.${action}`,
    permission: spec.permission,
    elevationOnly: authority.isDualControl(spec.permission),
  };
}

/**
 * Whether the state machine allows this step from the row's current state.
 *
 * The console does not restate the whole transition table — it lives in Go
 * (`internal/gates.transitions`) and again in migration 00701, and a third copy
 * would be a third thing to keep in step. What it does state is the part
 * SANDBOX adds, because getting it wrong in either direction is harmful: an
 * offered `approve` on a SANDBOX row invites an operator to think the sandbox
 * is on the way to being approved, and a refused `revoke` on one would be a
 * console that cannot stop something.
 *
 * SANDBOX leaves only to DISABLED (`unsandbox`) or REVOKED (`revoke`), so
 * `revoke` is offered from every state, always.
 */
function transitionOffered(action: GateActionName, gate: CapabilityGate | undefined): boolean {
  const state = gate?.state;
  if (action === "sandbox") return state !== undefined && SANDBOX_SOURCES.includes(state);
  if (action === "unsandbox") return state === "SANDBOX";
  if (action === "revoke") return true;
  return state !== "SANDBOX";
}

function transitionRefusal(action: GateActionName, gate: CapabilityGate | undefined): string {
  const state = gate?.state;
  if (action === "sandbox") {
    if (state === undefined) {
      return "There is no stored gate row for this capability, and a sandbox step moves an existing row rather than creating one. The deployment's own bootstrap (gates.Bootstrap, or CP_API_SANDBOX_GATES at boot) writes the DISABLED row first.";
    }
    return `A gate enters SANDBOX only from ${SANDBOX_SOURCES.join(", ")}. This one is ${state}. A configuration line never moves a real approval's state; revoke it first if that is what you mean.`;
  }
  if (action === "unsandbox") {
    return `Only a SANDBOX gate can be unsandboxed. This one is ${state ?? "not stored at all"}.`;
  }
  return "This gate is SANDBOX, which is not on the approval path. Unsandbox it back to DISABLED first — the real ceremony starts there — or revoke it.";
}

function openGateForm(
  ctx: ViewContext,
  capability: Capability,
  action: GateActionName,
  gate: CapabilityGate | undefined,
): void {
  const evidence = action === "propose";
  const isSandboxStep = SANDBOX_GATE_ACTIONS.includes(action);
  ctx.report(
    commandForm({
      title: `${action} ${capability}`,
      description: formDescription(action),
      submitLabel: action,
      variant: buttonVariant(action) === "danger" ? "danger" : "primary",
      ...(action === "activate"
        ? {
            warning:
              "Activating a live-money capability is the last step before real funds can move. The server still refuses unless all five conditions hold.",
          }
        : {}),
      ...(action === "sandbox"
        ? {
            warning:
              "This is not an approval and does not become one. The gate will read active only on this deployment, only because it declared itself a sandbox tier, and the console will label it “sandbox — not an approval” everywhere it appears. A deployment that is not a sandbox tier refuses this step outright.",
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
          ctx.report(outcomeNotice(capability, updated));
          ctx.refresh();
        } catch (err) {
          if (isSandboxStep) {
            // The refusal an operator will actually meet here is the API's
            // "this deployment is not a sandbox tier", which is a fact about
            // the deployment rather than about them. Render the server's own
            // words for it instead of a validation error under the field.
            ctx.report(problemNotice(err, `the ${action} step`));
            return;
          }
          throw new Error(describeProblem(err));
        }
      },
    }),
  );
}

function formDescription(action: GateActionName): string {
  switch (action) {
    case "propose":
      return "A proposal carries the evidence references the activation check requires. Missing evidence keeps the gate inactive however many people approve it.";
    case "sandbox":
      return "One operator with gate:propose and a recent multi-factor sign-in, on a sandbox tier only. It writes its own transition row and touches neither the approval chain nor the evidence references.";
    case "unsandbox":
      return "Returns the gate to DISABLED, which is where the real ceremony starts. Nothing about the approval chain changes, because a sandbox gate never had one.";
    default:
      return "The reason is recorded on the gate transition and in the audit stream.";
  }
}

/** What just happened, in words that do not promote a sandbox row. */
function outcomeNotice(capability: string, updated: CapabilityGate): HTMLElement {
  if (updated.state === "SANDBOX" || updated.sandbox === true) {
    return notice(
      "warn",
      el(
        "strong",
        {},
        `${capability} is SANDBOX${updated.active ? " and reads active on this deployment" : " and does not read active here"}.`,
      ),
      el(
        "p",
        {},
        "That is not an approval. Nobody approved this capability, no evidence was recorded, and the gate is not on the path to ACTIVE.",
      ),
      updated.inactive_reason ? el("p", { class: "muted" }, updated.inactive_reason) : null,
    );
  }
  return notice(
    "info",
    `${capability} is now ${updated.state}${updated.active ? " and ACTIVE" : " and not active"}${
      updated.inactive_reason ? `: ${updated.inactive_reason}` : "."
    }`,
  );
}

/** Exposed for the shell's empty-state copy. */
export function gatesEmptyState(): HTMLElement {
  return emptyState("No capability gate rows exist. Every capability is therefore DISABLED.");
}
