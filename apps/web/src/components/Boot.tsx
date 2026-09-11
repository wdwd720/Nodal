/**
 * The screen a customer sees while the API is starting.
 *
 * The launch tier runs the API on a free instance that spins down when idle
 * and takes about a minute to come back. The first request after that is not
 * a failure and must not look like one: it is the service starting. This
 * component says so, asks the API once a second whether it is ready, and hands
 * control back the moment it is. Past the ceiling it stops guessing and shows
 * what the last attempt actually said, with a retry -- an interface that keeps
 * saying "starting" forever would be lying about a real outage.
 */
import { useEffect, useState, type ReactNode } from "react";

import { probeReady } from "../api/client.ts";
import { Explanation } from "./DataState.tsx";

/** How long to keep asking before admitting the API is not coming up. */
const CEILING_SECONDS = 90;
const INTERVAL_MS = 1000;

export function Boot(props: { readonly error: unknown; readonly onReady: () => void }): ReactNode {
  const [elapsed, setElapsed] = useState(0);
  const [gaveUp, setGaveUp] = useState(false);
  const { onReady } = props;

  useEffect(() => {
    let cancelled = false;
    let seconds = 0;
    const tick = async (): Promise<void> => {
      if (cancelled) return;
      if (await probeReady()) {
        if (!cancelled) onReady();
        return;
      }
      seconds += 1;
      if (cancelled) return;
      setElapsed(seconds);
      if (seconds >= CEILING_SECONDS) {
        setGaveUp(true);
        return;
      }
      timer = setTimeout(() => void tick(), INTERVAL_MS);
    };
    let timer = setTimeout(() => void tick(), INTERVAL_MS);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [onReady]);

  if (gaveUp) {
    return (
      <div className="boot">
        <Explanation error={props.error} onRetry={onReady}>
          <p>
            Nodal did not answer within {String(CEILING_SECONDS)} seconds. This is a failure to reach
            the service, not a statement about your session: nothing has signed you out. Trying again
            is safe.
          </p>
        </Explanation>
      </div>
    );
  }

  return (
    <div className="boot" role="status" aria-live="polite">
      <p className="boot-title">Starting Nodal…</p>
      <p className="boot-body">
        The service is waking up. This usually takes under a minute, and nothing you do here is
        lost while it does.
      </p>
      <p className="boot-meta mono-small">waiting {String(elapsed)}s</p>
    </div>
  );
}
