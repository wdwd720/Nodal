/**
 * PART 112 enforced against what actually renders.
 *
 * `src/lib/honesty.test.ts` scans the source. This scans the pixels: it visits
 * every route, takes the text the browser would read aloud, and refuses the
 * phrasings the goal document forbids. It therefore also covers strings that
 * arrive from the API, which no source scan can see.
 *
 * It additionally checks the positive obligations — that the settlement asset
 * is disclosed where a balance appears, that simulated results are labelled,
 * and that a model score never appears without its denial.
 */
import { expect, test, type Page } from "@playwright/test";

const ROUTES = [
  "/",
  "/add-funds",
  "/nodal-economy",
  "/marketplace",
  "/native-markets",
  "/payouts",
  "/trade",
  "/portfolio",
  "/strategy",
  "/agents",
  "/lab",
  "/activity",
  "/settings",
] as const;

/** The vocabulary this product may not use, and why. */
const FORBIDDEN: ReadonlyArray<readonly [RegExp, string]> = [
  [/\bcash\b/i, 'a stablecoin is never called "cash"'],
  [/\blive performance\b/i, "simulated output is never presented as real trading"],
  [/\bguarantee/i, "nothing here is assured"],
  [/\brisk[ -]free\b/i, "nothing here is without risk"],
  [/\bno risk\b/i, "nothing here is without risk"],
  [/\bprobability of profit\b/i, "a model score is not the chance of making money"],
  [/\bchance of profit\b/i, "a model score is not the chance of making money"],
  [/\bwin rate\b/i, "implies a probability of profit"],
  [/\bsuccess rate\b/i, "implies a probability of profit"],
  [/\bexpected profit\b/i, "implies a probability of profit"],
];

async function visibleText(page: Page): Promise<string> {
  // The application gates itself behind a boot screen until the backend has
  // answered who is signed in, so the text must not be read until a page has
  // actually rendered. Waiting for the single <h1> is that signal; without it
  // this reads "Checking this session with the backend…" and proves nothing.
  await expect(page.locator("h1")).toHaveCount(1);
  // Includes the text of disabled-control explanations and disclosures, which
  // is exactly the copy most likely to drift.
  return page.evaluate(() => document.body.innerText);
}

for (const route of ROUTES) {
  test(`rendered text of ${route} uses no forbidden phrasing`, async ({ page }) => {
    await page.goto(route);
    await expect(page.locator("h1")).toHaveCount(1);
    const text = await visibleText(page);
    for (const [pattern, why] of FORBIDDEN) {
      expect(pattern.test(text), `${route}: ${String(pattern)} — ${why}`).toBe(false);
    }
  });
}

test("a page showing balances discloses that they are USDC", async ({ page }) => {
  for (const route of ["/", "/portfolio", "/add-funds"]) {
    await page.goto(route);
    const text = await visibleText(page);
    expect(text, `${route} names the settlement asset`).toContain("USDC");
    expect(text.toLowerCase(), `${route} says what USDC is`).toContain("stablecoin");
  }
});

test("simulated output is labelled wherever it appears", async ({ page }) => {
  await page.goto("/lab");
  const text = await visibleText(page);
  expect(text).toContain("No real capital was committed");
  // Every mode badge states whether capital is real.
  expect(text).toMatch(/simulated|real capital/);
});

test("a model score never appears without the denial beside it", async ({ page }) => {
  for (const route of ROUTES) {
    await page.goto(route);
    const text = await visibleText(page);
    if (!/confidence/i.test(text)) continue;
    expect(text, `${route}: confidence appears, so the denial must too`).toContain(
      "It is not the chance of making money",
    );
  }
});

test("pending settlement is never hidden", async ({ page }) => {
  await page.goto("/add-funds");
  const text = await visibleText(page);
  expect(text).toContain("not spendable until the backend marks them available");
});

test("the risk statement is on every page", async ({ page }) => {
  for (const route of ROUTES) {
    await page.goto(route);
    const text = await visibleText(page);
    expect(text, `${route} carries the risk statement`).toContain("can lose money");
  }
});

test("no capability that is off is shown as a zero", async ({ page }) => {
  // The quote endpoint is unavailable in this deployment. The trade page must
  // say so and must not put a figure where the price would be.
  await page.goto("/trade");
  await page.getByLabel("Amount to commit, in US dollars of value").fill("100");
  await page.getByRole("button", { name: "Get a quote" }).click();
  const panel = page.locator(".panel", { hasText: "Non-binding, fully disclosed" });
  await expect(panel.getByText(/code [A-Z_]+/)).toBeVisible();
  const text = await panel.innerText();
  expect(text).not.toMatch(/\$\s?0\.00/);
  expect(text).not.toMatch(/(^|\s)—(\s|$)/);
});
