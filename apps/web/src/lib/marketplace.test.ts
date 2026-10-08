/**
 * The pure helpers of the strategy marketplace section: what a version's
 * state admits, and how what a person typed becomes the exact request the
 * API takes. None of them decides anything the backend does not decide
 * again; these tests hold them to saying the same thing.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  BUILD_FAILURES,
  BUILD_FAILURE_MEANING,
  EMPTY_VERSION_DRAFT,
  FOLLOW_OUTCOMES,
  MAX_SIMULATION_SNAPSHOTS,
  MODERATION_MEANING,
  OUTCOME_MEANING,
  RUN_TERMINATIONS,
  STATUS_MEANING,
  STRATEGY_INTENT_KINDS,
  STRATEGY_MODERATIONS,
  STRATEGY_STATUSES,
  TERMINATION_MEANING,
  buildVersionInput,
  canFork,
  canInstall,
  canSimulate,
  canSubmit,
  isPublicVersion,
  isReasonCode,
  isSourceFileName,
  manifestLeftOut,
  moderationTargets,
  operatorActionsFor,
  parseConfig,
  parseInstrumentIds,
  parseList,
  parsePositiveDecimal,
  parseSnapshotSet,
  parseWholeNumber,
  toggle,
  type VersionDraft,
} from "./marketplace.ts";

const INSTRUMENT = "0193b2e0-0000-7000-8000-0000000000a1";

test("every enumeration is stated once and every state has a meaning", () => {
  for (const list of [STRATEGY_STATUSES, STRATEGY_MODERATIONS, STRATEGY_INTENT_KINDS, FOLLOW_OUTCOMES, RUN_TERMINATIONS]) {
    assert.equal(new Set<string>(list).size, list.length);
  }
  for (const status of STRATEGY_STATUSES) assert.ok(STATUS_MEANING[status].length > 10, status);
  for (const m of STRATEGY_MODERATIONS) assert.ok(MODERATION_MEANING[m].length > 10, m);
  for (const o of FOLLOW_OUTCOMES) assert.ok(OUTCOME_MEANING[o].length > 10, o);
  for (const t of RUN_TERMINATIONS) assert.ok(TERMINATION_MEANING[t].length > 10, t);
  for (const f of BUILD_FAILURES) assert.ok(BUILD_FAILURE_MEANING[f].length > 10, f);
});

test("a build past one of the builder's bounds is a refusal with its own reason", () => {
  assert.deepEqual(
    BUILD_FAILURES.filter((f) => f.startsWith("BUILD_")),
    ["BUILD_TIMEOUT", "BUILD_OUTPUT_LIMIT", "BUILD_MEMORY_LIMIT"],
  );
  assert.match(BUILD_FAILURE_MEANING.COMPILE_FAILED, /compiler's output/);
});

test("a strategy may never be granted a kind that moves value out of an account", () => {
  for (const forbidden of ["TRANSFER_VALUE", "WITHDRAW", "BRIDGE_VALUE", "BORROW", "MINT_STABLE_ASSET"]) {
    assert.equal((STRATEGY_INTENT_KINDS as readonly string[]).includes(forbidden), false, forbidden);
  }
});

test("what a version's state admits matches the lifecycle", () => {
  const approved = { status: "APPROVED", moderation: "CLEARED" };
  assert.equal(isPublicVersion(approved), true);
  assert.equal(canInstall(approved), true);
  assert.equal(canFork(approved), true);
  assert.equal(canSimulate(approved), true);
  assert.equal(canSubmit(approved), false);

  const flagged = { status: "APPROVED", moderation: "FLAGGED" };
  assert.equal(isPublicVersion(flagged), true, "a flagged version is still shown");
  assert.equal(canInstall(flagged), false, "but cannot be installed");

  const takenDown = { status: "APPROVED", moderation: "TAKEN_DOWN" };
  assert.equal(isPublicVersion(takenDown), false);

  const draft = { status: "DRAFT", moderation: "UNREVIEWED" };
  assert.equal(isPublicVersion(draft), false);
  assert.equal(canSubmit(draft), true);
  assert.equal(canFork(draft), false);
  assert.equal(canSimulate(draft), false, "a draft has no artifact");

  assert.equal(canFork({ status: "DEPRECATED", moderation: "CLEARED" }), true);
  assert.equal(canInstall({ status: "DEPRECATED", moderation: "CLEARED" }), false);
});

test("operators are offered exactly the decisions a state admits", () => {
  assert.deepEqual(operatorActionsFor({ status: "SUBMITTED", moderation: "UNREVIEWED" }), ["validate", "reject"]);
  assert.deepEqual(operatorActionsFor({ status: "VALIDATED", moderation: "UNREVIEWED" }), ["review", "reject"]);
  assert.deepEqual(operatorActionsFor({ status: "IN_REVIEW", moderation: "UNREVIEWED" }), ["approve", "reject"]);
  assert.deepEqual(operatorActionsFor({ status: "APPROVED", moderation: "CLEARED" }), ["moderate", "deprecate"]);
  assert.deepEqual(operatorActionsFor({ status: "REJECTED", moderation: "UNREVIEWED" }), []);
  assert.deepEqual(operatorActionsFor({ status: "DRAFT", moderation: "UNREVIEWED" }), []);
  assert.deepEqual(moderationTargets("CLEARED"), ["FLAGGED", "TAKEN_DOWN"]);
  assert.deepEqual(moderationTargets("FLAGGED"), ["CLEARED", "TAKEN_DOWN"]);
  assert.deepEqual(moderationTargets("TAKEN_DOWN"), [], "taken down is final");
});

test("whole numbers are read exactly, digit by digit, and bounded", () => {
  assert.deepEqual(parseWholeNumber("3600", 60, 31_536_000, "x"), { ok: true, value: 3600 });
  assert.deepEqual(parseWholeNumber(" 100 ", 1, 100, "x"), { ok: true, value: 100 });
  assert.equal(parseWholeNumber("0", 1, 100, "x").ok, false);
  assert.equal(parseWholeNumber("101", 1, 100, "x").ok, false);
  for (const bad of ["", "1e3", "-1", "12.5", "0x10", "1 000", "9999999999"]) {
    assert.equal(parseWholeNumber(bad, 0, 1_000_000_000, "x").ok, false, bad);
  }
});

test("a notional cap is kept exactly as typed, and refused unless it is a positive decimal", () => {
  assert.deepEqual(parsePositiveDecimal("1000.00", "x"), { ok: true, value: "1000.00" });
  assert.deepEqual(parsePositiveDecimal("0.5", "x"), { ok: true, value: "0.5" });
  for (const bad of ["", "0", "0.00", "-5", "01", "1,000", "1e3", ".5", "5."]) {
    assert.equal(parsePositiveDecimal(bad, "x").ok, false, bad);
  }
});

test("lists, instrument ids, configuration and reason codes are read strictly", () => {
  assert.deepEqual(parseList("closes, marks\n\n trades "), ["closes", "marks", "trades"]);
  assert.deepEqual(parseInstrumentIds(`${INSTRUMENT.toUpperCase()}\n`), { ok: true, value: [INSTRUMENT] });
  assert.equal(parseInstrumentIds("not-an-id").ok, false);
  assert.deepEqual(parseConfig("order_units = 1000000\n\nwindow=5"), { ok: true, value: { order_units: "1000000", window: "5" } });
  assert.equal(parseConfig("Order=1").ok, false, "keys are lower-case names");
  assert.equal(parseConfig("a=1\na=2").ok, false, "a key appears once");
  assert.equal(parseConfig("no equals sign").ok, false);
  assert.equal(isReasonCode("MEETS_POLICY"), true);
  assert.equal(isReasonCode("meets_policy"), false);
  assert.equal(isReasonCode("X"), false);
  assert.equal(isSourceFileName("main.go"), true);
  assert.equal(isSourceFileName("main_test.go"), false);
  assert.equal(isSourceFileName("../main.go"), false);
});

test("toggle keeps the contract's order", () => {
  assert.deepEqual(toggle(STRATEGY_INTENT_KINDS, ["REDUCE_EXPOSURE"], "ACQUIRE_EXPOSURE"), ["ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"]);
  assert.deepEqual(toggle(STRATEGY_INTENT_KINDS, ["ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"], "ACQUIRE_EXPOSURE"), ["REDUCE_EXPOSURE"]);
});

const COMPLETE: VersionDraft = {
  ...EMPTY_VERSION_DRAFT,
  description: "buys strength",
  sourceCommit: "abc123",
  source: "package main\n",
  intentKinds: ["ACQUIRE_EXPOSURE"],
  instrumentIds: INSTRUMENT,
  maxNotional: "1000.00",
  maxIntentsPerRun: "2",
  minIntervalSeconds: "3600",
  config: "order_units=1000000",
  performanceFeeBps: "2000",
};

test("a complete version becomes the exact request body", () => {
  const built = buildVersionInput(COMPLETE, false);
  assert.ok(built.ok, built.ok ? "" : built.error);
  assert.deepEqual(built.value, {
    description: "buys strength",
    risk_class: "MODERATE",
    source_commit: "abc123",
    source: { "main.go": "package main\n" },
    manifest: {
      allowed_intent_kinds: ["ACQUIRE_EXPOSURE"],
      allowed_instrument_ids: [INSTRUMENT],
      allowed_instrument_kinds: [],
      max_notional_per_intent: "1000.00",
      max_intents_per_run: 2,
      min_interval_seconds: 3600,
    },
    fees: { performance_fee_bps: 2000, management_fee_bps: 0, high_water_mark: false },
    config: { order_units: "1000000" },
  });
});

test("a version without source or manifest is refused, and a fork without them inherits", () => {
  const bare: VersionDraft = { ...EMPTY_VERSION_DRAFT, description: "a fork", sourceCommit: "fork-1" };
  const refused = buildVersionInput(bare, false);
  assert.equal(refused.ok, false);
  assert.match(refused.ok ? "" : refused.error, /source/);
  assert.equal(manifestLeftOut(bare), true);
  const fork = buildVersionInput(bare, true);
  assert.ok(fork.ok);
  assert.equal(fork.value.source, undefined, "the parent's source is taken");
  assert.equal(fork.value.manifest, undefined, "the parent's manifest is taken");

  const half: VersionDraft = { ...bare, maxNotional: "10.00" };
  assert.equal(manifestLeftOut(half), false);
  assert.equal(buildVersionInput(half, true).ok, false, "a manifest that is started must be complete");
});

test("a simulation's snapshot keeps every figure as typed", () => {
  const raw = JSON.stringify([
    {
      as_of: "2026-10-07T12:00:00.000Z",
      instruments: [{ instrument_id: INSTRUMENT, kind: "SPOT_PAIR", base_decimals: 6, quote_decimals: 6, mark: "150.25" }],
      portfolio: { balance: "10000.00", positions: [] },
    },
  ]);
  const parsed = parseSnapshotSet(raw);
  assert.ok(parsed.ok);
  assert.equal(parsed.value[0]?.portfolio.balance, "10000.00");
  assert.equal(parsed.value[0]?.instruments[0]?.mark, "150.25");
  assert.equal(parseSnapshotSet("{").ok, false);
  assert.equal(
    parseSnapshotSet(JSON.stringify([{ as_of: "t", instruments: [], portfolio: { positions: [] } }])).ok,
    false,
    "a balance is required",
  );
});

test("a simulation's snapshot set keeps each snapshot's own time and is bounded", () => {
  const one = { as_of: "2026-10-01T00:00:00Z", instruments: [], portfolio: { balance: "1.00", positions: [] } };
  const set = parseSnapshotSet(JSON.stringify([one, { ...one, as_of: "2026-10-02T00:00:00Z" }]));
  assert.ok(set.ok);
  assert.deepEqual(
    set.value.map((s) => s.as_of),
    ["2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z"],
  );
  assert.equal(parseSnapshotSet("[]").ok, false);
  assert.equal(parseSnapshotSet(JSON.stringify([{ ...one, as_of: "" }])).ok, false, "each snapshot names its time");
  const many = Array.from({ length: MAX_SIMULATION_SNAPSHOTS + 1 }, () => one);
  assert.equal(parseSnapshotSet(JSON.stringify(many)).ok, false);
});
