/**
 * One idempotency key per request, minted once and kept for exactly as long as
 * the request it belongs to.
 *
 * `Idempotency-Key` answers one question for the backend: "is this the same
 * request I have already seen?" Both ways of getting that wrong are defects,
 * and this application had both.
 *
 * A NEW KEY ON EVERY PRESS makes a retry a second request. The customer presses
 * Buy, the reply is lost in transit, they press it again, and the backend has
 * no way to know it is the same purchase — so it makes another one. That is the
 * failure `Idempotency-Key` exists to prevent, and minting inside the click
 * handler reintroduces it.
 *
 * A KEPT KEY OVER A CHANGED BODY is the opposite failure and reads worse. The
 * backend compares the body against the one it recorded under that key, finds a
 * different `quote_id` or a different amount, and refuses with
 * `INVALID_IDEMPOTENCY_REUSE` — so the customer, who has just changed their
 * order and confirmed it, is told something about a header instead of something
 * about their trade. Nothing they can do on the page fixes it, because the page
 * is the thing holding the stale key.
 *
 * The rule that avoids both is one sentence: A KEY BELONGS TO A BODY. Mint it
 * at the moment of confirmation, keep it while the request being confirmed is
 * the same request, and mint a new one the moment any part of the body changes.
 * `signature` is the caller's statement of what "the same request" means for
 * that command — every field the backend will compare, joined — and it is the
 * only thing this module knows about the request.
 *
 * It survives the sign-in round trip for the same reason the draft does: a
 * session that expires between the press and the answer must come back to a
 * retry of the SAME request (USER_JOURNEY §10, scenario J), not to a second
 * one. So the key and the signature it was minted for are stashed together in
 * `survives-sign-in.ts` and recovered together — a key recovered without the
 * body it belongs to would be the second defect wearing the first one's
 * clothes.
 */
import { useCallback, useMemo } from "react";

import { newIdempotencyKey } from "@controlplane/generated-client";

import { useSurvivesSignIn } from "./survives-sign-in.ts";

interface Kept {
  /** The key, or "" when no request has been confirmed yet. */
  readonly key: string;
  /** The body the key was minted for. */
  readonly signature: string;
}

const NONE: Kept = { key: "", signature: "" };

function isKept(value: unknown): value is Kept {
  if (typeof value !== "object" || value === null) return false;
  const candidate = value as { key?: unknown; signature?: unknown };
  return typeof candidate.key === "string" && typeof candidate.signature === "string";
}

export interface IdempotencyKey {
  /**
   * The key to send with this body.
   *
   * The same signature returns the same key — that is a retry. A different
   * signature mints a new one — that is a different request, and sending the
   * old key with it is what makes the backend answer about the header.
   */
  readonly forRequest: (signature: string) => string;
  /** Forgets the key. Call it once the command has actually succeeded. */
  readonly clear: () => void;
}

/**
 * Holds one command's idempotency key across renders and across a sign-in.
 *
 * `stashKey` names the command and the thing it acts on — `"products.buy.<id>"`,
 * `"agents.action.<id>"` — so two products, or two agents, never share a key.
 */
export function useIdempotencyKey(stashKey: string): IdempotencyKey {
  const kept = useSurvivesSignIn<Kept>(stashKey, NONE, isKept);
  const { value, set, clear } = kept;

  const forRequest = useCallback(
    (signature: string): string => {
      if (value.key !== "" && value.signature === signature) return value.key;
      const minted = newIdempotencyKey();
      set({ key: minted, signature });
      return minted;
    },
    [value.key, value.signature, set],
  );

  return useMemo(() => ({ forRequest, clear }), [forRequest, clear]);
}

/**
 * A body reduced to the fields the backend compares, in a fixed order.
 *
 * Written as a helper rather than left to each caller so that "the same
 * request" is spelled the same way everywhere, and so a field added to a
 * command is one argument here rather than a string-concatenation somewhere in
 * a click handler. Every part is a string already: nothing on this path parses
 * a number.
 */
export function requestSignature(parts: ReadonlyArray<string | undefined>): string {
  return parts.map((part) => part ?? "").join("|");
}
