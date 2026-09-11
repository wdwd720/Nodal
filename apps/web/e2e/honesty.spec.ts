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
  //
  // The served legal documents are subtracted, and that is a deliberate limit
  // on this scan rather than a convenience.
  //
  // The vocabulary below is a rule about the product's OWN copy: it exists so
  // that a marketing page cannot call a stablecoin "cash" or promise an
  // outcome. A legal document has the opposite job — the withdrawal disclosure
  // says "none of them is guaranteed to complete", which is the sentence a
  // customer most needs and which a ban on the word would delete. Those bytes
  // are authored in `internal/terms`, hashed into every acceptance record, and
  // rendered verbatim; policing them with a rule written for marketing copy
  // would be a category error, and "fixing" a hit would mean editing a document
  // this repository does not own. Everything the web app itself writes is still
  // scanned, which is the whole surface this rule was ever about.
  //
  // The rest includes disabled-control explanations and disclosures, which is
  // exactly the copy most likely to drift.
  return page.evaluate(() => {
    const body = document.body.innerText;
    const documents = Array.from(document.querySelectorAll("pre.doc-source")).map(
      (node) => (node as HTMLElement).innerText,
    );
    return documents.reduce((text, served) => text.split(served).join(" "), body);
  });
}

/** The served document itself, for the checks that are about its contents. */
async function documentText(page: Page): Promise<string> {
  await expect(page.locator("pre.doc-source")).toBeVisible();
  return page.locator("pre.doc-source").innerText();
}

async function signedOutPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  return context.newPage();
}

for (const route of APP_ROUTES) {
  test(`rendered text of ${route.path} uses no forbidden phrasing`, async ({ page }) => {
    await page.goto(route.path);
    // The page under test. Scanning the 404 for forbidden vocabulary proves
    // that the 404 is clean and nothing else.
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
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

test("every legal document is served by Nodal and says whether counsel has read it", async ({
  browser,
}) => {
  // The page renders `GET /v1/terms`, so the assertion is that the SERVER'S
  // answer reached the screen: the version it reports, the document id it
  // reports, and the draft notice that follows from its own
  // `counsel_review_required` flag rather than from a constant in this
  // repository.
  const page = await signedOutPage(browser);
  const response = await page.request.get("/v1/terms");
  expect(response.ok()).toBeTruthy();
  const documents = (await response.json()) as Array<{
    document_id: string;
    version: string;
    title: string;
    counsel_review_required: boolean;
    body: string;
  }>;
  expect(documents.length, "the registry serves documents").toBeGreaterThan(0);

  const paths: Readonly<Record<string, string>> = {
    TERMS_OF_SERVICE: "/terms",
    PRIVACY_POLICY: "/privacy",
    RISK_DISCLOSURE: "/risk",
    CREDITS_TERMS: "/credits-terms",
    WITHDRAWAL_DISCLOSURE: "/withdrawal-disclosure",
  };

  for (const doc of documents) {
    const path = paths[doc.document_id];
    expect(path, `${doc.document_id} has a page`).toBeTruthy();
    await page.goto(path as string);
    const text = await visibleText(page);
    expect(text, `${path} names the document`).toContain(doc.document_id);
    expect(text, `${path} names the version the server serves`).toContain(`version ${doc.version}`);
    if (doc.counsel_review_required) {
      expect(text, `${path} says counsel has not read it`).toContain(
        "This document has not been reviewed by a lawyer.",
      );
    }
    // And the document's own words are on the page, not a summary of them.
    const heading = (doc.body.split("\n").find((line) => line.trim() !== "") ?? "")
      .replace(/^#+\s*/, "")
      .trim();
    expect(heading.length, `${doc.document_id} has a body`).toBeGreaterThan(0);
    expect(await documentText(page), `${path} renders the served text`).toContain(heading);
  }
  await page.context().close();
});

test("the served terms state what Nodal is not", async ({ browser }) => {
  // Goal section 5's forbidden claims, as the document itself denies them. This
  // is a POSITIVE assertion on purpose: an earlier version of this test banned
  // the phrase "approved by a regulator", which appears inside the sentence
  // that denies it, and a test that bans the words is a test that pushes the
  // denial off the page.
  const page = await signedOutPage(browser);
  await page.goto("/terms");
  expect(await documentText(page)).toContain(
    "Nodal is not a bank, a broker-dealer, a money transmitter, an exchange",
  );
  await page.context().close();
});

test("a simulated surface on the public site says it is an example", async ({ browser }) => {
  // The rule is about the SURFACE, not about the page. `/product/markets` shows
  // the live discovery list when there is one and the example composition when
  // there is not, so demanding the example sentence on every page would demand
  // that live data carry an example label, which would be its own lie.
  //
  // What must hold is the pairing: a surface wearing the design system's
  // SIMULATED chip carries the sentence that says what it is.
  const page = await signedOutPage(browser);
  for (const path of ["/", "/product", "/product/markets", "/product/agents"]) {
    await page.goto(path);
    // The wait comes BEFORE the read. Reading first and settling after tested
    // the page as it was during its own fetches: an example composition that
    // the live list was about to replace, or a chip that had not rendered yet.
    // Both directions of that are wrong, and the harmless-looking one — a check
    // that passes because the thing it polices had not appeared — is the one
    // that leaves a real page unchecked.
    await page.waitForLoadState("networkidle");
    const text = await visibleText(page);
    if ((await page.locator(".badge-simulated").count()) === 0) continue;
    expect(text, `${path} labels its simulated surfaces`).toContain(
      "Example data, not a live account",
    );
  }
  await page.context().close();
});

test("the public market list is live data or a labelled example, never a blend", async ({
  browser,
}) => {
  const page = await signedOutPage(browser);
  await page.goto("/product/markets");
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");

  const response = await page.request.get("/v1/native-markets?limit=6&sort=NEWEST");
  expect(response.ok(), "the discovery list answers without a session").toBeTruthy();
  const listed = (await response.json()) as { markets: Array<{ name: string; demo: boolean }> };

  if (listed.markets.length > 0) {
    // Live: the markets the API returned are on the page, the only action is to
    // sign in, and no example label is attached to any of it.
    for (const market of listed.markets) {
      await expect(page.getByText(market.name).first()).toBeVisible();
    }
    await expect(page.getByRole("link", { name: "Sign in to trade" })).toBeVisible();
    if (listed.markets.some((market) => market.demo)) {
      await expect(page.getByText("DEMO").first()).toBeVisible();
    }
  } else {
    // Empty: the example, saying so, with the chip the design system renders on
    // any simulated surface.
    const text = await visibleText(page);
    expect(text).toContain("Example data, not a live account");
    await expect(page.locator(".badge-simulated").first()).toBeVisible();
  }
  await page.context().close();
});

test("a page showing a USD valuation says what is actually held", async ({ page }) => {
  // The list is empty of `/home` and `/portfolio` on purpose, and it is the
  // change D-077 made rather than an omission: both moved onto Credits, which
  // are not a settlement token and must not be described as though they were.
  // What they owe is the Credit disclosure, which the two tests below require.
  //
  // The rule this test protects is unchanged — a page that puts a dollar figure
  // in front of somebody must say what the underlying actually is, because a
  // "$" is the single most misread character in this product. What changed is
  // that the agent surface no longer has one to annotate: the previous /agents
  // screen valued account holdings in USD because v1 had no agent resource and
  // holdings were the closest thing it could show. The rebuilt one reads the
  // agent resource itself, and every figure on it is a Credit budget — which is
  // the same rule arriving at a stricter answer, since the safest way to not
  // misread a dollar figure is for there not to be one.
  //
  // So the assertion follows the figure: wherever a USD valuation renders it
  // must be explained, and where none renders the page must not be quietly
  // showing a dollar amount under another name.
  await page.goto("/agents");
  const agentsText = await visibleText(page);
  if (/USD figures are a valuation computed by the backend/.test(agentsText)) {
    expect(agentsText, "/agents says it is an estimate, not dollars held").toContain(
      "not an amount held in dollars",
    );
  } else {
    expect(
      /\$\s?\d/.test(agentsText),
      "/agents shows no dollar figure, so it owes no valuation note",
    ).toBe(false);
  }
});

test("a page showing Credits says what a Credit is", async ({ page }) => {
  for (const path of ["/markets", "/markets/products"]) {
    await page.goto(path);
    // Settle first, then read: the figure this rule is about arrives from the
    // API, so a read taken before the fetch lands finds no figure and skips the
    // page it was written to check.
    await page.waitForLoadState("networkidle");
    const text = await visibleText(page);
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
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
    const text = await visibleText(page);
    if (!/confidence/i.test(text)) continue;
    expect(text, `${route.path}: confidence appears, so the denial must too`).toContain(
      "It is not the chance of making money",
    );
  }
});

test("pending settlement is never hidden", async ({ page }) => {
  // The claim under test is that value which is not yet usable is shown
  // SEPARATELY and is named as not usable. On the closed-loop product that
  // lives on the dashboard's Credit module — a frozen bucket and a
  // payout-eligible split, each with its own figure — rather than on the
  // hosted rail's funding panel, which D-077 removed with its page.
  await page.goto("/home");
  const text = await visibleText(page);
  expect(text, "value that is held is named as not spendable").toContain("Not spendable until");
  expect(text, "and what may be paid out is a separate question").toContain(
    "Payout-eligible value",
  );
});

test("the risk statement is on every page", async ({ browser, page }) => {
  for (const route of APP_ROUTES) {
    await page.goto(route.path);
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
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

test("every public page says whether this deployment is a rehearsal", async ({ browser }) => {
  // `USER_JOURNEY.md` §0 promises the label, and the public site is where it
  // matters most: somebody signed out is deciding whether this is a real
  // product, and every sentence on the marketing pages reads as one until
  // something says the deployment is a rehearsal. Both directions again — a
  // real deployment wearing a sandbox label is the same lie as a sandbox
  // deployment without one.
  const page = await signedOutPage(browser);
  const response = await page.request.get("/v1/version");
  expect(response.ok()).toBeTruthy();
  const version = (await response.json()) as { sandbox_tier?: boolean };

  const wrong: string[] = [];
  for (const route of PUBLIC_ROUTES) {
    await page.goto(route.path);
    await expect(page.locator("h1")).toHaveCount(1);
    await page.waitForLoadState("networkidle");
    const lines = page.locator(".sandbox-line");
    const count = await lines.count();
    if (version.sandbox_tier === true) {
      if (count === 0) {
        wrong.push(`${route.path} is served by a sandbox tier and says nothing`);
        continue;
      }
      await expect(lines.first()).toContainText("nothing moves real value");
      // Part of the document, not something that can be dismissed.
      await expect(lines.first().getByRole("button")).toHaveCount(0);
    } else if (count > 0) {
      wrong.push(`${route.path} wears a sandbox label on a deployment that is not one`);
    }
  }
  await page.context().close();
  expect(wrong, "the public site's sandbox labelling").toEqual([]);
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
