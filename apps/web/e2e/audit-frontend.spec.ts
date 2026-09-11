/**
 * AUDIT REPRODUCTIONS (goal §54, area frontend-money-honesty).
 *
 * These tests are written by an independent auditor to DEMONSTRATE defects.
 * Each one is named for the finding it reproduces and FAILS while the defect
 * is present. Nothing here fixes anything; nothing here is product code.
 *
 * The account under test is seeded by the auditor's harness so that the Credit
 * balance carries a `reversed` bucket and a `frozen` bucket at the same time
 * (see the audit transcript for the two `credit_lot_state` rows).
 *
 * `Number(` is deliberately absent: this file lives under `e2e/`, which
 * `src/lib/source-scan.test.ts` scans, so every count here is BigInt.
 */
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

import { NARROW_HEIGHT, NARROW_WIDTH } from "./routes.ts";

const SIGNED_OUT = { cookies: [], origins: [] };

async function violations(page: Page): Promise<string[]> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  return results.violations.map(
    (v) =>
      `${v.id} (${v.impact ?? "unknown"}): ${v.help} — ${v.nodes
        .map((n) => n.target.join(" "))
        .join(", ")}`,
  );
}

async function accountId(page: Page): Promise<string> {
  const res = await page.request.get("/v1/accounts");
  const list = (await res.json()) as Array<{ id: string }>;
  return list[0]?.id ?? "";
}

/** The shares a SegmentedBar states, summed exactly. */
function shareSum(label: string): bigint {
  let total = 0n;
  for (const match of label.matchAll(/about ([0-9]+)%/g)) {
    total = total + BigInt(match[1] ?? "0");
  }
  return total;
}

function shareCount(label: string): number {
  return [...label.matchAll(/about ([0-9]+)%/g)].length;
}

/* ==========================================================================
 * The reversed bucket is inside the total and inside no bucket the page names
 * ========================================================================== */

test("F-web: Home's Credit panel accounts for every part of the total", async ({ page }) => {
  const id = await accountId(page);
  const res = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  const balance = (await res.json()) as Record<string, string>;
  expect(
    BigInt(balance["reversed"] ?? "0") > 0n,
    "precondition: the API reports a non-zero reversed bucket",
  ).toBe(true);

  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  await page.waitForLoadState("networkidle");
  const text = await page.evaluate(() => document.body.innerText);

  const named = BigInt(balance["spendable"] ?? "0") + BigInt(balance["frozen"] ?? "0");
  const gross = BigInt(balance["gross"] ?? "0");
  expect(
    String(gross - named),
    "every Credit inside Total Credits is in a bucket the panel names",
  ).toBe("0");
  expect(text.toLowerCase(), "or the panel names the reversed bucket in words").toContain(
    "reversed",
  );
});

test("F-web: the Home balance bar's parts account for the whole", async ({ page }) => {
  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  await page.waitForLoadState("networkidle");
  const label = (await page.locator('.segbar[role="img"]').first().getAttribute("aria-label")) ?? "";
  expect(shareCount(label) > 0, "the balance bar states a share per segment").toBe(true);
  // Truncation loses at most one point per segment, so two segments may sum to
  // 98. Below that is value the picture does not account for.
  expect(
    shareSum(label) >= 98n,
    `"how this balance is held" sums to ${String(shareSum(label))}% — ${label}`,
  ).toBe(true);
});

/* ==========================================================================
 * The withdrawal bar counts the frozen bucket twice
 * ========================================================================== */

test("F-web: the withdrawal bar does not count one Credit in two segments", async ({ page }) => {
  const id = await accountId(page);
  const res = await page.request.get(`/v1/me/eligibility?account_id=${id}`);
  const e = (await res.json()) as Record<string, string>;
  const drawn =
    BigInt(e["payout_eligible"] ?? "0") + BigInt(e["ineligible"] ?? "0") + BigInt(e["frozen"] ?? "0");
  expect(
    drawn <= BigInt(e["gross"] ?? "0"),
    "eligible + ineligible + frozen is what the page draws against gross, and frozen is already inside ineligible",
  ).toBe(true);

  await page.goto("/withdraw");
  await expect(page.getByRole("heading", { level: 1, name: "Withdraw" })).toBeVisible();
  await page.waitForLoadState("networkidle");
  const label = (await page.locator('.segbar[role="img"]').first().getAttribute("aria-label")) ?? "";
  expect(
    shareSum(label) <= 100n,
    `"your Credits by what may leave" sums to ${String(shareSum(label))}% — ${label}`,
  ).toBe(true);
});

/* ==========================================================================
 * The stream's scope map misses the portfolio and activity query keys
 * ========================================================================== */

test("F-web: a data.changed position/balance signal refetches the portfolio on screen", async ({
  page,
}) => {
  // Record every EventSource the app opens, so the auditor can deliver the
  // event the server would have sent.
  await page.addInitScript(() => {
    const opened: EventSource[] = [];
    (window as unknown as { __auditSources: EventSource[] }).__auditSources = opened;
    const Original = window.EventSource;
    class Recorded extends Original {
      constructor(url: string | URL, init?: EventSourceInit) {
        super(url, init);
        opened.push(this);
      }
    }
    window.EventSource = Recorded as unknown as typeof EventSource;
  });

  await page.goto("/portfolio");
  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();
  await page.waitForLoadState("networkidle");

  let portfolioReads = 0;
  page.on("request", (r) => {
    if (r.url().includes("/v1/me/portfolio")) portfolioReads = portfolioReads + 1;
  });

  const delivered = await page.evaluate(() => {
    const opened = (window as unknown as { __auditSources?: EventSource[] }).__auditSources ?? [];
    for (const source of opened) {
      for (const scope of ["position", "balance"]) {
        source.dispatchEvent(
          new MessageEvent("message", {
            data: JSON.stringify({ type: "data.changed", data: { scope, ref: "audit" } }),
          }),
        );
      }
    }
    return opened.length;
  });
  expect(delivered > 0, "the shell opened an event stream to deliver the signal to").toBe(true);

  await page.waitForTimeout(2000);
  expect(
    portfolioReads > 0,
    "a position/balance signal refetches the portfolio the customer is looking at",
  ).toBe(true);
});

/* ==========================================================================
 * `/agents/:agentId` is in no route list, so no cross-cutting sweep visits it
 * ========================================================================== */

test("F-web: the agent detail route passes axe and reflows at 375px", async ({ page }) => {
  const id = await accountId(page);
  const res = await page.request.get(`/v1/agents?account_id=${id}`);
  const body = (await res.json()) as { items?: Array<{ id: string }> };
  const agent = body.items?.[0];
  test.skip(agent === undefined, "no agent on this account to open");

  await page.goto(`/agents/${agent?.id ?? ""}`);
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  expect(await violations(page), "/agents/:agentId accessibility violations").toEqual([]);

  await page.setViewportSize({ width: NARROW_WIDTH, height: NARROW_HEIGHT });
  await page.waitForTimeout(300);
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "/agents/:agentId does not scroll horizontally at 375px").toBeLessThanOrEqual(1);
});

/* ==========================================================================
 * The public site never says this deployment is a sandbox tier
 * ========================================================================== */

test("F-web: the public site says whether this deployment is a rehearsal", async ({ browser }) => {
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  const page = await context.newPage();
  const version = (await (await page.request.get("/v1/version")).json()) as {
    sandbox_tier?: boolean;
  };
  test.skip(version.sandbox_tier !== true, "not a sandbox tier");

  const missing: string[] = [];
  for (const path of ["/", "/product/markets", "/product/agents"]) {
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await page.waitForLoadState("networkidle");
    const text = await page.evaluate(() => document.body.innerText);
    if (!/sandbox/i.test(text)) missing.push(path);
  }
  await context.close();
  expect(missing, "every public page served by a sandbox tier says so").toEqual([]);
});

/* ==========================================================================
 * The application sweeps never check they landed on the page under test
 * ========================================================================== */

test("F-web: the APP_ROUTES sweeps assert nothing that identifies the page", async ({ page }) => {
  // `accessibility.spec.ts:55` and `controls.spec.ts:94` walk APP_ROUTES
  // asserting only `h1` count 1. The 404 page satisfies every one of those
  // assertions, so a route that stopped existing, or a gate that diverted,
  // would keep the suite green. `RouteUnderTest.heading` exists and neither
  // application sweep reads it.
  await page.goto("/this-route-does-not-exist");
  await expect(page.locator("h1")).toHaveCount(1);
  const heading = (await page.locator("h1").innerText()).trim();
  expect(heading, "the sweep's page is the route's page").toBe("Home");
});

/* ==========================================================================
 * The mobile trade sheet
 * ========================================================================== */

test("F-web: the mobile trade sheet is accessible and traps focus", async ({ page }) => {
  const res = await page.request.get("/v1/native-markets?limit=1");
  const body = (await res.json()) as { markets: Array<{ market_id: string }> };
  const market = body.markets[0];
  test.skip(market === undefined, "no market to open");

  await page.setViewportSize({ width: NARROW_WIDTH, height: NARROW_HEIGHT });
  await page.goto(`/markets/${market?.market_id ?? ""}`);
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");

  const opener = page.getByRole("button", { name: "Buy or sell", exact: true });
  await opener.click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  expect(await violations(page), "the open trade sheet's accessibility violations").toEqual([]);

  const outside: string[] = [];
  for (let i = 0; i < 30; i = i + 1) {
    await page.keyboard.press("Tab");
    const where = await page.evaluate(() => {
      const active = document.activeElement;
      const host = document.querySelector("dialog[open]");
      if (host === null) return "no-dialog";
      if (active === null) return "none";
      if (host.contains(active)) return "in";
      // `body` is where a browser parks focus while it is in its own chrome,
      // which is not the page failing to trap anything.
      if (active === document.body) return "in";
      return `out:${active.tagName}:${(active.textContent ?? "").trim().slice(0, 30)}`;
    });
    if (where !== "in") outside.push(`${where} at tab ${String(i)}`);
  }
  expect(outside, "focus stays inside the open sheet").toEqual([]);
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
});

/* ==========================================================================
 * Focus after a route change
 * ========================================================================== */

test("F-web: changing route moves focus into the new page", async ({ page }) => {
  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  await page
    .getByRole("navigation", { name: "Sections" })
    .getByRole("link", { name: "Portfolio", exact: true })
    .click();
  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();
  const where = await page.evaluate(() => {
    const active = document.activeElement;
    if (active === null || active === document.body) return "body";
    const main = document.getElementById("main");
    return main !== null && (active === main || main.contains(active)) ? "main" : active.tagName;
  });
  expect(where, "focus is moved into the new page's main region").toBe("main");
});

/* ==========================================================================
 * The event stream's credentials, as the deployed tier would send them
 * ========================================================================== */

test("F-web: the event stream is opened with credentials", async ({ page }) => {
  await page.addInitScript(() => {
    const seen: Array<{ url: string; withCredentials: boolean }> = [];
    (window as unknown as { __auditStream: typeof seen }).__auditStream = seen;
    const Original = window.EventSource;
    class Recorded extends Original {
      constructor(url: string | URL, init?: EventSourceInit) {
        super(url, init);
        seen.push({ url: String(url), withCredentials: this.withCredentials });
      }
    }
    window.EventSource = Recorded as unknown as typeof EventSource;
  });
  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  await page.waitForLoadState("networkidle");
  const opened = await page.evaluate(
    () => (window as unknown as { __auditStream?: Array<{ url: string; withCredentials: boolean }> }).__auditStream ?? [],
  );
  expect(opened.length > 0, "the shell opens an event stream").toBe(true);
  // In the deployed tier VITE_API_ORIGIN makes this URL cross-origin, and an
  // EventSource without withCredentials sends no cookie at all.
  expect(
    opened.map((s) => s.withCredentials),
    "every event stream is opened with credentials, because the deployed API is on another origin",
  ).toEqual(opened.map(() => true));
});

/* ==========================================================================
 * What a signed-out tab keeps for the next person to use it
 * ========================================================================== */

test("F-web: signing out forgets what the previous customer typed", async ({ browser }) => {
  // `lib/survives-sign-in.ts` says the mirror is "scoped to the one tab and
  // dies with it, so a shared computer does not hand the next person a form".
  // Signing out is exactly the shared-computer case and the tab does not die:
  // `AppShell`'s sign-out reloads the page and clears no storage at all.
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  const page = await context.newPage();

  await page.goto("/v1/auth/login");
  await page.getByRole("heading", { name: "Choose an identity" }).waitFor();
  await page
    .locator("li", { has: page.locator("code", { hasText: /^customer-a$/ }) })
    .getByRole("link", { name: "sign in", exact: true })
    .first()
    .click();
  await page.waitForURL(/\/(welcome|home)(\/|$)/);
  await completeOnboardingIfOwed(page, "Customer A");

  await page.goto("/withdraw");
  await expect(page.getByRole("heading", { level: 1, name: "Withdraw" })).toBeVisible();
  const amount = page.getByLabel(/amount/i).first();
  await amount.fill("1234");
  await page.waitForTimeout(300);

  const beforeSignOut = await page.evaluate(() =>
    Object.keys(window.sessionStorage).filter((k) => k.startsWith("nodal.form.")),
  );
  expect(beforeSignOut.length > 0, "the draft was stashed").toBe(true);

  await page.getByRole("button", { name: "Open the account menu" }).click();
  await page.getByRole("dialog", { name: "Account" }).getByRole("button", { name: "Sign out" }).click();
  await page.waitForURL(/127\.0\.0\.1:\d+\/$/);
  await page.waitForTimeout(500);

  const afterSignOut = await page.evaluate(() => {
    const out: Record<string, string> = {};
    for (const key of Object.keys(window.sessionStorage)) {
      if (key.startsWith("nodal.")) out[key] = window.sessionStorage.getItem(key) ?? "";
    }
    return out;
  });
  await context.close();
  expect(
    Object.keys(afterSignOut),
    `signing out left the previous customer's form state in this tab: ${JSON.stringify(afterSignOut)}`,
  ).toEqual([]);
});

async function completeOnboardingIfOwed(page: Page, displayName: string): Promise<void> {
  for (let i = 0; i < 6; i = i + 1) {
    const path = new URL(page.url()).pathname;
    if (!path.startsWith("/welcome")) return;
    if (path === "/welcome") {
      await page.getByLabel("Display name").fill(displayName);
      await page.getByRole("button", { name: /continue|save/i }).first().click();
    } else if (path === "/welcome/terms") {
      for (const box of await page.getByRole("checkbox").all()) await box.check();
      await page.getByRole("button", { name: /accept|continue/i }).first().click();
    } else {
      await page.getByRole("link", { name: /dashboard|home/i }).first().click();
    }
    await page.waitForTimeout(600);
  }
}
