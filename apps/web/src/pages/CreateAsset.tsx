/**
 * CREATE ASSET (gola.md PART LIII).
 *
 * The flow the goal asks for — name, symbol, description, image, economics
 * preview, creator allocation, market mechanics, fees, risk warning,
 * moderation, activate — with one requirement above the rest:
 *
 *   Show exact immutable economics before launch.
 *
 * So this is deliberately two steps rather than one form with a submit button.
 * Step one collects. Step two shows the creator the numbers that will become
 * permanent, in exact base units, and requires them to confirm THAT — not the
 * form they filled in a minute ago. The confirmation carries an idempotency
 * key created at the moment of confirming, so a double-click cannot publish
 * two assets.
 *
 * What this page must never do:
 *
 *   - suggest a price, a return, or that anybody will buy the thing;
 *   - round or reformat the supply figures between the preview and the
 *     request, because the preview is the thing being agreed to;
 *   - present moderation as a formality. A draft is created; publication is a
 *     separate decision made by somebody else.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import { useCreateNativeAsset, type NativeAsset } from "../api/queries.ts";
import { EmptyState, Explanation } from "../components/DataState.tsx";
import { Button } from "../components/Button.tsx";
import { Disclosure, Field, FieldGrid, Identifier, Page, Panel, Pill } from "../components/Layout.tsx";
import { BaseUnits, Qty } from "../components/Money.tsx";
import { CREATE_ASSET_IMMUTABILITY, NATIVE_ASSET_RISK, NATIVE_PRICE_NOTE } from "../lib/honesty.ts";
import { useActiveAccountId } from "../session.tsx";

/** Nodal-native assets are held at six decimal places, like Credits. */
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

/**
 * What the creator must fix before the numbers can be shown back to them.
 *
 * This is not the authority — the backend validates and screens, and refuses
 * for reasons this page cannot know. It exists so the review step is never
 * reached with a blank where a permanent number belongs.
 */
function problems(draft: Draft): readonly string[] {
  const out: string[] = [];
  const digits = /^[0-9]+$/;
  if (draft.name.trim().length < 2) out.push("A name needs at least two characters.");
  if (!/^[A-Z0-9]{2,10}$/.test(draft.symbol)) {
    out.push("A symbol is 2–10 characters, upper case letters and digits only.");
  }
  if (!digits.test(draft.maxSupply) || draft.maxSupply === "0") {
    out.push("Total supply must be a positive whole number of base units.");
  }
  if (!digits.test(draft.creatorAllocation)) {
    out.push("Your allocation must be a whole number of base units, or zero.");
  }
  if (
    digits.test(draft.maxSupply) &&
    digits.test(draft.creatorAllocation) &&
    BigInt(draft.creatorAllocation) > BigInt(draft.maxSupply)
  ) {
    out.push("Your allocation cannot be larger than the total supply.");
  }
  return out;
}

export function CreateAsset(): ReactNode {
  const accountId = useActiveAccountId();
  const [draft, setDraft] = useState<Draft>(EMPTY);
  const [reviewing, setReviewing] = useState(false);
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
          onBack={() => setReviewing(false)}
          onConfirm={() => {
            create.mutate({
              accountId,
              name: draft.name.trim(),
              symbol: draft.symbol,
              description: draft.description.trim(),
              maxSupply: draft.maxSupply,
              creatorAllocation: draft.creatorAllocation === "" ? "0" : draft.creatorAllocation,
              decimals: ASSET_DECIMALS,
              idempotencyKey: newIdempotencyKey(),
            });
          }}
          pending={create.isPending}
          error={create.isError ? create.error : undefined}
          onReset={() => create.reset()}
        />
      ) : (
        <Form draft={draft} onChange={setDraft} problems={found} onReview={() => setReviewing(true)} />
      )}
    </Page>
  );
}

function Form(props: {
  readonly draft: Draft;
  readonly onChange: (d: Draft) => void;
  readonly problems: readonly string[];
  readonly onReview: () => void;
}): ReactNode {
  const { draft, onChange } = props;
  const set = (patch: Partial<Draft>): void => onChange({ ...draft, ...patch });

  return (
    <Panel
      title="What you are making"
      description="Nothing here is submitted until you have seen the exact economics on the next step."
    >
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          if (props.problems.length === 0) props.onReview();
        }}
      >
        <label className="inline-field">
          <span>Name</span>
          <input value={draft.name} maxLength={64} onChange={(e) => set({ name: e.target.value })} />
        </label>
        <label className="inline-field">
          <span>Symbol</span>
          <input
            value={draft.symbol}
            maxLength={10}
            onChange={(e) => set({ symbol: e.target.value.toUpperCase() })}
          />
        </label>
        <label className="inline-field">
          <span>Description</span>
          <textarea
            value={draft.description}
            maxLength={2000}
            rows={4}
            onChange={(e) => set({ description: e.target.value })}
          />
        </label>
        <label className="inline-field">
          <span>Total supply, in base units</span>
          <input
            value={draft.maxSupply}
            inputMode="numeric"
            onChange={(e) => set({ maxSupply: e.target.value })}
          />
        </label>
        <label className="inline-field">
          <span>Your allocation, in base units</span>
          <input
            value={draft.creatorAllocation}
            inputMode="numeric"
            onChange={(e) => set({ creatorAllocation: e.target.value })}
          />
        </label>
        <p className="field-note">
          Base units, not whole tokens: this asset has {ASSET_DECIMALS} decimal places, so a supply of
          one whole token is written as a one followed by {ASSET_DECIMALS} zeros. The exact figure is
          what becomes permanent, so it is entered exactly.
        </p>

        {props.problems.length > 0 && (
          <ul className="reasons">
            {props.problems.map((problem) => (
              <li key={problem}>{problem}</li>
            ))}
          </ul>
        )}

        {props.problems.length > 0 ? (
          <Button variant="primary" disabledReason="Fix the points above before reviewing the economics.">
            Review the economics
          </Button>
        ) : (
          <Button variant="primary" submit>
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
 * units the backend will store, with what each one means and what it will cost
 * them to be wrong.
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
  const toMarket = (BigInt(draft.maxSupply) - BigInt(allocation)).toString();

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
          <Qty value={draft.maxSupply} decimals={ASSET_DECIMALS} symbol={draft.symbol} />
          <p className="field-note">
            <BaseUnits value={draft.maxSupply} />
          </p>
        </Field>
        <Field
          label="Your allocation"
          note="Yours at launch. Everyone else buys from the pool, and they can see this figure."
          emphasis
        >
          <Qty value={allocation} decimals={ASSET_DECIMALS} symbol={draft.symbol} />
          <p className="field-note">
            <BaseUnits value={allocation} />
          </p>
        </Field>
        <Field label="Sold by the market" note="What the pricing formula has to sell.">
          <Qty value={toMarket} decimals={ASSET_DECIMALS} symbol={draft.symbol} />
        </Field>
        <Field label="Decimal places" note="Fixed at creation.">
          {String(ASSET_DECIMALS)}
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
          purchase raises it, each sale lowers it. Nobody quotes a price to you and nobody is obliged
          to buy your allocation. The pool is the only counterparty.
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

      {props.error !== undefined && <Explanation error={props.error} onRetry={props.onReset} />}

      <div className="panel-actions">
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
            <Identifier value={asset.asset_id} />
          </Field>
          <Field label="Total supply">
            <Qty value={asset.supply.max_supply} decimals={ASSET_DECIMALS} symbol={asset.symbol} />
          </Field>
          <Field label="Your allocation">
            <Qty value={asset.supply.creator_allocation} decimals={ASSET_DECIMALS} symbol={asset.symbol} />
          </Field>
        </FieldGrid>
        <Disclosure title="What happens now">
          <p>
            Nothing, until a moderator looks at it. Nodal is not reviewing this as an investment and
            has made no judgement about it beyond whether it may be published.
          </p>
          <p>{NATIVE_ASSET_RISK}</p>
        </Disclosure>
      </Panel>
    </Page>
  );
}
