/**
 * The margin section of the advanced area against the real API.
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database,
 * signed in as customer-a, and every assertion compares what a page shows
 * with what the backend answered — including where it answers no. What this
 * suite holds the section to:
 *
 *   - the margin account renders the engine's evaluation figure for figure,
 *     or the backend's answer that there is none, or its refusal with the
 *     code; and no liquidation price is ever estimated, because the engine
 *     reports none;
 *   - a command's idempotency key is created when the user confirms, and a
 *     retry of the same request sends the same key and gets the same answer;
 *   - the operator's pages are refused to a customer, and the refusal is shown
 *     with its code rather than hidden;
 *   - no money-shaped figure appears that the API did not send, and no
 *     control is dead.
 */
import { randomUUID } from "node:crypto";

import { expect, test, type Browser, type BrowserContext, type Page, type Request, type Response } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";

const MARGIN_PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/advanced/margin", heading: "Margin" },
  { path: "/advanced/margin/history", heading: "Margin calls and liquidation cases" },
  { path: "/advanced/margin/stress", heading: "Margin stress tests" },
  { path: "/advanced/margin/operator", heading: "Margin operations" },
];

/** Every cent figure of the evaluation, each rendered with data-field. */
const EVALUATION_USD_FIELDS = [
  "equity",
  "initial_margin",
  "maintenance_margin",
  "liquidation_buffer",
  "margin_available",
  "warning_threshold",
  "eligible_collateral",
  "market_value",
  "gross_exposure",
  "borrow_liabilities",
  "accrued_interest",
] as const;

interface Problem {
  readonly code: string;
  readonly detail?: string;
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

interface MarginAccountBody {
  readonly status: string;
  readonly margin_mode: string;
  readonly evaluation: {
    readonly health: string;
    readonly usd: Readonly<Record<string, string>>;
  };
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

/** Waits until a page has its answer on screen and nothing says it is still loading. */
async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
}

/** A wire USD string as the Money components render it. */
function renderedUsd(wire: string): string {
  const negative = wire.startsWith("-");
  const [whole = "", cents = ""] = (negative ? wire.slice(1) : wire).split(".");
  const digits = whole.replace(/^0+(?=[0-9])/, "");
  const grouped = digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const zero = /^0+$/.test(digits) && /^0+$/.test(cents);
  return `${negative && !zero ? "-" : ""}$${grouped}.${cents}`;
}

const USD_WIRE = /^-?[0-9]+\.[0-9]{2}$/;

function collectUsd(value: unknown, into: Set<string>): void {
  if (typeof value === "string") {
    if (USD_WIRE.test(value)) into.add(value);
  } else if (Array.isArray(value)) {
    for (const item of value) collectUsd(item, into);
  } else if (typeof value === "object" && value !== null) {
    for (const item of Object.values(value)) collectUsd(item, into);
  }
}

/** Navigates and keeps the JSON body of every /v1/advanced/ response the page itself received. */
async function visit(page: Page, path: string): Promise<Map<string, unknown>> {
  const pending: Array<Promise<readonly [string, unknown]>> = [];
  const listener = (response: Response): void => {
    const url = new URL(response.url());
    if (!url.pathname.startsWith("/v1/advanced/")) return;
    pending.push(
      response.json().then(
        (body: unknown) => [url.pathname, body] as const,
        () => [url.pathname, undefined] as const,
      ),
    );
  };
  page.on("response", listener);
  await page.goto(path);
  await settle(page);
  page.off("response", listener);
  return new Map(await Promise.all(pending));
}

/**
 * What the page says keeps the interface's vocabulary even where the words
 * are the backend's own, as a refusal's detail is (e2e/honesty.spec.ts).
 */
async function expectHonestWords(page: Page): Promise<void> {
  const text = await page.locator("main").innerText();
  expect(text, "the page never uses the banned word for a stablecoin, not even in a refusal").not.toMatch(/\bcash\b/i);
}

/** The idempotency key a command request carried. */
function keyOf(request: Request): string {
  const key = request.headers()["idempotency-key"];
  expect(key, "every command carries an Idempotency-Key").toBeTruthy();
  return key as string;
}

test.describe("the margin section", () => {
  for (const { path, heading } of MARGIN_PAGES) {
    test(`${heading} renders under the simulated-value notice`, async ({ page }) => {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1, name: heading, exact: true })).toBeVisible();
      const notice = page.getByRole("note", { name: NOTICE, exact: true });
      await expect(notice).toBeVisible();
      await expect(notice).toContainText("no economic substance");
      await settle(page);
      await expect(page.locator(".malformed")).toHaveCount(0);
      await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
    });
  }

  test("the section is reachable from the advanced area's navigation", async ({ page }) => {
    await page.goto("/advanced/overview");
    await expect(page.locator("h1")).toHaveCount(1);
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Margin", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Margin", exact: true })).toBeVisible();
    const pages = page.getByRole("navigation", { name: "Margin pages", exact: true });
    await pages.getByRole("link", { name: "Calls and cases", exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Margin calls and liquidation cases", exact: true })).toBeVisible();
  });
});

test("the margin account is the engine's evaluation figure for figure, its absence, or its refusal", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/advanced/margin/account?account_id=${id}`);
  const body: unknown = await response.json();

  await page.goto("/advanced/margin");
  await settle(page);

  if (response.ok()) {
    const account = body as MarginAccountBody;
    for (const field of EVALUATION_USD_FIELDS) {
      const wire = account.evaluation.usd[field];
      expect(typeof wire, `${field} is a decimal string`).toBe("string");
      await expect(page.locator(`[data-field="${field}"]`)).toHaveText(renderedUsd(wire as string));
    }
    await expect(page.locator('[data-field="status"]')).toHaveText(account.status);
    await expect(page.locator('[data-field="health"]')).toHaveText(account.evaluation.health);
    // The engine reports no liquidation price, so none is drawn.
    await expect(page.locator('[data-field="liquidation_price"]')).toHaveText("not reported by the engine");
    return;
  }

  expect(isProblem(body), "a refusal is a problem document with a stable code").toBe(true);
  const problem = body as Problem;
  if (problem.code === "NOT_FOUND") {
    await expect(page.getByText("No margin account", { exact: true })).toBeVisible();
    await expect(page.getByRole("region", { name: "Open a margin account", exact: true })).toBeVisible();
    // Nothing money-shaped stands in for an account that does not exist.
    const text = await page.locator("main").innerText();
    expect(text).not.toMatch(/\$\s?[0-9]/);
    return;
  }
  await expect(page.locator(".explain-meta").first()).toContainText(`code ${problem.code}`);
});

test("opening a margin account: the key is created at confirmation and a retry reuses it", async ({ page }) => {
  const id = await accountId(page);
  const before = await page.request.get(`/v1/advanced/margin/account?account_id=${id}`);
  await page.goto("/advanced/margin");
  await settle(page);

  if (before.status() !== 404) {
    // The account already has a margin account (or the read was refused):
    // there is nothing to open, and no open form is offered.
    await expect(page.getByRole("region", { name: "Open a margin account", exact: true })).toHaveCount(0);
    return;
  }

  const form = page.getByRole("region", { name: "Open a margin account", exact: true });
  await form.getByLabel("Margin mode").selectOption("CROSS");
  await form.getByLabel("Run mode").selectOption("PAPER");

  // Reviewing sends nothing: the key does not exist until the user confirms.
  let sent = 0;
  const counter = (request: Request): void => {
    if (request.method() === "POST" && new URL(request.url()).pathname === "/v1/advanced/margin/account") sent = sent + 1;
  };
  page.on("request", counter);
  await form.getByRole("button", { name: "Review: Open the margin account", exact: true }).click();
  await expect(form.getByText("Review what will be sent", { exact: true })).toBeVisible();
  expect(sent, "reviewing sends no request").toBe(0);

  const firstRequest = page.waitForRequest(
    (request) => request.method() === "POST" && new URL(request.url()).pathname === "/v1/advanced/margin/account",
  );
  const firstResponse = page.waitForResponse(
    (response) => response.request().method() === "POST" && new URL(response.url()).pathname === "/v1/advanced/margin/account",
  );
  await form.getByRole("button", { name: "Confirm: Open the margin account", exact: true }).click();
  const key = keyOf(await firstRequest);
  const answer = await firstResponse;
  await expect(form.getByText(`idempotency key ${key}`, { exact: true })).toBeVisible();

  if (answer.ok()) {
    // Opened: the page reads the account again and now shows the margin
    // account the backend answered with, in place of the open form.
    const opened = (await answer.json()) as { id: string; status: string };
    await expect(page.getByText(`margin account ${opened.id}`, { exact: true })).toBeVisible();
    await expect(page.locator('[data-field="status"]')).toHaveText(opened.status);
    expect(sent, "confirming sent exactly one request").toBe(1);
    page.off("request", counter);
    return;
  }

  // The backend refused (for instance UNSUPPORTED while the sandbox dollar is
  // not provisioned). The refusal is shown with its code, and trying again
  // sends the same request with the same key, which the backend answers the
  // same way.
  const problem = (await answer.json()) as Problem;
  await expect(form.locator(".explain-meta")).toContainText(`code ${problem.code}`);
  const retryRequest = page.waitForRequest(
    (request) => request.method() === "POST" && new URL(request.url()).pathname === "/v1/advanced/margin/account",
  );
  const retryResponse = page.waitForResponse(
    (response) => response.request().method() === "POST" && new URL(response.url()).pathname === "/v1/advanced/margin/account",
  );
  await form.getByRole("button", { name: "Try again with the same key", exact: true }).click();
  expect(keyOf(await retryRequest), "a retry carries the key created at confirmation").toBe(key);
  const retried = await retryResponse;
  expect(retried.status()).toBe(answer.status());
  expect(((await retried.json()) as Problem).code).toBe(problem.code);
  await expect(form.locator(".explain-meta")).toContainText(`code ${problem.code}`);
  await expectHonestWords(page);
  expect(sent, "exactly two requests were sent: the confirmation and the retry").toBe(2);
  page.off("request", counter);
});

test("a borrow the backend refuses is shown with its code, and retried with the same key", async ({ page }) => {
  const id = await accountId(page);
  const read = await page.request.get(`/v1/advanced/margin/account?account_id=${id}`);
  await page.goto("/advanced/margin");
  await settle(page);
  if (!read.ok()) {
    // No margin account to borrow against: the page offers no borrow form.
    await expect(page.getByLabel(/Amount to borrow/)).toHaveCount(0);
    return;
  }
  const account = (await read.json()) as MarginAccountBody;
  if (account.margin_mode !== "CROSS" && account.margin_mode !== "ISOLATED") {
    await expect(page.getByLabel(/Amount to borrow/)).toHaveCount(0);
    return;
  }

  // An amount no lending pool in a sandbox holds: whatever the backend
  // decides about it, the page shows exactly that.
  await page.getByLabel(/Amount to borrow/).fill("999999999999");
  await page.getByRole("button", { name: "Review: Borrow", exact: true }).click();
  const request = page.waitForRequest(
    (candidate) => candidate.method() === "POST" && new URL(candidate.url()).pathname === "/v1/advanced/margin/borrow",
  );
  const response = page.waitForResponse(
    (candidate) => candidate.request().method() === "POST" && new URL(candidate.url()).pathname === "/v1/advanced/margin/borrow",
  );
  await page.getByRole("button", { name: "Confirm: Borrow", exact: true }).click();
  const key = keyOf(await request);
  const answer = await response;
  const body: unknown = await answer.json();
  if (answer.ok()) {
    // Not expected for this amount, but if the backend lent it, the page says so.
    await expect(page.getByText("idempotency key " + key, { exact: true })).toBeVisible();
    return;
  }
  expect(isProblem(body)).toBe(true);
  const problem = body as Problem;
  await expect(page.locator(".explain-meta").filter({ hasText: `code ${problem.code}` })).toBeVisible();

  // A refusal is a conclusion the backend recorded for this key: the retry
  // carries the same key and is answered with the same refusal.
  const retry = page.waitForRequest(
    (candidate) => candidate.method() === "POST" && new URL(candidate.url()).pathname === "/v1/advanced/margin/borrow",
  );
  const retryResponse = page.waitForResponse(
    (candidate) => candidate.request().method() === "POST" && new URL(candidate.url()).pathname === "/v1/advanced/margin/borrow",
  );
  await page.getByRole("button", { name: "Try again with the same key", exact: true }).click();
  expect(keyOf(await retry)).toBe(key);
  const retried = await retryResponse;
  expect(retried.status()).toBe(answer.status());
  expect(((await retried.json()) as Problem).code).toBe(problem.code);
  await expect(page.locator(".explain-meta").filter({ hasText: `code ${problem.code}` })).toBeVisible();
  await expectHonestWords(page);
});

test("the operator's pages are refused to a customer, with the code on screen", async ({ page }) => {
  const queue = await page.request.get("/v1/advanced/margin/ops/queue");
  const calls = await page.request.get("/v1/advanced/margin/ops/calls");
  expect(queue.status(), "a customer holds no sandbox:operate").toBe(403);
  expect(calls.status()).toBe(403);
  const queueProblem = (await queue.json()) as Problem;
  const callsProblem = (await calls.json()) as Problem;

  await page.goto("/advanced/margin/operator");
  await settle(page);
  await expect(
    page.getByRole("region", { name: "Liquidation queue", exact: true }).locator(".explain-meta"),
  ).toContainText(`code ${queueProblem.code}`);
  await expect(
    page.getByRole("region", { name: "Margin calls across accounts", exact: true }).locator(".explain-meta"),
  ).toContainText(`code ${callsProblem.code}`);
});

test("calls and cases show exactly the rows the API returned", async ({ page }) => {
  const bodies = await visit(page, "/advanced/margin/history");
  for (const { path, panel, empty } of [
    { path: "/v1/advanced/margin/calls", panel: "Margin calls", empty: "No margin calls" },
    { path: "/v1/advanced/margin/cases", panel: "Liquidation cases", empty: "No liquidation cases" },
  ]) {
    const body = bodies.get(path);
    expect(body, `the page asked for ${path}`).toBeTruthy();
    const region = page.getByRole("region", { name: panel, exact: true });
    if (isProblem(body)) {
      await expect(region.locator(".explain-meta")).toContainText(`code ${body.code}`);
      continue;
    }
    const rows = body as { value_domain: string; items: Array<{ id: string }> };
    expect(rows.value_domain).toBe("SIMULATED");
    if (rows.items.length === 0) {
      await expect(region).toContainText(empty);
      continue;
    }
    for (const item of rows.items) {
      await expect(region.getByText(item.id, { exact: true }).first()).toBeVisible();
    }
  }
});

test("an unknown liquidation case is answered NOT_FOUND", async ({ page }) => {
  const v4 = randomUUID();
  const unknown = `${v4.slice(0, 14)}7${v4.slice(15)}`;
  const response = await page.request.get(`/v1/advanced/margin/cases/${unknown}`);
  expect(response.status()).toBe(404);
  await page.goto(`/advanced/margin/cases/${unknown}`);
  await expect(page.getByRole("heading", { level: 1, name: "Liquidation case", exact: true })).toBeVisible();
  await settle(page);
  await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
  await expect(page.locator(".explain-meta")).toContainText("code NOT_FOUND");
});

for (const { path } of MARGIN_PAGES) {
  test(`${path} shows no money-shaped figure the API did not return`, async ({ page }) => {
    const bodies = await visit(page, path);
    const wire = new Set<string>();
    for (const body of bodies.values()) collectUsd(body, wire);
    const allowed = new Set([...wire].map(renderedUsd));
    const text = await page.locator("main").innerText();
    for (const figure of text.match(/-?\$[0-9][0-9,]*\.[0-9]{2}/g) ?? []) {
      expect(allowed.has(figure), `${path} shows ${figure}, which no advanced response contained`).toBe(true);
    }
  });
}

test("no dead controls in the margin section", async ({ page }) => {
  for (const { path } of MARGIN_PAGES) {
    await page.goto(path);
    await settle(page);
    const buttons = page.locator("main button");
    const count = await buttons.count();
    for (let i = 0; i < count; i = i + 1) {
      const button = buttons.nth(i);
      if (!(await button.isDisabled())) continue;
      const describedBy = await button.getAttribute("aria-describedby");
      expect(describedBy, `${path}: a disabled button explains itself`).toBeTruthy();
      const reason = page.locator(`[id="${(describedBy as string).replace(/"/g, '\\"')}"]`);
      await expect(reason).toBeVisible();
      expect(((await reason.textContent()) ?? "").trim().length).toBeGreaterThan(10);
    }
    const links = page.locator("main a[href]");
    const linkCount = await links.count();
    for (let i = 0; i < linkCount; i = i + 1) {
      const href = (await links.nth(i).getAttribute("href")) ?? "";
      expect(href.trim() !== "" && href !== "#", `${path}: link ${String(i)} goes somewhere`).toBe(true);
    }
  }
});

test("the margin pages reflow at 390px without sideways scrolling", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  for (const { path } of MARGIN_PAGES) {
    await page.goto(path);
    await settle(page);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
  }
});

/* --------------------------------------------------------------------------
 * The covered deficit, and leaving MANUAL_REVIEW under dual control
 * ------------------------------------------------------------------------ */

/** A canonical id nobody holds: a random UUID stamped as version 7. */
function unknownId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
}

/** A command as the browser would send it: same-origin, JSON, with a fresh idempotency key. */
async function command(page: Page, path: string, body: unknown): Promise<{ readonly status: number; readonly body: unknown }> {
  const response = await page.request.post(path, {
    headers: { "Content-Type": "application/json", "Idempotency-Key": randomUUID(), "Sec-Fetch-Site": "same-origin" },
    data: JSON.stringify(body),
  });
  return { status: response.status(), body: (await response.json().catch(() => undefined)) as unknown };
}

/** Signs a development identity in, in a fresh context, through the real OIDC flow. */
async function signIn(browser: Browser, identity: string, mfa: boolean): Promise<{ context: BrowserContext; page: Page }> {
  const context = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  await page.goto("/v1/auth/login");
  await expect(page.getByRole("heading", { name: "Choose an identity" })).toBeVisible();
  const row = page.locator("li", { has: page.locator("code", { hasText: new RegExp(`^${identity}$`) }) });
  await row.getByRole("link", { name: mfa ? "sign in with MFA" : "sign in", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Home", level: 1 })).toBeVisible();
  return { context, page };
}

interface DeficitBody {
  readonly value_domain: string;
  readonly margin_account_id?: string;
  readonly deficit?: string;
  readonly outstanding?: string;
  readonly repayments: ReadonlyArray<{ readonly id: string }>;
}

test("the covered deficit is the backend's answer, and a repayment it cannot take is refused with its code", async ({ page }) => {
  const id = await accountId(page);
  const read = await page.request.get(`/v1/advanced/margin/deficit?account_id=${id}`);
  expect(read.status()).toBe(200);
  const deficit = (await read.json()) as DeficitBody;
  expect(deficit.value_domain).toBe("SIMULATED");
  if (deficit.margin_account_id === undefined) {
    // No margin account: nothing is owed, nothing is shown as owed, and nothing can be repaid.
    expect(deficit.deficit).toBeUndefined();
    expect(deficit.repayments).toEqual([]);
    const refused = await command(page, "/v1/advanced/margin/deficit/repayments", { account_id: id, amount: "1000000" });
    expect([404, 409]).toContain(refused.status);
    return;
  }
  await page.goto("/advanced/margin");
  await settle(page);
  const panel = page.getByRole("region", { name: "Covered deficit", exact: true });
  await expect(panel).toBeVisible();
  if (deficit.deficit === undefined) {
    await expect(panel).toContainText("No covered deficit");
    await expect(panel.getByRole("form", { name: "Repay the covered deficit" })).toHaveCount(0);
    // Repaying a deficit the account does not owe is refused, and nothing moves.
    const refused = await command(page, "/v1/advanced/margin/deficit/repayments", { account_id: id, amount: "1000000" });
    expect(refused.status).toBe(409);
    expect((refused.body as Problem).code).toBe("INVALID_STATE_TRANSITION");
    return;
  }
  // A deficit is owed: the page shows the backend's figures, and more than is
  // still owed is refused.
  await expect(panel.locator('[data-field="deficit-outstanding"]')).toBeVisible();
  for (const repayment of deficit.repayments) {
    await expect(panel.locator(`[data-repayment="${repayment.id}"]`)).toBeVisible();
  }
  const beyond = (BigInt(deficit.outstanding ?? "0") + 1n).toString();
  const refused = await command(page, "/v1/advanced/margin/deficit/repayments", { account_id: id, amount: beyond });
  expect(refused.status).toBe(400);
  expect((refused.body as Problem).code).toBe("VALIDATION_FAILED");
});

test("a customer is refused the review queue and every resolution command, with the code on screen", async ({ page }) => {
  const reviews = await page.request.get("/v1/advanced/margin/ops/reviews");
  expect(reviews.status(), "a customer holds no sandbox:operate").toBe(403);
  const propose = await command(page, `/v1/advanced/margin/ops/cases/${unknownId()}/resolutions`, {
    outcome: "RESUME",
    reason: "a customer may not propose",
  });
  expect(propose.status).toBe(403);
  const decide = await command(page, `/v1/advanced/margin/ops/resolutions/${unknownId()}/decision`, {
    decision: "APPROVED",
    reason: "a customer may not decide",
  });
  expect(decide.status).toBe(403);
  await page.goto("/advanced/margin/operator");
  await settle(page);
  await expect(
    page.getByRole("region", { name: "Manual review queue", exact: true }).locator(".explain-meta"),
  ).toContainText(`code ${((await reviews.json()) as Problem).code}`);
});

test("an operator reads the review queue, and a resolution for a case that is not in review is refused", async ({
  page,
  browser,
}) => {
  const customerAccount = await accountId(page);
  const admin = await signIn(browser, "admin", true);
  const queue = await admin.page.request.get("/v1/advanced/margin/ops/reviews");
  expect(queue.status()).toBe(200);
  const body = (await queue.json()) as { value_domain: string; items: Array<{ case: { id: string } }> };
  expect(body.value_domain).toBe("SIMULATED");
  await admin.page.goto("/advanced/margin/operator");
  await settle(admin.page);
  const region = admin.page.getByRole("region", { name: "Manual review queue", exact: true });
  if (body.items.length === 0) {
    await expect(region).toContainText("No case in manual review");
  } else {
    for (const item of body.items) {
      await expect(region.locator(`[data-review-case="${item.case.id}"]`)).toBeVisible();
    }
  }

  // An unknown case, and a case that is not in review, are refused.
  const unknown = await command(admin.page, `/v1/advanced/margin/ops/cases/${unknownId()}/resolutions`, {
    outcome: "RESUME",
    reason: "end-to-end check of a refused proposal",
  });
  expect(unknown.status).toBe(404);
  const cases = await admin.page.request.get(`/v1/advanced/margin/cases?account_id=${customerAccount}&limit=200`);
  if (cases.ok()) {
    const notInReview = ((await cases.json()) as { items: Array<{ id: string; status: string }> }).items.find(
      (item) => item.status !== "MANUAL_REVIEW",
    );
    if (notInReview !== undefined) {
      const refused = await command(admin.page, `/v1/advanced/margin/ops/cases/${notInReview.id}/resolutions`, {
        outcome: "RESUME",
        reason: "end-to-end check of a case that is not in review",
      });
      expect(refused.status).toBe(409);
      expect((refused.body as Problem).code).toBe("INVALID_STATE_TRANSITION");
    }
  }
  const undecided = await command(admin.page, `/v1/advanced/margin/ops/resolutions/${unknownId()}/decision`, {
    decision: "REJECTED",
    reason: "end-to-end check of an unknown resolution",
  });
  expect(undecided.status).toBe(404);
  // A decision outside the declared set never reaches the service.
  const malformed = await command(admin.page, `/v1/advanced/margin/ops/resolutions/${unknownId()}/decision`, {
    decision: "MAYBE",
    reason: "end-to-end check of a malformed decision",
  });
  expect(malformed.status).toBe(400);
  await admin.context.close();
});
