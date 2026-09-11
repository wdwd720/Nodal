/**
 * Scenario A — a new user, as far as this build goes.
 *
 * `docs/product/STAGING_E2E.md` defines A as landing → Get started → identity →
 * callback → `/welcome` → terms → `/welcome/done` → `/home`. The three
 * onboarding screens are built against `GET /v1/me`'s `profile` and
 * `onboarding` fields and `POST /v1/me/terms-acceptances`, none of which this
 * build's API exposes yet. Rather than assert against a screen that does not
 * exist, this spec walks the part that does — landing, Get started, the real
 * OIDC flow, and the dashboard — and the onboarding steps are added to this
 * file when the routes land. A skipped test would look like coverage; a spec
 * that walks half the path and says so is coverage.
 *
 * Nothing here is stubbed. The identity provider is the real one (the
 * development picker locally), the callback sets a real session cookie, and the
 * dashboard is drawn from real responses.
 */
import { expect, test, type Browser, type Page } from "@playwright/test";

const SIGNED_OUT = { cookies: [], origins: [] };

/** The identity the seeded database owns. */
const IDENTITY = "customer-a";

async function signedOutPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  return context.newPage();
}

/**
 * Completes the identity provider's own flow.
 *
 * The development provider renders a picker; choosing an identity is a real
 * navigation that ends at the OIDC callback, which sets the session cookie and
 * redirects back into the application.
 *
 * `mfa` chooses the picker's second link, which asserts a strong `amr`. It is
 * required for a step-up: the backend refuses the callback of a `step_up=true`
 * flow with `STEP_UP_REQUIRED` unless the provider actually asserted strong
 * authentication. That check lives in `internal/identity/login.go` rather than
 * being taken on trust from the request, which is the whole point of one.
 */
async function chooseIdentity(page: Page, options?: { readonly mfa?: boolean }): Promise<void> {
  await expect(page.getByRole("heading", { name: "Choose an identity" })).toBeVisible();
  const row = page.locator("li", { has: page.locator("code", { hasText: new RegExp(`^${IDENTITY}$`) }) });
  const label = options?.mfa === true ? "sign in with MFA" : "sign in";
  await row.getByRole("link", { name: label, exact: true }).first().click();
}

test("a visitor reaches the dashboard from the landing page", async ({ browser }) => {
  const page = await signedOutPage(browser);

  // The landing page renders with no session at all: no request for `/v1/me`
  // has to succeed for a visitor to read what this product is.
  await page.goto("/");
  await expect(
    page.getByRole("heading", { level: 1, name: "A control plane between your capital and markets." }),
  ).toBeVisible();
  // And not one figure is shown to somebody with no account — the example
  // surfaces are labelled, which is what makes them not a figure about anybody.
  await expect(page.getByText("Example data, not a live account").first()).toBeVisible();

  await page.getByRole("link", { name: "Get started" }).first().click();
  await expect(page.getByRole("heading", { level: 1, name: "Get started" })).toBeVisible();
  // The page says what pressing the button does before it is pressed.
  await expect(page.getByText("An e-mail address and a password")).toBeVisible();

  await page.getByRole("button", { name: "Continue to the identity provider" }).click();
  await chooseIdentity(page);

  // The callback lands on `/`, and a signed-in visitor there is sent to the
  // dashboard rather than shown the marketing page.
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  await expect(page).toHaveURL(/\/home$/);

  await page.context().close();
});

test("a deep link while signed out comes back to the page it asked for", async ({ browser }) => {
  const page = await signedOutPage(browser);

  // USER_JOURNEY §10: an application route with no session routes to sign-in
  // with a return path — it does not show an empty dashboard, and it does not
  // claim the visitor is signed out when the truth is that nobody asked.
  await page.goto("/portfolio");
  await expect(page).toHaveURL("/sign-in?return=%2Fportfolio");
  await expect(page.getByRole("heading", { level: 1, name: "Sign in" })).toBeVisible();
  await expect(page.getByText("Returning to /portfolio")).toBeVisible();

  await page.getByRole("button", { name: "Continue to sign in" }).click();
  await chooseIdentity(page);

  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();
  await expect(page).toHaveURL(/\/portfolio$/);

  await page.context().close();
});

test("a foreign return path is not followed", async ({ browser }) => {
  // The value is re-validated on the way in as well as on the way out, so a
  // hand-edited query string cannot turn the sign-in page into an open
  // redirect.
  const page = await signedOutPage(browser);
  await page.goto("/sign-in?return=https%3A%2F%2Fexample.invalid%2Fsteal");
  await expect(page.getByRole("heading", { level: 1, name: "Sign in" })).toBeVisible();
  const stored = await page.evaluate(() => window.sessionStorage.getItem("nodal.return-path"));
  expect(stored, "a foreign path is never remembered").toBe("/home");
  await page.context().close();
});

test("a step-up round trip brings back what was typed", async ({ page }) => {
  // §10 again, from the other side: a sensitive action that needs a stronger
  // sign-in keeps the form state, goes through the provider, and comes back to
  // the same page with the same form. The round trip destroys every value in
  // the tab, so this is the only way to prove the claim.
  await page.goto("/create-asset");
  await expect(page.getByRole("heading", { level: 1, name: "Create asset" })).toBeVisible();

  const NAME = "Round trip asset";
  await page.getByLabel("Name").fill(NAME);

  await page.goto("/sign-in?step=up&return=%2Fcreate-asset");
  await expect(page.getByRole("heading", { level: 1, name: "Confirm it's you" })).toBeVisible();
  await page.getByRole("button", { name: "Confirm it's you" }).click();
  await chooseIdentity(page, { mfa: true });

  await expect(page.getByRole("heading", { level: 1, name: "Create asset" })).toBeVisible();
  await expect(page).toHaveURL(/\/create-asset$/);
  await expect(page.getByLabel("Name")).toHaveValue(NAME);
});

test("the landing page names the boundary before it names the product", async ({ browser }) => {
  // Goal §5: the claims that may never be made are made as denials, and they
  // are above the fold of the second section rather than in the footer.
  const page = await signedOutPage(browser);
  await page.goto("/");
  const section = page.locator("section.site-section", { hasText: "What Nodal is, and what it is not" });
  await expect(section).toBeVisible();
  await expect(section).toContainText("Not an exchange, a broker, a bank or a custodian.");
  await expect(section).toContainText("Not regulated, licensed or approved by anybody, anywhere.");
  await page.context().close();
});

test("the policy documents are readable before there is an account to accept them with", async ({
  browser,
}) => {
  const page = await signedOutPage(browser);
  for (const [path, heading, version] of [
    ["/terms", "Product terms", "terms-v1"],
    ["/privacy", "Privacy", "privacy-v1"],
    ["/risk", "Risk disclosure", "risk-v1"],
  ] as const) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
    await expect(page.getByText(`version ${version}`).first()).toBeVisible();
    await expect(page.getByText("This document has not been reviewed by a lawyer.")).toBeVisible();
  }
  await page.context().close();
});
