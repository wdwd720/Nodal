/**
 * Source-tree walker shared by the two guard tests.
 *
 * It exists so the rules in `source-scan.test.ts` (no floating point near money,
 * no hardcoded figures, no raw buttons) and `honesty.test.ts` (the forbidden
 * vocabulary of PART 112) both see exactly the same set of files. A rule that
 * only covers the files someone remembered to list is not a rule.
 */
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL(".", import.meta.url));
/** apps/web/src */
export const SRC_ROOT = join(here, "..");
/** apps/web */
export const APP_ROOT = join(SRC_ROOT, "..");

export interface SourceFile {
  /** Path relative to apps/web, with forward slashes, e.g. "src/pages/Home.tsx". */
  readonly path: string;
  readonly text: string;
}

function walk(dir: string, out: string[]): void {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      walk(full, out);
      continue;
    }
    if (/\.(ts|tsx|css|html)$/.test(entry)) {
      out.push(full);
    }
  }
}

/**
 * Every source file the guards apply to: the whole app tree plus index.html,
 * minus the guard tests themselves, which necessarily quote what they forbid.
 *
 * The audit reproductions are on that list for exactly the same reason and no
 * other. `src/lib/audit-frontend.test.ts` asserts that the sentence
 * UI_UX_SYSTEM §4 bans appears nowhere, and it cannot make that assertion
 * without writing the sentence down three times — once in the pattern and twice
 * in the paragraph saying why; `e2e/audit-frontend.spec.ts` explains in prose
 * which numeric constructor it is avoiding and why. A rule that could not tell
 * a citation from a use would force both files to be vague about what they are
 * testing, which is the opposite of what a reproduction is for. Every file that
 * is not a guard — every page, every component, every other spec — is still
 * scanned, which is the whole surface these rules were ever about.
 */
export function sourceFiles(): SourceFile[] {
  const found: string[] = [];
  walk(SRC_ROOT, found);
  found.push(join(APP_ROOT, "index.html"));
  const e2e = join(APP_ROOT, "e2e");
  walk(e2e, found);

  const excluded = new Set([
    "src/lib/source-scan.test.ts",
    "src/lib/honesty.test.ts",
    "src/lib/audit-frontend.test.ts",
    "e2e/honesty.spec.ts",
    "e2e/audit-frontend.spec.ts",
  ]);
  return found
    .map((full) => ({
      path: relative(APP_ROOT, full).split(sep).join("/"),
      text: readFileSync(full, "utf8"),
    }))
    .filter((file) => !excluded.has(file.path))
    .sort((a, b) => (a.path < b.path ? -1 : 1));
}

/** The subset that renders user interface: pages and components. */
export function uiFiles(): SourceFile[] {
  return sourceFiles().filter(
    (file) => file.path.startsWith("src/pages/") || file.path.startsWith("src/components/"),
  );
}

/** Reports every line matching a pattern, for readable failure messages. */
export function matches(files: readonly SourceFile[], pattern: RegExp): string[] {
  const hits: string[] = [];
  for (const file of files) {
    const lines = file.text.split("\n");
    for (let i = 0; i < lines.length; i = i + 1) {
      const line = lines[i] ?? "";
      const re = new RegExp(pattern.source, pattern.flags.replace("g", ""));
      if (re.test(line)) {
        hits.push(`${file.path}:${String(i + 1)}: ${line.trim()}`);
      }
    }
  }
  return hits;
}
