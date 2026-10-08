/**
 * Prediction markets in the advanced (sandbox) area, against the real API.
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database
 * as customer-a, and every assertion compares what a page shows with what the
 * backend answered, including where it answers no:
 *
 *   - the list is exactly the backend's list for the status asked for, and an
 *     unknown status is said to have been ignored, never sent;
 *   - a customer is refused every operator action by the backend, and the
 *     pages offer them none;
 *   - another account's shares are refused before any market is looked up,
 *     an unknown market is NOT_FOUND, and a fraction of a complete set is
 *     VALIDATION_FAILED;
 *   - a void is two operators' act (proposal, then approval) and a customer
 *     is refused both; a held payout is claimed only on the caller's own
 *     account, and a market that holds none answers NOT_FOUND;
 *   - where a market exists, its books, the account's shares and the
 *     complete-set form are drawn from the backend's answers.
 *
 * A seeded database holds no prediction market, and a customer cannot define
 * one, so before its tests run this spec has the operator ("admin", signed in
 * with a strong sign-in in a context of its own) define and open a SANDBOX
 * market of its own. The market-level checks then always have a market, and
 * never depend on what another spec left behind; the stated-absence branches
 * remain for a deployment where defining one is refused.
 */
import { randomBytes, randomUUID } from "node:crypto";

import { expect, test, type BrowserContext, type Locator, type Page } from "@playwright/test";

import { signIn } from "./sign-in.ts";

const NOTICE = "Simulated value — sandbox only";
const LIST = "/advanced/prediction-markets";
const STATUSES = ["DRAFT", "OPEN", "CLOSED", "RESOLVING", "RESOLVED", "DISPUTED", "SETTLED", "VOIDED"] as const;

interface Problem {
  readonly code: string;
  readonly detail?: string;
}

interface MarketRow {
  readonly id: string;
  readonly symbol: string;
  readonly status: string;
  readonly share_decimals: number;
  readonly outcomes: ReadonlyArray<{ readonly symbol: string }>;
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

/** A well-formed UUIDv7 nobody recorded: the version nibble of a random v4 set to 7. */
function unknownId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
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

/** Waits until the page has its answer on screen (see advanced-core.spec.ts). */
async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
}

/** A command as the browser would send it: same-origin, with its own idempotency key. */
async function post(page: Page, path: string, body: unknown): Promise<{ status: number; body: unknown }> {
  const response = await page.request.post(path, {
    headers: { "Idempotency-Key": randomUUID(), "Sec-Fetch-Site": "same-origin" },
    data: body,
  });
  return { status: response.status(), body: (await response.json()) as unknown };
}

async function listed(page: Page, status: string): Promise<MarketRow[]> {
  const response = await page.request.get(`/v1/advanced/prediction-markets?status=${status}&limit=200`);
  expect(response.status(), `the ${status} list is served`).toBe(200);
  const body = (await response.json()) as { value_domain: string; items: MarketRow[] };
  expect(body.value_domain).toBe("SIMULATED");
  return body.items;
}

/** The OPEN market this spec defined for itself before its tests ran. */
let ownMarket: MarketRow | undefined;

test.beforeAll(async ({ browser }) => {
  const now = Date.now();
  // The customer's faucet grant creates the sandbox dollar, the market's collateral.
  const customer = await browser.newContext({ storageState: ".playwright/state.json" });
  let operator: BrowserContext | undefined;
  try {
    const page = await customer.newPage();
    const grant = await post(page, "/v1/advanced/transfers/faucet/grants", { account_id: await accountId(page), amount: "1" });
    expect([201, 429], `the faucet runs in this deployment: ${JSON.stringify(grant.body)}`).toContain(grant.status);

    const signedIn = await signIn(browser, "admin", true);
    operator = signedIn.context;
    const op = signedIn.page;

    const created = await post(op, "/v1/advanced/prediction-markets", {
      symbol: `E2E-PM-${randomBytes(3).toString("hex").toUpperCase()}`,
      question: "Does the prediction-market suite draw a market of its own?",
      outcomes: ["YES", "NO"],
      mode: "SANDBOX",
      open_at: new Date(now - 60_000).toISOString(),
      close_at: new Date(now + 24 * 60 * 60_000).toISOString(),
      dispute_window_ms: 0,
      oracle_source: "e2e",
      oracle_public_key: "ab".repeat(32),
    });
    expect(created.status, `the operator defines a market: ${JSON.stringify(created.body)}`).toBe(201);
    const id = (created.body as { id: string }).id;
    const opened = await post(op, `/v1/advanced/prediction-markets/${id}/open`, {});
    expect(opened.status, `the operator opens it: ${JSON.stringify(opened.body)}`).toBe(200);
    ownMarket = (await listed(page, "OPEN")).find((market) => market.id === id);
    expect(ownMarket, "the market the operator opened is listed OPEN").toBeTruthy();
  } finally {
    await operator?.close();
    await customer.close();
  }
});

/** This spec's own market; otherwise the first market in any status, if the deployment has one. */
async function anyMarket(page: Page): Promise<MarketRow | undefined> {
  if (ownMarket !== undefined) return ownMarket;
  for (const status of ["OPEN", ...STATUSES.filter((s) => s !== "OPEN")]) {
    const first = (await listed(page, status))[0];
    if (first !== undefined) return first;
  }
  return undefined;
}

async function expectRefusal(region: Locator, code: string): Promise<void> {
  await expect(region.locator(".explain-meta").first()).toContainText(`code ${code}`);
}

test.describe("prediction markets (sandbox)", () => {
  test("the section renders under the simulated-value notice and is reachable from the area", async ({ page }) => {
    await page.goto("/advanced/overview");
    await expect(page.locator("h1")).toHaveCount(1);
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Prediction markets", exact: true })
      .click();
    await expect(page).toHaveURL(/\/advanced\/prediction-markets$/);
    await expect(page.getByRole("heading", { level: 1, name: "Prediction markets", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");
    await settle(page);
    await expect(page.locator(".malformed")).toHaveCount(0);
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
  });

  test("the list is exactly the backend's list for the status asked for", async ({ page }) => {
    for (const status of ["OPEN", "SETTLED"] as const) {
      const items = await listed(page, status);
      await page.goto(`${LIST}?status=${status}`);
      await settle(page);
      const panel = page.getByRole("region", { name: "Markets", exact: true });
      await expect(panel.getByLabel("Market status")).toHaveValue(status);
      if (items.length === 0) {
        await expect(panel).toContainText(`No ${status} markets`);
        continue;
      }
      const table = page.getByRole("table", { name: "Markets in this status", exact: true });
      await expect(table.locator("tbody tr")).toHaveCount(items.length);
      for (const market of items) {
        await expect(table.getByRole("link", { name: market.symbol, exact: true })).toBeVisible();
      }
    }

    // Choosing a status asks the backend for it; the address says which.
    await page.goto(LIST);
    await settle(page);
    await page.getByLabel("Market status").selectOption("VOIDED");
    await expect(page).toHaveURL(/[?&]status=VOIDED$/);
  });

  test("an unknown status in the address is reported and not sent", async ({ page }) => {
    await page.goto(`${LIST}?status=LIVE`);
    await settle(page);
    await expect(page.getByRole("status").filter({ hasText: "LIVE" }).first()).toBeVisible();
    await expect(page.getByLabel("Market status")).toHaveValue("OPEN");
  });

  test("an unknown market answers NOT_FOUND on its page", async ({ page }) => {
    await page.goto(`${LIST}/${unknownId()}`);
    await expect(page.getByRole("heading", { level: 1, name: "Prediction market", exact: true })).toBeVisible();
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    await expectRefusal(page.locator("main"), "NOT_FOUND");
  });

  test("a customer is refused every operator action, and the pages offer none", async ({ page }) => {
    const created = await post(page, "/v1/advanced/prediction-markets", {
      symbol: "E2E-REFUSED",
      question: "May a customer define a prediction market?",
      outcomes: ["YES", "NO"],
      mode: "SANDBOX",
      open_at: "2099-01-01T00:00:00Z",
      close_at: "2099-01-02T00:00:00Z",
      dispute_window_ms: 0,
      oracle_source: "e2e",
      oracle_public_key: "ab".repeat(32),
    });
    expect(created.status, "defining a market is an operator action").toBe(403);
    expect(isProblem(created.body) && created.body.code).toBe("FORBIDDEN");

    const target = unknownId();
    for (const action of ["open", "close", "request-resolution", "settle", "reconcile"]) {
      const refused = await post(page, `/v1/advanced/prediction-markets/${target}/${action}`, {});
      expect(refused.status, `${action} is refused to a customer`).toBe(403);
      expect(isProblem(refused.body) && refused.body.code).toBe("FORBIDDEN");
    }
    const resolved = await post(page, `/v1/advanced/prediction-markets/${target}/resolve`, {});
    expect(resolved.status).toBe(403);
    // A void is two operators' act: a customer can neither propose one nor approve one.
    const proposed = await post(page, `/v1/advanced/prediction-markets/${target}/void-proposals`, {
      reason: "a customer would like this market voided",
      evidence: { statement: "none" },
    });
    expect(proposed.status, "proposing a void is an operator action").toBe(403);
    expect(isProblem(proposed.body) && proposed.body.code).toBe("FORBIDDEN");
    const approved = await post(page, `/v1/advanced/prediction-markets/${target}/void-proposals/${unknownId()}/approve`, {});
    expect(approved.status, "approving a void is an operator action").toBe(403);
    expect(isProblem(approved.body) && approved.body.code).toBe("FORBIDDEN");

    await page.goto(LIST);
    await settle(page);
    await expect(page.getByRole("region", { name: "Define a market", exact: true })).toHaveCount(0);
    const market = await anyMarket(page);
    if (market !== undefined) {
      await page.goto(`${LIST}/${market.id}`);
      await settle(page);
      await expect(page.getByRole("region", { name: "Operator actions", exact: true })).toHaveCount(0);
    }
  });

  test("another account's shares are refused before any market is looked up", async ({ page }) => {
    const own = await accountId(page);
    const someoneElse = unknownId();
    for (const market of [unknownId(), (await anyMarket(page))?.id].filter((id): id is string => id !== undefined)) {
      const foreign = await page.request.get(`/v1/advanced/prediction-markets/${market}/positions?account_id=${someoneElse}`);
      expect(foreign.status(), "a foreign account is refused").toBe(403);
      const problem = (await foreign.json()) as Problem;
      expect(problem.code).toBe("FORBIDDEN");
    }
    const missing = await page.request.get(`/v1/advanced/prediction-markets/${unknownId()}/positions?account_id=${own}`);
    expect(missing.status(), "the caller's own account on a market that does not exist").toBe(404);
  });

  test("a complete set is whole, and a set on a market that does not exist is refused", async ({ page }) => {
    const own = await accountId(page);
    const market = unknownId();
    const fraction = await post(page, `/v1/advanced/prediction-markets/${market}/issuances`, {
      account_id: own,
      quantity: "1500000",
    });
    expect(fraction.status, "a set and a half is refused before anything else").toBe(400);
    expect(isProblem(fraction.body) && fraction.body.code).toBe("VALIDATION_FAILED");

    const nowhere = await post(page, `/v1/advanced/prediction-markets/${market}/redemptions`, {
      account_id: own,
      quantity: "1000000",
    });
    expect(nowhere.status).toBe(404);
    expect(isProblem(nowhere.body) && nowhere.body.code).toBe("NOT_FOUND");

    const foreign = await post(page, `/v1/advanced/prediction-markets/${market}/issuances`, {
      account_id: unknownId(),
      quantity: "1000000",
    });
    expect(foreign.status, "a set is only ever issued on the caller's own account").toBe(403);
  });

  test("a held payout is claimed only on the caller's own account, and only where the market holds one", async ({ page }) => {
    const own = await accountId(page);
    const foreign = await post(page, `/v1/advanced/prediction-markets/${unknownId()}/held-payouts/claim`, {
      account_id: unknownId(),
    });
    expect(foreign.status, "a payout is claimed only by the account it is held for").toBe(403);
    expect(isProblem(foreign.body) && foreign.body.code).toBe("FORBIDDEN");

    const nowhere = await post(page, `/v1/advanced/prediction-markets/${unknownId()}/held-payouts/claim`, { account_id: own });
    expect(nowhere.status, "the caller's own account on a market that does not exist").toBe(404);
    expect(isProblem(nowhere.body) && nowhere.body.code).toBe("NOT_FOUND");

    const market = await anyMarket(page);
    if (market === undefined) return;
    const shares = (await (
      await page.request.get(`/v1/advanced/prediction-markets/${market.id}/positions?account_id=${own}`)
    ).json()) as { held_payout?: { claimed: boolean } };
    if (shares.held_payout === undefined) {
      // The market holds nothing for this account, and says so when asked.
      const none = await post(page, `/v1/advanced/prediction-markets/${market.id}/held-payouts/claim`, { account_id: own });
      expect(none.status).toBe(404);
      expect(isProblem(none.body) && none.body.code).toBe("NOT_FOUND");
      await page.goto(`${LIST}/${market.id}`);
      await settle(page);
      await expect(page.getByRole("region", { name: "Held payout", exact: true })).toHaveCount(0);
    }
  });

  test("a market's page draws its books, the account's shares and the set form from the backend", async ({ page }) => {
    const market = await anyMarket(page);
    if (market === undefined) {
      await page.goto(LIST);
      await settle(page);
      await expect(page.getByRole("region", { name: "Markets", exact: true })).toContainText("No OPEN markets");
      return;
    }
    const own = await accountId(page);
    const books = (await (await page.request.get(`/v1/advanced/prediction-markets/${market.id}/books`)).json()) as {
      reconciles: boolean;
      outcomes: Array<{ symbol: string }>;
    };
    const shares = (await (
      await page.request.get(`/v1/advanced/prediction-markets/${market.id}/positions?account_id=${own}`)
    ).json()) as { items: Array<{ outcome_symbol: string; quantity: string }> };

    await page.goto(`${LIST}/${market.id}`);
    await expect(page.getByRole("heading", { level: 1, name: market.symbol, exact: true })).toBeVisible();
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();

    const escrow = page.getByRole("region", { name: "Escrow books", exact: true });
    await expect(escrow).toContainText(books.reconciles ? "The books reconcile" : "The books do not reconcile");
    const perOutcome = escrow.getByRole("table", { name: "Shares of each outcome", exact: true });
    await expect(perOutcome.locator("tbody tr")).toHaveCount(books.outcomes.length);

    const held = page.getByRole("region", { name: "Shares held", exact: true });
    if (shares.items.length === 0) {
      await expect(held).toContainText("No shares held");
    } else {
      const rows = held.getByRole("table", { name: "Shares held, by outcome", exact: true }).locator("tbody tr");
      await expect(rows).toHaveCount(shares.items.length);
      for (const share of shares.items) {
        // Each outcome's row carries exactly the base units the backend reported for it.
        const row = rows.filter({ has: page.getByRole("rowheader", { name: share.outcome_symbol, exact: true }) });
        await expect(row).toContainText(`${share.quantity} base units`);
      }
    }

    // The set form says why it is off for a market that is not OPEN.
    const sets = page.getByRole("region", { name: "Complete sets", exact: true });
    if (market.status !== "OPEN") {
      await expect(sets).toContainText(`this market is ${market.status}`);
    }

    // The page says where the shares trade.
    await expect(page.getByRole("region", { name: "Outcomes", exact: true })).toContainText(
      "its shares trade on the internal order book",
    );
  });

  test("the section's pages reflow at 390px without sideways scrolling", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const market = await anyMarket(page);
    for (const path of market === undefined ? [LIST] : [LIST, `${LIST}/${market.id}`]) {
      await page.goto(path);
      await settle(page);
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      );
      expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
    }
  });

  test("no dead controls in the section", async ({ page }) => {
    const market = await anyMarket(page);
    const paths = market === undefined ? [LIST] : [LIST, `${LIST}/${market.id}`];
    for (const path of paths) {
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
    }
  });
});
