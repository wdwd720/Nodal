/**
 * The guards that keep two Stage 14 rules from decaying: money never touches a
 * float, and the interface never shows a figure that is not a real one.
 *
 * These are source-level rules because they have to hold for code nobody has
 * written yet. A reviewer can miss a `Number(balance)`; this test cannot.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { matches, sourceFiles, uiFiles, type SourceFile } from "./scan.ts";

/* --------------------------------------------------------------------------
 * THE CHART ALLOWLIST
 *
 * An SVG coordinate is a number. There is no way to place a wick at a price
 * without producing one, so `src/charts/` — and nothing else — may convert an
 * exact value to a double, and may do it only after the ratio has already been
 * computed in BigInt, on a quantity whose magnitude is bounded by the size of
 * the plot. `src/charts/geometry.ts` carries the argument in full.
 *
 * The allowlist is NARROW on purpose. It admits exactly two forms:
 *
 *   - the numeric constructor, which is how a BigInt number of hundredths of a
 *     pixel becomes a coordinate;
 *   - the maths namespace, for clamping and comparing values that are already
 *     pixels.
 *
 * Everything else stays refused inside `src/charts/` exactly as it is outside:
 * the two string-to-number parsers, fixed-point rounding, locale number
 * formatting and the input element's numeric reader are all still failures
 * here, because not one of them takes a pixel — they take a WIRE value.
 *
 * Three compensating rules, each its own test below, make the relaxation safe:
 * the directory cannot import the formatters, cannot draw a composed string as
 * a mark, and cannot name a unit of value. A chart that can do none of those
 * three cannot put a figure on the screen that nobody computed.
 * ------------------------------------------------------------------------ */
const CHART_ROOT = "src/charts/";

/** Refused in `src/charts/` too, because not one of them is pixel geometry. */
const BANNED_EVERYWHERE: ReadonlyArray<readonly [RegExp, string]> = [
  [/\bparseFloat\s*\(/, "parseFloat turns a decimal string into a double"],
  [/\bparseInt\s*\(/, "parseInt is a numeric parse; slice the string instead"],
  [/\.toFixed\s*\(/, "toFixed rounds in binary floating point"],
  [/\bnew\s+Intl\.NumberFormat\b/, "Intl.NumberFormat takes a number, so the value is a double by then"],
  [/\btoLocaleString\s*\(/, "toLocaleString on a number has already lost the exact value"],
  [/\+\s*Number\b/, "unary numeric coercion"],
  [/\bvalueAsNumber\b/, "input.valueAsNumber is a double"],
];

/** Refused everywhere EXCEPT `src/charts/`, where a pixel needs them. */
const BANNED_OUTSIDE_CHARTS: ReadonlyArray<readonly [RegExp, string]> = [
  [/\bNumber\s*\(/, "Number() converts a wire value to a double"],
  [/\bMath\s*\./, "Math.* is float arithmetic"],
];

/**
 * The file with every comment blanked out, line numbers preserved.
 *
 * The three chart rules below are rules about what the code DOES, and a doc
 * comment that quotes the thing it forbids is documentation rather than a
 * violation — this file is itself excluded from the scan for exactly that
 * reason. Blanking rather than deleting keeps each reported line number
 * pointing at the line it came from.
 */
function codeOnly(file: SourceFile): SourceFile {
  const blanked = file.text
    .replace(/\/\*[\s\S]*?\*\//g, (comment) => comment.replace(/[^\n]/g, " "))
    .replace(/(^|[^:"'`\\])\/\/[^\n]*/g, (match, lead: string) =>
      `${lead}${match.slice(lead.length).replace(/[^\n]/g, " ")}`,
    );
  return { path: file.path, text: blanked };
}

function chartCode(): SourceFile[] {
  return sourceFiles()
    .filter((file) => file.path.startsWith(CHART_ROOT))
    .map(codeOnly);
}

test("no floating-point conversion touches any value in the app", () => {
  const files = sourceFiles();
  const outsideCharts = files.filter((file) => !file.path.startsWith(CHART_ROOT));
  const failures: string[] = [];
  for (const [pattern, why] of BANNED_EVERYWHERE) {
    for (const hit of matches(files, pattern)) {
      failures.push(`${hit}\n    -> ${why}`);
    }
  }
  for (const [pattern, why] of BANNED_OUTSIDE_CHARTS) {
    for (const hit of matches(outsideCharts, pattern)) {
      failures.push(`${hit}\n    -> ${why} (allowed only under ${CHART_ROOT})`);
    }
  }
  assert.deepEqual(
    failures,
    [],
    `floating-point arithmetic must never reach a monetary value:\n${failures.join("\n")}`,
  );
});

/**
 * The allowlist is a hole in the most important rule this application has, so
 * it is kept small enough to read in one sitting. If `src/charts/` grows past a
 * handful of files, that is a design change, and it should have to argue with
 * this test rather than arrive in a commit nobody read.
 */
test("the chart allowlist covers a directory small enough to audit", () => {
  const charts = sourceFiles().filter((file) => file.path.startsWith(CHART_ROOT));
  assert.ok(charts.length > 0, `${CHART_ROOT} must exist for its allowlist to mean anything`);
  assert.ok(
    charts.length <= 6,
    `${CHART_ROOT} holds ${String(charts.length)} files and the float allowlist covers all of them`,
  );
});

test("nothing in the chart directory can format a figure", () => {
  // Rule one of three. Axis labels come from the API's exact strings, formatted
  // by `format.ts` OUTSIDE this directory and handed in as opaque strings. A
  // chart that could import the formatters could compose a figure nobody
  // computed, which is the thing the float ban exists to prevent.
  const offenders: string[] = [];
  for (const file of chartCode()) {
    if (/from\s+["'][^"']*lib\/(money|format)\.ts["']/.test(file.text)) {
      offenders.push(`${file.path}: imports a formatting module`);
    }
    if (/from\s+["'][^"']*components\/(Figure|Money)\.tsx["']/.test(file.text)) {
      offenders.push(`${file.path}: imports a figure component`);
    }
  }
  assert.deepEqual(
    offenders,
    [],
    `no file under ${CHART_ROOT} may format a value:\n${offenders.join("\n")}`,
  );
});

test("a chart renders a supplied label and never a composed one", () => {
  // Rule two. Every text-bearing expression in a chart is ONE bare property
  // read — `{tick.label}`, `{props.summary}` — and nothing else: no call, no
  // template literal, no concatenation. A chart can only ever say what the
  // caller already said, which is what makes the float allowlist survivable.
  //
  // It reads `>{ ... }<` rather than a specific element because the axis labels
  // moved out of SVG and into HTML when it turned out that text inside a
  // stretched viewBox renders at five pixels on a phone. The rule is about
  // where a string comes from, not about which tag it lands in.
  const bareRead = /^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$/;
  const offenders: string[] = [];
  for (const file of chartCode()) {
    // `[^{}]*` skips anything with a nested brace — a `.map()` body, a
    // conditional with an attribute in it — because those render elements
    // rather than text, and their own text children are matched on their own.
    for (const [, expression] of file.text.matchAll(/>\s*\{([^{}]*)\}\s*</g)) {
      const read = (expression ?? "").trim();
      if (!bareRead.test(read)) {
        offenders.push(`${file.path}: renders ${JSON.stringify(read)} as text`);
      }
    }
  }
  assert.deepEqual(
    offenders,
    [],
    `a chart renders a supplied label and nothing else:\n${offenders.join("\n")}`,
  );
});

test("the chart directory names no unit of value", () => {
  // Rule three. A chart that cannot say "Credits" cannot render a money string
  // as text, whatever it does with the digits. That vocabulary belongs to the
  // page around the plot, which is the thing holding the scale and the
  // formatters.
  const charts = chartCode();
  const hits = [
    ...matches(charts, /\bCredits?\b/),
    ...matches(charts, /\bUSDC?\b/),
    ...matches(charts, /\$[0-9]/),
  ];
  assert.deepEqual(hits, [], `${CHART_ROOT} must not name a unit of value:\n${hits.join("\n")}`);
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
