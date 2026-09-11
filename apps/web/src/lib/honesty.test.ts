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

import {
  CONFIDENCE_DISCLAIMER,
  CREDITS_DISCLOSURE,
  NATIVE_ASSET_RISK,
  NATIVE_PRICE_NOTE,
  SIMULATED_RESULTS_NOTICE,
  THREE_POTS_NOTE,
  USDC_DISCLOSURE,
} from "./honesty.ts";
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
  // There is deliberately no "at least one page must show a USD figure" here
  // any more. D-077 removed the hosted rail and the last screen that rendered
  // one went with it, so the product is Credit-denominated end to end and that
  // list is legitimately empty. The rule below is the one that matters and it
  // is unchanged: it bites the moment a USD figure comes back without saying
  // what the underlying asset actually is. An emptiness assertion would have
  // made a page that shows no dollar figures into a test failure, which is the
  // opposite of what PART 112 asks for.
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

/* --------------------------------------------------------------------------
 * The Nodal-native economy (gola.md PARTS LII, LIV)
 * ------------------------------------------------------------------------ */

test("a page showing Credits says what Credits are", () => {
  // PART LII: never misrepresent balances. A Credit figure with no statement
  // of what a Credit is invites the reader to assume it is money.
  const creditPages = uiFiles().filter(
    (file) => file.path.startsWith("src/pages/") && /symbol="Credits"/.test(file.text),
  );
  assert.ok(creditPages.length > 0, "expected at least one page to render a Credit figure");
  const offenders = creditPages.filter(
    (file) =>
      !file.text.includes("CREDITS_DISCLOSURE") &&
      !file.text.includes("NATIVE_PRICE_NOTE") &&
      !file.text.includes("PROVENANCE_NOTE"),
  );
  assert.deepEqual(
    offenders.map((f) => f.path),
    [],
    "a page showing Credits must say what they are and what they can do",
  );
});

test("a page showing a user-created asset carries the risk statement", () => {
  // PART LIV. A Nodal-native asset is made by a user, priced by a formula, and
  // reviewed by nobody as an investment. A page that shows one without saying
  // so is presenting it as though Nodal stood behind it.
  const offenders = uiFiles().filter(
    (file) =>
      file.path.startsWith("src/pages/") &&
      /native_asset|NativeAsset|native-markets/.test(file.text) &&
      !file.text.includes("NATIVE_ASSET_RISK"),
  );
  assert.deepEqual(
    offenders.map((f) => f.path),
    [],
    "a page showing a user-created asset must carry NATIVE_ASSET_RISK",
  );
});

test("no page converts Credits into a currency", () => {
  // PART LIV: prefer "Price: 2 Credits" over a currency figure, unless an
  // approved externally redeemable value exists — and none does. The check is
  // structural: a page that renders both a Credit figure and a <Usd> is either
  // converting one into the other or inviting the reader to.
  const offenders = uiFiles().filter(
    (file) =>
      file.path.startsWith("src/pages/") &&
      /symbol="Credits"/.test(file.text) &&
      /<Usd\b/.test(file.text),
  );
  assert.deepEqual(
    offenders.map((f) => f.path),
    [],
    "a page must not put a Credit figure and a currency figure on the same screen; " +
      "no approved external value for a Credit exists",
  );
});

test("the internal-economy disclosures say what they must say", () => {
  assert.match(CREDITS_DISCLOSURE, /not money/i);
  assert.match(CREDITS_DISCLOSURE, /not redeemable for money unless/i);
  assert.match(THREE_POTS_NOTE, /never added together/i);
  assert.match(NATIVE_PRICE_NOTE, /quoted in Credits/i);
  assert.match(NATIVE_ASSET_RISK, /created by a user/i);
  assert.match(NATIVE_ASSET_RISK, /nothing here is advice/i);
  for (const [pattern] of FORBIDDEN) {
    for (const text of [CREDITS_DISCLOSURE, THREE_POTS_NOTE, NATIVE_PRICE_NOTE, NATIVE_ASSET_RISK]) {
      assert.equal(pattern.test(text), false, `disclosure text trips ${String(pattern)}`);
    }
  }
});
