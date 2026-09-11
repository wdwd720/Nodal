/**
 * No dead controls anywhere, on any page, signed in or out.
 *
 * "No dead buttons" is about every control a customer can reach, not only the
 * `<button>` elements: a navigation action here is usually a link, and a page
 * that is purely a read-out legitimately has no `<button>` at all. So this
 * enumerates everything interactive on every route and requires each one to
 * have an accessible name and to either do something real or be disabled with a
 * stated reason rendered beside it.
 *
 * The source guard makes the same rule structural for `<button>` — every one
 * goes through `Button`, whose type demands an action, a submit, or a reason
 * for being off. This is the half of the rule a type cannot express.
 */
import { expect, test, type Locator, type Page } from "@playwright/test";

import { APP_ROUTES, PUBLIC_ROUTES } from "./routes.ts";

const SIGNED_OUT = { cookies: [], origins: [] };

async function accessibleName(control: Locator): Promise<string> {
  return (
    (await control.getAttribute("aria-label")) ??
    (await control.getAttribute("title")) ??
    ((await control.textContent()) ?? "")
  ).trim();
}

async function walk(page: Page, path: string): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  // Settle first. An assertion about the absence of something passes the
  // instant it is evaluated, so a walk that runs before the queries resolve is
  // only measuring how fast the test runs.
  await page.waitForLoadState("networkidle");

  const controls = page.locator("button, a[href], input, select, textarea");
  const count = await controls.count();
  expect(count, `${path} has controls`).toBeGreaterThan(0);

  for (let i = 0; i < count; i = i + 1) {
    const control = controls.nth(i);
    const tag = await control.evaluate((node) => node.tagName.toLowerCase());
    const name = await accessibleName(control);

    // Every control has an accessible name: a label, visible text, or an
    // explicit aria-label. An unnamed control cannot be operated by anyone
    // navigating by voice or by screen reader.
    const labelled =
      name !== "" ||
      (await control.evaluate((node) => {
        const id = node.getAttribute("id");
        if (id === null) return false;
        return document.querySelector(`label[for="${id.replace(/"/g, '\\"')}"]`) !== null;
      })) ||
      (await control.evaluate((node) => node.closest("label") !== null));
    expect(labelled, `${path}: ${tag} at index ${String(i)} has an accessible name`).toBe(true);

    // A link is dead if it goes nowhere. `#main` is the skip link and is a
    // real destination; a bare `#` is a placeholder.
    if (tag === "a") {
      const href = await control.getAttribute("href");
      expect(href, `${path}: link "${name}" points somewhere`).toBeTruthy();
      expect(
        (href as string) === "#" || (href as string).trim() === "",
        `${path}: link "${name}" is not a placeholder href`,
      ).toBe(false);
      continue;
    }

    if (await control.isDisabled()) {
      // A disabled control must say why, in text, next to itself.
      const describedBy = await control.getAttribute("aria-describedby");
      expect(describedBy, `${path}: disabled "${name}" explains itself`).toBeTruthy();
      // An attribute selector rather than `#id`: these ids come from React's
      // useId() and contain characters an unescaped id selector would choke
      // on, and CSS.escape is a browser global that does not exist here.
      const reason = page.locator(`[id="${(describedBy as string).replace(/"/g, '\\"')}"]`);
      await expect(reason, `${path}: the reason for "${name}" is visible`).toBeVisible();
      expect(((await reason.textContent()) ?? "").trim().length).toBeGreaterThan(10);
    }
  }
}

test("no dead controls on the public site", async ({ browser }) => {
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  const page = await context.newPage();
  for (const route of PUBLIC_ROUTES) {
    await page.goto(route.path);
    await walk(page, route.path);
  }
  await context.close();
});

test("no dead controls in the application", async ({ page }) => {
  for (const route of APP_ROUTES) {
    await page.goto(route.path);
    await walk(page, route.path);
  }
});

test("the navigation reaches every section it names", async ({ page }) => {
  // The rail only ever offers routes that exist: entries in the shell carry a
  // `present` flag and the absent ones are filtered out, so a link to a page
  // another branch has not landed yet cannot appear.
  await page.goto("/home");
  for (const route of APP_ROUTES) {
    if (route.nav === undefined) continue;
    await page
      .getByRole("navigation", { name: "Sections" })
      .getByRole("link", { name: route.nav, exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
  }
});

test("the account menu reaches settings and offers a real sign-out", async ({ page }) => {
  await page.goto("/home");
  await page.getByRole("button", { name: "Open the account menu" }).click();
  const dialog = page.getByRole("dialog", { name: "Account" });
  await expect(dialog).toBeVisible();
  // Sign out is a real action, so it is a live button rather than a disabled
  // one with a reason. It is not pressed here: the suite shares one session.
  await expect(dialog.getByRole("button", { name: "Sign out" })).toBeEnabled();
  await dialog.getByRole("link", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Settings and security" })).toBeVisible();
});

test("the public navigation reaches every section it names", async ({ browser }) => {
  const context = await browser.newContext({
    storageState: SIGNED_OUT,
    viewport: { width: 1280, height: 900 },
  });
  const page = await context.newPage();
  await page.goto("/");
  for (const route of PUBLIC_ROUTES) {
    if (route.nav === undefined) continue;
    await page
      .getByRole("navigation", { name: "Sections" })
      .getByRole("link", { name: route.nav, exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
  }
  await context.close();
});
