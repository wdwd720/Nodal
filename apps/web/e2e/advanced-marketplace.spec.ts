/**
 * The strategies section of the advanced area against the real API.
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database,
 * signed in as customer-a, and every assertion compares what a page shows
 * with what the backend answered, including where it answers no. What this
 * suite holds the section to:
 *
 *   - every page renders under the simulated-value notice, and paper and
 *     simulated results are labelled as such;
 *   - the catalogue, installs and runs are exactly the rows the API returned;
 *   - a strategy published through the form is a DRAFT only its creator sees,
 *     submitted with an idempotency key created when the person confirms;
 *   - the creator cannot approve their own strategy, an install in any mode
 *     but PAPER is refused by the service, and a customer is refused the
 *     review queue -- each refusal reaching the client with its code;
 *   - a paper follow sends the account and nothing else: the backend builds
 *     the run's input from the install's paper book and its own prices;
 *   - no money-shaped figure appears that the API did not send, and no
 *     control is dead.
 */
import { randomUUID } from "node:crypto";

import { expect, test, type Page, type Request, type Response } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";

const STRATEGY_PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/advanced/strategies", heading: "Strategies" },
  { path: "/advanced/strategies/new", heading: "Publish a strategy" },
  { path: "/advanced/strategies/installs", heading: "Installs and paper-follow" },
  { path: "/advanced/strategies/runs", heading: "Sandbox runs" },
  { path: "/advanced/strategies/review", heading: "Review queue" },
];

interface Problem {
  readonly code: string;
  readonly detail?: string;
}

function isProblem(body: unknown): body is Problem {
  return typeof body === "object" && body !== null && typeof (body as { code?: unknown }).code === "string";
}

interface ListingBody {
  readonly id: string;
  readonly versions: ReadonlyArray<{ readonly id: string; readonly number: number; readonly status: string }>;
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

/** A UUIDv7-shaped id nobody recorded: the version nibble of a random v4 set to 7. */
function unknownId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
}

/**
 * A command sent the way the application sends one: first-party, with an
 * Idempotency-Key. The backend's answer is returned whatever it is.
 */
async function command(page: Page, path: string, body: unknown): Promise<{ readonly status: number; readonly body: unknown }> {
  const response = await page.request.post(path, {
    data: body,
    headers: { "Idempotency-Key": `e2e-${randomUUID()}`, "Sec-Fetch-Site": "same-origin" },
  });
  return { status: response.status(), body: await response.json().catch(() => undefined) };
}

const USD_WIRE = /^-?[0-9]+\.[0-9]{2}$/;

/** Every string in a JSON body shaped like a two-decimal amount. */
function collectUsd(value: unknown, into: Set<string>): void {
  if (typeof value === "string") {
    if (USD_WIRE.test(value)) into.add(value);
  } else if (Array.isArray(value)) {
    for (const item of value) collectUsd(item, into);
  } else if (typeof value === "object" && value !== null) {
    for (const item of Object.values(value)) collectUsd(item, into);
  }
}

/** A two-decimal wire amount as the Money components render it. */
function renderedUsd(wire: string): string {
  const negative = wire.startsWith("-");
  const [whole = "", cents = ""] = (negative ? wire.slice(1) : wire).split(".");
  const digits = whole.replace(/^0+(?=[0-9])/, "");
  const grouped = digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const zero = /^0+$/.test(digits) && /^0+$/.test(cents);
  return `${negative && !zero ? "-" : ""}$${grouped}.${cents}`;
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

/** The idempotency key a command request carried. */
function keyOf(request: Request): string {
  const key = request.headers()["idempotency-key"];
  expect(key, "every command carries an Idempotency-Key").toBeTruthy();
  return key as string;
}

/** A small momentum strategy against the strategy SDK. */
const STRATEGY_SOURCE = `package main

import (
	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		for _, ins := range s.Instruments() {
			if len(ins.Closes) < 5 {
				continue
			}
			short, err := nodalsdk.SMA(ins.Closes, 3, 6, nodalsdk.RoundHalfEven)
			if err != nil {
				return err
			}
			long, err := nodalsdk.SMA(ins.Closes, 5, 6, nodalsdk.RoundHalfEven)
			if err != nil {
				return err
			}
			if short.Cmp(long) > 0 {
				if err := s.EmitIntent("ACQUIRE_EXPOSURE", ins.ID, "BUY", "1000000", ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
`;

test.describe("the strategies section", () => {
  for (const { path, heading } of STRATEGY_PAGES) {
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
      .getByRole("link", { name: "Strategies", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Strategies", exact: true })).toBeVisible();
    await page.getByRole("link", { name: "Sandbox runs", exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Sandbox runs", exact: true })).toBeVisible();
  });
});

test("the catalogue shows exactly the approved listings the API returned", async ({ page }) => {
  const bodies = await visit(page, "/advanced/strategies");
  const catalog = bodies.get("/v1/advanced/strategies/catalog");
  expect(catalog, "the page asked for the catalogue").toBeTruthy();
  const panel = page.getByRole("region", { name: "Approved strategies", exact: true });
  if (isProblem(catalog)) {
    await expect(panel.locator(".explain-meta")).toContainText(`code ${catalog.code}`);
    return;
  }
  const listings = catalog as { items: Array<{ id: string; title: string; latest_version: { status: string } }> };
  if (listings.items.length === 0) {
    await expect(panel).toContainText("No approved strategies");
    return;
  }
  const table = page.getByRole("table", { name: "Approved strategies on this page", exact: true });
  await expect(table.locator("tbody tr")).toHaveCount(listings.items.length);
  for (const listing of listings.items) {
    expect(listing.latest_version.status, "the catalogue lists approved versions only").toBe("APPROVED");
    await expect(table.getByRole("link", { name: listing.title, exact: true }).first()).toBeVisible();
  }
});

test("a strategy published through the form is a DRAFT its creator submits with a key made at confirmation", async ({ page }) => {
  const id = await accountId(page);
  const title = `e2e momentum ${randomUUID().slice(0, 8)}`;
  const instrument = unknownId();

  await page.goto("/advanced/strategies/new");
  await settle(page);
  await page.getByLabel("Title", { exact: true }).fill(title);
  await page.getByLabel("What the strategy does", { exact: true }).fill("Buys when the short average is above the long one.");
  await page.getByLabel("Source commit", { exact: true }).fill("e2e-momentum-1");
  await page.getByLabel("Source (Go, package main, against the strategy SDK)", { exact: true }).fill(STRATEGY_SOURCE);
  await page.getByRole("checkbox", { name: "ACQUIRE_EXPOSURE", exact: true }).check();
  await page.getByLabel("Manifest: instruments it may trade (ids, one per line)", { exact: true }).fill(instrument);
  await page.getByLabel("Manifest: notional cap per intent, in simulated dollars", { exact: true }).fill("250");
  await page.getByLabel("Manifest: intents per run, at most (1 to 100)", { exact: true }).fill("2");
  await page.getByLabel("Manifest: seconds between runs, at least (60 or more)", { exact: true }).fill("3600");

  const posted = page.waitForRequest(
    (request) => request.method() === "POST" && new URL(request.url()).pathname === "/v1/advanced/strategies/listings",
  );
  const answered = page.waitForResponse(
    (response) => response.request().method() === "POST" && new URL(response.url()).pathname === "/v1/advanced/strategies/listings",
  );
  await page.getByRole("button", { name: "Publish the draft", exact: true }).click();
  keyOf(await posted);
  const created = await answered;
  const createdBody: unknown = await created.json();

  if (!created.ok()) {
    // A deployment that does not run the marketplace says so with its code,
    // and the page shows exactly that.
    expect(isProblem(createdBody)).toBe(true);
    await expect(page.locator(".explain-meta")).toContainText(`code ${(createdBody as Problem).code}`);
    return;
  }
  const listing = createdBody as ListingBody;
  expect(created.status()).toBe(201);
  await expect(page.getByText("Published as a draft", { exact: true })).toBeVisible();
  const version = listing.versions[0];
  expect(version?.status).toBe("DRAFT");

  // Only its creator sees it: the creator's own list has it.
  const own = await page.request.get(`/v1/advanced/strategies/listings?account_id=${id}&limit=200`);
  expect(own.ok()).toBeTruthy();
  const ownBody = (await own.json()) as { items: Array<{ id: string }> };
  expect(ownBody.items.map((item) => item.id)).toContain(listing.id);

  await page.getByRole("link", { name: "Open the listing to submit it", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Strategy listing", exact: true })).toBeVisible();
  await settle(page);
  const versionPanel = page.getByRole("region", { name: "Version 1", exact: true });
  await expect(versionPanel).toContainText("DRAFT");
  await expect(versionPanel).toContainText("250 simulated dollars");

  const submitted = page.waitForResponse(
    (response) => response.request().method() === "POST" && new URL(response.url()).pathname.endsWith("/submit"),
  );
  await versionPanel.getByRole("button", { name: "Submit for review", exact: true }).click();
  const submitAnswer = await submitted;
  keyOf(submitAnswer.request());
  expect(submitAnswer.status(), "the creator submits their own draft").toBe(200);
  // The page refetches the listing and draws the backend's new state.
  await expect(versionPanel.getByText("SUBMITTED", { exact: true }).first()).toBeVisible();
  await expect(versionPanel.getByRole("button", { name: "Submit for review", exact: true })).toHaveCount(0);
  const after = await page.request.get(`/v1/advanced/strategies/listings/${listing.id}`);
  const afterBody = (await after.json()) as ListingBody;
  expect(afterBody.versions[0]?.status).toBe("SUBMITTED");

  // The creator cannot approve it: a customer holds no reviewer permission,
  // and the refusal reaches the client with its code.
  const approve = await command(page, `/v1/advanced/strategies/versions/${version?.id ?? ""}/approve`, {
    reason_code: "SELF_APPROVAL",
  });
  expect(approve.status).toBe(403);
  expect(isProblem(approve.body)).toBe(true);
  expect((approve.body as Problem).code).toBe("FORBIDDEN");
  expect((approve.body as Problem).detail ?? "", "refused by the permission check, not by the CSRF guard").not.toContain(
    "cross-site",
  );

  // Installs are PAPER only: the service refuses LIVE and says so.
  const live = await command(page, "/v1/advanced/strategies/installs", {
    account_id: id,
    version_id: version?.id ?? "",
    mode: "LIVE",
  });
  expect(live.status).toBe(403);
  expect((live.body as Problem).code).toBe("FORBIDDEN");
  expect((live.body as Problem).detail ?? "").toContain("PAPER only");

  // And a version nobody approved cannot be installed even in PAPER.
  const paper = await command(page, "/v1/advanced/strategies/installs", {
    account_id: id,
    version_id: version?.id ?? "",
    mode: "PAPER",
  });
  expect(paper.status).toBe(403);
  expect((paper.body as Problem).detail ?? "").toContain("cannot be installed");

  // The page agrees: no install control works on a version that is not approved.
  await page.reload();
  await settle(page);
  const install = versionPanel.getByRole("button", { name: "Install in PAPER mode", exact: true });
  await expect(install).toBeDisabled();
});

test("a customer is refused the review queue, and the page shows the refusal with its code", async ({ page }) => {
  const response = await page.request.get("/v1/advanced/strategies/review-queue");
  expect(response.status(), "only operators read the review queue").toBe(403);
  const problem = (await response.json()) as Problem;

  await page.goto("/advanced/strategies/review");
  await settle(page);
  await expect(page.locator(".explain-meta")).toContainText(`code ${problem.code}`);
  await expect(page.getByRole("button", { name: "Decide", exact: true })).toHaveCount(0);
});

test("a paper follow carries the account and nothing else: the backend builds the run's input itself", async ({ page }) => {
  const id = await accountId(page);
  // An install this account does not have: the route answers NOT_FOUND for
  // the install, not VALIDATION_FAILED for a missing snapshot -- the request
  // shape is the account alone.
  const follow = await command(page, `/v1/advanced/strategies/installs/${unknownId()}/follows`, { account_id: id });
  expect(follow.status).toBe(404);
  expect(isProblem(follow.body)).toBe(true);
  expect((follow.body as Problem).code).toBe("NOT_FOUND");

  // The installs page offers a follow without asking for any figure.
  await page.goto("/advanced/strategies/installs");
  await settle(page);
  await expect(page.getByLabel("Market and portfolio snapshot (JSON)", { exact: true })).toHaveCount(0);
  const follows = page.getByRole("button", { name: "Paper-follow", exact: true });
  if ((await follows.count()) > 0) {
    await follows.first().click();
    await expect(page.getByRole("button", { name: "Follow now", exact: true })).toBeVisible();
    await expect(page.locator("main textarea")).toHaveCount(0);
  }
});

test("an unknown listing is answered NOT_FOUND", async ({ page }) => {
  await page.goto(`/advanced/strategies/listings/${unknownId()}`);
  await expect(page.getByRole("heading", { level: 1, name: "Strategy listing", exact: true })).toBeVisible();
  await settle(page);
  await expect(page.getByRole("note", { name: NOTICE, exact: true })).toBeVisible();
  await expect(page.locator(".explain-meta")).toContainText("code NOT_FOUND");
});

test("installs and runs show exactly the rows the API returned for the account", async ({ page }) => {
  for (const { path, api, empty, table } of [
    { path: "/advanced/strategies/installs", api: "/v1/advanced/strategies/installs", empty: "No installs", table: "Installs on this page" },
    { path: "/advanced/strategies/runs", api: "/v1/advanced/strategies/runs", empty: "No sandbox runs", table: "Sandbox runs on this page" },
  ]) {
    const bodies = await visit(page, path);
    const body = bodies.get(api);
    expect(body, `the page asked for ${api}`).toBeTruthy();
    if (isProblem(body)) {
      await expect(page.locator(".explain-meta").first()).toContainText(`code ${body.code}`);
      continue;
    }
    const rows = body as { items: Array<{ id: string }> };
    if (rows.items.length === 0) {
      await expect(page.locator("main")).toContainText(empty);
      continue;
    }
    await expect(page.getByRole("table", { name: table, exact: true }).locator("tbody tr")).toHaveCount(rows.items.length);
  }

  const history = (await visit(page, "/advanced/strategies/installs")).get("/v1/advanced/strategies/follows");
  expect(history, "the installs page reads the paper-follow history").toBeTruthy();
  if (!isProblem(history)) {
    expect((history as { value_domain: string }).value_domain).toBe("SIMULATED");
  }
});

for (const { path } of STRATEGY_PAGES) {
  test(`${path} shows no money-shaped figure the API did not return`, async ({ page }) => {
    const bodies = await visit(page, path);
    const wire = new Set<string>();
    for (const body of bodies.values()) collectUsd(body, wire);
    const allowed = new Set([...wire].map(renderedUsd));
    const text = await page.locator("main").innerText();
    for (const figure of text.match(/-?\$[0-9][0-9,]*\.[0-9]{2}/g) ?? []) {
      expect(allowed.has(figure), `${path} shows ${figure}, which no response contained`).toBe(true);
    }
    expect(text, "the page never uses the banned word for a stablecoin").not.toMatch(/\bcash\b/i);
  });
}

test("no dead controls in the strategies section", async ({ page }) => {
  for (const { path } of STRATEGY_PAGES) {
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

test("the strategies pages reflow at 390px without sideways scrolling", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  for (const { path } of STRATEGY_PAGES) {
    await page.goto(path);
    await settle(page);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, `${path} does not scroll horizontally at 390px`).toBeLessThanOrEqual(1);
  }
});
