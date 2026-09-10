/**
 * A transient notice, for the small set of things that are genuinely transient.
 *
 * WHAT A TOAST MAY CARRY: a preference was saved, a link was copied, an export
 * has started, a filter was cleared. Interface bookkeeping.
 *
 * WHAT A TOAST MAY NEVER CARRY, and this is the whole reason the component is
 * written down rather than reached for:
 *
 *   - A REFUSAL. A refusal is not ephemeral. It stays until the customer acts
 *     on it, it names the rule and what would change the answer, and it is a
 *     `Refusal` rendered in place. A refusal that disappears after four seconds
 *     is a refusal the customer never read.
 *   - A FILL, or any other financial event. What happened to somebody's money
 *     belongs in the record — the panel, the table, the activity timeline —
 *     not in a message that deletes itself. A customer must be able to come
 *     back to it tomorrow.
 *   - A CELEBRATION. A trade is not a slot machine pull. There is no scatter of
 *     coloured paper, no sound on a fill, no streak counter, and no toast that
 *     congratulates.
 *
 * The region is `aria-live="polite"` and announces once. It is not a landmark
 * and it never steals focus: a message that moves focus interrupts whatever the
 * reader was doing, which for a transient notice is never worth it.
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import { IconButton } from "./Button.tsx";

export type ToastTone = "neutral" | "good" | "info";

interface Toast {
  readonly id: number;
  readonly text: string;
  readonly tone: ToastTone;
}

interface ToastApi {
  /** Show a non-financial transient notice. */
  readonly notify: (text: string, tone?: ToastTone) => void;
}

const ToastContext = createContext<ToastApi | undefined>(undefined);

const LIFETIME_MS = 6000;

export function ToastProvider(props: { readonly children: ReactNode }): ReactNode {
  const [toasts, setToasts] = useState<readonly Toast[]>([]);
  const nextId = useRef(0);
  const timers = useRef<number[]>([]);

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id));
  }, []);

  const notify = useCallback(
    (text: string, tone: ToastTone = "neutral") => {
      const id = nextId.current;
      nextId.current = id + 1;
      setToasts((current) => [...current, { id, text, tone }]);
      const timer = window.setTimeout(() => {
        dismiss(id);
      }, LIFETIME_MS);
      timers.current.push(timer);
    },
    [dismiss],
  );

  useEffect(() => {
    const pending = timers.current;
    return () => {
      for (const timer of pending) window.clearTimeout(timer);
    };
  }, []);

  const api = useMemo<ToastApi>(() => ({ notify }), [notify]);

  return (
    <ToastContext.Provider value={api}>
      {props.children}
      <div className="toast-region" role="status" aria-live="polite">
        {toasts.map((toast) => (
          <div key={toast.id} className={`toast toast-${toast.tone}`}>
            <span className="toast-text">{toast.text}</span>
            <IconButton
              label="Dismiss this notice"
              onClick={() => {
                dismiss(toast.id);
              }}
            >
              <span aria-hidden="true">✕</span>
            </IconButton>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

/**
 * The notifier. Throws outside a provider rather than silently doing nothing,
 * because a notice that never appears is indistinguishable from one that did.
 */
export function useToast(): ToastApi {
  const api = useContext(ToastContext);
  if (api === undefined) {
    throw new Error("useToast was called outside a ToastProvider");
  }
  return api;
}
