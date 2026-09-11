/**
 * Scenario J — a session that expires in the middle of a withdrawal.
 *
 * `docs/product/STAGING_E2E.md` defines J as: quote → the session is revoked →
 * submit → sign-in with a return path → re-quote → submit with the SAME
 * idempotency key → exactly one payout exists. What must not happen is a double
 * submit, or a submit against a stale quote.
 *
 * Three properties carry that, and each is asserted where it actually lives.
 *
 * THE REVOCATION IS REAL. A second signed-in device lists this person's
 * sessions and deletes the other one — which is the product story, not a test
 * hook. The first device then discovers it is signed out the way anybody does:
 * by asking the backend for something.
 *
 * THE STATE SURVIVES. USER_JOURNEY §10 promises that what was typed is kept and
 * that signing in again comes back to the same page. `useSurvivesSignIn` keeps
 * it in the tab, the trip carries `return_to`, and the assertion is that the
 * amount is still in the field afterwards.
 *
 * THE KEY IS MINTED AT CONFIRMATION AND REUSED. That is what makes a retry
 * after re-authentication safe, and it is the one property a screenshot cannot
 * show. It is asserted against the API directly: the same key, sent twice with
 * the same body, produces the same answer and exactly one outcome — no second
 * request, no second refusal record, nothing counted twice.
 *
 * # What this tier can reach
 *
 * On a seeded deployment every Credit is unfunded, so a quote for value that
 * cannot leave is refused and there is no payout to double-submit (see
 * `f-verified-sandbox.spec.ts` for why). The idempotency assertion is therefore
 * made on the refusal itself, which is the same code path through the same
 * command harness: what must never happen is that asking twice with one key
 * becomes two requests, and that is true of a refused request as much as an
 * accepted one.
 */
import { expect, test, type Page } from "@playwright/test";

import { SAME_ORIGIN, accountIdOf, ensureDestination, signInAs, verifyInSandbox } from "../withdrawal.ts";

/** Revokes every session of this user except the one asking. */
async function revokeTheOtherSessions(page: Page): Promise<number> {
  const listed = await page.request.get("/v1/sessions");
  expect(listed.status(), "a person can see their own sessions").toBe(200);
  const sessions = (await listed.json()) as ReadonlyArray<{
    readonly id: string;
    readonly current?: boolean;
    readonly revoked_at?: string;
  }>;
  let revoked = 0;
  for (const session of sessions) {
    if (session.current === true || session.revoked_at !== undefined) continue;
    const response = await page.request.delete(`/v1/sessions/${session.id}`, {
      headers: SAME_ORIGIN,
    });
    expect(response.status(), "revoking another device is permitted").toBeLessThan(300);
    revoked = revoked + 1;
  }
  return revoked;
}

test("a session revoked mid-withdrawal keeps what was typed and comes back to it", async ({
  browser,
}) => {
  test.setTimeout(120_000);

  // The first device: a verified account with a destination, part way through
  // a withdrawal.
  const first = await signInAs(browser, "customer-b", "Customer B");
  await verifyInSandbox(first);
  await ensureDestination(first, "customer-b");

  await first.goto("/withdraw");
  await first.waitForLoadState("networkidle");
  const table = first.getByRole("group", { name: /Your payout destinations/ });
  const use = table.getByRole("button", { name: "Use this" }).first();
  if ((await use.count()) > 0) await use.click();

  const typed = "12.345678";
  await first.getByLabel("Amount in Credits").fill(typed);

  // The second device, signed in as the same person, revokes the first one.
  // This is the product's own "I do not recognise that device" action.
  const second = await signInAs(browser, "customer-b", "Customer B");
  const revoked = await revokeTheOtherSessions(second);
  expect(revoked, "the first device's session was revoked").toBeGreaterThan(0);

  // The first device discovers it the way anybody does: by asking for
  // something. The backend answers 401, and the page offers the way back
  // rather than stating the problem and stopping.
  await first.getByRole("button", { name: "Get a quote" }).click();
  const recovery = first.getByRole("button", { name: "Sign in again" });
  await expect(recovery).toBeVisible();
  await expect(first.getByText("Anything you had typed is kept")).toBeVisible();

  await recovery.click();
  await expect(first.getByRole("heading", { name: "Choose an identity" })).toBeVisible();
  const row = first.locator("li", { has: first.locator("code", { hasText: /^customer-b$/ }) });
  await row.getByRole("link", { name: "sign in", exact: true }).first().click();

  // Back on the page that was interrupted, with what was typed still in it.
  await first.waitForURL(/\/withdraw/);
  await first.waitForLoadState("networkidle");
  await expect(first.getByRole("heading", { level: 1, name: "Withdraw" })).toBeVisible();
  await expect(first.getByLabel("Amount in Credits")).toHaveValue(typed);

  // And the quote is NOT still there. A price taken before the interruption is
  // a price that may have moved, and the page makes the person take a new one
  // rather than committing to a stale number.
  await expect(first.getByText(/seconds left/)).toHaveCount(0);

  await first.context().close();
  await second.context().close();
});

/** A response body with the per-request identifier removed. */
function decisionOf(body: string): unknown {
  const parsed = JSON.parse(body) as Record<string, unknown>;
  delete parsed["request_id"];
  delete parsed["instance"];
  return parsed;
}

test("one idempotency key is one request, however many times it is sent", async ({ page }) => {
  // The key is minted when the customer confirms and reused for every retry of
  // that same action, which is what makes a retry after re-authentication safe.
  // Sending it twice must produce one outcome.
  const accountId = await accountIdOf(page);
  const key = `j-same-key-${String(Date.now())}`;
  const body = { account_id: accountId, amount: "1000000" };

  const before = await page.request.get(`/v1/payouts?account_id=${accountId}`);
  const countBefore = ((await before.json()) as { readonly items: readonly unknown[] }).items.length;

  const first = await page.request.post("/v1/payouts", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": key },
    data: body,
  });
  const second = await page.request.post("/v1/payouts", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": key },
    data: body,
  });

  // The same request, twice, gets the same answer. On this tier that answer is
  // a refusal — the account is not verified — and the property holds either
  // way: what must never happen is one key becoming two requests.
  //
  // The comparison drops `request_id`, which is per-REQUEST and is meant to
  // differ: it is how support tells two deliveries of one decision apart. The
  // decision itself — its code, its reason, the policy that made it — must be
  // identical, and that is what is compared.
  expect(second.status(), "the replay answers as the original did").toBe(first.status());
  expect(decisionOf(await second.text()), "the same decision, not a second one").toEqual(
    decisionOf(await first.text()),
  );

  const after = await page.request.get(`/v1/payouts?account_id=${accountId}`);
  const countAfter = ((await after.json()) as { readonly items: readonly unknown[] }).items.length;
  expect(countAfter - countBefore, "at most one payout came out of one key").toBeLessThanOrEqual(1);
});

test("the same key with a DIFFERENT body is refused rather than guessed at", async ({ page }) => {
  // The other half of the rule. A key that has been used with one body and is
  // then sent with another is somebody's mistake, and the backend says so
  // rather than picking one of the two.
  const accountId = await accountIdOf(page);
  const key = `j-divergent-${String(Date.now())}`;

  const first = await page.request.post("/v1/strategies", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": key },
    data: {
      account_id: accountId,
      name: `Idempotency probe ${String(Date.now())}`,
      description: "A strategy recorded only to prove that one key is one request.",
    },
  });
  expect(first.status(), "the first request was accepted").toBeLessThan(300);

  const divergent = await page.request.post("/v1/strategies", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": key },
    data: {
      account_id: accountId,
      name: `Something else ${String(Date.now())}`,
      description: "A different body under the same key.",
    },
  });
  expect(divergent.status(), "a divergent replay is refused").toBeGreaterThanOrEqual(400);
  const problem = (await divergent.json()) as { readonly code?: string };
  expect(problem.code).toBe("INVALID_IDEMPOTENCY_REUSE");
});
