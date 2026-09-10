/**
 * Source-level rules, and the generated documents this console will not start
 * without.
 *
 * These are tests rather than review notes because they have to hold for code
 * nobody has written yet. A reviewer can miss a `// TODO: wire this up` in a
 * view; this cannot.
 *
 * Two rules:
 *
 *   1. **Nothing is left as an unexplained stub (goal §62).** The markers a
 *      §62 sweep looks for — TODO, FIXME, "not implemented", "coming soon",
 *      "stub", "temporary" — are refused outright in this app's sources. Where
 *      something genuinely is not reachable (Investigate and Escalate have no
 *      HTTP route; GET /v1/admin/users/{userId} does not exist yet) the console
 *      states it to the operator on screen, which is a decision that has been
 *      made and written down, not a note to self left in a comment.
 *
 *   2. **The generated documents are the ones this console understands.**
 *      `src/generated/authority.json` and `decisions.json` are produced by
 *      `internal/adminplane` and a Go golden test fails when they go stale. The
 *      corresponding risk on this side is that the console reads a document it
 *      cannot use: a version it was not written against, a missing surface for
 *      a route the shell renders, a capability the gates view would address
 *      without a permission to check. Those are asserted here, so a
 *      regeneration that changes the shape fails a test rather than a browser.
 */
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

import { ROUTES } from "./app.ts";
import { parseAuthority, SUPPORTED_AUTHORITY_VERSION } from "./authority.ts";
import type { Authority } from "./authority.ts";
import { VERBS } from "./decide.ts";

const here = path.dirname(fileURLToPath(import.meta.url));
const appRoot = path.join(here, "..");

interface SourceFile {
  readonly path: string;
  readonly text: string;
}

/**
 * This file, which necessarily quotes every word it forbids and so cannot be
 * scanned for them. It is the only exclusion: the other tests are scanned like
 * any other source, because a stub parked in a test is still a stub.
 */
const SELF = "src/scan.test.ts";

/** Every source file that ships or builds this app; never dist/ or node_modules. */
function sourceFiles(): SourceFile[] {
  const roots = [path.join(appRoot, "src"), path.join(appRoot, "public")];
  const files: SourceFile[] = [
    ...["build.mjs", "server.mjs"].map((name) => ({
      path: name,
      text: fs.readFileSync(path.join(appRoot, name), "utf8"),
    })),
  ];
  const walk = (dir: string): void => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.(ts|mjs|css|html)$/.test(entry.name)) continue;
      files.push({ path: path.relative(appRoot, full).split(path.sep).join("/"), text: fs.readFileSync(full, "utf8") });
    }
  };
  for (const root of roots) walk(root);
  return files.filter((f) => f.path !== SELF).sort((a, b) => (a.path < b.path ? -1 : 1));
}

/**
 * Every line, comments included. The marker rule is a rule about comments — a
 * TODO lives in one — so it must see them.
 */
function matches(files: readonly SourceFile[], pattern: RegExp): string[] {
  const hits: string[] = [];
  for (const file of files) {
    for (const [i, line] of file.text.split(/\r?\n/).entries()) {
      if (pattern.test(line)) hits.push(`${file.path}:${i + 1}: ${line.trim()}`);
    }
  }
  return hits;
}

/**
 * Code only, with line and block-comment bodies removed. The rules about what
 * the code *does* use this, so that a comment explaining why something is
 * forbidden does not read as a use of it.
 */
function codeMatches(files: readonly SourceFile[], pattern: RegExp): string[] {
  const hits: string[] = [];
  for (const file of files) {
    for (const [i, line] of file.text.split(/\r?\n/).entries()) {
      const code = line.replace(/\/\/.*$/, "").replace(/^\s*\*.*$/, "").replace(/^\s*\/\*.*$/, "");
      if (pattern.test(code)) hits.push(`${file.path}:${i + 1}: ${line.trim()}`);
    }
  }
  return hits;
}

function authority(): Authority {
  return parseAuthority(JSON.parse(fs.readFileSync(path.join(here, "generated", "authority.json"), "utf8")));
}

test("no unexplained stub is left anywhere in the console", () => {
  const banned: ReadonlyArray<readonly [RegExp, string]> = [
    [/\bTODO\b/, "a TODO is a decision nobody made"],
    [/\bFIXME\b/, "a FIXME is a defect nobody filed"],
    [/\bXXX\b/, "an XXX marks something the author did not want to explain"],
    [/\bHACK\b/, "say what the constraint is instead"],
    [/coming soon/i, "an operator console never promises a future"],
    [/\bnot implemented\b/i, "state what does exist and why the rest does not"],
    [/\bstub\b/i, "a stub in an operator console is a control that lies"],
    [/\btemporary\b/i, "nothing here is temporary; say what it is"],
  ];
  const failures: string[] = [];
  for (const [pattern, why] of banned) {
    for (const hit of matches(sourceFiles(), pattern)) failures.push(`${hit}\n    -> ${why}`);
  }
  assert.deepEqual(failures, [], `goal §62: no marker may be left unresolved:\n${failures.join("\n")}`);
});

test("only the api and authority modules speak HTTP", () => {
  // Every call to the API goes through the generated client so a contract
  // change is a type error rather than a runtime surprise. authority.ts is the
  // exception: it fetches a static document from this app's own origin, not
  // from the API, so it has no client to go through.
  const allowed = new Set(["src/api.ts", "src/authority.ts", "server.mjs"]);
  const files = sourceFiles().filter((f) => !allowed.has(f.path) && !f.path.endsWith(".test.ts"));
  const hits = [...codeMatches(files, /\bfetch\s*\(/), ...codeMatches(files, /\bXMLHttpRequest\b/)];
  assert.deepEqual(hits, [], `all API access goes through src/api.ts:\n${hits.join("\n")}`);
});

test("no operator-supplied text reaches the document as markup", () => {
  // dom.ts sets textContent and there is deliberately no helper that accepts
  // HTML. Reasons, evidence references and provider errors are all text an
  // operator or a provider wrote.
  const files = sourceFiles();
  const hits = [
    ...codeMatches(files, /\binnerHTML\b/),
    ...codeMatches(files, /\bouterHTML\b/),
    ...codeMatches(files, /\binsertAdjacentHTML\b/),
  ];
  assert.deepEqual(hits, [], `stored XSS in an operator console:\n${hits.join("\n")}`);
});

test("the generated authority document is the version this console understands", () => {
  const doc = authority();
  assert.equal(doc.version, SUPPORTED_AUTHORITY_VERSION);
  assert.match(doc.source, /adminplane/, "the document must name the package that generated it");
});

test("every route the shell renders has a surface in the authority document", () => {
  const doc = authority();
  const ids = new Set(doc.surfaces.map((s) => s.id));
  const missing = ROUTES.filter((r) => !ids.has(r.surface)).map((r) => `${r.path} -> ${r.surface}`);
  assert.deepEqual(missing, [], `a route with no surface renders as "you cannot read this":\n${missing.join("\n")}`);
});

test("every surface declares at least one permission that can read it", () => {
  const doc = authority();
  const permissions = new Set(doc.permissions);
  const bad: string[] = [];
  for (const surface of doc.surfaces) {
    if (surface.read_any_of.length === 0) bad.push(`${surface.id} names no read permission`);
    for (const p of surface.read_any_of) {
      if (!permissions.has(p)) bad.push(`${surface.id} reads with ${p}, which is not a declared permission`);
    }
    for (const write of surface.writes ?? []) {
      for (const p of write.any_of) {
        if (!permissions.has(p)) bad.push(`${write.id} needs ${p}, which is not a declared permission`);
      }
    }
  }
  assert.deepEqual(bad, [], bad.join("\n"));
});

test("the gates surface can address every declared capability", () => {
  const doc = authority();
  assert.ok(doc.capabilities.length > 0, "the document must declare the capabilities the gates view lists");
  const writes = new Set((doc.surfaces.find((s) => s.id === "gates")?.writes ?? []).map((w) => w.id));
  // The six steps of the real ceremony are declared writes. sandbox and
  // unsandbox (ADR-0023) are not yet exported by adminplane, and views/gates.ts
  // falls back to gate.propose for them -- which is the permission
  // internal/gates.Admin.sandboxOp actually requires. If the document ever
  // grows them, the view stops using the fallback and this test says so.
  for (const action of ["propose", "approve", "activate", "suspend", "resume", "revoke"]) {
    assert.ok(writes.has(`gate.${action}`), `gate.${action} must be a declared write`);
  }
  assert.ok(writes.has("gate.propose"), "the sandbox fallback depends on gate.propose existing");
});

test("the decision vectors cover every verb the console can offer", () => {
  const raw = fs.readFileSync(path.join(here, "generated", "decisions.json"), "utf8");
  const doc = JSON.parse(raw) as { version: number; cases: Array<{ verb: string }> };
  assert.equal(doc.version, 1, "the vector document's version is the one decide.test.ts replays");
  const covered = new Set(doc.cases.map((c) => c.verb));
  const missing = VERBS.filter((v) => !covered.has(v));
  assert.deepEqual(missing, [], `no vector proves the console's verdict for: ${missing.join(", ")}`);
});
