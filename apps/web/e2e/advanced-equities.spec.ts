/**
 * The equities section of the advanced (sandbox) area against the real API
 * (internal/equities, ADR-0020).
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database,
 * and every assertion compares what a page shows with what the backend
 * answered — including where it answers no. The deployment may not run the
 * paper venue at all (the equities routes then answer UNSUPPORTED), and the
 * seed may or may not list an equity; each test states what it expects in
 * every case and never invents the missing half.
 *
 *   - every page renders under the simulated-value notice;
 *   - listings, the session and the calendar are SIMULATED and drawn as sent;
 *   - an account's orders, fills, settlements and corporate-action postings
 *     are its own: a foreign account is FORBIDDEN and an absent order
 *     NOT_FOUND, on the wire and on the page;
 *   - commands are refused with their stable codes (malformed, foreign,
 *     operator-only — the worker runs included), and an order the backend
 *     refuses is shown with the very code it sent, linked to the order it
 *     recorded.
 */
import { randomUUID } from "node:crypto";

import { expect, test, type Page, type Response } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";

const PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/advanced/equities", heading: "Equities" },
  { path: "/advanced/equities/orders", heading: "My equity orders" },
  { path: "/advanced/equities/fills", heading: "Equity fills" },
  { path: "/advanced/equities/settlements", heading: "Equity settlements" },
  { path: "/advanced/equities/corporate-actions", heading: "Corporate actions" },
  { path: "/advanced/equities/calendar", heading: "Equities calendar" },
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

interface Session {
  readonly session: string;
  readonly trade_date: string;
  readonly trading_day: boolean;
  readonly next_trading_day: string;
  readonly settlement_date?: string;
}

interface Listing {
  readonly instrument_id: string;
  readonly value_domain: string;
  readonly symbol: string;
  readonly kind: string;
  readonly share_decimals: number;
  readonly min_quantity: string;
  readonly settlement_symbol: string;
  readonly tick_price: string;
  readonly mark?: { readonly price: string };
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

/**
 * The listings, or undefined when this deployment runs no paper venue — an
 * answer the backend gives as UNSUPPORTED, never as an empty list.
 */
async function listings(page: Page): Promise<{ readonly items: Listing[]; readonly session: Session } | undefined> {
  const response = await page.request.get("/v1/advanced/equities/listings");
  const body: unknown = await response.json();
  if (response.status() === 422 && isProblem(body) && body.code === "UNSUPPORTED") return undefined;
  expect(response.status(), JSON.stringify(body)).toBe(200);
  const list = body as { value_domain: string; items: Listing[]; session: Session };
  expect(list.value_domain).toBe("SIMULATED");
  return list;
}

test.describe("the equities section", () => {
  for (const { path, heading } of PAGES) {
    test(`${heading} renders under the simulated-value notice`, async ({ page }) => {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1, name: heading, exact: true })).toBeVisible();
      const notice = page.getByRole("note", { name: NOTICE, exact: true });
      await expect(notice).toBeVisible();
      await expect(notice).toContainText("LIVE is not implemented in this build");
      await expect(page.getByRole("navigation", { name: "Equities pages", exact: true })).toBeVisible();
      await settle(page);
      await expect(page.locator(".malformed")).toHaveCount(0);
      await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
    });
  }

  test("is a section of the advanced area", async ({ page }) => {
    await page.goto("/advanced/overview");
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Equities", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Equities", exact: true })).toBeVisible();
  });
});

test("listings and the session are simulated and drawn as the backend sent them", async ({ page }) => {
  const list = await listings(page);
  await page.goto("/advanced/equities");
  await settle(page);
  if (list === undefined) {
    // The deployment runs no paper venue: the page says so with the code, and shows no listing.
    await expect(page.locator(".explain-meta").first()).toContainText("code UNSUPPORTED");
    return;
  }
  const { items, session } = list;
  expect(["CLOSED", "PRE_MARKET", "REGULAR", "AFTER_HOURS"]).toContain(session.session);
  expect(session.trade_date).toMatch(/^[0-9]{4}-[0-9]{2}-[0-9]{2}$/);
  expect(session.trading_day, "a trading day says when its trades settle").toBe(session.settlement_date !== undefined);
  if (session.settlement_date !== undefined) expect(session.settlement_date > session.trade_date).toBe(true);
  for (const listing of items) {
    expect(listing.value_domain).toBe("SIMULATED");
    expect(["EQUITY", "ETF"]).toContain(listing.kind);
    expect(listing.min_quantity).toMatch(/^[0-9]+$/);
    if (listing.mark !== undefined) expect(listing.mark.price).toMatch(/^[0-9]+(\.[0-9]+)?$/);
  }

  const sessionPanel = page.getByRole("region", { name: "Market session", exact: true });
  await expect(sessionPanel).toContainText(session.session);
  await expect(sessionPanel).toContainText(session.trade_date);
  if (items.length === 0) {
    await expect(page.getByText("No listings", { exact: true })).toBeVisible();
  } else {
    const table = page.getByRole("table", { name: "Listings", exact: true });
    await expect(table.locator("tbody tr")).toHaveCount(items.length);
    for (const listing of items) {
      await expect(table.getByRole("link", { name: listing.symbol, exact: true })).toBeVisible();
    }
  }

  const calendar = await page.request.get("/v1/advanced/equities/calendar");
  expect(calendar.status()).toBe(200);
  const c = (await calendar.json()) as { settlement_cycle_days: number; holidays: Array<{ date: string; name: string }> };
  expect(c.settlement_cycle_days).toBe(1);
  await page.goto("/advanced/equities/calendar");
  await settle(page);
  await expect(page.getByRole("region", { name: "Now", exact: true })).toContainText("T+1");
  if (c.holidays.length > 0) {
    await expect(page.getByRole("table", { name: "Recorded closures", exact: true }).locator("tbody tr")).toHaveCount(c.holidays.length);
  }
  // A customer is not offered the operator's worker runs; the backend would refuse them anyway.
  await expect(page.getByRole("region", { name: "Settlement run", exact: true })).toHaveCount(0);
});

test("an account's equity records are its own; another account's are refused and an absent order is not found", async ({ page }) => {
  test.skip((await listings(page)) === undefined, "this deployment runs no paper venue");
  const own = await accountId(page);
  for (const path of ["orders", "fills", "settlements", "settlements?state=SETTLED", "corporate-action-applications"]) {
    const separator = path.includes("?") ? "&" : "?";
    const mine = await page.request.get(`/v1/advanced/equities/${path}${separator}account_id=${own}`);
    expect(mine.status(), path).toBe(200);
    const body = (await mine.json()) as { value_domain: string; next_cursor: string | null };
    expect(body.value_domain).toBe("SIMULATED");
    expect("next_cursor" in body, "the cursor is present, even when null").toBe(true);
  }
  for (const path of ["orders", "fills", "settlements", "corporate-action-applications"]) {
    const foreign = await page.request.get(`/v1/advanced/equities/${path}?account_id=${FOREIGN_ACCOUNT}`);
    expect(foreign.status(), path).toBe(403);
    expect(((await foreign.json()) as Problem).code).toBe("FORBIDDEN");
  }
  const badFilter = await page.request.get(`/v1/advanced/equities/settlements?state=SOON&account_id=${own}`);
  expect(badFilter.status()).toBe(400);

  const absent = await page.request.get(`/v1/advanced/equities/orders/${ABSENT}`);
  expect(absent.status()).toBe(404);
  expect(((await absent.json()) as Problem).code).toBe("NOT_FOUND");
  await page.goto(`/advanced/equities/orders/${ABSENT}`);
  await settle(page);
  await expect(page.locator(".explain-meta").first()).toContainText("code NOT_FOUND");

  const pending = (await (await page.request.get(`/v1/advanced/equities/settlements?state=PENDING&account_id=${own}`)).json()) as {
    items: Array<{ state: string; due_date: string; trade_date: string }>;
  };
  for (const s of pending.items) {
    expect(s.state).toBe("PENDING");
    expect(s.due_date > s.trade_date, "T+1: due after the trade date").toBe(true);
  }
  await page.goto("/advanced/equities/settlements");
  await settle(page);
  if (pending.items.length === 0) {
    await expect(page.getByText("Nothing pending", { exact: true })).toBeVisible();
  } else {
    await expect(page.getByRole("table", { name: "Settlements on this page", exact: true }).locator("tbody tr")).toHaveCount(
      pending.items.length,
    );
  }
});

test("commands are refused with their stable codes, the operator's worker runs included", async ({ page }) => {
  test.skip((await listings(page)) === undefined, "this deployment runs no paper venue");
  const own = await accountId(page);
  await page.goto("/advanced/equities");
  await settle(page);

  const order = { account_id: own, instrument_id: ABSENT, side: "BUY", order_type: "MARKET", quantity: "1", mode: "PAPER" };
  await expectCode(await command(page, "/advanced/equities/orders", { ...order, quantity: "1.5" }), 400, "VALIDATION_FAILED");
  await expectCode(await command(page, "/advanced/equities/orders", { ...order, account_id: FOREIGN_ACCOUNT }), 403, "FORBIDDEN");
  await expectCode(await command(page, "/advanced/equities/orders", order), 404, "NOT_FOUND");
  await expectCode(await command(page, `/advanced/equities/orders/${ABSENT}/cancel`, { account_id: own }), 404, "NOT_FOUND");

  // Every operator command is refused to a customer before anything is read.
  for (const [path, body] of [
    ["/advanced/equities/settlement-runs", {}],
    ["/advanced/equities/venue-runs", {}],
    ["/advanced/equities/calendar/holidays", { date: "2031-03-05", name: "e2e" }],
    [`/advanced/equities/corporate-actions/${ABSENT}/apply`, {}],
    [`/advanced/equities/corporate-actions/${ABSENT}/cancel`, { reason: "e2e" }],
    ["/advanced/equities/corporate-actions", { instrument_id: ABSENT, type: "SPLIT", reason: "e2e" }],
  ] as const) {
    await expectCode(await command(page, path, body), 403, "FORBIDDEN");
  }
});

test("an order is confirmed once, and the page shows exactly what the backend answered", async ({ page }) => {
  const list = await listings(page);
  const listing = list?.items[0];
  test.skip(listing === undefined, "this deployment lists no equity to order");
  const l = listing as Listing;

  await page.goto(`/advanced/equities/listings/${l.instrument_id}`);
  await settle(page);
  await expect(page.getByRole("heading", { level: 1, name: l.symbol, exact: true })).toBeVisible();
  const panel = page.getByRole("region", { name: "Place an order", exact: true });
  const confirm = panel.getByRole("button", { name: `Confirm: buy ${l.symbol}`, exact: true });
  await expect(confirm).toBeDisabled();
  await expect(panel).toContainText("created when you confirm");

  await panel.getByLabel(`Shares of ${l.symbol}`, { exact: true }).fill(asDecimal(l.min_quantity, l.share_decimals));
  await expect(confirm).toBeEnabled();

  const answered: Promise<Response> = page.waitForResponse(
    (response) => response.request().method() === "POST" && new URL(response.url()).pathname === "/v1/advanced/equities/orders",
  );
  await confirm.click();
  const response = await answered;
  const sent = response.request().headers()["idempotency-key"] ?? "";
  expect(sent, "the command carried the key created at confirmation").toMatch(/^[0-9a-f-]{36}$/);
  await expect(panel).toContainText(sent);

  const answer: unknown = await response.json();
  if (response.ok()) {
    const result = answer as {
      value_domain: string;
      order: { id: string; status: string; fill?: { settlement_date: string; trade_date: string; settlement?: { state: string } } };
      placement: { status: string };
    };
    expect(result.value_domain).toBe("SIMULATED");
    expect(["PLACED", "NOT_PLACED"]).toContain(result.placement.status);
    await expect(panel.locator(".callout")).toContainText(result.order.id);
    if (result.order.fill !== undefined) {
      expect(result.order.fill.settlement_date > result.order.fill.trade_date, "a fill settles T+1").toBe(true);
      expect(result.order.fill.settlement?.state, "nothing settles on the trade date").toBe("PENDING");
      await expect(panel.locator(".callout")).toContainText(result.order.fill.settlement_date);
    }
  } else {
    expect(isProblem(answer)).toBe(true);
    const problem = answer as Problem;
    await expect(panel.locator(".explain-meta")).toContainText(`code ${problem.code}`);
    const orderId = problem.fields?.["order_id"];
    if (typeof orderId === "string") {
      // A refusal the backend recorded links to the order, which stays readable.
      await expect(panel.getByRole("link", { name: orderId })).toBeVisible();
      const recorded = await page.request.get(`/v1/advanced/equities/orders/${orderId}`);
      expect(recorded.status()).toBe(200);
      expect(((await recorded.json()) as { status: string }).status).toBe("REJECTED");
    }
  }
});
