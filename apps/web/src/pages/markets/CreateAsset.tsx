/**
 * CREATE ASSET (product goal §53's flow, restyled onto the primitives).
 *
 * The behaviour is unchanged from the screen it replaces, because the shape was
 * the point: name, symbol, description, economics preview, creator allocation,
 * market mechanics, fees, risk warning, moderation — with one requirement above
 * the rest:
 *
 *   Show exact immutable economics before launch.
 *
 * So it is deliberately two steps rather than one form with a submit button.
 * Step one collects. Step two shows the creator the numbers that will become
 * permanent, in exact base units AND at the asset's own precision, and requires
 * them to confirm THAT — not the form they filled in a minute ago. The
 * confirmation mints its idempotency key at the moment of confirming, so a
 * double-click cannot publish two assets.
 *
 * What changed in the restyle: every figure goes through `Figure` — the one
 * formatter, now that the earlier one is deleted — the inputs are `FormField`
 * so an error is tied to its control
 * with `aria-describedby` rather than sitting in a list above the form, and a
 * refused creation is rendered as a refusal rather than as a fault.
 *
 * What this page must never do:
 *
 *   - suggest a price, a return, or that anybody will buy the thing;
 *   - round or reformat the supply figures between the preview and the request,
 *     because the preview is the thing being agreed to;
 *   - present moderation as a formality. A draft is created; publication is a
 *     separate decision made by somebody else.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import { useCreateNativeAsset, type NativeAsset } from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { EmptyState } from "../../components/DataState.tsx";
import { FormField } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import {
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  Page,
  Panel,
  Pill,
} from "../../components/Layout.tsx";
import {
  CREATE_ASSET_IMMUTABILITY,
  NATIVE_ASSET_RISK,
  NATIVE_PRICE_NOTE,
} from "../../lib/honesty.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { useActiveAccountId } from "../../session.tsx";
import { TradeRefusal } from "./TradeRefusal.tsx";
import "../../styles/markets.css";

/**
 * Nodal-native assets are created at six decimal places, like Credits.
 *
 * It is fixed here rather than offered as a field because it is the one number
 * on this form a creator cannot revise and cannot evaluate: a wrong precision
 * is not visible until somebody tries to buy a fraction of a unit, and by then
 * the asset is permanent. The screens that READ an asset never assume this —
 * every one of them renders at the `asset_decimals` the response carries.
 */
const ASSET_DECIMALS = 6;

interface Draft {
  readonly name: string;
  readonly symbol: string;
  readonly description: string;
  readonly maxSupply: string;
  readonly creatorAllocation: string;
}

const EMPTY: Draft = {
  name: "",
  symbol: "",
  description: "",
  maxSupply: "",
  creatorAllocation: "",
};

/** Per-field problems, so each one is announced with its own control. */
interface Problems {
  readonly name: string;
  readonly symbol: string;
  readonly maxSupply: string;
  readonly creatorAllocation: string;
}

const DIGITS = /^[0-9]+$/;

/**
 * What the creator must fix before the numbers can be shown back to them.
 *
 * This is not the authority — the backend validates and screens, and refuses
 * for reasons this page cannot know. It exists so the review step is never
 * reached with a blank where a permanent number belongs.
 */
function problems(draft: Draft): Problems {
  const supplyOk = DIGITS.test(draft.maxSupply) && draft.maxSupply !== "0";
  const allocationOk = DIGITS.test(draft.creatorAllocation) || draft.creatorAllocation === "";
  let allocation = "";
  if (!allocationOk) {
    allocation = "Your allocation must be a whole number of base units, or left empty for none.";
  } else if (supplyOk && DIGITS.test(draft.creatorAllocation)) {
    if (BigInt(draft.creatorAllocation) > BigInt(draft.maxSupply)) {
      allocation = "Your allocation cannot be larger than the total supply.";
    }
  }
  return {
    name: draft.name.trim().length < 2 ? "A name needs at least two characters." : "",
    symbol: /^[A-Z0-9]{2,10}$/.test(draft.symbol)
      ? ""
      : "A symbol is 2–10 characters, upper-case letters and digits only.",
    maxSupply: supplyOk ? "" : "Total supply must be a positive whole number of base units.",
    creatorAllocation: allocation,
  };
}

function clean(found: Problems): boolean {
  return (
    found.name === "" &&
    found.symbol === "" &&
    found.maxSupply === "" &&
    found.creatorAllocation === ""
  );
}

export function CreateAsset(): ReactNode {
  const accountId = useActiveAccountId();
  /**
   * The draft survives a sign-in.
   *
   * This form is the longest one in the product and a session can expire at any
   * keystroke in it. `useSurvivesSignIn` keeps what was typed across the
   * full-page trip to the identity provider and puts it back on return, which
   * is what USER_JOURNEY §10 promises and what §11's "anything you had typed is
   * kept" says out loud to the customer.
   */
  const kept = useSurvivesSignIn<Draft>("create-asset.draft", EMPTY);
  const draft = kept.value;
  const [reviewing, setReviewing] = useState(false);
  const [touched, setTouched] = useState(false);
  const create = useCreateNativeAsset();

  if (accountId === undefined) {
    return (
      <Page title="Create asset">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is nothing to create an asset under."
        />
      </Page>
    );
  }

  if (create.isSuccess) {
    return <Created asset={create.data} />;
  }

  const found = problems(draft);

  return (
    <Page
      title="Create asset"
      lead="Publishing an asset fixes its economics permanently. Read the numbers before you confirm them."
    >
      {reviewing ? (
        <Review
          draft={draft}
          onBack={() => {
            setReviewing(false);
          }}
          onConfirm={() => {
            create.mutate(
              {
                accountId,
                name: draft.name.trim(),
                symbol: draft.symbol,
                description: draft.description.trim(),
                maxSupply: draft.maxSupply,
                creatorAllocation: draft.creatorAllocation === "" ? "0" : draft.creatorAllocation,
                decimals: ASSET_DECIMALS,
                // Minted at the moment of confirmation, never on render: a
                // retry of the same confirmation must never make a second
                // asset.
                idempotencyKey: newIdempotencyKey(),
              },
              // Only a draft that was actually created is forgotten. A failed
              // attempt keeps everything, because the customer is about to try
              // again with it.
              {
                onSuccess: () => {
                  kept.clear();
                },
              },
            );
          }}
          pending={create.isPending}
          error={create.isError ? create.error : undefined}
          onReset={() => {
            create.reset();
          }}
        />
      ) : (
        <Form
          draft={draft}
          onChange={(next) => {
            setTouched(true);
            kept.set(next);
          }}
          problems={found}
          showProblems={touched}
          onReview={() => {
            setReviewing(true);
          }}
        />
      )}
    </Page>
  );
}

function Form(props: {
  readonly draft: Draft;
  readonly onChange: (draft: Draft) => void;
  readonly problems: Problems;
  readonly showProblems: boolean;
  readonly onReview: () => void;
}): ReactNode {
  const { draft, onChange } = props;
  const set = (patch: Partial<Draft>): void => {
    onChange({ ...draft, ...patch });
  };
  const show = (message: string): string => (props.showProblems ? message : "");

  return (
    <Panel
      title="What you are making"
      description="Nothing here is submitted until you have seen the exact economics on the next step."
    >
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          if (clean(props.problems)) props.onReview();
        }}
      >
        <FormField label="Name" error={show(props.problems.name)}>
          {(field) => (
            <input
              {...field}
              value={draft.name}
              maxLength={64}
              onChange={(event) => {
                set({ name: event.target.value });
              }}
            />
          )}
        </FormField>

        <FormField
          label="Symbol"
          hint="Upper-case letters and digits, 2 to 10 characters. It cannot be changed afterwards."
          error={show(props.problems.symbol)}
        >
          {(field) => (
            <input
              {...field}
              value={draft.symbol}
              maxLength={10}
              onChange={(event) => {
                set({ symbol: event.target.value.toUpperCase() });
              }}
            />
          )}
        </FormField>

        <FormField label="Description" hint="What it is, in your own words. Published as written.">
          {(field) => (
            <textarea
              {...field}
              value={draft.description}
              maxLength={2000}
              rows={4}
              onChange={(event) => {
                set({ description: event.target.value });
              }}
            />
          )}
        </FormField>

        <FormField
          label="Total supply, in base units"
          hint={`This asset holds ${String(ASSET_DECIMALS)} decimal places, so one whole unit is a 1 followed by ${String(ASSET_DECIMALS)} zeros. The exact figure is what becomes permanent, so it is entered exactly.`}
          error={show(props.problems.maxSupply)}
        >
          {(field) => (
            <input
              {...field}
              value={draft.maxSupply}
              inputMode="numeric"
              autoComplete="off"
              onChange={(event) => {
                set({ maxSupply: event.target.value });
              }}
            />
          )}
        </FormField>

        <FormField
          label="Your allocation, in base units"
          hint="What you keep at launch. Leave it empty to keep none. Everyone can see this figure."
          error={show(props.problems.creatorAllocation)}
        >
          {(field) => (
            <input
              {...field}
              value={draft.creatorAllocation}
              inputMode="numeric"
              autoComplete="off"
              onChange={(event) => {
                set({ creatorAllocation: event.target.value });
              }}
            />
          )}
        </FormField>

        {clean(props.problems) ? (
          <Button variant="primary" submit>
            Review the economics
          </Button>
        ) : (
          <Button
            variant="primary"
            disabledReason="Fix the problems shown beside the fields above before reviewing the economics."
          >
            Review the economics
          </Button>
        )}
      </form>

      <Disclosure title="What you are about to create">
        <p>{NATIVE_ASSET_RISK}</p>
        <p>{NATIVE_PRICE_NOTE}</p>
      </Disclosure>
    </Panel>
  );
}

/**
 * The step that matters. Every number the creator is agreeing to, in the exact
 * units the backend will store, with what each one means.
 */
function Review(props: {
  readonly draft: Draft;
  readonly onBack: () => void;
  readonly onConfirm: () => void;
  readonly pending: boolean;
  readonly error: unknown;
  readonly onReset: () => void;
}): ReactNode {
  const { draft } = props;
  const allocation = draft.creatorAllocation === "" ? "0" : draft.creatorAllocation;
  // Exact integer arithmetic on base units: what the creator keeps is taken out
  // of what will ever exist, and the remainder is what the curve has to sell.
  // Both operands were validated as digit strings before this step was reached.
  const toMarket = String(BigInt(draft.maxSupply) - BigInt(allocation));

  return (
    <Panel
      title="These numbers become permanent"
      description="Read them. After publication neither you nor Nodal can change any of them."
    >
      <FieldGrid columns={2}>
        <Field label="Name">{draft.name.trim()}</Field>
        <Field label="Symbol">
          <Pill tone="info">{draft.symbol}</Pill>
        </Field>
        <Field label="Total supply" note="Every unit that will ever exist." emphasis>
          <Figure
            kind="units"
            value={{ base: draft.maxSupply, scale: ASSET_DECIMALS }}
            symbol={draft.symbol}
          />
          <p className="field-note mono-small">{draft.maxSupply} base units</p>
        </Field>
        <Field
          label="Your allocation"
          note="Yours at launch, at no cost. Everyone else buys from the pool, and they can see this figure."
          emphasis
        >
          <Figure
            kind="units"
            value={{ base: allocation, scale: ASSET_DECIMALS }}
            symbol={draft.symbol}
          />
          <p className="field-note mono-small">{allocation} base units</p>
        </Field>
        <Field label="Sold by the market" note="What the pricing formula has to sell.">
          <Figure
            kind="units"
            value={{ base: toMarket, scale: ASSET_DECIMALS }}
            symbol={draft.symbol}
          />
        </Field>
        <Field label="Decimal places" note="Fixed at creation and never revised.">
          <Figure kind="count" count={ASSET_DECIMALS} />
        </Field>
      </FieldGrid>

      {draft.description.trim() !== "" && (
        <>
          <p className="field-note">Description, as it will be published:</p>
          <p>{draft.description.trim()}</p>
        </>
      )}

      <Disclosure title="How the market will work">
        <p>
          Buyers pay Credits into a shared pool and the price moves along a fixed formula: each
          purchase raises it, each sale lowers it. Nobody quotes a price to you and nobody is
          obliged to buy your allocation. The pool is the only counterparty.
        </p>
        <p>{NATIVE_PRICE_NOTE}</p>
      </Disclosure>

      <Disclosure title="What cannot be changed afterwards">
        <p>{CREATE_ASSET_IMMUTABILITY}</p>
      </Disclosure>

      <Disclosure title="What publishing means">
        <p>
          This creates a DRAFT and submits it for moderation. It does not start trading. Somebody
          else decides whether it is published, and a market becomes active only after that — so
          creating this is not a promise from Nodal that anyone will ever see it.
        </p>
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>

      {props.error !== undefined && (
        <TradeRefusal what="This asset" error={props.error} onRetry={props.onReset} />
      )}

      <div className="form-actions">
        <Button variant="secondary" onClick={props.onBack}>
          Change something
        </Button>
        <Button
          variant="primary"
          busy={props.pending}
          busyLabel="Creating…"
          onClick={props.onConfirm}
        >
          Create this asset with exactly these economics
        </Button>
      </div>
    </Panel>
  );
}

function Created(props: { readonly asset: NativeAsset }): ReactNode {
  const { asset } = props;
  // The create response does not carry the asset's precision, so these are
  // rendered at the precision this form just sent. Every screen that READS an
  // asset later gets `asset_decimals` from its own response and never assumes
  // this constant.
  const decimals = ASSET_DECIMALS;
  return (
    <Page title="Create asset" lead="Created as a draft and submitted for moderation.">
      <Panel title={asset.name} description="What the backend recorded.">
        <FieldGrid columns={2}>
          <Field label="Symbol">
            <Pill tone="info">{asset.symbol}</Pill>
          </Field>
          <Field label="Status" note="A draft does not trade. Moderation decides what happens next.">
            <Pill tone="neutral">{asset.status}</Pill>
          </Field>
          <Field label="Moderation">{asset.moderation_state}</Field>
          <Field label="Asset id">
            <Identifier value={asset.asset_id} label="asset" />
          </Field>
          <Field label="Total supply">
            <Figure
              kind="units"
              value={{ base: asset.supply.max_supply, scale: decimals }}
              symbol={asset.symbol}
            />
          </Field>
          <Field label="Your allocation">
            <Figure
              kind="units"
              value={{ base: asset.supply.creator_allocation, scale: decimals }}
              symbol={asset.symbol}
            />
          </Field>
          <Field label="Sold by the market" note="The backend's own figure, not this page's subtraction.">
            <Figure
              kind="units"
              value={{ base: asset.supply.pool_supply, scale: decimals }}
              symbol={asset.symbol}
            />
          </Field>
          <Field label="Decimal places" note="Fixed at creation, exactly as it was confirmed.">
            <Figure kind="count" count={decimals} />
          </Field>
        </FieldGrid>

        <Disclosure title="What happens now">
          <p>
            Nothing, until a moderator looks at it. Nodal is not reviewing this as an investment and
            has made no judgement about it beyond whether it may be published.
          </p>
          <p>{NATIVE_ASSET_RISK}</p>
        </Disclosure>

        <div className="form-actions">
          <LinkButton to="/markets" variant="secondary">
            Back to markets
          </LinkButton>
        </div>
      </Panel>
    </Page>
  );
}
