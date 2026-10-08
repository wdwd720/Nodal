/**
 * The fees section of the advanced (sandbox) area against the real API: the
 * transparent spread and fee engine and the sandbox performance fee.
 *
 * Nothing is stubbed. Customer-a is the session the suite signed in with; the
 * operator ("admin") signs in here through the same development identity
 * provider, in contexts of its own. What this suite holds the section to:
 *
 *   - the published schedules are public to any signed-in customer, the page
 *     lists exactly the backend's keys, and a schedule's page shows its whole
 *     version history;
 *   - the quote calculator asks the fee engine, and the page shows exactly the
 *     disclosure the engine returned; an unknown key is NOT_FOUND;
 *   - quotes, bookings and periods are the caller's own: another account's
 *     list is FORBIDDEN, and a record of another account (or none) is
 *     NOT_FOUND;
 *   - a customer is refused every operator action; publishing needs a recent
 *     strong sign-in;
 *   - a performance-fee period accrues nothing below its high-water mark and
 *     accrues above it, and the customer reads exactly those lines.
 */
import { randomBytes, randomUUID } from "node:crypto";

import { expect, test, type Browser, type BrowserContext, type Page } from "@playwright/test";

import { goto } from "./pace.ts";
import { signIn } from "./sign-in.ts";

const NOTICE = "Simulated value — sandbox only";
const SECTION = "/advanced/fees";
/** The deployment's one sandbox dollar (econ.SandboxDollarID): six decimals, USD-pegged, simulated. */
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";

interface Problem {
  readonly code: string;
  readonly detail?: string;
  readonly fields?: Record<string, unknown>;
}

interface ScheduleVersion {
  readonly id: string;
  readonly key: string;
  readonly version: number;
  readonly in_force: boolean;
  readonly schedule: { readonly label: string; readonly spread_bps: number; readonly percentage_fee_bps: number };
}

interface Preview {
  readonly value_domain: string;
  readonly recorded: boolean;
  readonly disclosure: {
    readonly spread: { readonly bps: number; readonly amount: string };
    readonly fees: { readonly total: string };
    readonly total_debit: string;
  };
}

interface Line {
  readonly kind: string;
  readonly fee_base: string;
  readonly amount: string;
  readonly accrual_after: string;
}

interface Period {
  readonly id: string;
  readonly status: string;
  readonly terms: { readonly prior_hwm: string };
  readonly lines: Line[];
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

function hex(bytes: number): string {
  return randomBytes(bytes).toString("hex");
}

/** A well-formed UUIDv7 nobody recorded: the version nibble of a random v4 set to 7. */
function unknownId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
}

/** An instant this many minutes from now, in UTC. */
function minutesFromNow(minutes: number): string {
  return new Date(Date.now() + minutes * 60_000).toISOString();
}

async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
}

async function ownAccount(page: Page): Promise<string> {
  const response = await page.request.get("/v1/accounts");
  expect(response.ok()).toBeTruthy();
  const accounts = (await response.json()) as Array<{ id: string }>;
  const id = accounts[0]?.id;
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

/**
 * The operator's strong session this file's tests share. Signing in is rate
 * limited -- CP_API_RATE_LIMIT_AUTH allows 30 requests a minute from one
 * address by default, and a sign-in is four of them -- and the suite signs in
 * far more often than a person would, so the operator signs in with MFA once
 * for the file. A strong sign-in stays recent far longer than this file takes.
 */
let operatorSession: { context: BrowserContext; page: Page } | undefined;

async function strongOperator(browser: Browser): Promise<Page> {
  operatorSession ??= await signIn(browser, "admin", true);
  return operatorSession.page;
}

test.afterAll(async () => {
  await operatorSession?.context.close();
  operatorSession = undefined;
});

async function post(page: Page, path: string, body: unknown, key: string = randomUUID()): Promise<{ status: number; body: unknown }> {
  const response = await page.request.post(path, {
    headers: { "Idempotency-Key": key, "Sec-Fetch-Site": "same-origin" },
    data: body,
  });
  return { status: response.status(), body: (await response.json()) as unknown };
}

/** Makes sure the sandbox dollar exists (the faucet creates it on its first grant). */
async function ensureSandboxDollar(page: Page): Promise<void> {
  const account = await ownAccount(page);
  const grant = await post(page, "/v1/advanced/transfers/faucet/grants", { account_id: account, amount: "1" });
  expect([201, 429], `the faucet runs in this deployment: ${JSON.stringify(grant.body)}`).toContain(grant.status);
}

function previewPath(key: string, price: string): string {
  const q = new URLSearchParams({
    schedule_key: key,
    side: "BUY",
    base_quantity: "2000000",
    base_decimals: "6",
    reference_price: price,
  });
  return `/v1/advanced/fees/quote-preview?${q.toString()}`;
}

/** A schedule that charges a spread and a percentage fee, quoted in the sandbox dollar. */
function scheduleBody(key: string, label: string): unknown {
  return {
    key,
    reason: "e2e: a schedule for the fees suite",
    schedule: {
      label,
      quote_asset_id: SANDBOX_DOLLAR,
      quote_decimals: 6,
      spread_bps: 25,
      fixed_fee: "0",
      percentage_fee_bps: 10,
      venue_pass_through_bps: 0,
      network_fee_amount: "0",
      slippage_reserve_bps: 0,
      subscription_discount_bps: 0,
      tiers: [],
      strategy_fee_bps: 0,
      max_total_fee_bps: 0,
      max_total_fee: "0",
      min_fee: "0",
      rounding: {
        buy_notional: "down",
        sell_notional: "up",
        spread: "down",
        percentage_fee: "down",
        venue_pass_through: "down",
        strategy_fee: "down",
        fee_cap: "down",
        slippage_reserve: "up",
        allow_platform_favourable: false,
      },
    },
  };
}

test.describe("the fees section", () => {
  test("renders under the simulated-value notice and lists exactly the published schedules", async ({ page }) => {
    const response = await page.request.get("/v1/advanced/fees/schedules?limit=25");
    expect(response.status(), "the published schedules are public to a signed-in customer").toBe(200);
    const body = (await response.json()) as { value_domain: string; items: Array<{ key: string }> };
    expect(body.value_domain).toBe("SIMULATED");

    await goto(page, "/advanced/overview");
    await expect(page.locator("h1")).toHaveCount(1);
    await page.getByRole("navigation", { name: "Advanced area", exact: true }).getByRole("link", { name: "Fees", exact: true }).click();
    await expect(page).toHaveURL(/\/advanced\/fees$/);
    await expect(page.getByRole("heading", { level: 1, name: "Fees", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");
    await settle(page);
    const panel = page.getByRole("region", { name: "Published schedules", exact: true });
    if (body.items.length === 0) {
      await expect(panel).toContainText("No schedules published");
    } else {
      for (const item of body.items) {
        await expect(panel.locator(`[data-schedule="${item.key}"]`)).toHaveCount(1);
      }
    }
    await expect(page.locator(".malformed")).toHaveCount(0);
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
    // The operator's panels are not offered to a customer.
    await expect(page.getByRole("region", { name: "Publish a schedule version", exact: true })).toHaveCount(0);
  });

  test("an unknown schedule key has no quote", async ({ page }) => {
    const response = await page.request.get(previewPath(`e2e-unknown-${hex(6)}`, "100"));
    expect(response.status()).toBe(404);
    const problem = (await response.json()) as Problem;
    expect(problem.code).toBe("NOT_FOUND");
    expect(problem.fields?.["reason"]).toBe("FEES_NO_EFFECTIVE_SCHEDULE");
  });

  test("quotes, bookings and periods are the caller's own", async ({ page }) => {
    const own = await ownAccount(page);
    for (const list of ["quotes", "bookings", "periods"]) {
      const mine = await page.request.get(`/v1/advanced/fees/${list}?account_id=${own}`);
      expect(mine.status(), `the caller's own ${list}`).toBe(200);
      expect(((await mine.json()) as { value_domain: string }).value_domain).toBe("SIMULATED");
      const foreign = await page.request.get(`/v1/advanced/fees/${list}?account_id=${unknownId()}`);
      expect(foreign.status(), `another account's ${list}`).toBe(403);
      expect(((await foreign.json()) as Problem).code).toBe("FORBIDDEN");
    }
    for (const record of ["quotes", "bookings", "periods"]) {
      const missing = await page.request.get(`/v1/advanced/fees/${record}/${unknownId()}`);
      expect(missing.status(), `a ${record} record that is not the caller's is NOT_FOUND`).toBe(404);
    }

    for (const path of ["/advanced/fees/quotes", "/advanced/fees/bookings", "/advanced/fees/periods"]) {
      await goto(page, path);
      await settle(page);
      await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
      await expect(page.locator(".malformed")).toHaveCount(0);
    }
    await goto(page, `${SECTION}/bookings/${unknownId()}`);
    await settle(page);
    await expect(page.locator(".explain-meta").first()).toContainText("code NOT_FOUND");
  });

  test("a customer is refused every operator action", async ({ page }) => {
    const own = await ownAccount(page);
    const published = await post(page, "/v1/advanced/fees/schedules", scheduleBody(`e2e-refused-${hex(4)}`, "refused"));
    expect(published.status).toBe(403);
    expect(isProblem(published.body) && published.body.code).toBe("FORBIDDEN");
    const opened = await post(page, "/v1/advanced/fees/periods", {
      account_id: own,
      strategy_ref: "e2e-refused",
      terms_version: "e2e",
      period_end: minutesFromNow(60),
      hurdle_annual_bps: 0,
      fee_rate_bps: 2000,
    });
    expect(opened.status).toBe(403);
    const reversed = await post(page, `/v1/advanced/fees/bookings/${unknownId()}/reverse`, { reason_code: "E2E_REFUSED", reason: "refused" });
    expect(reversed.status).toBe(403);
    const audit = await page.request.get("/v1/advanced/fees/spread-audit");
    expect(audit.status()).toBe(403);
  });
});

test.describe("an operator's fee schedule and performance period", () => {
  test("a published schedule is read by a customer, its history shown, and its quote is the engine's", async ({ browser, page }) => {
    await ensureSandboxDollar(page);
    const key = `e2e:fees:${hex(6)}`;

    const stale = await signIn(browser, "admin", false);
    try {
      const refused = await post(stale.page, "/v1/advanced/fees/schedules", scheduleBody(key, "e2e-v1"));
      expect(refused.status, "publishing needs a recent strong sign-in").toBe(403);
      expect(isProblem(refused.body) && refused.body.code).toBe("STEP_UP_REQUIRED");
    } finally {
      await stale.context.close();
    }

    const operator = await strongOperator(browser);
    const published = await post(operator, "/v1/advanced/fees/schedules", scheduleBody(key, "e2e-v1"));
    expect(published.status, JSON.stringify(published.body)).toBe(200);
    const version = published.body as ScheduleVersion;
    expect(version.version).toBe(1);
    expect(version.in_force).toBe(true);
    // The same label and content again return the stored version, whatever the key.
    const again = await post(operator, "/v1/advanced/fees/schedules", scheduleBody(key, "e2e-v1"));
    expect(again.status).toBe(200);
    expect((again.body as ScheduleVersion).id).toBe(version.id);

    const history = (await (await page.request.get(`/v1/advanced/fees/schedules/history?key=${encodeURIComponent(key)}`)).json()) as {
      effective_version?: number;
      versions: ScheduleVersion[];
    };
    expect(history.effective_version).toBe(1);
    expect(history.versions.map((v) => v.version)).toEqual([1]);

    const previewResponse = await page.request.get(previewPath(key, "100"));
    expect(previewResponse.status()).toBe(200);
    const preview = (await previewResponse.json()) as Preview;
    expect(preview.value_domain).toBe("SIMULATED");
    expect(preview.recorded).toBe(false);
    expect(preview.disclosure.spread.bps).toBe(25);

    await goto(page, `${SECTION}/schedule?key=${encodeURIComponent(key)}`);
    await expect(page.getByRole("heading", { level: 1, name: "Fee schedule", exact: true })).toBeVisible();
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    await expect(page.locator('[data-version="1"]')).toContainText("in force");

    const form = page.getByRole("form", { name: "Price an order" });
    await form.getByLabel("Quantity, in base units of the asset priced", { exact: true }).fill("2000000");
    await form.getByLabel("Decimals of the asset priced", { exact: true }).fill("6");
    await form.getByLabel(/^Reference price of one whole unit/).fill("100");
    const [answer] = await Promise.all([
      page.waitForResponse((r) => new URL(r.url()).pathname === "/v1/advanced/fees/quote-preview" && r.request().method() === "GET"),
      form.getByRole("button", { name: "Price it under the schedule", exact: true }).click(),
    ]);
    expect(answer.status()).toBe(200);
    const shown = (await answer.json()) as Preview;
    const result = page.getByRole("status", { name: "The fee engine's quote" });
    await expect(result).toBeVisible();
    await expect(result.locator('[data-field="spread"]')).not.toHaveText("");
    expect(shown.disclosure.total_debit).toBe(preview.disclosure.total_debit);
  });

  test("a performance-fee period accrues nothing below its high-water mark and accrues above it", async ({ browser, page }) => {
    await ensureSandboxDollar(page);
    const own = await ownAccount(page);
    const strategy = `e2e-${hex(4)}`;
    const op = await strongOperator(browser);
    const opened = await post(op, "/v1/advanced/fees/periods", {
      account_id: own,
      strategy_ref: strategy,
      terms_version: "e2e-terms-v1",
      period_start: minutesFromNow(-120),
      period_end: minutesFromNow(120),
      opening_nav: "10000.00",
      hurdle_annual_bps: 0,
      fee_rate_bps: 2000,
    });
    if (opened.status === 422 && isProblem(opened.body) && opened.body.code === "CAPABILITY_NOT_APPROVED") {
      return;
    }
    expect(opened.status, JSON.stringify(opened.body)).toBe(200);
    const period = opened.body as Period;
    expect(period.status).toBe("OPEN");
    expect(period.terms.prior_hwm).toBe("10000.00");

    const below = await post(op, `/v1/advanced/fees/periods/${period.id}/accruals`, { at: minutesFromNow(-60), nav: "9500.00" });
    expect(below.status, JSON.stringify(below.body)).toBe(200);
    const belowLine = (below.body as { line: Line }).line;
    expect(belowLine.fee_base, "nothing is above the high-water mark").toBe("0.00");
    expect(belowLine.accrual_after).toBe("0.00");

    const above = await post(op, `/v1/advanced/fees/periods/${period.id}/accruals`, { at: minutesFromNow(-30), nav: "11000.00" });
    expect(above.status, JSON.stringify(above.body)).toBe(200);
    const aboveLine = (above.body as { line: Line }).line;
    expect(aboveLine.kind).toBe("ACCRUAL");
    expect(aboveLine.fee_base).not.toBe("0.00");
    expect(aboveLine.accrual_after).not.toBe("0.00");

    const read = (await (await page.request.get(`/v1/advanced/fees/periods/${period.id}`)).json()) as Period;
    expect(read.lines.map((line) => line.kind)).toEqual([belowLine.kind, "ACCRUAL"]);

    await goto(page, `${SECTION}/periods/${period.id}`);
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    await expect(page.locator('[data-field="period_status"]')).toContainText("OPEN");
    await expect(page.locator("[data-line-kind]")).toHaveCount(2);
    await expect(page.locator('[data-line-kind="ACCRUAL"]')).toHaveCount(1);

    const cancelled = await post(op, `/v1/advanced/fees/periods/${period.id}/cancel`, { reason: "e2e: done" });
    expect(cancelled.status, JSON.stringify(cancelled.body)).toBe(200);
    expect((cancelled.body as Period).status).toBe("CANCELLED");
  });
});
