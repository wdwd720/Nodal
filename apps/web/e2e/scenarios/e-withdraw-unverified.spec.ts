/**
 * Scenario E — a withdrawal by somebody who is not verified.
 *
 * `docs/product/STAGING_E2E.md` defines E as `/withdraw` → the explanation →
 * "Start verification", plus a direct `POST /v1/payouts` refused with the same
 * reason. What must not happen is named just as precisely: hiding Withdraw, and
 * a payout at `NODAL_IDENTITY`.
 *
 * So this asserts three things that are easy to get wrong in opposite ways.
 *
 * THE PAGE IS THERE. Goal §19 is about the customer who cannot withdraw: they
 * are shown what withdrawal is, what would have to be true, and the next step.
 * A product that hid the page would pass a narrower test and fail the goal.
 *
 * THE SENTENCE IS THE PERMITTED ONE. "Verify your identity to enable withdrawal
 * eligibility" is what this product can promise. The wording §19 forbids offers
 * verification as a way of turning Credits into money, and the honesty suite
 * scans for the word; this asserts the positive sentence is actually present,
 * because a page can avoid a banned word by saying nothing at all.
 *
 * THE UI IS NOT THE GATE. The same refusal comes back from the API when the
 * page is bypassed entirely, with a request that is correct in every other
 * respect. If that ever stopped being true, the interface would be the only
 * thing standing between an unverified account and a payout.
 */
import { expect, test } from "@playwright/test";

import { SAME_ORIGIN, accountIdOf } from "../withdrawal.ts";

test("an unverified account is told what withdrawal is and what it needs", async ({ page }) => {
  await page.goto("/withdraw");
  await expect(page.getByRole("heading", { level: 1, name: "Withdraw" })).toBeVisible();
  await page.waitForLoadState("networkidle");

  // The page exists for everybody. It is reached from the masthead rather than
  // being hidden from the accounts that cannot use it.
  await expect(
    page.getByText("Credits are internal platform value and are not directly withdrawable"),
  ).toBeVisible();
  await expect(page.getByText("A verified financial profile.")).toBeVisible();
  await expect(page.getByText("An approved payout destination.")).toBeVisible();

  // What verification asks for, in the words the product is allowed to use.
  await expect(
    page.getByText("Verification asks a provider to check identity, age, jurisdiction and sanctions"),
  ).toBeVisible();
  await expect(page.getByText("It does not change what a Credit is and it moves nothing")).toBeVisible();

  // And the next action is a real link to a real page.
  const toVerify = page.getByRole("link", { name: "Identity verification" });
  await expect(toVerify).toBeVisible();
  await toVerify.click();
  await expect(page.getByRole("heading", { level: 1, name: "Verify your identity" })).toBeVisible();
  await expect(
    page.getByText(
      "Verification establishes who you are. It is what withdrawal eligibility needs, and it changes nothing about what your Credits are.",
    ),
  ).toBeVisible();
});

test("the eligible figure is per origin, and nothing is rendered as a bare total", async ({ page }) => {
  await page.goto("/withdraw");
  await page.waitForLoadState("networkidle");

  const breakdown = page.getByRole("group", { name: /Your Credits by origin/ });
  await expect(breakdown).toBeVisible();

  // The seeded balance is PROMOTIONAL, which the sandbox policy refuses on
  // purpose, and the page says which origin and why rather than showing one
  // number that would be true of nothing.
  await expect(breakdown.getByText("PROMOTIONAL").first()).toBeVisible();
  await expect(
    page.getByText("The policy does not permit value of this origin to leave").first(),
  ).toBeVisible();

  // The gross is shown and is explicitly NOT what can leave.
  await expect(page.getByText("Gross. It is not what can leave.")).toBeVisible();
});

/**
 * Two buckets of ONE origin, which is what the eligibility answer has returned
 * since F-272 and what the table could not tell apart until F-280.
 *
 * The API returns one bucket per (origin, origin floor, root set, finality),
 * because those four are what the payout policy reads about the value itself.
 * So an account holding trading proceeds out of a purchase and trading proceeds
 * out of a grant gets two rows with one label, one saying value may leave and
 * one saying none may, and until this change nothing on the page said which was
 * which — and the list was keyed on `origin`, which is no longer unique.
 *
 * Producing that state through the product would need purchased value behind
 * one trade and granted value behind another, and every Credit on a seeded tier
 * is a grant. So the ANSWER is composed here and the PAGE is what is under
 * test: the figures below are this test's own, they are never written anywhere,
 * and everything else in the payload is the real one this account got.
 */
test("two buckets of one origin render as two rows that say where each came from", async ({
  page,
}) => {
  const accountId = await accountIdOf(page);
  const real = await page.request.get(`/v1/me/eligibility?account_id=${accountId}`);
  expect(real.status(), "the real answer is readable").toBe(200);
  const answer = (await real.json()) as Record<string, unknown>;

  const composed = {
    ...answer,
    withdrawable_now: "200000000",
    buckets: [
      {
        origin: "MARKET_TRADING_PROCEEDS",
        origin_floor: "MARKET_TRADING_PROCEEDS",
        root_origins: ["MARKET_TRADING_PROCEEDS"],
        finality: "SETTLED",
        quantity: "200000000",
        withdrawable: "200000000",
        payout_allowed: true,
        consumption_rank: 6,
        reasons: [],
      },
      {
        origin: "MARKET_TRADING_PROCEEDS",
        origin_floor: "PROMOTIONAL",
        root_origins: ["PROMOTIONAL", "MARKET_TRADING_PROCEEDS"],
        refused_root: "PROMOTIONAL",
        finality: "SETTLED",
        quantity: "150000000",
        withdrawable: "0",
        payout_allowed: false,
        consumption_rank: 6,
        reasons: ["ORIGIN_NOT_WITHDRAWABLE"],
      },
    ],
  };
  await page.route("**/v1/me/eligibility**", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(composed),
    });
  });

  await page.goto("/withdraw");
  await page.waitForLoadState("networkidle");
  const breakdown = page.getByRole("group", { name: /Your Credits by origin/ });
  await expect(breakdown).toBeVisible();

  // Two rows, not one. A page that keyed on the origin would render whichever
  // of them React reconciled last.
  const rows = breakdown.getByRole("row").filter({ hasText: "MARKET_TRADING_PROCEEDS" });
  await expect(rows).toHaveCount(2);

  // The one nothing else funded says so, and its money is the money that may
  // leave.
  const own = rows.filter({ hasText: "nothing else funded it" });
  await expect(own).toHaveCount(1);
  await expect(own).toContainText("200.00");

  // The one a grant funded names the grant, marks it as the origin this policy
  // refuses, and explains the refusal in words rather than in the enum.
  const fromAGrant = rows.filter({ hasText: "PROMOTIONAL" });
  await expect(fromAGrant).toHaveCount(1);
  await expect(fromAGrant).toContainText("(refused)");
  await expect(fromAGrant).toContainText("This value came from a promotional grant");
  await expect(fromAGrant).toContainText("not the origin it now carries");
});

test("a direct POST /v1/payouts is refused for the same reason the page gives", async ({ page }) => {
  const accountId = await accountIdOf(page);

  const before = await page.request.get(`/v1/payouts?account_id=${accountId}`);
  const countBefore = ((await before.json()) as { readonly items: readonly unknown[] }).items.length;

  // A payout names the quote the customer was shown, and it is required
  // (D-119): without one there is no fee and no minimum to judge the payout
  // against, which is how a payout below the provider's published minimum was
  // once reserved and settled with the fee never taken.
  const noQuote = await page.request.post("/v1/payouts", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `e-noquote-${String(Date.now())}` },
    data: { account_id: accountId, amount: "1000000" },
  });
  expect(noQuote.status(), "a payout with no quote is refused").toBeGreaterThanOrEqual(400);
  const noQuoteProblem = (await noQuote.json()) as {
    readonly code?: string;
    readonly fields?: Record<string, unknown>;
  };
  expect(noQuoteProblem.code).toBe("VALIDATION_FAILED");
  expect(JSON.stringify(noQuoteProblem)).toContain("quote_id");

  // And with the request otherwise correct in every respect -- a real account, a
  // well-formed amount, a well-formed quote id, a fresh idempotency key and the
  // header the app's own fetch sends -- the refusal is still verification's.
  // The route decides that before it ever looks at the quote, which is what
  // makes the page not the gate.
  const response = await page.request.post("/v1/payouts", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `e-direct-${String(Date.now())}` },
    data: {
      account_id: accountId,
      amount: "1000000",
      // A well-formed quote id that names no quote. It is a version 7 UUID
      // because internal/id accepts no other version (F-131), so a v4 here
      // would be refused for its shape and prove nothing about the order the
      // route decides in.
      quote_id: "01900000-0000-7000-8000-000000000001",
    },
  });

  expect(response.status(), "the API refuses an unverified payout").toBeGreaterThanOrEqual(400);
  const problem = (await response.json()) as {
    readonly code?: string;
    readonly detail?: string;
    readonly fields?: Record<string, unknown>;
  };
  expect(problem.code, "the refusal is about verification, not a generic denial").toBe(
    "VERIFICATION_REQUIRED",
  );
  // The router's own reason code travels with it, so the refusal is checkable
  // against the policy that produced it rather than taken on trust.
  expect(JSON.stringify(problem.fields ?? {})).toContain("PAYOUT_REQUIRES_VERIFICATION");

  // Nothing was created. A refusal that left a DRAFT behind would be a refusal
  // that reserved something.
  const after = await page.request.get(`/v1/payouts?account_id=${accountId}`);
  const countAfter = ((await after.json()) as { readonly items: readonly unknown[] }).items.length;
  expect(countAfter, "no payout request was recorded").toBe(countBefore);
});

test("no payout is possible at NODAL_IDENTITY, and the API says so in the eligibility answer", async ({
  page,
}) => {
  const accountId = await accountIdOf(page);
  const response = await page.request.get(`/v1/me/eligibility?account_id=${accountId}`);
  expect(response.status()).toBe(200);
  const eligibility = (await response.json()) as {
    readonly eligible: boolean;
    readonly withdrawable_now: string;
    readonly current_verification: string;
    readonly required_verification: string;
    readonly reasons: readonly string[];
  };

  expect(eligibility.eligible, "nothing may be withdrawn at this level").toBe(false);
  expect(eligibility.withdrawable_now).toBe("0");
  expect(eligibility.current_verification).toBe("NODAL_IDENTITY");
  expect(eligibility.required_verification).toBe("PAYOUT_KYC");
  // Every reason names something that could change, which is what makes the
  // answer actionable rather than a shrug.
  expect(eligibility.reasons.length).toBeGreaterThan(0);
});
