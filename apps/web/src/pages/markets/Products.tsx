/**
 * THE INTERNAL PRODUCT MARKETPLACE (product goal §17; D-077's `/markets/products`).
 *
 * The other half of "things other people made, priced in Credits". Its logic is
 * unchanged from the screen it replaces; what changed is that every figure now
 * goes through `Figure` — the one formatter the application has — the listings sit on
 * `PanelCard`, the two order histories are `DataTable`, and a refused purchase
 * is rendered as a refusal rather than as a fault.
 *
 * What a customer needs to understand before they buy, and what this page
 * therefore refuses to hide:
 *
 *   - a price is in Credits and is never converted to a currency;
 *   - buying sends Credits to another user, and the platform takes a share that
 *     is shown as its own figure rather than folded into the price;
 *   - the provenance a SALE produces is shown on the seller's side, because a
 *     seller is entitled to know which category their revenue lands in before
 *     they list — it decides whether a payout policy can ever reach it.
 *
 * The purchase sends the price the customer was shown. If the backend disagrees
 * the purchase is refused, and the refusal is rendered as one; this page never
 * retries at a different price.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useInternalOrders,
  useInternalProducts,
  usePurchaseProduct,
  type InternalOrder,
  type InternalProduct,
} from "../../api/queries.ts";
import { Button } from "../../components/Button.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { FormField } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import {
  Disclosure,
  Field,
  FieldGrid,
  IdentifierShort,
  Page,
  Panel,
  PanelCard,
  Pill,
} from "../../components/Layout.tsx";
import { CREDITS_DISCLOSURE, PROVENANCE_NOTE } from "../../lib/honesty.ts";
import { useActiveAccountId } from "../../session.tsx";
import { MarketsNav } from "./MarketsNav.tsx";
import { TradeRefusal } from "./TradeRefusal.tsx";
import "../../styles/markets.css";

/** The scale a Credit is held at. See the note in `Markets.tsx`. */
const CREDIT_DECIMALS = 6;

/** The product kinds, with what each one actually is. */
const KINDS: ReadonlyArray<{ readonly value: string; readonly label: string }> = [
  { value: "", label: "Everything" },
  { value: "DATA", label: "Data" },
  { value: "AGENT_SERVICE", label: "Agent services" },
  { value: "COMPUTE", label: "Compute" },
  { value: "STRATEGY_TEMPLATE", label: "Strategy templates" },
  { value: "RESEARCH", label: "Research" },
  { value: "API_ACCESS", label: "API access" },
  { value: "COMPETITION_ENTRY", label: "Competition entries" },
  { value: "CREATOR_PRODUCT", label: "Other creator products" },
];

const ORIGIN_LABELS: Readonly<Record<string, string>> = {
  DATA_SALE_EARNING: "a data-sale earning",
  AGENT_SERVICE_EARNING: "an agent-service earning",
  CREATOR_EARNING: "a creator earning",
};

export function Products(): ReactNode {
  const accountId = useActiveAccountId();
  const [kind, setKind] = useState("");
  const products = useInternalProducts(kind);
  const bought = useInternalOrders(accountId, "BUYER");
  const sold = useInternalOrders(accountId, "SELLER");

  if (accountId === undefined) {
    return (
      <Page title="Marketplace">
        <MarketsNav />
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is nothing to buy with."
        />
      </Page>
    );
  }

  return (
    <Page
      title="Marketplace"
      lead="Things other people made, priced in Credits. Buying moves Credits between accounts; nothing here touches money."
    >
      <MarketsNav />

      <Panel
        title="For sale"
        description="Published products. A price is fixed for the version shown and cannot change under you."
        actions={
          <FormField label="Kind">
            {(field) => (
              <select
                {...field}
                value={kind}
                onChange={(event) => {
                  setKind(event.target.value);
                }}
              >
                {KINDS.map((entry) => (
                  <option key={entry.value} value={entry.value}>
                    {entry.label}
                  </option>
                ))}
              </select>
            )}
          </FormField>
        }
      >
        <AsyncPanel
          query={products}
          loadingLabel="Asking the backend what is for sale…"
          skeleton={
            <div className="skeleton-set" role="status" aria-live="polite" aria-busy="true">
              <span className="visually-hidden">The catalogue is loading</span>
              {["a", "b", "c"].map((key) => (
                <span key={key} className="skeleton skeleton-block" aria-hidden="true" />
              ))}
            </div>
          }
          empty={{
            isEmpty: (list: InternalProduct[]) => list.length === 0,
            title: "Nothing is listed",
            body: "The backend returned no published products of this kind. That is the catalogue, not a loading state.",
          }}
        >
          {(list: InternalProduct[]) => (
            <div className="stack">
              {list.map((product) => (
                <ProductRow key={product.product_id} product={product} accountId={accountId} />
              ))}
            </div>
          )}
        </AsyncPanel>

        <Disclosure title="What you are spending">
          <p>{CREDITS_DISCLOSURE}</p>
        </Disclosure>
      </Panel>

      <Panel title="What you bought" description="Every purchase this account has made.">
        <AsyncPanel
          query={bought}
          loadingLabel="Asking the backend for this account's purchases…"
          empty={{
            isEmpty: (list: InternalOrder[]) => list.length === 0,
            title: "No purchases",
            body: "This account has bought nothing in the marketplace.",
          }}
        >
          {(list: InternalOrder[]) => <OrderTable orders={list} side="paid" />}
        </AsyncPanel>
      </Panel>

      <Panel title="What you sold" description="Every sale, with the provenance each one produced.">
        <AsyncPanel
          query={sold}
          loadingLabel="Asking the backend for this account's sales…"
          empty={{
            isEmpty: (list: InternalOrder[]) => list.length === 0,
            title: "No sales",
            body: "This account has sold nothing in the marketplace.",
          }}
        >
          {(list: InternalOrder[]) => (
            <>
              <OrderTable orders={list} side="received" />
              <Disclosure title="Why the origin matters">
                <p>{PROVENANCE_NOTE}</p>
              </Disclosure>
            </>
          )}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function ProductRow(props: {
  readonly product: InternalProduct;
  readonly accountId: string;
}): ReactNode {
  const { product } = props;
  const purchase = usePurchaseProduct();
  const mine = product.seller_account_id === props.accountId;

  return (
    <PanelCard
      title={product.title}
      {...(product.description === undefined ? {} : { description: product.description })}
      actions={
        <>
          <Pill tone="info">{product.kind}</Pill>
          {product.terms_frozen === true && <Pill tone="good">Terms fixed</Pill>}
        </>
      }
    >
      <FieldGrid columns={3}>
        <Field
          label="Price"
          note={`Version ${String(product.version)}. Fixed for this version; a change makes a new one.`}
          emphasis
        >
          <Figure
            kind="money"
            value={{ base: product.price, scale: CREDIT_DECIMALS }}
            symbol="Credits"
          />
        </Field>
        <Field label="Platform share" note="Taken out of the price, rounded down, and shown separately.">
          <Figure
            kind="money"
            value={
              product.platform_fee === undefined
                ? null
                : { base: product.platform_fee, scale: CREDIT_DECIMALS }
            }
            symbol="Credits"
            absent="not stated"
          />
        </Field>
        <Field
          label="Seller receives"
          note={`Lands as ${ORIGIN_LABELS[product.earning_origin] ?? product.earning_origin}.`}
        >
          <Figure
            kind="money"
            value={
              product.seller_proceeds === undefined
                ? null
                : { base: product.seller_proceeds, scale: CREDIT_DECIMALS }
            }
            symbol="Credits"
            absent="not stated"
          />
        </Field>
        <Field label="Platform fee rate">
          <Figure kind="bps" bps={product.platform_fee_bps} absent="not stated" />
        </Field>
        <Field label="Seller">
          <IdentifierShort value={product.seller_account_id} what="seller account id" />
        </Field>
      </FieldGrid>

      {purchase.isError && (
        <TradeRefusal
          what="This purchase"
          error={purchase.error}
          onRetry={() => {
            purchase.reset();
          }}
        />
      )}

      {purchase.isSuccess ? (
        <p className="notice notice-good" role="status">
          Bought. The Credits have moved and the order is in your purchases below.
        </p>
      ) : mine ? (
        // A disabled control must carry its reason, and this one is worth
        // reading: buying from yourself would turn Credits into an earning
        // provenance, which is the thing the backend most needs to refuse.
        <Button
          variant="primary"
          disabledReason="You are the seller. An account cannot buy from itself — the backend refuses it too."
        >
          Buy for Credits
        </Button>
      ) : (
        <Button
          variant="primary"
          busy={purchase.isPending}
          busyLabel="Buying…"
          onClick={() => {
            purchase.mutate({
              productId: product.product_id,
              accountId: props.accountId,
              // The price the customer is looking at, sent back to the backend.
              // If it has changed, the purchase is refused rather than charged
              // at the new one.
              expectedPrice: product.price,
              idempotencyKey: newIdempotencyKey(),
            });
          }}
        >
          Buy for Credits
        </Button>
      )}
    </PanelCard>
  );
}

function OrderTable(props: {
  readonly orders: readonly InternalOrder[];
  readonly side: "paid" | "received";
}): ReactNode {
  const columns: ReadonlyArray<Column<InternalOrder>> = [
    {
      key: "order",
      header: "Order",
      cell: (order) => <IdentifierShort value={order.order_id} what="order id" />,
    },
    {
      key: "price",
      header: "Price",
      numeric: true,
      cell: (order) => (
        <Figure kind="money" value={{ base: order.price, scale: CREDIT_DECIMALS }} symbol="Credits" />
      ),
    },
    {
      key: "share",
      header: props.side === "paid" ? "Platform share" : "You received",
      numeric: true,
      cell: (order) => (
        <Figure
          kind="money"
          value={{
            base: props.side === "paid" ? order.platform_fee : order.seller_proceeds,
            scale: CREDIT_DECIMALS,
          }}
          symbol="Credits"
        />
      ),
    },
    {
      key: "origin",
      header: "Origin",
      cell: (order) => ORIGIN_LABELS[order.earning_origin] ?? order.earning_origin,
    },
  ];

  return (
    <DataTable
      caption={
        props.side === "paid"
          ? "Purchases this account has made, with what the platform took from each"
          : "Sales this account has made, with what it received and the provenance each one produced"
      }
      columns={columns}
      rows={props.orders}
      rowKey={(order) => order.order_id}
    />
  );
}
