/**
 * Navigating within the API's general transport budget.
 *
 * CP_API_RATE_LIMIT_GENERAL allows 600 requests a minute per principal by
 * default, counted in fixed windows, and every page of this application makes
 * several as it loads -- who is signed in, the accounts, the build, the event
 * stream -- before its own reads. A person never comes near the budget. A spec
 * that loads every route of the application, several times over, in a few
 * seconds does: the backend then answers RATE_LIMITED, which the application
 * shows honestly in place of the page, and the spec measures the budget
 * instead of the page.
 *
 * So these navigations are paced to the budget. Every answer the budget applies
 * to reports what is left of the window (RateLimit-Remaining) and when it
 * resets (RateLimit-Reset); the last report is kept per session, and when what
 * is left could not carry another page, goto() waits for the next window first
 * (extending the test's timeout by exactly that wait). The budget is never
 * raised and never bypassed.
 */
import { test, type Page, type Response } from "@playwright/test";

/** Enough for one page of this application to load and settle, with room to spare. */
const PAGE_REQUESTS = 40n;

interface Window {
  readonly remaining: bigint;
  readonly resetsAt: number;
}

/**
 * The last report of each session's window, by session cookie. The budget is
 * the principal's, and a session is one principal; tests that share the
 * suite's stored session share its budget, across files.
 */
const windows = new Map<string, Window>();
const watched = new WeakSet<Page>();

/** A header that counts whole units, exactly (the source guard admits no numeric parse). */
function headerCount(value: string | undefined): bigint | undefined {
  const text = value?.trim() ?? "";
  return /^[0-9]{1,6}$/.test(text) ? BigInt(text) : undefined;
}

function watch(page: Page, session: string): void {
  if (watched.has(page)) return;
  watched.add(page);
  page.on("response", (response) => {
    const path = new URL(response.url()).pathname;
    // The general budget is the one a GET under /v1 spends; /auth/ has its own.
    if (!path.startsWith("/v1/") || path.includes("/auth/") || response.request().method() !== "GET") return;
    const remaining = headerCount(response.headers()["ratelimit-remaining"]);
    const reset = headerCount(response.headers()["ratelimit-reset"]);
    if (remaining === undefined || reset === undefined) return;
    let resetsAt = Date.now();
    for (let second = 0n; second < reset; second += 1n) resetsAt += 1_000;
    const known = windows.get(session);
    // Answers of one window can arrive out of order: within a window keep the
    // least that was reported; a later reset is a new window.
    if (known !== undefined && resetsAt <= known.resetsAt + 1_500) {
      windows.set(session, { remaining: remaining < known.remaining ? remaining : known.remaining, resetsAt: known.resetsAt });
    } else {
      windows.set(session, { remaining, resetsAt });
    }
  });
}

/** Navigates `page` to `path`, first waiting for the next window when this one could not carry the page. */
export async function goto(page: Page, path: string): Promise<Response | null> {
  const session = (await page.context().cookies()).find((cookie) => cookie.name === "cp_session")?.value;
  if (session !== undefined) {
    watch(page, session);
    const window = windows.get(session);
    if (window !== undefined && window.remaining < PAGE_REQUESTS) {
      windows.delete(session);
      const waitMs = window.resetsAt - Date.now() + 1_000;
      if (waitMs > 0) {
        const info = test.info();
        info.setTimeout(info.timeout + waitMs);
        await page.waitForTimeout(waitMs);
      }
    }
  }
  return page.goto(path);
}
