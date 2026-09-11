/**
 * Scenario D — an agent, as far as this build honestly goes.
 *
 * `docs/product/STAGING_E2E.md` defines D as `/agents/new` → strategy →
 * compile (or `COMPILER_UNAVAILABLE`, stated) → review → create at level 1 →
 * inspect, proving the honest runtime state and that levels 4–6 are disabled,
 * and forbidding two things: an execution path, and a fabricated compile.
 *
 * THIS BUILD TAKES THE SECOND BRANCH, AND THAT IS THE POINT. `cmd/api/wire.go`
 * constructs the strategy service with `Compiler: nil`, so `compiler_configured`
 * is false everywhere, every compile attempt is recorded with outcome
 * MODEL_UNAVAILABLE and the failure code COMPILER_UNAVAILABLE, and no strategy
 * version is ever produced. An agent is created FROM a version — the column is
 * `REFERENCES strategy_versions(id)` — so on this build no agent can exist.
 *
 * A spec that skipped here would look like coverage. Instead this walks the
 * whole honest path and asserts the thing that actually matters: that the
 * product says why, in the API's own words, and that neither the interface nor
 * the API invents a compiled strategy to get past it. The `create at level 1`
 * assertions arrive in this file the day a compiler backend is configured, and
 * the branch below is written so that they can.
 *
 * Nothing is stubbed. The strategy is created through the real API, the compile
 * attempt is recorded in the real database, and the refusals are the backend's.
 */
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

import { NARROW_HEIGHT, NARROW_WIDTH } from "../routes.ts";

/**
 * Two identifiers that are well-formed and belong to nothing.
 *
 * They are version 7 UUIDs because every domain identifier in this system is —
 * `internal/id.Parse` refuses any other version — so a v4-shaped constant would
 * be turned away as malformed at the edge and would prove nothing about whether
 * the record exists. These get all the way to the lookup and the foreign key,
 * which is where the interesting refusal lives.
 */
const ABSENT_AGENT = "01999999-9999-7999-8999-999999999999";
const ABSENT_VERSION = "01999999-9999-7999-8999-999999999998";

/** A description worth compiling, so the refusal is about the tier and not the text. */
const DESCRIPTION =
  "Watch the internal market for one asset I have named. When its price falls more " +
  "than five per cent below its average over the last hour, propose a buy no larger " +
  "than the per-trade cap. Never propose anything outside the assets I listed, and " +
  "stop for the day once the loss stop is reached.";

/**
 * What a same-origin write from the app itself carries.
 *
 * `page.request` shares the browser context's cookies but is not a page, so it
 * sends no `Sec-Fetch-Site` and no `Origin`, and the API's CSRF middleware
 * refuses it — correctly. Setting the header the app's own fetch would send is
 * what makes these requests a test of the BUSINESS rule rather than a
 * rediscovery of the cross-site one.
 */
const SAME_ORIGIN = { "Sec-Fetch-Site": "same-origin" } as const;

/**
 * A name nothing else has used.
 *
 * A strategy name is unique per account, so a fixed one passes on a fresh
 * database and answers 409 on the second run against the same one. The suite
 * has to be re-runnable against a database it has already touched.
 */
function uniqueName(prefix: string): string {
  return `${prefix} ${String(Date.now())}`;
}

async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/accounts");
  expect(response.status(), "the account list is readable").toBe(200);
  const accounts = (await response.json()) as ReadonlyArray<{ readonly id: string }>;
  expect(accounts.length, "the seeded session owns an account").toBeGreaterThan(0);
  return accounts[0]?.id as string;
}

async function violations(page: Page): Promise<string[]> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  return results.violations.map(
    (violation) =>
      `${violation.id} (${violation.impact ?? "unknown"}): ${violation.help} — ${violation.nodes
        .map((node) => node.target.join(" "))
        .join(", ")}`,
  );
}

test("the authority ladder is shown whole, with 4 to 6 disabled by policy", async ({ page }) => {
  await page.goto("/agents");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();

  // Every declared level, enabled or not. A product that omitted the disabled
  // rungs would teach nobody that they exist and are switched off.
  const ladder = page.getByRole("group", {
    name: /Every declared authority level/,
  });
  await expect(ladder).toBeVisible();

  for (const name of ["RESEARCH_ONLY", "RECOMMENDATION", "PREPARE_TRANSACTION", "USER_APPROVED_RULE"]) {
    await expect(ladder.getByText(name, { exact: false }).first()).toBeVisible();
  }
  for (const name of ["BOUNDED_DISCRETION", "AUTONOMOUS_SELECTION", "AUTONOMOUS_PORTFOLIO"]) {
    const row = ladder.locator("tr", { has: page.getByText(name, { exact: false }) });
    await expect(row, `${name} is listed`).toHaveCount(1);
    // The badge, not the prose: each disabled level's own summary also says it
    // is disabled by policy, which is the point, and matching on the words
    // alone would resolve to both.
    await expect(row.locator(".badge", { hasText: "disabled by policy" })).toBeVisible();
  }

  // And the API agrees, which is what makes the table a report rather than a
  // sentence somebody typed beside it.
  const id = await accountId(page);
  const response = await page.request.get(`/v1/agents?account_id=${id}`);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as {
    readonly authority_levels: ReadonlyArray<{ readonly level: number; readonly enabled: boolean }>;
  };
  for (const level of body.authority_levels) {
    if (level.level >= 4) {
      expect(level.enabled, `level ${String(level.level)} is disabled by policy`).toBe(false);
    }
  }
});

test("a strategy is recorded, the compile attempt says COMPILER_UNAVAILABLE, and no agent follows", async ({
  page,
}) => {
  const id = await accountId(page);

  const before = await page.request.get(`/v1/agents?account_id=${id}`);
  const beforeBody = (await before.json()) as { readonly items: readonly unknown[] };
  const agentsBefore = beforeBody.items.length;

  await page.goto("/agents");
  await page.getByRole("link", { name: "Create an agent" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Create an agent" })).toBeVisible();

  // The tier says so BEFORE a description is written, not after.
  await expect(page.getByText("No agent can be created on this deployment.")).toBeVisible();

  // 1 · Describe. Nothing is compiled and nothing is activated by this.
  const name = uniqueName("Mean reversion, one asset");
  await page.getByLabel("A name for this strategy").fill(name);
  await page.getByLabel("What should it do?").fill(DESCRIPTION);
  await page.getByRole("button", { name: "Record this description" }).click();

  // The step is done and stays on the page: the recorded strategy, its id, and
  // the words exactly as written.
  await expect(page.getByRole("button", { name: "Describe a different strategy" })).toBeVisible();
  await expect(page.getByText(DESCRIPTION)).toBeVisible();

  // 2 · Compile. The attempt is recorded; the answer is a refusal, not an error.
  await page.getByRole("button", { name: "Compile this strategy" }).click();
  await expect(page.getByText("This description was not turned into a strategy.")).toBeVisible();
  // Rendered as exactly that stable code, verbatim (ADR-0029, D-074).
  await expect(page.getByText("COMPILER_UNAVAILABLE", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("MODEL_UNAVAILABLE", { exact: true }).first()).toBeVisible();

  // 3 · Review. There is nothing to review, and the page says that rather than
  // drawing a strategy nobody compiled.
  await expect(page.getByText("There is nothing to review")).toBeVisible();

  // 4 · Grant is refused, with the reason beside the control rather than in a
  // tooltip. This is the assertion that a fabricated compile would break.
  const create = page.getByRole("button", { name: "Create this agent, stopped" });
  await expect(create).toBeDisabled();
  const describedBy = await create.getAttribute("aria-describedby");
  expect(describedBy, "the disabled control explains itself").toBeTruthy();
  const reason = page.locator(`[id="${(describedBy as string).replace(/"/g, '\\"')}"]`);
  await expect(reason).toBeVisible();
  await expect(reason).toContainText("compiled strategy version");

  // And the strategy that WAS recorded is real: the API kept it, and it reports
  // honestly that this deployment cannot compile.
  const strategies = await page.request.get(`/v1/strategies?account_id=${id}`);
  expect(strategies.status()).toBe(200);
  const listed = (await strategies.json()) as {
    readonly items: ReadonlyArray<{ readonly name: string; readonly current_version?: unknown }>;
    readonly compiler_configured: boolean;
  };
  expect(listed.compiler_configured, "this build configures no compiler backend").toBe(false);
  const recorded = listed.items.find((item) => item.name === name);
  expect(recorded, "the description was recorded").toBeTruthy();
  expect(
    recorded?.current_version,
    "no compiled version was produced, and none was invented",
  ).toBeUndefined();

  // No agent appeared. Not from the interface, and not as a side effect.
  const after = await page.request.get(`/v1/agents?account_id=${id}`);
  const afterBody = (await after.json()) as { readonly items: readonly unknown[] };
  expect(afterBody.items.length, "no agent was created").toBe(agentsBefore);
});

test("the API refuses an agent built on a strategy version that does not exist", async ({ page }) => {
  // The interface refusing is not the gate. This asks the API directly, with a
  // request that is correct in every other respect — a real account, a real
  // strategy recorded a moment ago, a real asset in the universe, a well-formed
  // version id — and names a compiled version that does not exist. The backend
  // refuses it too: `agents.strategy_version_id REFERENCES strategy_versions(id)`,
  // so there is no path to an agent without a compiled strategy, not through
  // this page and not around it.
  const id = await accountId(page);

  const strategy = await page.request.post("/v1/strategies", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `d-agent-strategy-${String(Date.now())}` },
    data: {
      account_id: id,
      name: uniqueName("Direct request, no compiled version"),
      description: DESCRIPTION,
    },
  });
  expect(strategy.status(), "the strategy is recorded").toBeLessThan(300);
  const recorded = (await strategy.json()) as { readonly id: string };

  // The universe is checked against the asset registry, which is what the page
  // offers and what internal/agents validates a grant against.
  const assets = await page.request.get("/v1/assets");
  const registry = (await assets.json()) as ReadonlyArray<{ readonly id: string }>;
  const asset = registry[0]?.id;
  expect(asset, "the seeded deployment has at least one asset").toBeTruthy();

  const before = await page.request.get(`/v1/agents?account_id=${id}`);
  const agentsBefore = ((await before.json()) as { readonly items: readonly unknown[] }).items.length;

  const response = await page.request.post("/v1/agents", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `d-agent-no-version-${String(Date.now())}` },
    data: {
      account_id: id,
      strategy_id: recorded.id,
      strategy_version_id: ABSENT_VERSION,
      name: "An agent with no compiled strategy",
      authority_level: 1,
      limits: {
        budget_credits: "1000000",
        per_trade_cap_credits: "100000",
        daily_loss_stop_credits: "100000",
        max_position_share_bps: 2500,
        allowed_asset_ids: [asset as string],
        schedule: { kind: "MANUAL" },
      },
    },
  });
  expect(
    response.status(),
    "the backend refuses an agent with no compiled version",
  ).toBeGreaterThanOrEqual(400);
  const problem = (await response.json()) as { readonly code?: string };
  expect(problem.code, "the refusal carries a stable code").toBeTruthy();

  const after = await page.request.get(`/v1/agents?account_id=${id}`);
  const agentsAfter = ((await after.json()) as { readonly items: readonly unknown[] }).items.length;
  expect(agentsAfter, "no agent was created by the direct request either").toBe(agentsBefore);
});

test("an agent that does not exist is reported as not found, never drawn", async ({ page }) => {
  await page.goto(`/agents/${ABSENT_AGENT}`);
  await expect(page.getByRole("heading", { level: 1, name: "Agent" })).toBeVisible();
  await expect(page.getByText("This agent could not be read")).toBeVisible();
  // One explanation for one failed read, not one per panel.
  await expect(page.getByText("The backend has no record with that identifier.")).toHaveCount(1);
  // And no figure anywhere: an unknown agent has no budget of zero.
  await expect(page.locator(".figure")).toHaveCount(0);

  expect(await violations(page), "/agents/:agentId accessibility violations").toEqual([]);
});

test("the agent screens reflow at 375 px", async ({ browser }) => {
  const context = await browser.newContext({
    storageState: ".playwright/state.json",
    viewport: { width: NARROW_WIDTH, height: NARROW_HEIGHT },
  });
  const page = await context.newPage();
  for (const path of ["/agents", "/agents/new", `/agents/${ABSENT_AGENT}`]) {
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await page.waitForLoadState("networkidle");
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, `${path} does not scroll sideways at 375 px`).toBeLessThanOrEqual(0);
  }
  await context.close();
});
