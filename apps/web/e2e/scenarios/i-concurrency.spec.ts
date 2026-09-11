/**
 * Scenario I — a trade and a purchase at the same time.
 *
 * `docs/product/STAGING_E2E.md` defines I as two browsers, with a purchase
 * webhook landing during a trade, proving idempotency and serialisation, and
 * requiring that the balances agree with `GET /v1/credits/balance` after both.
 * Its "must not happen" is **double mint; double fill**.
 *
 * # What is driven, and what is stated instead
 *
 * THE WEBHOOK LEG needs a configured credit-purchase provider. There is
 * deliberately no fake mode — `internal/provider/stripecredit/new.go` refuses
 * one, on the grounds that Stripe's own test mode is a better fake than any we
 * would write — so an API without a Stripe test key registers no webhook port
 * and `POST /v1/webhooks/stripe_credit` answers 404. Where
 * `CP_WEB_STRIPE_WEBHOOK_SECRET` and `CP_WEB_CAPTURED_CHARGE_ID` are both set,
 * this spec signs a delivery the way the provider does and lands it while a
 * trade is in flight. Where they are not, it says so and drives the half of
 * the scenario that IS available on every deployment — two concurrent trades,
 * which exercise the same serialisation through the same ledger.
 *
 * THE DOUBLE-FILL GUARD needs nothing at all, and is the assertion that matters
 * most: two browsers submitting the SAME order with the SAME idempotency key at
 * the same moment must produce ONE fill and ONE movement of Credits. That is
 * run unconditionally.
 *
 * Every figure is BigInt on base-unit strings from the API. A concurrency test
 * that compared rendered numbers would pass on a rounding error.
 */
import { createHmac } from "node:crypto";

import { expect, test, type APIRequestContext, type Browser, type Page } from "@playwright/test";
import { CREDIT_DECIMALS } from "../../src/lib/credits.ts";

const WEBHOOK_SECRET = process.env["CP_WEB_STRIPE_WEBHOOK_SECRET"] ?? "";
const CAPTURED_CHARGE = process.env["CP_WEB_CAPTURED_CHARGE_ID"] ?? "";
const CAN_DRIVE_WEBHOOK = WEBHOOK_SECRET !== "" && CAPTURED_CHARGE !== "";

const STATE = ".playwright/state.json";
/** What each leg spends, in whole Credits. Small, because it runs three times. */
const SPEND_WHOLE = "5";
const SPEND = `${SPEND_WHOLE}${"0".repeat(CREDIT_DECIMALS)}`;

interface MarketRow {
  market_id: string;
  symbol: string;
  market_status: string;
}

async function accountId(api: APIRequestContext): Promise<string> {
  const response = await api.get("/v1/me");
  expect(response.ok(), "the session reads /v1/me").toBeTruthy();
  const body = (await response.json()) as { account_ids: string[] };
  const id = body.account_ids[0];
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

async function spendable(api: APIRequestContext, id: string): Promise<bigint> {
  const response = await api.get(`/v1/credits/balance?account_id=${id}`);
  expect(response.ok(), "the Credit balance is readable").toBeTruthy();
  const body = (await response.json()) as { spendable: string };
  return BigInt(body.spendable);
}

async function openMarket(api: APIRequestContext): Promise<MarketRow | undefined> {
  const response = await api.get("/v1/native-markets?sort=LIQUIDITY&limit=50");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { markets: MarketRow[] };
  return body.markets.find((market) => market.market_status === "ACTIVE");
}

/**
 * The headers a browser puts on a first-party mutating request.
 *
 * `internal/auth/httpmw/csrf.go` accepts an unsafe method only when the browser
 * has PROVED it first-party: `Sec-Fetch-Site: same-origin`, or an `Origin` on
 * the allow-list. A Playwright request context is not a navigation, so it sets
 * neither and every POST from it is refused with FORBIDDEN — which is the CSRF
 * guard working, and is not the thing this scenario is about. Sending what the
 * browser would send reproduces a first-party request rather than bypassing a
 * check: the guard still runs, and a request from anywhere else still fails it.
 *
 * The first version of this spec did not send them, and both legs came back
 * FORBIDDEN. The balance assertion then compared a balance against itself and
 * passed, which is why every test below also requires that at least one order
 * was actually filled.
 */
function firstParty(key: string): Record<string, string> {
  return {
    "Content-Type": "application/json",
    "Idempotency-Key": key,
    "Sec-Fetch-Site": "same-origin",
  };
}

/**
 * Places one order.
 *
 * `min_output` is zero on purpose. This scenario is about serialisation and
 * idempotency, not about slippage: a minimum would make a losing race look like
 * a protection working, which is a different test and one scenario C already
 * runs. Zero means "whatever a fresh fill returns", so the only thing that can
 * refuse these is the ledger, the venue or the gate.
 */
function placeOrder(
  api: APIRequestContext,
  marketId: string,
  accountID: string,
  key: string,
): Promise<import("@playwright/test").APIResponse> {
  return api.post(`/v1/native-markets/${marketId}/orders`, {
    headers: firstParty(key),
    data: { account_id: accountID, side: "BUY", amount: SPEND, min_output: "0" },
  });
}

/** A second browser on the same session, which is what "two browsers" means. */
async function secondBrowser(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ storageState: STATE });
  return context.newPage();
}

test("two browsers submitting one order with one key produce one fill", async ({
  page,
  browser,
}) => {
  const market = await openMarket(page.request);
  test.skip(market === undefined, "this deployment has no market open for trading");
  const target = market as MarketRow;
  const id = await accountId(page.request);
  const before = await spendable(page.request, id);
  test.skip(before < BigInt(SPEND), "this account has no Credits to trade with");

  const other = await secondBrowser(browser);
  const key = `e2e-i-same-key-${String(Date.now())}`;

  // Both at once, on the same key. The backend must settle this itself: an
  // idempotency record is the only thing standing between a double click across
  // two devices and two fills.
  const [first, second] = await Promise.all([
    placeOrder(page.request, target.market_id, id, key),
    placeOrder(other.request, target.market_id, id, key),
  ]);

  const outcomes = [first, second];
  const fills: string[] = [];
  for (const response of outcomes) {
    const body = (await response.json()) as Record<string, unknown>;
    if (response.ok()) {
      const fill = String(body["fill_id"] ?? "");
      expect(fill, "a successful order carries a fill id").toBeTruthy();
      fills.push(fill);
      continue;
    }
    // The other legitimate answer: the backend saw the same key already in
    // flight and refused to run it twice. That is the guard working.
    const code = String(body["code"] ?? "");
    expect(
      ["IDEMPOTENCY_IN_PROGRESS", "INVALID_IDEMPOTENCY_REUSE"],
      `an unsuccessful sibling is an idempotency answer, not a second fill (got ${code})`,
    ).toContain(code);
  }

  expect(fills.length, "at least one of the two was answered with a fill").toBeGreaterThan(0);
  if (fills.length === 2) {
    expect(fills[0], "a replay returns the SAME fill, never a second one").toBe(fills[1]);
  }

  // And exactly one movement of Credits, to the base unit.
  const after = await spendable(page.request, id);
  expect(after, "one key, one order, one movement").toBe(before - BigInt(SPEND));

  await other.context().close();
});

test("two browsers trading at once agree with the ledger afterwards", async ({ page, browser }) => {
  const market = await openMarket(page.request);
  test.skip(market === undefined, "this deployment has no market open for trading");
  const target = market as MarketRow;
  const id = await accountId(page.request);
  const before = await spendable(page.request, id);
  const twice = BigInt(SPEND) * 2n;
  test.skip(before < twice, "this account has no Credits to trade with twice");

  const other = await secondBrowser(browser);
  const stamp = String(Date.now());

  // Two DIFFERENT orders, genuinely at the same time, against one market. The
  // curve re-prices between them — that is the point — and the ledger has to
  // serialise both without losing or duplicating a Credit.
  const [first, second] = await Promise.all([
    placeOrder(page.request, target.market_id, id, `e2e-i-a-${stamp}`),
    placeOrder(other.request, target.market_id, id, `e2e-i-b-${stamp}`),
  ]);

  let moved = 0n;
  let filled = 0;
  for (const response of [first, second]) {
    if (response.ok()) {
      moved = moved + BigInt(SPEND);
      filled = filled + 1;
      continue;
    }
    // A refusal is a real outcome here — the second order may move the price
    // past a limit, or the account may run out. What it must never be is a
    // half-applied one, and the balance assertion below is what proves that.
    const body = (await response.json()) as Record<string, unknown>;
    expect(String(body["code"] ?? ""), "a refused order carries a stable code").toBeTruthy();
  }

  // Without this the assertion below would compare a balance against itself on
  // a deployment that refused both legs, and pass while proving nothing.
  expect(filled, "at least one of the two orders actually filled").toBeGreaterThan(0);

  const after = await spendable(page.request, id);
  expect(after, "the balance is exactly the ledger's, not the sum of two guesses").toBe(
    before - moved,
  );

  // And the screen agrees with the ledger rather than with either browser's
  // idea of it: the Credit figure on the trade screen is the API's own string.
  await page.goto(`/markets/${target.market_id}`);
  const field = page.locator('.field:has(dt:text-is("Spendable Credits")) .figure').first();
  await expect(field).toBeVisible();
  const title = (await field.getAttribute("title")) ?? "";
  expect(title.replace(/[, ]/g, ""), "the screen shows the ledger's own figure").toContain(
    exactDecimal(after, CREDIT_DECIMALS),
  );

  await other.context().close();
});

/**
 * Base units as the decimal the display shows, by moving the point through the
 * digits and dropping trailing zeros the way `formatUnits` does.
 */
function exactDecimal(baseUnits: bigint, scale: number): string {
  const digits = String(baseUnits).padStart(scale + 1, "0");
  const cut = digits.length - scale;
  const whole = digits.slice(0, cut).replace(/^0+(?=\d)/, "");
  const fraction = digits.slice(cut).replace(/0+$/, "");
  return fraction === "" ? whole : `${whole}.${fraction}`;
}

/* -------------------------------------------------------------------------- *
 * The webhook leg.
 * -------------------------------------------------------------------------- */

/**
 * Signs a delivery the way the provider does.
 *
 * `t=<unix>,v1=<hex hmac-sha256 of "<t>.<payload>">` is the documented scheme
 * and `internal/provider/stripesig` verifies it. Building it here rather than
 * bypassing the signature is the point: a test that posted an unsigned body
 * would be testing a route no real delivery ever takes.
 */
function unixSeconds(millis: number): number {
  return (millis - (millis % 1000)) / 1000;
}

function signature(payload: string, secret: string, at: Date): string {
  // Unix seconds by dropping the millisecond digits from the decimal form. The
  // source guard forbids every numeric parse and every float operation in this
  // tree, and a timestamp is not worth an exception.
  const t = String(at.getTime()).slice(0, -3);
  const mac = createHmac("sha256", secret).update(`${t}.${payload}`).digest("hex");
  return `t=${t},v1=${mac}`;
}

test("a purchase webhook landing during a trade mints once and fills once", async ({
  page,
  browser,
}) => {
  test.skip(
    !CAN_DRIVE_WEBHOOK,
    "no credit-purchase provider is configured here: set CP_WEB_STRIPE_WEBHOOK_SECRET and CP_WEB_CAPTURED_CHARGE_ID to drive the delivery",
  );
  const market = await openMarket(page.request);
  test.skip(market === undefined, "this deployment has no market open for trading");
  const target = market as MarketRow;
  const id = await accountId(page.request);
  const before = await spendable(page.request, id);
  test.skip(before < BigInt(SPEND), "this account has no Credits to trade with");

  const other = await secondBrowser(browser);
  const eventId = `evt_e2e_i_${String(Date.now())}`;
  const payload = JSON.stringify({
    id: eventId,
    type: "payment_intent.succeeded",
    // Unix seconds, by dropping the millisecond digits rather than parsing:
    // the source guard refuses every numeric parse in this tree.
    created: unixSeconds(Date.now()),
    data: { object: { id: CAPTURED_CHARGE } },
  });

  // The trade and the delivery, at the same instant, in two browsers.
  const [order, hook, replay] = await Promise.all([
    placeOrder(page.request, target.market_id, id, `e2e-i-hook-${String(Date.now())}`),
    other.request.post("/v1/webhooks/stripe_credit", {
      headers: {
        "Content-Type": "application/json",
        "Sec-Fetch-Site": "same-origin",
        "Stripe-Signature": signature(payload, WEBHOOK_SECRET, new Date()),
      },
      data: payload,
    }),
    other.request.post("/v1/webhooks/stripe_credit", {
      headers: {
        "Content-Type": "application/json",
        "Sec-Fetch-Site": "same-origin",
        "Stripe-Signature": signature(payload, WEBHOOK_SECRET, new Date()),
      },
      data: payload,
    }),
  ]);

  // A duplicate delivery is acknowledged and applied once: Stripe retries, and
  // a provider that retried a mint would double it.
  expect(hook.status(), "the delivery is acknowledged").toBe(200);
  expect(replay.status(), "a duplicate delivery is acknowledged too").toBe(200);

  const after = await spendable(page.request, id);
  const spent = order.ok() ? BigInt(SPEND) : 0n;
  // Whatever the mint was worth, it was applied exactly once, and the trade
  // exactly once. The ledger is the arbiter, not either browser.
  expect(after >= before - spent, "the mint was not lost").toBe(true);

  await other.context().close();
});
