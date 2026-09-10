/**
 * Accounts: search, and one account's whole read-side picture (goal §38).
 *
 * What an operator may see here is everything the admin plane will tell them
 * about one account: its status and what is restricting it, the Credit balance
 * broken into the buckets that answer "how much of this may leave", the open
 * reconciliation records against it, the controlled actions that have named it,
 * the gates and kill switches that decide what it may do, and the activity
 * timeline. What an operator may *do* here is exactly one thing: move the
 * account through the status machine of `internal/accounts`
 * (ACTIVE / RESTRICTED / FROZEN / CLOSED), with `account:freeze`, a recent
 * multi-factor sign-in and a recorded reason.
 *
 * **There is no balance-editing command in this system and this surface does
 * not imply one.** That is not achieved by hiding figures — an operator who
 * cannot see a balance cannot investigate a complaint about one — but by the
 * shape of the API underneath: every operator read of an account goes through
 * `security.RequireAccount`, which admits a holder of `account:read_any`, and
 * every account-scoped *write* goes through `RequireAccountOwner`, which has no
 * operator override at all. So the figures below are read-only in the strong
 * sense: there is no endpoint an operator could call to change one. The only
 * financial repair that exists is a compensating journal transaction posted by
 * the ledger and referenced from a reconciliation record.
 *
 * Balances are rendered as the exact base-unit integers the API sends. Nothing
 * here divides, converts or sums them (`money.ts`), and the response carries no
 * decimals field, so the console groups the digits and says "base units" rather
 * than inventing a scale.
 *
 * A frozen account still settles and reconciles (PART 194).
 */
import {
  changeAccountStatus,
  getAccount,
  getAccountActivity,
  getCreditBalance,
  listActions,
  listGates,
  listKillSwitches,
  listReconciliationRecords,
  searchAccounts,
} from "../api.ts";
import type { Account, ActivityItem, AdminAction, CapabilityGate, CreditBalance, KillSwitch, ReconciliationRecord } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { allowedWrites } from "../decide.ts";
import { append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm, refusedCommand } from "../forms.ts";
import { formatInstant, reasonText } from "../format.ts";
import { groupDigits, isQuantity } from "../money.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const STATUSES: ReadonlyArray<Account["status"]> = ["ACTIVE", "RESTRICTED", "FROZEN", "CLOSED"];

/** Reconciliation statuses that mean "still open against this account". */
const OPEN_RECONCILIATION: readonly string[] = ["OPEN", "MISMATCH", "INVESTIGATING", "ESCALATED"];

/** The kill-switch kind whose scope id is an account id. */
const ACCOUNT_SCOPED_KILL = "ACCOUNT_FREEZE";

/** The scope id that means "the whole platform" on a kill switch. */
const GLOBAL_SCOPE = "*";

export async function renderAccounts(ctx: ViewContext, root: HTMLElement): Promise<void> {
  const params = new URLSearchParams(window.location.hash.split("?")[1] ?? "");
  const query = params.get("q") ?? "";
  const selected = params.get("id") ?? "";

  clear(root);
  append(root, searchPanel(ctx, query));

  if (selected) {
    await renderOneAccount(ctx, root, selected);
    return;
  }

  let page;
  try {
    page = await searchAccounts(query, { limit: 50 });
  } catch (err) {
    append(root, problemNotice(err, "the account search"));
    return;
  }

  append(
    root,
    panel(
      "Accounts",
      `${page.items.length} account(s)${query ? ` matching "${query}"` : ""}.`,
      page.items.length === 0
        ? emptyState("No accounts matched. The server returned an empty page.")
        : table(
            ["Account", "Kind", "Status", "Reason", "Created", ""],
            page.items.map((a) =>
              el(
                "tr",
                {},
                el("td", {}, el("code", {}, a.id)),
                el("td", {}, a.kind),
                el("td", {}, pill(a.status, a.status.toLowerCase())),
                el("td", { class: "muted" }, a.status_reason ?? "—"),
                el("td", { class: "muted" }, formatInstant(a.created_at)),
                el(
                  "td",
                  {},
                  el(
                    "button",
                    { type: "button", class: "button quiet", onclick: () => ctx.navigate(`accounts?id=${a.id}`) },
                    "Inspect",
                  ),
                ),
              ),
            ),
          ),
    ),
  );
}

function searchPanel(ctx: ViewContext, query: string): HTMLElement {
  // `placeholder` here is the DOM input attribute -- the grey hint inside an
  // empty field. It is not a productization stub; see the note on Attrs in
  // dom.ts, which every §62 scan of this app will keep finding.
  const input = el("input", { type: "text", name: "q", value: query, placeholder: "account id or search term" });
  input.value = query;
  const form = el("form", {
    class: "search",
    onsubmit: (event: SubmitEvent) => {
      event.preventDefault();
      ctx.navigate(`accounts?q=${encodeURIComponent(input.value.trim())}`);
    },
  });
  append(form, input, el("button", { type: "submit", class: "button quiet" }, "Search"));
  return form;
}

/**
 * One account, section by section.
 *
 * The sections are ordered the way an operator answering a question reads
 * them: who and what state, what is restricting it, what it holds, what is
 * unresolved about it, what has been done to it, what it is permitted to do,
 * and finally the timeline. Each section fetches independently and reports its
 * own failure, so one unwired port cannot blank the page: an operator looking
 * at a deployment with no reconciliation engine still gets the status, the
 * balance and the audit trail.
 */
async function renderOneAccount(ctx: ViewContext, root: HTMLElement, accountId: string): Promise<void> {
  append(
    root,
    el(
      "button",
      { type: "button", class: "button quiet", onclick: () => ctx.navigate("accounts") },
      "Back to search",
    ),
  );

  let account: Account;
  try {
    account = await getAccount(accountId);
  } catch (err) {
    append(root, problemNotice(err, "the account"));
    return;
  }

  append(root, identityPanel(account), ownerPanel(account));
  await renderRestrictions(ctx, root, account);
  await renderCreditBalance(root, account.id);
  await renderOpenReconciliation(ctx, root, account.id);
  await renderRecentAdminActions(ctx, root, account.id);
  await renderCapabilities(ctx, root);
  append(root, agentsPanel());
  append(root, statusPanel(ctx, account));
  await renderActivity(root, accountId);
}

function identityPanel(account: Account): HTMLElement {
  return panel(
    `Account ${account.id}`,
    "Identity and status, as the admin plane reports them.",
    fields(
      field("Account", el("code", {}, account.id)),
      field("Kind", account.kind),
      field("Status", pill(account.status, account.status.toLowerCase())),
      field("Status reason", account.status_reason ?? "—"),
      field("Created", formatInstant(account.created_at)),
    ),
  );
}

/**
 * The user behind the account.
 *
 * **Slot for `GET /v1/admin/users/{userId}` (in flight on another branch).**
 * When it lands, this function is the only place that changes: fetch the
 * profile summary and render it here in place of the notice.
 *
 * It needs one thing this console cannot get today. The `Account` schema
 * carries `id`, `kind`, `status`, `status_reason` and `created_at` — and no
 * owner. No admin route maps an account to its user, so there is no `userId`
 * to pass. Either `Account` grows an owner field or `GET /v1/admin/accounts`
 * grows a lookup; until one of them exists this section states the gap rather
 * than guessing at an identity.
 */
function ownerPanel(account: Account): HTMLElement {
  return panel(
    "Person behind this account",
    "Not resolvable from the admin plane as it stands.",
    notice(
      "info",
      el("strong", {}, "No admin route maps an account to its owner."),
      el(
        "p",
        {},
        `The account record (${account.kind}) carries no owner field, so this console has no user id to look up. The profile summary belongs here the moment GET /v1/admin/users/{userId} exists and something reports which user owns an account.`,
      ),
    ),
  );
}

/**
 * What is restricting this account right now: its own status, plus every kill
 * switch whose scope covers it.
 *
 * A kill switch scoped `*` covers every account; `ACCOUNT_FREEZE` scoped to
 * this account id covers this one. Switches scoped to some other account,
 * agent, venue or chain are not shown, because listing them here would make an
 * unrelated incident look like a restriction on this customer.
 */
async function renderRestrictions(ctx: ViewContext, root: HTMLElement, account: Account): Promise<void> {
  let switches: KillSwitch[];
  try {
    switches = await listKillSwitches();
  } catch (err) {
    append(
      root,
      panel("Restrictions", "The kill switches covering this account could not be read.", problemNotice(err, "kill switches")),
    );
    return;
  }
  const covering = switches.filter(
    (s) => s.active && (s.scope_id === GLOBAL_SCOPE || (s.kind === ACCOUNT_SCOPED_KILL && s.scope_id === account.id)),
  );
  append(
    root,
    panel(
      "Restrictions",
      "The account's own status, and every active kill switch whose scope covers it.",
      account.status === "ACTIVE"
        ? notice("info", "The account status itself is not restricting anything.")
        : notice(
            "warn",
            `This account is ${account.status}. ${account.status_reason ?? "No reason was recorded on the status change."}`,
          ),
      covering.length === 0
        ? emptyState("No active kill switch covers this account.")
        : table(
            ["Kind", "Scope", "Severity", "Since", "Reason"],
            covering.map((s) =>
              el(
                "tr",
                { class: "blocking" },
                el("td", {}, el("code", {}, s.kind)),
                el(
                  "td",
                  {},
                  el("code", {}, s.scope_id),
                  s.scope_id === GLOBAL_SCOPE ? pill("platform-wide", "blocking") : pill("this account", "blocking"),
                ),
                el("td", {}, pill(s.severity, s.severity.toLowerCase())),
                el("td", { class: "muted" }, s.activated_at ? formatInstant(s.activated_at) : "—"),
                el("td", { class: "muted" }, s.reason ?? "—"),
              ),
            ),
          ),
      el(
        "p",
        { class: "muted" },
        "A kill switch stops new risk for its scope. It never stops settlement, reconciliation, ledger posting or audit for this account.",
      ),
      el(
        "button",
        { type: "button", class: "button quiet", onclick: () => ctx.navigate("kill-switches") },
        "Open kill switches",
      ),
    ),
  );
}

/**
 * The Credit balance, in the buckets PART XX requires.
 *
 * `GET /v1/credits/balance` is the only route that reports one, and it is not
 * under `/admin` — but it is not a customer-only route either: it resolves the
 * account through `security.RequireAccount`, which admits a principal holding
 * `account:read_any`, the same permission that lets this surface exist at all.
 * There is no separate admin balance route to prefer, and none is needed for a
 * read.
 *
 * Gross and payout-eligible are different numbers and this view never shows one
 * where it means the other; the reasons the remainder is ineligible are
 * rendered verbatim from the response.
 */
async function renderCreditBalance(root: HTMLElement, accountId: string): Promise<void> {
  let balance: CreditBalance;
  try {
    balance = await getCreditBalance(accountId);
  } catch (err) {
    append(
      root,
      panel(
        "Credit balance",
        "The balance could not be read; nothing here is a zero.",
        problemNotice(err, "the Credit balance"),
      ),
    );
    return;
  }
  append(
    root,
    panel(
      "Credit balance",
      "Exact base units as the ledger computed them. Nothing on this console can change any of these figures: there is no balance-editing endpoint, and account-scoped writes admit only the account's owner.",
      fields(
        quantityField("Gross", balance.gross, "Every remaining unit, whatever its origin or finality."),
        quantityField("Spendable", balance.spendable, "What may be spent inside the platform right now."),
        quantityField("Frozen", balance.frozen, "Held against a restriction, a dispute or an open reservation."),
        quantityField("Reversed", balance.reversed, "Units taken back by a refund or a chargeback."),
        quantityField(
          "Payout-eligible",
          balance.payout_eligible,
          "What the named policy version permits to leave right now. This is not the gross balance.",
        ),
        quantityField("Ineligible", balance.ineligible, "The remainder, which cannot be withdrawn."),
      ),
      balance.ineligible_reasons && balance.ineligible_reasons.length > 0
        ? panel(
            "Why the remainder cannot be withdrawn",
            "The server's own reasons, verbatim.",
            el("ul", { class: "reasons" }, ...balance.ineligible_reasons.map((r) => el("li", {}, r))),
          )
        : null,
      breakdownTable("By origin", balance.by_origin),
      breakdownTable("By finality", balance.by_finality),
      fields(
        field("Policy version", balance.policy_version),
        field("Policy hash", balance.policy_hash ? el("code", {}, balance.policy_hash) : "—"),
      ),
    ),
  );
}

/**
 * One quantity, grouped for reading and never converted.
 *
 * The response carries no decimals field, so there is no scale to apply and
 * the digits are shown as they arrived. A value that is not the exact wire form
 * is shown verbatim and flagged, the same rule `money.ts` applies everywhere.
 */
function quantityField(label: string, value: string | undefined, hint: string): HTMLElement | null {
  if (value === undefined) return null;
  const exact = isQuantity(value);
  return field(label, `${exact ? groupDigits(value) : value} base units — ${hint}`, {
    exact,
    raw: value,
  });
}

function breakdownTable(title: string, breakdown: Record<string, string> | undefined): HTMLElement | null {
  if (!breakdown) return null;
  const entries = Object.entries(breakdown);
  if (entries.length === 0) return null;
  return panel(
    title,
    "The same total, split by the dimension the ledger records.",
    table(
      ["Bucket", "Base units"],
      entries.map(([key, value]) =>
        el(
          "tr",
          {},
          el("td", {}, el("code", {}, key)),
          el(
            "td",
            isQuantity(value) ? {} : { class: "inexact" },
            isQuantity(value) ? groupDigits(value) : value,
          ),
        ),
      ),
    ),
  );
}

/** Open reconciliation records naming this account. */
async function renderOpenReconciliation(ctx: ViewContext, root: HTMLElement, accountId: string): Promise<void> {
  let page;
  try {
    page = await listReconciliationRecords("", accountId, { limit: 100 });
  } catch (err) {
    append(
      root,
      panel(
        "Open reconciliation",
        "Mismatches against this account could not be read.",
        problemNotice(err, "this account's reconciliation records"),
        notice(
          "info",
          "This is the deployment's state, not an empty result: cmd/api starts with no reconciliation engine wired, so the endpoint answers 422 UNSUPPORTED. The reconciliation worker is still recording mismatches; they are not readable over the API here.",
        ),
      ),
    );
    return;
  }
  const open = page.items.filter((r: ReconciliationRecord) => OPEN_RECONCILIATION.includes(r.status));
  append(
    root,
    panel(
      "Open reconciliation",
      `${open.length} unresolved of ${page.items.length} record(s) against this account.`,
      open.length === 0
        ? emptyState("Nothing unresolved against this account. The server returned no open record.")
        : table(
            ["Kind", "Status", "Material", "Blocks new risk", "Opened", ""],
            open.map((r) =>
              el(
                "tr",
                { class: r.material ? "material" : "" },
                el("td", {}, r.kind, el("br"), el("span", { class: "muted" }, r.mode)),
                el("td", {}, pill(r.status, r.status.toLowerCase())),
                el(
                  "td",
                  {},
                  r.material
                    ? pill("material", "material", "A human must resolve this, under dual control.")
                    : el("span", { class: "muted" }, "no"),
                ),
                el("td", {}, r.blocks_new_risk ? pill("blocking", "blocking") : el("span", { class: "muted" }, "no")),
                el("td", { class: "muted" }, formatInstant(r.opened_at)),
                el(
                  "td",
                  {},
                  el(
                    "button",
                    {
                      type: "button",
                      class: "button quiet",
                      onclick: () => ctx.navigate(`reconciliation?status=&account_id=${accountId}`),
                    },
                    "Open in reconciliation",
                  ),
                ),
              ),
            ),
          ),
    ),
  );
}

/**
 * The controlled actions that have named this account.
 *
 * `GET /v1/admin/actions` filters by status and nothing else, so the account
 * filter is applied here, over the most recent page. That is a real limitation
 * and it is stated: an action older than the page is not shown, and this is not
 * a claim that none exists.
 */
async function renderRecentAdminActions(ctx: ViewContext, root: HTMLElement, accountId: string): Promise<void> {
  const limit = 100;
  let page;
  try {
    page = await listActions("", { limit });
  } catch (err) {
    append(
      root,
      panel("Recent admin actions", "The action queue could not be read.", problemNotice(err, "the action queue")),
    );
    return;
  }
  const naming = page.items.filter((a: AdminAction) => a.target_id === accountId);
  append(
    root,
    panel(
      "Recent admin actions",
      `${naming.length} of the most recent ${String(page.items.length)} controlled action(s) name this account.`,
      naming.length === 0
        ? emptyState("No recent controlled action names this account.")
        : table(
            ["Kind", "Status", "Proposed by", "Approved by", "Proposed", "Reason"],
            naming.map((a) =>
              el(
                "tr",
                {},
                el("td", {}, a.kind, a.requires_dual ? pill("dual control", "dual") : pill("single", "single")),
                el("td", {}, pill(a.status, a.status.toLowerCase())),
                el("td", {}, el("code", {}, a.proposed_by)),
                el("td", {}, a.approved_by ? el("code", {}, a.approved_by) : el("span", { class: "muted" }, "—")),
                el("td", { class: "muted" }, formatInstant(a.proposed_at)),
                el("td", { class: "muted" }, a.reason ?? "—"),
              ),
            ),
          ),
      el(
        "p",
        { class: "muted" },
        `The API has no account filter on the action queue, so this is the newest ${String(limit)} actions filtered here by target id. An older action naming this account exists in the queue and is not listed above.`,
      ),
      el("button", { type: "button", class: "button quiet", onclick: () => ctx.navigate("actions?status=ALL") }, "Open the action queue"),
    ),
  );
}

/**
 * What this account is permitted to do: the capability gates, which are a
 * property of the deployment rather than of the account, and therefore decide
 * the ceiling for every account in it.
 *
 * A gate that is SANDBOX is shown as sandbox here too, with the same words the
 * gates view uses. An operator reading an account page must not come away
 * believing a capability was approved when a rehearsal is what turned it on.
 */
async function renderCapabilities(ctx: ViewContext, root: HTMLElement): Promise<void> {
  let gates: CapabilityGate[];
  try {
    gates = await listGates();
  } catch (err) {
    append(
      root,
      panel("Capabilities", "The capability gates could not be read.", problemNotice(err, "capability gates")),
    );
    return;
  }
  const active = gates.filter((g) => g.active);
  const sandbox = active.filter((g) => g.sandbox === true || g.state === "SANDBOX");
  append(
    root,
    panel(
      "Capabilities affecting this account",
      "Capability gates are deployment-wide: they set the ceiling for every account here, including this one.",
      gates.length === 0
        ? emptyState("No capability gate row exists, so every capability is DISABLED — the default and the safe state.")
        : table(
            ["Capability", "Row state", "Available here", "Why not"],
            gates.map((g) =>
              el(
                "tr",
                { class: g.state === "SANDBOX" ? "sandboxed" : "" },
                el("td", {}, el("code", {}, g.capability)),
                el(
                  "td",
                  {},
                  g.state === "SANDBOX"
                    ? pill("SANDBOX", "sandbox", "Active for this deployment only. Nobody approved it.")
                    : pill(g.state, g.state.toLowerCase()),
                ),
                el(
                  "td",
                  {},
                  !g.active
                    ? pill("no", "inactive")
                    : g.sandbox === true || g.state === "SANDBOX"
                      ? pill("sandbox — not an approval", "sandbox")
                      : pill("ACTIVE", "active"),
                ),
                el("td", { class: "muted" }, g.inactive_reason ?? (g.active ? "—" : "not reported")),
              ),
            ),
          ),
      sandbox.length > 0
        ? notice(
            "warn",
            `${sandbox.length} capability(ies) available to this account come from a SANDBOX gate, which carries no approval and exists only because this deployment declared itself a sandbox tier.`,
          )
        : null,
      el("button", { type: "button", class: "button quiet", onclick: () => ctx.navigate("gates") }, "Open capability gates"),
    ),
  );
}

/**
 * The agents acting for this account.
 *
 * **Slot for `GET /v1/admin/agents` (in flight on another branch).** When it
 * lands, this function is the only place that changes: list the agents whose
 * account is this one, with their ladder rung and their pause state, and drop
 * the notice.
 *
 * Nothing is rendered in its place today, because there is no route that lists
 * agents to an operator and a page that showed "no agents" would be asserting
 * something the console cannot know.
 */
function agentsPanel(): HTMLElement {
  return panel(
    "Agents acting for this account",
    "Not readable from the admin plane as it stands.",
    notice(
      "info",
      el("strong", {}, "No route lists an account's agents to an operator."),
      el(
        "p",
        {},
        "This is not “this account has no agents”: it is “nothing here can tell”. GET /v1/admin/agents belongs in this panel when it exists; an agent's pause state is meanwhile visible as an AGENT_PAUSE kill switch on the kill switches page.",
      ),
    ),
  );
}

function statusPanel(ctx: ViewContext, account: Account): HTMLElement {
  const permitted = allowedWrites(ctx.session.principal, "accounts", ctx.now(), ctx.authority);
  if (!permitted.includes("account.status")) {
    return refusedCommand(
      "Change status",
      `${reasonText("MISSING_PERMISSION")} Changing an account's status needs account:freeze and a recent multi-factor sign-in.`,
    );
  }
  const options = STATUSES.filter((s) => s !== account.status);
  return panel(
    "Change status",
    "The only write on this surface. Freezing or restricting stops new risk for this account; it does not stop settlement, reconciliation or ledger posting.",
    commandForm({
      title: `Move ${account.id} from ${account.status}`,
      submitLabel: "Change status",
      variant: "danger",
      fields: [
        { name: "to", label: "New status", options, ...(options[0] ? { value: options[0] } : {}) },
        {
          name: "reason",
          label: "Reason",
          multiline: true,
          minLength: ctx.authority.doc.min_reason_length,
          hint: `At least ${ctx.authority.doc.min_reason_length} characters. Recorded on the account and in the audit stream.`,
        },
      ],
      onSubmit: async (values, key) => {
        try {
          const updated = await changeAccountStatus(
            account.id,
            { to: (values["to"] ?? account.status) as Account["status"], reason: values["reason"] ?? "" },
            key,
          );
          ctx.report(notice("info", `Account ${updated.id} is now ${updated.status}.`));
          ctx.refresh();
        } catch (err) {
          throw new Error(describeProblem(err));
        }
      },
    }),
  );
}

/**
 * The audit trail for one account: the activity timeline the backend joins
 * across events, intents, eligibility, risk, execution, fills, reconciliation
 * and ledger postings (goal §52).
 */
async function renderActivity(root: HTMLElement, accountId: string): Promise<void> {
  let page;
  try {
    page = await getAccountActivity(accountId, { limit: 100 });
  } catch (err) {
    append(root, problemNotice(err, "this account's activity"));
    return;
  }
  append(
    root,
    panel(
      "Audit trail",
      "Every recorded step for this account, newest first, exactly as the backend joined it.",
      page.items.length === 0
        ? emptyState("No activity recorded for this account.")
        : table(
            ["When", "Kind", "Summary", "Correlation", "References"],
            page.items.map((item) => activityRow(item)),
          ),
      page.next_cursor
        ? el(
            "p",
            { class: "muted" },
            `More activity exists beyond this page (the server returned a cursor). The newest ${String(page.items.length)} entries are shown.`,
          )
        : null,
    ),
  );
}

function activityRow(item: ActivityItem): HTMLElement {
  const references = item.references ?? {};
  return el(
    "tr",
    {},
    el("td", { class: "muted" }, formatInstant(item.occurred_at)),
    el("td", {}, pill(item.kind, item.kind.toLowerCase())),
    el("td", {}, item.summary),
    el("td", {}, item.correlation_id ? el("code", {}, item.correlation_id) : el("span", { class: "muted" }, "—")),
    el(
      "td",
      { class: "muted" },
      Object.keys(references).length === 0
        ? "—"
        : Object.entries(references)
            .map(([k, v]) => `${k}=${v}`)
            .join(" "),
    ),
  );
}
