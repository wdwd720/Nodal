/**
 * The setup scenarios E, F and J share, and the reasons each step is real.
 *
 * Nothing here fakes a precondition. A withdrawal needs three things that do
 * not exist on a fresh database — value of a withdrawable origin, a verified
 * financial profile and a destination a provider accepts — and every one of
 * them is produced the way a person produces it:
 *
 *   - THE VALUE comes from a sale. Seeded Credits are PROMOTIONAL, and the
 *     sandbox payout policy refuses that origin on purpose: "a promotional
 *     grant that could leave the system would be the first rule somebody
 *     copied". So the buyer buys something from the seller through the real
 *     commerce path, and the SELLER earns CREATOR_EARNING — an origin the
 *     policy permits. That is why the withdrawal scenarios are driven as
 *     customer-b and the purchase is made as customer-a.
 *   - THE VERIFICATION comes from the rehearsal provider, through the sandbox
 *     control the API itself exposes. Nothing writes a verification row from
 *     the browser and nothing defaults to approved.
 *   - THE DESTINATION is a sandbox handle registered through the page, which
 *     the sandbox provider tokenises. No account number exists anywhere in it.
 *
 * `page.request` shares the browser context's cookies but is not a page, so it
 * sends no `Sec-Fetch-Site` and the API's CSRF middleware refuses it —
 * correctly. `SAME_ORIGIN` is the header the app's own fetch sends, and setting
 * it is what makes these calls a test of the business rule rather than a
 * rediscovery of the cross-site one.
 */
import { expect, type Browser, type Page } from "@playwright/test";

import { chooseIdentity, completeOnboarding } from "./onboarding.ts";

/** What a same-origin write from the app itself carries. */
export const SAME_ORIGIN = { "Sec-Fetch-Site": "same-origin" } as const;

/** The jurisdiction the rules table actually offers (US, subdivision required). */
export const COUNTRY = "US";
export const REGION = "CA";

/** A sandbox handle. It is not an account number, and there is nowhere for one to go. */
export function sandboxHandle(): string {
  return `sandbox-handle-${String(Date.now())}`;
}

export async function accountIdOf(page: Page): Promise<string> {
  const response = await page.request.get("/v1/accounts");
  expect(response.status(), "the account list is readable").toBe(200);
  const accounts = (await response.json()) as ReadonlyArray<{ readonly id: string }>;
  expect(accounts.length, "this session owns an account").toBeGreaterThan(0);
  return accounts[0]?.id as string;
}

/** Signs in as a named development identity in its own context, onboarding included. */
export async function signInAs(
  browser: Browser,
  identity: string,
  displayName: string,
): Promise<Page> {
  const context = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  await page.goto("/v1/auth/login");
  await chooseIdentity(page, { identity });
  await completeOnboarding(page, displayName);
  return page;
}

/**
 * Buys the seeded catalogue's most expensive product, so the SELLER ends up
 * holding an earning the payout policy permits to leave.
 *
 * It goes through the API rather than the marketplace screen because the
 * purchase is this spec's precondition and not its subject; scenario B is where
 * buying is the thing under test.
 */
export async function buySomething(buyer: Page, buyerAccountId: string): Promise<void> {
  const listed = await buyer.request.get("/v1/internal-products");
  expect(listed.status(), "the catalogue is readable").toBe(200);
  const catalogue = (await listed.json()) as {
    readonly items: ReadonlyArray<{ readonly product_id: string; readonly price: string }>;
  };
  expect(catalogue.items.length, "the seeded catalogue has something in it").toBeGreaterThan(0);
  const product = [...catalogue.items].sort((a, b) =>
    BigInt(a.price) < BigInt(b.price) ? 1 : BigInt(a.price) > BigInt(b.price) ? -1 : 0,
  )[0];
  expect(product, "a product was chosen").toBeTruthy();

  const order = await buyer.request.post(`/v1/internal-products/${product?.product_id ?? ""}/orders`, {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `f-buy-${String(Date.now())}` },
    data: { account_id: buyerAccountId, expected_price: product?.price ?? "0" },
  });
  expect(order.status(), "the purchase went through").toBeLessThan(300);
}

/**
 * Takes the account through verification using the sandbox control the API
 * exposes, driven through the page exactly as a tester would.
 *
 * It is idempotent: an account the provider has already verified is left alone
 * rather than pushed through a second session.
 */
export async function verifyInSandbox(page: Page): Promise<void> {
  await page.goto("/verify");
  await expect(page.getByRole("heading", { level: 1, name: "Verify your identity" })).toBeVisible();
  await page.waitForLoadState("networkidle");

  if ((await page.getByRole("heading", { level: 2, name: "Verified" }).count()) > 0) return;

  await page.getByLabel("Country").fill(COUNTRY);
  await page.getByLabel("State or region").fill(REGION);
  await page.getByRole("button", { name: "Start verification" }).click();

  // The rehearsal control appears only because the API said this session has
  // one. Its presence is what makes a rehearsal visibly a rehearsal.
  const control = page.getByRole("region", { name: "Sandbox control" });
  await expect(control).toBeVisible();
  await expect(control).toContainText("It is not an approval");
  await control.getByRole("button", { name: "Decide: verified" }).click();

  await expect(page.getByRole("heading", { level: 2, name: "Verified" })).toBeVisible();
}

/**
 * Registers a sandbox destination through the page, if there is not a usable
 * one.
 *
 * `identity` is the development identity this page is signed in as, and it is
 * required rather than inferred: registering needs a step-up, the step-up goes
 * back through the provider's picker, and answering that picker with the wrong
 * identity swaps the session to another account mid-scenario. An earlier
 * version guessed from the profile's display name and did exactly that.
 */
export async function ensureDestination(page: Page, identity: string): Promise<void> {
  await page.goto("/withdraw");
  await expect(page.getByRole("heading", { level: 1, name: "Withdraw" })).toBeVisible();
  await page.waitForLoadState("networkidle");

  const destinations = page.getByRole("region", { name: "Where value would be sent" });
  if ((await destinations.getByRole("button", { name: "Use this" }).count()) > 0) return;
  if ((await destinations.getByText("chosen").count()) > 0) return;

  await destinations.getByRole("button", { name: "Add a destination" }).click();
  await page.getByLabel("The provider's token").fill(sandboxHandle());
  await page.getByLabel("A name for it").fill("Rehearsal account");
  await page.getByLabel("Currency").fill("USD");
  await page.getByLabel("Country").fill(COUNTRY);
  await page.getByRole("button", { name: "Register this destination" }).click();

  // Registering a destination needs a recent STRONG sign-in, so the expected
  // answer the first time is STEP_UP_REQUIRED. That is the system working, and
  // the round trip is part of what this walks: the page offers it, the trip
  // asserts a strong `amr` at the provider, the API sends the browser back to
  // this page, and what was typed is still in the form.
  const confirm = page.getByRole("button", { name: "Confirm it's you" });
  if ((await confirm.count()) > 0) {
    await confirm.first().click();
    await chooseIdentity(page, { identity, mfa: true, waitFor: /\/withdraw/ });
    await page.waitForLoadState("networkidle");
    await expect(
      page.getByLabel("The provider's token"),
      "what was typed survived the trip",
    ).not.toBeEmpty();
    await page.getByRole("button", { name: "Register this destination" }).click();
  }

  await expect(destinations.getByRole("button", { name: "Use this" }).first()).toBeVisible();
}

/** Reads the account's Credit balance straight from the API. */
export async function creditBalance(
  page: Page,
  accountId: string,
): Promise<{ readonly gross: string; readonly spendable: string; readonly payout_eligible: string }> {
  const response = await page.request.get(`/v1/credits/balance?account_id=${accountId}`);
  expect(response.status(), "the credit balance is readable").toBe(200);
  return (await response.json()) as {
    readonly gross: string;
    readonly spendable: string;
    readonly payout_eligible: string;
  };
}
