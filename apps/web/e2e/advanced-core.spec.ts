/**
 * The advanced (sandbox) area against the real API (ADR-0020).
 *
 * Like critical-paths.spec.ts, nothing here is stubbed: the pages talk to a
 * running api over a seeded database, and every assertion compares what a
 * page shows with what the backend answered, including where it answers no.
 * Two answers this area exists to show honestly are asserted directly:
 *
 *   - LIVE is refused for every advanced capability, with the reason
 *     LIVE_NOT_IMPLEMENTED, and the capability table says exactly that;
 *   - buying power V2 renders either the backend's breakdown, figure for
 *     figure, or the backend's refusal with its code (UNSUPPORTED while the
 *     sandbox dollar is not provisioned) — never a zero standing in for a
 *     figure the backend did not compute.
 *
 * And one rule is checked on every core page against the responses that page
 * itself received: no money-shaped figure appears that the API did not send.
 */
import { randomUUID } from "node:crypto";

import { expect, test, type Locator, type Page, type Response } from "@playwright/test";

import { goto } from "./pace.ts";

/** The accessible name of the notice every page of the area opens with. */
const NOTICE = "Simulated value — sandbox only";

const CORE_PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/advanced/overview", heading: "Overview" },
  { path: "/advanced/buying-power", heading: "Buying power V2" },
  { path: "/advanced/activity", heading: "Economic activity" },
  { path: "/advanced/instruments", heading: "Instruments" },
];

/** The sixteen components of a buying power V2 answer, each rendered with data-field. */
const BUYING_POWER_FIELDS = [
  "trade_buying_power",
  "risk_increasing_buying_power",
  "margin_available",
  "withdrawable",
  "gross_equity",
  "eligible_collateral",
  "haircuts",
  "pending_bridge_haircut",
  "borrow_liabilities",
  "eligible_equity",
  "initial_margin_used",
  "maintenance_margin",
  "open_order_reservations",
  "pending_transfer_reservations",
  "pending_withdrawal_reservations",
  "strategy_reservations",
] as const;

interface Problem {
  readonly code: string;
  readonly detail?: string;
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

/** The account the application reads: the first one the backend lists for this session. */
async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/accounts");
  expect(response.ok()).toBeTruthy();
  const accounts = (await response.json()) as Array<{ id: string }>;
  const id = accounts[0]?.id;
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

/**
 * Waits until a page has its answer on screen. `toHaveCount(0)` passes the
 * instant it is evaluated, so an absence is only worth asserting once the
 * network is quiet and nothing says it is still loading (F-32, F-46).
 */
async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
}

/** A wire USD string as the Money components render it: grouping and a sign, nothing else. */
function renderedUsd(wire: string): string {
  const negative = wire.startsWith("-");
  const [whole = "", cents = ""] = (negative ? wire.slice(1) : wire).split(".");
  const digits = whole.replace(/^0+(?=[0-9])/, "");
  const grouped = digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const zero = /^0+$/.test(digits) && /^0+$/.test(cents);
  return `${negative && !zero ? "-" : ""}$${grouped}.${cents}`;
}

const USD_WIRE = /^-?[0-9]+\.[0-9]{2}$/;

/** Every string in a JSON body shaped like a USD amount. */
function collectUsd(value: unknown, into: Set<string>): void {
  if (typeof value === "string") {
    if (USD_WIRE.test(value)) into.add(value);
  } else if (Array.isArray(value)) {
    for (const item of value) collectUsd(item, into);
  } else if (typeof value === "object" && value !== null) {
    for (const item of Object.values(value)) collectUsd(item, into);
  }
}

/**
 * Navigates and keeps the JSON body of every /v1/advanced/ response the page
 * itself received, keyed by path. Observing the page's own responses — rather
 * than asking again — means the comparison is with exactly what it rendered.
 */
async function visit(page: Page, path: string): Promise<Map<string, unknown>> {
  const pending: Array<Promise<readonly [string, unknown]>> = [];
  const listener = (response: Response): void => {
    const url = new URL(response.url());
    if (!url.pathname.startsWith("/v1/advanced/")) return;
    pending.push(
      response.json().then(
        (body: unknown) => [url.pathname, body] as const,
        () => [url.pathname, undefined] as const,
      ),
    );
  };
  page.on("response", listener);
  await goto(page, path);
  await settle(page);
  page.off("response", listener);
  return new Map(await Promise.all(pending));
}

/** A refusal is shown with the backend's own code, inside the panel it concerns. */
async function expectRefusal(panel: Locator, problem: Problem): Promise<void> {
  await expect(panel.locator(".explain-meta")).toContainText(`code ${problem.code}`);
}

test.describe("the advanced (sandbox) area", () => {
  for (const { path, heading } of CORE_PAGES) {
    test(`${heading} renders under the simulated-value notice`, async ({ page }) => {
      await goto(page, path);
      await expect(page.getByRole("heading", { level: 1, name: heading, exact: true })).toBeVisible();
      await expect(page.locator("h1")).toHaveCount(1);

      const notice = page.getByRole("note", { name: NOTICE, exact: true });
      await expect(notice).toBeVisible();
      await expect(notice).toContainText("no economic substance");
      await expect(notice).toContainText("nothing here moves real money");
      await expect(notice).toContainText("LIVE is not implemented in this build");

      await settle(page);
      await expect(page.locator(".malformed")).toHaveCount(0);
      await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
    });
  }

  test("/advanced opens on the portfolio, and the overview is one link away", async ({ page }) => {
    await goto(page, "/advanced");
    await expect(page).toHaveURL(/\/advanced\/portfolio$/);
    await expect(page.getByRole("heading", { level: 1, name: "Portfolio", exact: true })).toBeVisible();
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Overview", exact: true })
      .click();
    await expect(page).toHaveURL(/\/advanced\/overview$/);
    await expect(page.getByRole("heading", { level: 1, name: "Overview", exact: true })).toBeVisible();
  });

  test("the area is reachable from the main navigation, and each core section from its own", async ({ page }) => {
    await goto(page, "/");
    await expect(page.locator("h1")).toHaveCount(1);
    await page
      .getByRole("navigation", { name: "Sections", exact: true })
      .getByRole("link", { name: "Advanced (sandbox)", exact: true })
      .click();
    // The area opens on the account's portfolio (ADVANCED_DEFAULT_SECTION).
    await expect(page.getByRole("heading", { level: 1, name: "Portfolio", exact: true })).toBeVisible();

    const area = page.getByRole("navigation", { name: "Advanced area", exact: true });
    for (const { heading } of CORE_PAGES) {
      await area.getByRole("link", { name: heading, exact: true }).click();
      await expect(page.getByRole("heading", { level: 1, name: heading, exact: true })).toBeVisible();
    }
  });
});

test("LIVE is refused for every capability with LIVE_NOT_IMPLEMENTED, and the table says so", async ({ page }) => {
  const response = await page.request.get("/v1/advanced/capabilities");
  expect(response.status(), "the capability table is served").toBe(200);
  const body = (await response.json()) as {
    environment: string;
    value_domain: string;
    items: Array<{ capability: string; decisions: Array<{ mode: string; allowed: boolean; reasons: string[] }> }>;
  };
  expect(body.value_domain).toBe("SIMULATED");
  expect(body.items.length, "the build declares advanced capabilities").toBeGreaterThan(0);

  // First the backend: this is the claim the page has to repeat, not invent.
  for (const item of body.items) {
    const live = item.decisions.find((decision) => decision.mode === "LIVE");
    expect(live, `${item.capability} reports a LIVE decision`).toBeTruthy();
    expect(live?.allowed, `${item.capability} refuses LIVE`).toBe(false);
    expect(live?.reasons, `${item.capability} says why`).toContain("LIVE_NOT_IMPLEMENTED");
  }

  // Then the page: one row per capability, and every cell says what the backend decided.
  await goto(page, "/advanced/overview");
  await settle(page);
  const table = page.getByRole("table", { name: "Decision for every capability and mode", exact: true });
  await expect(table.locator("tbody tr")).toHaveCount(body.items.length);
  for (const item of body.items) {
    const live = table.locator(`td[data-capability="${item.capability}"][data-mode="LIVE"]`);
    await expect(live).toContainText("Refused");
    await expect(live).toContainText("LIVE_NOT_IMPLEMENTED");
    for (const decision of item.decisions) {
      const cell = table.locator(`td[data-capability="${item.capability}"][data-mode="${decision.mode}"]`);
      await expect(cell).toContainText(decision.allowed ? "Allowed" : "Refused");
      for (const reason of decision.reasons) {
        await expect(cell).toContainText(reason);
      }
    }
  }

  const deployment = page.getByRole("region", { name: "This deployment", exact: true });
  await expect(deployment).toContainText(body.environment);
  await expect(deployment).toContainText("value domain SIMULATED");
});

test("buying power V2 shows the backend's breakdown, or its refusal and code — never a zero in place of either", async ({
  page,
}) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/advanced/buying-power?account_id=${id}`);
  const body: unknown = await response.json();

  await goto(page, "/advanced/buying-power");
  await settle(page);

  if (response.ok()) {
    const answer = body as Record<string, unknown>;
    expect(answer["value_domain"]).toBe("SIMULATED");
    for (const field of BUYING_POWER_FIELDS) {
      const wire = answer[field];
      expect(typeof wire, `${field} is a decimal string`).toBe("string");
      await expect(page.locator(`[data-field="${field}"]`)).toHaveText(renderedUsd(wire as string));
    }
    await expect(page.getByText(answer["policy_version"] as string, { exact: false }).first()).toBeVisible();
    return;
  }

  // The refusal branch. In a deployment without the sandbox dollar this is
  // UNSUPPORTED; whatever the code, it is the backend's, and it is on screen.
  expect(isProblem(body), "a refusal is a problem document with a stable code").toBe(true);
  const problem = body as Problem;
  const refusal = page.locator(".explain");
  await expect(refusal).toBeVisible();
  await expectRefusal(page.locator("main"), problem);
  if (problem.detail !== undefined && problem.detail !== "") {
    await expect(refusal).toContainText(problem.detail);
  }
  if (problem.code === "UNSUPPORTED") {
    await expect(refusal).toContainText("not an account worth zero");
  }
  // No component was drawn, and nothing money-shaped stands in for one.
  await expect(page.locator("[data-field]")).toHaveCount(0);
  const text = await page.locator("main").innerText();
  expect(text, "a refused buying power shows no figure").not.toMatch(/\$\s?[0-9]/);
});

test("economic activity shows exactly the rows the API returned for the account", async ({ page }) => {
  const bodies = await visit(page, "/advanced/activity");

  const registers: ReadonlyArray<{
    readonly path: string;
    readonly panel: string;
    readonly empty: string;
    readonly older: string | undefined;
  }> = [
    { path: "/v1/advanced/intents", panel: "Economic intents", empty: "No economic intents", older: "Older intents" },
    { path: "/v1/advanced/effects", panel: "Economic effects", empty: "No economic effects", older: "Older effects" },
    { path: "/v1/advanced/risk/decisions", panel: "Risk Kernel V2 decisions", empty: "No decisions recorded", older: undefined },
  ];

  for (const register of registers) {
    const body = bodies.get(register.path);
    expect(body, `the page asked for ${register.path}`).toBeTruthy();
    const panel = page.getByRole("region", { name: register.panel, exact: true });
    if (isProblem(body)) {
      await expectRefusal(panel, body);
      continue;
    }
    const rows = body as { items: Array<{ id: string }>; next_cursor?: string | null };
    if (rows.items.length === 0) {
      await expect(panel).toContainText(register.empty);
      continue;
    }
    for (const item of rows.items) {
      await expect(panel.getByText(item.id, { exact: true }).first()).toBeVisible();
    }
    if (register.older !== undefined) {
      const older = panel.getByRole("button", { name: register.older, exact: true });
      if (rows.next_cursor === null) {
        await expect(older).toBeDisabled();
      } else {
        await expect(older).toBeEnabled();
      }
    }
  }
});

test("an unknown intent is answered NOT_FOUND, and a listed one opens with its content hash", async ({ page }) => {
  // A well-formed UUIDv7 that nobody recorded: the version nibble of a random v4 set to 7.
  const v4 = randomUUID();
  const unknown = `${v4.slice(0, 14)}7${v4.slice(15)}`;
  await goto(page, `/advanced/activity/intents/${unknown}`);
  await expect(page.getByRole("heading", { level: 1, name: "Economic intent", exact: true })).toBeVisible();
  await settle(page);
  await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
  await expect(page.locator(".explain-meta")).toContainText("code NOT_FOUND");

  const id = await accountId(page);
  const response = await page.request.get(`/v1/advanced/intents?account_id=${id}&limit=1`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { items: Array<{ id: string; content_hash: string }> };
  const first = body.items[0];

  await goto(page, "/advanced/activity");
  await settle(page);
  const intents = page.getByRole("region", { name: "Economic intents", exact: true });
  if (first === undefined) {
    // Nothing to open: the honest outcome is the stated absence, not a skipped test.
    await expect(intents).toContainText("No economic intents");
    return;
  }
  await intents.getByRole("link", { name: first.id, exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Economic intent", exact: true })).toBeVisible();
  await settle(page);
  await expect(page.getByText(first.content_hash, { exact: true }).first()).toBeVisible();
});

test("instruments: the list, the kind filter, and one instrument's terms history", async ({ page }) => {
  const all = (await (await page.request.get("/v1/advanced/instruments?limit=200")).json()) as {
    items: Array<{ id: string; kind: string; canonical_name: string }>;
  };

  await goto(page, "/advanced/instruments");
  await settle(page);
  const panel = page.getByRole("region", { name: "Canonical instruments", exact: true });
  const first = all.items[0];
  if (first === undefined) {
    await expect(panel).toContainText("No instruments");
    return;
  }
  const table = page.getByRole("table", { name: "Instruments in this list", exact: true });
  await expect(table.locator("tbody tr")).toHaveCount(all.items.length);

  // Choosing a kind asks the backend for that kind; the rows are its answer.
  await page.getByLabel("Instrument kind").selectOption(first.kind);
  await expect(page).toHaveURL(new RegExp(`[?&]kind=${first.kind}$`));
  await settle(page);
  const filtered = (await (await page.request.get(`/v1/advanced/instruments?kind=${first.kind}&limit=200`)).json()) as {
    items: Array<{ id: string; canonical_name: string }>;
  };
  await expect(table.locator("tbody tr")).toHaveCount(filtered.items.length);

  await table.getByRole("link", { name: first.canonical_name, exact: true }).first().click();
  await expect(page.getByRole("heading", { level: 1, name: first.canonical_name, exact: true })).toBeVisible();
  await settle(page);
  const detail = (await (await page.request.get(`/v1/advanced/instruments/${first.id}`)).json()) as {
    terms_hash: string;
    terms_version: number;
    versions: Array<{ version: number; terms_hash: string }>;
  };
  await expect(page.getByText(detail.terms_hash, { exact: true }).first()).toBeVisible();
  const versions = page.getByRole("table", { name: "Terms versions", exact: true });
  await expect(versions.locator("tbody tr")).toHaveCount(detail.versions.length);
  await expect(versions.getByText("in force", { exact: true })).toHaveCount(1);
});

test("an unknown instrument kind in the address is reported and not sent", async ({ page }) => {
  await goto(page, "/advanced/instruments?kind=NOT_A_KIND");
  await settle(page);
  await expect(page.getByRole("status").filter({ hasText: "NOT_A_KIND" })).toBeVisible();
  await expect(page.getByLabel("Instrument kind")).toHaveValue("");
});

for (const { path } of CORE_PAGES) {
  test(`${path} shows no money-shaped figure the API did not return`, async ({ page }) => {
    const bodies = await visit(page, path);
    const wire = new Set<string>();
    for (const body of bodies.values()) collectUsd(body, wire);
    const allowed = new Set([...wire].map(renderedUsd));

    const text = await page.locator("main").innerText();
    for (const figure of text.match(/-?\$[0-9][0-9,]*\.[0-9]{2}/g) ?? []) {
      expect(allowed.has(figure), `${path} shows ${figure}, which no advanced response contained`).toBe(true);
    }
  });
}

test("no dead controls in the advanced area", async ({ page }) => {
  for (const { path } of CORE_PAGES) {
    await goto(page, path);
    await settle(page);

    const buttons = page.locator("main button");
    const count = await buttons.count();
    for (let i = 0; i < count; i = i + 1) {
      const button = buttons.nth(i);
      if (!(await button.isDisabled())) continue;
      // A disabled control says why, in text, next to itself.
      const describedBy = await button.getAttribute("aria-describedby");
      expect(describedBy, `${path}: a disabled button explains itself`).toBeTruthy();
      const reason = page.locator(`[id="${(describedBy as string).replace(/"/g, '\\"')}"]`);
      await expect(reason).toBeVisible();
      expect(((await reason.textContent()) ?? "").trim().length).toBeGreaterThan(10);
    }

    const links = page.locator("main a[href]");
    const linkCount = await links.count();
    for (let i = 0; i < linkCount; i = i + 1) {
      const href = (await links.nth(i).getAttribute("href")) ?? "";
      expect(href.trim() !== "" && href !== "#", `${path}: link ${String(i)} goes somewhere`).toBe(true);
    }
  }
});

test("the advanced pages reflow at 390px without sideways scrolling", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  for (const { path } of CORE_PAGES) {
    await goto(page, path);
    await settle(page);
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
  }
});
