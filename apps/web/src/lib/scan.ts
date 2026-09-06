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
 */
export function sourceFiles(): SourceFile[] {
  const found: string[] = [];
  walk(SRC_ROOT, found);
  found.push(join(APP_ROOT, "index.html"));
  const e2e = join(APP_ROOT, "e2e");
  walk(e2e, found);

  const excluded = new Set(["src/lib/source-scan.test.ts", "src/lib/honesty.test.ts", "e2e/honesty.spec.ts"]);
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
