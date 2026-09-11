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
  clearAllFormState,
  clearFormState,
  clearSignInPending,
  dropMemoryForTest,
  isLocalPath,
  markSignInStarted,
  sameShape,
  setClockForTest,
  signInPending,
  stashFormState,
  stashedFormCount,
  takeFormState,
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
    // The index half of the Storage interface, which `clearAllFormState` walks
    // because there is no other way to ask a Storage what is in it. Modelled
    // exactly as the browser behaves, including the part that matters: removing
    // during a walk shifts every later index down by one.
    get length(): number {
      if (options?.throws === true) throw new Error("denied");
      return backing.size;
    },
    key(index: number): string | null {
      if (options?.throws === true) throw new Error("denied");
      return [...backing.keys()][index] ?? null;
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
  removeStorage();
});

test("no storage at all is not an error either", () => {
  removeStorage();
  assert.doesNotThrow(() => {
    stashFormState("k", "v");
  });
  assert.equal(takeFormState("k"), "v");
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

test("the sign-in marker is set and cleared", () => {
  installStorage();
  assert.equal(signInPending(), false);
  markSignInStarted();
  assert.equal(signInPending(), true);
  clearSignInPending();
  assert.equal(signInPending(), false);
  removeStorage();
});

/* --------------------------------------------------------------------------
 * Signing out
 * ------------------------------------------------------------------------ */

test("signing out forgets every draft this tab holds, and the sign-in marker", () => {
  // The shared-computer case this module's header is written about. The tab
  // does not die when a session ends, so something has to empty it.
  const backing = installStorage();
  stashFormState("withdraw.amount", { amount: "1234", destinationId: "dest-7" });
  stashFormState("buy-credits.draft", { amount: "50.00", key: "k" });
  markSignInStarted();
  backing.set("someone-elses.key", "not ours");
  assert.equal(stashedFormCount(), 2);

  clearAllFormState();

  assert.equal(stashedFormCount(), 0, "the in-tab map is empty");
  assert.deepEqual(
    [...backing.keys()].filter((key) => key.startsWith("nodal.")),
    [],
    "no nodal-prefixed key survives the sign-out",
  );
  assert.equal(signInPending(), false, "the sign-in marker goes with the session");
  assert.equal(backing.get("someone-elses.key"), "not ours", "and nothing else is touched");

  // And the value really is gone rather than merely dropped from the map: a
  // full-page navigation would otherwise recover it from the mirror.
  dropMemoryForTest();
  assert.equal(takeFormState("withdraw.amount"), undefined);
  removeStorage();
});

test("a browser that denies storage still signs out cleanly", () => {
  installStorage({ throws: true });
  stashFormState("withdraw.amount", { amount: "1234" });
  assert.doesNotThrow(() => {
    clearAllFormState();
  });
  assert.equal(stashedFormCount(), 0, "the in-tab copy is gone, which is the one that could leak");
  removeStorage();
});

/* --------------------------------------------------------------------------
 * What comes back out of storage
 * ------------------------------------------------------------------------ */

test("a recovered draft of the wrong shape is dropped rather than cast", () => {
  // `sessionStorage` is a string a previous build wrote and a person with the
  // developer tools open can edit. A page that casts it and hands it to the
  // BigInt constructor blanks the whole application on every load until the tab
  // is closed, with no message and nothing to click.
  installStorage();
  const template = { amount: "", side: "BUY", toleranceBps: 100 };

  stashFormState("ticket", { amount: 25, side: "BUY", toleranceBps: 100 });
  assert.equal(
    takeFormState("ticket", (value): value is typeof template => sameShape(template, value)),
    undefined,
    "a numeric amount is not the string shape the page will parse",
  );

  stashFormState("ticket", { amount: "25", side: "BUY" });
  assert.equal(
    takeFormState("ticket", (value): value is typeof template => sameShape(template, value)),
    undefined,
    "a missing field is a different shape too",
  );

  // What does match comes back untouched, extra fields and all: a stash written
  // by yesterday's build still opens today's form.
  stashFormState("ticket", { amount: "25", side: "SELL", toleranceBps: 50, leftover: "x" });
  assert.deepEqual(
    takeFormState("ticket", (value): value is typeof template => sameShape(template, value)),
    { amount: "25", side: "SELL", toleranceBps: 50, leftover: "x" },
  );
  removeStorage();
});

test("the shape check reads through objects and arrays", () => {
  assert.equal(sameShape({ a: "" }, { a: "x" }), true);
  assert.equal(sameShape({ a: "" }, { a: 1 }), false);
  assert.equal(sameShape({ a: "" }, null), false);
  assert.equal(sameShape({ a: "" }, []), false);
  assert.equal(sameShape({ a: { b: 0 } }, { a: { b: 7 } }), true);
  assert.equal(sameShape({ a: { b: 0 } }, { a: { b: "7" } }), false);
  assert.equal(sameShape({ ids: [""] }, { ids: ["a", "b"] }), true);
  assert.equal(sameShape({ ids: [""] }, { ids: ["a", 2] }), false);
  assert.equal(sameShape({ ids: [""] }, { ids: [] }), true);
  // An empty template says nothing about the element type, so it accepts any
  // array. That is deliberate: a template with no example cannot describe one.
  assert.equal(sameShape({ ids: [] }, { ids: [1, "two"] }), true);
  assert.equal(sameShape("", "text"), true);
  assert.equal(sameShape("", 3), false);
});

test("recovering a draft is idempotent, because a discarded render must not eat it", () => {
  // React discards the first render of a suspended tree — which is every
  // `React.lazy` route — and renders it again when the chunk lands. The read
  // that recovers a draft happens in that render, so it has to be safe to
  // happen twice: the first one took the value and the second one found
  // nothing, and the customer came back from a step-up to an empty form.
  installStorage();
  stashFormState("create-asset.draft", { name: "Round trip asset" });

  const first = takeFormState<{ name: string }>("create-asset.draft");
  assert.deepEqual(first, { name: "Round trip asset" });
  // What the hook now does in the same breath as the read.
  if (first !== undefined) stashFormState("create-asset.draft", first);

  // The render that actually mounts reads the same value rather than nothing.
  assert.deepEqual(takeFormState("create-asset.draft"), { name: "Round trip asset" });
  removeStorage();
});
