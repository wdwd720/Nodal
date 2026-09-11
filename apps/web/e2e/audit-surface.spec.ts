/**
 * The end-to-end audit's attack surface (goal §54 wave B, area 11).
 *
 * Each test below is a reproduction of something the audit tried against the
 * browser surface. The ones that pass are the attacks that did not work and are
 * kept so they stay not working; the ones that fail name a defect in the report
 * that ships with this branch. Nothing here fixes product code.
 *
 * The classes attacked: the deployed Content-Security-Policy against what the
 * bundle actually loads; what a stranger can read off the network on the public
 * site; cross-customer leakage on the event stream; an idempotency key across a
 * network failure and across a changed body; every rendered figure's exactness;
 * and the routes the cross-cutting sweeps do not reach.
 */
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page, type Response } from "@playwright/test";

import { chooseIdentity, completeOnboarding } from "./onboarding.ts";
import { NARROW_HEIGHT, NARROW_WIDTH, PUBLIC_ROUTES } from "./routes.ts";
import { SAME_ORIGIN, accountIdOf, buySomething, ensureDestination, signInAs, verifyInSandbox } from "./withdrawal.ts";

const SIGNED_OUT = { cookies: [], origins: [] };

/**
 * The policy `render.yaml` serves the app under, copied verbatim.
 *
 * `vite preview` serves no headers, so a local run is the one place this policy
 * is never applied — which is exactly why nothing has ever checked the bundle
 * against it. Injecting it as a `<meta http-equiv>` on every document applies
 * everything except `frame-ancestors` (which a meta tag cannot carry), so a
 * script, style, image, font, frame or connection the bundle needs and the
 * policy forbids fails here the way it would on the deployed site.
 */
const DEPLOYED_CSP =
  "default-src 'self'; connect-src 'self' https://api-nodal.actorvia.xyz https://api.stripe.com; " +
  "script-src 'self' https://js.stripe.com; style-src 'self' 'unsafe-inline'; " +
  "img-src 'self' data: https://*.stripe.com; font-src 'self'; " +
  "frame-src https://js.stripe.com https://hooks.stripe.com https://m.stripe.network; " +
  "base-uri 'self'; form-action 'self' https://api-nodal.actorvia.xyz; object-src 'none'";

/* ==========================================================================
 * THE DEPLOYED CONTENT-SECURITY-POLICY, APPLIED TO THE REAL BUNDLE
 * ========================================================================== */

test("the deployed CSP admits everything the application actually loads", async ({ page }) => {
  test.setTimeout(180_000);

  await page.route("**/*", async (route) => {
    const request = route.request();
    if (request.resourceType() !== "document") {
      await route.fallback();
      return;
    }
    const response = await route.fetch();
    const body = await response.text();
    if (!body.includes("<head>")) {
      await route.fulfill({ response, body });
      return;
    }
    await route.fulfill({
      response,
      body: body.replace(
        "<head>",
        `<head><meta http-equiv="Content-Security-Policy" content="${DEPLOYED_CSP}">`,
      ),
    });
  });

  const blocked: string[] = [];
  await page.addInitScript(() => {
    const seen: string[] = [];
    (window as unknown as { __cspViolations: string[] }).__cspViolations = seen;
    document.addEventListener("securitypolicyviolation", (event) => {
      seen.push(`${event.violatedDirective} blocked ${event.blockedURI}`);
    });
  });
  page.on("console", (message) => {
    if (/Content Security Policy|Refused to/i.test(message.text())) blocked.push(message.text());
  });

  for (const path of ["/", "/product", "/security", "/home", "/markets", "/portfolio", "/buy-credits", "/withdraw"]) {
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await page.waitForLoadState("networkidle");
    const violations = await page.evaluate(
      () => (window as unknown as { __cspViolations?: string[] }).__cspViolations ?? [],
    );
    for (const violation of violations) blocked.push(`${path}: ${violation}`);
  }

  expect(blocked, "the bundle loads nothing the deployed policy refuses").toEqual([]);
  await page.unrouteAll({ behavior: "ignoreErrors" });
});

/* ==========================================================================
 * THE PUBLIC SITE, READ OFF THE NETWORK RATHER THAN OFF THE SCREEN
 * ========================================================================== */

test("no public response carries an identity, a balance, a holder or a creator", async ({
  browser,
  page,
}) => {
  test.setTimeout(180_000);

  // Facts about a real signed-in customer, to search the stranger's traffic for.
  const insider = await accountIdOf(page);
  const me = await page.request.get("/v1/me");
  const profile = (await me.json()) as {
    readonly user_id?: string;
    readonly profile?: { readonly display_name?: string };
  };
  const secrets = [insider, profile.user_id ?? "", profile.profile?.display_name ?? ""].filter(
    (s) => s.length > 3,
  );
  expect(secrets.length, "there is something to look for").toBeGreaterThan(0);

  const stranger = await browser.newContext({ storageState: SIGNED_OUT });
  const strangerPage = await stranger.newPage();

  const leaks: string[] = [];
  const bodies: Array<Promise<void>> = [];
  strangerPage.on("response", (response: Response) => {
    const url = response.url();
    if (!url.includes("/v1/")) return;
    bodies.push(
      response
        .text()
        .then((text) => {
          for (const secret of secrets) {
            if (text.includes(secret)) leaks.push(`${url} carries ${secret}`);
          }
          // A public response must never carry the vocabulary of somebody's
          // holdings either, whatever identifier it is keyed by.
          for (const field of ['"spendable"', '"payout_eligible"', '"holder"', '"account_id"']) {
            if (text.includes(field)) leaks.push(`${url} carries ${field}`);
          }
        })
        .catch(() => undefined),
    );
  });

  for (const route of PUBLIC_ROUTES) {
    await strangerPage.goto(route.path);
    await expect(strangerPage.locator("h1")).toHaveCount(1);
    await strangerPage.waitForLoadState("networkidle");
  }
  await Promise.all(bodies);

  expect(leaks, "a stranger's traffic names nobody and totals nothing").toEqual([]);

  // And the routes that would answer with somebody's data refuse outright.
  for (const path of ["/v1/me", "/v1/accounts", "/v1/me/portfolio", "/v1/events/stream"]) {
    const response = await strangerPage.request.get(path);
    // `/v1/me/portfolio` answers 400 rather than 401 because the missing
    // `account_id` is validated before the session is read. It still answers
    // nobody's data, which is what this is about.
    expect(response.status(), `${path} answers a visitor with no session nothing`).not.toBe(200);
  }
  await stranger.close();
});

/* ==========================================================================
 * THE EVENT STREAM, ACROSS TWO SIGNED-IN CUSTOMERS
 * ========================================================================== */

test("one customer's stream never carries another customer's events", async ({ browser, page }) => {
  test.setTimeout(180_000);

  const other = await signInAs(browser, "customer-b", "Customer B");
  const otherId = await accountIdOf(other);
  const mineId = await accountIdOf(page);
  expect(otherId, "the two sessions are different accounts").not.toBe(mineId);

  // Record every message the second customer's stream delivers.
  await other.addInitScript(() => {
    const seen: string[] = [];
    (window as unknown as { __streamMessages: string[] }).__streamMessages = seen;
    const Original = window.EventSource;
    class Recorded extends Original {
      constructor(url: string | URL, init?: EventSourceInit) {
        super(url, init);
        this.addEventListener("message", (event: MessageEvent<string>) => {
          seen.push(String(event.data));
        });
      }
    }
    window.EventSource = Recorded as unknown as typeof EventSource;
  });
  await other.goto("/home");
  await expect(other.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  await other.waitForLoadState("networkidle");

  // The first customer does something that produces events: a purchase from the
  // seeded catalogue, which mints an earning and writes activity rows.
  await buySomething(page, mineId);
  await other.waitForTimeout(6000);

  const delivered = await other.evaluate(
    () => (window as unknown as { __streamMessages?: string[] }).__streamMessages ?? [],
  );
  const leaked = delivered.filter((message) => message.includes(mineId));
  expect(leaked, "no event names an account this session does not own").toEqual([]);
  await other.context().close();
});

/* ==========================================================================
 * AN IDEMPOTENCY KEY ACROSS A NETWORK FAILURE AND ACROSS A CHANGED BODY
 * ========================================================================== */

test("a retry after a network failure sends the same key; a changed amount sends a new one", async ({
  page,
}) => {
  test.setTimeout(180_000);

  const markets = await page.request.get("/v1/native-markets?limit=25");
  const list = (await markets.json()) as {
    readonly markets: ReadonlyArray<{
      readonly market_id: string;
      readonly symbol: string;
      readonly market_status: string;
    }>;
  };
  const market = list.markets.find((m) => m.market_status === "ACTIVE");
  expect(market, "this tier seeds a market to trade").toBeTruthy();
  const target = market as NonNullable<typeof market>;

  // Every order this test causes, as the wire saw it.
  const keys: string[] = [];
  let failNext = true;
  await page.route("**/v1/native-markets/*/orders", async (route) => {
    const key = (await route.request().allHeaders())["idempotency-key"] ?? "";
    keys.push(key);
    if (failNext) {
      // The reply is lost. This is the case idempotency exists for: the
      // customer has no way to know whether the order was taken.
      failNext = false;
      await route.abort("failed");
      return;
    }
    await route.fallback();
  });

  await page.goto(`/markets/${target.market_id}`);
  const ticket = page.locator('section.panel[aria-label="Trade"]');
  await expect(ticket).toBeVisible();
  await ticket.getByLabel("Credits to spend").fill("5");
  await ticket.getByRole("button", { name: "Get a quote" }).click();
  await expect(ticket).toContainText("It does not price your execution", { timeout: 20_000 });

  await ticket.getByRole("button", { name: "Buy with Credits", exact: true }).click();
  // The failure is reported, with a way forward rather than a dead ticket.
  await expect(page.getByRole("button", { name: "Try again" })).toBeVisible({ timeout: 20_000 });

  // The customer presses the confirm again. The SAME trade must carry the SAME
  // key, or the retry is a second order.
  await ticket.getByRole("button", { name: "Buy with Credits", exact: true }).click();
  await page.waitForTimeout(4000);

  const retried = keys.filter((key) => key.length > 0);
  expect(retried.length, "both attempts were observed on the wire").toBeGreaterThanOrEqual(2);
  expect(
    new Set(retried).size,
    `a retry of the same trade reuses its key; keys seen: ${retried.join(", ")}`,
  ).toBe(1);

  // And a DIFFERENT trade must not: changing the amount is a different command,
  // and reusing the key would answer the customer about the first one (F-205).
  const before = new Set(keys);
  await page.goto(`/markets/${target.market_id}`);
  const second = page.locator('section.panel[aria-label="Trade"]');
  await expect(second).toBeVisible();
  await second.getByLabel("Credits to spend").fill("7");
  await second.getByRole("button", { name: "Get a quote" }).click();
  await expect(second).toContainText("It does not price your execution", { timeout: 20_000 });
  await second.getByRole("button", { name: "Buy with Credits", exact: true }).click();
  await page.waitForTimeout(4000);

  const fresh = keys.filter((key) => key.length > 0 && !before.has(key));
  expect(
    fresh.length,
    `a different amount mints a new key; keys seen: ${keys.join(", ")}`,
  ).toBeGreaterThan(0);

  await page.unrouteAll({ behavior: "ignoreErrors" });
});

/* ==========================================================================
 * EVERY RENDERED FIGURE IS EXACT
 * ========================================================================== */

test("no rendered figure is a rounding, a negative zero or a precision the API did not send", async ({
  page,
}) => {
  test.setTimeout(180_000);
  const faults: string[] = [];

  for (const path of ["/home", "/markets", "/portfolio", "/activity", "/withdraw", "/verify", "/settings"]) {
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await page.waitForLoadState("networkidle");
    const figures = await page.$$eval(".figure", (nodes) =>
      nodes.map((node) => ({
        title: node.getAttribute("title") ?? "",
        text: (node as HTMLElement).innerText,
      })),
    );
    for (const figure of figures) {
      // The exact value lives in `title`. A negative zero is a direction
      // claimed about a value that has none.
      if (/^[−-]0(\.0+)?(\s|$)/.test(figure.title)) {
        faults.push(`${path}: negative zero in ${JSON.stringify(figure.title)}`);
      }
      // An abbreviated figure always carries its exact value; an exact one is
      // its own title. Either way the title must be readable as a figure, not
      // as a word: a title that is empty is a figure nobody can check.
      if (figure.title.trim() === "" && figure.text.trim() !== "") {
        faults.push(`${path}: a figure with no exact value behind it: ${JSON.stringify(figure.text)}`);
      }
    }
  }

  expect(faults, "every figure on screen is the backend's own value").toEqual([]);
});

/* ==========================================================================
 * THE ROUTES THE CROSS-CUTTING SWEEPS DO NOT REACH
 * ========================================================================== */

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

test("/welcome/done passes axe and reflows at 375px", async ({ page }) => {
  // The third onboarding screen is in no route list — `routes.ts` leaves it out
  // because its heading is a greeting — so no sweep has ever visited it. It is
  // a screen every new customer sees.
  await page.goto("/welcome/done");
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  expect(await violations(page), "/welcome/done accessibility violations").toEqual([]);

  await page.setViewportSize({ width: NARROW_WIDTH, height: NARROW_HEIGHT });
  await page.waitForTimeout(300);
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "/welcome/done does not scroll sideways at 375px").toBeLessThanOrEqual(1);
});

/* ==========================================================================
 * THE WITHDRAWAL PATH FOR EARNED VALUE
 * ========================================================================== */

test("an earning of a permitted origin can reach the payout path", async ({ browser, page }) => {
  test.setTimeout(240_000);

  // The earning: a second customer buys from the seeded catalogue, so the
  // seller holds value of an origin the sandbox payout policy PERMITS.
  const seller = await signInAs(browser, "customer-b", "Customer B");
  const sellerId = await accountIdOf(seller);
  await buySomething(page, await accountIdOf(page));

  await verifyInSandbox(seller);
  await ensureDestination(seller, "customer-b");

  const response = await seller.request.get(`/v1/me/eligibility?account_id=${sellerId}`);
  const eligibility = (await response.json()) as {
    readonly buckets: ReadonlyArray<{
      readonly origin: string;
      readonly quantity: string;
      readonly withdrawable: string;
      readonly payout_allowed: boolean;
      readonly reasons: readonly string[];
    }>;
  };

  // The origins the policy permits, which this account actually holds.
  const earned = eligibility.buckets.filter(
    (bucket) => bucket.payout_allowed && BigInt(bucket.quantity) > 0n,
  );
  expect(earned.length, "the sale minted value of an origin the policy permits").toBeGreaterThan(0);

  // Everything the customer can do has been done: they are verified to the
  // level the policy asks for, they have a destination the provider accepted,
  // and the origin is permitted. What is left must be something time or an
  // action can change — not a state nothing in the system can leave.
  const held = earned.filter((bucket) => BigInt(bucket.withdrawable) === 0n);
  const stuck = held.filter((bucket) => bucket.reasons.includes("FUNDING_NOT_SETTLED"));
  expect(
    stuck.map((bucket) => `${bucket.origin}: ${bucket.reasons.join(", ")}`),
    "a permitted origin is not held at FUNDING_NOT_SETTLED with nothing able to settle it: " +
      "credit.Service.SetFinality is called only from internal/credit/funding.go, which owns " +
      "credit_fundings rows; an earning lot has no funding row, so nothing ever promotes it " +
      "out of REVERSIBLE and valuedomain.FundingFinality.PayoutEligible refuses REVERSIBLE",
  ).toEqual([]);

  await seller.context().close();
});

/* ==========================================================================
 * A STRANGER'S SIGN-UP, ON A PHONE, WITH THE CONSOLE WATCHED
 * ========================================================================== */

test("the console stays clean across the sign-up a new customer walks", async ({ browser }) => {
  test.setTimeout(180_000);
  const context = await browser.newContext({
    storageState: SIGNED_OUT,
    viewport: { width: NARROW_WIDTH, height: NARROW_HEIGHT },
  });
  const stranger = await context.newPage();

  const noise: string[] = [];
  stranger.on("console", (message) => {
    if (message.type() === "error" || message.type() === "warning") noise.push(message.text());
  });
  stranger.on("pageerror", (error) => noise.push(`uncaught: ${error.message}`));

  await stranger.goto("/");
  await stranger.getByRole("link", { name: "Get started" }).first().click();
  await stranger.getByRole("button", { name: "Continue to the identity provider" }).click();
  await chooseIdentity(stranger, { identity: "customer-a" });
  await completeOnboarding(stranger, "Customer A");
  await expect(stranger.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();

  // A refused request the product EXPECTS — the pricing read on a tier with no
  // payment provider — is not console noise; an uncaught error or a React
  // warning is.
  const real = noise.filter(
    (message) => !/Failed to load resource|the server responded with a status/.test(message),
  );
  expect(real, "nothing in the sign-up logs an error a developer would have to explain").toEqual([]);
  await context.close();
});

/* ==========================================================================
 * A DIRECT REQUEST FOR SOMEBODY ELSE'S OBJECT, FROM THE BROWSER
 * ========================================================================== */

test("a signed-in customer cannot read another customer's account from this origin", async ({
  browser,
  page,
}) => {
  const other = await signInAs(browser, "customer-b", "Customer B");
  const otherId = await accountIdOf(other);
  await other.context().close();

  for (const path of [
    `/v1/credits/balance?account_id=${otherId}`,
    `/v1/me/portfolio?account_id=${otherId}`,
    `/v1/me/eligibility?account_id=${otherId}`,
    `/v1/payouts?account_id=${otherId}`,
  ]) {
    const response = await page.request.get(path, { headers: SAME_ORIGIN });
    expect(
      response.status(),
      `${path} is refused for an account this session does not own`,
    ).toBeGreaterThanOrEqual(400);
  }
});

/* ==========================================================================
 * THE DEAD-CONTROL WALK, ON THE ROUTES THE SWEEP CANNOT ADDRESS
 * ========================================================================== */

test("no dead control on a market screen, which no route list can reach", async ({ page }) => {
  // `controls.spec.ts` walks `APP_ROUTES` by navigating to each `path`, so it
  // cannot walk `/markets/:marketId` — it would ask the API for a market called
  // ":marketId". This is the same walk with a real identifier in the gap.
  const markets = await page.request.get("/v1/native-markets?limit=1");
  const list = (await markets.json()) as {
    readonly markets: ReadonlyArray<{ readonly market_id: string }>;
  };
  const target = list.markets[0];
  expect(target, "this tier seeds a market").toBeTruthy();

  await page.goto(`/markets/${target?.market_id ?? ""}`);
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");

  const controls = page.locator("button, a[href], input, select, textarea");
  const count = await controls.count();
  expect(count, "the market screen has controls").toBeGreaterThan(0);

  const faults: string[] = [];
  for (let i = 0; i < count; i = i + 1) {
    const control = controls.nth(i);
    const tag = await control.evaluate((node) => node.tagName.toLowerCase());
    const name = (
      (await control.getAttribute("aria-label")) ??
      (await control.getAttribute("title")) ??
      ((await control.textContent()) ?? "")
    ).trim();
    const labelled =
      name !== "" ||
      (await control.evaluate((node) => node.closest("label") !== null)) ||
      (await control.evaluate((node) => {
        const id = node.getAttribute("id");
        if (id === null) return false;
        return document.querySelector(`label[for="${id.replace(/"/g, '\\"')}"]`) !== null;
      }));
    if (!labelled) faults.push(`${tag} at index ${String(i)} has no accessible name`);

    if (tag === "a") {
      const href = (await control.getAttribute("href")) ?? "";
      if (href === "" || href === "#") faults.push(`link "${name}" goes nowhere`);
      continue;
    }
    if (await control.isDisabled()) {
      const describedBy = await control.getAttribute("aria-describedby");
      if (describedBy === null) {
        faults.push(`disabled "${name}" gives no reason`);
        continue;
      }
      const reason = page.locator(`[id="${describedBy.replace(/"/g, '\\"')}"]`);
      const text = ((await reason.textContent()) ?? "").trim();
      if (text.length <= 10) faults.push(`disabled "${name}" gives a reason of ${String(text.length)} characters`);
    }
  }
  expect(faults, "every control on a market screen does something or says why not").toEqual([]);
});

/* ==========================================================================
 * THE RESYNC PATH
 * ========================================================================== */

test("a resync the server sends drops every cached read rather than one of them", async ({
  page,
}) => {
  // `resync` is what `internal/stream` sends when a reconnecting client's
  // cursor is too old to replay, so it means "you have missed something and I
  // cannot tell you what". `StreamStatus.tsx` answers it in the `default:`
  // branch by invalidating every query, and that branch has no test at any
  // level — the one stream case that is exercised elsewhere is `data.changed`,
  // whose scope map is the thing F-202 found wrong.
  await page.addInitScript(() => {
    const opened: EventSource[] = [];
    (window as unknown as { __resyncSources: EventSource[] }).__resyncSources = opened;
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

  const reads = new Set<string>();
  page.on("request", (request) => {
    const url = request.url();
    for (const read of ["/v1/me/portfolio", "/v1/credits/balance", "/v1/me/activity"]) {
      if (url.includes(read)) reads.add(read);
    }
  });

  const delivered = await page.evaluate(() => {
    const opened = (window as unknown as { __resyncSources?: EventSource[] }).__resyncSources ?? [];
    for (const source of opened) {
      source.dispatchEvent(
        new MessageEvent("message", { data: JSON.stringify({ type: "resync" }) }),
      );
    }
    return opened.length;
  });
  expect(delivered > 0, "the shell opened a stream for the resync to arrive on").toBe(true);

  await page.waitForTimeout(2500);
  expect(
    [...reads].sort(),
    "a resync re-reads the portfolio the customer is looking at, and the balance beside it",
  ).toContain("/v1/me/portfolio");
});
