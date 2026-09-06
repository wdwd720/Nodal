/**
 * Proves the promise `format.ts` makes: every refusal an operator can be shown
 * is phrased in this console's own words, and nothing is phrased that the Go
 * side does not declare.
 *
 * This is the console half of the guarantee `internal/adminplane` provides.
 * That package exports `reasons` into `src/generated/authority.json` from the
 * same table `adminplane.Decide` returns, and a Go golden test fails if the
 * file goes stale. So a reason added in Go arrives here on the next
 * regeneration, and this test fails until someone writes the sentence an
 * operator will read. Without it, a new reason would reach the screen as a
 * bare `APPROVER_NOT_DISTINCT` — which is not wrong, but is not an explanation
 * either, and a refusal an operator cannot read is a refusal they will retry.
 */
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

import { parseAuthority } from "./authority.ts";
import {
  codeText,
  coveredCodes,
  coveredReasons,
  formatDuration,
  formatInstant,
  kindLabel,
  reasonText,
  relativeInstant,
  secondsUntil,
} from "./format.ts";

const here = path.dirname(fileURLToPath(import.meta.url));

function authority() {
  return parseAuthority(JSON.parse(fs.readFileSync(path.join(here, "generated", "authority.json"), "utf8")));
}

test("every reason the Go side declares has wording here", () => {
  const declared = authority().reasons;
  assert.ok(declared.length > 0, "the authority document declares no reasons");
  const covered = new Set(coveredReasons());
  const missing = declared.filter((r) => !covered.has(r));
  assert.deepEqual(
    missing,
    [],
    `internal/adminplane declares reasons this console cannot phrase: ${missing.join(", ")}`,
  );
});

test("no wording exists for a reason the Go side does not declare", () => {
  const declared = new Set(authority().reasons);
  const stale = coveredReasons().filter((r) => !declared.has(r));
  assert.deepEqual(stale, [], `this console phrases reasons that no longer exist: ${stale.join(", ")}`);
});

test("an unknown reason is shown verbatim rather than swallowed", () => {
  // The alternative — a generic fallback — would turn a reason the console has
  // not been taught into "something went wrong", which is the one thing this
  // module exists to prevent.
  assert.equal(reasonText("SOME_REASON_FROM_THE_FUTURE"), "SOME_REASON_FROM_THE_FUTURE");
  assert.equal(codeText("SOME_CODE_FROM_THE_FUTURE"), "SOME_CODE_FROM_THE_FUTURE");
});

test("no refusal is phrased as a generic failure", () => {
  const vague = [/something went wrong/i, /unknown error/i, /oops/i, /try again later/i, /unexpected/i];
  for (const reason of coveredReasons()) {
    const text = reasonText(reason);
    assert.notEqual(text, reason, `${reason} has no wording`);
    for (const pattern of vague) {
      assert.ok(!pattern.test(text), `${reason} is phrased vaguely: ${text}`);
    }
  }
  for (const code of coveredCodes()) {
    const text = codeText(code);
    for (const pattern of vague) {
      assert.ok(!pattern.test(text), `${code} is phrased vaguely: ${text}`);
    }
  }
});

test("the self-approval refusal names dual control, not just a denial", () => {
  // This is the sentence the disabled Approve control carries on an action you
  // proposed. It has to say why, or the operator's next move is to find
  // someone's session rather than someone's second opinion.
  const text = reasonText("SELF_APPROVAL");
  assert.match(text, /you proposed this action/i);
  assert.match(text, /different person/i);
});

test("the agent refusal is absolute, not conditional", () => {
  // An agent is never permitted here under any elevation, and the wording must
  // not suggest that some role or grant would change the answer.
  assert.match(reasonText("AGENT_PRINCIPAL"), /never/i);
});

test("every declared action kind renders a readable label", () => {
  for (const spec of authority().action_kinds) {
    const label = kindLabel(spec.kind);
    assert.ok(label.length > 0, `${spec.kind} has no label`);
    assert.ok(!label.includes("_"), `${spec.kind} label still reads as an identifier: ${label}`);
  }
});

test("instants render in UTC to the second, and an unreadable one is shown as given", () => {
  assert.equal(formatInstant("2026-09-06T12:00:00Z"), "2026-09-06 12:00:00Z");
  assert.equal(formatInstant("2026-09-06T12:00:00.123456Z"), "2026-09-06 12:00:00Z");
  assert.equal(formatInstant(null), "—");
  assert.equal(formatInstant(""), "—");
  // Not a date: shown verbatim rather than as "Invalid Date".
  assert.equal(formatInstant("not-a-date"), "not-a-date");
});

test("durations render in whole units and never round a deadline up", () => {
  assert.equal(formatDuration(0), "0s");
  assert.equal(formatDuration(59), "59s");
  assert.equal(formatDuration(60), "1m");
  assert.equal(formatDuration(119), "1m");
  assert.equal(formatDuration(3600), "1h");
  assert.equal(formatDuration(3660), "1h 1m");
  assert.equal(formatDuration(90000), "1d 1h");
  assert.equal(formatDuration(-30), "-30s");
});

test("time remaining is counted, not guessed", () => {
  const now = new Date("2026-09-06T12:00:00Z");
  assert.equal(secondsUntil("2026-09-06T12:05:00Z", now), 300);
  assert.equal(secondsUntil("2026-09-06T11:55:00Z", now), -300);
  assert.equal(secondsUntil(null, now), null);
  assert.equal(secondsUntil("not-a-date", now), null);

  assert.equal(relativeInstant("2026-09-06T12:05:00Z", now), "in 5m");
  assert.equal(relativeInstant("2026-09-06T11:55:00Z", now), "5m ago");
  assert.equal(relativeInstant(null, now), "—");
  // An expiry that has just passed must read as past, never as "in 0s".
  assert.equal(relativeInstant("2026-09-06T11:59:59Z", now), "1s ago");
});
