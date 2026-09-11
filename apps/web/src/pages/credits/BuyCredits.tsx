/**
 * BUY CREDITS (goal §9, USER_JOURNEY §4, Scenario B).
 *
 * The path is: choose an amount → the server prices it and starts a payment →
 * the provider takes the card on its own origin → a webhook captures it → the
 * ledger issues Credits → the balance moves. This page owns the first and third
 * steps and reports honestly on the rest.
 *
 * Four decisions worth writing down, because each of them is a place where the
 * obvious implementation would have been a lie.
 *
 * # The exact quantity comes from the server, before the card is touched
 *
 * `GET /v1/credits/pricing` gives the rate and the bounds, but not the whole
 * formula: `minor_units_per_major_unit` and the rounding mode are not in the
 * response, so a Credit quantity computed in this browser would be a guess that
 * happens to be right today. `internal/credit/pricing.go` is the only thing in
 * the system that decides how many Credits an amount buys, and it says so.
 *
 * So the page shows the RATE while the amount is being chosen, and shows the
 * EXACT QUANTITY the server decided as soon as `POST /v1/payments` answers —
 * which is before the customer has entered a card, because a PaymentIntent is
 * not a charge. "USD payment → Credits received" is therefore on screen, exact,
 * with no money moved, exactly as §9 asks.
 *
 * # A key that is absent or malformed is a stated reason, never a form
 *
 * Without a usable publishable key there is no way to take a card here at all,
 * so the page says that and shows no amount chooser either. A flow that lets
 * somebody pick $50 and then tells them the deployment cannot take payments has
 * wasted their attention to no end.
 *
 * # The provider saying yes is not the balance moving
 *
 * Stripe reporting `succeeded` means the issuing bank agreed. Credits exist
 * when the webhook reaches the backend and the ledger posts. The page therefore
 * polls `GET /v1/payments/{id}` for the authoritative state and says "Balance
 * updating" until the backend agrees. It never adds the purchase to a displayed
 * balance itself.
 *
 * # The idempotency key is minted at confirm, and survives a sign-in
 *
 * It is created when the customer presses the button that starts a payment,
 * never on render, and it is written through `useSurvivesSignIn` with the
 * amount — so a session that expires mid-purchase comes back to the same
 * amount and the same key, and the retry cannot make a second payment.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import { explain } from "../../api/problem.ts";
import {
  useCreditPricing,
  useCreditPurchase,
  useStartCreditPurchase,
  type CreditPricing,
  type CreditPurchase,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { EmptyState, Explanation } from "../../components/DataState.tsx";
import { Field, FieldGrid, FormField } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Disclosure, Page, Panel } from "../../components/Layout.tsx";
import { Refusal } from "../../components/Refusal.tsx";
import { Refused } from "../../components/Refused.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import {
  CREDIT_DECIMALS,
  PRESET_AMOUNTS_MINOR,
  minorToUsd,
  outOfBounds,
  usdToMinor,
} from "../../lib/credits.ts";
import { CREDITS_DISCLOSURE, PROVENANCE_NOTE } from "../../lib/honesty.ts";
import { parseUsdAmountInput } from "../../lib/money.ts";
import { readPublishableKey } from "../../lib/stripe.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { useActiveAccountId } from "../../session.tsx";
import { PaymentForm, categorySentence, type ProviderOutcome } from "./PaymentForm.tsx";

/** The sentence goal §9 requires wherever an amount of Credits is being bought. */
const INTERNAL_VALUE_SENTENCE =
  "Credits are internal platform value and aren't directly withdrawable. Buying them moves money to " +
  "a payment provider and creates a balance that is usable inside Nodal.";

/** How often the page asks the backend whether the webhook has arrived. */
const POLL_MS = 2000;

/** States after which nothing more will happen without somebody doing something. */
const TERMINAL_STATES: readonly string[] = [
  "CAPTURED",
  "REVERSIBLE",
  "SETTLED",
  "REVERSED",
  "REFUNDED",
  "DISPUTED",
  "FAILED",
  "CANCELED",
  "MANUAL_REVIEW",
];

/** What each purchase state means, said once, in the customer's terms. */
const STATE_COPY: Readonly<Record<string, string>> = {
  CREATED: "The payment has been created with the provider and no card has been presented yet.",
  AUTHORIZATION_PENDING: "The provider is asking the card's bank to authorise the amount.",
  AUTHORIZED: "The bank authorised the amount. It has not been taken yet.",
  CAPTURE_PENDING: "The provider is taking the authorised amount.",
  CAPTURED:
    "The provider has taken the payment and the Credits have been issued. They are reversible " +
    "while the card dispute window is open, which is why they are not payout-eligible yet.",
  REVERSIBLE:
    "The Credits exist and can be spent inside Nodal. They stay reversible until the card dispute " +
    "window closes, so they are not payout-eligible yet.",
  SETTLED: "The dispute window has closed. These Credits are settled.",
  REVERSED: "The payment was reversed after it was taken, and the Credits it created were removed.",
  REFUNDED: "This payment was refunded, and the Credits it created were removed.",
  DISPUTED:
    "The cardholder's bank has opened a dispute on this payment. The Credits it created are frozen " +
    "until the dispute is decided.",
  FAILED: "The payment did not complete. Nothing was charged and no Credits were created.",
  CANCELED: "This payment was cancelled before it completed. Nothing was charged.",
  MANUAL_REVIEW: "This payment is being reviewed by an operator before anything else happens.",
};

/** Purchase-specific refusals, where the shared sentence would name the wrong thing. */
function purchaseRefusal(error: unknown): ReactNode {
  const detail = explain(error);
  const known: Readonly<Record<string, { readonly what: string; readonly rule: string; readonly remedy: string }>> = {
    CAPABILITY_NOT_APPROVED: {
      what: "Buying Credits is not open in this deployment",
      rule:
        "The CREDIT_PURCHASE capability gate has not been approved and activated. The gate is " +
        "dual-controlled and ships closed, so the path is refused for everybody, not for you.",
      remedy: "Nothing you can do changes this. It is a governance decision about the product.",
    },
    UNSUPPORTED: {
      what: "This deployment does not sell Credits",
      rule: "The API this application talks to exposes no active Credit purchase path.",
      remedy: "Nothing you can do changes this. It is a decision about the deployment.",
    },
    AT_CAPACITY: {
      what: "Buying Credits is paused for capacity",
      rule:
        "The capacity guard refused this before anything was attempted. It bounds how much new " +
        "value enters the system at once, and it refuses everybody equally while it is engaged.",
      remedy: "Try again later. Nothing was charged and nothing was reserved.",
    },
    ELIGIBILITY_JURISDICTION: {
      what: "Buying Credits is not available where this account is",
      rule:
        "The legal router refused this for the jurisdiction recorded on the account. The refusal is " +
        "made before any payment is attempted.",
      remedy: "Nothing was charged. The restriction is shown on the account page.",
    },
  };
  const entry = known[detail.code];
  if (entry === undefined) return null;
  return (
    <Refusal
      what={entry.what}
      rule={entry.rule}
      code={detail.code}
      remedy={entry.remedy}
      {...(detail.requestId === undefined ? {} : { correlationId: detail.requestId })}
    >
      <p className="refusal-body">{detail.body}</p>
      {detail.code === "ELIGIBILITY_JURISDICTION" && (
        <div className="form-actions">
          <LinkButton to="/settings/account" variant="secondary">
            See the restriction
          </LinkButton>
        </div>
      )}
    </Refusal>
  );
}

/** The sandbox label, rendered only when the API said so. */
function SandboxNote(props: { readonly sandbox: boolean | undefined }): ReactNode {
  if (props.sandbox !== true) return null;
  return (
    <Disclosure title="This is a sandbox payment">
      <p>
        The payment provider this deployment is configured with is not a live one. The card is a test
        card, no money moves, and the Credits this creates are sandbox value that describes nothing
        real.
      </p>
    </Disclosure>
  );
}

export function BuyCredits(): ReactNode {
  const accountId = useActiveAccountId();
  const pricing = useCreditPricing();
  const start = useStartCreditPurchase();

  // Both survive a sign-in round trip: the amount so the customer does not
  // retype it, and the key so a retry after re-authentication is the same
  // request rather than a second payment.
  const typed = useSurvivesSignIn<string>("buy-credits.amount", "");
  const key = useSurvivesSignIn<string>("buy-credits.key", "");

  const [purchase, setPurchase] = useState<CreditPurchase | undefined>(undefined);
  const [outcome, setOutcome] = useState<ProviderOutcome | undefined>(undefined);

  const reading = readPublishableKey(import.meta.env["VITE_STRIPE_PUBLISHABLE_KEY"]);

  // The authoritative state, asked for once a purchase exists and polled only
  // while something is still expected to happen to it.
  const settled = purchase !== undefined && TERMINAL_STATES.includes(purchase.state);
  const state = useCreditPurchase(
    purchase?.purchase_id,
    outcome === undefined || settled ? {} : { refetchMs: POLL_MS },
  );
  const current: CreditPurchase | undefined = state.data ?? purchase;

  if (accountId === undefined) {
    return (
      <Page title="Buy Credits">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is nothing to buy Credits into. That is the backend's answer, not a loading state."
        />
      </Page>
    );
  }

  if (!reading.ok) {
    return (
      <Page
        title="Buy Credits"
        lead="Credits are internal platform value, bought with money through an approved payment provider."
      >
        <Panel title="Payments unavailable">
          <EmptyState title="No payment can be taken here" body={reading.reason} />
          <p className="field-note">
            No amount is offered and no card fields are rendered, because neither would lead
            anywhere. Nothing is wrong with your account and no balance has changed.
          </p>
        </Panel>
        <Disclosure title="What Credits are">
          <p>{INTERNAL_VALUE_SENTENCE}</p>
          <p>{CREDITS_DISCLOSURE}</p>
        </Disclosure>
      </Page>
    );
  }

  const publishableKey = reading.key;

  return (
    <Page
      title="Buy Credits"
      lead="Credits are internal platform value, bought with money through an approved payment provider."
    >
      {/* The five states, written out rather than delegated, because the error
          one is special here. On a deployment with no credit-purchase provider
          the pricing port is unwired and this route answers UNSUPPORTED, which
          is a statement about the PRODUCT — "this deployment does not sell
          Credits" — and deserves that sentence rather than the generic "this
          response could not be used". */}
      {pricing.isPending && (
        <Panel title="Buying Credits">
          <Skeleton shape="text" count={3} label="The pricing policy is loading" />
        </Panel>
      )}

      {pricing.isError && (
        <Panel title="Buying Credits">
          {purchaseRefusal(pricing.error) ?? (
            <Refused
              what="Buying Credits"
              error={pricing.error}
              onRetry={() => {
                void pricing.refetch();
              }}
            />
          )}
          <p className="field-note">
            No amount is offered and no card fields are rendered, because the server has not said
            what an amount buys. Nothing is wrong with your account and no balance has changed.
          </p>
        </Panel>
      )}

      {pricing.data !== undefined &&
        (current === undefined ? (
          <ChooseAmount
            policy={pricing.data}
            typed={typed}
            busy={start.isPending}
            error={start.error}
            onRetry={() => {
              start.reset();
            }}
            onConfirm={(amountMinor) => {
              // The key is minted HERE — at the moment of confirmation — and
              // kept, so every retry of this same confirmation reuses it.
              // Never on render.
              const existing = key.value === "" ? newIdempotencyKey() : key.value;
              key.set(existing);
              start.mutate(
                {
                  accountId,
                  amountMinor,
                  currency: (pricing.data as CreditPricing).currency,
                  idempotencyKey: existing,
                },
                {
                  onSuccess: (created) => {
                    setPurchase(created);
                  },
                },
              );
            }}
          />
        ) : (
          <TakePayment
            purchase={current}
            clientSecret={purchase?.client_secret}
            publishableKey={publishableKey}
            outcome={outcome}
            stateQuery={state}
            onOutcome={setOutcome}
            onStartOver={() => {
              setPurchase(undefined);
              setOutcome(undefined);
              key.set("");
              typed.clear();
              start.reset();
            }}
          />
        ))}

      <SandboxNote sandbox={current?.sandbox} />

      <Disclosure title="What Credits are">
        <p>{INTERNAL_VALUE_SENTENCE}</p>
        <p>{CREDITS_DISCLOSURE}</p>
        <p>{PROVENANCE_NOTE}</p>
      </Disclosure>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

interface Typed {
  readonly value: string;
  readonly set: (next: string) => void;
  readonly clear: () => void;
}

function ChooseAmount(props: {
  readonly policy: CreditPricing;
  readonly typed: Typed;
  readonly busy: boolean;
  readonly error: unknown;
  readonly onRetry: () => void;
  readonly onConfirm: (amountMinor: number) => void;
}): ReactNode {
  const { policy, typed } = props;
  const bounds = { minMinor: policy.min_amount_minor, maxMinor: policy.max_amount_minor };
  const presets = PRESET_AMOUNTS_MINOR.filter(
    (minor) => minor >= bounds.minMinor && minor <= bounds.maxMinor,
  );

  const parsed = typed.value === "" ? undefined : parseUsdAmountInput(typed.value);
  const minor = parsed !== undefined && parsed.ok ? usdToMinor(parsed.value) : undefined;
  const boundsError = minor === undefined ? "" : outOfBounds(minor, bounds);
  const fieldError = parsed === undefined ? "" : parsed.ok ? boundsError : parsed.error;

  const refusal = props.error === undefined || props.error === null ? null : purchaseRefusal(props.error);

  return (
    <Panel
      title="How much"
      description="The amount is money. What it buys is decided by the server, under the pricing policy named below."
      temp="economy"
    >
      <FieldGrid columns={3}>
        <Field label="Rate" note={`Pricing policy ${policy.version}`}>
          <Figure kind="count" count={policy.credits_per_major_unit} symbol="Credits" />
          <p className="field-note">
            per 1 {policy.currency}. The exact quantity your amount buys is computed by the server
            and shown, in full, before any card is entered.
          </p>
        </Field>
        <Field label="Smallest purchase">
          <Figure
            kind="money"
            value={{ decimal: minorToUsd(bounds.minMinor) }}
            symbol={policy.currency}
          />
        </Field>
        <Field label="Largest single purchase">
          <Figure
            kind="money"
            value={{ decimal: minorToUsd(bounds.maxMinor) }}
            symbol={policy.currency}
          />
        </Field>
      </FieldGrid>

      <div className="form-actions">
        {presets.map((preset) => (
          <Button
            key={preset}
            variant={minor === preset ? "primary" : "secondary"}
            onClick={() => {
              typed.set(minorToUsd(preset));
            }}
          >
            {minorToUsd(preset)} {policy.currency}
          </Button>
        ))}
      </div>

      <FormField
        label={`Amount in ${policy.currency}`}
        hint={`Between ${minorToUsd(bounds.minMinor)} and ${minorToUsd(bounds.maxMinor)}.`}
        error={fieldError}
      >
        {(field) => (
          <input
            className="input"
            inputMode="decimal"
            autoComplete="off"
            value={typed.value}
            onChange={(event) => {
              typed.set(event.target.value);
            }}
            {...field}
          />
        )}
      </FormField>

      <p className="field-note">{INTERNAL_VALUE_SENTENCE}</p>

      {refusal}
      {props.error !== undefined && props.error !== null && refusal === null && (
        <Refused what="Buying Credits" error={props.error} onRetry={props.onRetry} />
      )}

      <div className="form-actions">
        <Button
          variant="primary"
          busy={props.busy}
          busyLabel="Asking the server to price this…"
          {...(minor !== undefined && fieldError === ""
            ? {
                onClick: () => {
                  props.onConfirm(minor);
                },
              }
            : {
                disabledReason:
                  "Enter an amount inside the bounds above. Nothing is sent until it is a real amount.",
              })}
        >
          Continue to payment
        </Button>
      </div>
    </Panel>
  );
}

/* -------------------------------------------------------------------------- */

function TakePayment(props: {
  readonly purchase: CreditPurchase;
  /**
   * The provider secret, handed back exactly once on creation. It lives in the
   * mutation result and in nothing else — never in a query cache, because a
   * cached secret is a second browser resuming somebody else's payment.
   */
  readonly clientSecret: string | undefined;
  readonly publishableKey: string;
  readonly outcome: ProviderOutcome | undefined;
  readonly stateQuery: { readonly isError: boolean; readonly error: unknown; readonly refetch: () => unknown };
  readonly onOutcome: (outcome: ProviderOutcome) => void;
  readonly onStartOver: () => void;
}): ReactNode {
  const { purchase, outcome } = props;
  const sandbox = purchase.sandbox === true;
  const stateCopy =
    STATE_COPY[purchase.state] ??
    "The backend reported a state this page has no sentence for. It is shown exactly as the backend gave it.";
  const credited =
    purchase.state === "CAPTURED" || purchase.state === "REVERSIBLE" || purchase.state === "SETTLED";

  return (
    <Panel
      title="Confirm and pay"
      description="This is what the server priced. Nothing has been charged yet."
      temp={sandbox ? "simulated" : "economy"}
      actions={<StatusBadge tone={credited ? "good" : "neutral"}>{purchase.state}</StatusBadge>}
    >
      <FieldGrid columns={2}>
        <Field label={`${purchase.currency} payment`} note="What leaves the card.">
          <Figure
            kind="money"
            value={{ decimal: minorToUsd(purchase.amount_minor) }}
            symbol={purchase.currency}
            big
          />
        </Field>
        <Field
          label="Credits received"
          note={`Computed by the server under pricing policy ${purchase.pricing_version}.`}
        >
          <Figure
            kind="units"
            value={{ base: purchase.credit_quantity, scale: CREDIT_DECIMALS }}
            symbol="Credits"
          />
        </Field>
      </FieldGrid>

      <p className="field-note">{stateCopy}</p>
      <p className="field-note">{INTERNAL_VALUE_SENTENCE}</p>

      {props.stateQuery.isError && (
        <Explanation
          error={props.stateQuery.error}
          onRetry={() => {
            void props.stateQuery.refetch();
          }}
        >
          <p className="explain-body">
            The payment itself is unaffected by this page failing to read it. Whatever the provider
            and the webhook have done has been done.
          </p>
        </Explanation>
      )}

      <Outcome
        purchase={purchase}
        outcome={outcome}
        clientSecret={props.clientSecret}
        publishableKey={props.publishableKey}
        onOutcome={props.onOutcome}
      />

      <div className="form-actions">
        <Button variant="quiet" onClick={props.onStartOver}>
          Start a different purchase
        </Button>
        <LinkButton to="/home" variant="secondary">
          Back to Home
        </LinkButton>
      </div>
    </Panel>
  );
}

function Outcome(props: {
  readonly purchase: CreditPurchase;
  readonly outcome: ProviderOutcome | undefined;
  readonly clientSecret: string | undefined;
  readonly publishableKey: string;
  readonly onOutcome: (outcome: ProviderOutcome) => void;
}): ReactNode {
  const { purchase, outcome } = props;

  // The backend's own word beats anything the browser saw. A captured purchase
  // is captured whatever the provider said a second ago.
  if (purchase.state === "CAPTURED" || purchase.state === "REVERSIBLE" || purchase.state === "SETTLED") {
    return (
      <EmptyState
        title="The Credits have been issued"
        body={
          "The webhook reached the backend, the ledger posted, and the balance on Home reflects it. " +
          "Reversible Credits can be spent inside Nodal but are not payout-eligible until the card " +
          "dispute window closes."
        }
        action={
          <LinkButton to="/home" variant="primary">
            See the balance
          </LinkButton>
        }
      />
    );
  }

  if (purchase.state === "FAILED" || purchase.state === "CANCELED") {
    return (
      <Refusal
        what="The payment did not complete"
        rule={
          purchase.failure_reason === undefined || purchase.failure_reason === ""
            ? "The backend recorded this payment as finished without a capture. No Credits were created."
            : `The backend recorded this payment as finished without a capture, with reason ${purchase.failure_reason}. No Credits were created.`
        }
        code={purchase.state}
        remedy="Nothing was charged. Start a different purchase to try again."
      />
    );
  }

  if (purchase.state === "REVERSED" || purchase.state === "REFUNDED" || purchase.state === "DISPUTED") {
    return (
      <Refusal
        what="This payment was reversed"
        rule={
          "The provider reversed, refunded or disputed this payment after it was taken. The Credits " +
          "it created were removed or frozen by the same ledger entry that created them."
        }
        code={purchase.state}
        remedy="Your activity and notifications carry the record. Nothing here can undo a provider's decision."
      />
    );
  }

  if (outcome?.kind === "failed") {
    return (
      <Refusal
        what="The payment provider refused the card"
        rule={categorySentence(outcome.category)}
        code={`${outcome.category}/${outcome.code}`}
        remedy="Nothing was charged and no Credits were created. Another card is the next step."
      />
    );
  }

  if (outcome?.kind === "requires-action") {
    return (
      <EmptyState
        title="Your bank is asking you to confirm"
        body="The card's bank requires an extra confirmation step. Complete it in the provider's window; this page follows the outcome."
      />
    );
  }

  if (outcome?.kind === "succeeded" || outcome?.kind === "processing") {
    return (
      <EmptyState
        title="Balance updating"
        body={
          "The provider accepted the payment. Credits exist once the webhook reaches the backend and " +
          "the ledger posts, which is what this page is waiting for. Nothing is charged twice if you wait."
        }
      />
    );
  }

  if (props.clientSecret === undefined || props.clientSecret === "") {
    return (
      <EmptyState
        title="This payment is already in progress"
        body={
          "The backend answered with the existing payment rather than creating a second one, and a " +
          "provider secret is handed back exactly once. The state above is the authoritative answer; " +
          "it updates as the provider reports."
        }
      />
    );
  }

  return (
    <PaymentForm
      clientSecret={props.clientSecret}
      publishableKey={props.publishableKey}
      payLabel={`Pay ${minorToUsd(purchase.amount_minor)} ${purchase.currency}`}
      onOutcome={props.onOutcome}
    />
  );
}
