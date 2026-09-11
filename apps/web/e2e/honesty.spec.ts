/**
 * The vocabulary rules enforced against what actually renders.
 *
 * `src/lib/honesty.test.ts` scans the source. This scans the pixels: it visits
 * every route, takes the text the browser would read aloud, and refuses the
 * phrasings the goal document forbids. It therefore also covers strings that
 * arrive from the API, which no source scan can see.
 *
 * It additionally checks the positive obligations — that the settlement asset
 * is disclosed where a balance appears, that Credits are explained where a
 * Credit figure appears, that simulated surfaces are labelled, that a model
 * score never appears without its denial, and that the public site states what
 * the product is not.
 */
import { expect, test, type Browser, type Page } from "@playwright/test";

import { APP_ROUTES, PUBLIC_ROUTES } from "./routes.ts";

const SIGNED_OUT = { cookies: [], origins: [] };

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

/** The marketing filler goal §28 bans by name. */
const FILLER: readonly RegExp[] = [
  /\brevolutioni[sz]e/i,
  /\bunlock the future\b/i,
  /\bseamlessly\b/i,
  /\bAI-powered ecosystem\b/i,
  /\bnext-generation\b/i,
  /\bcutting-edge\b/i,
];

/** A rendered Credit figure: `Qty` writes `.num`, `Figure` writes `.figure-symbol`. */
const CREDIT_FIGURE = '.num:has-text("Credits"), .figure-symbol:has-text("Credits")';

async function visibleText(page: Page): Promise<string> {
  // The application gates itself behind a boot screen until the backend has
  // answered who is signed in, so the text must not be read until a page has
  // actually rendered. Waiting for the single <h1> is that signal; without it
  // this reads "Checking this session with the backend…" and proves nothing.
  await expect(page.locator("h1")).toHaveCount(1);
  // Includes the text of disabled-control explanations and disclosures, which
  // is exactly the copy most likely to drift.
  //
  // It excludes `.doc-source`, and only that: the verbatim bytes of a legal
  // document the API served. An acceptance records the sha256 of exactly what
  // was shown, so the app must reproduce those bytes unaltered — it has no
  // licence to reword them and this scan has no business asserting over them.
  // The document that forced the question says "none of them is guaranteed to
  // complete", which is a DENIAL, and a substring ban cannot tell a claim from
  // its denial — the same reasoning the terms assertions below already rest on.
  // Every word the product writes for itself is still scanned.
  return page.evaluate(() => {
    const copy = document.body.cloneNode(true) as HTMLElement;
    for (const served of copy.querySelectorAll(".doc-source")) served.remove();
    return copy.innerText;
  });
}

async function signedOutPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  return context.newPage();
}

for (const route of APP_ROUTES) {
  test(`rendered text of ${route.path} uses no forbidden phrasing`, async ({ page }) => {
    await page.goto(route.path);
    const text = await visibleText(page);
    for (const [pattern, why] of FORBIDDEN) {
      expect(pattern.test(text), `${route.path}: ${String(pattern)} — ${why}`).toBe(false);
    }
  });
}

test("no public page uses forbidden phrasing or marketing filler", async ({ browser }) => {
  const page = await signedOutPage(browser);
  for (const route of PUBLIC_ROUTES) {
    await page.goto(route.path);
    const text = await visibleText(page);
    for (const [pattern, why] of FORBIDDEN) {
      expect(pattern.test(text), `${route.path}: ${String(pattern)} — ${why}`).toBe(false);
    }
    for (const pattern of FILLER) {
      expect(pattern.test(text), `${route.path}: ${String(pattern)} is banned by goal §28`).toBe(
        false,
      );
    }
  }
  await page.context().close();
});

test("the landing page says what the product is not", async ({ browser }) => {
  // Goal §5's forbidden claims, stated as denials on the first page a visitor
  // sees. This is the copy a growth-minded edit removes first, so it is the
  // copy with a test on it.
  const page = await signedOutPage(browser);
  await page.goto("/");
  const text = await visibleText(page);
  for (const phrase of [
    "Not an exchange, a broker, a bank or a custodian.",
    "Not regulated, licensed or approved by anybody, anywhere.",
    "Not insured.",
    "You can lose everything you put in.",
  ]) {
    expect(text, `the landing page states: ${phrase}`).toContain(phrase);
  }
  await page.context().close();
});

test("every policy document says it is a draft and names its version", async ({ browser }) => {
  const page = await signedOutPage(browser);
  for (const [path, version] of [
    ["/terms", "terms-v1"],
    ["/privacy", "privacy-v1"],
    ["/risk", "risk-v1"],
  ] as const) {
    await page.goto(path);
    const text = await visibleText(page);
    expect(text, `${path} names its version`).toContain(version);
    expect(text, `${path} says it is a draft`).toContain("Draft pending legal review");
    expect(text, `${path} does not claim counsel approved it`).toContain(
      "no counsel has approved them",
    );
  }
  // The architecture is never self-certified as lawful, and the terms say
  // so in the document rather than only in a comment.
  //
  // This is a POSITIVE assertion on purpose. The first version banned the
  // phrase "approved by a regulator", which the terms contain — inside the
  // sentence "nothing in the product has been approved by a regulator". A
  // substring cannot tell a claim from its denial, and a test that bans the
  // words is a test that pushes the denial off the page.
  await page.goto("/terms");
  const terms = await visibleText(page);
  expect(terms).toContain("is not regulated as any of those");
  expect(terms).toContain("nothing in the product has been approved by a regulator");
  expect(terms).toContain("The architecture described here is not certified as lawful anywhere.");
  await page.context().close();
});

test("example data on the public site is labelled as an example", async ({ browser }) => {
  const page = await signedOutPage(browser);
  for (const path of ["/", "/product", "/product/markets", "/product/agents"]) {
    await page.goto(path);
    const text = await visibleText(page);
    await page.waitForLoadState("networkidle");
    // Gated on a rendered Credit FIGURE rather than on the word: every page
    // here names Credits in prose, and the rule is about figures.
    if ((await page.locator(CREDIT_FIGURE).count()) === 0) continue;
    expect(text, `${path} labels its example figures`).toContain("Example data, not a live account");
    // The chip the design system renders on any simulated surface, which takes
    // no prop to suppress it.
    await expect(page.locator(".badge-simulated").first()).toBeVisible();
  }
  await page.context().close();
});

test("a page showing balances discloses that they are USDC", async ({ page }) => {
  for (const route of ["/home", "/portfolio"]) {
    await page.goto(route);
    const text = await visibleText(page);
    expect(text, `${route} names the settlement asset`).toContain("USDC");
    expect(text.toLowerCase(), `${route} says what USDC is`).toContain("stablecoin");
  }
});

test("a page showing Credits says what a Credit is", async ({ page }) => {
  for (const path of ["/markets", "/markets/products"]) {
    await page.goto(path);
    const text = await visibleText(page);
    await page.waitForLoadState("networkidle");
    // The gate is a rendered Credit FIGURE, not the word. A page can name
    // Credits in prose — "a shared pool of Credits" — while showing no figure
    // at all, which is what `/markets` does on a deployment with no assets
    // yet, and demanding the disclosure there would demand it beside nothing.
    if ((await page.locator(CREDIT_FIGURE).count()) === 0) continue;
    expect(text.toLowerCase(), `${path} says what Credits are`).toMatch(
      /not money|quoted in credits|internal platform value/,
    );
  }
});

test("a model score never appears without the denial beside it", async ({ page }) => {
  for (const route of APP_ROUTES) {
    await page.goto(route.path);
    const text = await visibleText(page);
    if (!/confidence/i.test(text)) continue;
    expect(text, `${route.path}: confidence appears, so the denial must too`).toContain(
      "It is not the chance of making money",
    );
  }
});

test("pending settlement is never hidden", async ({ page }) => {
  await page.goto("/settings");
  const text = await visibleText(page);
  expect(text).toContain("not spendable until the backend marks them available");
});

test("the risk statement is on every page", async ({ browser, page }) => {
  for (const route of APP_ROUTES) {
    await page.goto(route.path);
    const text = await visibleText(page);
    expect(text, `${route.path} carries the risk statement`).toContain("can lose money");
  }
  const publicPage = await signedOutPage(browser);
  for (const route of PUBLIC_ROUTES) {
    await publicPage.goto(route.path);
    const text = await visibleText(publicPage);
    expect(text, `${route.path} carries the risk statement`).toContain("can lose money");
  }
  await publicPage.context().close();
});

/**
 * The Credit-denominated pages.
 *
 * Kept as its own list rather than derived from `APP_ROUTES`, because the rule
 * below is about pages that quote Credits and a page that shows a USD funding
 * record is not one of them. Deriving it would turn a real rule into a rule
 * about whichever pages happen to mention the word.
 */
const CREDIT_ROUTES: readonly string[] = ["/markets", "/markets/products", "/create-asset"];

test("no page puts a Credit figure and a currency figure together", async ({ page }) => {
  // There is no approved external value for a Credit, so a currency figure
  // beside one would be an exchange rate nobody set. This reads what actually
  // rendered, which the source scan cannot do for text that arrives from the
  // API.
  for (const path of CREDIT_ROUTES) {
    await page.goto(path);
    const text = await visibleText(page);
    if (!text.includes("Credits")) continue;
    expect(
      /\$\s?\d/.test(text),
      `${path} rendered a currency amount on a page that quotes Credits`,
    ).toBeFalsy();
  }
});

test("the shell says whether this deployment is a rehearsal", async ({ page }) => {
  // The label comes from `GET /v1/version`, which the client never second-
  // guesses. Both directions are asserted, because a real deployment wearing a
  // sandbox label is the same lie as a sandbox deployment without one.
  const response = await page.request.get("/v1/version");
  expect(response.ok()).toBeTruthy();
  const version = (await response.json()) as { sandbox_tier?: boolean };

  await page.goto("/home");
  await expect(page.locator("h1")).toHaveCount(1);
  const line = page.locator(".sandbox-line");

  if (version.sandbox_tier === true) {
    await expect(line).toBeVisible();
    await expect(line).toContainText("nothing moves real value");
    // And it is part of the document, not something that can be dismissed.
    await expect(line.getByRole("button")).toHaveCount(0);
  } else {
    await expect(line, "a deployment that is not a sandbox tier wears no sandbox label").toHaveCount(0);
  }
});
