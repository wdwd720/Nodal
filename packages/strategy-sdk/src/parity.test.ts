import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import { semanticHash } from "./hash.ts";
import type { IRDocument } from "./types.ts";

/**
 * Go/TypeScript hash parity.
 *
 * These fixtures are the same files the Go test reads
 * (internal/strategy/ir/parity_test.go). They are shared data rather than
 * two hand-copied literals on purpose: if the two implementations ever
 * disagree, the platform would store two versions of one strategy and treat
 * an SDK document and its natural-language twin as different artifacts. A
 * failure here is release-blocking.
 *
 * Go is authoritative for the algorithm. Regenerate with:
 *   IR_PARITY_WRITE=1 go test ./internal/strategy/ir/ -run TestParity
 */
const here = dirname(fileURLToPath(import.meta.url));
const parityDir = join(here, "..", "..", "..", "internal", "strategy", "ir", "testdata", "parity");

interface ParityFixture {
  name: string;
  description: string;
  expected_hash: string;
  ir: IRDocument;
}

function loadFixtures(): ParityFixture[] {
  const files = readdirSync(parityDir).filter((f) => f.endsWith(".json"));
  return files.map((file) => {
    const raw = readFileSync(join(parityDir, file), "utf8");
    return JSON.parse(raw) as ParityFixture;
  });
}

test("parity fixtures are present", () => {
  const fixtures = loadFixtures();
  assert.ok(
    fixtures.length >= 4,
    `expected the full parity set, found ${fixtures.length}. Regenerate with IR_PARITY_WRITE=1 go test ./internal/strategy/ir/`,
  );
});

for (const fixture of loadFixtures()) {
  test(`semantic hash matches Go: ${fixture.name}`, () => {
    assert.equal(fixture.expected_hash.length, 64, "expected_hash is a hex sha256");
    const got = semanticHash(fixture.ir);
    assert.equal(
      got,
      fixture.expected_hash,
      `TypeScript and Go disagree on ${fixture.name}. This is release-blocking: ` +
        `the same strategy would be stored twice. ${fixture.description}`,
    );
  });
}

test("the momentum fixture reproduces the documented golden hash", () => {
  const momentum = loadFixtures().find((f) => f.name === "momentum");
  assert.ok(momentum, "the momentum fixture must exist");
  // This constant is also asserted in internal/strategy/ir/hash_test.go.
  assert.equal(
    semanticHash(momentum.ir),
    "8487051d87f34f11fc81bdab65b5baad3f6ec5cc77872331bfa3fa46f72513d5",
  );
});

test("the hash ignores provenance but tracks meaning", () => {
  const momentum = loadFixtures().find((f) => f.name === "momentum");
  assert.ok(momentum);
  const base = semanticHash(momentum.ir);

  // Provenance is excluded: an SDK document and a natural-language
  // compilation of the same strategy must dedupe.
  const reprovenanced: IRDocument = {
    ...momentum.ir,
    hash: "deadbeef",
    version: 99,
    built_at: "2030-01-01T00:00:00Z",
    lineage: { ...momentum.ir.lineage, source: "NATURAL_LANGUAGE", compiler_version: "other" },
  };
  assert.equal(semanticHash(reprovenanced), base, "provenance must not change the hash");

  // Meaning is covered: a different trade size is a different strategy.
  const resized = structuredClone(momentum.ir);
  const action = resized.actions.find((a) => a.kind === "CREATE_TRADE_INTENT");
  assert.ok(action?.intent);
  action.intent.sizing.notional_usd = "999.00";
  assert.notEqual(semanticHash(resized), base, "a changed trade size must change the hash");
});
