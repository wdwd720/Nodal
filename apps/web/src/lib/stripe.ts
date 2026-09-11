/**
 * Stripe.js, loaded from Stripe's own origin, and nothing else.
 *
 * Card details must never touch Nodal's origin. The Payment Element renders
 * inside an iframe served by Stripe, so the PAN is typed into Stripe's document
 * and this application never sees it, never holds it and never sends it
 * anywhere. That is the whole reason the library is loaded from
 * `https://js.stripe.com/v3/` rather than bundled: a bundled copy would be
 * running on this origin, and Stripe's own terms and every card-data rule turn
 * on which origin the field lives in.
 *
 * The content security policy the deployment serves (D-078, `render.yaml`)
 * admits exactly Stripe's hosts — `script-src https://js.stripe.com`,
 * `frame-src js.stripe.com hooks.stripe.com m.stripe.network`,
 * `connect-src https://api.stripe.com` — so nothing else can be pulled in
 * through this door even if something tried.
 *
 * Two rules shape the rest of the file:
 *
 *   1. A KEY THAT IS ABSENT OR MALFORMED IS A STATED REASON, NEVER A FORM.
 *      A publishable key is the one piece of configuration that decides whether
 *      a payment can be taken at all. If it is missing, or is not a publishable
 *      key at all (a secret key pasted into the wrong variable is the failure
 *      this is really guarding against), the page says so and shows no fields.
 *      Rendering a dead card form would take somebody's attention, their
 *      keystrokes, and eventually their trust, and give nothing back.
 *
 *   2. THE SCRIPT IS LOADED ONCE. The promise is memoised, so a page that
 *      mounts, unmounts and mounts again does not insert a second copy of the
 *      library — and a failure to load is reported rather than retried
 *      silently forever.
 */

/** What a publishable key must look like. A secret key does not match, deliberately. */
const PUBLISHABLE_KEY_PATTERN = /^pk_(test|live)_[A-Za-z0-9]+$/;

const SCRIPT_SRC = "https://js.stripe.com/v3/";

export type StripeMode = "test" | "live";

export type KeyReading =
  | { readonly ok: true; readonly key: string; readonly mode: StripeMode }
  | { readonly ok: false; readonly reason: string };

/**
 * Reads the configured publishable key, or says exactly why there is none.
 *
 * The reasons are different sentences because the remedies are different: an
 * unconfigured deployment is an operator's job, and a malformed value is a
 * mistake somebody can see in their own environment file.
 */
export function readPublishableKey(raw: string | undefined): KeyReading {
  if (raw === undefined || raw.trim() === "") {
    return {
      ok: false,
      reason:
        "This deployment has no payment provider key configured, so there is no way to take a payment here. " +
        "Nothing is wrong with your account.",
    };
  }
  const value = raw.trim();
  if (!PUBLISHABLE_KEY_PATTERN.test(value)) {
    return {
      ok: false,
      reason:
        "The payment provider key this deployment is configured with is not a publishable key, so the payment " +
        "form is not rendered. This is a deployment configuration fault, not a problem with your account.",
    };
  }
  return { ok: true, key: value, mode: value.startsWith("pk_test_") ? "test" : "live" };
}

/* --------------------------------------------------------------------------
 * The minimum of Stripe's surface this application uses.
 *
 * Declared structurally rather than pulled from `@stripe/stripe-js`, because
 * adding a dependency to describe a global that arrives over the network buys
 * nothing here: these four calls are the entire contact surface, and a type
 * that says so is easier to audit than one that says everything.
 * ------------------------------------------------------------------------ */

export interface StripeElement {
  mount: (target: HTMLElement) => void;
  unmount: () => void;
  destroy: () => void;
}

export interface StripeElements {
  create: (type: "payment", options?: Record<string, unknown>) => StripeElement;
  submit: () => Promise<{ error?: { message?: string; type?: string; code?: string } }>;
}

export interface StripeConfirmResult {
  error?: { message?: string; type?: string; code?: string; decline_code?: string };
  paymentIntent?: { status?: string };
}

export interface StripeInstance {
  elements: (options: Record<string, unknown>) => StripeElements;
  confirmPayment: (options: {
    elements: StripeElements;
    confirmParams?: Record<string, unknown>;
    redirect?: "if_required" | "always";
  }) => Promise<StripeConfirmResult>;
}

type StripeFactory = (key: string) => StripeInstance;

declare global {
  interface Window {
    Stripe?: StripeFactory;
  }
}

let pending: Promise<StripeFactory> | undefined;

/**
 * Loads Stripe.js, once.
 *
 * Rejects rather than resolving with a stub when the script cannot be fetched:
 * a payment surface that silently degrades is one that will take an amount from
 * somebody and do nothing with it.
 */
export function loadStripeJs(): Promise<StripeFactory> {
  if (pending !== undefined) return pending;

  pending = new Promise<StripeFactory>((resolve, reject) => {
    const existing = window.Stripe;
    if (existing !== undefined) {
      resolve(existing);
      return;
    }
    const script = document.createElement("script");
    script.src = SCRIPT_SRC;
    script.async = true;
    script.addEventListener("load", () => {
      const factory = window.Stripe;
      if (factory === undefined) {
        reject(new Error("Stripe.js loaded but did not define its entry point."));
        return;
      }
      resolve(factory);
    });
    script.addEventListener("error", () => {
      reject(new Error("Stripe.js could not be loaded from js.stripe.com."));
    });
    document.head.appendChild(script);
  });

  return pending;
}

/** Forgets the memoised load. Exists for tests; nothing in the app calls it. */
export function resetStripeLoaderForTest(): void {
  pending = undefined;
}
