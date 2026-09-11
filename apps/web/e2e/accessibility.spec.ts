/**
 * Goal §32, checked rather than asserted.
 *
 * axe-core runs against every route — public and application — at the WCAG 2.1
 * A and AA rule sets, and zero violations is the merge gate. The automated pass
 * catches the mechanical failures: contrast, missing names, broken landmark
 * structure, unlabelled controls. The hand-written checks below cover the three
 * things it cannot see — that keyboard focus is always visible, that the
 * reduced-motion preference is honoured, and that no page scrolls sideways on a
 * phone.
 *
 * The public routes are visited in a **signed-out** context. Running them with
 * the suite's stored session would send `/` and `/sign-in` straight to the
 * dashboard, and the landing page — the one page every visitor sees — would
 * never be checked at all.
 */
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Browser, type Page } from "@playwright/test";

import { APP_ROUTES, NARROW_HEIGHT, NARROW_WIDTH, ONBOARDING_ROUTES, PUBLIC_ROUTES } from "./routes.ts";

const SIGNED_OUT = { cookies: [], origins: [] };

async function signedOutPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ storageState: SIGNED_OUT });
  return context.newPage();
}

async function violations(page: Page): Promise<string[]> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  return results.violations.map(
    (violation) =>
      `${violation.id} (${violation.impact ?? "unknown"}): ${violation.help} — ${violation.nodes
        .map((node) => node.target.join(" "))
        .join(", ")}`,
  );
}

for (const route of PUBLIC_ROUTES) {
  test(`public ${route.path} has no automatically detectable WCAG A/AA violation`, async ({
    browser,
  }) => {
    const page = await signedOutPage(browser);
    await page.goto(route.path);
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
    await expect(page.locator("h1")).toHaveCount(1);
    expect(await violations(page), `${route.path} accessibility violations`).toEqual([]);
    await page.context().close();
  });
}

for (const route of APP_ROUTES) {
  test(`${route.path} has no automatically detectable WCAG A/AA violation`, async ({ page }) => {
    await page.goto(route.path);
    await expect(page.locator("h1")).toHaveCount(1);
    expect(await violations(page), `${route.path} accessibility violations`).toEqual([]);
  });
}

for (const route of ONBOARDING_ROUTES) {
  test(`${route.path} has no automatically detectable WCAG A/AA violation`, async ({ page }) => {
    await page.goto(route.path);
    await expect(page.getByRole("heading", { level: 1, name: route.heading })).toBeVisible();
    await expect(page.locator("h1")).toHaveCount(1);
    expect(await violations(page), `${route.path} accessibility violations`).toEqual([]);
  });
}

test("onboarding does not scroll sideways on a phone", async ({ page }) => {
  await page.setViewportSize({ width: NARROW_WIDTH, height: NARROW_HEIGHT });
  for (const route of ONBOARDING_ROUTES) {
    await page.goto(route.path);
    await expect(page.locator("h1")).toHaveCount(1);
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, `${route.path} does not scroll horizontally at 375px`).toBeLessThanOrEqual(1);
  }
});

test("no public page scrolls sideways on a phone", async ({ browser }) => {
  // Goal §31. The one element allowed to be wider than the viewport is a table,
  // which lives inside its own scroll container, so the DOCUMENT must never
  // overflow. There is deliberately no `overflow-x: hidden` on the body: it
  // would clamp scrollWidth to clientWidth and make this test pass without the
  // layout reflowing.
  const context = await browser.newContext({
    storageState: SIGNED_OUT,
    viewport: { width: NARROW_WIDTH, height: NARROW_HEIGHT },
  });
  const page = await context.newPage();
  for (const route of PUBLIC_ROUTES) {
    await page.goto(route.path);
    await expect(page.locator("h1")).toHaveCount(1);
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, `${route.path} does not scroll horizontally at 375px`).toBeLessThanOrEqual(1);
  }
  await context.close();
});

test("no application page scrolls sideways on a phone", async ({ page }) => {
  await page.setViewportSize({ width: NARROW_WIDTH, height: NARROW_HEIGHT });
  for (const route of APP_ROUTES) {
    await page.goto(route.path);
    await expect(page.locator("h1")).toHaveCount(1);
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, `${route.path} does not scroll horizontally at 375px`).toBeLessThanOrEqual(1);
  }
});

test("the public site offers one navigation at a time", async ({ browser }) => {
  // The shell is chosen in JavaScript, not hidden with CSS: a navigation that
  // is merely invisible is still a navigation to a screen reader, and a link
  // inside it is still a tab stop.
  const wide = await browser.newContext({
    storageState: SIGNED_OUT,
    viewport: { width: 1280, height: 900 },
  });
  const widePage = await wide.newPage();
  await widePage.goto("/");
  await expect(widePage.getByRole("navigation", { name: "Sections" })).toHaveCount(1);
  await expect(widePage.getByRole("button", { name: "Open the section list" })).toHaveCount(0);
  await wide.close();

  const narrow = await browser.newContext({
    storageState: SIGNED_OUT,
    viewport: { width: NARROW_WIDTH, height: NARROW_HEIGHT },
  });
  const narrowPage = await narrow.newPage();
  await narrowPage.goto("/");
  // Closed, the sheet holds no navigation at all.
  await expect(narrowPage.getByRole("navigation", { name: "Sections" })).toHaveCount(0);
  await narrowPage.getByRole("button", { name: "Open the section list" }).click();
  await expect(narrowPage.getByRole("navigation", { name: "Sections" })).toHaveCount(1);
  await narrow.close();
});

test("focus is always visible as it moves through a page", async ({ page }) => {
  await page.goto("/markets");
  await expect(page.locator("h1")).toHaveCount(1);
  for (let i = 0; i < 25; i = i + 1) {
    await page.keyboard.press("Tab");
    const outline = await page.evaluate(() => {
      const element = document.activeElement;
      if (element === null || element === document.body) return "none";
      const style = getComputedStyle(element);
      return `${style.outlineStyle}:${style.outlineWidth}`;
    });
    if (outline === "none") continue;
    expect(outline, "the focused element draws an outline").not.toMatch(/^none:/);
  }
});

test("the skip link is the first stop on the public site too", async ({ browser }) => {
  const page = await signedOutPage(browser);
  await page.goto("/");
  await expect(page.locator("h1")).toHaveCount(1);
  await page.keyboard.press("Tab");
  const focused = page.locator(":focus");
  await expect(focused).toHaveText("Skip to main content");
  await focused.press("Enter");
  await expect(page).toHaveURL(/#main$/);
  await page.context().close();
});

test("the interface renders under a reduced-motion preference", async ({ browser }) => {
  const context = await browser.newContext({
    reducedMotion: "reduce",
    storageState: ".playwright/state.json",
  });
  const page = await context.newPage();
  await page.goto("/home");
  await expect(page.getByRole("heading", { level: 1, name: "Home" })).toBeVisible();
  const duration = await page.evaluate(() => {
    const button = document.querySelector("a.btn, button.btn");
    return button === null ? "0s" : getComputedStyle(button).transitionDuration;
  });
  // The reduced-motion reset collapses transitions to 0.01ms rather than to a
  // literal zero, which is the conventional form: it keeps `transitionend`
  // firing so nothing waiting on that event hangs, while being imperceptible.
  // These are the renderings a browser gives for "no perceivable transition";
  // they are compared as strings because parsing one would be float arithmetic,
  // which the source guard forbids anywhere in this tree.
  const SUPPRESSED = new Set(["0s", "0ms", "1e-05s", "0.01ms"]);
  expect(SUPPRESSED.has(duration.trim()), `transitions are suppressed (got ${duration})`).toBe(true);
  await context.close();
});

test("the interface renders in a dark colour scheme", async ({ browser }) => {
  const context = await browser.newContext({
    colorScheme: "dark",
    storageState: ".playwright/state.json",
  });
  const page = await context.newPage();
  await page.goto("/portfolio");
  await expect(page.getByRole("heading", { level: 1, name: "Portfolio" })).toBeVisible();
  const background = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  expect(background, "the page paints its own background in dark mode").not.toBe("rgba(0, 0, 0, 0)");
  await context.close();
});

test("the public site paints its own background in dark mode too", async ({ browser }) => {
  const context = await browser.newContext({
    colorScheme: "dark",
    storageState: SIGNED_OUT,
  });
  const page = await context.newPage();
  await page.goto("/");
  await expect(page.locator("h1")).toHaveCount(1);
  const background = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  expect(background).not.toBe("rgba(0, 0, 0, 0)");
  await context.close();
});
