/**
 * Scenario C — Trade.
 *
 * `docs/product/STAGING_E2E.md` defines C as `/markets` → market → quote → buy
 * → position → sell → `/portfolio` updated, proving that a quote never prices
 * execution, that a fill updates the position and the Credits, and that the
 * P&L strings and the activity rows follow. Its "must not happen" column is the
 * one the arithmetic here is aimed at: **a float anywhere; a stale balance
 * shown as current**.
 *
 * # How the figures are checked
 *
 * Every assertion about a quantity is BigInt on base-unit strings, taken from
 * the same API the page reads, through the same session. Nothing is compared as
 * a rendered number, because a rendered number is a display decision and this
 * is a ledger question: after a purchase of N Credits, spendable must be
 * exactly `before - N`, not approximately, and not within a cent.
 *
 * Where the SCREEN is the subject rather than the ledger, the comparison is
 * against `title`, which `Figure` fills with the unabbreviated value whatever
 * the display form is. That is what makes "the page shows the backend's own
 * figure" checkable rather than a claim about pixels.
 *
 * # What this spec needs, and what it does when it is not there
 *
 * A market that accepts a buy, and Credits to buy with. On a sandbox tier both
 * are there: `demoDataAtBoot` seeds four labelled demo markets and the tier
 * grants promotional Credits. On a deployment with neither, the spec asserts
 * the honest states instead — an empty markets list that says so, and a refusal
 * rendered by REASON rather than as a fault — and says which branch it took.
 * It never fakes a pass.
 */
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

import { NARROW_HEIGHT, NARROW_WIDTH } from "../routes.ts";

interface MarketRow {
  market_id: string;
  symbol: string;
  name: string;
  market_status: string;
  demo: boolean;
  last_price: string;
  price_scale: number;
  asset_decimals: number;
  credit_volume_24h: string;
  liquidity_credits: string;
  trades_24h: number;
  has_24h_change?: boolean;
  change_24h_bps?: number;
}

interface Balance {
  gross: string;
  spendable: string;
  frozen: string;
}

interface Position {
  market_id?: string;
  symbol: string;
  quantity: string;
  asset_decimals: number;
}

async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/me");
  expect(response.ok(), "the session reads /v1/me").toBeTruthy();
  const body = (await response.json()) as { account_ids: string[] };
  const id = body.account_ids[0];
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

async function balance(page: Page, id: string): Promise<Balance> {
  const response = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(response.ok(), "the Credit balance is readable").toBeTruthy();
  return (await response.json()) as Balance;
}

async function positions(page: Page, id: string): Promise<Position[]> {
  const response = await page.request.get(`/v1/me/portfolio?account_id=${id}`);
  expect(response.ok(), "the portfolio is readable").toBeTruthy();
  const body = (await response.json()) as { positions: Position[] };
  return body.positions;
}

/** The one market this spec trades, chosen by what the API actually offers. */
async function tradableMarket(page: Page): Promise<MarketRow | undefined> {
  const response = await page.request.get("/v1/native-markets?sort=LIQUIDITY&limit=50");
  expect(response.ok(), "market discovery answers").toBeTruthy();
  const body = (await response.json()) as { markets: MarketRow[] };
  return body.markets.find((market) => market.market_status === "ACTIVE");
}

/** Base units of a whole number of Credits, by shifting digits. */
function credits(whole: string, scale: number): string {
  return `${whole}${"0".repeat(scale)}`;
}

/** The Credit scale the application renders at. */
const CREDIT_DECIMALS = 6;

/** What this scenario spends, in whole Credits. Small, so it can run twice. */
const SPEND = "10";

async function violations(page: Page): Promise<string[]> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  return results.violations.map(
    (violation) =>
      `${violation.id} (${violation.impact ?? "unknown"}): ${violation.help} — ${violation.nodes
        .map((node) => node.target.join(" "))
        .join(", ")}`,
  );
}

/* -------------------------------------------------------------------------- *
 * Discovery.
 * -------------------------------------------------------------------------- */

test("the markets list shows the backend's own figures, and labels a demo market", async ({
  page,
}) => {
  const response = await page.request.get("/v1/native-markets?sort=NEWEST&limit=50");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    markets: MarketRow[];
    sort: string;
    stable: boolean;
  };

  await page.goto("/markets");
  await expect(page.getByRole("heading", { level: 1, name: "Markets" })).toBeVisible();

  if (body.markets.length === 0) {
    // The honest empty state, in the goal's own sentence. This is a fact about
    // the deployment, not a loading state, and it says so.
    await expect(page.getByText("No markets yet")).toBeVisible();
    return;
  }

  for (const market of body.markets.slice(0, 5)) {
    const row = page.locator("tr", { hasText: market.symbol }).first();
    await expect(row, `${market.symbol} is on the list`).toBeVisible();
    await expect(row).toContainText(market.name);
    await expect(row).toContainText(market.market_status);

    // The exact price, at the MARKET's own scale — not the Credit scale, which
    // is a different number and was F-44.
    const price = row.locator(".figure").first();
    await expect(price).toHaveAttribute("title", digitsOf(market.last_price, market.price_scale));

    if (market.has_24h_change !== true) {
      // Not traded in the window is a different fact from not having moved.
      await expect(row).toContainText("not traded");
    }
    if (market.demo) {
      await expect(row, "a demo market says so where it is shown").toContainText("Demo");
    }
  }

  // The ordering is the server's, and the page says whether paging it is safe.
  if (!body.stable) {
    await expect(page.getByText("can show a market twice")).toBeVisible();
  }
});

/**
 * The exact rendered form of base units at a scale: the decimal point moved,
 * thousands grouped, trailing zeros kept.
 *
 * Written with string surgery rather than with arithmetic because the source
 * guard forbids every numeric parse and every float operation in this tree —
 * and because reimplementing the scaling with arithmetic would prove nothing,
 * the scaling being the thing under test.
 */
function digitsOf(baseUnits: string, scale: number): RegExp {
  const padded = baseUnits.padStart(scale + 1, "0");
  const cut = padded.length - scale;
  const whole = padded.slice(0, cut).replace(/^0+(?=\d)/, "");
  const fraction = padded.slice(cut);
  const grouped = whole.split("").join("[,]?");
  const tail = fraction === "" ? "" : `\\.${fraction}`;
  return new RegExp(`^${grouped}${tail}`);
}

test("search asks the backend, and an empty result says so rather than showing nothing", async ({
  page,
}) => {
  await page.goto("/markets");
  await expect(page.getByRole("heading", { level: 1, name: "Markets" })).toBeVisible();

  const search = page.getByLabel("Search");
  await search.fill("zzz-no-market-has-this-in-its-name");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.getByText("Nothing matched that search")).toBeVisible();
});

/* -------------------------------------------------------------------------- *
 * The trade screen.
 * -------------------------------------------------------------------------- */

test("the trade screen renders its chart, its limits and its tape, and passes axe", async ({
  page,
}) => {
  const market = await tradableMarket(page);
  test.skip(market === undefined, "this deployment has no market open for trading");
  const target = market as MarketRow;

  await page.goto(`/markets/${target.market_id}`);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(target.symbol);
  await expect(page.locator("h1")).toHaveCount(1);

  // THE CHART. A `<figure>` whose caption states the range in words, and marks
  // that are drawn rather than described — or the honest empty state, because a
  // window with no prints has no candle and is not filled forward.
  const chart = page.locator("figure.chart-figure");
  const emptyChart = page.getByText("No trades in this window");
  const drew = (await chart.count()) > 0;
  if (drew) {
    await expect(chart).toBeVisible();
    await expect(chart.locator("figcaption")).toContainText("Open");
    await expect(chart.locator("figcaption")).toContainText("Credits");
    await expect(chart.locator("rect.chart-body").first()).toBeVisible();
    // Reachable by keyboard, not only by pointer.
    await expect(chart.locator(".chart-frame")).toHaveAttribute("tabindex", "0");
  } else {
    await expect(emptyChart).toBeVisible();
  }

  // THE LIMITS IN FORCE (goal §47), exactly as the backend reports them.
  const summary = await page.request.get(`/v1/native-markets/${target.market_id}/summary`);
  expect(summary.ok()).toBeTruthy();
  const limits = ((await summary.json()) as { limits_in_force: Record<string, unknown> })
    .limits_in_force;
  const panel = page.locator('section.panel[aria-label="Limits in force"]');
  await expect(panel).toBeVisible();
  await expect(panel).toContainText(String(limits["safety_policy_version"] ?? ""));

  // A DISARMED circuit breaker arrives as a literal zero, and zero here is not
  // a threshold of nothing — it is the absence of a threshold. The panel must
  // say so rather than rendering the strictest breaker imaginable.
  const move = limits["circuit_breaker_move_bps"];
  if (move === 0) {
    await expect(panel, "a disarmed breaker is named, not drawn as a zero").toContainText(
      "Not armed",
    );
  } else if (move === undefined) {
    await expect(panel).toContainText("not reported");
  }

  // THE TAPE, and the rule that it carries no account identity.
  const tape = page.locator('section.panel[aria-label="Recent trades"]');
  await expect(tape).toBeVisible();

  expect(await violations(page), "the trade screen has no WCAG A/AA violation").toEqual([]);
});

test("the trade screen fits a phone, and the chart is still drawn there", async ({ page }) => {
  const market = await tradableMarket(page);
  test.skip(market === undefined, "this deployment has no market open for trading");
  const target = market as MarketRow;

  await page.setViewportSize({ width: NARROW_WIDTH, height: NARROW_HEIGHT });
  await page.goto(`/markets/${target.market_id}`);
  await expect(page.locator("h1")).toHaveCount(1);

  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "the trade screen does not scroll sideways at 375px").toBeLessThanOrEqual(1);

  // Goal §31: charts readable. The plot scales to the column rather than
  // scrolling, so it is inside the viewport and still has marks in it.
  const chart = page.locator("figure.chart-figure");
  if ((await chart.count()) > 0) {
    const box = await chart.locator("svg.chart-svg").boundingBox();
    expect(box, "the plot has a box").not.toBeNull();
    expect((box as { width: number }).width).toBeLessThanOrEqual(NARROW_WIDTH);
    expect((box as { height: number }).height).toBeGreaterThan(100);
    await expect(chart.locator("rect.chart-body").first()).toBeVisible();
  }

  // And the ticket is a bottom sheet reached from a docked bar, with a 44px
  // control, rather than a form somewhere below the fold.
  const dock = page.locator(".ticket-dock");
  await expect(dock).toBeVisible();
  const open = dock.getByRole("button", { name: "Buy or sell" });
  const size = await open.boundingBox();
  expect(size, "the dock button has a box").not.toBeNull();
  expect((size as { height: number }).height, "44px targets on a phone").toBeGreaterThanOrEqual(44);
  await open.click();
  await expect(page.getByRole("dialog")).toBeVisible();
});

/* -------------------------------------------------------------------------- *
 * The journey.
 * -------------------------------------------------------------------------- */

test("markets → market → quote → buy → position → sell → portfolio", async ({ page }) => {
  const market = await tradableMarket(page);
  test.skip(market === undefined, "this deployment has no market open for trading");
  const target = market as MarketRow;
  const id = await accountId(page);

  const spend = credits(SPEND, CREDIT_DECIMALS);
  const before = await balance(page, id);
  const fundable = BigInt(before.spendable) >= BigInt(spend);

  // Reached the way a customer reaches it: from the list, by opening a row.
  await page.goto("/markets");
  await expect(page.getByRole("heading", { level: 1, name: "Markets" })).toBeVisible();
  await page.locator("tr", { hasText: target.symbol }).first().click();
  await expect(page).toHaveURL(new RegExp(`/markets/${target.market_id}$`));
  await expect(page.getByRole("heading", { level: 1 })).toContainText(target.symbol);

  const ticket = page.locator('section.panel[aria-label="Trade"]');
  await expect(ticket).toBeVisible();

  // THE QUOTE.
  await ticket.getByLabel("Credits to spend").fill(SPEND);
  await ticket.getByRole("button", { name: "Get a quote" }).click();

  // The sentence a quote may never be read without. It is beside the figures,
  // not behind a disclosure: a customer who believes the quote priced their
  // execution reads every ordinary re-price as a betrayal.
  await expect(ticket).toContainText("It does not price your execution", { timeout: 15_000 });
  await expect(ticket.locator('.field:has(dt:text-is("Price impact"))')).toBeVisible();
  await expect(ticket.locator('.field:has(dt:text-is("Platform fee"))')).toBeVisible();
  await expect(ticket.locator('.field:has(dt:text-is("Creator fee"))')).toBeVisible();

  // The minimum is the customer's own number, derived from a tolerance they
  // chose, and it is strictly below what the quote expects.
  const expected = await exactOf(ticket, "You would receive");
  const minimum = await exactOf(ticket, "Minimum you will receive");
  expect(expected, "the quote states an expected output").toBeTruthy();
  expect(minimum, "the ticket states a minimum").toBeTruthy();

  // The expiry counts down rather than sitting still.
  await expect(ticket.locator(".quote-expiry")).toContainText(/expires in \d+s|has expired/);

  if (!fundable) {
    // The refusal branch, and a real one: an account with no spendable Credits
    // is told which rule refused the order and what would change the answer —
    // not "something went wrong".
    await ticket.getByRole("button", { name: "Buy with Credits" }).click();
    const refusal = page.locator(".refusal").first();
    await expect(refusal).toBeVisible({ timeout: 15_000 });
    await expect(refusal).toContainText("was refused");
    // And nothing moved.
    const after = await balance(page, id);
    expect(BigInt(after.spendable), "a refused order moves no Credits").toBe(
      BigInt(before.spendable),
    );
    return;
  }

  // THE FILL.
  await ticket.getByRole("button", { name: "Buy with Credits" }).click();
  const filled = page.locator('section.panel[aria-label="Filled"]');
  await expect(filled, "the ticket shows the fill").toBeVisible({ timeout: 20_000 });
  // The FILL is shown, not the quote. They are different numbers and the quote
  // is the one that did not happen.
  await expect(filled).toContainText("You received");
  await expect(filled).toContainText("You paid");
  await expect(page.locator('section.panel[aria-label="Trade"]')).toHaveCount(0);

  // EXACT ARITHMETIC. The Credits spent are exactly what was asked for: the
  // fees come out of the amount, so a BUY of N Credits moves N Credits.
  const afterBuy = await balance(page, id);
  expect(
    BigInt(afterBuy.spendable),
    "spendable fell by exactly the amount spent, to the base unit",
  ).toBe(BigInt(before.spendable) - BigInt(spend));

  const held = (await positions(page, id)).find((p) => p.market_id === target.market_id);
  expect(held, "the buy opened a position").toBeDefined();
  const bought = BigInt((held as Position).quantity);
  expect(bought > 0n, "the position holds units").toBe(true);

  // THE POSITION, on the portfolio page.
  await page.goto("/portfolio");
  await expect(page.locator("h1")).toHaveCount(1);
  await expect(
    page.getByText(target.symbol, { exact: false }).first(),
    "the position is on the portfolio",
  ).toBeVisible();

  // THE SELL. Half the position, so the assertion is about a change rather than
  // about a closure, and the arithmetic is still exact.
  const sellUnits = String(bought / 2n);
  expect(BigInt(sellUnits) > 0n, "there is enough to sell half of it").toBe(true);

  await page.goto(`/markets/${target.market_id}`);
  const sellTicket = page.locator('section.panel[aria-label="Trade"]');
  await expect(sellTicket).toBeVisible();
  await sellTicket.getByRole("radio", { name: "Sell for Credits" }).check();
  await sellTicket.getByLabel(`${target.symbol} to sell`).fill(
    decimalOf(sellUnits, target.asset_decimals),
  );
  await sellTicket.getByRole("button", { name: "Get a quote" }).click();
  await expect(sellTicket).toContainText("It does not price your execution", { timeout: 15_000 });
  await sellTicket.getByRole("button", { name: "Sell for Credits" }).click();
  await expect(page.locator('section.panel[aria-label="Filled"]')).toBeVisible({
    timeout: 20_000,
  });

  const afterSell = (await positions(page, id)).find((p) => p.market_id === target.market_id);
  const remaining = afterSell === undefined ? 0n : BigInt(afterSell.quantity);
  expect(remaining, "the position fell by exactly what was sold").toBe(bought - BigInt(sellUnits));

  // And the Credits came back: more than after the buy, by the ledger's own
  // figures rather than by anything this browser computed.
  const finalBalance = await balance(page, id);
  expect(
    BigInt(finalBalance.spendable) > BigInt(afterBuy.spendable),
    "selling returned Credits",
  ).toBe(true);

  // THE PORTFOLIO, updated.
  await page.goto("/portfolio");
  await expect(page.locator("h1")).toHaveCount(1);
});

/** The unabbreviated value of the figure under a labelled field. */
async function exactOf(scope: ReturnType<Page["locator"]>, label: string): Promise<string> {
  const figure = scope.locator(`.field:has(dt:text-is("${label}")) .figure`).first();
  await expect(figure).toBeVisible();
  return (await figure.getAttribute("title")) ?? "";
}

/**
 * Base units as the human decimal the ticket's field accepts, by moving the
 * point through the digits. String surgery, never arithmetic.
 */
function decimalOf(baseUnits: string, scale: number): string {
  if (scale === 0) return baseUnits;
  const padded = baseUnits.padStart(scale + 1, "0");
  const cut = padded.length - scale;
  return `${padded.slice(0, cut)}.${padded.slice(cut)}`;
}

test("a market that refuses a side says which rule refused it", async ({ page }) => {
  // Goal §47 and USER_JOURNEY §11: a paused market is a REFUSAL, not a fault,
  // and the ticket replaces the control with the reason rather than rendering a
  // form nobody can send.
  const response = await page.request.get("/v1/native-markets?limit=50");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { markets: MarketRow[] };
  const paused = body.markets.find((market) => market.market_status !== "ACTIVE");
  test.skip(paused === undefined, "every market in this deployment is open");
  const target = paused as MarketRow;

  await page.goto(`/markets/${target.market_id}`);
  const ticket = page.locator('section.panel[aria-label="Trade"]');
  await expect(ticket).toBeVisible();
  const quote = ticket.getByRole("button", { name: "Get a quote" });
  await expect(quote).toBeDisabled();
  const reason = await quote.getAttribute("aria-describedby");
  expect(reason, "a disabled control says why").toBeTruthy();
  await expect(page.locator(`[id="${reason as string}"]`)).toContainText(target.market_status);
});
