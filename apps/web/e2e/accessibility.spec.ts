/**
 * PART 113, checked rather than asserted.
 *
 * axe-core is run against every route at the WCAG 2.1 A and AA rule sets. The
 * automated pass catches the mechanical failures — contrast, missing names,
 * broken landmark structure, unlabelled controls — and the hand-written checks
 * below cover the two things it cannot see: that keyboard focus is always
 * visible, and that the reduced-motion preference is honoured.
 */
import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";

const ROUTES = [
  "/",
  "/add-funds",
  "/nodal-economy",
  "/marketplace",
  "/native-markets",
  "/create-asset",
  "/payouts",
  "/trade",
  "/portfolio",
  "/strategy",
  "/agents",
  "/lab",
  "/activity",
  "/settings",
] as const;

for (const route of ROUTES) {
  test(`${route} has no automatically detectable WCAG A/AA violation`, async ({ page }) => {
    await page.goto(route);
    await expect(page.locator("h1")).toHaveCount(1);

    const results = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .analyze();

    const summary = results.violations.map(
      (violation) =>
        `${violation.id} (${violation.impact ?? "unknown"}): ${violation.help} — ${violation.nodes
          .map((node) => node.target.join(" "))
          .join(", ")}`,
    );
    expect(summary, `${route} accessibility violations`).toEqual([]);
  });
}

test("focus is always visible as it moves through a page", async ({ page }) => {
  await page.goto("/trade");
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

test("the interface renders under a reduced-motion preference", async ({ browser }) => {
  const context = await browser.newContext({
    reducedMotion: "reduce",
    storageState: ".playwright/state.json",
  });
  const page = await context.newPage();
  await page.goto("/");
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
