/**
 * What a customer typed, kept across the sign-in round trip.
 *
 * `docs/product/USER_JOURNEY.md` §10: a 401 during a sensitive action keeps the
 * form state, routes to sign-in with a return path, and on return re-validates
 * before allowing the submit. §11 promises the customer that in words —
 * "anything you had typed is kept" — so the promise has to be true.
 *
 * # Why this is not purely in memory
 *
 * Signing in is a full-page navigation: `startSignIn()` sends the browser to
 * `GET /v1/auth/login`, the identity provider renders its own pages, and the
 * callback navigates back. Every JavaScript value in the tab is destroyed on
 * the way out. A module-level variable therefore cannot survive the trip, and
 * saying it does would be a promise this code cannot keep.
 *
 * So the store is a module-level `Map` — which is what serves every navigation
 * *inside* the app — mirrored into `sessionStorage`, which is what survives the
 * trip out and back. The mirror is deliberately narrow:
 *
 *   - `sessionStorage`, never `localStorage`: it is scoped to the one tab and
 *     dies with it, so a shared computer does not hand the next person a form;
 *   - it holds only what the customer typed and the path they were on. No
 *     token, no session, no balance, no figure the backend computed;
 *   - an entry expires after thirty minutes and is removed the moment it is
 *     read, so a stale amount cannot be resurrected into a later submit;
 *   - every access is wrapped, because a browser in private mode throws from
 *     `sessionStorage` rather than returning null, and losing a draft is a
 *     nuisance while a crash is a defect.
 *
 * # Why the state is written through on every change
 *
 * A session can expire at any keystroke, not only at submit. If the stash were
 * written only when the app noticed the 401, a form abandoned by a redirect the
 * app did not initiate would be lost. Writing through costs one small string
 * per keystroke and makes the promise unconditional.
 */
import { useCallback, useEffect, useRef, useState } from "react";

/** Namespace, so nothing else in the origin can collide with a draft. */
const PREFIX = "nodal.form.";
/** Where the app was when the session went away. */
const RETURN_KEY = "nodal.return-path";
/** Set while a sign-in navigation is in flight, so `/` knows not to flash. */
const PENDING_KEY = "nodal.sign-in-pending";

/** Thirty minutes. A draft older than that is not what the customer meant. */
export const STASH_TTL_MS = 30 * 60 * 1000;

interface Entry {
  readonly value: unknown;
  /** `Date.now()` at the moment it was written. */
  readonly at: number;
}

/** The in-tab store. Serves every navigation that does not leave the page. */
const memory = new Map<string, Entry>();

function session(): Storage | undefined {
  try {
    return window.sessionStorage;
  } catch {
    // Private mode, or a browser configured to deny storage. The in-memory map
    // still works for navigation inside the app; only the round trip is lost,
    // and losing a draft is better than throwing on a keystroke.
    return undefined;
  }
}

function writeMirror(key: string, entry: Entry): void {
  const store = session();
  if (store === undefined) return;
  try {
    store.setItem(PREFIX + key, JSON.stringify(entry));
  } catch {
    // Quota, or a value that will not serialise. The memory copy stands.
  }
}

function readMirror(key: string): Entry | undefined {
  const store = session();
  if (store === undefined) return undefined;
  try {
    const raw = store.getItem(PREFIX + key);
    if (raw === null) return undefined;
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return undefined;
    const at: unknown = (parsed as { at?: unknown }).at;
    if (typeof at !== "number") return undefined;
    return { value: (parsed as { value?: unknown }).value, at };
  } catch {
    return undefined;
  }
}

function dropMirror(key: string): void {
  const store = session();
  if (store === undefined) return;
  try {
    store.removeItem(PREFIX + key);
  } catch {
    /* nothing to do; the memory copy is already gone */
  }
}

/** Injectable clock, so the expiry rule is testable without waiting. */
let now: () => number = () => Date.now();

/** Test seam. Production code never calls this. */
export function setClockForTest(clock: () => number): void {
  now = clock;
}

/**
 * Test seam: empties the in-tab map without touching the mirror, which is
 * exactly what a full-page navigation to the identity provider does. It is the
 * only way to prove the claim this module is built on — that a draft survives
 * the trip — rather than asserting it in a comment.
 */
export function dropMemoryForTest(): void {
  memory.clear();
}

/**
 * Keeps a value for the round trip. Overwrites any earlier value for the key.
 */
export function stashFormState(key: string, value: unknown): void {
  const entry: Entry = { value, at: now() };
  memory.set(key, entry);
  writeMirror(key, entry);
}

/**
 * Returns the kept value and forgets it.
 *
 * Reading removes: a draft that survives being read would be re-applied to the
 * next visit to the same form, which is how a customer ends up submitting an
 * amount they typed an hour ago.
 */
export function takeFormState<T>(key: string): T | undefined {
  const entry = memory.get(key) ?? readMirror(key);
  memory.delete(key);
  dropMirror(key);
  if (entry === undefined) return undefined;
  if (now() - entry.at > STASH_TTL_MS) return undefined;
  return entry.value as T;
}

/** Forgets a kept value without reading it. Called once a submit succeeds. */
export function clearFormState(key: string): void {
  memory.delete(key);
  dropMirror(key);
}

/** How many drafts are held. For tests and for the security page. */
export function stashedFormCount(): number {
  return memory.size;
}

/* --------------------------------------------------------------------------
 * The return path
 * ------------------------------------------------------------------------ */

/**
 * Records where to come back to, and refuses anything that is not a local path.
 *
 * The value ends up in a redirect, so an absolute URL or a protocol-relative
 * `//host` would be an open redirect wearing a return path's clothes. The
 * backend applies the same rule to its own `return_to` (`internal/identity`);
 * this is the browser-side half of it.
 */
export function isLocalPath(path: string): boolean {
  return path.startsWith("/") && !path.startsWith("//") && !path.includes("\\");
}

export function rememberReturnPath(path: string): void {
  if (!isLocalPath(path)) return;
  const store = session();
  if (store === undefined) return;
  try {
    store.setItem(RETURN_KEY, path);
  } catch {
    /* the customer lands on the dashboard instead; nothing is lost */
  }
}

/** The remembered path, forgotten as it is read. Never returns a foreign URL. */
export function takeReturnPath(): string | undefined {
  const store = session();
  if (store === undefined) return undefined;
  try {
    const raw = store.getItem(RETURN_KEY);
    store.removeItem(RETURN_KEY);
    if (raw === null || !isLocalPath(raw)) return undefined;
    return raw;
  } catch {
    return undefined;
  }
}

/**
 * The remembered path, read once per page load however many times it is asked
 * for.
 *
 * The route that forwards a returning customer runs inside `StrictMode`, which
 * renders a component twice in development to surface exactly this class of
 * bug. A plain `takeReturnPath()` in a render would consume the path on the
 * first pass and hand `undefined` to the second, so the customer would land on
 * the dashboard in development and on their own page in production — the worst
 * kind of difference between the two. Caching the first answer for the lifetime
 * of the page makes the read idempotent and the behaviour identical.
 */
let consumedReturn: { readonly value: string | undefined } | undefined;

export function consumeReturnPathOnce(): string | undefined {
  consumedReturn ??= { value: takeReturnPath() };
  return consumedReturn.value;
}

/** Test seam: forgets that the path was already consumed. */
export function resetConsumedReturnForTest(): void {
  consumedReturn = undefined;
}

/**
 * Marks that this tab has just sent the browser to the identity provider.
 *
 * The OIDC callback lands on `/`, which is the public landing page for a
 * signed-out visitor. Without this marker the app would paint the marketing
 * page for the fraction of a second it takes `GET /v1/me` to answer, which is a
 * poor thing to show somebody who has just finished signing in.
 */
export function markSignInStarted(): void {
  const store = session();
  if (store === undefined) return;
  try {
    store.setItem(PENDING_KEY, "1");
  } catch {
    /* the landing page flashes; nothing is broken */
  }
}

export function signInPending(): boolean {
  const store = session();
  if (store === undefined) return false;
  try {
    return store.getItem(PENDING_KEY) !== null;
  } catch {
    return false;
  }
}

export function clearSignInPending(): void {
  const store = session();
  if (store === undefined) return;
  try {
    store.removeItem(PENDING_KEY);
  } catch {
    /* nothing to do */
  }
}

/* --------------------------------------------------------------------------
 * The hook
 * ------------------------------------------------------------------------ */

export interface SurvivingState<T> {
  readonly value: T;
  /** Updates the value and writes it through, so any redirect keeps it. */
  readonly set: (next: T) => void;
  /** Forgets the kept copy. Call it once the action has actually succeeded. */
  readonly clear: () => void;
  /** True when this mount recovered a value the customer had typed earlier. */
  readonly recovered: boolean;
}

/**
 * Form state that outlives a sign-in.
 *
 * ```tsx
 * const amount = useSurvivesSignIn("buy-credits.amount", "");
 * <FormField value={amount.value} onChange={amount.set} />
 * ```
 *
 * On mount it takes back anything stashed under `key`; from then on every `set`
 * writes through. `clear()` on success is what stops a completed purchase from
 * pre-filling the next one.
 */
export function useSurvivesSignIn<T>(key: string, initial: T): SurvivingState<T> {
  // The recovery happens once, during the first render, so the first paint
  // already shows what the customer typed rather than an empty field that
  // fills in a tick later.
  const recoveredRef = useRef<boolean | undefined>(undefined);
  const [value, setValue] = useState<T>(() => {
    const kept = takeFormState<T>(key);
    recoveredRef.current = kept !== undefined;
    return kept === undefined ? initial : kept;
  });

  // A recovered value is put straight back, so that a customer who is bounced
  // to sign-in twice does not lose it the second time.
  useEffect(() => {
    if (recoveredRef.current === true) stashFormState(key, value);
    // Runs once per key: the write-through in `set` covers every later change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  const set = useCallback(
    (next: T) => {
      setValue(next);
      stashFormState(key, next);
    },
    [key],
  );

  const clear = useCallback(() => {
    clearFormState(key);
  }, [key]);

  return { value, set, clear, recovered: recoveredRef.current === true };
}
