/**
 * The vocabulary is complete and does not contradict itself.
 *
 * The list in `docs/product/USER_JOURNEY.md` §11 is the requirement, so it is
 * written out here in the document's own words: if somebody removes a situation
 * from `errors.ts`, this fails naming the one that went missing, rather than
 * failing somewhere in a page six months later.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  EMPTY_STATES,
  SITUATIONS,
  SITUATION_BY_ID,
  situationFor,
  situationForCode,
  type SituationId,
} from "./errors.ts";

/** USER_JOURNEY.md §11, verbatim, in order. */
const REQUIRED: ReadonlyArray<readonly [string, SituationId]> = [
  ["Provider unavailable", "provider-unavailable"],
  ["Payment pending", "payment-pending"],
  ["Payment failed", "payment-failed"],
  ["Balance updating", "balance-updating"],
  ["Trade rejected", "trade-rejected"],
  ["Insufficient Credits", "insufficient-credits"],
  ["Price changed", "price-changed"],
  ["Market paused", "market-paused"],
  ["Account restricted", "account-restricted"],
  ["Verification pending", "verification-pending"],
  ["Verification rejected", "verification-rejected"],
  ["Withdrawal unavailable", "withdrawal-unavailable"],
  ["Payout delayed", "payout-delayed"],
  ["Session expired", "session-expired"],
  ["Network offline", "network-offline"],
];

test("every situation the user journey lists has a sentence", () => {
  const missing: string[] = [];
  for (const [name, id] of REQUIRED) {
    const situation = SITUATION_BY_ID[id];
    if (situation === undefined) {
      missing.push(`${name} (${id}) has no entry`);
      continue;
    }
    if (situation.name !== name) {
      missing.push(`${id} is named "${situation.name}", the journey calls it "${name}"`);
    }
    if (situation.sentence.trim() === "") {
      missing.push(`${id} has an empty sentence`);
    }
  }
  assert.deepEqual(missing, [], `USER_JOURNEY §11 situations without copy:\n${missing.join("\n")}`);
});

test("session expiry and step-up route the customer somewhere", () => {
  // §10: a 401 mid-action routes to sign-in with a return path; a step-up
  // offers the step-up and returns to the same place. Both need a control.
  assert.equal(SITUATION_BY_ID["session-expired"].recovery.kind, "sign-in");
  assert.equal(SITUATION_BY_ID["step-up-required"].recovery.kind, "step-up");
  assert.notEqual(SITUATION_BY_ID["session-expired"].recovery.label, "");
  assert.notEqual(SITUATION_BY_ID["step-up-required"].recovery.label, "");
});

test("a recovery that needs no control carries no label", () => {
  // A button that cannot change the answer is a dead control, which is the
  // defect this codebase enforces against with a type in Button.tsx.
  for (const situation of SITUATIONS) {
    const needsControl = situation.recovery.kind !== "wait" && situation.recovery.kind !== "none";
    assert.equal(
      situation.recovery.label !== "",
      needsControl,
      `${situation.id}: recovery kind "${situation.recovery.kind}" and label "${situation.recovery.label}" disagree`,
    );
  }
});

test("a recovery that navigates names where it goes", () => {
  for (const situation of SITUATIONS) {
    if (situation.recovery.kind !== "go") continue;
    assert.ok(
      situation.recovery.to !== undefined && situation.recovery.to.startsWith("/"),
      `${situation.id} navigates but names no local path`,
    );
  }
});

test("no problem code means two different things", () => {
  const seen = new Map<string, SituationId>();
  for (const situation of SITUATIONS) {
    for (const code of situation.codes) {
      const first = seen.get(code);
      assert.equal(first, undefined, `${code} maps to both ${String(first)} and ${situation.id}`);
      seen.set(code, situation.id);
    }
  }
});

test("codes resolve to their situation and unknown codes resolve to nothing", () => {
  assert.equal(situationForCode("QUOTE_EXPIRED")?.id, "price-changed");
  assert.equal(situationForCode("INSUFFICIENT_BUYING_POWER")?.id, "insufficient-credits");
  assert.equal(situationForCode("UNAUTHENTICATED")?.id, "session-expired");
  assert.equal(situationForCode("STEP_UP_REQUIRED")?.id, "step-up-required");
  // Not one of the fifteen: undefined is the honest answer, because
  // `explain()` already produces a sentence for it and guessing a recovery
  // action here would put the wrong control in front of somebody.
  assert.equal(situationForCode("LEDGER_UNBALANCED"), undefined);
  assert.equal(situationForCode(""), undefined);
});

test("an offline browser is told it is offline whatever the code was", () => {
  assert.equal(situationFor({ code: "UNREACHABLE", online: false })?.id, "network-offline");
  assert.equal(situationFor({ code: "PROVIDER_UNAVAILABLE", online: false })?.id, "network-offline");
  assert.equal(situationFor({ code: "PROVIDER_UNAVAILABLE", online: true })?.id, "provider-unavailable");
  assert.equal(situationFor({ code: "PROVIDER_UNAVAILABLE" })?.id, "provider-unavailable");
});

test("no sentence names a provider exception or a raw server message", () => {
  for (const situation of SITUATIONS) {
    assert.doesNotMatch(situation.sentence, /exception|stack|traceback|panic:|HTTP \d/i, situation.id);
    assert.doesNotMatch(situation.sentence, /something went wrong/i, situation.id);
  }
});

test("the empty states say what would be here and how to cause it", () => {
  // Goal §50. The four the goal names by example must exist and must not use
  // enterprise phrasing.
  for (const id of ["holdings", "agents", "activity", "verification"] as const) {
    const text = EMPTY_STATES[id];
    assert.ok(text.body.length > 0, `${id} has no body`);
    assert.doesNotMatch(text.body, /no records|criteria|no data available/i, id);
  }
  assert.match(EMPTY_STATES.holdings.action ?? "", /Explore markets/);
  assert.match(EMPTY_STATES.agents.action ?? "", /Create your first agent/);
  assert.match(EMPTY_STATES.activity.body, /Your activity will appear here/);
  assert.match(EMPTY_STATES.verification.body, /Verify when you're ready to request withdrawals/);
});

test("every empty state with an action names a local path for it", () => {
  for (const [id, text] of Object.entries(EMPTY_STATES)) {
    if (text.action === undefined) {
      assert.equal(text.to, undefined, `${id} names a path but no action`);
      continue;
    }
    assert.ok(text.to !== undefined && text.to.startsWith("/"), `${id} has an action with no path`);
  }
});
