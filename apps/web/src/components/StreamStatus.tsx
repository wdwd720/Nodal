/**
 * The live stream, and its honest place in the hierarchy of truth.
 *
 * Realtime updates are never authoritative. This subscribes to the SSE endpoint
 * and uses it only as a hint that canonical REST state has moved on — every
 * event, and every `resync`, results in a refetch rather than in a figure being
 * patched into the cache. That is why a dropped connection is a cosmetic
 * problem here and not a correctness one, and why the badge says what the
 * connection is doing instead of pretending it is always healthy.
 *
 * # Resume is the browser's, which is why this effect must not churn
 *
 * The server sends an `id:` with every event and accepts `Last-Event-ID` on
 * reconnect (`internal/stream`), replaying what was missed and sending `resync`
 * when it cannot. `EventSource` sends that header **itself**, from the last id
 * it saw — a page cannot set it, because `EventSource` takes no headers.
 *
 * The consequence shapes this file: a NEW `EventSource` starts with no
 * `Last-Event-ID` and therefore resumes nothing. So the effect depends only on
 * `enabled` and the query client, both stable for the life of the shell, and
 * the connection is left alone to reconnect on its own. Adding a dependency
 * that changes per render here would silently turn resume off, and nothing
 * would look broken.
 *
 * # What an event is allowed to carry
 *
 * Identifiers and state names. `notification.created` carries a title and a
 * kind because a bell needs them and neither is financial truth;
 * `data.changed` carries a scope and a reference and no values at all. Nothing
 * on this path ever becomes a figure on screen without a REST read in between.
 */
import { useEffect, useState, type ReactNode } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";

import { API_BASE } from "../api/client.ts";
import {
  agentKeys,
  keys,
  marketKeys,
  meKeys,
  nodalKeys,
  portfolioKeys,
  withdrawKeys,
} from "../api/queries.ts";
import { Pill } from "./Layout.tsx";

export type StreamState = "connecting" | "open" | "reconnecting";

export interface StreamStatusValue {
  readonly state: StreamState;
  /** Events received on this connection. Zero is a fact, not a failure. */
  readonly received: number;
  readonly lastEventAt: string | undefined;
  /**
   * True once the connection has dropped at least once in this session.
   *
   * The badge says so even after it recovers, because a customer who has been
   * disconnected may have missed something the replay could not reach, and
   * "connected" alone would imply an unbroken record.
   */
  readonly reconnected: boolean;
}

/**
 * The scopes `data.changed` names. `internal/notifications/follower.go` is the
 * authority; all eight of its constants are here, because a scope this build
 * does not know about falls through to invalidating the entire cache.
 */
type Scope =
  | "balance"
  | "position"
  | "market"
  | "payout"
  | "account"
  | "verification"
  | "eligibility"
  | "agent";

/**
 * A query key's leading, constant segments — the prefix an invalidation matches.
 *
 * TanStack Query matches a `queryKey` by prefix, so invalidating `["me",
 * "portfolio"]` reaches `["me", "portfolio", accountId]` and invalidating
 * `["me"]` reaches every one of the nine keys that start with it.
 *
 * The prefixes below are TAKEN FROM THE KEY FACTORIES rather than written out
 * again (D-112). That is the whole fix for the defect the audit found here: the
 * previous map was a list of hand-copied strings, five of which — "buying-
 * power", "holdings", "activity", "native-asset", "native-assets" — named reads
 * this application stopped making when D-077 removed the hosted rail, while the
 * two keys a fill actually changes, `["me","portfolio",…]` and
 * `["me","activity",…]`, were in no scope at all. A signal arrived, five
 * invalidations matched nothing, and the position the customer was looking at
 * did not refresh. Renaming a key in `queries.ts` now changes this map with it;
 * deleting one stops it compiling.
 */
function prefixOf(key: ReadonlyArray<string | number | boolean>, depth: number): readonly string[] {
  return key.slice(0, depth).map((segment) => String(segment));
}

const creditsPrefix = prefixOf(nodalKeys.credits(""), 1);
const portfolioPrefix = prefixOf(portfolioKeys.portfolio(""), 2);
const meActivityPrefix = prefixOf(portfolioKeys.activity("", "", ""), 2);
const eligibilityPrefix = prefixOf(withdrawKeys.eligibility(""), 2);
const verificationPrefix = prefixOf(withdrawKeys.verification(""), 2);
const destinationsPrefix = prefixOf(withdrawKeys.destinations(""), 2);
const payoutsPrefix = prefixOf(nodalKeys.payouts(""), 1);
const payoutPrefix = prefixOf(agentKeys.payout(""), 1);
const marketPrefix = prefixOf(marketKeys.summary(""), 1);
const marketsPrefix = prefixOf(portfolioKeys.markets("", 0), 1);
const productsPrefix = prefixOf(nodalKeys.products(""), 1);
const internalOrdersPrefix = prefixOf(nodalKeys.orders("", ""), 1);
const agentPrefix = prefixOf(agentKeys.agent(""), 1);
const agentsPrefix = prefixOf(agentKeys.agents(""), 1);
const accountsPrefix = keys.accounts;
const mePrefix = keys.me;
const myAccountPrefix = meKeys.myAccount;

/**
 * Which cached reads a scope makes stale.
 *
 * Over-invalidating is always safe — the worst case is a refetch nobody needed
 * — and under-invalidating leaves a figure on screen that the backend has
 * already moved past, which is the failure this whole mechanism exists to
 * prevent. So a balance signal reaches eligibility as well as the balance:
 * `frozen` and `payout_eligible` are computed from the same lots.
 */
const SCOPE_KEYS: Readonly<Record<Scope, ReadonlyArray<readonly string[]>>> = {
  balance: [creditsPrefix, portfolioPrefix, meActivityPrefix, eligibilityPrefix, internalOrdersPrefix],
  position: [portfolioPrefix, meActivityPrefix, marketPrefix, marketsPrefix],
  market: [marketsPrefix, marketPrefix, productsPrefix],
  payout: [payoutsPrefix, payoutPrefix, creditsPrefix, eligibilityPrefix, meActivityPrefix],
  account: [accountsPrefix, mePrefix, meActivityPrefix],
  verification: [verificationPrefix, eligibilityPrefix, myAccountPrefix],
  eligibility: [eligibilityPrefix, destinationsPrefix, creditsPrefix],
  agent: [agentPrefix, agentsPrefix, meActivityPrefix],
};

function invalidate(client: QueryClient, prefixes: ReadonlyArray<readonly string[]>): void {
  for (const prefix of prefixes) {
    void client.invalidateQueries({ queryKey: prefix });
  }
}

/**
 * The same invalidation a signal would have caused, for a page that just caused
 * the change itself.
 *
 * A command's own success is better evidence than an event: it is certain, and
 * it arrives whether or not a connection is up. What a page must NOT do is
 * decide for itself which reads a fill made stale — that is the map above, and
 * a second hand-written copy of it beside a mutation is how the map came to be
 * wrong in the first place while every screen still looked correct.
 */
export function invalidateScopes(client: QueryClient, ...scopes: readonly Scope[]): void {
  for (const scope of scopes) {
    invalidate(client, SCOPE_KEYS[scope]);
  }
}

/** Reads a string field off a parsed payload without trusting any of it. */
function stringField(value: unknown, field: string): string {
  if (typeof value !== "object" || value === null) return "";
  const candidate = (value as Record<string, unknown>)[field];
  return typeof candidate === "string" ? candidate : "";
}

/**
 * Subscribes for as long as the component is mounted. Every message invalidates
 * the queries it could affect; nothing is written into the cache from the
 * stream itself.
 */
export function useEventStream(enabled: boolean): StreamStatusValue {
  const queryClient = useQueryClient();
  const [state, setState] = useState<StreamState>("connecting");
  const [received, setReceived] = useState(0);
  const [lastEventAt, setLastEventAt] = useState<string | undefined>(undefined);
  const [reconnected, setReconnected] = useState(false);

  useEffect(() => {
    if (!enabled) return;
    const source = new EventSource(`${API_BASE}/events/stream`);

    const onOpen = (): void => {
      setState((previous) => {
        // Reaching `open` from `reconnecting` means the connection dropped and
        // came back. The badge keeps saying so.
        if (previous === "reconnecting") setReconnected(true);
        return "open";
      });
    };
    const onError = (): void => {
      // EventSource retries on its own, carrying Last-Event-ID. Say that rather
      // than claiming failure, and do not close the source: closing it would
      // throw away the cursor that makes the retry a resume.
      setState("reconnecting");
    };
    const onMessage = (event: MessageEvent<string>): void => {
      setReceived((count) => count + 1);
      setLastEventAt(new Date().toISOString());
      // The payload is a hint. Canonical state is refetched, never patched.
      let parsed: unknown = undefined;
      try {
        parsed = JSON.parse(event.data);
      } catch {
        parsed = undefined;
      }
      const type = stringField(parsed, "type");
      const data: unknown = typeof parsed === "object" && parsed !== null ? (parsed as { data?: unknown }).data : undefined;

      switch (type) {
        // The typed events the outbox relay publishes. `agent.state` and the
        // credit-affecting ones are the only two shapes this product still
        // emits; the hosted rail's `order.transitioned`,
        // `intent.transitioned` and `deposit.transitioned` went with the pages
        // that read them (D-077), so they are no longer named here — an
        // unrecognised type already refetches everything, which is the right
        // answer to an event this build does not understand.
        case "buying_power.changed":
          invalidate(queryClient, SCOPE_KEYS.balance);
          break;
        case "agent.state":
          invalidate(queryClient, SCOPE_KEYS.agent);
          break;
        case "notification.created":
          // The count and the list, and nothing else: the notification's own
          // body is already in the event and is not a figure.
          void queryClient.invalidateQueries({ queryKey: meKeys.notifications });
          break;
        case "data.changed": {
          const scope = stringField(data, "scope");
          const keys = SCOPE_KEYS[scope as Scope];
          if (keys === undefined) {
            // A scope this build does not know about. Refetching everything is
            // the safe answer to "something you care about changed and I
            // cannot tell you what".
            void queryClient.invalidateQueries();
            break;
          }
          invalidate(queryClient, keys);
          break;
        }
        default:
          // `resync` and anything unrecognised: drop every cached read and
          // let the server re-answer. Refetching too much is always safe.
          void queryClient.invalidateQueries();
          break;
      }
      // Anything at all happened, so the feed that records everything is stale.
      // This used to invalidate `["activity"]`, which is the hosted rail's key
      // and which no page has read since D-077.
      void queryClient.invalidateQueries({ queryKey: meActivityPrefix });
    };

    source.addEventListener("open", onOpen);
    source.addEventListener("error", onError);
    source.addEventListener("message", onMessage);
    return () => {
      source.removeEventListener("open", onOpen);
      source.removeEventListener("error", onError);
      source.removeEventListener("message", onMessage);
      source.close();
    };
  }, [enabled, queryClient]);

  return { state, received, lastEventAt, reconnected };
}

export function StreamBadge(props: { readonly status: StreamStatusValue }): ReactNode {
  const { state, received, reconnected } = props.status;
  const tone = state === "open" ? (reconnected ? "info" : "good") : state === "connecting" ? "neutral" : "warn";
  const label =
    state === "open"
      ? reconnected
        ? `stream reconnected · ${String(received)} events`
        : received === 0
          ? "stream connected · no events yet"
          : `stream connected · ${String(received)} events`
      : state === "connecting"
        ? "stream connecting"
        : "stream reconnecting";
  return (
    <Pill
      tone={tone}
      title={
        (reconnected
          ? "This connection dropped and came back. The browser resumed it from the last event it saw, and anything it could not replay was answered by a full refetch. "
          : "") +
        "Realtime updates are advisory. Every figure on this page is refetched from the REST API, " +
        "which is the authoritative source; the stream only says when to ask again."
      }
    >
      {label}
    </Pill>
  );
}
