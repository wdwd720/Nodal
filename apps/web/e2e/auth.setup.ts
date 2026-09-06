/**
 * Signs in through the real OIDC flow and saves the resulting session.
 *
 * There is no shortcut here: the test drives `GET /v1/auth/login`, follows the
 * redirect to the development identity provider, chooses an identity, and lets
 * the callback set the session cookie. Everything the suite does afterwards is
 * done as a genuinely authenticated customer.
 */
import { expect, test as setup } from "@playwright/test";

export const STATE_PATH = ".playwright/state.json";

setup("sign in as customer-a", async ({ page }) => {
  await page.goto("/v1/auth/login");

  // The development identity provider renders a picker. Choosing an identity is
  // a real navigation that ends at the OIDC callback.
  await expect(page.getByRole("heading", { name: "Choose an identity" })).toBeVisible();
  const row = page.locator("li", { has: page.locator("code", { hasText: /^customer-a$/ }) });
  await row.getByRole("link", { name: "sign in", exact: true }).first().click();

  // The callback redirects to the application root.
  await expect(page.getByRole("heading", { name: "Home", level: 1 })).toBeVisible();

  const cookies = await page.context().cookies();
  expect(cookies.some((cookie) => cookie.name === "cp_session")).toBe(true);

  await page.context().storageState({ path: STATE_PATH });
});
