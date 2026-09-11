/**
 * The console shell: identity banner, navigation, routing and the one place a
 * failure is rendered.
 *
 * Navigation is built from `visibleSurfaces()`, so an operator is never shown
 * a page their roles cannot read. A surface they cannot read is listed as
 * unavailable with the permission it needs rather than silently omitted — an
 * operator who cannot find reconciliation needs to know whether the page is
 * missing or their access is.
 */
import type { Route, ViewContext } from "./context.ts";
import { visibleSurfaces } from "./decide.ts";
import { append, clear, el, notice, pill } from "./dom.ts";
import { formatDuration, formatInstant, secondsUntil } from "./format.ts";
import type { Session } from "./session.ts";
import { stepUpFresh } from "./session.ts";
import type { AuthorityIndex } from "./authority.ts";
import { renderAccounts } from "./views/accounts.ts";
import { renderActions } from "./views/actions.ts";
import { approvalSurface } from "./views/approvals.ts";
import { renderBreakGlass } from "./views/breakglass.ts";
import { renderGates } from "./views/gates.ts";
import { renderKillSwitches } from "./views/killswitches.ts";
import { renderProviders } from "./views/providers.ts";
import { renderReconciliation } from "./views/reconciliation.ts";

export const ROUTES: readonly Route[] = [
  {
    path: "actions",
    title: "Action queue",
    surface: "actions",
    description: "Propose, review, approve, reject and execute controlled administrative actions under dual control.",
    render: renderActions,
  },
  {
    path: "reconciliation",
    title: "Reconciliation",
    surface: "reconciliation",
    description: "Open and material mismatches, the evidence behind each, and resolution under dual control.",
    render: renderReconciliation,
  },
  {
    path: "kill-switches",
    title: "Kill switches",
    surface: "kill_switches",
    description: "Stop new risk immediately; release it deliberately.",
    render: renderKillSwitches,
  },
  {
    path: "gates",
    title: "Capability gates",
    surface: "gates",
    description: "The five-condition switch that decides whether a live-money capability may be exercised.",
    render: renderGates,
  },
  {
    path: "accounts",
    title: "Accounts",
    surface: "accounts",
    description:
      "Search, and read one account whole: its owner, status, restrictions, Credit balance, open reconciliation, the actions that named it, the capabilities it may use, its agents and its audit trail. The writes are the status machine, a closure request the customer opened, and pausing one agent.",
    render: renderAccounts,
  },
  {
    path: "withdrawals",
    title: "Withdrawals",
    surface: "withdrawals",
    description: "Payout approvals, which are controlled actions with their own approve permission.",
    render: approvalSurface(
      "withdrawals",
      "Withdrawal approvals",
      "A withdrawal is reviewed by compliance or finance and confirmed by a distinct principal holding withdrawal:approve.",
    ),
  },
  {
    path: "envelopes",
    title: "Capital envelopes",
    surface: "envelopes",
    description: "Changes to an envelope's allocation, caps and allowed venues.",
    render: approvalSurface(
      "envelopes",
      "Envelope authority changes",
      "Changing what capital an agent may command is dual-controlled: risk proposes, a distinct principal approves.",
    ),
  },
  {
    path: "agent-promotion",
    title: "Agent promotion",
    surface: "agent_promotion",
    description: "Promotion up the SHADOW to CANARY to LIMITED to LIVE ladder.",
    render: approvalSurface(
      "agent_promotion",
      "Agent promotions",
      "Moving an agent up the ladder widens what it may propose. It is dual-controlled and the agent runtime verifies the approval before it takes effect.",
    ),
  },
  {
    path: "break-glass",
    title: "Break-glass",
    surface: "break_glass",
    description: "Request and grant the time-boxed elevation that carries the approve side of dual control.",
    render: renderBreakGlass,
  },
  {
    path: "providers",
    title: "Providers",
    surface: "providers",
    description: "Provider health, verification labels and whether each slot is fake, sandbox or live.",
    render: renderProviders,
  },
];

export interface ShellOptions {
  readonly authority: AuthorityIndex;
  readonly session: Session;
  readonly now: () => Date;
  readonly root: HTMLElement;
}

export function mount(opts: ShellOptions): void {
  const { authority, session, now, root } = opts;
  const visible = new Set(visibleSurfaces(session.principal, now(), authority));

  const banner = el("div", { class: "report", "aria-live": "polite" });
  const outlet = el("main", { class: "outlet" });
  const nav = el("nav", { class: "nav" });

  const ctx: ViewContext = {
    authority,
    session,
    now,
    refresh: () => void render(),
    navigate: (route: string) => {
      window.location.hash = `#${route}`;
    },
    report: (message: HTMLElement) => {
      clear(banner);
      append(banner, message);
      banner.scrollIntoView({ block: "nearest" });
    },
  };

  function currentRoute(): Route | undefined {
    const hash = window.location.hash.replace(/^#/, "");
    const path = hash.split("?")[0] ?? "";
    return ROUTES.find((r) => r.path === path);
  }

  function buildNav(active: Route | undefined): void {
    clear(nav);
    for (const route of ROUTES) {
      const readable = visible.has(route.surface);
      const surface = authority.surface(route.surface);
      const item = el("a", {
        class: `nav-item${active?.path === route.path ? " on" : ""}${readable ? "" : " unavailable"}`,
        href: `#${route.path}`,
        ...(readable
          ? {}
          : {
              title: `Needs one of: ${surface?.read_any_of.join(", ") ?? "an unknown permission"}`,
              "aria-disabled": "true",
            }),
      });
      append(item, route.title);
      if (!readable) append(item, pill("no access", "inactive"));
      nav.appendChild(item);
    }
  }

  async function render(): Promise<void> {
    const route = currentRoute() ?? ROUTES.find((r) => visible.has(r.surface)) ?? ROUTES[0];
    if (!route) return;
    if (!window.location.hash) window.location.hash = `#${route.path}`;
    buildNav(route);
    clear(outlet);
    append(
      outlet,
      el("header", { class: "page-head" }, el("h1", {}, route.title), el("p", { class: "muted" }, route.description)),
    );
    const body = el("div", { class: "page-body" });
    append(outlet, body);

    if (!visible.has(route.surface)) {
      const surface = authority.surface(route.surface);
      append(
        body,
        notice(
          "warn",
          `You cannot read this surface. It needs one of: ${surface?.read_any_of.join(", ") ?? "an unknown permission"}. Your roles are ${session.principal.roles.join(", ") || "none"}.`,
        ),
      );
      return;
    }

    try {
      await route.render(ctx, body);
    } catch (err) {
      clear(body);
      append(body, notice("error", err instanceof Error ? err.message : String(err)));
    }
  }

  clear(root);
  append(root, identityBar(ctx), nav, banner, outlet);
  window.addEventListener("hashchange", () => void render());
  void render();
}

/** Who you are, what you hold, and how fresh your authentication is. */
function identityBar(ctx: ViewContext): HTMLElement {
  const { session, authority } = ctx;
  const now = ctx.now();
  const elevated = session.principal.roles.includes(authority.doc.break_glass.role);
  const stepUp = stepUpFresh(session, now);
  const stepUpLeft = secondsUntil(session.stepUpValidUntil, now);

  const bar = el("div", { class: "identity" });
  append(
    bar,
    el("span", { class: "brand" }, "Operator console"),
    el("code", { class: "subject", title: "Your users.id. This is the identity recorded as proposer or approver." }, session.principal.subjectId),
    el("span", { class: "roles" }, ...session.principal.roles.map((r) => pill(r, r === authority.doc.break_glass.role ? "dual" : "role"))),
    stepUp
      ? pill(
          `step-up ok${stepUpLeft === null ? "" : ` (${formatDuration(stepUpLeft)} left)`}`,
          "active",
          `Authenticated ${formatInstant(session.raw.auth_time)} with ${session.principal.amr.join(", ")}.`,
        )
      : pill(
          "step-up needed",
          "inactive",
          "Several operator commands need a multi-factor sign-in within the last few minutes. Sign in again with step-up to perform them.",
        ),
    elevated ? pill("break-glass held", "dual", "The API does not report this elevation's deadline; the server enforces it.") : null,
    el("a", { class: "signin", href: "/v1/auth/login?step_up=true" }, "Re-authenticate with step-up"),
  );
  return bar;
}
