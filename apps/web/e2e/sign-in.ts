/**
 * Signing a development identity in, the way every spec does it: in a fresh
 * context, through the real OIDC flow -- GET /v1/auth/login, the development
 * identity provider's picker, the picker's redirect and the callback that sets
 * the session cookie. There is no shortcut and no injected cookie.
 *
 * The API rate-limits its /auth/ routes per address (CP_API_RATE_LIMIT_AUTH:
 * 30 requests a minute by default, counted in fixed windows), and a sign-in is
 * four of those requests. A person signs in a few times a day; this suite signs
 * in a dozen identities in a few minutes, from one address. So a sign-in is
 * paced to the deployment's own budget: the picker's answer reports what is
 * left of the window (RateLimit-Remaining) and when it resets
 * (RateLimit-Reset), and when the window cannot hold the two requests still to
 * come -- or had already run out, so the picker itself was refused with 429 --
 * the sign-in waits for the next window and starts again. Nothing has been
 * signed in at that point, so starting again changes nothing. The budget is
 * never raised and never bypassed.
 */
import { expect, test, type Browser, type BrowserContext, type Page } from "@playwright/test";

/** The requests a sign-in still makes after the picker has answered: its redirect and the callback. */
const REQUESTS_AFTER_PICKER = 2n;

/** A header that counts whole units, exactly (the source guard admits no numeric parse). */
function headerCount(value: string | undefined): bigint | undefined {
  const text = value?.trim() ?? "";
  return /^[0-9]{1,6}$/.test(text) ? BigInt(text) : undefined;
}

/** Signs `identity` in, with a strong (MFA) sign-in when `mfa` is set, in a context of its own. */
export async function signIn(browser: Browser, identity: string, mfa: boolean): Promise<{ context: BrowserContext; page: Page }> {
  const context = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  for (;;) {
    const picker = await page.goto("/v1/auth/login");
    const headers = picker?.headers() ?? {};
    const remaining = headerCount(headers["ratelimit-remaining"]);
    if (picker?.status() !== 429 && (remaining === undefined || remaining >= REQUESTS_AFTER_PICKER)) break;
    // Wait out the window -- one second past its reset -- and give the test
    // the time it spends waiting.
    const reset = headerCount(headers["ratelimit-reset"]) ?? headerCount(headers["retry-after"]) ?? 60n;
    let waitMs = 1_000;
    for (let second = 0n; second < reset; second += 1n) waitMs += 1_000;
    const info = test.info();
    info.setTimeout(info.timeout + waitMs);
    await page.waitForTimeout(waitMs);
  }
  await expect(page.getByRole("heading", { name: "Choose an identity" })).toBeVisible();
  const row = page.locator("li", { has: page.locator("code", { hasText: new RegExp(`^${identity}$`) }) });
  await row.getByRole("link", { name: mfa ? "sign in with MFA" : "sign in", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Home", level: 1 })).toBeVisible();
  return { context, page };
}
