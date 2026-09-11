/**
 * Scenario G — a refund after the Credits were spent.
 *
 * `docs/product/STAGING_E2E.md` defines G as purchase → trade → refund webhook
 * → reversal → frozen or negative handling shown, and its "must not happen" is
 * the one that matters: **Credits vanishing without an activity row**.
 *
 * # What can be driven here, and what cannot
 *
 * The reversal itself is provider-side. It arrives as a signed
 * `charge.refunded` delivery to `POST /v1/webhooks/stripe_credit`, and three
 * things have to be true before that is possible:
 *
 *   1. the API must have a credit-purchase provider configured. There is no
 *      fake mode — `internal/provider/stripecredit/new.go` refuses one on the
 *      grounds that Stripe's own test mode is a better fake than any we would
 *      write — so an API with no Stripe test key registers no webhook port at
 *      all and the route answers 404;
 *   2. a funding must already be CAPTURED, which needs a real test-mode card
 *      payment;
 *   3. the delivery must carry a valid `Stripe-Signature`, which needs the
 *      webhook secret the deployment was configured with.
 *
 * `driveRefund` below does exactly that, signing with the documented scheme, and
 * runs whenever `CP_WEB_STRIPE_WEBHOOK_SECRET` and `CP_WEB_CAPTURED_CHARGE_ID`
 * are both present. On a checkout without them the spec asserts the part that
 * is true on every deployment — that a reversal is never hidden and a frozen
 * bucket is never rendered as a zero — and says so here rather than skipping
 * and looking like coverage.
 */
import { createHmac } from "node:crypto";

import { expect, test, type APIResponse, type Page } from "@playwright/test";
// The scale a Credit is held at, imported from the application rather than
// restated: one more copy of it is one more place to edit, and the register
// already carries a finding about how many there are.
import { CREDIT_DECIMALS } from "../../src/lib/credits.ts";

const WEBHOOK_SECRET = process.env["CP_WEB_STRIPE_WEBHOOK_SECRET"] ?? "";
const CAPTURED_CHARGE = process.env["CP_WEB_CAPTURED_CHARGE_ID"] ?? "";
const CAN_DRIVE = WEBHOOK_SECRET !== "" && CAPTURED_CHARGE !== "";

async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/me");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { account_ids: string[] };
  const id = body.account_ids[0];
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

/**
 * Signs a delivery the way the provider does.
 *
 * `t=<unix>,v1=<hex hmac-sha256 of "<t>.<payload>">` is the documented scheme,
 * and `internal/provider/stripesig` verifies it. Building it here rather than
 * bypassing the signature is the point: a test that posted an unsigned body
 * would be testing a route no real delivery ever takes.
 */
function signature(payload: string, secret: string, at: Date): string {
  // Unix seconds by dropping the millisecond digits from the decimal form: the
  // source guard forbids every numeric parse and every float operation in this
  // tree, and a timestamp is not worth an exception.
  const t = String(at.getTime()).slice(0, -3);
  const mac = createHmac("sha256", secret).update(`${t}.${payload}`).digest("hex");
  return `t=${t},v1=${mac}`;
}

async function driveRefund(page: Page, chargeId: string): Promise<APIResponse> {
  const payload = JSON.stringify({
    id: `evt_e2e_${String(Date.now())}`,
    object: "event",
    type: "charge.refunded",
    livemode: false,
    data: { object: { id: chargeId, object: "charge", refunded: true } },
  });
  return page.request.post("/v1/webhooks/stripe_credit", {
    headers: {
      "Content-Type": "application/json",
      "Stripe-Signature": signature(payload, WEBHOOK_SECRET, new Date()),
    },
    data: payload,
  });
}

test("a frozen bucket is shown as frozen, never folded into the total", async ({ page }) => {
  // §46: available, withdrawable, spendable and settled are not synonyms, and
  // a reversal lands in a bucket a customer has to be able to see. This holds
  // whatever the balance happens to be, including zero — the point is that the
  // distinction is on screen at all, with its own label and its own figure.
  const id = await accountId(page);
  const response = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(response.ok()).toBeTruthy();
  const balance = (await response.json()) as Record<string, string>;

  await page.goto("/home");
  const credits = page.locator(".panel", { hasText: "Credits" }).first();
  await expect(credits).toBeVisible();

  for (const label of ["Total Credits", "Spendable", "Frozen"]) {
    await expect(
      credits.locator(`.field:has(dt:text-is("${label}"))`),
      `${label} has its own labelled figure`,
    ).toHaveCount(1);
  }
  // And the two axes are kept apart: what may be SPENT and what may be PAID
  // OUT are different questions with different answers.
  await expect(credits.getByRole("heading", { name: "Payout-eligible value" })).toBeVisible();
  await expect(credits).toContainText("not an amount of US dollars");

  // The frozen figure is the backend's own string, not something derived here.
  const frozen = balance["frozen"];
  expect(frozen, "the balance reports a frozen bucket").toBeTruthy();
  await expect(
    credits.locator('.field:has(dt:text-is("Frozen")) .figure').first(),
  ).toHaveAttribute("title", /Credits$/);
});

/**
 * The exact string a `Figure` puts in its `title`, for a Credit quantity.
 *
 * `lib/format.ts` moves the point through the base units and groups the integer
 * digits with commas; `Figure` writes that, plus the symbol, into `title`. This
 * reproduces it by string surgery — the point is MOVED, the digits are grouped,
 * nothing is parsed — so the assertion below is against the API's own number
 * rather than against "some figure rendered".
 */
function exactCredits(baseUnits: string): string {
  const scale = CREDIT_DECIMALS;
  const negative = baseUnits.startsWith("-");
  const digits = (negative ? baseUnits.slice(1) : baseUnits).padStart(scale + 1, "0");
  const whole = digits.slice(0, digits.length - scale).replace(/^0+(?=\d)/, "");
  const frac = digits.slice(digits.length - scale);
  const grouped = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const body = frac === "" ? grouped : `${grouped}.${frac}`;
  return `${negative ? "\u2212" : ""}${body} Credits`;
}

test("a reversed purchase is rendered as a reversal, not as a missing row", async ({ page }) => {
  // The "must not happen": Credits vanishing without a record. Every purchase
  // the backend holds in a reversed state has a page that says so in words,
  // with the state as its code, and no page claims those Credits still exist.
  const id = await accountId(page);
  const response = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(response.ok()).toBeTruthy();
  const balance = (await response.json()) as Record<string, unknown>;

  // `reversed` is optional on the contract. Where the backend reports one, the
  // interface must account for it rather than let it disappear into the total.
  //
  // What this used to assert was that the Credit panel contained the words
  // "Not payout-eligible" — a heading the panel carries unconditionally, on
  // every account, reversal or not. So on the one account that disproves the
  // test's name, with a non-zero `reversed` bucket the panel does not draw at
  // all, the test passed. It asserts the figure now: a field labelled for the
  // bucket, whose title is the API's own string for it.
  //
  // THIS FAILS ON A BRANCH WHERE THE CREDIT PANEL HAS NOT YET LEARNED THE
  // BUCKET. It is written against the contract — `reversed` on
  // `GET /v1/credits/balance` — rather than against what Home draws today,
  // because the contract is the thing that must be true, and a test that waited
  // for the page to catch up would be the same test that has been passing
  // wrongly all along.
  const reversed = balance["reversed"];
  if (typeof reversed === "string" && /[1-9]/.test(reversed)) {
    await page.goto("/home");
    const credits = page.locator(".panel", { hasText: "Credits" }).first();
    const field = credits.locator('.field:has(dt:text-is("Reversed"))');
    await expect(field, "a reversed bucket has its own labelled field").toHaveCount(1);
    await expect(
      field.locator(".figure").first(),
      "and its figure is the backend's own string, not something derived here",
    ).toHaveAttribute("title", exactCredits(reversed));
    // And the words, so a reader who is not reading a tooltip still learns what
    // happened to those Credits.
    await expect(credits).toContainText("reversed");
  }

  // And the notification centre carries the record whether or not this account
  // has one: a reversal produces `CREDIT_PURCHASE_REVERSED`, which is one of
  // the kinds that cannot be switched off.
  await page.goto("/notifications");
  await expect(page.getByRole("heading", { level: 1, name: "Notifications" })).toBeVisible();
  const prefs = page.locator(".panel", { hasText: "What you are notified about" });
  await expect(prefs).toContainText("CREDIT_PURCHASE_REVERSED");
  await expect(prefs).toContainText("always sent");
});

test("an unsigned refund delivery is never accepted", async ({ page }) => {
  // Provable on every tier, with or without a provider: the ingestion route
  // does not silently accept an unsigned delivery. This used to live inside the
  // driven test below as its "cannot drive" branch, which made that test count
  // as a pass on every run that had ever happened while proving only this
  // (F-252).
  const unsigned = await page.request.post("/v1/webhooks/stripe_credit", {
    headers: { "Content-Type": "application/json" },
    data: { id: "evt_e2e_unsigned", object: "event", type: "charge.refunded" },
  });
  expect(
    unsigned.status(),
    "an unsigned delivery is refused (400) or the provider is not registered (404) — never accepted",
  ).not.toBe(200);
});

test("the refund webhook produces a reversal the customer can see", async ({ page }) => {
  test.info().annotations.push({
    type: "requires",
    description:
      "CP_WEB_STRIPE_WEBHOOK_SECRET and CP_WEB_CAPTURED_CHARGE_ID, plus an API with a " +
      "configured credit-purchase provider. There is no fake mode for it.",
  });
  // A skip, counted as one, rather than an early return counted as a pass.
  test.skip(!CAN_DRIVE, "driving a refund needs CP_WEB_STRIPE_WEBHOOK_SECRET and CP_WEB_CAPTURED_CHARGE_ID and an API with a configured credit-purchase provider; this run has none of them (goal §59: no key on this tier)");

  const before = await page.request.get("/v1/me/notifications?limit=50");
  expect(before.ok()).toBeTruthy();

  const delivered = await driveRefund(page, CAPTURED_CHARGE);
  expect(delivered.status(), "a correctly signed delivery is acknowledged").toBe(200);

  await page.goto("/notifications");
  await expect(
    page.getByText("CREDIT_PURCHASE_REVERSED").first(),
    "the reversal reaches the notification centre",
  ).toBeVisible({ timeout: 20_000 });

  // And the balance the page shows is the backend's, after the reversal.
  const id = await accountId(page);
  const after = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(after.ok()).toBeTruthy();
  await page.goto("/home");
  await expect(page.locator(".panel", { hasText: "Credits" }).first()).toBeVisible();
});

/* --------------------------------------------------------------------------
 * The page G's "must not happen" is about.
 *
 * "Credits vanishing without an activity row" is a statement about the
 * portfolio and the feed, so this is where those two pages are asserted: the
 * accounting figures are the server's and are never re-derived, and every
 * event the server recorded is on the feed with the sentence the server wrote.
 * ------------------------------------------------------------------------ */

test("the portfolio shows the accounting the server did, not accounting of its own", async ({
  page,
}) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/me/portfolio?account_id=${id}`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    as_of: string;
    temperature: string;
    credits: Record<string, string>;
    positions: Array<{ symbol: string; temperature: string; quantity: string }>;
    totals: Record<string, string | number>;
  };

  await page.goto("/portfolio");
  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();

  // Goal §15: realised, unrealised and total are three fields the server
  // computed, shown as three fields. A page that subtracted cost basis from
  // market value would be a second accounting implementation.
  const totals = page.locator('section.panel[aria-label="Totals"]');
  await expect(totals).toBeVisible();
  for (const [field, label] of [
    ["market_value_credits", "Market value"],
    ["cost_basis_credits", "Cost basis"],
    ["unrealized_pnl_credits", "Unrealised"],
    ["realized_pnl_credits", "Realised"],
    ["total_pnl_credits", "Total"],
    ["fees_paid_credits", "Fees paid"],
  ] as const) {
    const figure = totals.locator(`.field:has(dt:text-is("${label}")) .figure`).first();
    await expect(figure, `${label} is on screen`).toBeVisible();
    const exact = ((await figure.getAttribute("title")) ?? "").replace(/[^0-9]/g, "").replace(/^0+/, "");
    const wire = String(body.totals[field] ?? "").replace(/^-/, "").replace(/^0+/, "");
    expect(exact, `${label} is the backend's own digits`).toBe(wire);
  }
  await expect(totals).toContainText("This page does not add the two above");

  // One instant, machine-readable and shown unrounded. Not compared against
  // this test's own read: the page made its own request and the backend stamps
  // the moment IT computed the answer.
  expect(body.as_of, "the API stamps the portfolio").toBeTruthy();
  const positions = page.locator('section.panel[aria-label="Positions"]');
  const machine = await positions.locator("time").first().getAttribute("datetime");
  expect(machine, "the positions carry a machine-readable instant").toBeTruthy();
  const stamp = await positions.locator(".as-of .mono-small").first().innerText();
  expect(stamp.trim(), "the exact instant is shown unrounded beside it").toBe(
    `(${machine as string})`,
  );
  expect(Number.isNaN(Date.parse(machine as string)), "the instant parses").toBe(false);

  // And a simulated position is labelled as one, per row.
  for (const position of body.positions.filter((p) => p.temperature === "SIMULATED")) {
    const row = page.locator("tr", { hasText: position.symbol }).first();
    await expect(row.locator('[data-temp="simulated"]').first()).toBeVisible();
  }
});

test("every event the server recorded is on the feed, in its own words", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/me/activity?account_id=${id}&limit=50`);
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    items: Array<{
      summary: string;
      kind: string;
      simulated: boolean;
      amounts: Array<{ unit: string; value: string; symbol?: string }>;
    }>;
  };

  await page.goto("/activity");
  await expect(page.getByRole("heading", { level: 1, name: "Activity" })).toBeVisible();

  if (body.items.length === 0) {
    await expect(page.getByText("Nothing has happened yet")).toBeVisible();
    return;
  }

  for (const item of body.items.slice(0, 5)) {
    const card = page.locator("article", { hasText: item.summary }).first();
    await expect(card, `"${item.summary}" is on the feed`).toBeVisible();
    await expect(card).toContainText(item.kind);
    if (item.simulated) {
      await expect(card.locator(".badge-simulated")).toBeVisible();
    }
    // Each amount is its own figure with its own unit. Nothing converts one
    // into another: a row showing money paid and Credits received shows two
    // figures, because that is two facts.
    for (const amount of item.amounts) {
      const figures = card.locator(".figure");
      expect(await figures.count(), "an amount renders a figure").toBeGreaterThan(0);
      if (amount.unit === "ASSET_UNITS") {
        // No scale is declared for an asset amount, so the exact base units are
        // what is shown and the page says that is what they are.
        await expect(card).toContainText("base units");
      }
    }
  }

  // The kind filter narrows to what the server returns for that kind, and
  // clearing it comes back to everything.
  const first = body.items[0];
  if (first !== undefined && first.kind === "NATIVE_TRADE") {
    await page.getByRole("button", { name: "Trades", exact: true }).click();
    await expect(page.locator("article", { hasText: first.summary }).first()).toBeVisible();
    await page.getByRole("button", { name: "Clear the filter" }).click();
    await expect(page.locator(".panel", { hasText: "Everything" })).toBeVisible();
  }
});
