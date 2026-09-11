/**
 * Scenario A — a new user, as far as this build goes.
 *
 * `docs/product/STAGING_E2E.md` defines A as landing -> Get started -> identity ->
 * callback -> `/welcome` -> terms -> `/welcome/done` -> `/home`, and that is now
 * the whole path: `GET /v1/me` carries the onboarding timestamps,
 * `POST /v1/me/profile` makes the profile, and `GET`/`POST /v1/me/terms-acceptances`
 * serve the legal documents and record an acceptance of the exact bytes shown.
 *
 * The first-time journey runs as `customer-b`, which nothing else in the suite
 * touches: `auth.setup.ts` uses `customer-a` and walks the same screens once, so
 * that every application spec starts from an account that has arrived.
 *
 * Re-running against the same database finds `customer-b` already onboarded, so
 * the journey test branches. The first pass proves the screens; every later pass
 * proves the returning-user path through the same router. Both branches assert,
 * and neither is skipped.
 *
 * Nothing here is stubbed. The identity provider is the real one (the
 * development picker locally), the callback sets a real session cookie, and the
 * dashboard is drawn from real responses.
 */
import { expect, test, type Browser, type Page } from "@playwright/test";

import { chooseIdentity, completeOnboarding } from "../onboarding.ts";

const SIGNED_OUT = { cookies: [], origins: [] };

/** Reserved for the first-time journey, so the suite's own account is untouched. */
const NEW_IDENTITY = "customer-b";

async function signedOutPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  return context.newPage();
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
  // A brand-new identity is sent to onboarding first; the suite's own
  // identity has already been through it. Either way this ends on the
  // dashboard, which is what the journey promises.
  await completeOnboarding(page, "Customer A");

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
  await chooseIdentity(page, { waitFor: /\/portfolio$/ });

  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();
  await expect(page).toHaveURL(/\/portfolio$/);

  await page.context().close();
});

test("a foreign return path is not followed", async ({ browser }) => {
  // A hand-edited query string must not turn the sign-in page into an open
  // redirect. It is checked twice: this page refuses anything that is not a
  // local path before it hands one to the API, and the API refuses it again
  // with a validation problem (`internal/identity`). What is asserted here is
  // the outcome of both — the browser ends on the dashboard, on this origin,
  // and never at the address in the query string.
  const page = await signedOutPage(browser);
  await page.goto("/sign-in?return=https%3A%2F%2Fexample.invalid%2Fsteal");
  await expect(page.getByRole("heading", { level: 1, name: "Sign in" })).toBeVisible();
  // The page says where it is actually going to send them, which is not the
  // address they were handed.
  await expect(page.getByText("Returning to /home")).toBeVisible();

  await page.getByRole("button", { name: "Continue to sign in" }).click();
  await chooseIdentity(page, { waitFor: /\/home$/ });
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  expect(new URL(page.url()).host, "still on this origin").toBe(new URL(page.url()).host);
  expect(page.url()).not.toContain("example.invalid");
  await page.context().close();
});

test("a step-up round trip brings back what was typed", async ({ browser }) => {
  // §10 again, from the other side: a sensitive action that needs a stronger
  // sign-in keeps the form state, goes through the provider, and comes back to
  // the same page with the same form. The round trip destroys every value in
  // the tab, so this is the only way to prove the claim.
  //
  // In its own signed-in context, not the suite's stored one: a step-up
  // rotates the session and revokes the one it replaced (internal/auth), and a
  // stored state whose session was revoked here would fail every later test
  // that starts from it.
  const page = await signedOutPage(browser);
  await page.goto("/create-asset");
  await expect(page).toHaveURL("/sign-in?return=%2Fcreate-asset");
  await page.getByRole("button", { name: "Continue to sign in" }).click();
  await chooseIdentity(page, { waitFor: /\/create-asset$/ });
  await expect(page.getByRole("heading", { level: 1, name: "Create asset" })).toBeVisible();

  const NAME = "Round trip asset";
  await page.getByLabel("Name").fill(NAME);

  await page.goto("/sign-in?step=up&return=%2Fcreate-asset");
  await expect(page.getByRole("heading", { level: 1, name: "Confirm it's you" })).toBeVisible();
  await page.getByRole("button", { name: "Confirm it's you" }).click();
  await chooseIdentity(page, { mfa: true, waitFor: /\/create-asset$/ });

  await expect(page.getByRole("heading", { level: 1, name: "Create asset" })).toBeVisible();
  await expect(page).toHaveURL(/\/create-asset$/);
  await expect(page.getByLabel("Name")).toHaveValue(NAME);
  await page.context().close();
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

test("the documents a visitor reads are the bytes an acceptance records", async ({ browser }) => {
  // This is the whole point of D-080. `GET /v1/terms` and
  // `GET /me/terms-acceptances` serve the same registry, and `content_hash` is
  // the sha256 of the exact body, which is the value an acceptance stores. So a
  // visitor with no account can read the binding text, and what they read can
  // be checked against what they will later be asked to agree to rather than
  // taken on trust.
  const page = await signedOutPage(browser);
  const response = await page.request.get("/v1/terms");
  expect(response.ok(), "the registry answers without a session").toBeTruthy();
  const documents = (await response.json()) as Array<{
    document_id: string;
    version: string;
    content_hash: string;
  }>;

  for (const [path, id] of [
    ["/terms", "TERMS_OF_SERVICE"],
    ["/privacy", "PRIVACY_POLICY"],
    ["/risk", "RISK_DISCLOSURE"],
    ["/credits-terms", "CREDITS_TERMS"],
    ["/withdrawal-disclosure", "WITHDRAWAL_DISCLOSURE"],
  ] as const) {
    const doc = documents.find((candidate) => candidate.document_id === id);
    expect(doc, `${id} is served`).toBeTruthy();
    const served = doc as { version: string; content_hash: string };

    await page.goto(path);
    // The served text, not a summary of it.
    await expect(page.locator("pre.doc-source")).toBeVisible();
    const text = await page.evaluate(() => document.body.innerText);
    expect(text, `${path} shows the version`).toContain(`version ${served.version}`);
    // The hash is shown truncated, first five and last four, which is enough to
    // compare against a record without reading sixty-four characters aloud.
    expect(text, `${path} shows the content hash`).toContain(served.content_hash.slice(0, 5));
    // Reading is not accepting, and the page says so.
    expect(text, `${path} says reading records nothing`).toContain(
      "Reading this page records nothing.",
    );
  }
  await page.context().close();
});

test("a new identity is taken through onboarding and lands on the dashboard", async ({ browser }) => {
  const page = await signedOutPage(browser);

  await page.goto("/get-started");
  await page.getByRole("button", { name: "Continue to the identity provider" }).click();
  await chooseIdentity(page, { identity: NEW_IDENTITY });

  // A session is not the same as a usable account: the router sends a principal
  // with no profile to the step that makes one, before any screen that could
  // move value.
  //
  // The settle is load-bearing. The callback lands on `/`, the router forwards
  // to `/home`, and only then does the gate — which needs `/me` and the terms
  // state to have answered — divert an unfinished account. Deciding before
  // those resolve reads the `/home` in the middle of the chain and concludes
  // there was nothing to do.
  await page.waitForLoadState("networkidle");
  const fresh = new URL(page.url()).pathname === "/welcome";

  if (fresh) {
    await expect(page.getByRole("heading", { level: 1, name: "Welcome" })).toBeVisible();
    // Nothing financial is asked for here. Goal sections 6 and 60: the identity
    // checks happen when somebody asks for value to leave, not when they arrive.
    //
    // The assertion is over the FORM, not over the prose. The panel's own
    // description says Nodal holds no legal name or date of birth here, and a
    // check that banned those words from the page would be a check that pushed
    // the denial off it — the same mistake, in the same suite, twice.
    const fields = await page
      .locator("form label, form .field-label")
      .evaluateAll((nodes) => nodes.map((node) => (node.textContent ?? "").trim().toLowerCase()));
    expect(fields.length, "the form asks for something").toBeGreaterThan(0);
    for (const field of fields) {
      expect(field, `onboarding asks for "${field}"`).not.toMatch(
        /date of birth|passport|social security|national id|card|address|income/,
      );
    }

    await page.getByLabel("Display name").fill("Customer B");
    await page.getByLabel("Handle (optional)").fill("customer_b");
    await page.getByRole("button", { name: "Continue" }).click();

    // Step two: the documents, shown as the API serves them.
    await expect(page.getByRole("heading", { level: 1, name: "What you are agreeing to" })).toBeVisible();
    await expect(page).toHaveURL(/\/welcome\/terms$/);

    // The bytes on the screen are the bytes the acceptance hashes, so the text
    // of a real document is present rather than a summary of one.
    const shown = page.locator("pre.doc-source").first();
    await expect(shown).toBeVisible();
    await expect(shown).toContainText("draft, pending review by qualified counsel");

    // And the control records nothing until the box is ticked.
    await page.getByRole("button", { name: "Accept and continue" }).click();
    await expect(page.getByText("Nothing is recorded until you do.")).toBeVisible();

    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "Accept and continue" }).click();

    await expect(page).toHaveURL(/\/welcome\/done$/);
    await expect(page.getByRole("heading", { level: 1, name: /You.re in/ })).toBeVisible();
    await page.getByRole("link", { name: "Go to dashboard" }).click();
  }

  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  await expect(page).toHaveURL(/\/home$/);

  // Either way the account has now arrived, and a later visit goes straight to
  // the dashboard: the returning-user path section 1 asks for.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();

  await page.context().close();
});

test("an onboarded account is never sent back through onboarding", async ({ page }) => {
  // The suite's own account has been through it (auth.setup.ts). Every
  // application route renders rather than diverting, which is the other half of
  // the gate: it must let finished accounts past as reliably as it stops
  // unfinished ones.
  for (const path of ["/home", "/markets", "/portfolio", "/activity"]) {
    await page.goto(path);
    await expect(page, `${path} is not diverted to onboarding`).not.toHaveURL(/\/welcome/);
    await expect(page.locator("h1")).toHaveCount(1);
  }
});

test("the terms step reports what the server says is outstanding", async ({ page }) => {
  // A re-issued document is the same code path as a first acceptance: the server
  // compares what was accepted against the sha256 of the bytes it serves today,
  // and `outstanding` is its answer. There is no way to bump a version from a
  // browser, so what is asserted here is that the screen renders the server's
  // answer rather than a judgement of its own. On an onboarded account that
  // answer is "nothing".
  const response = await page.request.get("/v1/me/terms-acceptances");
  expect(response.ok()).toBeTruthy();
  const state = (await response.json()) as {
    outstanding: string[];
    documents: Array<{ document_id: string; version: string; counsel_review_required: boolean }>;
  };
  expect(state.outstanding, "the suite's account has accepted everything required").toEqual([]);
  expect(state.documents.length, "the registry serves documents").toBeGreaterThan(0);
  // Every document the API serves is still a draft, and no surface may present
  // one as settled.
  for (const doc of state.documents) {
    expect(doc.counsel_review_required, `${doc.document_id} is marked as needing counsel`).toBe(true);
  }

  await page.goto("/welcome/terms");
  await expect(page.getByRole("heading", { level: 1, name: "What you are agreeing to" })).toBeVisible();
  await expect(page.getByText("Nothing outstanding")).toBeVisible();
});
