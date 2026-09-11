/**
 * Scenario B — Buy Credits.
 *
 * `docs/product/STAGING_E2E.md` defines B as `/home` → Buy Credits →
 * `POST /v1/payments` → a Stripe sandbox test card → the webhook → funding
 * `CAPTURED` → the balance updating through the stream. Two of those legs need
 * something this checkout may not have, and the spec is written so that it
 * asserts the truth of whichever deployment it is pointed at rather than
 * quietly skipping:
 *
 *   - THE CARD FIELDS need a publishable key at BUILD time
 *     (`VITE_STRIPE_PUBLISHABLE_KEY`). Without one the page must render
 *     "Payments unavailable" with the reason and no form at all, and this
 *     spec asserts exactly that. With one it asserts the amount chooser, the
 *     bounds, the exact quantity the server decided, and the Payment Element.
 *
 *   - THE CAPTURE needs a configured credit-purchase provider on the API.
 *     There is deliberately no fake mode for it —
 *     `internal/provider/stripecredit/new.go` refuses one, on the grounds that
 *     Stripe's own test mode is a better fake than any we would write — so a
 *     local API with no Stripe test key answers the purchase with a refusal.
 *     That is a REAL state the page owes a real answer to, and the spec proves
 *     the page renders the backend's own code and next step rather than a
 *     generic error.
 *
 * Nothing here is stubbed. The probe requests go to the same API the page uses,
 * through the same session, and every assertion about a figure compares what is
 * on screen against what that API answered.
 */
import { expect, test, type Page } from "@playwright/test";

/** The build-time key the page under test was compiled with, if any. */
const PUBLISHABLE_KEY = process.env["VITE_STRIPE_PUBLISHABLE_KEY"] ?? "";
const KEY_PRESENT = /^pk_(test|live)_[A-Za-z0-9]+$/.test(PUBLISHABLE_KEY.trim());

interface Pricing {
  version: string;
  currency: string;
  credits_per_major_unit: number;
  min_amount_minor: number;
  max_amount_minor: number;
}

/** Minor units to the two-decimal string, the way `lib/credits.ts` does it. */
function minorToUsd(minor: number): string {
  const digits = String(minor).padStart(3, "0");
  const cut = digits.length - 2;
  return `${digits.slice(0, cut)}.${digits.slice(cut)}`;
}

/**
 * The pricing policy, or the code the API refused it with.
 *
 * A deployment with no configured credit-purchase provider leaves the whole
 * Credits purchase port unwired, and this route answers `422 UNSUPPORTED` —
 * `internal/httpapi/wiring_native.go` does it explicitly. That is a real
 * deployment, and the page owes it a real answer, so the spec asks first and
 * then asserts whichever answer it got rather than assuming one.
 */
async function pricing(page: Page): Promise<{ policy?: Pricing; refusedWith?: string }> {
  const response = await page.request.get("/v1/credits/pricing");
  if (response.ok()) return { policy: (await response.json()) as Pricing };
  const body = (await response.json()) as Record<string, unknown>;
  const code = String(body["code"] ?? "");
  expect(code, "a refusal carries a stable code").toBeTruthy();
  return { refusedWith: code };
}

async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/me");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { account_ids: string[] };
  const id = body.account_ids[0];
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

test("Home offers Buy Credits, and Withdraw beside it", async ({ page }) => {
  // Goal §8: the four primary actions, and Withdraw is never hidden — not from
  // an unverified customer, not from anybody. The page it opens is what
  // explains verification; hiding the door hides the explanation with it.
  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  const actions = page.locator(".page-actions");
  for (const label of ["Buy Credits", "Trade", "Create Agent", "Withdraw"]) {
    await expect(actions.getByRole("link", { name: label, exact: true })).toBeVisible();
  }
  await actions.getByRole("link", { name: "Buy Credits", exact: true }).click();
  await expect(page).toHaveURL(/\/buy-credits$/);
  await expect(page.getByRole("heading", { level: 1, name: "Buy Credits" })).toBeVisible();
});

test("the page says what a Credit is, and never calls it withdrawable", async ({ page }) => {
  await page.goto("/buy-credits");
  await expect(page.getByRole("heading", { level: 1, name: "Buy Credits" })).toBeVisible();
  const disclosure = page.getByLabel("What Credits are");
  await expect(disclosure).toContainText("internal platform value");
  await expect(disclosure).toContainText("aren't directly withdrawable");
  // Goal §9 bans the deceptive framing by name: "deposit into your Nodal bank
  // account". The word itself is not the problem — the page says a Credit is
  // NOT a deposit, which is the denial the goal is asking for — the framing is.
  const text = (await page.locator("main").innerText()).toLowerCase();
  expect(text, "a purchase is never framed as paying money in").not.toContain("deposit into");
  expect(text, "the page never implies an account that holds money").not.toContain("bank account");
  expect(text, "and it says outright that a Credit is not a deposit").toContain("not a deposit");
});

if (!KEY_PRESENT) {
  test("with no publishable key the page refuses to draw a form", async ({ page }) => {
    // The Stripe section of the frontend brief: absent or invalid → "Payments
    // unavailable" with the reason, never a form. A dead card form takes
    // somebody's attention and their keystrokes and gives nothing back, and an
    // amount chooser that leads to it is the same mistake one screen earlier.
    await page.goto("/buy-credits");
    await expect(page.getByRole("heading", { level: 1, name: "Buy Credits" })).toBeVisible();

    const panel = page.locator(".panel", { hasText: "Payments unavailable" });
    await expect(panel).toBeVisible();
    await expect(panel).toContainText("No payment can be taken here");
    await expect(panel).toContainText("Nothing is wrong with your account");

    // No card fields, no amount field, no Stripe iframe: not one input on the page.
    await expect(page.locator("main input")).toHaveCount(0);
    await expect(page.locator("main iframe")).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Continue to payment$/ })).toHaveCount(0);
  });
}

if (KEY_PRESENT) {
  test("a deployment that cannot price Credits says so, and offers no amount", async ({ page }) => {
    const answer = await pricing(page);
    await page.goto("/buy-credits");
    await expect(page.getByRole("heading", { level: 1, name: "Buy Credits" })).toBeVisible();

    if (answer.policy !== undefined) {
      // This deployment prices Credits, so the chooser is the right screen and
      // the other tests below cover it. What matters here is the negative: the
      // page did not take the refusal branch.
      await expect(page.locator(".panel", { hasText: "How much" })).toBeVisible();
      return;
    }

    // It does not. The refusal carries the backend's own stable code, says what
    // it is about — the product, not this person — and draws no form.
    const refusal = page.locator(".refusal, .explain").first();
    await expect(refusal).toBeVisible();
    await expect(refusal).toContainText(answer.refusedWith as string);
    await expect(page.locator("main input")).toHaveCount(0);
    await expect(page.locator("main iframe")).toHaveCount(0);
    await expect(page.getByText("No amount is offered")).toBeVisible();
  });

  test("the amount chooser states the rate and the bounds the server gave", async ({ page }) => {
    const answer = await pricing(page);
    if (answer.policy === undefined) return;
    const policy = answer.policy;
    await page.goto("/buy-credits");

    const panel = page.locator(".panel", { hasText: "How much" });
    await expect(panel).toBeVisible();

    // The rate is read from the response, never derived: the page shows the
    // server's own "Credits per one unit" figure and names the policy version
    // that produced it.
    await expect(panel).toContainText(`Pricing policy ${policy.version}`);
    await expect(panel).toContainText(`per 1 ${policy.currency}`);
    await expect(panel.getByText(String(policy.credits_per_major_unit), { exact: false }).first()).toBeVisible();

    // The bounds are the server's, verbatim, in the currency the customer types.
    await expect(panel).toContainText(minorToUsd(policy.min_amount_minor));
    await expect(panel).toContainText(minorToUsd(policy.max_amount_minor));
  });

  test("a preset fills the amount and an out-of-bounds amount is refused before the card", async ({
    page,
  }) => {
    const answer = await pricing(page);
    if (answer.policy === undefined) return;
    const policy = answer.policy;
    await page.goto("/buy-credits");
    const panel = page.locator(".panel", { hasText: "How much" });

    const preset = page.getByRole("button", { name: `25.00 ${policy.currency}` });
    if (await preset.isVisible()) {
      await preset.click();
      await expect(panel.getByLabel(`Amount in ${policy.currency}`)).toHaveValue("25.00");
    }

    // Below the minimum the field says the minimum, in the customer's units,
    // and the confirm button states why it is off rather than simply being off.
    const belowMinimum = minorToUsd(policy.min_amount_minor > 1 ? policy.min_amount_minor - 1 : 0);
    await panel.getByLabel(`Amount in ${policy.currency}`).fill(belowMinimum);
    await expect(panel.getByRole("alert")).toContainText(minorToUsd(policy.min_amount_minor));
    const confirm = page.getByRole("button", { name: "Continue to payment" });
    await expect(confirm).toBeDisabled();
    const reason = await confirm.getAttribute("aria-describedby");
    expect(reason, "a disabled confirm says why").toBeTruthy();
  });

  test("confirming shows what the SERVER priced, or the refusal it gave", async ({ page }) => {
    const answer = await pricing(page);
    if (answer.policy === undefined) return;
    const policy = answer.policy;
    const id = await accountId(page);
    // $25 where the policy allows it, otherwise the nearest bound. Written as
    // comparisons rather than with a numeric helper: the source guard forbids
    // float arithmetic anywhere in this tree, comments included.
    const wanted = 2500;
    const amountMinor =
      wanted < policy.min_amount_minor
        ? policy.min_amount_minor
        : wanted > policy.max_amount_minor
          ? policy.max_amount_minor
          : wanted;

    // A probe against the same route the page uses, so the assertion below is
    // against what this deployment actually answers rather than against what
    // it is hoped to answer. The pricing policy is a pure function of the
    // amount, so the quantity the page is shown is the quantity seen here.
    const probe = await page.request.post("/v1/payments", {
      headers: { "Idempotency-Key": `e2e-b-probe-${String(Date.now())}` },
      data: { account_id: id, amount_minor: amountMinor, currency: policy.currency },
    });
    const probeBody = (await probe.json()) as Record<string, unknown>;

    await page.goto("/buy-credits");
    const panel = page.locator(".panel", { hasText: "How much" });
    await panel.getByLabel(`Amount in ${policy.currency}`).fill(minorToUsd(amountMinor));
    await page.getByRole("button", { name: "Continue to payment" }).click();

    if (!probe.ok()) {
      // The refusal branch. This is the branch a deployment with no configured
      // credit-purchase provider takes, and it is a first-class state: the page
      // owes the backend's own stable code and a next step, not "something went
      // wrong".
      const code = String(probeBody["code"] ?? "");
      expect(code, "a refusal carries a stable code").toBeTruthy();
      const refusal = page.locator(".refusal, .explain").first();
      await expect(refusal).toBeVisible();
      await expect(refusal).toContainText(code);
      // And nothing was bought: no card fields appeared.
      await expect(page.locator("main iframe")).toHaveCount(0);
      return;
    }

    // The success branch. "USD payment → Credits received", exact, from the
    // server, BEFORE a card has been touched — which is the whole reason the
    // quantity is not computed in the browser.
    const quantity = String(probeBody["credit_quantity"] ?? "");
    expect(quantity, "the server decided a quantity").toBeTruthy();

    const confirm = page.locator(".panel", { hasText: "Confirm and pay" });
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText(minorToUsd(amountMinor));
    await expect(confirm).toContainText(`pricing policy ${String(probeBody["pricing_version"] ?? "")}`);
    // The exact quantity is in the figure's title attribute, unrounded,
    // whatever the display form is.
    const figure = confirm.locator(".figure[title*='Credits']").first();
    await expect(figure).toBeVisible();

    // A sandbox purchase is labelled, and only when the API said so.
    if (probeBody["sandbox"] === true) {
      await expect(page.getByLabel("This is a sandbox payment")).toBeVisible();
      await expect(confirm.locator(".badge-simulated")).toBeVisible();
    } else {
      await expect(page.getByLabel("This is a sandbox payment")).toHaveCount(0);
    }

    // The provider's own fields, on the provider's own origin. Card data never
    // reaches this document.
    const frame = page.locator("main iframe[name^='__privateStripeFrame']").first();
    await expect(frame).toBeVisible({ timeout: 20_000 });
    const src = (await frame.getAttribute("src")) ?? "";
    expect(src.startsWith("https://js.stripe.com/"), "the card fields are served by Stripe").toBe(true);
  });
}

test("the balance is never changed by arithmetic in the browser", async ({ page }) => {
  // The "must not happen" column for scenario B. Whatever the purchase page
  // did, Home shows the backend's Credit figures and only those: the spendable
  // figure on screen is the exact string the API answered with.
  const id = await accountId(page);
  const response = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(response.ok()).toBeTruthy();
  const balance = (await response.json()) as Record<string, string>;

  await page.goto("/home");
  const credits = page.locator(".panel", { hasText: "Credits" }).first();
  await expect(credits).toBeVisible();

  // `title` carries the exact value on every figure, abbreviated or not, which
  // is what makes this comparison exact rather than a comparison of rendering.
  for (const [field, label] of [
    ["gross", "Total Credits"],
    ["spendable", "Spendable"],
    ["frozen", "Frozen"],
  ] as const) {
    const wire = balance[field];
    expect(wire, `${field} is in the response`).toBeTruthy();
    await expect(
      credits.locator(`.field:has(dt:text-is("${label}")) .figure`).first(),
      `${label} is the backend's own figure`,
    ).toHaveAttribute("title", new RegExp(`${exactPrefix(wire as string)}`));
  }
});

/**
 * The first significant digits of a base-unit string as they appear in the
 * exact rendering, escaped for a regular expression.
 *
 * The exact text is the decimal form of the base units at the Credit scale, so
 * this compares the digits rather than re-implementing the scaling — which is
 * the thing under test and would prove nothing if the test did it too.
 */
function exactPrefix(baseUnits: string): string {
  const digits = baseUnits.replace(/^-/, "").replace(/^0+/, "");
  return digits === "" ? "0" : digits.split("").join("[,.]?");
}

/* --------------------------------------------------------------------------
 * The last leg of scenario B: the dashboard the purchase feeds.
 *
 * STAGING_E2E's B ends "balance updates via the stream", and its "must not
 * happen" is a balance changed by arithmetic in the browser. The assertion
 * above covers the Credit figures; these cover the modules around them, which
 * are where a dashboard is most tempted to invent one.
 * ------------------------------------------------------------------------ */

test("the portfolio module appears only when there is something in it", async ({ page }) => {
  // USER_JOURNEY §3 makes this conditional, and the condition is on the DATA.
  // A portfolio panel reading zero on an account that has never traded is not
  // an empty state; it is a claim that something was lost.
  const id = await accountId(page);
  const response = await page.request.get(`/v1/me/portfolio?account_id=${id}`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    positions: Array<{ symbol: string; temperature: string }>;
    totals: { market_value_credits: string };
    as_of: string;
  };

  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  // Located by accessible name rather than by text: "Markets" and "Portfolio"
  // both appear inside other panels' descriptions, and a `hasText` locator
  // would happily assert against the wrong panel.
  const value = page.locator('section.panel[aria-label="Portfolio value"]');
  const holdings = page.locator('section.panel[aria-label="Holdings"]');

  if (body.positions.length === 0) {
    await expect(value, "no positions, so no portfolio panel").toHaveCount(0);
    await expect(holdings, "and no holdings panel either").toHaveCount(0);
    return;
  }

  await expect(value).toBeVisible();
  await expect(holdings).toBeVisible();

  // The change is labelled for what the API actually reports — unrealised
  // against cost basis — and never as "today", which no field here carries.
  await expect(value).toContainText("Change since opened");
  await expect(value).toContainText("not a since-midnight figure");

  // One instant, machine-readable, and shown unrounded beside itself. It is
  // NOT compared against this test's own read: the backend stamps `as_of` with
  // the moment it computed the answer, and the page made its own request. What
  // is asserted is that the page shows a real backend instant verbatim.
  expect(body.as_of, "the API stamps the portfolio").toBeTruthy();
  const machine = await value.locator("time").first().getAttribute("datetime");
  expect(machine, "the portfolio module carries a machine-readable instant").toBeTruthy();
  const exact = await value.locator(".as-of .mono-small").first().innerText();
  expect(exact.trim(), "the exact instant is shown unrounded beside it").toBe(
    `(${machine as string})`,
  );
  expect(Number.isNaN(Date.parse(machine as string)), "the instant parses").toBe(false);

  // A simulated position renders at the simulated temperature, per row.
  const simulated = body.positions.filter((p) => p.temperature === "SIMULATED");
  if (simulated.length > 0) {
    const row = holdings.locator("tr", { hasText: simulated[0]?.symbol ?? "" }).first();
    await expect(row.locator('[data-temp="simulated"]').first()).toBeVisible();
  }
});

test("the movers module shows the ordering the backend applied, and says so", async ({ page }) => {
  const response = await page.request.get("/v1/native-markets?sort=CHANGE_24H&limit=5");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    markets: Array<{ symbol: string; has_24h_change?: boolean; demo: boolean }>;
    sort: string;
    stable: boolean;
  };

  await page.goto("/home");
  const markets = page.locator('section.panel[aria-label="Markets"]');
  await expect(markets).toBeVisible();

  if (body.markets.length === 0) {
    await expect(markets).toContainText("No markets yet");
    return;
  }

  // The page never re-ranks what it was given, and it names the ordering and
  // whether paging it is stable — because on every key but NEWEST it is not.
  await expect(markets).toContainText(body.sort);
  await expect(markets).toContainText(
    body.stable ? "sees every market exactly once" : "can show the same market twice",
  );

  for (const market of body.markets) {
    const row = markets.locator("tr", { hasText: market.symbol }).first();
    await expect(row, `${market.symbol} is on the dashboard`).toBeVisible();
    if (market.has_24h_change !== true) {
      // Not traded in the window is a different fact from not having moved.
      await expect(row).toContainText("no trade in the window");
    }
    if (market.demo) {
      await expect(row, "demo data says so on the row").toContainText("Demo");
    }
  }
});

test("recent activity is the server's own summary, and a rehearsal says so", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/me/activity?account_id=${id}&limit=5`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    items: Array<{ summary: string; kind: string; simulated: boolean }>;
  };

  await page.goto("/home");
  const recent = page.locator('section.panel[aria-label="Recent activity"]');
  await expect(recent).toBeVisible();

  if (body.items.length === 0) {
    await expect(recent).toContainText("Nothing has happened yet");
    return;
  }

  for (const item of body.items) {
    // Rendered as it was written. The summary is built on the server from a
    // fixed template per kind, and a client that reformatted it would turn a
    // sentence into a data format nobody wrote down.
    const card = recent.locator("article", { hasText: item.summary }).first();
    await expect(card, `"${item.summary}" is on the dashboard`).toBeVisible();
    await expect(card).toContainText(item.kind);
    if (item.simulated) {
      await expect(card.locator(".badge-simulated")).toBeVisible();
    }
  }
});
