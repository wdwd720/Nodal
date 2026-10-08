/**
 * The order-book section of the advanced (sandbox) area against the real API
 * (internal/orders, ADR-0020).
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database,
 * and every assertion compares what a page shows with what the backend
 * answered — including where it answers no. The seed may or may not hold an
 * internal book; each test states what it expects in either case and never
 * invents the missing half.
 *
 *   - every page renders under the simulated-value notice;
 *   - every book is value domain SIMULATED on the SIMULATED_INTERNAL_BOOK
 *     venue, discloses its fee schedules in its own quote asset and the
 *     crossing rule in force, and the page draws it as sent;
 *   - an account's orders and trades are its own: a foreign account is
 *     FORBIDDEN, an absent order or trade proof NOT_FOUND, on the wire and on
 *     the page;
 *   - commands are refused with their stable codes (malformed, foreign,
 *     operator-only), no route of this API changes the crossing rule or
 *     places a house order, and an order the backend refuses is shown with
 *     the very code it sent, linked to the order it recorded.
 */
import { randomUUID } from "node:crypto";

import { expect, test, type Page, type Response } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";

const PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/advanced/order-books", heading: "Order books" },
  { path: "/advanced/order-books/orders", heading: "My book orders" },
  { path: "/advanced/order-books/trades", heading: "My book trades" },
];

/** A well-formed UUIDv7 that names nothing. */
const ABSENT = "01a00000-0000-7000-8000-0000000000dd";
/** A well-formed account id this session does not own. */
const FOREIGN_ACCOUNT = "01a00000-0000-7000-8000-0000000000de";

interface Problem {
  readonly code: string;
  readonly detail?: string;
  readonly fields?: Record<string, unknown>;
}

interface Schedule {
  readonly quote_asset_id: string;
  readonly quote_decimals: number;
  readonly percentage_fee_bps: number;
  readonly hash: string;
}

interface BookRow {
  readonly id: string;
  readonly instrument_id: string;
  readonly value_domain: string;
  readonly venue_label: string;
  readonly status: string;
  readonly mode: string;
  readonly base_symbol: string;
  readonly base_decimals: number;
  readonly quote_symbol: string;
  readonly quote_asset_id: string;
  readonly quote_decimals: number;
  readonly tick_price: string;
  readonly min_quantity: string;
  readonly maker_fee: Schedule;
  readonly taker_fee: Schedule;
  readonly crossing_rule: { readonly book_id: string; readonly version: number; readonly is_default: boolean; readonly allow_house_customer_crossing: boolean };
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

/** The account the application reads: the first one the backend lists for this session. */
async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/accounts");
  expect(response.ok()).toBeTruthy();
  const accounts = (await response.json()) as Array<{ id: string }>;
  const id = accounts[0]?.id;
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
}

/**
 * A command sent the way the application sends one: a same-origin fetch from
 * the page, with the session cookie and an Idempotency-Key, so the backend's
 * CSRF check sees exactly what it sees from the app.
 */
async function command(page: Page, path: string, body: unknown): Promise<{ readonly status: number; readonly body: unknown }> {
  return page.evaluate(
    async ({ url, payload, key }) => {
      const response = await fetch(url, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json", "Idempotency-Key": key },
        body: JSON.stringify(payload),
      });
      const text = await response.text();
      let parsed: unknown = undefined;
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = undefined;
      }
      return { status: response.status, body: parsed };
    },
    { url: `/v1${path}`, payload: body, key: randomUUID() },
  );
}

async function expectCode(result: { readonly status: number; readonly body: unknown }, status: number, code: string): Promise<void> {
  expect(result.status, JSON.stringify(result.body)).toBe(status);
  expect(isProblem(result.body), "a refusal is a problem document").toBe(true);
  expect((result.body as Problem).code).toBe(code);
}

/** Base units as the decimal the order form takes ("1500", 3 -> "1.5"): digits moved, never computed. */
function asDecimal(units: string, decimals: number): string {
  if (decimals === 0) return units;
  const padded = units.padStart(decimals + 1, "0");
  const whole = padded.slice(0, padded.length - decimals);
  const fraction = padded.slice(padded.length - decimals).replace(/0+$/, "");
  return fraction === "" ? whole : `${whole}.${fraction}`;
}

async function listBooks(page: Page): Promise<BookRow[]> {
  const response = await page.request.get("/v1/advanced/books?limit=200");
  expect(response.status(), "the book list is served").toBe(200);
  const body = (await response.json()) as { value_domain: string; items: BookRow[]; next_cursor: string | null };
  expect(body.value_domain).toBe("SIMULATED");
  expect("next_cursor" in body, "the cursor is present, even when null").toBe(true);
  return body.items;
}

test.describe("the order-book section", () => {
  for (const { path, heading } of PAGES) {
    test(`${heading} renders under the simulated-value notice`, async ({ page }) => {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1, name: heading, exact: true })).toBeVisible();
      const notice = page.getByRole("note", { name: NOTICE, exact: true });
      await expect(notice).toBeVisible();
      await expect(notice).toContainText("LIVE is not implemented in this build");
      await expect(page.getByRole("navigation", { name: "Order book pages", exact: true })).toBeVisible();
      await settle(page);
      await expect(page.locator(".malformed")).toHaveCount(0);
      await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
    });
  }

  test("is a section of the advanced area", async ({ page }) => {
    await page.goto("/advanced/overview");
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Order books", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Order books", exact: true })).toBeVisible();
  });
});

test("books are simulated internal books that disclose their fees and crossing rule, drawn as sent", async ({ page }) => {
  const books = await listBooks(page);
  for (const book of books) {
    expect(book.value_domain).toBe("SIMULATED");
    expect(book.venue_label).toBe("SIMULATED_INTERNAL_BOOK");
    expect(["OPEN", "HALTED", "CLOSED"]).toContain(book.status);
    expect(["PAPER", "SANDBOX"]).toContain(book.mode);
    for (const schedule of [book.maker_fee, book.taker_fee]) {
      expect(schedule.quote_asset_id.toLowerCase(), "a fee schedule is quoted in the book's quote asset").toBe(book.quote_asset_id.toLowerCase());
      expect(schedule.hash).toMatch(/^[0-9a-f]{64}$/);
      expect(Number.isInteger(schedule.percentage_fee_bps)).toBe(true);
    }
    expect(book.crossing_rule.book_id.toLowerCase()).toBe(book.id.toLowerCase());
    if (book.crossing_rule.is_default) {
      expect(book.crossing_rule.allow_house_customer_crossing, "by default the house never trades with a customer").toBe(false);
    }
  }

  await page.goto("/advanced/order-books");
  await settle(page);
  if (books.length === 0) {
    await expect(page.getByText("No order books", { exact: true })).toBeVisible();
    return;
  }
  const table = page.getByRole("table", { name: "Order books", exact: true });
  await expect(table.locator("tbody tr")).toHaveCount(books.length);
  const first = books[0] as BookRow;
  await table.getByRole("link", { name: `${first.base_symbol}/${first.quote_symbol}`, exact: true }).first().click();
  await expect(page.getByRole("heading", { level: 1, name: `${first.base_symbol}/${first.quote_symbol} order book`, exact: true })).toBeVisible();
  await settle(page);
  const crossing = page.getByRole("region", { name: "Crossing rule", exact: true });
  await expect(crossing).toContainText(first.crossing_rule.allow_house_customer_crossing ? "ALLOWED" : "FORBIDDEN");
  await expect(page.getByRole("region", { name: "Disclosed fees", exact: true })).toContainText(first.taker_fee.hash);
  await expect(page.getByRole("region", { name: "Depth", exact: true })).toBeVisible();
  // A customer is not offered the operator's controls; the backend would refuse them anyway.
  await expect(page.getByRole("region", { name: "Book status", exact: true })).toHaveCount(0);
});

test("an account's orders and trades are its own; another account's are refused and an absent order is not found", async ({ page }) => {
  const own = await accountId(page);
  for (const path of [`/v1/advanced/books/orders?account_id=${own}`, `/v1/advanced/books/orders?scope=ALL&account_id=${own}`, `/v1/advanced/books/trades?account_id=${own}`]) {
    const mine = await page.request.get(path);
    expect(mine.status(), path).toBe(200);
    const body = (await mine.json()) as { value_domain: string; items: unknown[]; next_cursor: string | null };
    expect(body.value_domain).toBe("SIMULATED");
    expect("next_cursor" in body).toBe(true);
  }
  for (const path of ["orders", "trades"]) {
    const foreign = await page.request.get(`/v1/advanced/books/${path}?account_id=${FOREIGN_ACCOUNT}`);
    expect(foreign.status(), path).toBe(403);
    expect(((await foreign.json()) as Problem).code).toBe("FORBIDDEN");
  }
  const badScope = await page.request.get(`/v1/advanced/books/orders?scope=SOME&account_id=${own}`);
  expect(badScope.status()).toBe(400);

  const absent = await page.request.get(`/v1/advanced/books/orders/${ABSENT}`);
  expect(absent.status()).toBe(404);
  expect(((await absent.json()) as Problem).code).toBe("NOT_FOUND");
  const proof = await page.request.get(`/v1/advanced/books/${ABSENT}/trades/1/proof`);
  expect(proof.status()).toBe(404);

  await page.goto(`/advanced/order-books/orders/${ABSENT}`);
  await settle(page);
  await expect(page.locator(".explain-meta").first()).toContainText("code NOT_FOUND");
  await page.goto(`/advanced/order-books/books/${ABSENT}/trades/1`);
  await settle(page);
  await expect(page.locator(".explain-meta").first()).toContainText("code NOT_FOUND");

  const orders = (await (await page.request.get(`/v1/advanced/books/orders?account_id=${own}`)).json()) as { items: unknown[] };
  await page.goto("/advanced/order-books/orders");
  await settle(page);
  if (orders.items.length === 0) {
    await expect(page.getByText("No working orders", { exact: true })).toBeVisible();
  } else {
    await expect(page.getByRole("table", { name: "Orders on this page", exact: true }).locator("tbody tr")).toHaveCount(orders.items.length);
  }
});

test("commands are refused with their stable codes, and none changes the crossing rule or places a house order", async ({ page }) => {
  const own = await accountId(page);
  await page.goto("/advanced/order-books");
  await settle(page);

  const order = { account_id: own, side: "BUY", order_type: "LIMIT", time_in_force: "GTC", price: "1", quantity: "1" };
  await expectCode(await command(page, `/advanced/books/${ABSENT}/orders`, { ...order, quantity: "1.5" }), 400, "VALIDATION_FAILED");
  await expectCode(await command(page, `/advanced/books/${ABSENT}/orders`, { ...order, account_id: FOREIGN_ACCOUNT }), 403, "FORBIDDEN");
  await expectCode(await command(page, `/advanced/books/${ABSENT}/orders`, order), 404, "NOT_FOUND");
  await expectCode(await command(page, `/advanced/books/orders/${ABSENT}/cancel`, { account_id: own }), 404, "NOT_FOUND");
  await expectCode(
    await command(page, `/advanced/books/orders/${ABSENT}/replace`, { account_id: own, price: "1", quantity: "1" }),
    404,
    "NOT_FOUND",
  );
  await expectCode(await command(page, `/advanced/books/orders/${ABSENT}/cancel`, { account_id: FOREIGN_ACCOUNT }), 403, "FORBIDDEN");

  // The operator side is not a customer's: refused before anything is read.
  await expectCode(await command(page, `/advanced/books/${ABSENT}/status`, { status: "HALTED", reason: "e2e" }), 403, "FORBIDDEN");
  await expectCode(
    await command(page, "/advanced/books", {
      instrument_id: ABSENT,
      mode: "PAPER",
      maker_fee: { version: "e2e", quote_asset_id: ABSENT, quote_decimals: 6, percentage_fee_bps: 10 },
      taker_fee: { version: "e2e", quote_asset_id: ABSENT, quote_decimals: 6, percentage_fee_bps: 20 },
    }),
    403,
    "FORBIDDEN",
  );
  const journal = await page.request.get(`/v1/advanced/books/${ABSENT}/journal-verification`);
  expect(journal.status()).toBe(403);

  // House orders and the crossing rule go through the treasury service; this API has no route for them.
  for (const path of [`/advanced/books/${ABSENT}/crossing-rule`, `/advanced/books/${ABSENT}/house-orders`]) {
    const refused = await command(page, path, { allow_house_customer_crossing: true, reason: "e2e" });
    expect([404, 405], `${path}: ${JSON.stringify(refused.body)}`).toContain(refused.status);
  }
});

test("an order is confirmed once, and the page shows exactly what the backend answered", async ({ page }) => {
  const books = await listBooks(page);
  const open = books.find((book) => book.status === "OPEN");
  test.skip(open === undefined, "the seed holds no OPEN internal book to place an order on");
  const book = open as BookRow;

  await page.goto(`/advanced/order-books/books/${book.instrument_id}`);
  await settle(page);
  const panel = page.getByRole("region", { name: "Place an order", exact: true });
  const confirm = panel.getByRole("button", { name: /^Confirm: buy on the book/ });
  await expect(confirm).toBeDisabled();
  await expect(panel).toContainText("created when you confirm");

  // One tick and the book's minimum quantity: a resting bid far from the market.
  await panel.getByLabel(`Limit price (${book.quote_symbol})`, { exact: true }).fill(book.tick_price);
  await panel.getByLabel(`Quantity (${book.base_symbol})`, { exact: true }).fill(asDecimal(book.min_quantity, book.base_decimals));
  await expect(confirm).toBeEnabled();
  await expect(panel).toContainText("created when you confirm");

  const answered: Promise<Response> = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/v1/advanced/books/${book.instrument_id}/orders`,
  );
  await confirm.click();
  const response = await answered;
  const sent = response.request().headers()["idempotency-key"] ?? "";
  expect(sent, "the command carried the key created at confirmation").toMatch(/^[0-9a-f-]{36}$/);
  await expect(panel).toContainText(sent);

  const answer: unknown = await response.json();
  if (response.ok()) {
    const result = answer as { value_domain: string; order: { id: string; status: string; account_id: string } };
    expect(result.value_domain).toBe("SIMULATED");
    expect(result.order.account_id).toBe(await accountId(page));
    await expect(panel.locator(".callout")).toContainText(result.order.id);
    await expect(panel.locator(".callout")).toContainText(result.order.status);
  } else {
    expect(isProblem(answer)).toBe(true);
    const problem = answer as Problem;
    await expect(panel.locator(".explain-meta")).toContainText(`code ${problem.code}`);
    const orderId = problem.fields?.["order_id"];
    if (typeof orderId === "string") {
      // A refusal the backend recorded links to the order, which stays readable.
      await expect(panel.getByRole("link", { name: orderId })).toBeVisible();
      const recorded = await page.request.get(`/v1/advanced/books/orders/${orderId}`);
      expect(recorded.status()).toBe(200);
      expect(((await recorded.json()) as { status: string }).status).toBe("REJECTED");
    }
  }
});
