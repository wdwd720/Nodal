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

/** The scopes `data.changed` names. `internal/notifications/follower.go`. */
type Scope = "balance" | "position" | "market" | "payout" | "account";

/**
 * Which cached reads a scope makes stale.
 *
 * Keyed by the prefix of the query keys in `api/queries.ts`. Over-invalidating
 * is always safe — the worst case is a refetch nobody needed — and under-
 * invalidating leaves a figure on screen that the backend has already moved
 * past, which is the failure this whole mechanism exists to prevent.
 */
const SCOPE_KEYS: Readonly<Record<Scope, readonly string[]>> = {
  balance: ["credits", "buying-power", "holdings", "activity"],
  position: ["holdings", "native-market", "native-asset", "activity"],
  market: ["native-markets", "native-market", "native-asset", "native-assets"],
  payout: ["payouts", "credits", "activity"],
  account: ["accounts", "me", "activity"],
};

function invalidate(client: QueryClient, prefixes: readonly string[]): void {
  for (const prefix of prefixes) {
    void client.invalidateQueries({ queryKey: [prefix] });
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
    // `withCredentials` is what carries the session.
    //
    // EventSource's credentials mode is "same-origin" unless this is set, and
    // the deployed topology is app-nodal.actorvia.xyz calling
    // api-nodal.actorvia.xyz — the same SITE, a different ORIGIN. So the
    // session cookie was not attached, `GET /v1/events/stream` (which requires
    // PermAccountRead) refused it, and the bell, the activity feed and every
    // stream-driven invalidation were dead on the deployed tier while the badge
    // said "reconnecting" forever. Nothing else in the app was affected: the
    // generated client sets `credentials: "include"` and `probeReady`
    // deliberately omits them, so this was the one request that broke.
    //
    // The browser suite could not catch it either — vite.config.ts proxies /v1,
    // so those tests run same-origin, where the default already sends the
    // cookie. `src/lib/source-scan.test.ts` holds this option in place instead.
    const source = new EventSource(`${API_BASE}/events/stream`, { withCredentials: true });

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
        case "buying_power.changed":
          invalidate(queryClient, ["buying-power", "holdings", "credits"]);
          break;
        case "order.transitioned":
          invalidate(queryClient, ["orders", "intents"]);
          break;
        case "intent.transitioned":
          invalidate(queryClient, ["intents", "intent"]);
          break;
        case "deposit.transitioned":
          invalidate(queryClient, ["deposits"]);
          break;
        case "agent.state":
          invalidate(queryClient, ["agents", "intents"]);
          break;
        case "notification.created":
          // The count and the list, and nothing else: the notification's own
          // body is already in the event and is not a figure.
          void queryClient.invalidateQueries({ queryKey: ["me", "notifications"] });
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
      void queryClient.invalidateQueries({ queryKey: ["activity"] });
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
