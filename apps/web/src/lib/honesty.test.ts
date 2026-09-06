/**
 * PART 112, enforced.
 *
 * The goal document lists things this interface must never say. Saying them is
 * not a styling mistake, it is a misrepresentation of what a customer owns and
 * what a number means, so it is a test failure. This scans the source; the
 * Playwright suite in `e2e/honesty.spec.ts` scans what actually renders,
 * including strings that arrive from the API.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { CONFIDENCE_DISCLAIMER, SIMULATED_RESULTS_NOTICE, USDC_DISCLOSURE } from "./honesty.ts";
import { matches, sourceFiles, uiFiles } from "./scan.ts";

/**
 * Patterns forbidden everywhere. Written out here rather than imported so the
 * banned words live in exactly one place — the place that bans them.
 */
export const FORBIDDEN: ReadonlyArray<readonly [RegExp, string]> = [
  [/\bcash\b/i, 'a stablecoin is not "cash"; it is USDC, and the interface says so'],
  [/\bcash[ -]?(balance|available|out|in)\b/i, "same rule, compound forms"],
  [/\blive performance\b/i, "a simulated or shadow result is never presented as real trading"],
  [/\bguarantee/i, "nothing here is assured"],
  [/\brisk[ -]free\b/i, "nothing here is without risk"],
  [/\bno risk\b/i, "nothing here is without risk"],
  [/\bcan.?t lose\b/i, "nothing here is without risk"],
  [/\bsure thing\b/i, "nothing here is without risk"],
  [/\bprobability of profit\b/i, "a model score is not the chance of making money"],
  [/\bchance of profit\b/i, "a model score is not the chance of making money"],
  [/\blikelihood of profit\b/i, "a model score is not the chance of making money"],
  [/\bwin rate\b/i, "implies a probability of profit"],
  [/\bsuccess rate\b/i, "implies a probability of profit"],
  [/\bexpected profit\b/i, "implies a probability of profit"],
  [/\bhands[- ]free\b/i, "overstates what an agent does on a customer's behalf"],
];

test("the forbidden vocabulary appears nowhere in the source", () => {
  const files = sourceFiles();
  const failures: string[] = [];
  for (const [pattern, why] of FORBIDDEN) {
    for (const hit of matches(files, pattern)) {
      failures.push(`${hit}\n    -> ${why}`);
    }
  }
  assert.deepEqual(failures, [], `PART 112 violations:\n${failures.join("\n")}`);
});

test("a file that renders a model score also renders the denial", () => {
  // The disclaimer has to travel with the number. Importing the constant is the
  // cheapest way to make that structurally true.
  const offenders = uiFiles().filter(
    (file) => /confidence/i.test(file.text) && !file.text.includes("CONFIDENCE_DISCLAIMER"),
  );
  assert.deepEqual(
    offenders.map((f) => f.path),
    [],
    "a page showing model confidence must also show CONFIDENCE_DISCLAIMER",
  );
});

test("a file that renders simulated results also labels them", () => {
  const offenders = uiFiles().filter(
    (file) =>
      /\b(BACKTEST|SHADOW)\b/.test(file.text) &&
      !file.text.includes("SIMULATED_RESULTS_NOTICE") &&
      !file.text.includes("modeBadge"),
  );
  assert.deepEqual(
    offenders.map((f) => f.path),
    [],
    "a page showing backtest or shadow output must label it as not real trading",
  );
});

test("the disclosures say what they must say", () => {
  assert.match(USDC_DISCLOSURE, /USDC/);
  assert.match(USDC_DISCLOSURE, /stablecoin/i);
  assert.match(USDC_DISCLOSURE, /not a bank deposit/i);
  assert.match(SIMULATED_RESULTS_NOTICE, /no real capital/i);
  assert.match(CONFIDENCE_DISCLAIMER, /is not the chance/i);
  for (const [pattern] of FORBIDDEN) {
    for (const text of [USDC_DISCLOSURE, SIMULATED_RESULTS_NOTICE, CONFIDENCE_DISCLAIMER]) {
      assert.equal(pattern.test(text), false, `disclosure text trips ${String(pattern)}`);
    }
  }
});

test("the settlement asset is disclosed on every page that shows a balance", () => {
  const balancePages = uiFiles().filter(
    (file) => file.path.startsWith("src/pages/") && /formatUsd|<Usd\b/.test(file.text),
  );
  assert.ok(balancePages.length > 0, "expected at least one page to render a USD figure");
  const offenders = balancePages.filter(
    (file) =>
      !file.text.includes("USDC_DISCLOSURE") &&
      !file.text.includes("USD_VALUATION_NOTE") &&
      !file.text.includes("SIMULATED_RESULTS_NOTICE"),
  );
  assert.deepEqual(
    offenders.map((f) => f.path),
    [],
    "a page showing a USD figure must disclose what the underlying asset actually is",
  );
});
