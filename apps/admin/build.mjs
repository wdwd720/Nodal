// Build the operator console for the browser with no bundler and no build
// dependency beyond Node itself.
//
// The repository pins its JavaScript dependencies in one lockfile that several
// agents write to at once, so this app deliberately adds none: it uses Node's
// own TypeScript type stripper (node:module.stripTypeScriptTypes, Node >= 22.13)
// to turn each source file into the ES module a browser can load, and copies
// the one runtime dependency — openapi-fetch, already pinned for
// packages/generated-client — into dist/vendor.
//
// Type *checking* is not this script's job and never silently passes: run
// `pnpm --filter @controlplane/admin typecheck`, which is what CI runs. Strip
// mode erases types, it does not verify them.
//
// Usage:
//   node build.mjs           build once into dist/
//   node build.mjs --watch   rebuild on change
import { stripTypeScriptTypes } from "node:module";
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const srcDir = path.join(here, "src");
const publicDir = path.join(here, "public");
const outDir = path.join(here, "dist");
const require = createRequire(import.meta.url);

/** The workspace package whose source is compiled alongside ours. */
const CLIENT_PKG = "@controlplane/generated-client";
const clientEntry = path.resolve(here, "../../packages/generated-client/src/index.ts");
const openapiFetchEntry = resolveOpenapiFetch();

function resolveOpenapiFetch() {
  // openapi-fetch ships a self-contained ESM bundle with no imports of its
  // own, so it can be served to the browser verbatim.
  const candidates = [
    path.resolve(here, "../../packages/generated-client/node_modules/openapi-fetch/dist/index.mjs"),
    path.resolve(here, "../../node_modules/openapi-fetch/dist/index.mjs"),
  ];
  for (const c of candidates) {
    if (fs.existsSync(c)) return c;
  }
  try {
    return require.resolve("openapi-fetch");
  } catch {
    throw new Error(
      "openapi-fetch is not installed. Run `pnpm install` at the repository root; this app adds no dependency of its own.",
    );
  }
}

/** Rewrite the import specifiers a browser cannot resolve. */
function rewriteSpecifiers(code, fromFile) {
  const outFile = outPathFor(fromFile);
  const vendorDir = path.join(outDir, "vendor");
  const toClient = relSpecifier(path.dirname(outFile), path.join(vendorDir, "generated-client.js"));
  return code.replace(
    /(\bfrom\s*|\bimport\s*\(\s*)(["'])([^"']+)\2/g,
    (whole, prefix, quote, spec) => {
      let next = spec;
      if (spec === CLIENT_PKG || spec.startsWith(`${CLIENT_PKG}/`)) {
        next = toClient;
      } else if (spec === "openapi-fetch") {
        next = "./openapi-fetch.js";
      } else if (spec.startsWith(".") && spec.endsWith(".ts")) {
        next = `${spec.slice(0, -3)}.js`;
      }
      return `${prefix}${quote}${next}${quote}`;
    },
  );
}

function relSpecifier(fromDir, toFile) {
  const rel = path.relative(fromDir, toFile).split(path.sep).join("/");
  return rel.startsWith(".") ? rel : `./${rel}`;
}

function outPathFor(srcFile) {
  const rel = path.relative(srcDir, srcFile);
  return path.join(outDir, rel.replace(/\.ts$/, ".js"));
}

function compile(srcFile, destFile) {
  const source = fs.readFileSync(srcFile, "utf8");
  const stripped = stripTypeScriptTypes(source, { mode: "strip", sourceMap: false });
  fs.mkdirSync(path.dirname(destFile), { recursive: true });
  fs.writeFileSync(destFile, rewriteSpecifiers(stripped, srcFile));
}

function* walk(dir) {
  if (!fs.existsSync(dir)) return;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) yield* walk(full);
    else yield full;
  }
}

function build() {
  const started = Date.now();
  fs.rmSync(outDir, { recursive: true, force: true });
  fs.mkdirSync(path.join(outDir, "vendor"), { recursive: true });

  // 1. The one runtime dependency, verbatim.
  fs.copyFileSync(openapiFetchEntry, path.join(outDir, "vendor", "openapi-fetch.js"));

  // 2. The workspace client, type-stripped.
  const clientSource = fs.readFileSync(clientEntry, "utf8");
  fs.writeFileSync(
    path.join(outDir, "vendor", "generated-client.js"),
    rewriteSpecifiers(stripTypeScriptTypes(clientSource, { mode: "strip" }), clientEntry).replace(
      /from\s*(["'])[^"']*openapi-fetch[^"']*\1/g,
      'from "./openapi-fetch.js"',
    ),
  );

  // 3. Our own sources, minus tests, which run under Node and never ship.
  let count = 0;
  for (const file of walk(srcDir)) {
    if (file.endsWith(".test.ts")) continue;
    if (file.endsWith(".ts")) {
      compile(file, outPathFor(file));
      count++;
    } else {
      // Generated JSON and anything else in src/ is copied as-is.
      const dest = outPathFor(file);
      fs.mkdirSync(path.dirname(dest), { recursive: true });
      fs.copyFileSync(file, dest);
    }
  }

  // 4. Static shell.
  for (const file of walk(publicDir)) {
    const dest = path.join(outDir, path.relative(publicDir, file));
    fs.mkdirSync(path.dirname(dest), { recursive: true });
    fs.copyFileSync(file, dest);
  }

  console.log(`admin: built ${count} module(s) into ${path.relative(here, outDir)} in ${Date.now() - started}ms`);
}

build();

if (process.argv.includes("--watch")) {
  let pending = null;
  const rebuild = () => {
    clearTimeout(pending);
    pending = setTimeout(() => {
      try {
        build();
      } catch (err) {
        console.error("admin: build failed:", err instanceof Error ? err.message : err);
      }
    }, 60);
  };
  for (const dir of [srcDir, publicDir]) {
    fs.watch(dir, { recursive: true }, rebuild);
  }
  console.log("admin: watching src/ and public/");
}
