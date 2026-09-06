/**
 * The guards that keep two Stage 14 rules from decaying: money never touches a
 * float, and the interface never shows a figure that is not a real one.
 *
 * These are source-level rules because they have to hold for code nobody has
 * written yet. A reviewer can miss a `Number(balance)`; this test cannot.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { matches, sourceFiles, uiFiles } from "./scan.ts";

test("no floating-point conversion touches any value in the app", () => {
  const files = sourceFiles();
  const banned: ReadonlyArray<readonly [RegExp, string]> = [
    [/\bparseFloat\s*\(/, "parseFloat turns a decimal string into a double"],
    [/\bparseInt\s*\(/, "parseInt is a numeric parse; slice the string instead"],
    [/\.toFixed\s*\(/, "toFixed rounds in binary floating point"],
    [/\bNumber\s*\(/, "Number() converts a wire value to a double"],
    [/\bMath\s*\./, "Math.* is float arithmetic"],
    [/\bnew\s+Intl\.NumberFormat\b/, "Intl.NumberFormat takes a number, so the value is a double by then"],
    [/\btoLocaleString\s*\(/, "toLocaleString on a number has already lost the exact value"],
    [/\+\s*Number\b/, "unary numeric coercion"],
    [/\bvalueAsNumber\b/, "input.valueAsNumber is a double"],
  ];
  const failures: string[] = [];
  for (const [pattern, why] of banned) {
    for (const hit of matches(files, pattern)) {
      failures.push(`${hit}\n    -> ${why}`);
    }
  }
  assert.deepEqual(
    failures,
    [],
    `floating-point arithmetic must never reach a monetary value:\n${failures.join("\n")}`,
  );
});

test("money-shaped literals never appear in interface code", () => {
  // A hardcoded "1,234.56" or "$0.00" in a page is a fake balance waiting to be
  // mistaken for a real one. Every figure must come from a response.
  const hits = matches(uiFiles(), /["'`]\$?-?[0-9][0-9,]*\.[0-9]{2}["'`]/);
  assert.deepEqual(
    hits,
    [],
    `hardcoded money figures found; render only values the API returned:\n${hits.join("\n")}`,
  );
});

test("interface code contains no decimal numeric literals", () => {
  // Closes the other half of the same hole: a bare 1234.56 passed to a
  // formatter would render exactly like a real balance.
  const hits = matches(uiFiles(), /(^|[^\w.])[0-9]+\.[0-9]+([^\w.]|$)/);
  assert.deepEqual(
    hits,
    [],
    `decimal literals found in interface code:\n${hits.join("\n")}`,
  );
});

test("every button goes through the Button component", () => {
  // The Button component's type makes a control that does nothing impossible to
  // express: it demands an action, a submit, or a stated reason for being off.
  // Raw <button> elements would route around that, so they are refused here.
  const offenders = uiFiles().filter(
    (file) => file.path !== "src/components/Button.tsx" && /<button[\s>]/.test(file.text),
  );
  assert.deepEqual(
    offenders.map((f) => f.path),
    [],
    "raw <button> elements bypass the no-dead-buttons contract; use <Button> or <LinkButton>",
  );
});

test("no animation library or celebration effect is present", () => {
  // PART 112: a trade is not a slot machine pull.
  const hits = matches(sourceFiles(), /\bconfetti\b|\bcanvas-confetti\b|\bfireworks\b/i);
  assert.deepEqual(hits, [], `celebration effects are forbidden:\n${hits.join("\n")}`);
});

test("no second HTTP client is introduced", () => {
  // The typed client in packages/generated-client is the only way to the API,
  // so contract drift shows up as a type error rather than at runtime.
  const files = sourceFiles().filter((f) => f.path !== "src/api/client.ts" && !f.path.startsWith("e2e/"));
  const hits = [
    ...matches(files, /\bfetch\s*\(/),
    ...matches(files, /\bXMLHttpRequest\b/),
    ...matches(files, /\baxios\b/),
  ];
  assert.deepEqual(
    hits,
    [],
    `all API access goes through the generated client:\n${hits.join("\n")}`,
  );
});

test("reduced motion is honoured in the stylesheet", () => {
  const css = sourceFiles().find((f) => f.path === "src/styles.css");
  assert.ok(css, "src/styles.css must exist");
  assert.match(css.text, /@media \(prefers-reduced-motion: reduce\)/);
});
