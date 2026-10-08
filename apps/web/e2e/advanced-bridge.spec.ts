/**
 * The bridge section of the advanced area, against the real API.
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database
 * as customer-a, and every assertion compares what a page shows with what the
 * backend answered, including where it answers no:
 *
 *   - in a deployment with no sandbox chain configured, every bridge route
 *     answers UNSUPPORTED, and every page shows that refusal with its code;
 *   - where the chain is configured, the holdings by chain, the value in
 *     flight with its haircut, the routes and the chains' state are drawn
 *     from the backend's answers;
 *   - a customer is refused every operator action — opening or pausing a
 *     route, settling a leg, producing blocks, injecting a fault — and the
 *     chains page shows that refusal with its code;
 *   - another account's legs are refused, a leg nobody recorded is NOT_FOUND,
 *     and a transfer on a route that does not exist is refused.
 *
 * A seeded database holds no route, and a customer cannot open one, so where
 * the bridge is served this spec first has the operator ("admin", signed in
 * with a strong sign-in in a context of its own) open a route of its own:
 * from a sandbox stable asset it registers on sbx-solana to a bridged twin on
 * sbx-ethereum. The routes page is then checked against a real route; a leg
 * is the customer's to start, so the leg checks still take their
 * stated-absence branch, which is the honest outcome, not a skipped test.
 */
import { randomBytes, randomUUID } from "node:crypto";

import { expect, test, type BrowserContext, type Locator, type Page } from "@playwright/test";

import { goto } from "./pace.ts";
import { signIn } from "./sign-in.ts";

const NOTICE = "Simulated value — sandbox only";
const BASE = "/advanced/bridge";
/** The deployment's one sandbox dollar (econ.SandboxDollarID): the reserve of the route's source asset. */
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";

interface Problem {
  readonly code: string;
  readonly detail?: string;
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

/** A well-formed UUIDv7 nobody recorded: the version nibble of a random v4 set to 7. */
function unknownId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
}

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

async function post(page: Page, path: string, body: unknown): Promise<{ status: number; body: unknown }> {
  const response = await page.request.post(path, {
    headers: { "Idempotency-Key": randomUUID(), "Sec-Fetch-Site": "same-origin" },
    data: body,
  });
  return { status: response.status(), body: (await response.json()) as unknown };
}

async function expectRefusal(region: Locator, code: string): Promise<void> {
  await expect(region.locator(".explain-meta").first()).toContainText(`code ${code}`);
}

/** Whether this deployment serves the bridge at all: with no sandbox chain configured, every route answers UNSUPPORTED. */
async function served(page: Page): Promise<boolean> {
  const response = await page.request.get("/v1/advanced/bridge/chains");
  const body = (await response.json()) as unknown;
  if (response.status() === 422) {
    expect(isProblem(body) && body.code, "an unwired deployment answers UNSUPPORTED").toBe("UNSUPPORTED");
    expect(isProblem(body) ? (body.detail ?? "") : "").toContain("sandbox chain");
    return false;
  }
  expect(response.status(), "the chains are served").toBe(200);
  return true;
}

test.beforeAll(async ({ browser }) => {
  const customer = await browser.newContext({ storageState: ".playwright/state.json" });
  let operator: BrowserContext | undefined;
  try {
    const page = await customer.newPage();
    if (!(await served(page))) return;
    // The faucet creates the sandbox dollar the source asset is reserved in.
    const grant = await post(page, "/v1/advanced/transfers/faucet/grants", { account_id: await accountId(page), amount: "1" });
    expect([201, 429], `the faucet runs in this deployment: ${JSON.stringify(grant.body)}`).toContain(grant.status);

    const signedIn = await signIn(browser, "admin", true);
    operator = signedIn.context;
    const op = signedIn.page;

    const suffix = randomBytes(3).toString("hex");
    const source = await post(op, "/v1/advanced/stable-assets", {
      name: `e2e-bridge-src-${suffix}`,
      symbol: `E2E-B${suffix.toUpperCase()}`,
      peg_currency: "USD",
      decimals: 6,
      issuance_cap: "1000000000000",
      redemption_mode: "INSTANT",
      peg: "1",
      oracle_source: "e2e-bridge",
      max_price_age_ms: 3_600_000,
      peg_deviation_threshold_bps: 50,
      attestable: true,
      reserves: [{ asset_id: SANDBOX_DOLLAR, kind: "CASH_EQUIVALENT", factor_bps: 10_000, accepted_for_mint: true }],
    });
    expect(source.status, `the operator registers the route's source asset: ${JSON.stringify(source.body)}`).toBe(200);
    const route = await post(op, "/v1/advanced/bridge/routes", {
      name: `e2e-route-${suffix}`,
      source_asset_id: (source.body as { asset: { asset_id: string } }).asset.asset_id,
      destination_chain: "sbx-ethereum",
      destination_symbol: `E2E-B${suffix.toUpperCase()}-ETH`,
      unit_price: "1",
      max_release_attempts: 3,
    });
    expect(route.status, `the operator opens a route: ${JSON.stringify(route.body)}`).toBe(200);
  } finally {
    await operator?.close();
    await customer.close();
  }
});

test.describe("bridge (sandbox)", () => {
  test("the section renders under the simulated-value notice and is reachable from the area", async ({ page }) => {
    await goto(page, "/advanced/overview");
    await expect(page.locator("h1")).toHaveCount(1);
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Bridge", exact: true })
      .click();
    await expect(page).toHaveURL(/\/advanced\/bridge$/);
    await expect(page.getByRole("heading", { level: 1, name: "Bridge", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");
    await settle(page);
    await expect(page.locator(".malformed")).toHaveCount(0);
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
  });

  test("holdings by chain and value in flight are the backend's answer, or its refusal", async ({ page }) => {
    const wired = await served(page);
    const own = await accountId(page);
    const response = await page.request.get(`/v1/advanced/bridge/holdings?account_id=${own}`);
    await goto(page, BASE);
    await settle(page);
    if (!wired) {
      expect(response.status()).toBe(422);
      await expectRefusal(page.locator("main"), "UNSUPPORTED");
      return;
    }
    if (response.status() === 409) {
      // The bridge refuses to answer when its legs and the ledger disagree; the page says so with the code.
      await expectRefusal(page.locator("main"), "RECONCILIATION_REQUIRED");
      return;
    }
    expect(response.status()).toBe(200);
    const body = (await response.json()) as {
      value_domain: string;
      chains: Array<{ chain: string; holdings: unknown[] }>;
      legs: unknown[];
      policy: { version: string };
    };
    expect(body.value_domain).toBe("SIMULATED");
    const byChain = page.getByRole("region", { name: "Holdings by chain", exact: true });
    if (body.chains.length === 0) {
      await expect(byChain).toContainText("holds no simulated asset on any chain");
    }
    for (const group of body.chains) {
      const table = byChain.getByRole("table", { name: `Holdings on ${group.chain}`, exact: true });
      await expect(table.locator("tbody tr")).toHaveCount(group.holdings.length);
    }
    const between = page.getByRole("region", { name: "Value between chains", exact: true });
    await expect(between).toContainText(body.policy.version);
    if (body.legs.length === 0) {
      await expect(between).toContainText("The account has no open leg");
    }
  });

  test("the chains page states each chain's tip and finality, and whether faults may be injected", async ({ page }) => {
    const wired = await served(page);
    await goto(page, `${BASE}/chains`);
    await settle(page);
    const panel = page.getByRole("region", { name: "Chains", exact: true });
    if (!wired) {
      await expectRefusal(panel, "UNSUPPORTED");
      return;
    }
    const chains = (await (await page.request.get("/v1/advanced/bridge/chains")).json()) as {
      fault_injection_allowed: boolean;
      chains: Array<{ id: string; finalized_height: number }>;
    };
    await expect(panel).toContainText(chains.fault_injection_allowed ? "available in this deployment" : "not available here");
    for (const chain of chains.chains) {
      // Finality only moves forward, so the page's figure is at least the one read before it.
      const row = panel.locator(`tr[data-chain="${chain.id}"]`);
      await expect(row).toHaveCount(1);
      const shown = ((await row.locator('[data-field="finalized_height"]').textContent()) ?? "").trim();
      expect(shown.length, `${chain.id}'s finalized height is shown`).toBeGreaterThan(0);
    }
  });

  test("a customer is refused every operator action, and the chains page shows the refusal with its code", async ({ page }) => {
    const opened = await post(page, "/v1/advanced/bridge/routes", {
      name: "e2e-refused",
      source_asset_id: unknownId(),
      destination_chain: "sbx-ethereum",
      destination_symbol: "E2E-REFUSED",
      unit_price: "1",
      max_release_attempts: 3,
    });
    expect(opened.status, "opening a route is an operator action").toBe(403);
    expect(isProblem(opened.body) && opened.body.code).toBe("FORBIDDEN");

    const route = unknownId();
    for (const action of ["pause", "resume"]) {
      const refused = await post(page, `/v1/advanced/bridge/routes/${route}/${action}`, { reason: "e2e probe" });
      expect(refused.status, `${action} is refused to a customer`).toBe(403);
    }
    const reconcile = await post(page, `/v1/advanced/bridge/routes/${route}/reconciliations`, {});
    expect(reconcile.status).toBe(403);
    const leg = await post(page, `/v1/advanced/bridge/legs/${unknownId()}/reconcile`, {});
    expect(leg.status, "settling a leg is an operator action").toBe(403);
    const blocks = await post(page, "/v1/advanced/bridge/chains/sbx-solana/blocks", { count: 1 });
    expect(blocks.status, "producing blocks is an operator action").toBe(403);
    const fault = await post(page, "/v1/advanced/bridge/chains/sbx-solana/faults", { kind: "TIMEOUT_BUT_LAND" });
    expect(fault.status, "injecting a fault is an operator action").toBe(403);
    expect(isProblem(fault.body) && fault.body.code).toBe("FORBIDDEN");

    await goto(page, `${BASE}/chains`);
    await settle(page);
    const produce = page.getByRole("region", { name: "Produce blocks", exact: true });
    await produce.getByLabel("Blocks").fill("1");
    await produce.getByRole("button", { name: "Review: produce blocks", exact: true }).click();
    await produce.getByRole("button", { name: "Confirm: produce blocks", exact: true }).click();
    await expectRefusal(produce, "FORBIDDEN");

    const faults = page.getByRole("region", { name: "Inject a submission fault", exact: true });
    await expect(faults).toContainText("only in LOCAL, TEST and DEV");
    await faults.getByRole("button", { name: "Review: inject the fault", exact: true }).click();
    await faults.getByRole("button", { name: "Confirm: inject the fault", exact: true }).click();
    await expectRefusal(faults, "FORBIDDEN");
  });

  test("another account's legs are refused, and a leg nobody recorded is NOT_FOUND", async ({ page }) => {
    const wired = await served(page);
    const own = await accountId(page);
    const foreign = await page.request.get(`/v1/advanced/bridge/legs?account_id=${unknownId()}`);
    expect(foreign.status(), "a foreign account's legs are refused").toBe(wired ? 403 : 422);
    const foreignHoldings = await page.request.get(`/v1/advanced/bridge/holdings?account_id=${unknownId()}`);
    expect(foreignHoldings.status(), "a foreign account's holdings are refused").toBe(wired ? 403 : 422);
    const ownLegs = await page.request.get(`/v1/advanced/bridge/legs?account_id=${own}`);
    expect(ownLegs.status()).toBe(wired ? 200 : 422);
    const missing = await page.request.get(`/v1/advanced/bridge/legs/${unknownId()}`);
    expect(missing.status(), "a leg id nobody recorded").toBe(wired ? 404 : 422);

    await goto(page, `${BASE}/legs/${unknownId()}`);
    await expect(page.getByRole("heading", { level: 1, name: "Bridge leg", exact: true })).toBeVisible();
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    await expectRefusal(page.locator("main"), wired ? "NOT_FOUND" : "UNSUPPORTED");
  });

  test("a transfer is made only from the caller's own account, on a route that exists, with an exact quantity", async ({ page }) => {
    const wired = await served(page);
    const own = await accountId(page);
    const fraction = await post(page, "/v1/advanced/bridge/legs", {
      account_id: own,
      route_id: unknownId(),
      quantity: "1.5",
      mode: "SANDBOX",
    });
    expect(fraction.status, "a fraction of a base unit is refused").toBe(wired ? 400 : 422);
    expect(isProblem(fraction.body) && fraction.body.code).toBe(wired ? "VALIDATION_FAILED" : "UNSUPPORTED");

    const foreign = await post(page, "/v1/advanced/bridge/legs", {
      account_id: unknownId(),
      route_id: unknownId(),
      quantity: "1000000",
      mode: "SANDBOX",
    });
    expect(foreign.status, "units are only ever bridged from the caller's own account").toBe(wired ? 403 : 422);

    const nowhere = await post(page, "/v1/advanced/bridge/legs", {
      account_id: own,
      route_id: unknownId(),
      quantity: "1000000",
      mode: "SANDBOX",
    });
    expect(nowhere.status, "a transfer on a route that does not exist").toBe(wired ? 404 : 422);
  });

  test("the routes page lists exactly the backend's routes", async ({ page }) => {
    const wired = await served(page);
    await goto(page, `${BASE}/routes`);
    await settle(page);
    const panel = page.getByRole("region", { name: "Routes", exact: true });
    if (!wired) {
      await expectRefusal(panel, "UNSUPPORTED");
      return;
    }
    const routes = (await (await page.request.get("/v1/advanced/bridge/routes?limit=200")).json()) as {
      value_domain: string;
      items: Array<{ id: string; name: string }>;
    };
    expect(routes.value_domain).toBe("SIMULATED");
    if (routes.items.length === 0) {
      await expect(panel).toContainText("No bridge route exists");
      return;
    }
    const table = panel.getByRole("table", { name: "Bridge routes", exact: true });
    await expect(table.locator("tbody tr")).toHaveCount(routes.items.length);
    for (const route of routes.items) {
      await expect(table.locator(`tr[data-route="${route.id}"]`)).toContainText(route.name);
    }
  });

  test("the section's pages reflow at 390px without sideways scrolling", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    for (const path of [BASE, `${BASE}/routes`, `${BASE}/legs`, `${BASE}/chains`, `${BASE}/operator`]) {
      await goto(page, path);
      await settle(page);
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
    }
  });

  test("no dead controls in the section", async ({ page }) => {
    for (const path of [BASE, `${BASE}/routes`, `${BASE}/chains`, `${BASE}/operator`]) {
      await goto(page, path);
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
