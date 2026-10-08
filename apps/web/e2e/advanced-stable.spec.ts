/**
 * The stable asset section of the advanced area, against the real API.
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database
 * as customer-a, and every assertion compares what a page shows with what the
 * backend answered, including where it answers no:
 *
 *   - in a deployment with no sandbox chain configured, every stable asset
 *     route answers UNSUPPORTED, and every page shows that refusal with its
 *     code rather than a figure;
 *   - where the chain is configured, the asset list, the holdings, the reserve
 *     ledger and the snapshot chain's verification are drawn from the
 *     backend's answers, a snapshot's verified badge being exactly the
 *     backend's `verified`;
 *   - a customer is refused every operator action by the backend, and the
 *     operator page shows that refusal with its code;
 *   - another account's requests are refused, a request read by an id nobody
 *     recorded is NOT_FOUND, and a malformed quantity is VALIDATION_FAILED.
 *
 * A seeded database usually holds no stable asset, and a customer cannot
 * register one, so the asset-level checks take the stated-absence branch
 * there; that is the honest outcome, not a skipped test.
 */
import { randomUUID } from "node:crypto";

import { expect, test, type Locator, type Page } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";
const BASE = "/advanced/stable-assets";

interface Problem {
  readonly code: string;
  readonly detail?: string;
}

interface AssetRow {
  readonly id: string;
  readonly symbol: string;
  readonly status: string;
  readonly attestable: boolean;
}

interface SnapshotChain {
  readonly total: number;
  readonly verification: { readonly verified: boolean; readonly checked: number };
  readonly items: ReadonlyArray<{ readonly seq: number; readonly verified: boolean }>;
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

async function expectRefusal(region: Locator, code: string): Promise<void> {
  await expect(region.locator(".explain-meta").first()).toContainText(`code ${code}`);
}

/**
 * Whether this deployment serves the stable asset at all: with no sandbox
 * chain configured, every route answers UNSUPPORTED (422), saying so.
 */
async function served(page: Page): Promise<{ readonly wired: boolean; readonly assets: readonly AssetRow[] }> {
  const response = await page.request.get("/v1/advanced/stable-assets?limit=200");
  const body = (await response.json()) as unknown;
  if (response.status() === 422) {
    expect(isProblem(body) && body.code, "an unwired deployment answers UNSUPPORTED").toBe("UNSUPPORTED");
    expect(isProblem(body) ? (body.detail ?? "") : "").toContain("sandbox chain");
    return { wired: false, assets: [] };
  }
  expect(response.status(), "the stable asset list is served").toBe(200);
  const list = body as { value_domain: string; items: AssetRow[] };
  expect(list.value_domain).toBe("SIMULATED");
  return { wired: true, assets: list.items };
}

test.describe("stable assets (sandbox)", () => {
  test("the section renders under the simulated-value notice and is reachable from the area", async ({ page }) => {
    await page.goto("/advanced/overview");
    await expect(page.locator("h1")).toHaveCount(1);
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Stable assets", exact: true })
      .click();
    await expect(page).toHaveURL(/\/advanced\/stable-assets$/);
    await expect(page.getByRole("heading", { level: 1, name: "Stable assets", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");
    await settle(page);
    await expect(page.locator(".malformed")).toHaveCount(0);
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
  });

  test("the asset list and the holdings are the backend's answer, or its UNSUPPORTED refusal", async ({ page }) => {
    const { wired, assets } = await served(page);
    await page.goto(BASE);
    await settle(page);
    const list = page.getByRole("region", { name: "Registered stable assets", exact: true });
    const holdings = page.getByRole("region", { name: "Holdings", exact: true });
    if (!wired) {
      await expectRefusal(list, "UNSUPPORTED");
      await expectRefusal(holdings, "UNSUPPORTED");
      return;
    }
    if (assets.length === 0) {
      await expect(list).toContainText("No stable asset is registered");
    } else {
      const table = list.getByRole("table", { name: "Stable assets", exact: true });
      await expect(table.locator("tbody tr")).toHaveCount(assets.length);
      for (const asset of assets) {
        await expect(table.getByRole("link", { name: asset.symbol, exact: true })).toBeVisible();
      }
    }
    const own = await accountId(page);
    const answer = (await (await page.request.get(`/v1/advanced/stable-assets/holdings?account_id=${own}`)).json()) as {
      value_domain: string;
      items: Array<{ stable_asset_id: string }>;
      payment_assets: Array<{ asset_id: string }>;
    };
    expect(answer.value_domain).toBe("SIMULATED");
    if (answer.items.length > 0) {
      await expect(holdings.getByRole("table", { name: "Stable asset holdings", exact: true }).locator("tbody tr")).toHaveCount(
        answer.items.length,
      );
    } else {
      await expect(holdings).toContainText("the account holds none");
    }
  });

  test("a customer is refused every operator action, and the page shows the refusal with its code", async ({ page }) => {
    const registered = await post(page, "/v1/advanced/stable-assets", {
      name: "e2e-refused",
      symbol: "E2E-REFUSED",
      peg_currency: "USD",
      decimals: 6,
      issuance_cap: "1000000000",
      redemption_mode: "INSTANT",
      peg: "1",
      oracle_source: "e2e",
      max_price_age_ms: 60000,
      peg_deviation_threshold_bps: 50,
      attestable: true,
      reserves: [],
    });
    expect(registered.status, "registering a stable asset is an operator action").toBe(403);
    expect(isProblem(registered.body) && registered.body.code).toBe("FORBIDDEN");

    const target = unknownId();
    for (const action of ["pause", "resume"]) {
      const refused = await post(page, `/v1/advanced/stable-assets/${target}/${action}`, { reason: "e2e probe" });
      expect(refused.status, `${action} is refused to a customer`).toBe(403);
      expect(isProblem(refused.body) && refused.body.code).toBe("FORBIDDEN");
    }
    for (const action of ["monitor-runs", "reconciliations", "snapshots", "redemption-queue/run"]) {
      const refused = await post(page, `/v1/advanced/stable-assets/${target}/${action}`, {});
      expect(refused.status, `${action} is refused to a customer`).toBe(403);
    }
    const resolve = await post(page, "/v1/advanced/stable-assets/ops/resolve-unknown", {});
    expect(resolve.status, "settling unknown submissions is an operator action").toBe(403);

    // The operator page offers the command and shows the backend's refusal.
    await page.goto(`${BASE}/operator`);
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    const panel = page.getByRole("region", { name: "Settle unknown submissions", exact: true });
    await panel.getByRole("button", { name: "Review: settle unknown submissions", exact: true }).click();
    await panel.getByRole("button", { name: "Confirm: settle unknown submissions", exact: true }).click();
    await expectRefusal(panel, "FORBIDDEN");
    await expect(panel).toContainText("idempotency key");
  });

  test("another account's requests are refused, and an id nobody recorded is NOT_FOUND", async ({ page }) => {
    const { wired } = await served(page);
    const own = await accountId(page);
    const someoneElse = unknownId();
    for (const list of ["issuances", "redemptions", "holdings"]) {
      const foreign = await page.request.get(`/v1/advanced/stable-assets/${list}?account_id=${someoneElse}`);
      expect(foreign.status(), `a foreign account's ${list} are refused`).toBe(wired ? 403 : 422);
      const ownList = await page.request.get(`/v1/advanced/stable-assets/${list}?account_id=${own}`);
      expect(ownList.status(), `the caller's own ${list}`).toBe(wired ? 200 : 422);
    }
    for (const kind of ["issuances", "redemptions"]) {
      const missing = await page.request.get(`/v1/advanced/stable-assets/${kind}/${unknownId()}`);
      expect(missing.status(), `an ${kind} id nobody recorded`).toBe(wired ? 404 : 422);
    }
    await page.goto(`${BASE}/issuances/${unknownId()}`);
    await expect(page.getByRole("heading", { level: 1, name: "Issuance request", exact: true })).toBeVisible();
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    await expectRefusal(page.locator("main"), wired ? "NOT_FOUND" : "UNSUPPORTED");
  });

  test("an issuance is made only on the caller's own account, with an exact quantity", async ({ page }) => {
    const { wired } = await served(page);
    const own = await accountId(page);
    const asset = unknownId();
    const fraction = await post(page, `/v1/advanced/stable-assets/${asset}/issuances`, {
      account_id: own,
      quantity: "1.5",
      reserve_asset_id: unknownId(),
      mode: "SANDBOX",
    });
    expect(fraction.status, "a fraction of a base unit is refused").toBe(wired ? 400 : 422);
    expect(isProblem(fraction.body) && fraction.body.code).toBe(wired ? "VALIDATION_FAILED" : "UNSUPPORTED");

    const foreign = await post(page, `/v1/advanced/stable-assets/${asset}/redemptions`, {
      account_id: unknownId(),
      quantity: "1000000",
      mode: "SANDBOX",
    });
    expect(foreign.status, "a redemption is only ever made from the caller's own account").toBe(wired ? 403 : 422);

    const nowhere = await post(page, `/v1/advanced/stable-assets/${asset}/issuances`, {
      account_id: own,
      quantity: "1000000",
      reserve_asset_id: unknownId(),
      mode: "SANDBOX",
    });
    expect(nowhere.status, "an issuance of an asset nobody registered").toBe(wired ? 404 : 422);
  });

  test("the reserve page draws the ledger and the snapshot chain's verification from the backend", async ({ page }) => {
    const { wired, assets } = await served(page);
    if (!wired) {
      await page.goto(`${BASE}/${unknownId()}/reserve`);
      await settle(page);
      await expectRefusal(page.getByRole("region", { name: "Reserve ledger", exact: true }), "UNSUPPORTED");
      await expectRefusal(page.getByRole("region", { name: "Snapshot chain", exact: true }), "UNSUPPORTED");
      return;
    }
    const asset = assets[0];
    if (asset === undefined) {
      await page.goto(`${BASE}/${unknownId()}/reserve`);
      await settle(page);
      await expectRefusal(page.getByRole("region", { name: "Reserve ledger", exact: true }), "NOT_FOUND");
      return;
    }
    const reserve = (await (await page.request.get(`/v1/advanced/stable-assets/${asset.id}/reserve`)).json()) as {
      value_domain: string;
      covered: boolean;
    };
    expect(reserve.value_domain).toBe("SIMULATED");
    const chain = (await (await page.request.get(`/v1/advanced/stable-assets/${asset.id}/snapshots?limit=50`)).json()) as SnapshotChain;

    await page.goto(`${BASE}/${asset.id}/reserve`);
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    const ledger = page.getByRole("region", { name: "Reserve ledger", exact: true });
    await expect(ledger).toContainText(reserve.covered ? "covered" : "not covered");
    const snapshots = page.getByRole("region", { name: "Snapshot chain", exact: true });
    await expect(snapshots.locator('[data-field="verification"]')).toContainText(
      chain.verification.verified ? "chain verified" : "chain does not verify",
    );
    for (const item of chain.items) {
      // The badge is the backend's verdict on that snapshot, never the page's own.
      const card = snapshots.getByRole("region", { name: `Snapshot ${String(item.seq)}`, exact: true });
      await expect(card.locator(".pill").first()).toHaveText(item.verified ? "verified" : "unverified");
    }
    if (chain.items.length === 0) {
      await expect(snapshots).toContainText("No snapshot has been published");
    }
  });

  test("the section's pages reflow at 390px without sideways scrolling", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const { assets } = await served(page);
    const paths = [BASE, `${BASE}/issuances`, `${BASE}/redemptions`, `${BASE}/operator`];
    const first = assets[0];
    if (first !== undefined) paths.push(`${BASE}/${first.id}`, `${BASE}/${first.id}/reserve`);
    for (const path of paths) {
      await page.goto(path);
      await settle(page);
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
    }
  });

  test("no dead controls in the section", async ({ page }) => {
    const { assets } = await served(page);
    const paths = [BASE, `${BASE}/operator`];
    const first = assets[0];
    if (first !== undefined) paths.push(`${BASE}/${first.id}`);
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
