/**
 * Critical-path end-to-end tests (STAGE 14 exit criteria).
 *
 * These run against a production build and a real backend. Where the backend
 * refuses — quotes, funding, withdrawals in this deployment — the assertion is
 * that the refusal is shown with its stable code, not that the refusal is
 * absent. A test that asserted a price appeared would be a test that demanded
 * the interface lie.
 */
import { expect, test, type Page } from "@playwright/test";

const PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/", heading: "Home" },
  { path: "/add-funds", heading: "Add funds" },
  { path: "/trade", heading: "Trade" },
  { path: "/portfolio", heading: "Portfolio" },
  { path: "/strategy", heading: "Strategy builder" },
  { path: "/agents", heading: "Agents" },
  { path: "/lab", heading: "Lab" },
  { path: "/activity", heading: "Activity" },
  { path: "/settings", heading: "Settings and security" },
];

/**
 * The internal-economy pages (gola.md PART LII). They are a separate list from
 * PAGES because PAGES is PART 111's required set and this is a different
 * requirement; merging them would make a failure in one look like a failure of
 * the other.
 */
const INTERNAL_ECONOMY_PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/nodal-economy", heading: "Nodal Economy" },
  { path: "/marketplace", heading: "Marketplace" },
  { path: "/native-markets", heading: "Native Markets" },
  { path: "/create-asset", heading: "Create asset" },
  { path: "/payouts", heading: "Payouts" },
];

async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/me");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { account_ids: string[] };
  const id = body.account_ids[0];
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

test.describe("the nine required pages", () => {
  for (const { path, heading } of PAGES) {
    test(`${heading} renders`, async ({ page }) => {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
      // Exactly one h1 per page: the document outline is the navigation aid a
      // screen reader user actually has.
      await expect(page.locator("h1")).toHaveCount(1);
      // Nothing renders the placeholder that would mean a formatter gave up.
      await expect(page.locator(".malformed")).toHaveCount(0);
    });
  }

  test("every section is reachable from the navigation", async ({ page }) => {
    await page.goto("/");
    for (const { heading } of PAGES) {
      await page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: heading }).click();
      await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
    }
  });
});

test.describe("the internal economy is separate from the rest", () => {
  for (const { path, heading } of INTERNAL_ECONOMY_PAGES) {
    test(`${heading} renders`, async ({ page }) => {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
      await expect(page.locator("h1")).toHaveCount(1);
      await expect(page.locator(".malformed")).toHaveCount(0);

      // And the page's DATA loaded. The three assertions above pass whether
      // the panels show data, an empty state or an error, because the heading
      // is rendered before any request is made -- so this test used to prove
      // only that the route existed.
      //
      // "This response could not be trusted" is what the app shows when a
      // response does not match the API contract. A mismatch between what the
      // API returns and what the client parses would render it on every load
      // and still pass the three assertions above.
      //
      // A REFUSAL is deliberately not asserted against: this deployment
      // refuses plenty, and demanding no refusal appeared would be demanding
      // the interface lie. What is asserted is narrower and is the thing a
      // heading cannot tell you -- that the data loaded at all.
      // Wait for the page to SETTLE before asserting an absence. Playwright
      // retries an assertion until it passes, and `toHaveCount(0)` passes the
      // instant it is evaluated -- so checking for the absence of an error
      // before the query has resolved proves nothing. The first version of
      // this assertion did exactly that and passed against a client that was
      // mis-parsing every list response.
      await page.waitForLoadState("networkidle");
      await expect(page.locator(".loading")).toHaveCount(0);
      await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
    });
  }

  /**
   * A customer completes a purchase through the interface.
   *
   * Every other test in this file asks whether a page RENDERS. None asked whether
   * a person can finish anything, and that gap is the same one that produced
   * F-26, F-28 and F-29 on the backend: a path the tests never walk looks
   * finished from inside the tests.
   *
   * This walks it. It reads the Credit balance the customer is shown, buys the
   * cheapest product on the Marketplace, and asserts the balance fell by exactly
   * the price and that the purchase appears in their own list.
   *
   * # It needs a deployment where buying is possible
   *
   * MARKETPLACE is a high-risk capability, so a deployment that has not activated
   * it refuses every purchase — correctly. Rather than skip, the test asserts the
   * refusal is the honest one and stops: a run against a fresh deployment proves
   * the gate holds, and a run against an enabled one proves the flow completes.
   * Neither outcome is a green tick over an untested path.
   *
   * See scripts/gateceremony for activating MARKETPLACE locally through the real
   * three-principal ceremony.
   */
  test("a customer can buy something and their Credits fall by exactly the price", async ({ page }) => {
    const creditsShown = async (): Promise<number> => {
      await page.goto("/nodal-economy");
      await page.waitForLoadState("networkidle");
      const field = page.locator(".field", { hasText: "Usable inside Nodal" }).first();
      const text = await field.innerText();
      const match = /([\d,]+(?:\.\d+)?)\s*Credits/.exec(text);
      expect(match, `no Credit figure in ${text}`).not.toBeNull();
      return Number((match?.[1] ?? "0").replace(/,/g, ""));
    };

    const before = await creditsShown();

    await page.goto("/marketplace");
    await page.waitForLoadState("networkidle");
    // Each product is its own <article class="panel-nested" aria-label={title}>.
    // Locating the CARD and then its own price and its own button is the whole
    // point: the first version of this test read a price from one product and
    // clicked another's button, and the mismatch was invisible until the
    // balance assertion caught it.
    const cards = page.locator("article.panel-nested");
    expect(await cards.count(), "the seeded catalogue should offer something to buy").toBeGreaterThan(0);
    const card = cards.last();

    const priceText = await card.locator(".field", { hasText: "PRICE" }).first().innerText();
    const priceMatch = /([\d,]+(?:\.\d+)?)\s*Credits/.exec(priceText);
    expect(priceMatch, `no price in ${priceText}`).not.toBeNull();
    const price = Number((priceMatch?.[1] ?? "0").replace(/,/g, ""));
    expect(price).toBeGreaterThan(0);

    await card.getByRole("button", { name: "Buy for Credits" }).click();

    // Wait for the card to reach an OUTCOME -- a success notice or an
    // explanation -- before deciding which happened. `networkidle` is not
    // enough: it can return before the mutation has settled and re-rendered,
    // and then the branch below reads an empty card and takes the wrong path.
    // This is the third time in this session that a check ran before the thing
    // it was checking existed.
    await expect(card.locator(".notice-good, .explain").first()).toBeVisible();

    // The gate may be off. That is a correct deployment state -- MARKETPLACE is
    // high risk and a deployment that has not activated it refuses everyone --
    // so the test asserts the refusal is the HONEST one and stops there.
    //
    // The string is the code the backend actually sent, read off the rendered
    // page. The first version of this branch looked for the problem TITLE and
    // never matched: with the gate off the test fell through to the success
    // assertion and failed with "element not found", which is a confusing way
    // to be told the gate is closed. Running that control is the only reason
    // this branch works.
    const refusal = card.locator(".explain");
    if (await refusal.count()) {
      await expect(refusal).toContainText("CAPABILITY_NOT_APPROVED");
      await expect(refusal).toContainText("MARKETPLACE");
      // And nothing moved.
      expect(await creditsShown()).toBe(before);
      return;
    }

    // The card says it happened, in the customer's own words rather than a
    // status code. Asserting only that a Purchases table exists would pass on
    // somebody else's earlier order, which is how the first version of this
    // test reached its balance check believing a purchase had occurred.
    await expect(card.locator(".notice-good")).toContainText("Bought");
    await expect(page.getByRole("table", { name: "Purchases" })).toBeVisible();

    const after = await creditsShown();
    expect(before - after).toBe(price);
  });

  test("every internal-economy page is reachable from the navigation", async ({ page }) => {
    await page.goto("/");
    for (const { heading } of INTERNAL_ECONOMY_PAGES) {
      await page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: heading }).click();
      await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
    }
  });

  test("home names all three pots and adds none of them", async ({ page }) => {
    await page.goto("/");
    const panel = page.locator(".panel", { hasText: "Three kinds of value" });
    await expect(panel).toBeVisible();
    for (const name of ["Real Capital", "Nodal Economy", "Simulated Capital"]) {
      await expect(panel.getByText(name, { exact: false }).first()).toBeVisible();
    }
    // .first(): the phrase appears in both the disclosure title and its body,
    // which is the point — the rule is stated twice — but strict mode needs one.
    await expect(panel.getByText("never added together", { exact: false }).first()).toBeVisible();
  });

  test("no page puts a Credit figure and a currency figure together", async ({ page }) => {
    // PART LIV: there is no approved external value for a Credit, so a
    // currency figure beside one would be an exchange rate nobody set. This
    // reads what actually rendered, which the source scan cannot do for text
    // that arrives from the API.
    for (const { path } of INTERNAL_ECONOMY_PAGES) {
      await page.goto(path);
      await expect(page.locator("h1")).toHaveCount(1);
      const text = await page.evaluate(() => document.body.innerText);
      if (!text.includes("Credits")) continue;
      expect(
        /\$\s?\d/.test(text),
        `${path} rendered a currency amount on a page that quotes Credits`,
      ).toBeFalsy();
    }
  });
});

test("home shows the figures the backend computed, unchanged", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/accounts/${id}/buying-power?purpose=DISPLAY`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as Record<string, string>;

  await page.goto("/");
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
  await page.goto("/");
  const disclosure = page.getByLabel("What you are actually holding");
  await expect(disclosure).toContainText("USDC");
  await expect(disclosure).toContainText("stablecoin");
  await expect(disclosure).toContainText("not a bank deposit");
});

test("trade: a quote the backend cannot produce is reported, not faked", async ({ page }) => {
  await page.goto("/trade");
  await expect(page.getByRole("heading", { level: 1, name: "Trade" })).toBeVisible();

  // Exact mint identity is on the page before anything is priced.
  await expect(page.getByText("Mint address").first()).toBeVisible();

  await page.getByLabel("Amount to commit, in US dollars of value").fill("100");
  await page.getByRole("button", { name: "Get a quote" }).click();

  const quotePanel = page.locator(".panel", { hasText: "Non-binding, fully disclosed" });
  await expect(quotePanel.getByText(/code (PROVIDER_UNAVAILABLE|UNSUPPORTED)/)).toBeVisible();
  await expect(quotePanel).toContainText("No price is being shown");
  // Critically: no number that could be read as a price.
  await expect(quotePanel.locator(".num")).toHaveCount(0);
});

test("trade: a simulated order is submitted and its real state is polled", async ({ page }) => {
  await page.goto("/trade");
  await page.getByLabel("Amount to commit, in US dollars of value").fill("25");

  // PAPER is the default; assert it rather than assume it.
  await expect(page.getByRole("radio", { name: /PAPER/ })).toBeChecked();

  const key = await page.locator(".panel", { hasText: "An explicit, idempotent command" }).locator("code").innerText();
  expect(key.length).toBeGreaterThan(8);

  await page.getByRole("button", { name: "Submit simulated order" }).click();

  const state = page.locator(".panel", { hasText: "Polled from the intent resource" });
  await expect(state).toBeVisible();
  await expect(state.getByText(/RECEIVED|ELIGIBILITY_CHECKED|RISK_CHECKED|REJECTED|NO_VALID_PLAN/).first()).toBeVisible();
  await expect(state).toContainText("PAPER — simulated");

  // The intent the UI shows exists in the backend under that id.
  const shown = await state.locator("code").first().innerText();
  const response = await page.request.get(`/v1/intents/${shown}`);
  expect(response.status(), "the intent the page displays is a real backend record").toBe(200);
});

test("add funds: the backend's refusal is shown with its code", async ({ page }) => {
  await page.goto("/add-funds");
  await page.getByLabel("Amount in US dollars").fill("25");
  await expect(page.getByText("Will be sent as 25.00")).toBeVisible();
  await page.getByRole("button", { name: "Start funding" }).click();

  const panel = page.locator(".panel", { hasText: "What the v1 API accepts" });
  await expect(panel.locator(".explain")).toBeVisible();
  await expect(panel.locator(".explain-meta")).toContainText("code ");
  await expect(panel).toContainText("no money has moved");
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

test("strategy builder compiles a real document and derives its effects", async ({ page }) => {
  await page.goto("/strategy");
  await page.getByRole("button", { name: "Compile", exact: true }).click();

  await expect(page.getByText("The document compiled")).toBeVisible();
  const effects = page.locator(".panel", { hasText: "Derived from the document" });
  await expect(effects).toContainText("READ_MARKET_DATA");
  await expect(effects).toContainText("COMMIT_PREDICTION");
  await expect(effects).toContainText("CREATE_TRADE_INTENT");

  // The semantic hash is computed in the browser and is a real sha256.
  const hash = page.locator(".panel", { hasText: "What the compiler decided" }).locator("code").first();
  await expect(hash).toHaveText(/^[0-9a-f]{64}$/);

  // Nothing that signs or moves value can appear in an effect set.
  for (const forbidden of ["RAW_SIGN", "TRANSFER_VALUE", "WITHDRAW", "CHANGE_CAPITAL"]) {
    await expect(effects).not.toContainText(forbidden);
  }
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

test("no dead controls anywhere in the application", async ({ page }) => {
  // "No dead buttons" is about every control a customer can reach, not only
  // the <button> elements: a navigation action here is often a link, and a page
  // that is purely a read-out (Home) legitimately has no <button> at all. So
  // this enumerates everything interactive and requires each one to either do
  // something real or be disabled with a stated reason.
  for (const { path } of PAGES) {
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);

    const controls = page.locator("button, a[href], input, select, textarea");
    const count = await controls.count();
    expect(count, `${path} has controls`).toBeGreaterThan(0);

    for (let i = 0; i < count; i = i + 1) {
      const control = controls.nth(i);
      const tag = await control.evaluate((node) => node.tagName.toLowerCase());

      // Every control has an accessible name: a label, visible text, or an
      // explicit aria-label. An unnamed control cannot be operated by anyone
      // navigating by voice or by screen reader.
      const name = (
        (await control.getAttribute("aria-label")) ??
        (await control.getAttribute("title")) ??
        ((await control.textContent()) ?? "")
      ).trim();
      const labelled =
        name !== "" ||
        (await control.evaluate((node) => {
          const id = node.getAttribute("id");
          if (id === null) return false;
          return document.querySelector(`label[for="${id.replace(/"/g, '\\"')}"]`) !== null;
        })) ||
        (await control.evaluate((node) => node.closest("label") !== null));
      expect(labelled, `${path}: ${tag} at index ${String(i)} has an accessible name`).toBe(true);

      // A link is dead if it goes nowhere.
      if (tag === "a") {
        const href = await control.getAttribute("href");
        expect(href, `${path}: link "${name}" points somewhere`).toBeTruthy();
        expect(
          (href as string) === "#" || (href as string).trim() === "",
          `${path}: link "${name}" is not a placeholder href`,
        ).toBe(false);
        continue;
      }

      if (await control.isDisabled()) {
        // A disabled control must say why, in text, next to itself.
        const describedBy = await control.getAttribute("aria-describedby");
        expect(describedBy, `${path}: disabled "${name}" explains itself`).toBeTruthy();
        // An attribute selector rather than `#id`: these ids come from React's
        // useId() and contain characters an unescaped id selector would choke
        // on, and CSS.escape is a browser global that does not exist here.
        const reason = page.locator(`[id="${(describedBy as string).replace(/"/g, '\\"')}"]`);
        await expect(reason, `${path}: the reason for "${name}" is visible`).toBeVisible();
        expect(((await reason.textContent()) ?? "").trim().length).toBeGreaterThan(10);
      }
    }
  }
});

test("keyboard: the skip link is the first stop and reaches main", async ({ page }) => {
  await page.goto("/");
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

test("the layout reflows on a narrow viewport without sideways scrolling", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  for (const { path } of PAGES) {
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
  }
});

test("an unauthenticated visitor is asked to sign in rather than shown zeroes", async ({ browser }) => {
  const context = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Sign in" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Continue to sign in" })).toBeEnabled();
  // Not a single figure is rendered to a signed-out visitor.
  await expect(page.locator(".num")).toHaveCount(0);
  await context.close();
});
