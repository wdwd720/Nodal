import assert from "node:assert/strict";
import { test } from "node:test";

import { readPublishableKey } from "./stripe.ts";

test("a well-formed publishable key is read with its mode", () => {
  const live = readPublishableKey("pk_live_51AbCdEf0123456789");
  assert.equal(live.ok, true);
  assert.equal(live.ok === true ? live.mode : "", "live");
  assert.equal(live.ok === true ? live.key : "", "pk_live_51AbCdEf0123456789");

  const sandbox = readPublishableKey("  pk_test_51AbCdEf0123456789  ");
  assert.equal(sandbox.ok, true);
  assert.equal(sandbox.ok === true ? sandbox.mode : "", "test");
  assert.equal(sandbox.ok === true ? sandbox.key : "", "pk_test_51AbCdEf0123456789");
});

test("an unconfigured deployment is a stated reason, not a form", () => {
  for (const absent of [undefined, "", "   "]) {
    const reading = readPublishableKey(absent);
    assert.equal(reading.ok, false);
    assert.match(reading.ok === false ? reading.reason : "", /no payment provider key configured/);
  }
});

test("a secret key in the publishable slot is refused, loudly", () => {
  // The failure this guard exists for: `sk_` pasted into the variable meant for
  // `pk_`. Mounting a form against it would send the key to Stripe from a
  // browser, which is exactly the thing that must never happen.
  const reading = readPublishableKey("sk_test_51AbCdEf0123456789");
  assert.equal(reading.ok, false);
  assert.match(reading.ok === false ? reading.reason : "", /not a publishable key/);
});

test("anything that is not a publishable key is refused", () => {
  const bad = [
    "pk_",
    "pk_test_",
    "pk_staging_abc123",
    "PK_TEST_abc123",
    "pk_test_abc-123",
    "rk_test_abc123",
    "whsec_abc123",
    "https://example.test/pk_test_abc",
  ];
  for (const value of bad) {
    const reading = readPublishableKey(value);
    assert.equal(reading.ok, false, value);
  }
});
