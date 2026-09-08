/**
 * NATIVE MARKETS (gola.md PARTS XIII–XVI, LIV).
 *
 * PART LIV's rule decides how every number on this page is written: a price is
 * quoted in Credits and is never re-expressed in a currency.
 *
 * There is no approved external value for a Credit in this deployment, so a
 * currency figure here would be an exchange rate nobody set. The page shows
 * what the backend computed, in the units it computed them in, and says why
 * there is no currency column.
 *
 * The disclosure this page owes a buyer, and shows without being asked:
 * supply, the reserve backing the price, holder concentration, both fees, the
 * asset's status, and the fact that a user created it and Nodal did not review
 * it as an investment.
 */
import { useState, type ReactNode } from "react";

import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useNativeAsset,
  useNativeAssets,
  useNativeMarket,
  useNativeOrder,
  useNativeQuote,
  type NativeAsset,
  type NativeMarket,
} from "../api/queries.ts";
import { AsyncPanel, Explanation } from "../components/DataState.tsx";
import { Button } from "../components/Button.tsx";
import { Disclosure, Field, FieldGrid, Identifier, Page, Panel, Pill, Table } from "../components/Layout.tsx";
import { Bps, DecimalValue, Qty } from "../components/Money.tsx";
import { NATIVE_ASSET_RISK, NATIVE_PRICE_NOTE } from "../lib/honesty.ts";
import { useActiveAccountId } from "../session.tsx";

const CREDIT_DECIMALS = 6;

/** What each asset status means for somebody deciding whether to trade. */
const STATUS_COPY: Readonly<Record<string, { readonly tone: "neutral" | "good" | "warn" | "bad" | "info"; readonly text: string }>> = {
  DRAFT: { tone: "neutral", text: "Not published. Nothing trades." },
  PENDING_REVIEW: { tone: "info", text: "Submitted and awaiting a moderation decision." },
  ACTIVE: { tone: "good", text: "Trading normally." },
  CLOSE_ONLY: { tone: "warn", text: "Holders can sell. Nobody new can buy." },
  HALTED: { tone: "warn", text: "An operator stopped trading. Existing holdings are untouched." },
  FROZEN: { tone: "bad", text: "Everything is stopped, including selling, while an incident is looked at." },
  DELISTED: { tone: "bad", text: "Permanently removed." },
  REJECTED: { tone: "bad", text: "Refused at moderation." },
};

export function NativeMarkets(): ReactNode {
  const assets = useNativeAssets();
  const [selected, setSelected] = useState<string | undefined>(undefined);

  return (
    <Page
      title="Native Markets"
      lead="Assets created by users, priced by a formula against a shared pool of Credits."
    >
      <Panel
        title="Assets"
        description="Every asset whose market currently accepts at least selling."
      >
        <AsyncPanel
          query={assets}
          loadingLabel="Asking the backend which internal assets are tradable…"
          empty={{
            isEmpty: (list: NativeAsset[]) => list.length === 0,
            title: "No internal assets",
            body: "The backend returned none. Either nobody has created one or none has been activated in this deployment.",
          }}
        >
          {(list: NativeAsset[]) => (
            <Table caption="Internal assets" headers={["Symbol", "Name", "Status", "Creator", ""]}>
              {list.map((asset) => {
                const copy = STATUS_COPY[asset.status] ?? { tone: "neutral" as const, text: asset.status };
                return (
                  <tr key={asset.asset_id}>
                    <td>
                      <strong>{asset.symbol}</strong>
                    </td>
                    <td>{asset.name}</td>
                    <td>
                      <Pill tone={copy.tone}>{asset.status}</Pill>
                      <p className="field-note">{copy.text}</p>
                    </td>
                    <td>
                      <Identifier value={asset.creator_account_id} />
                    </td>
                    <td>
                      <Button variant="quiet" onClick={() => setSelected(asset.asset_id)}>
                        Show details
                      </Button>
                    </td>
                  </tr>
                );
              })}
            </Table>
          )}
        </AsyncPanel>
        <Disclosure title="What these are">
          <p>{NATIVE_ASSET_RISK}</p>
        </Disclosure>
      </Panel>

      {selected !== undefined && <AssetDetail assetId={selected} />}
    </Page>
  );
}

function AssetDetail(props: { readonly assetId: string }): ReactNode {
  const asset = useNativeAsset(props.assetId);

  return (
    <Panel title="Asset" description="Everything the backend records about it, including what cannot change.">
      <AsyncPanel query={asset} loadingLabel="Asking the backend about this asset…">
        {(a: NativeAsset) => (
          <>
            <FieldGrid columns={3}>
              <Field label="Name">{a.name}</Field>
              <Field label="Symbol">{a.symbol}</Field>
              <Field label="Status" note={STATUS_COPY[a.status]?.text ?? ""}>
                <Pill tone={STATUS_COPY[a.status]?.tone ?? "neutral"}>{a.status}</Pill>
              </Field>
              <Field label="Moderation" note="A content decision. It does not by itself start or stop trading.">
                {a.moderation_state ?? "not recorded"}
              </Field>
              <Field
                label="Economics fixed at"
                note="After this instant, supply, allocation, symbol and fees cannot change — not by the creator and not by Nodal."
              >
                {a.economics_locked_at ?? "not yet"}
              </Field>
              <Field label="Created by">
                <Identifier value={a.creator_account_id} />
              </Field>
            </FieldGrid>
            {a.description !== undefined && a.description !== "" && <p>{a.description}</p>}
            <MarketDetail assetId={a.asset_id} />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/**
 * The market behind an asset.
 *
 * The API keys markets by market id and the asset row does not carry one, so
 * this renders the disclosure only once a market id is known. It does not
 * guess one from the asset id: a wrong id would show somebody else's reserve.
 */
function MarketDetail(props: { readonly assetId: string }): ReactNode {
  const [marketId, setMarketId] = useState<string | undefined>(undefined);
  const market = useNativeMarket(marketId);

  if (marketId === undefined) {
    return (
      <Panel
        title="Market"
        description="The v1 API addresses a market by its own id, which the asset record does not carry."
      >
        <form
          className="stack"
          onSubmit={(event) => {
            event.preventDefault();
            const value = new FormData(event.currentTarget).get("market_id");
            if (typeof value === "string" && value !== "") setMarketId(value);
          }}
        >
          <label className="inline-field">
            <span>Market id</span>
            <input name="market_id" placeholder="the market id for this asset" />
          </label>
          <Button variant="secondary" submit>
            Show the market
          </Button>
        </form>
        <p className="field-note">
          Asset <Identifier value={props.assetId} />. Rather than derive a market id this page does not have,
          it asks for one — a derived id would eventually address the wrong market.
        </p>
      </Panel>
    );
  }

  return (
    <Panel title="Market" description="Priced by a constant-product formula against the reserve shown.">
      <AsyncPanel query={market} loadingLabel="Asking the backend about this market…">
        {(m: NativeMarket) => (
          <>
            <FieldGrid columns={3}>
              <Field
                label="Price"
                note="In Credits. Not converted to a currency, because no approved external value for a Credit exists."
                emphasis
              >
                <DecimalValue value={m.spot_price} /> Credits
              </Field>
              <Field label="Status" note={STATUS_COPY[m.status]?.text ?? ""}>
                <Pill tone={STATUS_COPY[m.status]?.tone ?? "neutral"}>{m.status}</Pill>
              </Field>
              <Field label="In circulation" note="Everything the curve has sold.">
                <Qty value={m.circulating_supply} decimals={CREDIT_DECIMALS} />
              </Field>
              <Field
                label="Credits in the reserve"
                note="What actually backs a sale. The virtual part of the curve is not money and is shown separately."
              >
                <Qty value={m.real_credit_reserve} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </Field>
              <Field label="Virtual reserve" note="Part of the pricing formula. No Credits sit behind it.">
                <Qty value={m.virtual_credit_reserve} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </Field>
              <Field label="Unsold supply" note="Still held by the curve.">
                <Qty value={m.asset_reserve} decimals={CREDIT_DECIMALS} />
              </Field>
              <Field label="Platform fee">
                <Bps value={m.platform_fee_bps} />
              </Field>
              <Field label="Creator fee" note="Paid to whoever created the asset, on every trade.">
                <Bps value={m.creator_fee_bps} />
              </Field>
              <Field label="State version" note="Moves once per trade. A quote priced against an older one is refused.">
                {String(m.state_version)}
              </Field>
            </FieldGrid>

            <Holders market={m} />

            <TradePanel market={m} />

            <Disclosure title="How this price works, and what it does not mean">
              <p>{NATIVE_PRICE_NOTE}</p>
              <p>{NATIVE_ASSET_RISK}</p>
            </Disclosure>
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/** Holder concentration, which is the number a buyer most needs (PART LIV). */
function Holders(props: { readonly market: NativeMarket }): ReactNode {
  const holders = props.market.top_holders ?? [];
  if (holders.length === 0) {
    return (
      <p className="field-note">
        The backend reported no holders for this market. Nobody has bought any of it yet.
      </p>
    );
  }
  return (
    <Table caption="Largest holders" headers={["Account", "Holding"]}>
      {holders.map((holder, index) => (
        <tr key={holder.account_id ?? String(index)}>
          <td>
            <Identifier value={holder.account_id} />
          </td>
          <td>
            <Qty value={holder.quantity} decimals={CREDIT_DECIMALS} />
          </td>
        </tr>
      ))}
    </Table>
  );
}

/**
 * Trading against the curve.
 *
 * The shape of this is the whole point. A quote is a RECORD of what the market
 * said at a state version; it is not a promise, and the backend re-prices on
 * execution. So the customer is never asked to agree to a quote — they are
 * asked to agree to a MINIMUM they will accept, which travels with the order
 * and is checked against a freshly computed fill.
 *
 * Two things this deliberately does not do:
 *
 *   - carry the quote's expected output into `min_output` for the customer. A
 *     default that equals the quote would be a slippage tolerance of zero
 *     dressed up as a protection, and every trade would fail. The field starts
 *     empty and the customer states the number.
 *   - hide the fees. The platform fee and the creator fee are separate figures
 *     because they go to different people.
 */
function TradePanel(props: { readonly market: NativeMarket }): ReactNode {
  const accountId = useActiveAccountId();
  const [side, setSide] = useState<"BUY" | "SELL">("BUY");
  const [amount, setAmount] = useState("");
  const [minOutput, setMinOutput] = useState("");
  const quote = useNativeQuote();
  const order = useNativeOrder();

  const tradable = props.market.status === "ACTIVE" || (props.market.status === "CLOSE_ONLY" && side === "SELL");

  if (accountId === undefined) {
    return (
      <Panel title="Trade" description="No account on this session, so there is nothing to trade with.">
        <p className="field-note">The backend returned an empty account list.</p>
      </Panel>
    );
  }

  return (
    <Panel
      title="Trade"
      description="Priced against the pool at execution, never against a quote."
    >
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          quote.mutate({
            marketId: props.market.market_id,
            accountId,
            side,
            amount,
          });
        }}
      >
        <fieldset className="inline-field">
          <legend>Direction</legend>
          <label>
            <input
              type="radio"
              name="side"
              value="BUY"
              checked={side === "BUY"}
              onChange={() => setSide("BUY")}
            />
            <span>Buy with Credits</span>
          </label>
          <label>
            <input
              type="radio"
              name="side"
              value="SELL"
              checked={side === "SELL"}
              onChange={() => setSide("SELL")}
            />
            <span>Sell for Credits</span>
          </label>
        </fieldset>

        <label className="inline-field">
          <span>{side === "BUY" ? "Credits to spend, in base units" : "Units to sell, in base units"}</span>
          <input value={amount} inputMode="numeric" onChange={(e) => setAmount(e.target.value)} />
        </label>

        {amount === "" ? (
          <Button variant="secondary" disabledReason="Enter an amount to price.">
            Price this trade
          </Button>
        ) : (
          <Button variant="secondary" submit busy={quote.isPending} busyLabel="Pricing…">
            Price this trade
          </Button>
        )}
      </form>

      {quote.isError && <Explanation error={quote.error} onRetry={() => quote.reset()} />}

      {quote.isSuccess && (
        <>
          <FieldGrid columns={3}>
            <Field label="You would receive" note="At the state version priced against, and not a promise." emphasis>
              <Qty value={quote.data.expected_output} decimals={CREDIT_DECIMALS} />
            </Field>
            <Field label="Platform fee">
              <Qty value={quote.data.platform_fee} decimals={CREDIT_DECIMALS} symbol="Credits" />
            </Field>
            <Field label="Creator fee" note="Paid to whoever created this asset.">
              <Qty value={quote.data.creator_fee} decimals={CREDIT_DECIMALS} symbol="Credits" />
            </Field>
            <Field label="Effective price" note="In Credits. Not converted to a currency.">
              <DecimalValue value={quote.data.effective_price} /> Credits
            </Field>
            <Field label="Price impact">
              <Bps value={quote.data.slippage_bps} absent="not reported" />
            </Field>
            <Field label="Priced at state version" note="The market moves on every trade; execution re-prices.">
              {String(quote.data.state_version)}
            </Field>
          </FieldGrid>

          <p className="field-note">
            This is a record of what the market said, not an offer. The order below is priced again
            when it executes, and the only number you are agreeing to is the minimum you state.
          </p>

          <form
            className="stack"
            onSubmit={(event) => {
              event.preventDefault();
              order.mutate({
                marketId: props.market.market_id,
                accountId,
                side,
                amount,
                minOutput,
                quoteId: quote.data.quote_id,
                idempotencyKey: newIdempotencyKey(),
              });
            }}
          >
            <label className="inline-field">
              <span>Least you will accept, in base units</span>
              <input value={minOutput} inputMode="numeric" onChange={(e) => setMinOutput(e.target.value)} />
            </label>
            <p className="field-note">
              Left empty this order will not be sent. The number is yours to choose: the trade is
              refused if the market would return less, and a number equal to the quote above would
              refuse almost every trade.
            </p>

            {order.isError && <Explanation error={order.error} onRetry={() => order.reset()} />}

            {order.isSuccess ? (
              <p className="notice notice-good" role="status">
                Filled. The market moved to state version {String(order.data.state_version_after)}.
              </p>
            ) : !tradable ? (
              <Button
                variant="primary"
                disabledReason={`This market is ${props.market.status}. It is not accepting this direction.`}
              >
                Place the order
              </Button>
            ) : minOutput === "" ? (
              <Button variant="primary" disabledReason="State the least you will accept before ordering.">
                Place the order
              </Button>
            ) : (
              <Button variant="primary" submit busy={order.isPending} busyLabel="Placing…">
                Place the order
              </Button>
            )}
          </form>
        </>
      )}

      <Disclosure title="What you are trading against">
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>
    </Panel>
  );
}
