/**
 * The pure helpers of the advanced (sandbox) area: counting decisions, paging,
 * parsing, durations and the buying power V2 layout. None of them touches a
 * monetary value; these tests pin down that they name, order and count
 * exactly what the backend sent and nothing else.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ADVANCED_MODES,
  BUYING_POWER_V2_FIELDS,
  BUYING_POWER_V2_GROUPS,
  EFFECT_STATUSES,
  EFFECT_STATUS_MEANING,
  FIRST_PAGE,
  GATE_REASONS,
  INSTRUMENT_KINDS,
  LIVE_NOT_IMPLEMENTED,
  PERSISTABLE_MODES,
  PHYSICAL_SETTLEMENT,
  back,
  componentLabel,
  decisionFor,
  distinctReasons,
  duplicatedModes,
  effectStatusTone,
  formatMillis,
  formatSeconds,
  forward,
  groupByField,
  intentStatusTone,
  isCanonicalUuid,
  modeTone,
  pageNumber,
  parseInstrumentKind,
  settlementStyleLabel,
  summariseLive,
  verdictTone,
} from "./advanced.ts";

function decisions(live: { allowed: boolean; reasons: string[] } | undefined, paperAllowed: boolean) {
  const list = [
    { mode: "PAPER", allowed: paperAllowed, reasons: paperAllowed ? [] : ["CAPABILITY_GATE_NOT_ACTIVE"] },
    { mode: "SANDBOX", allowed: false, reasons: ["MODE_NOT_SUPPORTED"] },
    { mode: "DEVNET", allowed: false, reasons: ["MODE_NOT_SUPPORTED"] },
  ];
  return live === undefined ? list : [...list, { mode: "LIVE", ...live }];
}

test("the enumerations are the contract's, without repeats", () => {
  for (const list of [ADVANCED_MODES, INSTRUMENT_KINDS, EFFECT_STATUSES, PERSISTABLE_MODES]) {
    assert.equal(new Set(list).size, list.length);
  }
  assert.deepEqual([...ADVANCED_MODES], ["PAPER", "SANDBOX", "DEVNET", "LIVE"]);
  assert.equal(INSTRUMENT_KINDS.length, 9);
});

test("an economic intent can never be stored LIVE, so LIVE is not a persistable mode", () => {
  assert.deepEqual([...PERSISTABLE_MODES], ["PAPER", "SANDBOX", "DEVNET"]);
  assert.equal((PERSISTABLE_MODES as readonly string[]).includes("LIVE"), false);
});

test("decisionFor finds a mode's decision and reports a missing one as missing", () => {
  const list = decisions({ allowed: false, reasons: [LIVE_NOT_IMPLEMENTED] }, true);
  assert.equal(decisionFor(list, "LIVE")?.allowed, false);
  assert.equal(decisionFor(list, "PAPER")?.allowed, true);
  assert.equal(decisionFor(decisions(undefined, true), "LIVE"), undefined);
});

test("duplicatedModes names every mode reported twice", () => {
  assert.deepEqual(duplicatedModes(decisions({ allowed: false, reasons: [] }, true)), []);
  assert.deepEqual(
    duplicatedModes([{ mode: "LIVE" }, { mode: "PAPER" }, { mode: "LIVE" }, { mode: "LIVE" }]),
    ["LIVE"],
  );
});

test("summariseLive counts refusals, the reason, allowances and gaps without deciding anything", () => {
  const summary = summariseLive([
    { capability: "MARGIN", decisions: decisions({ allowed: false, reasons: ["CAPABILITY_GATE_NOT_ACTIVE", LIVE_NOT_IMPLEMENTED] }, true) },
    { capability: "EQUITIES", decisions: decisions({ allowed: false, reasons: ["CAPABILITY_GATE_NOT_ACTIVE"] }, false) },
    { capability: "BRIDGE_X", decisions: decisions({ allowed: true, reasons: [] }, false) },
    { capability: "UNREPORTED", decisions: decisions(undefined, true) },
  ]);
  assert.deepEqual(summary, {
    capabilities: 4,
    refused: 2,
    notImplemented: 1,
    allowed: ["BRIDGE_X"],
    unreported: ["UNREPORTED"],
    runnable: 2,
  });
  assert.deepEqual(summariseLive([]), {
    capabilities: 0,
    refused: 0,
    notImplemented: 0,
    allowed: [],
    unreported: [],
    runnable: 0,
  });
});

test("distinctReasons lists every code once, sorted", () => {
  assert.deepEqual(
    distinctReasons([
      { capability: "A", decisions: decisions({ allowed: false, reasons: [LIVE_NOT_IMPLEMENTED] }, false) },
      { capability: "B", decisions: decisions({ allowed: false, reasons: [LIVE_NOT_IMPLEMENTED] }, true) },
    ]),
    ["CAPABILITY_GATE_NOT_ACTIVE", LIVE_NOT_IMPLEMENTED, "MODE_NOT_SUPPORTED"],
  );
});

test("every reason gates.DecideMode can return has a gloss", () => {
  for (const code of [
    LIVE_NOT_IMPLEMENTED,
    "CAPABILITY_GATE_NOT_ACTIVE",
    "MODE_NOT_SUPPORTED",
    "CAPABILITY_KILL_SWITCHED",
    "ENVIRONMENT_UNKNOWN",
    "MODE_UNKNOWN",
    "NOT_AN_ADVANCED_CAPABILITY",
  ]) {
    assert.ok((GATE_REASONS[code] ?? "").length > 20, code);
  }
});

test("a cursor trail goes forward by the backend's cursor and back along the way it came", () => {
  assert.equal(pageNumber(FIRST_PAGE), 1);
  assert.equal(back(FIRST_PAGE), FIRST_PAGE, "there is nothing before the first page");

  const second = forward(FIRST_PAGE, "c2");
  const third = forward(second, "c3");
  assert.deepEqual(third, { current: "c3", previous: [undefined, "c2"] });
  assert.equal(pageNumber(third), 3);

  assert.deepEqual(back(third), second);
  assert.deepEqual(back(back(third)), FIRST_PAGE);
});

test("parseInstrumentKind accepts only the contract's kinds and says what it ignored", () => {
  assert.deepEqual(parseInstrumentKind(null), { kind: undefined, ignored: undefined });
  assert.deepEqual(parseInstrumentKind(""), { kind: undefined, ignored: undefined });
  assert.deepEqual(parseInstrumentKind("OPTION"), { kind: "OPTION", ignored: undefined });
  assert.deepEqual(parseInstrumentKind("option"), { kind: undefined, ignored: "option" });
  assert.deepEqual(parseInstrumentKind("BOND"), { kind: undefined, ignored: "BOND" });
});

test("isCanonicalUuid accepts the 36-character form only", () => {
  assert.equal(isCanonicalUuid("01920000-0000-7000-8000-5342582d5553"), true);
  assert.equal(isCanonicalUuid("01920000-0000-7000-8000-5342582D5553"), true);
  assert.equal(isCanonicalUuid("019200000000700080005342582d5553"), false);
  assert.equal(isCanonicalUuid("01920000-0000-7000-8000-5342582d555"), false);
  assert.equal(isCanonicalUuid(" 01920000-0000-7000-8000-5342582d5553"), false);
  assert.equal(isCanonicalUuid("not-a-uuid"), false);
});

test("durations are stated exactly, in the largest unit that divides them", () => {
  const cases: ReadonlyArray<readonly [number, string]> = [
    [2_592_000, "30 days"],
    [86_400, "1 day"],
    [7_200, "2 hours"],
    [3_600, "1 hour"],
    [60, "1 minute"],
    [90, "90 seconds"],
    [1, "1 second"],
    [0, "0 seconds"],
  ];
  for (const [seconds, expected] of cases) {
    assert.equal(formatSeconds(seconds), expected, String(seconds));
  }
  assert.match(formatSeconds(-1), /not a whole/);
  assert.match(formatSeconds(1.5), /not a whole/);

  assert.equal(formatMillis(300_000), "5 minutes");
  assert.equal(formatMillis(60_000), "1 minute");
  assert.equal(formatMillis(1_500), "1500 ms");
  assert.match(formatMillis(-5), /not a whole/);
});

test("groupByField keeps the backend's order, within and across groups", () => {
  const lines = [
    { field: "eligible_collateral", n: 1 },
    { field: "eligible_equity", n: 2 },
    { field: "eligible_collateral", n: 3 },
    { field: "eligible_equity", n: 4 },
  ];
  assert.deepEqual(groupByField(lines), [
    { field: "eligible_collateral", lines: [lines[0], lines[2]] },
    { field: "eligible_equity", lines: [lines[1], lines[3]] },
  ]);
  assert.deepEqual(groupByField([]), []);
});

test("the buying power V2 layout shows each of the sixteen components exactly once", () => {
  assert.equal(BUYING_POWER_V2_FIELDS.length, 16);
  assert.equal(new Set(BUYING_POWER_V2_FIELDS).size, 16);
  assert.equal(BUYING_POWER_V2_GROUPS.filter((group) => group.emphasis).length, 1);
  for (const group of BUYING_POWER_V2_GROUPS) {
    for (const component of group.components) {
      assert.ok(component.note.length > 20, `${component.field} explains itself`);
    }
  }
  assert.equal(componentLabel("eligible_equity"), "Eligible equity");
  assert.equal(componentLabel("a_field_the_backend_added"), "a_field_the_backend_added");
});

test("the non-physical settlement style is named for what it means", () => {
  assert.equal(settlementStyleLabel(PHYSICAL_SETTLEMENT), "Physical delivery of the underlying");
  assert.match(settlementStyleLabel("ANY_OTHER"), /^Settled in value/);
});

test("tones follow the status, and LIVE is always drawn as a refusal", () => {
  assert.equal(effectStatusTone("APPLIED"), "good");
  assert.equal(effectStatusTone("UNKNOWN"), "warn");
  assert.equal(effectStatusTone("FAILED"), "bad");
  assert.equal(effectStatusTone("SOMETHING_NEW"), "neutral");
  assert.equal(intentStatusTone("REJECTED"), "bad");
  assert.equal(intentStatusTone("RECONCILED"), "good");
  assert.equal(verdictTone("ALLOW"), "good");
  assert.equal(verdictTone("REJECT"), "bad");
  assert.equal(modeTone("LIVE"), "bad");
  assert.equal(modeTone("SANDBOX"), "info");
  for (const status of EFFECT_STATUSES) {
    assert.ok(EFFECT_STATUS_MEANING[status].length > 10, status);
  }
});
