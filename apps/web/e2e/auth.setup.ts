/**
 * Signs in through the real OIDC flow and saves the resulting session.
 *
 * There is no shortcut here: the test drives `GET /v1/auth/login`, follows the
 * redirect to the development identity provider, chooses an identity, and lets
 * the callback set the session cookie. Everything the suite does afterwards is
 * done as a genuinely authenticated customer.
 *
 * # Why it now finishes onboarding
 *
 * On a fresh database this identity has no profile and has accepted nothing, so
 * the router sends it to `/welcome` — correctly, because a session is not the
 * same as a usable account. Every other spec in the suite is about the
 * application, and an application spec that had to cope with landing on the
 * onboarding screens would be testing onboarding by accident. So this setup
 * walks those screens once, exactly as a person would, and hands the rest of
 * the suite an account that has arrived.
 *
 * It is conditional on purpose: against a database where this identity has
 * already onboarded, nothing is clicked and the assertion at the end is the
 * same. `scenarios/a-new-user.spec.ts` is where the journey itself is the
 * subject, and it uses an identity this file never touches.
 */
import { expect, test as setup } from "@playwright/test";

import { chooseIdentity, completeOnboarding } from "./onboarding.ts";

export const STATE_PATH = ".playwright/state.json";

setup("sign in as customer-a", async ({ page }) => {
  await page.goto("/v1/auth/login");
  await chooseIdentity(page, { identity: "customer-a" });
  await completeOnboarding(page, "Customer A");

  await expect(page.getByRole("heading", { name: "Home", level: 1 })).toBeVisible();

  const cookies = await page.context().cookies();
  expect(cookies.some((cookie) => cookie.name === "cp_session")).toBe(true);

  await page.context().storageState({ path: STATE_PATH });
});
