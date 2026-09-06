import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

import {
  formatQuantity,
  formatUSD,
  groupDigits,
  isQuantity,
  isUSD,
  isZeroDecimal,
  renderEvidenceValue,
  THIN_SPACE,
} from "./money.ts";

test("USD wire form is recognised exactly", () => {
  for (const good of ["0.00", "1234.56", "-0.01", "-1000000.00"]) {
    assert.equal(isUSD(good), true, good);
  }
  for (const bad of ["1234.5", "1234", "1234.567", "1,234.56", "$1.00", "1e3", ""]) {
    assert.equal(isUSD(bad), false, bad);
  }
});

test("quantities are exact integer base units", () => {
  assert.equal(isQuantity("1500000000"), true);
  assert.equal(isQuantity("-1"), true);
  assert.equal(isQuantity("1.5"), false);
  assert.equal(isQuantity("1e9"), false);
});

test("formatting never loses a digit, at any magnitude", () => {
  // Larger than Number.MAX_SAFE_INTEGER: the exact digits must survive.
  const huge = "9007199254740993123456789";
  const out = formatQuantity(huge, 9);
  assert.equal(out.exact, true);
  assert.equal(out.text.split(THIN_SPACE).join("").replace(".", ""), huge);
  assert.equal(out.text.endsWith(".123456789"), true);
});

test("quantities smaller than one unit keep their leading zeros", () => {
  assert.deepEqual(formatQuantity("1", 9), { text: "0.000000001", exact: true });
  assert.deepEqual(formatQuantity("-1", 6), { text: "-0.000001", exact: true });
  assert.deepEqual(formatQuantity("0", 2), { text: "0.00", exact: true });
  assert.deepEqual(formatQuantity("100", 0), { text: "100", exact: true });
});

test("a value that is not the promised wire form is shown verbatim and flagged", () => {
  assert.deepEqual(formatUSD("1234.5"), { text: "1234.5", exact: false });
  assert.deepEqual(formatQuantity("1.5", 9), { text: "1.5", exact: false });
  assert.deepEqual(formatUSD(null), { text: "—", exact: true });
  assert.deepEqual(formatUSD(undefined), { text: "—", exact: true });
});

test("USD renders grouped and prefixed", () => {
  assert.deepEqual(formatUSD("1234567.89"), { text: `$1${THIN_SPACE}234${THIN_SPACE}567.89`, exact: true });
  assert.deepEqual(formatUSD("-0.01"), { text: "$-0.01", exact: true });
  assert.equal(groupDigits("1000"), `1${THIN_SPACE}000`);
  assert.equal(groupDigits("100"), "100");
  assert.equal(groupDigits("-1234567"), `-1${THIN_SPACE}234${THIN_SPACE}567`);
  assert.equal(THIN_SPACE.charCodeAt(0), 0x2009, "the separator is a thin space, not an ordinary one");
});

test("zero is recognised without parsing a number", () => {
  assert.equal(isZeroDecimal("0.00"), true);
  assert.equal(isZeroDecimal("-0.00"), true);
  assert.equal(isZeroDecimal("0"), true);
  assert.equal(isZeroDecimal("0.01"), false);
  assert.equal(isZeroDecimal("not a number"), false);
});

test("evidence values report whether they are exact", () => {
  assert.deepEqual(renderEvidenceValue("100000000"), { text: "100000000", exact: true });
  assert.deepEqual(renderEvidenceValue(null), { text: "null", exact: true });
  assert.deepEqual(renderEvidenceValue(true), { text: "true", exact: true });
  // A JSON number has already been through the double: say so.
  assert.deepEqual(renderEvidenceValue(99.99), { text: "99.99", exact: false });
  assert.equal(renderEvidenceValue({ quantity: "1", source: "ledger" }).exact, true);
  assert.equal(renderEvidenceValue({ quantity: 1 }).exact, false);
});

test("no source file parses money with Number, parseFloat or parseInt", () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const offenders: string[] = [];
  const walk = (dir: string): void => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walk(full);
        continue;
      }
      if (!entry.name.endsWith(".ts") || entry.name.endsWith(".test.ts")) continue;
      const source = fs.readFileSync(full, "utf8");
      for (const [i, line] of source.split(/\r?\n/).entries()) {
        const code = line.replace(/\/\/.*$/, "").replace(/^\s*\*.*$/, "");
        if (/\b(parseFloat|parseInt)\s*\(/.test(code) || /\bNumber\s*\(/.test(code)) {
          offenders.push(`${path.relative(here, full)}:${i + 1}: ${line.trim()}`);
        }
      }
    }
  };
  walk(here);
  assert.deepEqual(
    offenders,
    [],
    `financial values must never be parsed into a JavaScript number:\n${offenders.join("\n")}`,
  );
});
