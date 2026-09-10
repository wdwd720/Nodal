/**
 * The promise "anything you had typed is kept" is tested, not asserted.
 *
 * The interesting case is the one a unit test usually cannot reach: a full-page
 * navigation to the identity provider, which destroys every value in the tab.
 * `dropMemoryForTest()` reproduces exactly that — the map goes, the tab's
 * storage stays — so the round trip is proved rather than described.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  STASH_TTL_MS,
  clearFormState,
  clearSignInPending,
  consumeReturnPathOnce,
  dropMemoryForTest,
  isLocalPath,
  markSignInStarted,
  rememberReturnPath,
  resetConsumedReturnForTest,
  setClockForTest,
  signInPending,
  stashFormState,
  stashedFormCount,
  takeFormState,
  takeReturnPath,
} from "./survives-sign-in.ts";

/** A `sessionStorage` that behaves like the real one, including its throwing. */
function installStorage(options?: { readonly throws?: boolean }): Map<string, string> {
  const backing = new Map<string, string>();
  const storage = {
    getItem(key: string): string | null {
      if (options?.throws === true) throw new Error("denied");
      return backing.get(key) ?? null;
    },
    setItem(key: string, value: string): void {
      if (options?.throws === true) throw new Error("denied");
      backing.set(key, value);
    },
    removeItem(key: string): void {
      if (options?.throws === true) throw new Error("denied");
      backing.delete(key);
    },
  };
  (globalThis as { window?: unknown }).window = { sessionStorage: storage };
  return backing;
}

function removeStorage(): void {
  delete (globalThis as { window?: unknown }).window;
}

test("a value comes back once and only once", () => {
  installStorage();
  stashFormState("ticket.amount", { amount: "25", side: "BUY" });
  assert.deepEqual(takeFormState("ticket.amount"), { amount: "25", side: "BUY" });
  // Reading removes it: a draft that survived being read would be re-applied
  // to the next visit to the same form.
  assert.equal(takeFormState("ticket.amount"), undefined);
  removeStorage();
});

test("a draft survives the full-page trip to the identity provider", () => {
  installStorage();
  stashFormState("withdraw.amount", "40");
  // What `window.location.assign(LOGIN_PATH)` does to the tab.
  dropMemoryForTest();
  assert.equal(takeFormState("withdraw.amount"), "40");
  removeStorage();
});

test("money keeps its exact string form across the trip", () => {
  installStorage();
  // The one thing that would make this module a defect rather than a
  // convenience: a value that comes back as a double.
  stashFormState("ticket", { credits: "1000000.000001", price: "0.0000001" });
  dropMemoryForTest();
  const back = takeFormState<{ credits: string; price: string }>("ticket");
  assert.equal(typeof back?.credits, "string");
  assert.equal(back?.credits, "1000000.000001");
  assert.equal(back?.price, "0.0000001");
  removeStorage();
});

test("drafts are kept apart by key", () => {
  installStorage();
  stashFormState("a", "one");
  stashFormState("b", "two");
  assert.equal(takeFormState("a"), "one");
  assert.equal(takeFormState("b"), "two");
  removeStorage();
});

test("a stale draft is dropped rather than resurrected", () => {
  installStorage();
  let clock = 1_000_000;
  setClockForTest(() => clock);
  stashFormState("old", "typed a long time ago");
  clock = clock + STASH_TTL_MS + 1;
  assert.equal(takeFormState("old"), undefined);
  // And it is gone, not merely hidden.
  clock = 1_000_000;
  assert.equal(takeFormState("old"), undefined);
  setClockForTest(() => Date.now());
  removeStorage();
});

test("clearing forgets without reading", () => {
  installStorage();
  stashFormState("done", "submitted");
  clearFormState("done");
  assert.equal(takeFormState("done"), undefined);
  assert.equal(stashedFormCount(), 0);
  removeStorage();
});

test("a browser that denies storage still works inside the app", () => {
  // Private mode throws from sessionStorage rather than returning null. Losing
  // the round trip is a nuisance; throwing on a keystroke is a defect.
  installStorage({ throws: true });
  assert.doesNotThrow(() => {
    stashFormState("k", "v");
  });
  assert.equal(takeFormState("k"), "v", "the in-tab map still serves navigation inside the app");
  assert.doesNotThrow(() => {
    rememberReturnPath("/withdraw");
  });
  assert.equal(takeReturnPath(), undefined);
  removeStorage();
});

test("no storage at all is not an error either", () => {
  removeStorage();
  assert.doesNotThrow(() => {
    stashFormState("k", "v");
  });
  assert.equal(takeFormState("k"), "v");
  assert.equal(takeReturnPath(), undefined);
  assert.equal(signInPending(), false);
});

test("only a local path is accepted as a return path", () => {
  assert.equal(isLocalPath("/withdraw"), true);
  assert.equal(isLocalPath("/markets/abc?side=buy"), true);
  assert.equal(isLocalPath("//evil.example"), false);
  assert.equal(isLocalPath("https://evil.example/"), false);
  assert.equal(isLocalPath("/\\evil.example"), false);
  assert.equal(isLocalPath("javascript:alert(1)"), false);
});

test("a foreign return path is never stored and never returned", () => {
  const backing = installStorage();
  rememberReturnPath("https://evil.example/steal");
  assert.equal(backing.size, 0);
  assert.equal(takeReturnPath(), undefined);

  rememberReturnPath("/portfolio");
  assert.equal(takeReturnPath(), "/portfolio");
  assert.equal(takeReturnPath(), undefined, "the path is consumed once");
  removeStorage();
});

test("the return path reads the same however many times it is asked for", () => {
  // StrictMode renders the forwarding route twice. A read that consumed on the
  // first pass would send the customer to the dashboard in development and to
  // their own page in production.
  installStorage();
  resetConsumedReturnForTest();
  rememberReturnPath("/markets/abc");
  assert.equal(consumeReturnPathOnce(), "/markets/abc");
  assert.equal(consumeReturnPathOnce(), "/markets/abc");
  assert.equal(takeReturnPath(), undefined, "the underlying value was consumed exactly once");
  removeStorage();
});

test("the sign-in marker is set and cleared", () => {
  installStorage();
  assert.equal(signInPending(), false);
  markSignInStarted();
  assert.equal(signInPending(), true);
  clearSignInPending();
  assert.equal(signInPending(), false);
  removeStorage();
});
