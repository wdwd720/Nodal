/**
 * The table's keyboard and sort rules, asserted rather than assumed.
 *
 * These are the parts of `DataTable` a reviewer cannot check by looking: that
 * the arrow keys clamp instead of wrapping, that a sortable column can be
 * returned to the order the backend sent, and that sorting never mutates the
 * caller's rows.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { applySort, nextSort, rovingIndex, rovingTab } from "./table.ts";

test("a sortable column cycles through three states, not two", () => {
  // The third state is the one that matters: it is the only way back to the
  // order the backend returned, which is frequently the meaningful order.
  assert.equal(nextSort("none"), "ascending");
  assert.equal(nextSort("ascending"), "descending");
  assert.equal(nextSort("descending"), "none");
});

test("arrow keys move one row and clamp at both ends", () => {
  assert.equal(rovingIndex("ArrowDown", 0, 5), 1);
  assert.equal(rovingIndex("ArrowUp", 3, 5), 2);
  // Clamping, not wrapping. Wrapping from the last row to the first loses the
  // reader's place in a way they cannot detect.
  assert.equal(rovingIndex("ArrowDown", 4, 5), 4);
  assert.equal(rovingIndex("ArrowUp", 0, 5), 0);
  assert.equal(rovingIndex("Home", 3, 5), 0);
  assert.equal(rovingIndex("End", 1, 5), 4);
  assert.equal(rovingIndex("PageDown", 0, 5), 4);
  assert.equal(rovingIndex("PageUp", 4, 50), 0);
  assert.equal(rovingIndex("PageDown", 0, 50), 10);
});

test("a key the table does not own is left to the browser", () => {
  assert.equal(rovingIndex("Tab", 0, 5), undefined);
  assert.equal(rovingIndex("a", 0, 5), undefined);
  assert.equal(rovingIndex("ArrowDown", 0, 0), undefined);
});

test("a tablist wraps, because the set is small enough to hold in your head", () => {
  assert.equal(rovingTab("ArrowRight", 2, 3), 0);
  assert.equal(rovingTab("ArrowLeft", 0, 3), 2);
  assert.equal(rovingTab("Home", 2, 3), 0);
  assert.equal(rovingTab("End", 0, 3), 2);
  assert.equal(rovingTab("ArrowDown", 0, 3), undefined);
  assert.equal(rovingTab("ArrowRight", 0, 0), undefined);
});

test("sorting never mutates the rows it was given", () => {
  const rows = ["c", "a", "b"];
  const compare = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0);

  assert.deepEqual(applySort(rows, "ascending", compare), ["a", "b", "c"]);
  assert.deepEqual(applySort(rows, "descending", compare), ["c", "b", "a"]);
  assert.deepEqual(rows, ["c", "a", "b"], "the caller's array is untouched");

  // Unsorted returns the backend's own order, unchanged and not copied.
  assert.equal(applySort(rows, "none", compare), rows);
  assert.equal(applySort(rows, "ascending", undefined), rows);
});
