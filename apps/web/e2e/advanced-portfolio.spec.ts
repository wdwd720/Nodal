/**
 * The portfolio page of the advanced (sandbox) area against the real API.
 *
 * Nothing is stubbed. The suite's own session is customer-a; another
 * customer and an operator sign in here through the real OIDC flow. Every
 * assertion compares what the page shows with what the backend answered for
 * the same account, including where it answers no:
 *
 *   - /advanced opens on the portfolio, under the simulated-value notice, and
 *     the overview is still one link away;
 *   - every balance the backend reports is a row, its wallet, available and
 *     total quantities exactly as the backend counted them and its valuation
 *     at par, at a mark, or unvalued with the reason -- after the faucet has
 *     put sandbox dollars in the wallet when this deployment runs one;
 *   - every section shows the state the backend reported: its answer when
 *     PRESENT, "not configured" when the subsystem does not run here, the
 *     refusal's code when its read refused -- never a zero in its place;
 *   - no money-shaped figure appears that the API did not send;
 *   - another customer's account and an account that does not exist are
 *     refused alike; an operator inspecting an absent account sees NOT_FOUND.
 */
import { randomUUID } from "node:crypto";

import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Browser, type BrowserContext, type Locator, type Page, type Response } from "@playwright/test";

import { goto } from "./pace.ts";
import { signIn } from "./sign-in.ts";

const NOTICE = "Simulated value — sandbox only";
const PAGE = "/advanced/portfolio";
const API = "/v1/advanced/portfolio";

interface Problem {
  readonly code: string;
  readonly detail?: string;
}

interface Valuation {
  readonly status: string;
  readonly value?: string;
  readonly problem?: Problem;
}

interface Balance {
  readonly asset_id: string;
  readonly symbol: string;
  readonly decimals: number;
  readonly wallet: string;
  readonly reserved: string;
  readonly available: string;
  readonly total: string;
  readonly withdrawal_holds: string;
  readonly reservations: ReadonlyArray<{ readonly purpose: string; readonly quantity: string }>;
  readonly held_elsewhere: ReadonlyArray<{ readonly code: string; readonly quantity: string }>;
  readonly valuation: Valuation;
}

interface Section {
  readonly state: string;
  readonly problem?: Problem;
}

interface Portfolio {
  readonly account_id: string;
  readonly value_domain: string;
  readonly quote_symbol: string;
  readonly quote_decimals: number;
  readonly balances: readonly Balance[];
  readonly buying_power: Section & { readonly buying_power?: Record<string, unknown> };
  readonly positions: {
    readonly margin: Section;
    readonly derivatives: Section;
    readonly prediction_markets: Section & {
      readonly markets: ReadonlyArray<{ readonly positions: { readonly held_payout?: { readonly claimed: boolean } } }>;
    };
    readonly equities: Section;
    readonly order_books: Section;
  };
  readonly delegation: Section;
  readonly performance_fees: Section;
  readonly risk: { readonly decisions: Section };
}

/** The panels of the page, by the section of the answer each one shows. */
const SECTIONS: ReadonlyArray<{ readonly title: string; readonly subsystem: string; readonly pick: (p: Portfolio) => Section }> = [
  { title: "Buying power and equity", subsystem: "Buying power V2", pick: (p) => p.buying_power },
  { title: "Margin account", subsystem: "Margin account", pick: (p) => p.positions.margin },
  { title: "Derivative positions", subsystem: "Derivatives", pick: (p) => p.positions.derivatives },
  { title: "Prediction market shares", subsystem: "Prediction markets", pick: (p) => p.positions.prediction_markets },
  { title: "Equities", subsystem: "Equities", pick: (p) => p.positions.equities },
  { title: "Resting orders", subsystem: "Order books", pick: (p) => p.positions.order_books },
  { title: "Delegated authority", subsystem: "Agent authority", pick: (p) => p.delegation },
  { title: "Performance fees", subsystem: "Performance fees", pick: (p) => p.performance_fees },
];

/** The buying power figures the summary shows, each rendered with data-field. */
const BUYING_POWER_FIELDS = [
  "trade_buying_power",
  "risk_increasing_buying_power",
  "margin_available",
  "withdrawable",
  "gross_equity",
  "eligible_equity",
  "borrow_liabilities",
  "maintenance_margin",
] as const;

/** The pages each section links to, as the area routes them. */
const SUMMARISED_PAGES = [
  "/advanced/bridge",
  "/advanced/buying-power",
  "/advanced/margin",
  "/advanced/derivatives/positions",
  "/advanced/prediction-markets",
  "/advanced/equities/settlements",
  "/advanced/order-books/orders",
  "/advanced/agent-authority",
  "/advanced/fees/periods",
  "/advanced/activity",
] as const;

/** Exact base units as the Money components render them: the decimal point moved, grouped, trailing zeros dropped. */
function renderedQuantity(baseUnits: string, decimals: number): string {
  const negative = baseUnits.startsWith("-");
  const digits = negative ? baseUnits.slice(1) : baseUnits;
  const padded = digits.padStart(decimals + 1, "0");
  const whole = padded.slice(0, padded.length - decimals).replace(/^0+(?=[0-9])/, "");
  const fraction = padded.slice(padded.length - decimals).replace(/0+$/, "");
  const grouped = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const zero = /^0+$/.test(digits);
  return `${negative && !zero ? "-" : ""}${grouped}${fraction === "" ? "" : `.${fraction}`}`;
}

/** A wire USD string as the Money components render it: grouping and a sign, nothing else. */
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

async function portfolioOf(page: Page, accountId: string): Promise<{ readonly status: number; readonly body: unknown }> {
  const response = await page.request.get(`${API}?account_id=${accountId}`);
  return { status: response.status(), body: (await response.json()) as unknown };
}

/** A command as the browser would send it: same-origin, JSON, with a fresh idempotency key. */
async function command(page: Page, path: string, body: unknown): Promise<number> {
  const response = await page.request.post(path, {
    headers: { "Content-Type": "application/json", "Idempotency-Key": randomUUID(), "Sec-Fetch-Site": "same-origin" },
    data: JSON.stringify(body),
  });
  return response.status();
}

/** A refusal is on screen with the backend's own code. */
async function expectCode(scope: Locator, code: string): Promise<void> {
  await expect(scope.locator(".explain-meta").first()).toContainText(`code ${code}`);
}

/** A well-formed UUIDv7 nobody recorded: the version nibble of a random v4 set to 7. */
function absentId(): string {
  const v4 = randomUUID();
  return `${v4.slice(0, 14)}7${v4.slice(15)}`;
}

/** How many rows the pending capital panel owes the answer: one per figure the backend reported as not yet free. */
function pendingRows(p: Portfolio): number {
  let rows = 0;
  for (const b of p.balances) {
    rows += b.reservations.length;
    if (!/^-?0+$/.test(b.withdrawal_holds)) rows += 1;
    rows += b.held_elsewhere.filter((h) => h.code === "UNSETTLED_RECEIVABLE" || h.code === "BRIDGE_IN_FLIGHT").length;
  }
  for (const m of p.positions.prediction_markets.markets) {
    if (m.positions.held_payout !== undefined && !m.positions.held_payout.claimed) rows += 1;
  }
  return rows;
}

test.describe("the portfolio", () => {
  test("/advanced opens on the portfolio, under the simulated-value notice, and the overview is one link away", async ({
    page,
  }) => {
    await goto(page, "/advanced");
    await expect(page).toHaveURL(/\/advanced\/portfolio$/);
    await expect(page.getByRole("heading", { level: 1, name: "Portfolio", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");

    const area = page.getByRole("navigation", { name: "Advanced area", exact: true });
    await expect(area.getByRole("link").first()).toHaveText("Portfolio");
    await area.getByRole("link", { name: "Overview", exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Overview", exact: true })).toBeVisible();
  });

  test("every balance and every section is the backend's own answer", async ({ page }) => {
    const id = await ownAccount(page);
    // Put something in the wallet when this deployment runs the faucet; when
    // it does not, the portfolio is still checked against whatever it holds.
    const granted = await command(page, "/v1/advanced/transfers/faucet/grants", { account_id: id, amount: "5" });
    expect([201, 200, 403, 409, 422, 429], `the faucet answered ${String(granted)}`).toContain(granted);

    const { status, body } = await portfolioOf(page, id);
    expect(status, "the owner reads the portfolio").toBe(200);
    const p = body as Portfolio;
    expect(p.value_domain).toBe("SIMULATED");
    expect(p.account_id).toBe(id);
    if (granted === 201) {
      expect(p.balances.some((b) => b.symbol === p.quote_symbol), "the faucet's sandbox dollars are a balance").toBe(true);
    }

    await goto(page, PAGE);
    await settle(page);
    await expect(page.locator(".malformed")).toHaveCount(0);
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);

    const balances = page.getByRole("region", { name: "Balances", exact: true });
    if (p.balances.length === 0) {
      await expect(balances).toContainText("No simulated assets on this account");
    } else {
      await expect(balances.locator("tbody tr")).toHaveCount(p.balances.length);
    }
    for (const b of p.balances) {
      const row = balances.locator(`tr[data-asset="${b.asset_id}"]`);
      await expect(row.locator('[data-field="wallet"]')).toHaveText(renderedQuantity(b.wallet, b.decimals));
      await expect(row.locator('[data-field="available"]')).toHaveText(renderedQuantity(b.available, b.decimals));
      await expect(row.locator('[data-field="reserved"]')).toContainText(renderedQuantity(b.reserved, b.decimals));
      await expect(row.locator('[data-field="total"]')).toHaveText(`${renderedQuantity(b.total, b.decimals)} ${b.symbol}`);
      const valuation = row.locator('[data-field="valuation"]');
      if (b.valuation.status === "UNVALUED") {
        await expect(valuation).toContainText("Unvalued");
        await expect(valuation).toContainText(`code ${b.valuation.problem?.code ?? ""}`);
        await expect(valuation.locator('[data-field="value"]')).toHaveCount(0);
      } else {
        await expect(valuation.locator('[data-field="value"]')).toHaveText(
          `${renderedQuantity(b.valuation.value ?? "", p.quote_decimals)} ${p.quote_symbol}`,
        );
      }
    }

    for (const section of SECTIONS) {
      const answer = section.pick(p);
      const panel = page.getByRole("region", { name: section.title, exact: true });
      await expect(panel.locator(".panel-actions")).toHaveText(answer.state);
      switch (answer.state) {
        case "NOT_CONFIGURED":
          await expect(panel).toContainText(`${section.subsystem} is not configured in this deployment`);
          break;
        case "UNAVAILABLE":
          await expectCode(panel, answer.problem?.code ?? "");
          break;
        case "NONE":
          await expect(panel).toContainText("No margin account on this account");
          break;
        default:
          await expect(panel.locator(".explain")).toHaveCount(0);
      }
    }

    const bp = p.buying_power.buying_power;
    if (bp !== undefined) {
      for (const field of BUYING_POWER_FIELDS) {
        await expect(page.locator(`[data-field="${field}"]`)).toHaveText(renderedUsd(bp[field] as string));
      }
    } else {
      await expect(page.locator(`[data-field="${BUYING_POWER_FIELDS[0]}"]`)).toHaveCount(0);
    }

    const pending = page.getByRole("region", { name: "Pending capital", exact: true });
    const rows = pendingRows(p);
    if (rows === 0) {
      await expect(pending).toContainText("Nothing pending");
    } else {
      await expect(pending.locator("tbody tr")).toHaveCount(rows);
    }

    const risk = page.getByRole("region", { name: "Risk", exact: true });
    if (p.risk.decisions.state === "UNAVAILABLE") {
      await expectCode(risk, p.risk.decisions.problem?.code ?? "");
    }
  });

  test("the page shows no money-shaped figure the API did not return", async ({ page }) => {
    const pending: Array<Promise<unknown>> = [];
    const listener = (response: Response): void => {
      if (!new URL(response.url()).pathname.startsWith("/v1/advanced/")) return;
      pending.push(response.json().catch(() => undefined));
    };
    page.on("response", listener);
    await goto(page, PAGE);
    await settle(page);
    page.off("response", listener);
    const wire = new Set<string>();
    for (const body of await Promise.all(pending)) collectUsd(body, wire);
    const allowed = new Set([...wire].map(renderedUsd));

    const text = await page.locator("main").innerText();
    for (const figure of text.match(/-?\$[0-9][0-9,]*\.[0-9]{2}/g) ?? []) {
      expect(allowed.has(figure), `the portfolio shows ${figure}, which no advanced response contained`).toBe(true);
    }
  });

  test("each section links to the page it summarises", async ({ page }) => {
    await goto(page, PAGE);
    await settle(page);
    for (const href of SUMMARISED_PAGES) {
      await expect(page.locator(`main a[href="${href}"]`).first(), `a link to ${href}`).toBeVisible();
    }
  });

  test("another customer's account and an absent one are refused alike; an operator learns only that one is absent", async ({
    page,
    browser,
  }) => {
    const b = await signIn(browser, "customer-b", false);
    const other = await ownAccount(b.page);
    await b.context.close();

    const foreign = await portfolioOf(page, other);
    const missing = await portfolioOf(page, absentId());
    expect(foreign.status, "a stranger's account is refused").toBe(403);
    expect(missing.status, "an absent account answers exactly as a foreign one").toBe(foreign.status);
    expect((foreign.body as Problem).code).toBe("FORBIDDEN");
    expect((missing.body as Problem).code).toBe((foreign.body as Problem).code);
    expect((missing.body as Problem).detail).toBe((foreign.body as Problem).detail);
    expect(JSON.stringify(foreign.body), "nothing of the foreign account comes back").not.toContain("balances");

    const admin = await signIn(browser, "admin", true);
    try {
      const absent = absentId();
      const answer = await portfolioOf(admin.page, absent);
      expect(answer.status).toBe(404);
      expect((answer.body as Problem).code).toBe("NOT_FOUND");

      await admin.page.goto(PAGE);
      await settle(admin.page);
      await admin.page.getByLabel("Inspect an account by id").fill(absent);
      await admin.page.getByRole("button", { name: "Inspect", exact: true }).click();
      await expect(admin.page.getByText(absent).first()).toBeVisible();
      await expectCode(admin.page.locator("main"), "NOT_FOUND");
      await expect(admin.page.locator('[data-field="wallet"]')).toHaveCount(0);
    } finally {
      await admin.context.close();
    }
  });

  test("the portfolio has no automatically detectable WCAG A/AA violation", async ({ page }) => {
    await goto(page, PAGE);
    await settle(page);
    const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"]).analyze();
    const summary = results.violations.map(
      (violation) =>
        `${violation.id} (${violation.impact ?? "unknown"}): ${violation.help} — ${violation.nodes
          .map((node) => node.target.join(" "))
          .join(", ")}`,
    );
    expect(summary, "portfolio accessibility violations").toEqual([]);
  });
});
