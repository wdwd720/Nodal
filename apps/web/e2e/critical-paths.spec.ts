/**
 * The application pages, against a real backend.
 *
 * These run against a production build and a real API. Where the backend
 * refuses — a withdrawal in this deployment, a capability whose gate is closed —
 * the assertion is that the refusal is shown with its stable code, not that the
 * refusal is absent. A test that asserted a price appeared would be a test that
 * demanded the interface lie.
 *
 * # What left this file, and where it went
 *
 * D-077 removed the hosted rail (`/trade`, `/add-funds`, `/lab`, `/strategy`,
 * `/nodal-economy`, `/payouts`, `/marketplace`), so the cases that drove those
 * pages went with them rather than being retargeted at a page that does
 * something else. The cross-cutting checks moved to the files
 * `docs/product/STAGING_E2E.md` names for them: axe and the 375px reflow to
 * `accessibility.spec.ts`, the forbidden vocabulary to `honesty.spec.ts`, the
 * dead-control walk and the navigation to `controls.spec.ts`, and the new-user
 * journey to `scenarios/a-new-user.spec.ts`. One route list feeds all of them,
 * in `routes.ts`.
 *
 * The Marketplace purchase walk — buy the cheapest product, assert Credits fell
 * by exactly the price — was driven from the Credit balance on `/nodal-economy`
 * and has no page to read that balance from until the dashboard and the Buy
 * Credits screens land. It belongs to scenario B, whose branch owns both.
 */
import { expect, test, type Page } from "@playwright/test";

import { APP_ROUTES } from "./routes.ts";

async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/me");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { account_ids: string[] };
  const id = body.account_ids[0];
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

for (const route of APP_ROUTES) {
  test(`${route.heading} renders`, async ({ page }) => {
    await page.goto(route.path);
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
    // Exactly one h1 per page: the document outline is the navigation aid a
    // screen reader user actually has.
    await expect(page.locator("h1")).toHaveCount(1);
    // SETTLE before asserting an absence. `toHaveCount(0)` passes the instant
    // it is evaluated, so a check for "nothing is broken" that runs before the
    // queries resolve is only measuring how fast the test runs. Every page here
    // waits, and asserts that nothing is still loading before asserting that
    // nothing is malformed.
    await page.waitForLoadState("networkidle");
    await expect(page.locator(".loading")).toHaveCount(0);
    // Nothing renders the placeholder that would mean a formatter gave up.
    await expect(page.locator(".malformed")).toHaveCount(0);
    // And nothing renders the contract-violation notice, which is what the app
    // shows when a response does not match the API contract. A mismatch would
    // render it on every load and still pass the assertions above.
    //
    // A REFUSAL is deliberately not asserted against: this deployment refuses
    // plenty, and demanding that no refusal appeared would be demanding the
    // interface lie.
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
  });
}

test("home shows the figures the backend computed, unchanged", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/accounts/${id}/buying-power?purpose=DISPLAY`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as Record<string, string>;

  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();

  // The rendered figure is the wire string with grouping applied and nothing else.
  const rendered = (value: string): string => {
    const [whole, cents] = value.replace("-", "").split(".");
    const grouped = (whole ?? "").replace(/\B(?=(\d{3})+(?!\d))/g, ",");
    return `${value.startsWith("-") ? "-" : ""}$${grouped}.${cents ?? ""}`;
  };

  for (const field of ["portfolio_value", "buying_power", "available_now", "reserved", "pending"]) {
    const wire = body[field];
    expect(wire, `${field} present in the response`).toBeTruthy();
    await expect(
      page.locator(".panel", { hasText: "Balances" }).getByText(rendered(wire as string), { exact: true }).first(),
    ).toBeVisible();
  }

  // The snapshot instant the backend stamped is on screen, exactly. The page
  // made its own request, so its instant is not this test's instant — the
  // backend stamps `as_of` with the moment it computed the answer. What is
  // asserted is therefore that the page shows a real backend instant verbatim:
  // the machine-readable attribute and the visible exact rendering agree, it
  // parses, and it falls in the window this test was running.
  const asOf = page.locator(".panel", { hasText: "Balances" }).locator("time").first();
  const machine = await asOf.getAttribute("datetime");
  expect(machine, "the snapshot carries a machine-readable instant").toBeTruthy();
  const exact = await page.locator(".panel", { hasText: "Balances" }).locator(".as-of .mono-small").first().innerText();
  expect(exact.trim(), "the exact instant is shown unrounded beside it").toBe(`(${machine as string})`);
  const shownAt = Date.parse(machine as string);
  expect(Number.isNaN(shownAt), "the instant parses").toBe(false);
  // Bracketed with two comparisons rather than an absolute difference: the
  // source guard forbids float arithmetic anywhere in this tree.
  const observed = Date.parse(body["as_of"] as string);
  const window = 300_000;
  expect(shownAt, "the instant is not stale").toBeGreaterThan(observed - window);
  expect(shownAt, "the instant is not fabricated ahead of the backend").toBeLessThan(observed + window);
});

test("home discloses what the balance actually is", async ({ page }) => {
  await page.goto("/home");
  const disclosure = page.getByLabel("What you are actually holding");
  await expect(disclosure).toContainText("USDC");
  await expect(disclosure).toContainText("stablecoin");
  await expect(disclosure).toContainText("not a bank deposit");
});

test("portfolio shows exact units beside every valuation", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/accounts/${id}/holdings`);
  const body = (await response.json()) as { holdings: Array<{ quantity: string; symbol: string }> };

  await page.goto("/portfolio");
  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();

  for (const holding of body.holdings) {
    await expect(page.getByText(`${holding.quantity} base units`).first()).toBeVisible();
  }
  await expect(page.getByRole("link", { name: "Export as CSV" })).toHaveAttribute("href", /export\?format=csv/);
});

test("activity draws every lifecycle stage, including the ones with no rows", async ({ page }) => {
  await page.goto("/activity");
  const stages = [
    "Data event",
    "Prediction",
    "Intent",
    "Eligibility",
    "Risk",
    "Plan",
    "Execution",
    "Fill",
    "Reconciliation",
  ];
  for (const stage of stages) {
    await expect(page.getByText(stage, { exact: true }).first()).toBeVisible();
  }
  // A stage with no record says so rather than being omitted.
  await expect(page.getByText("not recorded").first()).toBeVisible();
});

test("settings lists real sessions and refuses a withdrawal with a reason", async ({ page }) => {
  const response = await page.request.get("/v1/sessions");
  const sessions = (await response.json()) as Array<{ id: string }>;

  await page.goto("/settings");
  await expect(page.getByRole("heading", { level: 1, name: "Settings and security" })).toBeVisible();
  for (const session of sessions.slice(0, 3)) {
    await expect(page.getByText(session.id, { exact: false }).first()).toBeVisible();
  }

  const withdrawals = page.locator(".panel", { hasText: "Moving assets off the platform" });
  await withdrawals.getByLabel("Amount").fill("1");
  await withdrawals.getByLabel("Destination address").fill("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU");
  await withdrawals.getByRole("button", { name: "Request withdrawal" }).click();

  await expect(withdrawals.locator(".explain-meta")).toContainText(
    /code (STEP_UP_REQUIRED|CAPABILITY_NOT_APPROVED|FORBIDDEN|UNSUPPORTED)/,
  );
  await expect(withdrawals).toContainText("Nothing was moved");
});

test("keyboard: the skip link is the first stop and reaches main", async ({ page }) => {
  await page.goto("/home");
  // Tab must not be pressed while the boot screen is up: it holds no focusable
  // element, so the keypress would be swallowed and focus would never reach the
  // skip link that the rendered page puts first.
  await expect(page.locator("h1")).toHaveCount(1);
  await page.keyboard.press("Tab");
  const focused = page.locator(":focus");
  await expect(focused).toHaveText("Skip to main content");
  await focused.press("Enter");
  await expect(page).toHaveURL(/#main$/);
});

test("a signed-out visitor to an application route is never shown a figure", async ({ browser }) => {
  const context = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  await page.goto("/home");
  // Sent to sign in, carrying where they were going — not shown an empty
  // dashboard, and not told they are signed out by a page that never asked.
  await expect(page).toHaveURL("/sign-in?return=%2Fhome");
  await expect(page.getByRole("heading", { level: 1, name: "Sign in" })).toBeVisible();
  await expect(page.locator(".num")).toHaveCount(0);
  await context.close();
});
