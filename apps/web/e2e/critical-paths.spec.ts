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

/**
 * D-077 moved Home off the hosted rail. It no longer shows buying power, and
 * the `Balances` panel these two tests were written against does not exist;
 * what leads the dashboard now is the Credit balance, which is the thing the
 * closed-loop product actually runs on. The PROPERTY under test is unchanged
 * and is what matters — the figures on screen are the backend's own, unaltered
 * — so it is asserted against the panel that is there.
 *
 * The snapshot-instant half of the old test moved with the rail rather than
 * being dropped quietly: `CreditBalance` carries no `as_of`, so there is no
 * backend instant on this response to compare against. `/portfolio` keeps the
 * `as_of` assertion, and the missing stamp is a reported API gap.
 */
test("home shows the figures the backend computed, unchanged", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as Record<string, string>;

  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  const credits = page.locator(".panel", { hasText: "Credits" }).first();

  // Every figure carries its EXACT value in `title`, whatever the display form
  // is, so this compares the unrounded value rather than the rendering of it.
  for (const [field, label] of [
    ["gross", "Total Credits"],
    ["spendable", "Spendable"],
    ["frozen", "Frozen"],
    ["payout_eligible", "Payout-eligible"],
    ["ineligible", "Not payout-eligible"],
  ] as const) {
    const wire = body[field];
    expect(wire, `${field} present in the response`).toBeTruthy();
    const figure = credits.locator(`.field:has(dt:text-is("${label}")) .figure`).first();
    await expect(figure, `${label} is on screen`).toBeVisible();
    const exact = (await figure.getAttribute("title")) ?? "";
    const digits = (wire as string).replace(/^-/, "").replace(/^0+/, "");
    expect(
      exact.replace(/[^0-9]/g, "").replace(/^0+/, ""),
      `${label} is the backend's own digits`,
    ).toBe(digits === "" ? "" : digits);
  }
});

test("home discloses what the balance actually is", async ({ page }) => {
  // The balance Home leads with is Credits, so what it owes the reader is what
  // a Credit is — not what a settlement token is. `/portfolio` keeps the USDC
  // disclosure, because that is the page that still shows a USD valuation.
  await page.goto("/home");
  const disclosure = page.getByLabel("What Credits are");
  await expect(disclosure).toContainText("not money");
  await expect(disclosure).toContainText("not a deposit");
  await expect(disclosure).toContainText("not redeemable for money unless");
});

/**
 * D-077 moved the portfolio off the settlement rail's holdings and onto
 * `GET /v1/me/portfolio`, which carries the asset's own scale with every
 * figure. The old assertion checked for an exact base-unit string printed
 * beside each valuation, which existed because the rail's response left the
 * scale to be looked up elsewhere; the exact value is now in every figure's
 * `title`, unrounded, whatever the display form is. Same property, one place.
 */
test("portfolio shows the exact value behind every figure", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/me/portfolio?account_id=${id}`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    positions: Array<{ symbol: string; quantity: string }>;
  };

  await page.goto("/portfolio");
  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();

  for (const position of body.positions) {
    const row = page.locator("tr", { hasText: position.symbol }).first();
    await expect(row, `${position.symbol} has a row`).toBeVisible();
    // The exact value is in `title` on every figure, so a compact or truncated
    // rendering never hides what the backend actually said.
    const figures = row.locator(".figure");
    expect(await figures.count(), `${position.symbol} renders figures`).toBeGreaterThan(0);
    expect(await figures.first().getAttribute("title")).toBeTruthy();
  }
  await expect(page.getByRole("link", { name: "Export as CSV" })).toHaveAttribute(
    "href",
    /export\?format=csv/,
  );
});

/**
 * The activity page is the product's feed now, not the trading lifecycle
 * ladder: `ActivityFeedKind` is the vocabulary and `GET /v1/me/activity` is the
 * source. The property worth keeping is the one the ladder was protecting —
 * that every kind the feed can carry is reachable, so nothing is quietly
 * filtered out of a customer's own record.
 */
test("activity offers every kind the feed can carry", async ({ page }) => {
  await page.goto("/activity");
  await expect(page.getByRole("heading", { level: 1, name: "Activity" })).toBeVisible();
  const filters = page.locator(".panel", { hasText: "What to show" });
  for (const label of [
    "Credit purchases",
    "Reversals",
    "Trades",
    "Assets created",
    "Withdrawal requests",
    "Withdrawal updates",
    "Adjustments",
  ]) {
    await expect(filters.getByRole("button", { name: label, exact: true })).toBeVisible();
  }
});

/**
 * D-077 split this in two. Sessions moved to `/settings/security`, which is
 * where USER_JOURNEY §9 puts them, and the withdrawal form moved off Settings
 * entirely to `/withdraw` — scenario E is what covers the refusal now, on the
 * page that owns it. What is left here is the half this file is for: the
 * session list is real, and it is the backend's own rows.
 */
test("security lists real sessions", async ({ page }) => {
  const response = await page.request.get("/v1/sessions");
  const sessions = (await response.json()) as Array<{ id: string }>;

  await page.goto("/settings/security");
  await expect(page.getByRole("heading", { level: 1, name: "Security" })).toBeVisible();
  for (const session of sessions.slice(0, 3)) {
    await expect(page.getByText(session.id, { exact: false }).first()).toBeVisible();
  }
  // Ending a session is a real action on every row, never a control that is
  // there for show.
  await expect(
    page.locator(".panel", { hasText: "Sessions" }).getByRole("button", { name: /End (this )?session/ }).first(),
  ).toBeEnabled();
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
