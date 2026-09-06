/**
 * Accounts: search, inspect, change status, read the audit trail.
 *
 * There is no balance-editing endpoint in this system and this surface must
 * not create the appearance of one. It therefore shows no balance at all: the
 * operator sees identity, status, status reason and the activity timeline. A
 * customer's authoritative figures are computed on demand by the backend for
 * that customer; putting a number here that an operator could mistake for a
 * balance — or for something they could change — would be worse than useless.
 *
 * The only write is the status machine of `internal/accounts`
 * (ACTIVE / RESTRICTED / FROZEN / CLOSED), which requires `account:freeze`, a
 * recent multi-factor sign-in and a recorded reason. A frozen account still
 * settles and reconciles (PART 194).
 */
import { changeAccountStatus, getAccount, getAccountActivity, searchAccounts } from "../api.ts";
import type { Account, ActivityItem } from "../api.ts";
import type { ViewContext } from "../context.ts";
import { allowedWrites } from "../decide.ts";
import { append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm, refusedCommand } from "../forms.ts";
import { formatInstant, reasonText } from "../format.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const STATUSES: ReadonlyArray<Account["status"]> = ["ACTIVE", "RESTRICTED", "FROZEN", "CLOSED"];

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

  append(
    root,
    panel(
      `Account ${account.id}`,
      "Identity and status only. This system has no balance-editing command, and this console shows no figure an operator could mistake for one.",
      fields(
        field("Account", el("code", {}, account.id)),
        field("Kind", account.kind),
        field("Status", pill(account.status, account.status.toLowerCase())),
        field("Status reason", account.status_reason ?? "—"),
        field("Created", formatInstant(account.created_at)),
      ),
    ),
    statusPanel(ctx, account),
  );

  await renderActivity(root, accountId);
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
    "Freezing or restricting stops new risk for this account. It does not stop settlement, reconciliation or ledger posting.",
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
 * and ledger postings.
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
