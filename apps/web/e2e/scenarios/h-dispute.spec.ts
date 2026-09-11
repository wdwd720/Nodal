/**
 * Scenario H — a dispute or chargeback after the Credits were traded.
 *
 * `docs/product/STAGING_E2E.md` defines H as purchase → trade → dispute webhook
 * → frozen bucket, restricted state, reconciliation record, and its "must not
 * happen" is one sentence: **a hidden freeze**. A person whose value has been
 * frozen has to be able to see that it is frozen and why, on a page they can
 * find, without asking anybody.
 *
 * So that is what this asserts, and it asserts it whether or not a dispute
 * exists on the account under test. The three obligations are structural:
 *
 *   1. Home shows the frozen bucket as its own labelled figure, separate from
 *      the total and from what is spendable;
 *   2. `/settings/account` shows every restriction the backend reports, with
 *      its stable code and the message written for the person it applies to —
 *      and says "nothing is restricted" only when the backend returned none,
 *      never as a default for a request that failed;
 *   3. `ACCOUNT_RESTRICTED` and `CREDIT_PURCHASE_REVERSED` cannot be switched
 *      off in the notification preferences, so a freeze cannot be silenced.
 *
 * Driving the dispute itself needs what scenario G needs and for the same
 * reasons — a configured credit-purchase provider (there is no fake mode), a
 * captured funding, and the webhook secret — and the drive runs when
 * `CP_WEB_STRIPE_WEBHOOK_SECRET` and `CP_WEB_CAPTURED_CHARGE_ID` are set.
 */
import { createHmac } from "node:crypto";

import { expect, test, type Page } from "@playwright/test";

const WEBHOOK_SECRET = process.env["CP_WEB_STRIPE_WEBHOOK_SECRET"] ?? "";
const CAPTURED_CHARGE = process.env["CP_WEB_CAPTURED_CHARGE_ID"] ?? "";
const CAN_DRIVE = WEBHOOK_SECRET !== "" && CAPTURED_CHARGE !== "";

interface Restriction {
  code: string;
  message: string;
  account_id?: string;
}

async function accountId(page: Page): Promise<string> {
  const response = await page.request.get("/v1/me");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as { account_ids: string[] };
  const id = body.account_ids[0];
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

function signature(payload: string, secret: string): string {
  const t = String(Date.now()).slice(0, -3);
  const mac = createHmac("sha256", secret).update(`${t}.${payload}`).digest("hex");
  return `t=${t},v1=${mac}`;
}

test("the freeze is visible on Home, with the two axes kept apart", async ({ page }) => {
  const id = await accountId(page);
  const response = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(response.ok()).toBeTruthy();

  await page.goto("/home");
  const credits = page.locator(".panel", { hasText: "Credits" }).first();
  await expect(credits).toBeVisible();

  // Frozen has its own label, its own figure and its own sentence saying what
  // put it there. It is never merely absent from what is spendable.
  const frozen = credits.locator('.field:has(dt:text-is("Frozen"))');
  await expect(frozen).toHaveCount(1);
  await expect(frozen).toContainText("restriction or an open dispute");

  // The bar that draws the same fact, with the whole taken from the same
  // response the parts came from rather than summed in the browser.
  await expect(credits.getByRole("img", { name: /How this balance is held/ })).toBeVisible();
});

test("every restriction the backend reports is shown with its reason", async ({ page }) => {
  const response = await page.request.get("/v1/me/account");
  expect(response.ok(), "the API reports where this account stands").toBeTruthy();
  const body = (await response.json()) as {
    user_status: string;
    restrictions: Restriction[];
    cooling_off_days: number;
  };

  await page.goto("/settings/account");
  await expect(page.getByRole("heading", { level: 1, name: "Account" })).toBeVisible();

  // The standing itself, as the backend words it.
  await expect(page.locator(".panel", { hasText: "Standing" })).toContainText(body.user_status);

  const panel = page.locator(".panel", { hasText: "Restrictions" });
  await expect(panel).toBeVisible();

  if (body.restrictions.length === 0) {
    // "Nothing is restricted" is a claim about the account, and it is made only
    // because the backend said so — never as the default for a page that could
    // not read the answer.
    await expect(panel).toContainText("Nothing is restricted");
    await expect(panel.locator(".refusal")).toHaveCount(0);
    return;
  }

  for (const restriction of body.restrictions) {
    const rendered = panel.locator(".refusal", { hasText: restriction.code });
    await expect(rendered, `${restriction.code} is on screen`).toBeVisible();
    // The message is the one written for the person it applies to. The free
    // text an operator wrote for other operators is not in this response and
    // must not be invented in its place.
    await expect(rendered).toContainText(restriction.message);
  }
});

test("a freeze cannot be silenced", async ({ page }) => {
  // The preferences page is where a person would go to turn this off, so it is
  // where the fact that they cannot has to be stated. `enforced: false` on the
  // contract means the kind is always sent; the page renders it as fixed
  // rather than as a switch that quietly does nothing.
  const response = await page.request.get("/v1/me/notification-preferences");
  expect(response.ok()).toBeTruthy();
  const body = (await response.json()) as {
    items: Array<{ kind: string; enabled: boolean; enforced: boolean }>;
  };

  await page.goto("/notifications");
  const panel = page.locator(".panel", { hasText: "What you are notified about" });
  await expect(panel).toBeVisible();

  for (const pref of body.items) {
    const row = panel.locator("tr", { has: page.getByText(pref.kind, { exact: true }) });
    await expect(row, `${pref.kind} has a row`).toHaveCount(1);
    if (pref.enforced) {
      await expect(row.locator("input[type=checkbox]")).toHaveCount(1);
    } else {
      await expect(row).toContainText("always sent");
      await expect(row.locator("input")).toHaveCount(0);
    }
  }

  const unsilenceable = body.items.filter((item) => !item.enforced).map((item) => item.kind);
  expect(
    unsilenceable,
    "a reversed purchase and an account restriction are among the kinds that cannot be switched off",
  ).toEqual(expect.arrayContaining(["CREDIT_PURCHASE_REVERSED", "ACCOUNT_RESTRICTED"]));
});

test("an unsigned dispute delivery is never accepted", async ({ page }) => {
  // Provable everywhere: the ingestion route does not accept an unsigned
  // dispute. A freeze somebody could cause by POSTing JSON would be worse than
  // one nobody can see. It is its own test so that the driven test below is
  // counted as what it is on a run that cannot drive it (F-252).
  const unsigned = await page.request.post("/v1/webhooks/stripe_credit", {
    headers: { "Content-Type": "application/json" },
    data: { id: "evt_e2e_unsigned", object: "event", type: "charge.dispute.created" },
  });
  expect(
    unsigned.status(),
    "an unsigned delivery is refused (400) or the provider is not registered (404) — never accepted",
  ).not.toBe(200);
});

test("the dispute webhook freezes value the customer can see is frozen", async ({ page }) => {
  test.info().annotations.push({
    type: "requires",
    description:
      "CP_WEB_STRIPE_WEBHOOK_SECRET and CP_WEB_CAPTURED_CHARGE_ID, plus an API with a " +
      "configured credit-purchase provider. There is no fake mode for it.",
  });
  test.skip(!CAN_DRIVE, "driving a dispute needs CP_WEB_STRIPE_WEBHOOK_SECRET and CP_WEB_CAPTURED_CHARGE_ID and an API with a configured credit-purchase provider; this run has none of them (goal §59: no key on this tier)");

  const payload = JSON.stringify({
    id: `evt_e2e_${String(Date.now())}`,
    object: "event",
    type: "charge.dispute.created",
    livemode: false,
    data: {
      object: {
        id: `dp_e2e_${String(Date.now())}`,
        object: "dispute",
        charge: CAPTURED_CHARGE,
        status: "needs_response",
      },
    },
  });
  const delivered = await page.request.post("/v1/webhooks/stripe_credit", {
    headers: {
      "Content-Type": "application/json",
      "Stripe-Signature": signature(payload, WEBHOOK_SECRET),
    },
    data: payload,
  });
  expect(delivered.status(), "a correctly signed delivery is acknowledged").toBe(200);

  const id = await accountId(page);
  const balance = await page.request.get(`/v1/credits/balance?account_id=${id}`);
  expect(balance.ok()).toBeTruthy();
  const after = (await balance.json()) as Record<string, string>;
  expect(/[1-9]/.test(after["frozen"] ?? ""), "the dispute froze value").toBe(true);

  await page.goto("/home");
  const credits = page.locator(".panel", { hasText: "Credits" }).first();
  await expect(credits.locator('.field:has(dt:text-is("Frozen")) .figure')).toBeVisible();
});
