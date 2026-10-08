/**
 * The operator console of the advanced (sandbox) area against the real API.
 *
 * Nothing is stubbed. The suite's own session is customer-a, who is refused
 * every operator read and command; the operator signs in here as the
 * development "admin" identity with MFA (the console's commands need a
 * recent strong sign-in). Every assertion compares what the page shows with
 * what the backend answered, including where it answers no:
 *
 *   - the overview is served to an operator, figure for figure, and refused
 *     to a customer with the code on screen;
 *   - a reference mark of a SIMULATED asset is recorded through the page and
 *     appears in that asset's marks; the backend refuses a mark of an asset
 *     that is not SIMULATED, an observation in the future, a price more
 *     precise than 18 fractional digits, and a mark posted by a customer;
 *   - the simulated chains are reported, or the deployment's UNSUPPORTED is
 *     shown as the refusal it is.
 */
import { randomBytes, randomUUID } from "node:crypto";

import { expect, test, type Browser, type BrowserContext, type Locator, type Page } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";
const CONSOLE = "/advanced/operator";
const MARKS_PATH = "/v1/advanced/ops/marks";

interface Problem {
  readonly code: string;
  readonly detail?: string;
  readonly fields?: Record<string, unknown>;
}

interface Asset {
  readonly id: string;
  readonly kind: string;
  readonly symbol: string;
  readonly status: string;
}

interface Overview {
  readonly value_domain: string;
  readonly dual_control: {
    readonly transfer_reversals_pending: number;
    readonly margin_cases_in_review: number;
    readonly margin_resolutions_pending: number;
  };
  readonly reconciliation: ReadonlyArray<{ readonly kind: string; readonly unresolved: number }>;
  readonly oracle: ReadonlyArray<{ readonly asset_id: string; readonly stale: boolean }>;
  readonly stable_assets: ReadonlyArray<{ readonly id: string }>;
  readonly bridge_routes: ReadonlyArray<{ readonly id: string }>;
  readonly bridge_legs_in_review: ReadonlyArray<{ readonly id: string; readonly released: boolean; readonly refund_requested: boolean }>;
  readonly kill_switches: ReadonlyArray<{ readonly kind: string }>;
  readonly chains_configured: boolean;
}

interface MarkList {
  readonly value_domain: string;
  readonly items: ReadonlyArray<{
    readonly id: string;
    readonly price: string;
    readonly source: string;
    readonly stale: boolean;
    readonly future: boolean;
  }>;
}

function hex(bytes: number): string {
  return randomBytes(bytes).toString("hex");
}

/**
 * A well-formed UUIDv7 nobody recorded: the version nibble of a random v4 set
 * to 7. The contract's UUID is "UUIDv7 in canonical form", so a bare v4 from
 * randomUUID() is a malformed id the backend refuses with VALIDATION_FAILED
 * before it looks anything up -- it can never stand for "an id nobody holds".
 */
function unknownId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
}

/**
 * An instant as a datetime-local control holds it: local time to the minute.
 * The browser normalises the control's value and drops seconds that are zero,
 * so a value written with ":00" would not be the value the control holds.
 */
function localMinute(at: Date): string {
  const two = (n: number): string => String(n).padStart(2, "0");
  return `${String(at.getFullYear())}-${two(at.getMonth() + 1)}-${two(at.getDate())}T${two(at.getHours())}:${two(at.getMinutes())}`;
}

/** Waits until a page has its answer on screen and nothing says it is still loading. */
async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
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

/** A command as the browser would send it: same-origin, JSON, with a fresh idempotency key. */
async function command(page: Page, path: string, body: unknown): Promise<{ readonly status: number; readonly body: unknown }> {
  const response = await page.request.post(path, {
    headers: { "Content-Type": "application/json", "Idempotency-Key": randomUUID(), "Sec-Fetch-Site": "same-origin" },
    data: JSON.stringify(body),
  });
  return { status: response.status(), body: (await response.json().catch(() => undefined)) as unknown };
}

/** A refusal is on screen with the backend's own code. */
async function expectCode(scope: Locator, code: string): Promise<void> {
  await expect(scope.locator(".explain-meta").first()).toContainText(`code ${code}`);
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

/**
 * A SIMULATED asset other than the sandbox dollar to mark: a fresh prediction
 * market's outcome share, defined by the operator, or else any SIMULATED
 * asset the registry already lists. Undefined when the deployment can offer
 * neither, which the test then says rather than inventing one.
 */
async function simulatedAssetToMark(customer: Page, operator: Page): Promise<string | undefined> {
  // The faucet provisions the sandbox dollar a market's collateral is in.
  await command(customer, "/v1/advanced/transfers/faucet/grants", { account_id: await ownAccount(customer), amount: "1" });
  const created = await command(operator, "/v1/advanced/prediction-markets", {
    symbol: `E2E-OPS-${hex(3).toUpperCase()}`,
    question: "Does the operator console record a reference mark?",
    outcomes: ["YES", "NO"],
    mode: "SANDBOX",
    open_at: "2099-01-01T00:00:00Z",
    close_at: "2099-01-02T00:00:00Z",
    dispute_window_ms: 0,
    oracle_source: "e2e",
    oracle_public_key: "ab".repeat(32),
  });
  if (created.status === 201 || created.status === 200) {
    const share = (created.body as { outcomes?: Array<{ share_asset_id?: string }> }).outcomes?.[0]?.share_asset_id;
    if (share !== undefined) return share;
  }
  const assets = (await (await operator.request.get("/v1/assets")).json()) as Asset[];
  return assets.find((asset) => asset.kind === "SIMULATED" && asset.status === "ACTIVE" && asset.symbol !== "SBX-USD")?.id;
}

test.describe("the operator console", () => {
  test("renders under the simulated-value notice and is reachable from the area's navigation", async ({ page }) => {
    await page.goto("/advanced/overview");
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Operator console", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Operator console", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");
    await settle(page);
    await expect(page.locator(".malformed")).toHaveCount(0);
  });

  test("a customer is refused the overview and every console command, with the code on screen", async ({ page }) => {
    const overview = await page.request.get("/v1/advanced/ops/overview");
    expect(overview.status(), "a customer holds no sandbox:operate").toBe(403);
    const problem = (await overview.json()) as Problem;
    expect(problem.code).toBe("FORBIDDEN");
    const marks = await page.request.get(`${MARKS_PATH}?asset_id=${unknownId()}`);
    expect(marks.status()).toBe(403);
    const posted = await command(page, MARKS_PATH, { asset_id: unknownId(), price: "1.5", source: "e2e-customer" });
    expect(posted.status, "a customer may not record a reference mark").toBe(403);
    expect((posted.body as Problem).code).toBe("FORBIDDEN");

    await page.goto(CONSOLE);
    await settle(page);
    await expectCode(page.getByRole("region", { name: "Awaiting an operator", exact: true }), problem.code);
    await expectCode(page.getByRole("region", { name: "Circuit breakers", exact: true }), problem.code);
    await expectCode(page.getByRole("region", { name: "Reconciliation mismatches", exact: true }), problem.code);
    await expectCode(page.getByRole("region", { name: "Oracle freshness", exact: true }), problem.code);
  });

  test("an operator reads the overview figure for figure", async ({ browser }) => {
    const admin = await signIn(browser, "admin", true);
    const response = await admin.page.request.get("/v1/advanced/ops/overview");
    expect(response.status()).toBe(200);
    const overview = (await response.json()) as Overview;
    expect(overview.value_domain).toBe("SIMULATED");
    await admin.page.goto(CONSOLE);
    await settle(admin.page);

    const awaiting = admin.page.getByRole("region", { name: "Awaiting an operator", exact: true });
    // Counts move only when an operator acts; none acts between the read and the page here.
    await expect(awaiting.locator('[data-field="transfer-reversals-pending"]')).toHaveText(
      String(overview.dual_control.transfer_reversals_pending),
    );
    await expect(awaiting.locator('[data-field="margin-cases-in-review"]')).toHaveText(
      String(overview.dual_control.margin_cases_in_review),
    );
    // Every bridge leg in review is listed with the actions its state allows.
    if (overview.bridge_legs_in_review.length === 0) {
      await expect(awaiting).toContainText("No bridge leg awaits an operator");
    }
    for (const leg of overview.bridge_legs_in_review) {
      const card = awaiting.locator(`[data-bridge-leg="${leg.id}"]`);
      await expect(card).toHaveAttribute("data-released", String(leg.released));
      await expect(card).toHaveAttribute("data-refund-requested", String(leg.refund_requested));
      if (leg.released) {
        await expect(card.locator('[data-leg-action="refund"]')).toContainText("count its value twice");
      } else {
        await expect(card.locator('[data-leg-action="resolve-review"]')).toContainText("nothing to reconcile");
      }
    }

    const reconciliation = admin.page.getByRole("region", { name: "Reconciliation mismatches", exact: true });
    for (const entry of overview.reconciliation) {
      await expect(reconciliation.locator(`[data-reconciliation-kind="${entry.kind}"]`)).toHaveAttribute(
        "data-unresolved",
        String(entry.unresolved),
      );
    }
    const oracle = admin.page.getByRole("region", { name: "Oracle freshness", exact: true });
    for (const asset of overview.oracle) {
      await expect(oracle.locator(`[data-oracle-asset="${asset.asset_id}"]`)).toHaveAttribute("data-stale", String(asset.stale));
    }
    const breakers = admin.page.getByRole("region", { name: "Circuit breakers", exact: true });
    for (const asset of overview.stable_assets) {
      await expect(breakers.locator(`[data-stable-asset="${asset.id}"]`)).toBeVisible();
    }
    for (const route of overview.bridge_routes) {
      await expect(breakers.locator(`[data-bridge-route="${route.id}"]`)).toBeVisible();
    }
    if (overview.kill_switches.length === 0) {
      await expect(breakers).toContainText("No kill switch is active");
    }

    // The capability gates and the liquidation queue are their own reads.
    await expect(admin.page.getByRole("region", { name: "Capability gates", exact: true }).locator("tbody tr").first()).toBeVisible();
    await expect(admin.page.getByRole("region", { name: "Margin liquidation queue", exact: true })).toBeVisible();

    // The simulated chains: reported, or the deployment's refusal shown as it is.
    const chains = await admin.page.request.get("/v1/advanced/ops/chains");
    const venue = admin.page.getByRole("region", { name: "Venue health", exact: true });
    if (chains.ok()) {
      const body = (await chains.json()) as { items: Array<{ chain_id: string }> };
      for (const chain of body.items) {
        await expect(venue.locator(`[data-chain="${chain.chain_id}"]`)).toBeVisible();
      }
    } else {
      const refusal = (await chains.json()) as Problem;
      expect(refusal.code).toBe("UNSUPPORTED");
      expect(overview.chains_configured).toBe(false);
      await expectCode(venue, "UNSUPPORTED");
    }
    await expect(venue).toContainText("The paper venue fills at the latest fresh reference mark");

    // Every subsystem's operator page is linked at a route that exists.
    const links = admin.page.getByRole("region", { name: "Operator pages", exact: true }).locator("a[href]");
    const count = await links.count();
    expect(count).toBeGreaterThanOrEqual(5);
    for (let i = 0; i < count; i = i + 1) {
      const href = (await links.nth(i).getAttribute("href")) ?? "";
      expect(href.startsWith("/advanced/"), href).toBe(true);
    }
    await admin.context.close();
  });

  test("a reference mark is recorded through the page and appears among the asset's marks", async ({ page, browser }) => {
    const admin = await signIn(browser, "admin", true);
    const assetId = await simulatedAssetToMark(page, admin.page);
    test.skip(assetId === undefined, "this deployment offers no SIMULATED asset other than the sandbox dollar to mark");
    const asset = assetId as string;

    await admin.page.goto(CONSOLE);
    await settle(admin.page);
    const form = admin.page.getByRole("form", { name: "Record a reference mark" });
    await form.getByLabel("Asset", { exact: true }).selectOption(asset);
    const source = `e2e-desk-${hex(4)}`;
    await form.getByLabel("Price in sandbox dollars per whole unit", { exact: true }).fill("2500.125");
    await form.getByLabel("Source", { exact: true }).fill(source);
    await form.getByRole("button", { name: "Review: Record the mark", exact: true }).click();
    const [response] = await Promise.all([
      admin.page.waitForResponse((r) => new URL(r.url()).pathname === MARKS_PATH && r.request().method() === "POST"),
      form.getByRole("button", { name: "Confirm: Record the mark", exact: true }).click(),
    ]);
    expect(response.status(), "the mark is recorded").toBe(201);
    const recorded = (await response.json()) as { mark: { id: string; price: string }; duplicate: boolean; value_domain: string };
    expect(recorded.value_domain).toBe("SIMULATED");
    expect(recorded.duplicate).toBe(false);
    expect(recorded.mark.price, "recorded exactly as typed").toBe("2500.125");
    await expect(form.locator(`[data-recorded-mark="${recorded.mark.id}"]`)).toBeVisible();

    const list = (await (await admin.page.request.get(`${MARKS_PATH}?asset_id=${asset}`)).json()) as MarkList;
    expect(list.value_domain).toBe("SIMULATED");
    const mine = list.items.find((item) => item.id === recorded.mark.id);
    expect(mine, "the recorded mark is among the asset's marks").toBeTruthy();
    expect(mine?.source).toBe(source);
    expect(mine?.future).toBe(false);
    expect(mine?.stale).toBe(false);
    const marks = admin.page.getByRole("region", { name: "Reference marks", exact: true });
    await expect(marks.locator(`[data-mark="${recorded.mark.id}"]`)).toBeVisible();

    // An observation in the future is refused, through the same form, and nothing is recorded.
    await form.getByRole("button", { name: "Done", exact: true }).click();
    const future = new Date(Date.now() + 2 * 60 * 60 * 1000);
    await form.getByLabel("Observed at (optional)", { exact: true }).fill(localMinute(future));
    await form.getByRole("button", { name: "Review: Record the mark", exact: true }).click();
    const [refused] = await Promise.all([
      admin.page.waitForResponse((r) => new URL(r.url()).pathname === MARKS_PATH && r.request().method() === "POST"),
      form.getByRole("button", { name: "Confirm: Record the mark", exact: true }).click(),
    ]);
    expect(refused.status(), "a future observation is refused").toBe(400);
    expect(((await refused.json()) as Problem).code).toBe("VALIDATION_FAILED");
    await expectCode(form, "VALIDATION_FAILED");

    // More than 18 fractional digits is refused by the backend as well as the form.
    const tooFine = await command(admin.page, MARKS_PATH, { asset_id: asset, price: `1.${"1".repeat(19)}`, source });
    expect(tooFine.status).toBe(422);
    expect((tooFine.body as Problem).code).toBe("PRECISION_LOSS");
    const zero = await command(admin.page, MARKS_PATH, { asset_id: asset, price: "0", source });
    expect(zero.status, "a price must be positive").toBe(400);
    await admin.context.close();
  });

  test("a mark of an asset that is not SIMULATED is refused", async ({ browser }) => {
    const admin = await signIn(browser, "admin", true);
    const assets = (await (await admin.page.request.get("/v1/assets")).json()) as Asset[];
    const real = assets.find((asset) => asset.kind !== "SIMULATED");
    test.skip(real === undefined, "the registry lists no asset that is not SIMULATED");
    const refused = await command(admin.page, MARKS_PATH, {
      asset_id: (real as Asset).id,
      price: "1.25",
      source: "e2e-not-simulated",
    });
    expect(refused.status, "only SIMULATED assets take a sandbox reference mark").toBe(403);
    expect((refused.body as Problem).code).toBe("FORBIDDEN");
    const unknown = await command(admin.page, MARKS_PATH, { asset_id: unknownId(), price: "1.25", source: "e2e" });
    expect(unknown.status, "an asset the registry does not hold").toBe(404);
    expect((unknown.body as Problem).code).toBe("NOT_FOUND");
    // A malformed id is refused as such before anything is looked up.
    const malformed = await command(admin.page, MARKS_PATH, { asset_id: randomUUID(), price: "1.25", source: "e2e" });
    expect(malformed.status, "an id that is not a UUIDv7").toBe(400);
    expect((malformed.body as Problem).code).toBe("VALIDATION_FAILED");
    await admin.context.close();
  });

  test("the bridge leg actions are refused to a customer, and an unknown leg is the backend's to name", async ({
    page,
    browser,
  }) => {
    const unknown = unknownId();
    const actions = ["resume-relay", "resolve-review", "refund"] as const;
    for (const action of actions) {
      const refused = await command(page, `/v1/advanced/bridge/legs/${unknown}/${action}`, { reason: "a customer may not act on a leg" });
      expect(refused.status, `${action}: a customer holds no sandbox:operate`).toBe(403);
      expect((refused.body as Problem).code).toBe("FORBIDDEN");
    }
    const admin = await signIn(browser, "admin", true);
    for (const action of actions) {
      const answer = await command(admin.page, `/v1/advanced/bridge/legs/${unknown}/${action}`, {
        reason: "end-to-end check of an unknown leg",
      });
      // Where the bridge is wired the leg is unknown; where it is not, the
      // deployment says the bridge is UNSUPPORTED. Either is the backend's
      // answer, and nothing else is.
      expect([404, 422], `${action}: ${String(answer.status)}`).toContain(answer.status);
      expect((answer.body as Problem).code).toBe(answer.status === 404 ? "NOT_FOUND" : "UNSUPPORTED");
    }
    // A reason is required: an empty one never reaches the service.
    const reasonless = await command(admin.page, `/v1/advanced/bridge/legs/${unknown}/refund`, { reason: "" });
    expect([400, 422]).toContain(reasonless.status);
    await admin.context.close();
  });

  test("no dead controls on the console", async ({ browser }) => {
    const admin = await signIn(browser, "admin", true);
    await admin.page.goto(CONSOLE);
    await settle(admin.page);
    const buttons = admin.page.locator("main button");
    const count = await buttons.count();
    for (let i = 0; i < count; i = i + 1) {
      const button = buttons.nth(i);
      if (!(await button.isDisabled())) continue;
      const describedBy = await button.getAttribute("aria-describedby");
      expect(describedBy, "a disabled button explains itself").toBeTruthy();
      const reason = admin.page.locator(`[id="${(describedBy as string).replace(/"/g, '\\"')}"]`);
      await expect(reason).toBeVisible();
    }
    const text = await admin.page.locator("main").innerText();
    expect(text, "the console states sandbox prices, never a dollar figure").not.toMatch(/\$\s?[0-9]/);
    expect(text).not.toMatch(/\bcash\b/i);
    await admin.context.close();
  });
});
