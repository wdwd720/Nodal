/**
 * The agent authority section of the advanced area against the real API:
 * delegated agent signing and delegated agent withdrawals, as the account's
 * owner and an operator see and steer them.
 *
 * Nothing is stubbed. Every assertion compares what a page shows with what
 * the backend answered, including where it answers no -- and an agent never
 * appears here at all, because no route of the API admits one: its requests
 * are admitted inside the platform. What this suite holds the section to:
 *
 *   - a deployment without a sandbox chain answers UNSUPPORTED everywhere,
 *     and the pages say so with the code instead of drawing empty tables;
 *   - every list shows exactly the rows the API returned, or its refusal;
 *   - a grant, a destination and a policy version need a recent strong
 *     sign-in: without one the backend's STEP_UP_REQUIRED is on screen, and a
 *     retry of the same attempt sends the same idempotency key;
 *   - a grant to an agent that is not the account's is refused by the backend
 *     and the refusal is shown with its code;
 *   - the operator's view is refused to a customer, with the code on screen,
 *     and an operator reads venues, co-signers and what is pending;
 *   - an unknown request id is answered NOT_FOUND, and no control is dead.
 *
 * Customer-a is the session the suite signed in with (no MFA). Customer-a with
 * MFA and the admin sign in here, in contexts of their own.
 */
import { randomBytes, randomUUID } from "node:crypto";

import { expect, test, type Browser, type BrowserContext, type Locator, type Page, type Response } from "@playwright/test";

import { signIn } from "./sign-in.ts";

const NOTICE = "Simulated value — sandbox only";
const SECTION = "/advanced/agent-authority";

const PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: SECTION, heading: "Agent authority" },
  { path: `${SECTION}/destinations`, heading: "Destinations and withdrawal policy" },
  { path: `${SECTION}/operator`, heading: "Delegated authority — operator view" },
];

interface Problem {
  readonly code: string;
  readonly detail?: string;
  readonly fields?: Record<string, unknown>;
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

/** A fresh UUIDv7-shaped id that names nothing. */
function unknownId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
}

const BASE58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
const BASE58_DIGITS = new Map(BASE58.split("").map((digit, index) => [BigInt(index), digit] as const));

/** A fresh simulated-chain address: 32 random bytes in base58, exactly as the backend parses one. */
function freshAddress(): string {
  const bytes = randomBytes(32);
  let value = 0n;
  for (const byte of bytes) value = value * 256n + BigInt(byte);
  let out = "";
  while (value > 0n) {
    out = `${BASE58_DIGITS.get(value % 58n) ?? ""}${out}`;
    value = value / 58n;
  }
  for (const byte of bytes) {
    if (byte !== 0) break;
    out = `1${out}`;
  }
  return out;
}

/** Waits until a page has its answer on screen and nothing says it is still loading. */
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

/**
 * Customer-a's strong session, which this file's tests share. Signing in is
 * rate limited -- CP_API_RATE_LIMIT_AUTH allows 30 requests a minute from one
 * address by default, and a sign-in is four of them -- and the suite signs in
 * far more often than a person would, so customer-a signs in with MFA once for
 * the file. A strong sign-in stays recent far longer than this file takes.
 */
let strongSession: { context: BrowserContext; page: Page } | undefined;

async function strongCustomer(browser: Browser): Promise<{ context: BrowserContext; page: Page }> {
  strongSession ??= await signIn(browser, "customer-a", true);
  return strongSession;
}

test.afterAll(async () => {
  await strongSession?.context.close();
  strongSession = undefined;
});

/** The response to the POST an action makes, observed as the page received it. */
async function answerTo(page: Page, path: string, act: () => Promise<void>): Promise<Response> {
  const [response] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === path && r.request().method() === "POST"),
    act(),
  ]);
  return response;
}

/** A refusal is on screen with the backend's own code. */
async function expectCode(scope: Locator, code: string): Promise<void> {
  await expect(scope.locator(".explain-meta").first()).toContainText(`code ${code}`);
}

/**
 * Whether this deployment runs delegated authority at all: it needs a sandbox
 * chain, and without one the backend answers UNSUPPORTED on every route.
 */
async function delegationRuns(page: Page): Promise<boolean> {
  const response = await page.request.get("/v1/advanced/delegation/venues");
  if (response.status() === 422) {
    const problem = (await response.json()) as Problem;
    expect(problem.code, "an unwired deployment says so with a stable code").toBe("UNSUPPORTED");
    return false;
  }
  expect(response.ok(), "the venue list is readable by any signed-in customer").toBeTruthy();
  return true;
}

/** A venue this deployment co-signs for, when one is registered: the natural first destination of a grant. */
async function aVenueAddress(page: Page): Promise<string | undefined> {
  const response = await page.request.get("/v1/advanced/delegation/venues");
  if (!response.ok()) return undefined;
  const list = (await response.json()) as { items: Array<{ address: string; co_signer: boolean }> };
  return list.items.find((venue) => venue.co_signer)?.address;
}

/** Fills the grant form for an agent id; the rest of the terms are a modest sandbox grant. */
async function fillGrant(page: Page, agentId: string, destination: string): Promise<Locator> {
  const form = page.getByRole("form", { name: "Grant a capability" });
  await form.getByLabel("Agent id", { exact: true }).fill(agentId);
  await form.getByLabel("Single request at most (sandbox dollars)", { exact: true }).fill("50.00");
  await form.getByLabel("Per day at most (sandbox dollars)", { exact: true }).fill("100.00");
  await form.getByLabel("In all at most (sandbox dollars)", { exact: true }).fill("500.00");
  await form.getByLabel("Destinations", { exact: true }).fill(destination);
  await form.getByRole("button", { name: "Review the grant", exact: true }).click();
  return page.getByRole("group", { name: "Review the grant" });
}

test.describe("the agent authority section", () => {
  for (const { path, heading } of PAGES) {
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
      .getByRole("link", { name: "Agent authority", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Agent authority", exact: true })).toBeVisible();
    await page
      .getByRole("navigation", { name: "Agent authority pages", exact: true })
      .getByRole("link", { name: "Destinations and withdrawal policy", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Destinations and withdrawal policy", exact: true })).toBeVisible();
  });
});

test("an unwired deployment says UNSUPPORTED instead of drawing empty tables", async ({ page }) => {
  test.skip(await delegationRuns(page), "this deployment runs delegated authority; the wired case is covered below");
  await page.goto(SECTION);
  await settle(page);
  await expectCode(page.getByRole("region", { name: "Capabilities", exact: true }), "UNSUPPORTED");
  await expectCode(page.getByRole("region", { name: "Signing requests", exact: true }), "UNSUPPORTED");
  await expectCode(page.getByRole("region", { name: "Withdrawals", exact: true }), "UNSUPPORTED");
});

test("the lists show exactly the rows the API returned", async ({ page }) => {
  test.skip(!(await delegationRuns(page)), "this deployment does not run delegated authority");
  const id = await ownAccount(page);
  await page.goto(SECTION);
  await settle(page);
  for (const { path, panel, empty, attribute } of [
    { path: "capabilities", panel: "Capabilities", empty: "No capabilities", attribute: "data-capability-id" },
    { path: "swaps", panel: "Signing requests", empty: "No signing requests", attribute: "data-swap-id" },
    { path: "withdrawals", panel: "Withdrawals", empty: "No withdrawals", attribute: "data-withdrawal-id" },
  ]) {
    const response = await page.request.get(`/v1/advanced/delegation/${path}?account_id=${id}&limit=25`);
    const body: unknown = await response.json();
    const region = page.getByRole("region", { name: panel, exact: true });
    if (isProblem(body)) {
      await expectCode(region, body.code);
      continue;
    }
    const rows = body as { value_domain: string; items: Array<{ id: string }> };
    expect(rows.value_domain).toBe("SIMULATED");
    if (rows.items.length === 0) {
      await expect(region).toContainText(empty);
      continue;
    }
    for (const item of rows.items) {
      await expect(region.locator(`[${attribute}="${item.id}"]`)).toBeVisible();
    }
  }
});

test("a grant without a recent strong sign-in is refused, and a retry sends the same key", async ({ page }) => {
  test.skip(!(await delegationRuns(page)), "this deployment does not run delegated authority");
  await page.goto(SECTION);
  await settle(page);
  const review = await fillGrant(page, unknownId(), (await aVenueAddress(page)) ?? freshAddress());
  const first = await answerTo(page, "/v1/advanced/delegation/capabilities", () =>
    review.getByRole("button", { name: "Confirm and grant", exact: true }).click(),
  );
  expect(first.status(), "the default session signed in without MFA").toBe(403);
  expect(((await first.json()) as Problem).code).toBe("STEP_UP_REQUIRED");
  await expectCode(review, "STEP_UP_REQUIRED");
  const key = first.request().headers()["idempotency-key"];
  expect(key, "the key was created at confirmation").toBeTruthy();

  // Confirming the same grant again is a retry of the same attempt.
  const retry = await answerTo(page, "/v1/advanced/delegation/capabilities", () =>
    review.getByRole("button", { name: "Confirm and grant", exact: true }).click(),
  );
  expect(retry.request().headers()["idempotency-key"]).toBe(key);
  expect(retry.status()).toBe(403);
});

test("a grant to an agent that is not the account's is refused by the backend, with its code", async ({ browser, page }) => {
  test.skip(!(await delegationRuns(page)), "this deployment does not run delegated authority");
  const strong = await strongCustomer(browser);
  await strong.page.goto(SECTION);
  await settle(strong.page);
  const review = await fillGrant(strong.page, unknownId(), (await aVenueAddress(strong.page)) ?? freshAddress());
  const response = await answerTo(strong.page, "/v1/advanced/delegation/capabilities", () =>
    review.getByRole("button", { name: "Confirm and grant", exact: true }).click(),
  );
  expect(response.status(), "an agent of another account, or of nobody, is never granted anything").toBe(403);
  const problem = (await response.json()) as Problem;
  expect(problem.code).toBe("FORBIDDEN");
  await expectCode(review, "FORBIDDEN");
});

test("the owner keeps the allow-list and writes a policy version, each with a strong sign-in", async ({ browser, page }) => {
  test.skip(!(await delegationRuns(page)), "this deployment does not run delegated authority");
  const strong = await strongCustomer(browser);
  const owner = strong.page;
  const id = await ownAccount(owner);
  await owner.goto(`${SECTION}/destinations`);
  await settle(owner);

  // Allow-list a fresh destination.
  const address = freshAddress();
  const label = `e2e ${address.slice(0, 6)}`;
  const add = owner.getByRole("form", { name: "Add a destination" });
  await add.getByLabel("Address", { exact: true }).fill(address);
  await add.getByLabel("Label", { exact: true }).fill(label);
  await add.getByRole("button", { name: "Review the destination", exact: true }).click();
  const added = await answerTo(owner, "/v1/advanced/delegation/destinations", () =>
    add.getByRole("button", { name: "Confirm and add", exact: true }).click(),
  );
  expect(added.status()).toBe(200);
  const destination = (await added.json()) as { id: string; status: string };
  expect(destination.status).toBe("ACTIVE");
  await expect(owner.locator(`[data-added-destination="${destination.id}"]`)).toBeVisible();

  // The list shows it, as the API does; then remove it.
  await owner.reload();
  await settle(owner);
  const row = owner.locator(`[data-destination-id="${destination.id}"]`);
  await expect(row).toContainText("ACTIVE");
  await row.getByRole("button", { name: `Remove ${label}`, exact: true }).click();
  await row.getByLabel("Reason", { exact: true }).fill("end-to-end clean-up");
  await row.getByRole("button", { name: `Remove ${label}`, exact: true }).click();
  const removed = await answerTo(owner, `/v1/advanced/delegation/destinations/${destination.id}/remove`, () =>
    row.getByRole("button", { name: "Confirm removal", exact: true }).click(),
  );
  expect(removed.status()).toBe(200);
  expect(((await removed.json()) as { status: string }).status).toBe("REMOVED");

  // A new policy version, exactly as typed.
  const before = (await (await owner.request.get(`/v1/advanced/delegation/withdrawal-policy?account_id=${id}`)).json()) as {
    current?: { version: number };
  };
  const form = owner.getByRole("form", { name: "Write a new policy version" });
  await form.getByLabel("Per withdrawal at most (sandbox dollars)", { exact: true }).fill("250.00");
  await form.getByLabel("Per day at most (sandbox dollars)", { exact: true }).fill("1000.00");
  await form.getByLabel("Withdrawals per 24 hours at most", { exact: true }).fill("10");
  await form.getByLabel("Highest risk score accepted", { exact: true }).fill("80");
  await form.getByLabel("Automatic approval at most (sandbox dollars)", { exact: true }).fill("0.00");
  await form.getByRole("button", { name: "Review the new version", exact: true }).click();
  const written = await answerTo(owner, "/v1/advanced/delegation/withdrawal-policy", () =>
    form.getByRole("button", { name: "Confirm the new version", exact: true }).click(),
  );
  expect(written.status()).toBe(200);
  const policy = (await written.json()) as { version: number; per_withdrawal_limit_usd: string; value_domain: string };
  expect(policy.version, "versions are never edited; a change is the next version").toBe((before.current?.version ?? 0) + 1);
  expect(policy.per_withdrawal_limit_usd).toBe("250.00");
  expect(policy.value_domain).toBe("SIMULATED");
  await expect(owner.locator(`[data-policy-version="${String(policy.version)}"]`)).toBeVisible();
});

test("a destination without a recent strong sign-in is refused with STEP_UP_REQUIRED", async ({ page }) => {
  test.skip(!(await delegationRuns(page)), "this deployment does not run delegated authority");
  await page.goto(`${SECTION}/destinations`);
  await settle(page);
  const add = page.getByRole("form", { name: "Add a destination" });
  await add.getByLabel("Address", { exact: true }).fill(freshAddress());
  await add.getByLabel("Label", { exact: true }).fill("refused without MFA");
  await add.getByRole("button", { name: "Review the destination", exact: true }).click();
  const response = await answerTo(page, "/v1/advanced/delegation/destinations", () =>
    add.getByRole("button", { name: "Confirm and add", exact: true }).click(),
  );
  expect(response.status()).toBe(403);
  await expectCode(add, "STEP_UP_REQUIRED");
});

test("the operator's view is refused to a customer, with the code on screen", async ({ page }) => {
  const pending = await page.request.get("/v1/advanced/delegation/ops/pending");
  const capabilities = await page.request.get("/v1/advanced/delegation/ops/capabilities");
  expect(pending.status(), "a customer holds no sandbox:operate").toBe(403);
  expect(capabilities.status()).toBe(403);
  await page.goto(`${SECTION}/operator`);
  await settle(page);
  await expectCode(page.getByRole("region", { name: "Not yet at an end", exact: true }), ((await pending.json()) as Problem).code);
  await expectCode(
    page.getByRole("region", { name: "Capabilities across accounts", exact: true }),
    ((await capabilities.json()) as Problem).code,
  );
});

test("an operator reads venues, co-signers and what is pending, and runs the executions that are ready", async ({ browser, page }) => {
  test.skip(!(await delegationRuns(page)), "this deployment does not run delegated authority");
  const operator = await signIn(browser, "admin", true);
  try {
    const op = operator.page;
    await op.goto(`${SECTION}/operator`);
    await settle(op);
    const venues = (await (await op.request.get("/v1/advanced/delegation/venues")).json()) as {
      items: Array<{ name: string }>;
      co_signers: Array<{ name: string; registered: boolean }>;
    };
    const venuePanel = op.getByRole("region", { name: "Venues", exact: true });
    for (const venue of venues.items) {
      await expect(venuePanel.locator(`[data-venue="${venue.name}"]`)).toBeVisible();
    }
    for (const coSigner of venues.co_signers) {
      await expect(venuePanel.locator(`[data-co-signer="${coSigner.name}"]`)).toContainText(coSigner.registered ? "yes" : "no");
    }

    const pending = (await (await op.request.get("/v1/advanced/delegation/ops/pending?limit=100")).json()) as {
      swaps: Array<{ id: string }>;
      withdrawals: Array<{ id: string }>;
      value_domain: string;
    };
    expect(pending.value_domain).toBe("SIMULATED");
    const pendingPanel = op.getByRole("region", { name: "Not yet at an end", exact: true });
    if (pending.swaps.length === 0 && pending.withdrawals.length === 0) {
      await expect(pendingPanel).toContainText("Nothing pending");
    }
    for (const swap of pending.swaps) {
      await expect(pendingPanel.locator(`[data-pending-swap="${swap.id}"]`)).toBeVisible();
    }

    const runPanel = op.getByRole("region", { name: "Run the executions that are ready", exact: true });
    const form = runPanel.getByRole("form", { name: "Run the executions that are ready" });
    await form.getByLabel("At most this many", { exact: true }).fill("5");
    await form.getByRole("button", { name: "Review the run", exact: true }).click();
    const run = await answerTo(op, "/v1/advanced/delegation/ops/execute-pending", () =>
      runPanel.getByRole("button", { name: "Confirm the run", exact: true }).click(),
    );
    expect(run.status()).toBe(200);
    const result = (await run.json()) as { items: unknown[]; value_domain: string };
    expect(result.value_domain).toBe("SIMULATED");
    await expect(runPanel.locator(`[data-run-items="${String(result.items.length)}"]`)).toBeVisible();
  } finally {
    await operator.context.close();
  }
});

test("an unknown capability, signing request or withdrawal is answered NOT_FOUND", async ({ page }) => {
  test.skip(!(await delegationRuns(page)), "this deployment does not run delegated authority");
  for (const { kind, heading } of [
    { kind: "capabilities", heading: "Capability" },
    { kind: "swaps", heading: "Signing request" },
    { kind: "withdrawals", heading: "Withdrawal" },
  ]) {
    const id = unknownId();
    const response = await page.request.get(`/v1/advanced/delegation/${kind}/${id}`);
    expect(response.status()).toBe(404);
    await page.goto(`${SECTION}/${kind}/${id}`);
    await expect(page.getByRole("heading", { level: 1, name: heading, exact: true })).toBeVisible();
    await settle(page);
    await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
    await expect(page.locator(".explain-meta").first()).toContainText("code NOT_FOUND");
  }
});

test("no dead controls in the agent authority section", async ({ page }) => {
  for (const { path } of PAGES) {
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

test("the agent authority pages reflow at 390px without sideways scrolling", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  for (const { path } of PAGES) {
    await page.goto(path);
    await settle(page);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
  }
});
