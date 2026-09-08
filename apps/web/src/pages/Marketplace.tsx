/**
 * MARKETPLACE — the creator economy (gola.md PART XVII).
 *
 * What a customer needs to understand before they buy, and what this page
 * therefore refuses to hide:
 *
 *   - a price is in Credits and is never converted to a currency;
 *   - buying sends Credits to another user, and the platform takes a share
 *     that is shown as its own figure rather than folded into the price;
 *   - the provenance a SALE produces is shown on the seller's side, because a
 *     seller is entitled to know which category their revenue lands in before
 *     they list — it decides whether a payout policy can ever reach it.
 *
 * The purchase button sends the price the customer was shown. If the backend
 * disagrees the purchase is refused, and the refusal is rendered as one — this
 * page never retries at a different price.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useInternalOrders,
  useInternalProducts,
  usePurchaseProduct,
  type InternalOrder,
  type InternalProduct,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState, Explanation } from "../components/DataState.tsx";
import { Button } from "../components/Button.tsx";
import { Disclosure, Identifier, Page, Panel, Pill, Table } from "../components/Layout.tsx";
import { Bps, Qty } from "../components/Money.tsx";
import { CREDITS_DISCLOSURE, PROVENANCE_NOTE } from "../lib/honesty.ts";
import { useActiveAccountId } from "../session.tsx";

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

export function Marketplace(): ReactNode {
  const accountId = useActiveAccountId();
  const [kind, setKind] = useState("");
  const products = useInternalProducts(kind);
  const bought = useInternalOrders(accountId, "BUYER");
  const sold = useInternalOrders(accountId, "SELLER");

  if (accountId === undefined) {
    return (
      <Page title="Marketplace">
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
      <Panel
        title="For sale"
        description="Published products. Prices are fixed for the version shown and cannot change under you."
        actions={
          <label className="inline-field">
            <span>Kind</span>
            <select value={kind} onChange={(e) => setKind(e.target.value)}>
              {KINDS.map((k) => (
                <option key={k.value} value={k.value}>
                  {k.label}
                </option>
              ))}
            </select>
          </label>
        }
      >
        <AsyncPanel
          query={products}
          loadingLabel="Asking the backend what is for sale…"
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
          {(list: InternalOrder[]) => <OrderTable orders={list} caption="Purchases" side="paid" />}
        </AsyncPanel>
      </Panel>

      <Panel
        title="What you sold"
        description="Every sale, with the provenance each one produced."
      >
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
              <OrderTable orders={list} caption="Sales" side="received" />
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
    <article className="panel-nested" aria-label={product.title}>
      <div className="panel-head">
        <div>
          <h3>{product.title}</h3>
          <p className="panel-description">{product.description ?? ""}</p>
        </div>
        <div className="panel-actions">
          <Pill tone="info">{product.kind}</Pill>
          {product.terms_frozen === true && <Pill tone="good">Terms fixed</Pill>}
        </div>
      </div>

      <dl className="field-grid cols-3">
        <div className="field field-emphasis">
          <dt>Price</dt>
          <dd>
            <Qty value={product.price} decimals={CREDIT_DECIMALS} symbol="Credits" />
            <p className="field-note">Version {product.version}. Fixed for this version.</p>
          </dd>
        </div>
        <div className="field">
          <dt>Platform share</dt>
          <dd>
            <Qty value={product.platform_fee} decimals={CREDIT_DECIMALS} symbol="Credits" />
            <p className="field-note">
              <Bps value={product.platform_fee_bps} absent="not stated" /> of the price, rounded down.
            </p>
          </dd>
        </div>
        <div className="field">
          <dt>Seller receives</dt>
          <dd>
            <Qty value={product.seller_proceeds} decimals={CREDIT_DECIMALS} symbol="Credits" />
            <p className="field-note">
              Lands as {ORIGIN_LABELS[product.earning_origin] ?? product.earning_origin}.
            </p>
          </dd>
        </div>
      </dl>

      <p className="field-note">
        <Identifier label="Seller" value={product.seller_account_id} />
      </p>

      {purchase.isError && <Explanation error={purchase.error} onRetry={() => purchase.reset()} />}

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
              // The price the customer is looking at, sent back to the
              // backend. If it has changed, the purchase is refused rather
              // than charged at the new one.
              expectedPrice: product.price,
              idempotencyKey: newIdempotencyKey(),
            });
          }}
        >
          Buy for Credits
        </Button>
      )}
    </article>
  );
}

function OrderTable(props: {
  readonly orders: readonly InternalOrder[];
  readonly caption: string;
  readonly side: "paid" | "received";
}): ReactNode {
  return (
    <Table
      caption={props.caption}
      headers={["Order", "Price", props.side === "paid" ? "Platform share" : "You received", "Origin"]}
    >
      {props.orders.map((order) => (
        <tr key={order.order_id}>
          <td>
            <Identifier value={order.order_id} />
          </td>
          <td>
            <Qty value={order.price} decimals={CREDIT_DECIMALS} symbol="Credits" />
          </td>
          <td>
            <Qty
              value={props.side === "paid" ? order.platform_fee : order.seller_proceeds}
              decimals={CREDIT_DECIMALS}
              symbol="Credits"
            />
          </td>
          <td>{ORIGIN_LABELS[order.earning_origin] ?? order.earning_origin}</td>
        </tr>
      ))}
    </Table>
  );
}
