/**
 * Driving the onboarding screens, shared by the auth setup and by scenario A.
 *
 * It lives here rather than in either file because both need it for different
 * reasons: the setup walks it once so the rest of the suite starts from an
 * account that has arrived, and the scenario walks it as the subject under
 * test. A copy in each would drift, and the copy that drifted would be the one
 * every other spec depends on.
 *
 * # Why every step waits for a URL
 *
 * Choosing an identity in the development provider starts a chain of real
 * redirects — provider, callback, application — and each one is a navigation
 * the browser performs on its own. Reading `page.url()` straight after the
 * click reads the middle of that chain, which is how the first version of this
 * decided an account had already onboarded and then failed ten seconds later
 * looking for a dashboard that was never coming. Waiting for a URL the
 * application actually serves is the difference between a check and a race.
 */
import { expect, type Page } from "@playwright/test";

/** Where the router can legitimately put somebody after a sign-in. */
const AFTER_SIGN_IN = /\/(welcome|home)(\/|$)/;

/**
 * Chooses an identity in the development provider and waits for the
 * application to answer.
 *
 * `mfa` takes the picker's second link, which asserts a strong `amr`. A
 * step-up needs it: the backend refuses the callback of a `step_up=true` flow
 * with `STEP_UP_REQUIRED` unless the provider actually asserted strong
 * authentication, which is checked in `internal/identity/login.go` rather than
 * taken on trust from the request that asked for one.
 */
export async function chooseIdentity(
  page: Page,
  options?: { readonly identity?: string; readonly mfa?: boolean; readonly waitFor?: RegExp },
): Promise<void> {
  await expect(page.getByRole("heading", { name: "Choose an identity" })).toBeVisible();
  const identity = options?.identity ?? "customer-a";
  const row = page.locator("li", { has: page.locator("code", { hasText: new RegExp(`^${identity}$`) }) });
  const label = options?.mfa === true ? "sign in with MFA" : "sign in";
  await row.getByRole("link", { name: label, exact: true }).first().click();
  await page.waitForURL(options?.waitFor ?? AFTER_SIGN_IN);
}

/**
 * Completes whichever onboarding steps this account still owes, and leaves the
 * browser on the dashboard.
 *
 * Each step is conditional because the steps are independent timestamps, not a
 * sequence (D-053): an account can owe the profile, the terms, both or
 * neither, and the router sends it to whichever comes first.
 */
export async function completeOnboarding(page: Page, displayName: string): Promise<void> {
  // A loop over the pathname rather than a sequence of `if`s over a regex.
  // The steps are independent timestamps, not a sequence (D-053): an account
  // can owe the profile, the terms, both or neither, and the router sends it to
  // whichever comes first. Reading `pathname` also removes every question about
  // trailing slashes and query strings that a regex over the whole URL invites,
  // and the loop ends on the dashboard or says which screen it could not get
  // past — which is a far better failure than a ten-second wait for a heading
  // that was never coming.
  for (let step = 0; step < 5; step = step + 1) {
    // Settle first. The callback lands on `/`, the router forwards to `/home`,
    // and only then does the onboarding gate — which needs `/me` and the terms
    // state to have answered — send an unfinished account to `/welcome`. A
    // pathname read before those two requests resolve sees the `/home` in the
    // middle of that chain and concludes the account had nothing left to do,
    // which is exactly the race that made this helper give up on its first
    // outing.
    await page.waitForLoadState("networkidle");
    const path = new URL(page.url()).pathname;

    if (path === "/home") return;

    if (path === "/welcome") {
      await expect(page.getByRole("heading", { level: 1, name: "Welcome" })).toBeVisible();
      await page.getByLabel("Display name").fill(displayName);
      await page.getByRole("button", { name: "Continue" }).click();
      await page.waitForURL(/\/welcome\/(terms|done)$/);
      continue;
    }

    if (path === "/welcome/terms") {
      await expect(
        page.getByRole("heading", { level: 1, name: "What you are agreeing to" }),
      ).toBeVisible();
      await page.getByRole("checkbox").check();
      await page.getByRole("button", { name: "Accept and continue" }).click();
      await page.waitForURL(/\/welcome\/done$/);
      continue;
    }

    if (path === "/welcome/done") {
      await page.getByRole("link", { name: "Go to dashboard" }).click();
      await page.waitForURL(/\/home$/);
      continue;
    }

    throw new Error(`onboarding: unexpected page ${path}`);
  }
  expect(new URL(page.url()).pathname, "onboarding finished on the dashboard").toBe("/home");
}
