/**
 * The live stream, and its honest place in the hierarchy of truth.
 *
 * PART 109: realtime updates are never authoritative. This subscribes to the
 * SSE endpoint and uses it only as a hint that canonical REST state has moved
 * on — every event, and every `resync`, results in a refetch rather than in a
 * figure being patched into the cache. That is why a dropped connection is a
 * cosmetic problem here and not a correctness one, and why the badge says what
 * the connection is doing instead of pretending it is always healthy.
 *
 * In this deployment the event bus is not wired, so the stream carries
 * heartbeat comments and no events. The badge says exactly that rather than
 * implying a quiet market.
 */
import { useEffect, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { API_BASE } from "../api/client.ts";
import { Pill } from "./Layout.tsx";

export type StreamState = "connecting" | "open" | "reconnecting";

export interface StreamStatusValue {
  readonly state: StreamState;
  /** Events received on this connection. Zero is a fact, not a failure. */
  readonly received: number;
  readonly lastEventAt: string | undefined;
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

  useEffect(() => {
    if (!enabled) return;
    const source = new EventSource(`${API_BASE}/events/stream`);

    const onOpen = (): void => {
      setState("open");
    };
    const onError = (): void => {
      // EventSource retries on its own; say so rather than claiming failure.
      setState("reconnecting");
    };
    const onMessage = (event: MessageEvent<string>): void => {
      setReceived((count) => count + 1);
      setLastEventAt(new Date().toISOString());
      // The payload is a hint. Canonical state is refetched, never patched.
      let type = "";
      try {
        const parsed: unknown = JSON.parse(event.data);
        if (typeof parsed === "object" && parsed !== null && "type" in parsed) {
          const candidate = (parsed as { type?: unknown }).type;
          type = typeof candidate === "string" ? candidate : "";
        }
      } catch {
        type = "";
      }
      switch (type) {
        case "buying_power.changed":
          void queryClient.invalidateQueries({ queryKey: ["buying-power"] });
          void queryClient.invalidateQueries({ queryKey: ["holdings"] });
          break;
        case "order.transitioned":
          void queryClient.invalidateQueries({ queryKey: ["orders"] });
          void queryClient.invalidateQueries({ queryKey: ["intents"] });
          break;
        case "intent.transitioned":
          void queryClient.invalidateQueries({ queryKey: ["intents"] });
          void queryClient.invalidateQueries({ queryKey: ["intent"] });
          break;
        case "deposit.transitioned":
          void queryClient.invalidateQueries({ queryKey: ["deposits"] });
          break;
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

  return { state, received, lastEventAt };
}

export function StreamBadge(props: { readonly status: StreamStatusValue }): ReactNode {
  const { state, received } = props.status;
  const tone = state === "open" ? "good" : state === "connecting" ? "neutral" : "warn";
  const label =
    state === "open"
      ? received === 0
        ? "stream connected · no events yet"
        : `stream connected · ${String(received)} events`
      : state === "connecting"
        ? "stream connecting"
        : "stream reconnecting";
  return (
    <Pill
      tone={tone}
      title={
        "Realtime updates are advisory. Every figure on this page is refetched from the REST API, " +
        "which is the authoritative source; the stream only says when to ask again."
      }
    >
      {label}
    </Pill>
  );
}
