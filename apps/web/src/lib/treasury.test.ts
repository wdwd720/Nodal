/**
 * The treasury section's pure helpers: limit states read from the treasury's
 * own codes, refusal fields read exactly as sent, one idempotency key per
 * confirmed request, and operator input checked without ever becoming a
 * number.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { ApiProblem } from "@controlplane/generated-client";

import {
  LIMIT_KIND_LABEL,
  LIMIT_KIND_MEANING,
  LIMIT_STATE_LABEL,
  TREASURY_LIMIT_KINDS,
  TREASURY_RUN_MODES,
  confirmAttempt,
  formatExactPrice,
  legOutcome,
  limitState,
  limitStateTone,
  orderStatusTone,
  outcomeTone,
  parseBaseUnits,
  parseCrossingDisclosure,
  parseDecimalPrice,
  parseLimitLines,
  parseText,
  parseUuid,
  parseWholeNumber,
  refusalDetail,
  sameRequest,
} from "./treasury.ts";

test("a limit's state is the treasury's codes, never a comparison made here", () => {
  assert.equal(limitState({ codes: [], used: "5000000" }), "room");
  assert.equal(limitState({ codes: ["TREASURY_INVENTORY_CAP"], used: "5000001" }), "refusing");
  // A refusal with no known use is still a refusal: the codes decide first.
  assert.equal(limitState({ codes: ["TREASURY_MARK_UNAVAILABLE"] }), "refusing");
  assert.equal(limitState({ codes: [] }), "unknown");
  assert.equal(limitState({ codes: [], used: undefined }), "unknown");
  assert.equal(limitStateTone("room"), "good");
  assert.equal(limitStateTone("refusing"), "bad");
  assert.equal(limitStateTone("unknown"), "warn");
  for (const state of ["room", "refusing", "unknown"] as const) {
    assert.ok(LIMIT_STATE_LABEL[state].length > 0);
  }
  for (const kind of TREASURY_LIMIT_KINDS) {
    assert.ok(LIMIT_KIND_LABEL[kind].length > 0);
    assert.ok(LIMIT_KIND_MEANING[kind].length > 0);
  }
});

test("an exact price is a moved decimal point", () => {
  assert.equal(formatExactPrice("10025", 2), "100.25");
  assert.equal(formatExactPrice("1001000", 4), "100.1");
  assert.equal(formatExactPrice("7", 0), "7");
  assert.equal(formatExactPrice("5", 3), "0.005");
});

test("a refusal's fields are read exactly as the backend sent them", () => {
  const problem = new ApiProblem({
    type: "urn:problem:risk_rejected",
    title: "Rejected by the risk kernel",
    status: 422,
    code: "RISK_REJECTED",
    fields: {
      reason: "RISK_CIRCUIT_BREAKER",
      rejection_code: "RISK_REJECTED",
      reason_codes: ["RISK_CIRCUIT_BREAKER", "TREASURY_INVENTORY_CAP", 7],
      action_id: "01a075f1-f847-7a98-adbc-5fc08c37e83f",
    },
  });
  const detail = refusalDetail(problem);
  assert.ok(detail !== undefined);
  assert.equal(detail.reason, "RISK_CIRCUIT_BREAKER");
  assert.equal(detail.rejectionCode, "RISK_REJECTED");
  assert.deepEqual(detail.reasonCodes, ["RISK_CIRCUIT_BREAKER", "TREASURY_INVENTORY_CAP"]);
  assert.equal(detail.actionId, "01a075f1-f847-7a98-adbc-5fc08c37e83f");

  const bare = new ApiProblem({ type: "urn:problem:forbidden", title: "Forbidden", status: 403, code: "FORBIDDEN" });
  assert.equal(refusalDetail(bare), undefined);
  assert.equal(refusalDetail(new Error("transport")), undefined);
  assert.equal(
    refusalDetail(new ApiProblem({ type: "t", title: "t", status: 400, code: "VALIDATION_FAILED", fields: { field: "price" } })),
    undefined,
  );
});

test("a quote leg is applied, or refused with its rejection code first", () => {
  assert.deepEqual(legOutcome({ action: { outcome: "APPLIED", reason_codes: [] } }), { applied: true, codes: [] });
  assert.deepEqual(
    legOutcome({ action: { outcome: "REJECTED", rejection_code: "RISK_REJECTED", reason_codes: ["TREASURY_INVENTORY_CAP"] } }),
    { applied: false, codes: ["RISK_REJECTED", "TREASURY_INVENTORY_CAP"] },
  );
  assert.deepEqual(
    legOutcome({ action: { outcome: "REJECTED", rejection_code: "TREASURY_POLICY_MISSING", reason_codes: ["TREASURY_POLICY_MISSING"] } }),
    { applied: false, codes: ["TREASURY_POLICY_MISSING"] },
  );
  assert.equal(outcomeTone("APPLIED"), "good");
  assert.equal(outcomeTone("REJECTED"), "bad");
  assert.equal(orderStatusTone("OPEN"), "info");
  assert.equal(orderStatusTone("REJECTED"), "bad");
  assert.equal(orderStatusTone("CANCELLED"), "neutral");
});

test("one key per confirmed request, reused on a retry and replaced for a change", () => {
  let minted = 0;
  const newKey = (): string => {
    minted = minted + 1;
    return `key-${String(minted)}-00000000`;
  };
  const first = confirmAttempt(undefined, { side: "BUY", quantity: "100" }, newKey);
  assert.equal(first.idempotencyKey, "key-1-00000000");
  const retry = confirmAttempt(first, { side: "BUY", quantity: "100" }, newKey);
  assert.equal(retry, first, "the same request keeps its attempt and its key");
  const changed = confirmAttempt(first, { side: "BUY", quantity: "101" }, newKey);
  assert.equal(changed.idempotencyKey, "key-2-00000000");
  assert.ok(sameRequest({ a: "1" }, { a: "1" }));
  assert.ok(!sameRequest({ a: "1" }, { a: "2" }));
});

test("operator input is checked as text and never becomes a number", () => {
  assert.deepEqual(parseBaseUnits(" 0001500000 ", "Quantity"), { ok: true, value: "1500000" });
  assert.equal(parseBaseUnits("0", "Quantity").ok, false);
  assert.deepEqual(parseBaseUnits("0", "Target", true), { ok: true, value: "0" });
  assert.equal(parseBaseUnits("1.5", "Quantity").ok, false);
  assert.equal(parseBaseUnits("-5", "Quantity").ok, false);
  assert.equal(parseBaseUnits("", "Quantity").ok, false);

  assert.deepEqual(parseDecimalPrice(" 100.25 ", "Price"), { ok: true, value: "100.25" });
  assert.deepEqual(parseDecimalPrice("100", "Price"), { ok: true, value: "100" });
  assert.equal(parseDecimalPrice("0.00", "Price").ok, false);
  assert.equal(parseDecimalPrice("-1", "Price").ok, false);
  assert.equal(parseDecimalPrice("1e3", "Price").ok, false);
  assert.equal(parseDecimalPrice(".5", "Price").ok, false);

  assert.deepEqual(parseWholeNumber("250", 0, 10000, "Spread"), { ok: true, value: 250 });
  assert.deepEqual(parseWholeNumber("86400000", 1, 86400000, "Mark age"), { ok: true, value: 86400000 });
  assert.equal(parseWholeNumber("10001", 0, 10000, "Spread").ok, false);
  assert.equal(parseWholeNumber("2.5", 0, 10000, "Spread").ok, false);
  assert.equal(parseWholeNumber("", 0, 10000, "Spread").ok, false);

  assert.deepEqual(parseUuid(" 01A075F1-F847-7A98-ADBC-5FC08C37E83F ", "Asset"), {
    ok: true,
    value: "01a075f1-f847-7a98-adbc-5fc08c37e83f",
  });
  assert.equal(parseUuid("not-a-uuid", "Asset").ok, false);

  assert.deepEqual(parseText("  seed the book ", "Reason", 500), { ok: true, value: "seed the book" });
  assert.equal(parseText("   ", "Reason", 500).ok, false);
  assert.equal(parseText("x".repeat(501), "Reason", 500).ok, false);
});

test("a risk policy's caps are read one per line, each id once", () => {
  const a = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
  const b = "01a075f1-f846-719e-bcbb-493c8281c524";
  assert.deepEqual(parseLimitLines(`${a} 100000000\n\n  ${b.toUpperCase()}=0005  `, "Inventory caps"), {
    ok: true,
    value: [
      { id: a, max: "100000000" },
      { id: b, max: "5" },
    ],
  });
  assert.deepEqual(parseLimitLines("   \n", "Inventory caps"), { ok: true, value: [] });
  assert.equal(parseLimitLines(`${a}`, "Inventory caps").ok, false);
  assert.equal(parseLimitLines(`${a} 1.5`, "Inventory caps").ok, false);
  assert.equal(parseLimitLines(`not-an-id 5`, "Inventory caps").ok, false);
  const twice = parseLimitLines(`${a} 1\n${a} 2`, "Inventory caps");
  assert.equal(twice.ok, false);
  assert.match(twice.ok ? "" : twice.error, /line 2/);
});

test("a crossing disclosure must say the activity is simulated", () => {
  assert.equal(parseCrossingDisclosure("House books may trade against you here.").ok, false);
  assert.equal(parseCrossingDisclosure("Simulated.").ok, false, "too short");
  assert.deepEqual(parseCrossingDisclosure("  A house book may take the other side of your simulated order.  "), {
    ok: true,
    value: "A house book may take the other side of your simulated order.",
  });
});

test("the forms offer only the modes the treasury runs in", () => {
  assert.deepEqual([...TREASURY_RUN_MODES], ["PAPER", "SANDBOX"]);
});
