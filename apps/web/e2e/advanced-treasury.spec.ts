/**
 * The treasury section of the advanced (sandbox) area against the real API:
 * the house books, which trade only simulated house capital.
 *
 * Nothing is stubbed. Customer-a is the session the suite signed in with; the
 * operator ("admin") signs in here through the same development identity
 * provider, with and without a strong sign-in, in contexts of its own. What
 * this suite holds the section to:
 *
 *   - every house-book route is the operator's: a customer is refused by the
 *     backend, and the page shows that refusal with its code;
 *   - what customers are told (the crossing disclosures) and the trades in
 *     which the house was their counterparty are theirs to read, and another
 *     account's are not;
 *   - an operator registers a house book, is refused a fill while the book
 *     has no limits (the refusal carries the treasury's stable reason and the
 *     action that recorded it), records limits, funds the book, and the
 *     report, balances and reconciliation read back exactly what was done;
 *   - anything that moves value or changes a policy needs a recent strong
 *     sign-in, and a cancel does not;
 *   - a command replays under its key, and the page creates the key when the
 *     operator confirms.
 */
import { randomBytes, randomUUID } from "node:crypto";

import { expect, test, type Browser, type BrowserContext, type Locator, type Page } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";
const SECTION = "/advanced/treasury";
const BOOKS = "/v1/advanced/treasury/books";

interface Problem {
  readonly code: string;
  readonly detail?: string;
  readonly fields?: Record<string, unknown>;
}

interface AssetRef {
  readonly asset_id: string;
  readonly symbol: string;
  readonly decimals: number;
}

interface BookDetail {
  readonly value_domain: string;
  readonly house_account_id: string;
  readonly name: string;
  readonly strategy: string;
  readonly class: string;
  readonly status: string;
  readonly valuation_asset: AssetRef;
  readonly signing_policy: { readonly version: number; readonly operators_allowed: boolean; readonly key_custody: string };
  readonly risk_policy?: { readonly version: number; readonly label: string };
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

/** Waits until a page has its answer on screen (see advanced-core.spec.ts). */
async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
}

/** The account the application reads for a session: the first the backend lists. */
async function ownAccount(page: Page): Promise<string> {
  const response = await page.request.get("/v1/accounts");
  expect(response.ok()).toBeTruthy();
  const accounts = (await response.json()) as Array<{ id: string }>;
  const id = accounts[0]?.id;
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
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

/** A command as the browser would send it: same-origin, with an idempotency key. */
async function post(page: Page, path: string, body: unknown, key: string = randomUUID()): Promise<{ status: number; body: unknown }> {
  const response = await page.request.post(path, {
    headers: { "Idempotency-Key": key, "Sec-Fetch-Site": "same-origin" },
    ...(body === undefined ? {} : { data: body }),
  });
  return { status: response.status(), body: (await response.json()) as unknown };
}

async function expectRefusal(region: Locator, code: string): Promise<void> {
  await expect(region.locator(".explain-meta").first()).toContainText(`code ${code}`);
}

/**
 * Makes sure the deployment's one sandbox dollar exists: the faucet creates it
 * on its first grant. A grant beyond the faucet's rolling cap is RATE_LIMITED,
 * which also means the dollar exists.
 */
async function ensureSandboxDollar(page: Page): Promise<void> {
  const account = await ownAccount(page);
  const grant = await post(page, "/v1/advanced/transfers/faucet/grants", { account_id: account, amount: "1" });
  expect([201, 429], `the faucet runs in this deployment: ${JSON.stringify(grant.body)}`).toContain(grant.status);
}

test.describe("the treasury section", () => {
  test("renders under the simulated-value notice, is reachable from the area, and shows a customer the backend's refusal", async ({ page }) => {
    await page.goto("/advanced/overview");
    await expect(page.locator("h1")).toHaveCount(1);
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Treasury", exact: true })
      .click();
    await expect(page).toHaveURL(/\/advanced\/treasury$/);
    await expect(page.getByRole("heading", { level: 1, name: "Treasury", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");
    await settle(page);
    // The house books are the operator's: the customer sees the refusal, with its code.
    await expectRefusal(page.getByRole("region", { name: "House books", exact: true }), "FORBIDDEN");
    await expect(page.locator(".malformed")).toHaveCount(0);
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);

    await page.getByRole("navigation", { name: "Treasury pages", exact: true }).getByRole("link", { name: "Crossing rules", exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: "House crossing rules", exact: true })).toBeVisible();
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
  });

  test("a customer is refused every house-book route", async ({ page }) => {
    const list = await page.request.get(BOOKS);
    expect(list.status()).toBe(403);
    expect(((await list.json()) as Problem).code).toBe("FORBIDDEN");

    const book = unknownId();
    for (const path of [`${BOOKS}/${book}`, `${BOOKS}/${book}/report`, `${BOOKS}/${book}/balances`, `${BOOKS}/${book}/actions`]) {
      const read = await page.request.get(path);
      expect(read.status(), `${path} is the operator's`).toBe(403);
    }
    const registered = await post(page, BOOKS, {
      name: `refused ${hex(4)}`,
      strategy: "MARKET_MAKING",
      service_actor_id: "e2e-house-service",
      operators_allowed: true,
      reason: "a customer may not register a house book",
    });
    expect(registered.status).toBe(403);
    expect(isProblem(registered.body) && registered.body.code).toBe("FORBIDDEN");
    for (const action of ["fundings", "orders", "hedges", "limits", "actor-policy", "sync", "mark-to-market"]) {
      const refused = await post(page, `${BOOKS}/${book}/${action}`, {});
      expect(refused.status, `${action} is refused to a customer`).toBe(403);
    }
    const crossing = await post(page, "/v1/advanced/treasury/crossing-rules", {
      instrument_id: unknownId(),
      allow: true,
      disclosure: "A house book may take the other side of your simulated order here.",
      reason: "a customer may not change a crossing rule",
    });
    expect(crossing.status).toBe(403);
  });

  test("a customer reads what customers are told and the house trades against their own account, and no one else's", async ({ page }) => {
    const disclosures = await page.request.get(`/v1/advanced/treasury/crossing-disclosures?instrument_id=${unknownId()}`);
    expect(disclosures.status(), "what customers are told is theirs to read").not.toBe(403);
    if (disclosures.status() === 200) {
      const body = (await disclosures.json()) as { value_domain: string; items: unknown[] };
      expect(body.value_domain).toBe("SIMULATED");
      expect(Array.isArray(body.items)).toBe(true);
    }

    const own = await ownAccount(page);
    const trades = await page.request.get(`/v1/advanced/treasury/house-trades?account_id=${own}`);
    expect(trades.status()).toBe(200);
    const body = (await trades.json()) as { value_domain: string; items: unknown[]; next_cursor: string | null };
    expect(body.value_domain).toBe("SIMULATED");
    const foreign = await page.request.get(`/v1/advanced/treasury/house-trades?account_id=${unknownId()}`);
    expect(foreign.status()).toBe(403);
    expect(((await foreign.json()) as Problem).code).toBe("FORBIDDEN");

    await page.goto("/advanced/fees/house-trades");
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    const panel = page.getByRole("region", { name: "House trades against this account — simulated", exact: true });
    if (body.items.length === 0) {
      await expect(panel).toContainText("No house trades");
    } else {
      await expect(panel.getByRole("table", { name: "House trades", exact: true }).locator("tbody tr")).toHaveCount(body.items.length);
    }
  });
});

test.describe("an operator's house book", () => {
  test("is registered, refused a fill without limits, given limits and funded, and reads back exactly that", async ({ browser, page }) => {
    await ensureSandboxDollar(page);
    const operator = await signIn(browser, "admin", true);
    try {
      const op = operator.page;
      const name = `e2e house ${hex(6)}`;
      const registered = await post(op, BOOKS, {
        name,
        strategy: "MARKET_MAKING",
        service_actor_id: "e2e-house-service",
        operators_allowed: true,
        reason: "e2e: a house book for the treasury suite",
      });
      expect(registered.status, JSON.stringify(registered.body)).toBe(200);
      const book = registered.body as BookDetail;
      expect(book.value_domain).toBe("SIMULATED");
      expect(book.name).toBe(name);
      expect(book.class).toBe("PRINCIPAL");
      expect(book.signing_policy.operators_allowed).toBe(true);
      expect(book.signing_policy.key_custody).toBe("NONE_SANDBOX");
      expect(book.risk_policy, "a new book has no limits yet").toBeUndefined();
      const id = book.house_account_id;

      // Without limits every fill is refused: the treasury records the refusal
      // and answers with its stable reason and the action that recorded it.
      const hedge = await post(op, `${BOOKS}/${id}/hedges`, { asset_id: unknownId(), side: "BUY", quantity: "1000000" });
      if (hedge.status === 422 && isProblem(hedge.body) && hedge.body.code === "CAPABILITY_NOT_APPROVED") {
        // This deployment does not run principal trading at all; that refusal is the answer.
        return;
      }
      expect(hedge.status, JSON.stringify(hedge.body)).toBe(400);
      expect(isProblem(hedge.body) && hedge.body.code).toBe("VALIDATION_FAILED");
      const refusal = hedge.body as Problem;
      expect(refusal.fields?.["reason"]).toBe("TREASURY_POLICY_MISSING");
      expect(typeof refusal.fields?.["action_id"]).toBe("string");

      const limits = await post(op, `${BOOKS}/${id}/limits`, {
        label: `e2e-${hex(3)}`,
        daily_loss_limit: "1000000000",
        max_mark_age_ms: 60000,
        max_inventory: [],
        max_instrument_exposure: [],
        paper_spread_bps: 0,
        paper_fee_bps: 0,
        reason: "e2e: limits that name nothing, so nothing risky can be bought",
      });
      expect(limits.status, JSON.stringify(limits.body)).toBe(200);
      expect((limits.body as { version: number }).version).toBe(1);

      const fundingKey = randomUUID();
      const funding = await post(
        op,
        `${BOOKS}/${id}/fundings`,
        { asset_id: book.valuation_asset.asset_id, quantity: "5000000000", reason: "e2e: seed the house book" },
        fundingKey,
      );
      expect(funding.status, JSON.stringify(funding.body)).toBe(200);
      const funded = funding.body as { id: string; quantity: string; value_domain: string; action: { kind: string; outcome: string } };
      expect(funded.quantity).toBe("5000000000");
      expect(funded.value_domain).toBe("SIMULATED");
      expect(funded.action.kind).toBe("FUND");
      expect(funded.action.outcome).toBe("APPLIED");

      // The same key and body replay the first answer; the same key with
      // another body is refused.
      const again = await post(
        op,
        `${BOOKS}/${id}/fundings`,
        { asset_id: book.valuation_asset.asset_id, quantity: "5000000000", reason: "e2e: seed the house book" },
        fundingKey,
      );
      expect(again.status).toBe(200);
      expect((again.body as { id: string }).id).toBe(funded.id);
      const reused = await post(
        op,
        `${BOOKS}/${id}/fundings`,
        { asset_id: book.valuation_asset.asset_id, quantity: "5000000001", reason: "e2e: seed the house book" },
        fundingKey,
      );
      expect(reused.status).toBe(409);
      expect(isProblem(reused.body) && reused.body.code).toBe("INVALID_IDEMPOTENCY_REUSE");

      // The valuation asset is the book's unit of account, never something the paper venue sells it.
      const valuationHedge = await post(op, `${BOOKS}/${id}/hedges`, { asset_id: book.valuation_asset.asset_id, side: "BUY", quantity: "1000000" });
      expect(valuationHedge.status).toBe(400);
      expect(isProblem(valuationHedge.body) && valuationHedge.body.fields?.["reason"]).toBe("TREASURY_VALUATION_ASSET");

      const report = (await (await op.request.get(`${BOOKS}/${id}/report`)).json()) as {
        value_domain: string;
        risk_policy_version?: number;
        loss_measure: string;
        limits?: Array<{ kind: string; cap?: string; codes: string[] }>;
      };
      expect(report.value_domain).toBe("SIMULATED");
      expect(report.risk_policy_version).toBe(1);
      const daily = report.limits?.find((limit) => limit.kind === "DAILY_LOSS");
      expect(daily?.cap).toBe("1000000000");

      const balances = (await (await op.request.get(`${BOOKS}/${id}/balances`)).json()) as {
        items: Array<{ asset: AssetRef; inventory: string; funded: string }>;
      };
      const dollar = balances.items.find((item) => item.asset.asset_id === book.valuation_asset.asset_id);
      expect(dollar?.inventory).toBe("5000000000");
      expect(dollar?.funded).toBe("5000000000");

      const reconciliation = (await (await op.request.get(`${BOOKS}/${id}/reconciliation`)).json()) as { agrees: boolean };
      expect(reconciliation.agrees).toBe(true);

      const actions = (await (await op.request.get(`${BOOKS}/${id}/actions`)).json()) as { items: Array<{ kind: string; outcome: string }> };
      const kinds = actions.items.map((action) => `${action.kind}:${action.outcome}`);
      expect(kinds).toContain("REGISTER:APPLIED");
      expect(kinds).toContain("PAPER_FILL:REJECTED");
      expect(kinds).toContain("SET_POLICY:APPLIED");
      expect(kinds).toContain("FUND:APPLIED");

      // The pages read the same answers back.
      await op.goto(`${SECTION}/books/${id}`);
      await expect(op.getByRole("heading", { level: 1, name: "House book", exact: true })).toBeVisible();
      await settle(op);
      await expect(op.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
      await expect(op.getByRole("region", { name, exact: true })).toBeVisible();
      await expect(op.locator('[data-limit="DAILY_LOSS"]')).toHaveCount(1);
      await expect(op.locator('[data-field="reconciliation"]')).toContainText("the register and the ledger agree");
      await expect(op.locator(`[data-balance="${book.valuation_asset.asset_id}"]`)).toHaveCount(1);
      await expect(op.locator(".malformed")).toHaveCount(0);

      await op.goto(`${SECTION}/books/${id}/activity`);
      await settle(op);
      await expect(op.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
      await expect(op.getByRole("table", { name: "House actions", exact: true }).locator("tbody tr")).toHaveCount(actions.items.length);
    } finally {
      await operator.context.close();
    }
  });

  test("needs a recent strong sign-in to register or fund, and none to cancel", async ({ browser }) => {
    const operator = await signIn(browser, "admin", false);
    try {
      const op = operator.page;
      const registered = await post(op, BOOKS, {
        name: `e2e stale ${hex(4)}`,
        strategy: "HEDGING",
        service_actor_id: "e2e-house-service",
        operators_allowed: true,
        reason: "e2e: refused without a strong sign-in",
      });
      expect(registered.status).toBe(403);
      expect(isProblem(registered.body) && registered.body.code).toBe("STEP_UP_REQUIRED");

      const funded = await post(op, `${BOOKS}/${unknownId()}/fundings`, { asset_id: unknownId(), quantity: "1", reason: "e2e" });
      expect(funded.status).toBe(403);
      expect(isProblem(funded.body) && funded.body.code).toBe("STEP_UP_REQUIRED");

      // A cancel is never new risk: it reaches the treasury, which answers for the order it cannot find.
      const cancelled = await post(op, `${BOOKS}/${unknownId()}/orders/${unknownId()}/cancel`, undefined);
      expect(cancelled.status, JSON.stringify(cancelled.body)).not.toBe(403);
      expect(isProblem(cancelled.body) && cancelled.body.code).not.toBe("STEP_UP_REQUIRED");
    } finally {
      await operator.context.close();
    }
  });

  test("the register form creates its idempotency key when the operator confirms", async ({ browser, page }) => {
    await ensureSandboxDollar(page);
    const operator = await signIn(browser, "admin", true);
    try {
      const op = operator.page;
      await op.goto(SECTION);
      await settle(op);
      const form = op.getByRole("form", { name: "Register a house book" });
      await form.getByLabel("Name", { exact: true }).fill(`e2e form ${hex(6)}`);
      await form.getByLabel("House service id", { exact: true }).fill("e2e-house-service");
      await form.getByLabel("Reason", { exact: true }).fill("e2e: registered through the page");
      await form.getByRole("button", { name: "Review: Register the house book", exact: true }).click();
      await expect(form.getByText("Review what will be sent")).toBeVisible();
      const [request] = await Promise.all([
        op.waitForRequest((r) => new URL(r.url()).pathname === BOOKS && r.method() === "POST"),
        form.getByRole("button", { name: "Confirm: Register the house book", exact: true }).click(),
      ]);
      const key = request.headers()["idempotency-key"];
      expect(key, "the confirmed command carries its key").toBeTruthy();
      await expect(form.getByText(`idempotency key ${key as string}`)).toBeVisible();
      await expect(form.getByRole("status", { name: "The backend's answer: Register the house book" })).toBeVisible();
    } finally {
      await operator.context.close();
    }
  });
});
