/**
 * The derivatives section of the advanced (sandbox) area against the real API
 * (internal/derivatives, ADR-0020).
 *
 * Nothing is stubbed: the pages talk to a running api over a seeded database,
 * and every assertion compares what a page shows with what the backend
 * answered — including where it answers no. The seed may or may not list a
 * derivative contract; each test states what it expects in either case and
 * never invents the missing half.
 *
 *   - every page renders under the simulated-value notice;
 *   - every derivatives answer is value domain SIMULATED, figures are exact
 *     strings, and a contract carries the mark it says it has;
 *   - an account's positions are its own: a foreign account is FORBIDDEN and
 *     an absent position NOT_FOUND, on the wire and on the page;
 *   - commands are refused with their stable codes (malformed, foreign,
 *     operator-only), and an open the backend refuses is shown with the very
 *     code the backend sent, linked to the intent it recorded;
 *   - no money-shaped figure appears that the API did not send.
 */
import { randomUUID } from "node:crypto";

import { expect, test, type Page, type Response } from "@playwright/test";

const NOTICE = "Simulated value — sandbox only";

const PAGES: ReadonlyArray<{ readonly path: string; readonly heading: string }> = [
  { path: "/advanced/derivatives", heading: "Derivatives" },
  { path: "/advanced/derivatives/positions", heading: "Derivative positions" },
  { path: "/advanced/derivatives/flows", heading: "Flows and funding" },
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

interface ContractRow {
  readonly id: string;
  readonly kind: string;
  readonly marked: boolean;
  readonly settled: boolean;
  readonly latest_mark?: { readonly mark_mantissa: string; readonly price_scale: number };
  readonly settlement_asset: { readonly symbol: string; readonly decimals: number };
  readonly terms: { readonly symbol: string };
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

/** A wire USD string as the Money components render it. */
function renderedUsd(wire: string): string {
  const negative = wire.startsWith("-");
  const [whole = "", cents = ""] = (negative ? wire.slice(1) : wire).split(".");
  const digits = whole.replace(/^0+(?=[0-9])/, "");
  const grouped = digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const zero = /^0+$/.test(digits) && /^0+$/.test(cents);
  return `${negative && !zero ? "-" : ""}$${grouped}.${cents}`;
}

function collectUsd(value: unknown, into: Set<string>): void {
  if (typeof value === "string") {
    if (/^-?[0-9]+\.[0-9]{2}$/.test(value)) into.add(value);
  } else if (Array.isArray(value)) {
    for (const item of value) collectUsd(item, into);
  } else if (typeof value === "object" && value !== null) {
    for (const item of Object.values(value)) collectUsd(item, into);
  }
}

test.describe("the derivatives section", () => {
  for (const { path, heading } of PAGES) {
    test(`${heading} renders under the simulated-value notice`, async ({ page }) => {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1, name: heading, exact: true })).toBeVisible();
      const notice = page.getByRole("note", { name: NOTICE, exact: true });
      await expect(notice).toBeVisible();
      await expect(notice).toContainText("LIVE is not implemented in this build");
      await expect(page.getByRole("navigation", { name: "Derivatives pages", exact: true })).toBeVisible();
      await settle(page);
      await expect(page.locator(".malformed")).toHaveCount(0);
      await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
    });
  }

  test("is a section of the advanced area", async ({ page }) => {
    await page.goto("/advanced/overview");
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Derivatives", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Derivatives", exact: true })).toBeVisible();
  });
});

test("contracts are listed in the simulated domain and drawn as the backend sent them", async ({ page }) => {
  const response = await page.request.get("/v1/advanced/derivatives/contracts?limit=200");
  expect(response.status(), "the contract list is served").toBe(200);
  const list = (await response.json()) as { value_domain: string; items: ContractRow[] };
  expect(list.value_domain).toBe("SIMULATED");
  for (const item of list.items) {
    expect(["FUTURE", "PERPETUAL", "OPTION"]).toContain(item.kind);
    expect(item.marked, `${item.terms.symbol} carries the mark it says it has`).toBe(item.latest_mark !== undefined);
    expect(Number.isInteger(item.settlement_asset.decimals)).toBe(true);
    if (item.latest_mark !== undefined) expect(item.latest_mark.mark_mantissa).toMatch(/^[0-9]+$/);
  }

  const policy = await page.request.get("/v1/advanced/derivatives/policy");
  expect(policy.status()).toBe(200);
  const p = (await policy.json()) as {
    version: string;
    hash: string;
    paper_slippage_bps: number;
    max_mark_lag_ms: number;
    settlement_override_max_bps: number;
  };
  expect(p.version).not.toBe("");
  expect(p.hash).toMatch(/^[0-9a-f]{64}$/);
  expect(Number.isInteger(p.paper_slippage_bps)).toBe(true);
  expect(Number.isInteger(p.max_mark_lag_ms) && p.max_mark_lag_ms > 0, "marks are recorded within a stated lag").toBe(true);
  expect(p.settlement_override_max_bps > 0 && p.settlement_override_max_bps <= 10000, "an override is bounded").toBe(true);

  await page.goto("/advanced/derivatives");
  await settle(page);
  if (list.items.length === 0) {
    await expect(page.getByText("No derivative contracts", { exact: true })).toBeVisible();
  } else {
    const table = page.getByRole("table", { name: "Derivative contracts", exact: true });
    await expect(table.locator("tbody tr")).toHaveCount(list.items.length);
    for (const item of list.items) {
      await expect(table.getByRole("link", { name: item.terms.symbol, exact: true })).toBeVisible();
    }
  }
  await expect(page.getByRole("region", { name: "Policy", exact: true })).toContainText(p.version);
});

test("an account's positions are its own; another account's are refused and an absent one is not found", async ({ page }) => {
  const own = await accountId(page);
  const mine = await page.request.get(`/v1/advanced/derivatives/positions?account_id=${own}`);
  expect(mine.status()).toBe(200);
  const body = (await mine.json()) as {
    value_domain: string;
    book_status: string;
    book_refusal?: { code: string };
    items: unknown[];
    next_cursor: string | null;
  };
  expect(body.value_domain).toBe("SIMULATED");
  expect(["VALUED", "REFUSED"]).toContain(body.book_status);
  if (body.book_status === "REFUSED") expect(body.book_refusal?.code ?? "").not.toBe("");
  expect("next_cursor" in body, "the cursor is present, even when null").toBe(true);

  const foreign = await page.request.get(`/v1/advanced/derivatives/positions?account_id=${FOREIGN_ACCOUNT}`);
  expect(foreign.status()).toBe(403);
  expect(((await foreign.json()) as Problem).code).toBe("FORBIDDEN");
  for (const path of ["flows", "funding-accruals"]) {
    const refused = await page.request.get(`/v1/advanced/derivatives/${path}?account_id=${FOREIGN_ACCOUNT}`);
    expect(refused.status(), path).toBe(403);
  }

  const absent = await page.request.get(`/v1/advanced/derivatives/positions/${ABSENT}`);
  expect(absent.status()).toBe(404);
  expect(((await absent.json()) as Problem).code).toBe("NOT_FOUND");

  // The page says what the backend said: the book, or the refusal with its code.
  await page.goto("/advanced/derivatives/positions");
  await settle(page);
  const bookPanel = page.getByRole("region", { name: "The open book", exact: true });
  if (body.book_status === "REFUSED") {
    await expect(bookPanel.locator(".explain-meta")).toContainText(`code ${body.book_refusal?.code ?? ""}`);
  } else {
    await expect(bookPanel).toContainText("Gross notional");
  }

  await page.goto(`/advanced/derivatives/positions/${ABSENT}`);
  await settle(page);
  await expect(page.locator(".explain-meta").first()).toContainText("code NOT_FOUND");
});

test("commands are refused with their stable codes", async ({ page }) => {
  const own = await accountId(page);
  await page.goto("/advanced/derivatives");
  await settle(page);

  const open = { account_id: own, instrument_id: ABSENT, side: "LONG", quantity: "1", mode: "PAPER" };
  await expectCode(await command(page, "/advanced/derivatives/positions", { ...open, quantity: "1.5" }), 400, "VALIDATION_FAILED");
  await expectCode(await command(page, "/advanced/derivatives/positions", { ...open, side: "UP" }), 400, "VALIDATION_FAILED");
  await expectCode(
    await command(page, "/advanced/derivatives/positions", { ...open, account_id: FOREIGN_ACCOUNT }),
    403,
    "FORBIDDEN",
  );
  await expectCode(await command(page, "/advanced/derivatives/positions", open), 404, "NOT_FOUND");
  await expectCode(await command(page, `/advanced/derivatives/positions/${ABSENT}/close`, { mode: "PAPER" }), 404, "NOT_FOUND");
  await expectCode(await command(page, `/advanced/derivatives/positions/${ABSENT}/exercise`, { mode: "PAPER" }), 404, "NOT_FOUND");

  // The operator side is not a customer's: refused before anything is read.
  await expectCode(
    await command(page, `/advanced/derivatives/contracts/${ABSENT}/marks`, { mark: "100", source: "e2e" }),
    403,
    "FORBIDDEN",
  );
  await expectCode(await command(page, "/advanced/derivatives/clearing-house/reconciliations", {}), 403, "FORBIDDEN");
  await expectCode(
    await command(page, `/advanced/derivatives/contracts/${ABSENT}/variation-margin-runs`, { interval_start: "2026-10-07T08:00:00Z" }),
    403,
    "FORBIDDEN",
  );

  const analytics = await page.request.get(`/v1/advanced/derivatives/contracts/${ABSENT}/analytics`);
  expect(analytics.status(), "the model needs a volatility").toBe(400);
  const contract = await page.request.get(`/v1/advanced/derivatives/contracts/${ABSENT}`);
  expect(contract.status()).toBe(404);
});

test("an open is confirmed once, and the page shows exactly what the backend answered", async ({ page }) => {
  const list = (await (await page.request.get("/v1/advanced/derivatives/contracts?limit=200")).json()) as {
    items: ContractRow[];
  };
  const tradable = list.items.find((item) => !item.settled);
  test.skip(tradable === undefined, "the seed lists no unsettled derivative contract to open a position in");
  const contract = tradable as ContractRow;

  await page.goto(`/advanced/derivatives/contracts/${contract.id}`);
  await settle(page);
  await expect(page.getByRole("heading", { level: 1, name: contract.terms.symbol, exact: true })).toBeVisible();
  if (!contract.marked) {
    await expect(page.getByText("Never marked", { exact: true })).toBeVisible();
  }

  const panel = page.getByRole("region", { name: "Open a position", exact: true });
  const confirm = panel.getByRole("button", { name: /^Confirm: open/ });
  await expect(confirm).toBeDisabled();
  await expect(panel).toContainText("created when you confirm");

  await panel.getByLabel("Contracts", { exact: true }).fill("1");
  await expect(confirm).toBeEnabled();
  await expect(panel).toContainText("created when you confirm");

  const answered: Promise<Response> = page.waitForResponse(
    (response) => response.request().method() === "POST" && new URL(response.url()).pathname === "/v1/advanced/derivatives/positions",
  );
  await confirm.click();
  const response = await answered;
  const sent = response.request().headers()["idempotency-key"] ?? "";
  expect(sent, "the command carried the key created at confirmation").toMatch(/^[0-9a-f-]{36}$/);
  await expect(panel).toContainText(sent);

  const answer: unknown = await response.json();
  if (response.ok()) {
    const trade = answer as { value_domain: string; intent: { id: string } };
    expect(trade.value_domain).toBe("SIMULATED");
    await expect(panel.locator(".callout")).toContainText(trade.intent.id);
  } else {
    expect(isProblem(answer)).toBe(true);
    const problem = answer as Problem;
    await expect(panel.locator(".explain-meta")).toContainText(`code ${problem.code}`);
    const intentId = problem.fields?.["intent_id"];
    if (typeof intentId === "string") {
      // A refusal the backend recorded links to the intent, which stays readable.
      await expect(panel.getByRole("link", { name: intentId })).toBeVisible();
      const intent = await page.request.get(`/v1/advanced/intents/${intentId}`);
      expect(intent.status()).toBe(200);
      expect(((await intent.json()) as { status: string }).status).toBe("REJECTED");
    }
  }
});

test("a dated contract states its settlement rule, and settling or overriding it is an operator's act", async ({ page }) => {
  const list = (await (await page.request.get("/v1/advanced/derivatives/contracts?limit=200")).json()) as {
    items: ContractRow[];
  };
  for (const item of list.items.filter((row) => row.kind !== "PERPETUAL")) {
    const one = await page.request.get(`/v1/advanced/derivatives/contracts/${item.id}`);
    expect(one.status()).toBe(200);
    const c = (await one.json()) as {
      settlement_preview?: { rule_statement: string; expired: boolean; settleable: boolean; has_reference: boolean };
      settlement_overrides?: unknown[];
    };
    expect(c.settlement_preview?.rule_statement ?? "", `${item.terms.symbol} states the rule`).toContain("EXPIRY_MARK v1");
    expect(Array.isArray(c.settlement_overrides), `${item.terms.symbol} lists its override proposals`).toBe(true);
    if (c.settlement_preview?.expired === false) {
      expect(c.settlement_preview.settleable, "nothing settles before expiry").toBe(false);
      expect(c.settlement_preview.has_reference, "the reference is read at expiry").toBe(false);
    }
    await page.goto(`/advanced/derivatives/contracts/${item.id}`);
    await settle(page);
    await expect(page.getByRole("region", { name: "Settlement rule", exact: true })).toContainText("EXPIRY_MARK v1");
  }

  await page.goto("/advanced/derivatives");
  await settle(page);
  const path = `/advanced/derivatives/contracts/${ABSENT}/settlement`;
  for (const body of [
    {},
    { method: "RULE", price: "65000.00" },
    {
      method: "PROPOSE_OVERRIDE",
      price: "65000.00",
      reason: "e2e",
      evidence_ref: "e2e",
      evidence_sha256: "0".repeat(64),
    },
    { method: "APPROVE_OVERRIDE", override_id: ABSENT, reason: "e2e" },
    { method: "REJECT_OVERRIDE", override_id: ABSENT, reason: "e2e" },
  ]) {
    await expectCode(await command(page, path, body), 403, "FORBIDDEN");
  }
});

test("the flow register and funding are read for the account and drawn as sent", async ({ page }) => {
  const own = await accountId(page);
  const flows = await page.request.get(`/v1/advanced/derivatives/flows?account_id=${own}`);
  expect(flows.status()).toBe(200);
  const flowPage = (await flows.json()) as { value_domain: string; items: Array<{ amount: string }> };
  expect(flowPage.value_domain).toBe("SIMULATED");
  for (const flow of flowPage.items) expect(flow.amount).toMatch(/^-?[0-9]+$/);
  const accruals = await page.request.get(`/v1/advanced/derivatives/funding-accruals?account_id=${own}`);
  expect(accruals.status()).toBe(200);

  await page.goto("/advanced/derivatives/flows");
  await settle(page);
  const register = page.getByRole("region", { name: "Flow register", exact: true });
  if (flowPage.items.length === 0) {
    await expect(register.getByText("No flows", { exact: true })).toBeVisible();
  } else {
    await expect(register.locator("tbody tr")).toHaveCount(flowPage.items.length);
  }
});

test("no money-shaped figure appears on the positions page that the API did not send", async ({ page }) => {
  const sent = new Set<string>();
  const pending: Array<Promise<void>> = [];
  page.on("response", (response) => {
    if (!new URL(response.url()).pathname.startsWith("/v1/advanced/derivatives/")) return;
    pending.push(
      response.json().then(
        (body: unknown) => {
          collectUsd(body, sent);
        },
        () => undefined,
      ),
    );
  });
  await page.goto("/advanced/derivatives/positions");
  await settle(page);
  await Promise.all(pending);
  const allowed = new Set([...sent].map(renderedUsd));
  const text = await page.locator("main").innerText();
  for (const figure of text.match(/-?\$[0-9][0-9,]*\.[0-9]{2}/g) ?? []) {
    expect(allowed.has(figure), `${figure} was not sent by the API`).toBe(true);
  }
});
