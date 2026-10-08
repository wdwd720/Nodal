/**
 * One confirmed command and its idempotency key.
 *
 * A command's key is created at the moment the person confirms it, not when
 * the form appears: until they confirm there is no action to identify. The
 * command is built once, at that moment, and kept with its key, so a retry
 * after a dropped connection resends exactly the same request under the same
 * key -- the backend replays its first answer instead of acting twice, and a
 * retry can never become a different request (which the backend would refuse
 * as a reused key). Both are dropped only when the person starts over.
 */
import { useState } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

export interface Confirmation<C> {
  /** The confirmed command, or undefined before confirmation. */
  readonly command: C | undefined;
  /** The key of the confirmed command, or undefined before confirmation. */
  readonly key: string | undefined;
  /**
   * The command to send: on the first confirmation a new key is created and
   * the command built with it; on a retry the stored command, unchanged.
   */
  readonly confirm: (build: (key: string) => C) => C;
  /** Forgets the command and its key: the next confirmation is a new action. */
  readonly reset: () => void;
}

export function useConfirmation<C>(): Confirmation<C> {
  const [held, setHeld] = useState<{ readonly key: string; readonly command: C } | undefined>(undefined);
  return {
    command: held?.command,
    key: held?.key,
    confirm: (build) => {
      if (held !== undefined) return held.command;
      const key = newIdempotencyKey();
      const command = build(key);
      setHeld({ key, command });
      return command;
    },
    reset: () => {
      setHeld(undefined);
    },
  };
}
