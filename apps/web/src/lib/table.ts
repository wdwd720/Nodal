/**
 * The pure logic behind `DataTable`: which row the keyboard moves to, and what
 * a column's sort state becomes when its header is pressed.
 *
 * It lives here rather than inside the component because a rule about keyboard
 * navigation is testable and a component is not — the unit runner in this
 * package strips types, it does not compile JSX, so anything worth asserting
 * belongs in a plain module. That constraint has been good for this codebase:
 * the interesting part of most components turns out to be a function.
 */

/** How a column is currently sorted. `none` means sortable but not sorted. */
export type SortState = "none" | "ascending" | "descending";

/**
 * Pressing a sortable header cycles through three states, not two.
 *
 * The third state matters: a table that can only be sorted one way or the other
 * gives a reader no way back to the order the backend returned, and the
 * backend's order is frequently the meaningful one — most recent first, or the
 * order a settlement plan will execute in.
 */
export function nextSort(current: SortState): SortState {
  if (current === "none") return "ascending";
  if (current === "ascending") return "descending";
  return "none";
}

/**
 * Where the keyboard moves next, or `undefined` when the key is not ours and
 * the browser should keep it.
 *
 * It CLAMPS rather than wraps. Wrapping from the last row to the first is
 * disorienting in a table of a customer's own transactions: the reader loses
 * their place and cannot tell whether they moved one row or five hundred.
 */
export function rovingIndex(key: string, current: number, count: number): number | undefined {
  if (count <= 0) return undefined;
  const last = count - 1;
  const clamp = (index: number): number => (index < 0 ? 0 : index > last ? last : index);
  switch (key) {
    case "ArrowDown":
      return clamp(current + 1);
    case "ArrowUp":
      return clamp(current - 1);
    case "Home":
      return 0;
    case "End":
      return last;
    case "PageDown":
      return clamp(current + 10);
    case "PageUp":
      return clamp(current - 10);
    default:
      return undefined;
  }
}

/**
 * Where a tablist moves next. Unlike a table, a tablist WRAPS — the WAI-ARIA
 * pattern says so, the set is small enough to hold in your head, and every tab
 * is one keypress from every other.
 */
export function rovingTab(key: string, current: number, count: number): number | undefined {
  if (count <= 0) return undefined;
  switch (key) {
    case "ArrowRight":
      return (current + 1) % count;
    case "ArrowLeft":
      return (current - 1 + count) % count;
    case "Home":
      return 0;
    case "End":
      return count - 1;
    default:
      return undefined;
  }
}

/**
 * Applies a sort without touching the caller's array.
 *
 * `none` returns the rows in the order the backend sent them, which is a real
 * answer and often the right one.
 */
export function applySort<T>(
  rows: readonly T[],
  state: SortState,
  compare: ((a: T, b: T) => number) | undefined,
): readonly T[] {
  if (state === "none" || compare === undefined) return rows;
  const copy = rows.slice();
  copy.sort(compare);
  return state === "ascending" ? copy : copy.reverse();
}
