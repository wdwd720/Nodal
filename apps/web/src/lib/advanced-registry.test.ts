/**
 * The advanced area's section registry is shared by twelve subsystem lanes,
 * each of which inserts one import and one entry below its own markers. These
 * checks keep that arrangement from decaying: every marker present exactly
 * once, in order, in the right place, and every registered path unique and
 * URL-safe. The registry is read as text because it is TSX, which the test
 * runner does not load; the markers are text anyway.
 */
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

import { SRC_ROOT } from "./scan.ts";

const SUBSYSTEMS = [
  "margin",
  "derivatives",
  "equities",
  "orders",
  "transfers",
  "treasury",
  "stable",
  "bridge",
  "predmarket",
  "marketplace",
  "delegation",
  "operator",
] as const;

const lines = readFileSync(join(SRC_ROOT, "pages", "advanced", "registry.tsx"), "utf8").split("\n");

function lineOf(predicate: (line: string) => boolean, what: string): number {
  const index = lines.findIndex(predicate);
  assert.ok(index >= 0, `registry.tsx has ${what}`);
  return index;
}

function markerLines(place: "import" | "section"): number[] {
  return SUBSYSTEMS.map((sub) => {
    const marker = `// >>> advanced:${sub} ${place}`;
    const hits = lines.flatMap((line, index) => (line.trim() === marker ? [index] : []));
    assert.equal(hits.length, 1, `"${marker}" appears exactly once, on a line of its own`);
    return hits[0] as number;
  });
}

test("every subsystem marker is present once, in order, in both places", () => {
  for (const place of ["import", "section"] as const) {
    const positions = markerLines(place);
    const ordered = [...positions].sort((a, b) => a - b);
    assert.deepEqual(positions, ordered, `the ${place} markers keep the subsystem order`);
  }
});

test("import markers sit with the imports and section markers inside the sections array", () => {
  const typeDeclaration = lineOf((line) => line.startsWith("export interface AdvancedSection"), "the entry type");
  const arrayStart = lineOf((line) => line.startsWith("export const ADVANCED_SECTIONS"), "the sections array");
  const arrayEnd = lines.findIndex((line, index) => index > arrayStart && line.startsWith("];"));
  assert.ok(arrayEnd > arrayStart, "the sections array is closed");

  for (const position of markerLines("import")) {
    assert.ok(position < typeDeclaration, "an import marker comes before any declaration");
  }
  for (const position of markerLines("section")) {
    assert.ok(position > arrayStart && position < arrayEnd, "a section marker is inside ADVANCED_SECTIONS");
  }
});

test("every registered path is unique, URL-safe, and the default section is one of them", () => {
  const code = lines.filter((line) => !/^\s*(\/\/|\/\*|\*)/.test(line)).join("\n");
  const paths = [...code.matchAll(/\bpath:\s*"([^"]*)"/g)].map((match) => match[1] ?? "");
  assert.ok(paths.length >= 4, "the core sections are registered");
  assert.equal(new Set(paths).size, paths.length, `paths are unique: ${paths.join(", ")}`);
  for (const path of paths) {
    assert.match(path, /^[a-z][a-z0-9-]*$/, `"${path}" is one lowercase URL segment`);
  }
  const fallback = /ADVANCED_DEFAULT_SECTION = "([^"]+)"/.exec(code)?.[1];
  assert.ok(fallback !== undefined && paths.includes(fallback), "the default section is registered");
});
