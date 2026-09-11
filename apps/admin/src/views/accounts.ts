/**
 * Accounts: search, and one account's whole read-side picture (goal §38).
 *
 * What an operator may see here is everything the admin plane will tell them
 * about one account: the person who owns it, its status and what is restricting
 * it, the Credit balance broken into the buckets that answer "how much of this
 * may leave", the open reconciliation records against it, the controlled
 * actions that have named it, the gates and kill switches that decide what it
 * may do, the agents acting for it, and the activity timeline.
 *
 * What an operator may *do* here is three things, and each one is a reaction
 * rather than an initiative:
 *
 *   - move the account through the status machine of `internal/accounts`
 *     (ACTIVE / RESTRICTED / FROZEN / CLOSED), with `account:freeze`, a recent
 *     multi-factor sign-in and a recorded reason;
 *   - decide a closure request **the user themselves opened** — cancel, refuse,
 *     or effect it once the cooling-off period has passed. There is no route
 *     that closes an account nobody asked to close, so there is no control here
 *     that could start one;
 *   - pause one agent, which stops it acting again and unwinds nothing.
 *
 * Nothing on this surface creates a financial position, moves value, or decides
 * anything on a customer's behalf that the customer did not ask for.
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
  decideClosure,
  getAccount,
  getAccountActivity,
  getAdminUser,
  getCreditBalance,
  listActions,
  listAgents,
  listGates,
  listKillSwitches,
  listReconciliationRecords,
  pauseAgent,
  searchAccounts,
} from "../api.ts";
import type {
  Account,
  ActivityItem,
  AdminAction,
  AdminUserView,
  Agent,
  AgentPage,
  CapabilityGate,
  ClosureDecisionName,
  CreditBalance,
  KillSwitch,
  ReconciliationRecord,
} from "../api.ts";
import type { ViewContext } from "../context.ts";
import { allowedWrites, holds, steppedUp } from "../decide.ts";
import { actionButton, append, clear, el, emptyState, field, fields, notice, panel, pill, table } from "../dom.ts";
import { commandForm, refusedCommand } from "../forms.ts";
import { formatDuration, formatInstant, reasonText, relativeInstant } from "../format.ts";
import { groupDigits, isQuantity } from "../money.ts";
import { describeProblem, problemNotice } from "../problem.ts";

const STATUSES: ReadonlyArray<Account["status"]> = ["ACTIVE", "RESTRICTED", "FROZEN", "CLOSED"];

/** Reconciliation statuses that mean "still open against this account". */
const OPEN_RECONCILIATION: readonly string[] = ["OPEN", "MISMATCH", "INVESTIGATING", "ESCALATED"];

/** The kill-switch kind whose scope id is an account id. */
const ACCOUNT_SCOPED_KILL = "ACCOUNT_FREEZE";

/** The scope id that means "the whole platform" on a kill switch. */
const GLOBAL_SCOPE = "*";

/**
 * The two permissions `POST /v1/admin/agents/{agentId}/pause` needs, and it
 * needs both.
 *
 * `internal/httpapi/authz.go` floors the route on `kill:activate` rather than
 * on `agent:pause`, and says why: the CUSTOMER role holds `agent:pause` — it is
 * how an owner stops their own agent — so a route floored on it would be
 * reachable by every customer. `internal/agents` then demands `agent:pause` and
 * an OPERATOR actor. The route says who may reach it; the domain says who may
 * do it. A console that checked only the first would offer the control to
 * RISK and SECURITY, who hold `kill:activate` and not `agent:pause`, and the
 * server would refuse them.
 */
const AGENT_PAUSE_PERMISSIONS: readonly string[] = ["kill:activate", "agent:pause"];

/** Agent states that a pause can still act on. */
const PAUSABLE_STATUSES: readonly string[] = ["ENABLED", "STOPPED"];

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
  // empty field. It stands in for nothing; see the note on Attrs in dom.ts,
  // which every §62 scan of this app will keep finding.
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

  append(root, identityPanel(account));
  await renderOwner(ctx, root, account);
  await renderRestrictions(ctx, root, account);
  await renderCreditBalance(root, account.id);
  await renderOpenReconciliation(ctx, root, account.id);
  await renderRecentAdminActions(ctx, root, account.id);
  await renderCapabilities(ctx, root);
  await renderAgents(ctx, root, account.id);
  append(root, statusPanel(ctx, account));
  await renderActivity(root, accountId);
}

function identityPanel(account: Account): HTMLElement {
  return panel(
    `Account ${account.id}`,
    "Identity and status, as the admin plane reports them.",
    fields(
      field("Account", el("code", {}, account.id)),
      field("Owner", account.owner_user_id ? el("code", {}, account.owner_user_id) : "not reported"),
      field("Kind", account.kind),
      field("Status", pill(account.status, account.status.toLowerCase())),
      field("Status reason", account.status_reason ?? "—"),
      field("Created", formatInstant(account.created_at)),
    ),
  );
}

/**
 * The person behind the account: `GET /v1/admin/users/{userId}`, reached
 * through the `owner_user_id` that `Account` now carries.
 *
 * The view is read-only by construction. It carries no e-mail address, legal
 * name, phone number or date of birth — those stay sealed in `identity_pii`,
 * which this surface has no route to — and it points at the audit stream rather
 * than restating it. `email_verified` says the identity provider asserted a
 * verified address; the address itself is not reachable from here and this
 * console does not pretend otherwise.
 *
 * The one mutation is deciding a closure request **the user themselves
 * opened**. There is no operator route that closes an account nobody asked to
 * close, and this console therefore has no control that could originate one.
 */
async function renderOwner(ctx: ViewContext, root: HTMLElement, account: Account): Promise<void> {
  const userId = account.owner_user_id;
  if (!userId) {
    append(
      root,
      panel(
        "Person behind this account",
        "The account record did not name an owner.",
        notice(
          "warn",
          el("strong", {}, "This account reported no owner_user_id."),
          el(
            "p",
            {},
            `An account of kind ${account.kind} is expected to name the user who owns it. PLATFORM and CANARY accounts belong to Nodal rather than to a person, which is the ordinary reason for this; for a CUSTOMER account it is a gap worth reporting.`,
          ),
        ),
      ),
    );
    return;
  }

  let view: AdminUserView;
  try {
    view = await getAdminUser(userId);
  } catch (err) {
    append(
      root,
      panel(
        "Person behind this account",
        "The support view could not be read.",
        problemNotice(err, "the user support view"),
      ),
    );
    return;
  }

  append(
    root,
    panel(
      "Person behind this account",
      "Read-only. No field here is writable, and no personal data reaches this surface: the e-mail address, name and date of birth are sealed and have no route to this console.",
      fields(
        field("User", el("code", {}, view.user_id)),
        field("User status", pill(view.user_status, view.user_status.toLowerCase())),
        field(
          "E-mail verified",
          view.email_verified
            ? "yes — the identity provider asserted it; the address itself is sealed"
            : "no — the identity provider has not asserted a verified address",
        ),
        field("Identity", el("code", {}, `${view.idp_issuer} / ${view.idp_subject}`)),
        field("Signed up", formatInstant(view.created_at)),
        field("Active sessions", String(view.active_sessions)),
        field("Verification", verificationValue(view)),
        field("Audit stream", el("code", {}, view.audit_stream)),
      ),
      profilePanel(view),
      onboardingPanel(view),
      restrictionsPanel(view),
      acceptancesPanel(view),
      otherAccountsPanel(ctx, view, account.id),
      closurePanel(ctx, view),
    ),
  );
}

/**
 * The verification level Nodal established. `known: false` means this
 * deployment wired no resolver, which is reported as "not established" and
 * never as NONE — the two are different facts.
 */
function verificationValue(view: AdminUserView): string {
  if (!view.verification.known) {
    return "not established — this deployment wired no verification resolver. That is not the same as “no verification”.";
  }
  return view.verification.level ?? "known, but the level was not reported";
}

function profilePanel(view: AdminUserView): HTMLElement {
  const profile = view.profile;
  if (!profile) {
    return panel(
      "Profile",
      "No profile row exists for this user.",
      emptyState("The user has not completed the profile step of onboarding."),
    );
  }
  return panel(
    "Profile",
    "Product-level state only: what the person chose to be called and how they want dates and numbers rendered.",
    fields(
      field("Display name", profile.display_name ?? "not set"),
      field("Handle", profile.handle ? el("code", {}, profile.handle) : "not set"),
      field("Locale", profile.locale),
      field("Time zone", profile.time_zone),
      field("Avatar seed", el("code", {}, profile.avatar_seed)),
      field("Created", formatInstant(profile.created_at)),
      field("Updated", profile.updated_at ? formatInstant(profile.updated_at) : "never"),
    ),
  );
}

/**
 * Onboarding is timestamps, not a state machine: the steps are independent,
 * may be done in any order and cannot be undone (D-053). The view says which
 * are done and when, and never implies an order that does not exist.
 */
function onboardingPanel(view: AdminUserView): HTMLElement {
  const onboarding = view.onboarding;
  if (!onboarding) {
    return panel("Onboarding", "Not reported for this user.", emptyState("The support view carried no onboarding record."));
  }
  return panel(
    "Onboarding",
    onboarding.complete
      ? `Complete${onboarding.completed_at ? ` at ${formatInstant(onboarding.completed_at)}` : ""}.`
      : `Incomplete${onboarding.next_step ? `; the next step the product would offer is ${onboarding.next_step}` : ""}.`,
    table(
      ["Step", "Done", "When"],
      onboarding.steps.map((step) =>
        el(
          "tr",
          {},
          el("td", {}, el("code", {}, step.key)),
          el("td", {}, step.complete ? pill("done", "approved") : pill("not yet", "inactive")),
          el("td", { class: "muted" }, step.completed_at ? formatInstant(step.completed_at) : "—"),
        ),
      ),
    ),
    el(
      "p",
      { class: "muted" },
      `Started ${formatInstant(onboarding.started_at)}. The steps are independent and cannot be undone; this is a record of what happened, not a position in a queue.`,
    ),
  );
}

/**
 * The restrictions as the *customer* is told them. The free text an operator
 * wrote on a status change is deliberately not in this list — it was written
 * for other operators and appears under the account's own status above.
 */
function restrictionsPanel(view: AdminUserView): HTMLElement {
  if (view.restrictions.length === 0) {
    return panel(
      "What this person is told",
      "Nothing is restricting them.",
      emptyState("The support view reported no restriction."),
    );
  }
  return panel(
    "What this person is told",
    "The restriction messages the customer sees, in their own words. The operator-facing reason is on the account status above.",
    table(
      ["Code", "Account", "Message"],
      view.restrictions.map((r) =>
        el(
          "tr",
          { class: "blocking" },
          el("td", {}, el("code", {}, r.code)),
          el("td", {}, r.account_id ? el("code", {}, r.account_id) : el("span", { class: "muted" }, "the user, not one account")),
          el("td", {}, r.message),
        ),
      ),
    ),
  );
}

function acceptancesPanel(view: AdminUserView): HTMLElement {
  const acceptances = view.acceptances ?? [];
  if (acceptances.length === 0) {
    return panel(
      "Terms accepted",
      "No acceptance is on record.",
      emptyState("Nothing here is a claim that the person refused anything: it is that no acceptance row exists."),
    );
  }
  return panel(
    "Terms accepted",
    "Each acceptance names the exact document version and its content hash, so what was agreed to can be reproduced.",
    table(
      ["Document", "Version", "Content hash", "Accepted"],
      acceptances.map((a) =>
        el(
          "tr",
          {},
          el("td", {}, el("code", {}, a.document_id)),
          el("td", {}, a.version),
          el("td", { class: "muted" }, el("code", {}, a.content_hash)),
          el("td", { class: "muted" }, formatInstant(a.accepted_at)),
        ),
      ),
    ),
  );
}

/** The person's other accounts, so an operator sees the whole relationship. */
function otherAccountsPanel(ctx: ViewContext, view: AdminUserView, currentId: string): HTMLElement {
  const others = view.accounts.filter((a) => a.id !== currentId);
  if (others.length === 0) {
    return panel("Other accounts", "This is the only account this person holds.", emptyState("No other account."));
  }
  return panel(
    "Other accounts",
    `${others.length} further account(s) belong to the same person.`,
    table(
      ["Account", "Kind", "Status", "Reason", ""],
      others.map((a) =>
        el(
          "tr",
          {},
          el("td", {}, el("code", {}, a.id)),
          el("td", {}, a.kind),
          el("td", {}, pill(a.status, a.status.toLowerCase())),
          el("td", { class: "muted" }, a.status_reason ?? "—"),
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
  );
}

/**
 * The closure request, and the operator's decision on it.
 *
 * Three properties the console must not blur, each enforced by the server and
 * each stated here before the form is offered:
 *
 *   - the request is the **user's**. An operator decides one; none of them can
 *     start one, and no control here could;
 *   - EFFECT is refused until the cooling-off period has passed, by the service
 *     and again by the database. The console does not offer it before then, and
 *     says how long is left rather than leaving the button mysteriously absent;
 *   - an operator may not decide a request of their own, which is the same rule
 *     dual control applies everywhere else on this console.
 */
function closurePanel(ctx: ViewContext, view: AdminUserView): HTMLElement {
  const request = view.closure_request;
  if (!request) {
    return panel(
      "Closure request",
      "This person has not asked to close their account.",
      emptyState("There is no request to decide. An operator cannot open one: closure is the customer's to request."),
    );
  }
  const now = ctx.now();
  const summary = fields(
    field("Request", el("code", {}, request.id)),
    field("State", pill(request.state, request.state.toLowerCase())),
    field("Requested", formatInstant(request.requested_at)),
    field(
      "Cooling-off ends",
      `${formatInstant(request.cooling_off_until)} (${relativeInstant(request.cooling_off_until, now)})`,
    ),
    field("Decided", request.decided_at ? formatInstant(request.decided_at) : "not yet"),
    field("Decision reason", request.decided_reason ?? "—"),
  );

  if (request.state !== "PENDING") {
    return panel(
      "Closure request",
      `Already ${request.state.toLowerCase()}. There is nothing left to decide.`,
      summary,
    );
  }

  const decisions = availableDecisions(request);
  const refusal = closureRefusal(ctx, view, request);
  if (refusal) {
    return panel("Closure request", "Open, and not yours to decide.", summary, notice("info", refusal));
  }

  return panel(
    "Closure request",
    "Open. Cancelling withdraws it on the person's behalf; refusing declines it with a reason they will see; effecting it closes the account.",
    summary,
    request.effectable
      ? null
      : notice(
          "info",
          `EFFECT is not offered yet: the cooling-off period runs until ${formatInstant(request.cooling_off_until)} (${formatDuration(Math.max(0, Math.floor((new Date(request.cooling_off_until).getTime() - now.getTime()) / 1000)))} left). The service and the database both refuse it before then.`,
        ),
    commandForm({
      title: "Decide this closure request",
      description:
        "Recorded on the request and in the audit stream, with your subject id as the decider. The person is told the reason for a refusal.",
      submitLabel: "Record the decision",
      variant: "danger",
      warning:
        "EFFECT closes the account. It is not a status change you can walk back from this console: reopening is a new relationship, not an undo.",
      fields: [
        { name: "decision", label: "Decision", options: decisions, ...(decisions[0] ? { value: decisions[0] } : {}) },
        {
          name: "reason",
          label: "Reason",
          multiline: true,
          minLength: ctx.authority.doc.min_reason_length,
          hint: `At least ${ctx.authority.doc.min_reason_length} characters. Recorded on the request; a refusal's reason is shown to the person.`,
        },
      ],
      onSubmit: async (values, key) => {
        const decision = values["decision"] ?? "";
        if (!isClosureDecision(decision)) {
          throw new Error(`${decision} is not a decision this route accepts.`);
        }
        try {
          const updated = await decideClosure(view.user_id, { decision, reason: values["reason"] ?? "" }, key);
          ctx.report(
            notice(
              "info",
              `The closure request for ${updated.user_id} is now ${updated.closure_request?.state ?? "decided"}; the user is ${updated.user_status}.`,
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

/** CANCEL and REFUSE always; EFFECT only once the cooling-off period passed. */
function availableDecisions(request: NonNullable<AdminUserView["closure_request"]>): string[] {
  return request.effectable ? ["CANCEL", "REFUSE", "EFFECT"] : ["CANCEL", "REFUSE"];
}

function isClosureDecision(value: string): value is ClosureDecisionName {
  return value === "CANCEL" || value === "REFUSE" || value === "EFFECT";
}

/**
 * Why this operator may not decide this request, or null when they may.
 *
 * `POST /v1/admin/users/{userId}/closure` takes `account:freeze` and a step-up
 * — the same permission and window as the account status change, because that
 * is what EFFECT ends up performing — and the service refuses an operator
 * deciding their own request.
 */
function closureRefusal(
  ctx: ViewContext,
  view: AdminUserView,
  request: NonNullable<AdminUserView["closure_request"]>,
): string | null {
  void request;
  if (view.user_id.toLowerCase() === ctx.session.principal.subjectId.toLowerCase()) {
    return "This is your own closure request. The service refuses an operator deciding their own, for the same reason dual control refuses a self-approval; ask a different operator.";
  }
  const permitted = allowedWrites(ctx.session.principal, "accounts", ctx.now(), ctx.authority);
  if (!permitted.includes("account.status")) {
    return `${reasonText("MISSING_PERMISSION")} Deciding a closure request needs account:freeze and a recent multi-factor sign-in — the same as an account status change, because effecting one performs exactly that.`;
  }
  return null;
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
 * The agents acting for this account: `GET /v1/admin/agents?account_id=`.
 *
 * Three things this panel keeps apart, because conflating any two of them
 * produces a wrong conclusion about whether anything is happening:
 *
 *   - **status** is the product's word for the lifecycle — STOPPED means
 *     created and never enabled, ENABLED means the owner granted it the right
 *     to be evaluated;
 *   - **runtime** is what is actually evaluating and executing. NOT_DEPLOYED
 *     means this deployment runs no such worker, so an agent can be ENABLED,
 *     correct, and evaluated by nothing at all;
 *   - **authority** is how far it may go on its own, from the ladder the code
 *     enforces. A level this build does not permit is shown with the capability
 *     it would need rather than hidden.
 *
 * Budgets are exact base-unit Credit strings and `budget.source` is rendered,
 * so a zero that was never measured is never read as a zero that was.
 */
async function renderAgents(ctx: ViewContext, root: HTMLElement, accountId: string): Promise<void> {
  let page: AgentPage;
  try {
    page = await listAgents({ accountId, includeArchived: false, limit: 100 });
  } catch (err) {
    append(
      root,
      panel(
        "Agents acting for this account",
        "The agents could not be read.",
        problemNotice(err, "this account's agents"),
      ),
    );
    return;
  }

  append(
    root,
    panel(
      "Agents acting for this account",
      `${page.items.length} live agent(s). Archived agents are not listed.`,
      page.items.length === 0
        ? emptyState("This account has no agent. The server returned an empty list; this is not a failure to read one.")
        : table(
            ["Agent", "Status", "Stage", "Authority", "Runtime", "Budget", ""],
            page.items.flatMap((a) => agentRows(ctx, a)),
          ),
      authorityLadder(page),
    ),
  );
}

function agentRows(ctx: ViewContext, agent: Agent): HTMLElement[] {
  const row = el(
    "tr",
    { class: agent.status === "PAUSED" || agent.status === "FAILED" ? "blocking" : "" },
    el("td", {}, agent.name, el("br"), el("code", { class: "muted" }, agent.id)),
    el("td", {}, pill(agent.status, agent.status.toLowerCase())),
    el("td", {}, agent.stage, agent.mode ? el("span", { class: "muted" }, ` / ${agent.mode}`) : null),
    el(
      "td",
      {},
      agent.authority.name,
      agent.authority.enabled
        ? null
        : pill("not permitted here", "inactive", `Needs ${agent.authority.required_capability ?? "a capability this build does not have"}.`),
    ),
    el(
      "td",
      { class: "muted" },
      `evaluator ${agent.runtime.evaluator}, executor ${agent.runtime.executor}`,
      el("br"),
      agent.runtime.detail,
    ),
    el("td", { class: "muted" }, budgetText(agent)),
    el("td", {}, pauseControl(ctx, agent)),
  );

  const detail = el("tr", { class: "detail" });
  const cell = el("td", { colspan: "7" });
  append(
    cell,
    fields(
      field("Strategy", el("code", {}, `${agent.strategy_id} @ ${agent.strategy_version_id}`)),
      field("Per-trade cap", `${quantityText(agent.limits.per_trade_cap_credits)} Credit base units`),
      field("Daily loss stop", `${quantityText(agent.limits.daily_loss_stop_credits)} Credit base units`),
      field("Maximum position share", `${String(agent.limits.max_position_share_bps)} bps`),
      field(
        "Schedule",
        agent.limits.schedule.kind === "MANUAL"
          ? "MANUAL — it evaluates only when the owner runs it"
          : `INTERVAL — every ${String(agent.limits.schedule.interval_minutes ?? 0)} minutes`,
      ),
      field("Allowed assets", String(agent.limits.allowed_asset_ids.length)),
      field("Runs", `${String(agent.runs_total ?? 0)}${agent.last_run_at ? `, last ${formatInstant(agent.last_run_at)} (${agent.last_run_status ?? "status not reported"})` : ", none recorded"}`),
      field("Last heartbeat", agent.runtime.last_heartbeat ? formatInstant(agent.runtime.last_heartbeat) : "never"),
      agent.pause ? field("Paused", pauseText(agent)) : null,
    ),
  );
  append(detail, cell);
  return [row, detail];
}

/** The ceiling and what has been used, with where the used figure came from. */
function budgetText(agent: Agent): string {
  const granted = quantityText(agent.budget.granted_credits);
  const used = quantityText(agent.budget.used_credits);
  switch (agent.budget.source) {
    case "NO_RUNS_RECORDED":
      return `${used} of ${granted} — the agent has never run, so nothing was measured`;
    case "NO_INTENTS_CREATED":
      return `${used} of ${granted} — it ran and created no intent, so nothing was committed`;
    default:
      return `${used} of ${granted}, from committed intents`;
  }
}

function pauseText(agent: Agent): string {
  const p = agent.pause;
  if (!p) return "—";
  const orders =
    p.open_orders_policy === "CANCEL_CANCELABLE"
      ? "cancelable open orders were cancelled"
      : "open orders were left alone";
  return `${formatInstant(p.paused_at)} by a ${p.paused_by_actor_type} actor (${p.reason_code}): ${p.reason}. ${orders}.`;
}

function quantityText(value: string): string {
  return isQuantity(value) ? groupDigits(value) : value;
}

/**
 * The authority ladder this build permits, returned whole rather than filtered,
 * so an operator sees which rungs exist and which are switched off here.
 */
function authorityLadder(page: AgentPage): HTMLElement {
  const disabled = page.authority_levels.filter((l) => !l.enabled);
  if (disabled.length === 0) {
    return el("p", { class: "muted" }, "Every rung of the authority ladder is permitted in this deployment.");
  }
  return notice(
    "info",
    el("strong", {}, `${disabled.length} rung(s) of the authority ladder are not permitted in this deployment.`),
    el(
      "ul",
      { class: "reasons" },
      ...disabled.map((l) =>
        el("li", {}, `${l.name} — needs ${l.required_capability ?? "a capability this build does not declare"}`),
      ),
    ),
    el(
      "p",
      { class: "muted" },
      "A rung is off because its capability gate is not active, which is the gates surface's answer and not a property of any agent.",
    ),
  );
}

/**
 * The operator pause.
 *
 * It stops new risk from one agent; it does not unwind what the agent already
 * did, and the API leaves open orders alone. Both permissions are required (see
 * AGENT_PAUSE_PERMISSIONS) and so is a step-up, which the route demands even
 * though the `kill.activate` write it borrows its permission from does not —
 * activating a kill switch is the fast path, and pausing one customer's agent
 * deliberately is not the same act.
 */
function pauseControl(ctx: ViewContext, agent: Agent): HTMLElement {
  if (!PAUSABLE_STATUSES.includes(agent.status)) {
    return actionButton({
      label: "Pause",
      onClick: () => undefined,
      allowed: false,
      reason: `This agent is ${agent.status}. A pause acts on an agent that could still be evaluated.`,
    });
  }
  const refusal = pauseRefusal(ctx);
  return actionButton({
    label: "Pause",
    variant: "danger",
    allowed: refusal === null,
    ...(refusal === null ? {} : { reason: refusal }),
    onClick: () => openPauseForm(ctx, agent),
  });
}

function pauseRefusal(ctx: ViewContext): string | null {
  const now = ctx.now();
  const missing = AGENT_PAUSE_PERMISSIONS.filter((p) => !holds(ctx.session.principal, p, now, ctx.authority));
  if (missing.length > 0) {
    return `${reasonText("MISSING_PERMISSION")} An operator pause needs both ${AGENT_PAUSE_PERMISSIONS.join(" and ")}; you are missing ${missing.join(" and ")}. The route is floored on kill:activate and internal/agents then demands agent:pause and an OPERATOR actor.`;
  }
  if (!steppedUp(ctx.session.principal, stepUpWindowSeconds(ctx), now)) {
    return `${reasonText("STEP_UP_REQUIRED")} Pausing someone else's agent needs a recent multi-factor sign-in, even though activating a kill switch does not: stopping all new risk is the fast path, and stopping one customer's agent is a deliberate act.`;
  }
  return null;
}

/**
 * The step-up window the API boundary enforces, read from the document rather
 * than written down again.
 *
 * `internal/httpapi.stepUpMaxAge` is the ceiling for every route marked
 * `StepUp: true`, and `account.status` is the declared write that carries it
 * (`PostAdminAccountsAccountIdStatus` is the route `authz.go` names when it
 * explains the closure route's requirement). A deployment may tighten the
 * window through CP_AUTH_STEP_UP_MAX_AGE and can never widen it, so treating
 * this as the window is never more permissive than the server — and the server
 * checks again regardless.
 */
function stepUpWindowSeconds(ctx: ViewContext): number {
  const write = ctx.authority.surface("accounts")?.writes?.find((w) => w.id === "account.status");
  return write?.step_up_max_age_seconds ?? 0;
}

function openPauseForm(ctx: ViewContext, agent: Agent): void {
  ctx.report(
    commandForm({
      title: `Pause ${agent.name}`,
      description:
        "Writes an agent_pauses row under the OPERATOR reason code and moves the agent to PAUSED, so the owner's own history shows plainly that somebody else stopped it.",
      submitLabel: "Pause this agent",
      variant: "danger",
      warning:
        "Open orders are left alone. This stops the agent from acting again; it does not unwind anything it has already done, and the owner can see that an operator did it.",
      fields: [
        {
          name: "reason",
          label: "Reason",
          multiline: true,
          minLength: ctx.authority.doc.min_reason_length,
          hint: `At least ${ctx.authority.doc.min_reason_length} characters. Recorded on the pause and visible to the agent's owner.`,
        },
      ],
      onSubmit: async (values, key) => {
        try {
          const updated = await pauseAgent(agent.id, values["reason"] ?? "", key);
          ctx.report(notice("warn", `${updated.name} is now ${updated.status}. Its owner can see that an operator paused it.`));
          ctx.refresh();
        } catch (err) {
          throw new Error(describeProblem(err));
        }
      },
    }),
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
