/**
 * The card fields, which are not on this origin.
 *
 * The Payment Element renders inside an iframe served by Stripe. The customer
 * types their card number into Stripe's document, this application never sees
 * it, and nothing here reads from or writes to those fields — the only handle
 * this file has on them is an opaque object it passes back to
 * `stripe.confirmPayment`.
 *
 * Everything else in the component is about not lying:
 *
 *   - the element is mounted against the `client_secret` the backend handed
 *     back exactly once, and it is never put in a query cache;
 *   - `redirect: "if_required"` means a card that needs 3-D Secure gets its
 *     challenge and a card that does not is not sent on a pointless round
 *     trip. Either way the answer comes back here;
 *   - a provider failure is reported by CATEGORY and by the provider's stable
 *     code, never by its message. A provider exception is written for an
 *     engineer, usually names something the customer cannot act on, and is
 *     occasionally a sentence about our own configuration;
 *   - and, critically, a successful confirmation is NOT a captured payment.
 *     Stripe saying "succeeded" means the customer's bank agreed. Credits are
 *     minted when the webhook reaches the backend and the ledger posts. So
 *     this component reports what the provider said and hands back; the page
 *     above it asks the backend.
 */
import { useEffect, useRef, useState, type ReactNode } from "react";

import { Button } from "../../components/Button.tsx";
import { Explanation } from "../../components/DataState.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import {
  loadStripeJs,
  type StripeElements,
  type StripeInstance,
} from "../../lib/stripe.ts";

/** What the provider said, in this application's words. */
export type ProviderOutcome =
  | { readonly kind: "succeeded" }
  | { readonly kind: "requires-action" }
  | { readonly kind: "processing" }
  | { readonly kind: "failed"; readonly category: string; readonly code: string };

/**
 * The categories Stripe reports, each turned into a sentence a customer can do
 * something with. The provider's own message is deliberately not among them.
 */
const CATEGORY_COPY: Readonly<Record<string, string>> = {
  card_error:
    "The card was declined by the bank that issued it. Nodal was not told why, and cannot be. " +
    "Another card, or the bank itself, is the only way forward.",
  validation_error:
    "The card details were incomplete or did not pass the provider's own checks. Nothing was charged.",
  invalid_request_error:
    "The payment provider refused the request as invalid. Nothing was charged. This is a fault on our " +
    "side rather than something to correct on the card.",
  api_error:
    "The payment provider had an internal failure. Nothing was charged. Trying again in a moment is " +
    "reasonable; the amount has not moved.",
  api_connection_error:
    "The browser could not reach the payment provider. Nothing was charged.",
  rate_limit_error: "The payment provider is rate limiting this session. Nothing was charged.",
  authentication_error:
    "The payment provider rejected this deployment's credentials. Nothing was charged, and nothing " +
    "you do changes it.",
};

export function categorySentence(category: string): string {
  return (
    CATEGORY_COPY[category] ??
    "The payment provider refused this and reported a category this application does not have a " +
      "sentence for. Nothing was charged. The category and code are shown so support can act on them."
  );
}

export function PaymentForm(props: {
  /** Handed back by `POST /v1/payments` on creation, once, and never cached. */
  readonly clientSecret: string;
  readonly publishableKey: string;
  /** What the button says it is paying, already formatted by the caller. */
  readonly payLabel: string;
  readonly onOutcome: (outcome: ProviderOutcome) => void;
}): ReactNode {
  const holder = useRef<HTMLDivElement | null>(null);
  const [stripe, setStripe] = useState<StripeInstance | undefined>(undefined);
  const [elements, setElements] = useState<StripeElements | undefined>(undefined);
  const [loadError, setLoadError] = useState<unknown>(undefined);
  const [busy, setBusy] = useState(false);

  const { clientSecret, publishableKey } = props;

  useEffect(() => {
    let live = true;
    let mounted: { destroy: () => void } | undefined;

    loadStripeJs().then(
      (factory) => {
        if (!live) return;
        const instance = factory(publishableKey);
        const set = instance.elements({ clientSecret });
        const element = set.create("payment");
        const target = holder.current;
        if (target === null) return;
        element.mount(target);
        mounted = element;
        setStripe(instance);
        setElements(set);
      },
      (error: unknown) => {
        if (live) setLoadError(error);
      },
    );

    return () => {
      live = false;
      if (mounted !== undefined) mounted.destroy();
    };
  }, [clientSecret, publishableKey]);

  if (loadError !== undefined) {
    return (
      <Explanation error={loadError}>
        <p className="explain-body">
          The payment fields are served by the provider, from the provider&rsquo;s own origin. Without
          them there is no way to take a card here, and nothing has been charged.
        </p>
      </Explanation>
    );
  }

  const ready = stripe !== undefined && elements !== undefined;

  return (
    <div className="stack">
      {/* Stripe's iframe lands here. Nothing in this application reads it. */}
      <div ref={holder} />
      {!ready && <Skeleton shape="block" label="The payment provider's card fields are loading" />}
      <div className="form-actions">
        <Button
          variant="primary"
          busy={busy}
          busyLabel="Asking the provider…"
          {...(ready
            ? {
                onClick: () => {
                  if (stripe === undefined || elements === undefined) return;
                  setBusy(true);
                  stripe
                    .confirmPayment({ elements, redirect: "if_required" })
                    .then(
                      (result) => {
                        setBusy(false);
                        if (result.error !== undefined) {
                          props.onOutcome({
                            kind: "failed",
                            category: result.error.type ?? "unknown",
                            code: result.error.code ?? result.error.decline_code ?? "none",
                          });
                          return;
                        }
                        const status = result.paymentIntent?.status ?? "";
                        if (status === "succeeded") {
                          props.onOutcome({ kind: "succeeded" });
                          return;
                        }
                        if (status === "processing") {
                          props.onOutcome({ kind: "processing" });
                          return;
                        }
                        props.onOutcome({ kind: "requires-action" });
                      },
                      () => {
                        setBusy(false);
                        props.onOutcome({
                          kind: "failed",
                          category: "api_connection_error",
                          code: "none",
                        });
                      },
                    );
                },
              }
            : { disabledReason: "The provider's card fields have not finished loading." })}
        >
          {props.payLabel}
        </Button>
      </div>
    </div>
  );
}
