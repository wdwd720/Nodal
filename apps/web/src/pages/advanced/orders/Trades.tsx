/**
 * THE ACCOUNT'S TRADES on the internal books, newest first, each from the
 * account's own side — its side, whether it made or took liquidity, its fee —
 * with the kind of counterparty and never the counterparty's account; and
 * THE AUDIT PROOF of one trade: the journal command and event that produced
 * it, the ledger transaction and effect that posted it, the evidence hash,
 * the fee schedules by hash and the book state it was read at.
 *
 * A proof is shown to the trade's parties only; to anyone else the backend
 * answers NOT_FOUND, and so does this page.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { useBookTrades, useTradeProof, type BookFill, type BookTradeProof } from "../../../api/advanced/orders.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { INTERNAL_BOOK_NOTE } from "../../../lib/orders.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable, ValueDomainPill } from "../shared.tsx";
import { OrderBooksNav, PriceOf, Recorded, Units, booksHref } from "./parts.tsx";

export function TradesPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  return (
    <Page title="My book trades" lead="The account's side of every trade on an internal book, newest first.">
      <SimulatedNotice />
      <OrderBooksNav />
      {accountId === undefined ? (
        <Panel title="Trades">
          <NoAccount />
        </Panel>
      ) : (
        <TradesPanel key={accountId} accountId={accountId} />
      )}
    </Page>
  );
}

function TradesPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const trades = useBookTrades(props.accountId, trail.current);
  return (
    <Panel
      title="Trades"
      description="Each row is the account's own side of a trade: its fee, in the book's quote asset, and whether its order rested (maker) or arrived (taker). The other side is named only as a customer or the house."
    >
      <AsyncPanel
        query={trades}
        loadingLabel="Loading trades…"
        empty={{
          isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
          title: "No trades",
          body: "The backend lists no trade on an internal book for this account.",
        }}
      >
        {(page) => (
          <>
            <ScrollTable
              caption="Trades on this page"
              headers={["Trade", "Side", "Price", "Quantity", "Notional", "Fee", "Counterparty", "Executed", "Proof"]}
            >
              {page.items.map((fill) => (
                <TradeRow key={`${fill.book_id}-${String(fill.trade_seq)}-${fill.order_id}`} fill={fill} />
              ))}
            </ScrollTable>
            <CursorPager
              noun="trades"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function TradeRow(props: { readonly fill: BookFill }): ReactNode {
  const { fill } = props;
  return (
    <tr>
      <th scope="row">
        <Link to={booksHref("books", fill.instrument_id)}>
          {fill.base_symbol ?? "base"}/{fill.quote_symbol ?? "quote"}
        </Link>
        <span className="cell-note">
          trade #{String(fill.trade_seq)} ·{" "}
          <Link to={booksHref("orders", fill.order_id)}>order</Link>
        </span>
      </th>
      <td>
        {fill.side}
        <span className="cell-note">{fill.liquidity}</span>
      </td>
      <td>
        <PriceOf value={fill.price} quote={fill.quote_symbol} />
      </td>
      <td>
        <Units value={fill.quantity} decimals={fill.base_decimals} symbol={fill.base_symbol} />
      </td>
      <td>
        <Units value={fill.quote_notional} decimals={fill.quote_decimals} symbol={fill.quote_symbol} />
      </td>
      <td>
        <Units value={fill.fee} decimals={fill.quote_decimals} symbol={fill.quote_symbol} />
      </td>
      <td>{fill.counterparty_owner_type}</td>
      <td>
        <Recorded at={fill.executed_at} />
      </td>
      <td>
        <Link to={booksHref("books", fill.instrument_id, "trades", String(fill.trade_seq))}>Audit proof</Link>
      </td>
    </tr>
  );
}

export function TradeProofPage(): ReactNode {
  const { instrumentId, tradeSeq } = useParams();
  const { accountId } = useAdvancedAccount();
  const proof = useTradeProof(accountId, instrumentId, tradeSeq);
  return (
    <Page
      title="Trade audit proof"
      lead="How one trade on an internal book is tied to the book's journal and to the ledger, as the backend proves it."
    >
      <SimulatedNotice />
      <OrderBooksNav />
      <AsyncPanel query={proof} loadingLabel="Loading the proof…">
        {(p) => <ProofPanels proof={p} />}
      </AsyncPanel>
    </Page>
  );
}

function ProofPanels(props: { readonly proof: BookTradeProof }): ReactNode {
  const { proof } = props;
  return (
    <>
      <Panel title="The trade" description={INTERNAL_BOOK_NOTE}>
        <FieldGrid columns={3}>
          <Field label="Book">
            <Link to={booksHref("books", proof.instrument_id)}>
              {proof.base_symbol ?? "base"}/{proof.quote_symbol ?? "quote"}
            </Link>
            <span className="cell-note">trade #{String(proof.trade_seq)}</span>
          </Field>
          <Field label="Value domain">
            <ValueDomainPill value={proof.value_domain} />
          </Field>
          <Field label="Executed">
            <Recorded at={proof.executed_at} />
          </Field>
          <Field label="Price">
            <PriceOf value={proof.price} quote={proof.quote_symbol} />
            <span className="cell-note">{String(proof.price_ticks)} ticks</span>
          </Field>
          <Field label="Quantity">
            <Units value={proof.quantity} decimals={proof.base_decimals} symbol={proof.base_symbol} />
          </Field>
          <Field label="Notional">
            <Units value={proof.quote_notional} decimals={proof.quote_decimals} symbol={proof.quote_symbol} />
          </Field>
          <Field label="Aggressor" note="The side of the order that arrived and took liquidity.">
            {proof.aggressor_side}
          </Field>
          <Field label="Maker" note="The resting order, its owner's kind and its fee.">
            <Pill tone="neutral">{proof.maker_owner_type}</Pill>{" "}
            <Units value={proof.maker_fee} decimals={proof.quote_decimals} symbol={proof.quote_symbol} />
            <span className="cell-note">
              <Identifier value={proof.maker_order_id} label="order" />
            </span>
          </Field>
          <Field label="Taker" note="The arriving order, its owner's kind and its fee.">
            <Pill tone="neutral">{proof.taker_owner_type}</Pill>{" "}
            <Units value={proof.taker_fee} decimals={proof.quote_decimals} symbol={proof.quote_symbol} />
            <span className="cell-note">
              <Identifier value={proof.taker_order_id} label="order" />
            </span>
          </Field>
        </FieldGrid>
      </Panel>
      <Panel
        title="Journal"
        description="The book's journal command and event that produced the trade, and the state the book was in when the proof was read."
      >
        <FieldGrid columns={3}>
          <Field label="Command">{String(proof.command_seq)}</Field>
          <Field label="Event">{String(proof.event_seq)}</Field>
          <Field label="Crossing rule version" note="The house-customer crossing rule the trade was matched under.">
            {String(proof.crossing_rule_version)}
          </Field>
          <Field label="Latest snapshot" note="The journal snapshot a replay starts from.">
            {String(proof.snapshot_seq)}
            {proof.snapshot_hash !== undefined && (
              <span className="cell-note">
                <Identifier value={proof.snapshot_hash} />
              </span>
            )}
          </Field>
          <Field label="Book read at command">{String(proof.book_last_command_seq)}</Field>
          <Field label="Book state hash">
            <Identifier value={proof.book_state_hash} />
          </Field>
        </FieldGrid>
      </Panel>
      <Panel title="Ledger and evidence" description="Where the trade was posted, exactly once, and the evidence it was posted from.">
        <FieldGrid columns={2}>
          <Field label="Ledger transaction">
            <Identifier value={proof.journal_transaction_id} />
          </Field>
          <Field label="Economic effect" note="The exactly-once effect that posted the trade.">
            <Identifier value={proof.effect_id} />
          </Field>
          <Field label="Evidence">
            <Identifier value={proof.evidence_ref} />
          </Field>
          <Field label="Evidence SHA-256">
            <Identifier value={proof.evidence_sha256} />
          </Field>
          <Field label="Maker fee schedule hash">
            <Identifier value={proof.maker_schedule_hash} />
          </Field>
          <Field label="Taker fee schedule hash">
            <Identifier value={proof.taker_schedule_hash} />
          </Field>
        </FieldGrid>
      </Panel>
    </>
  );
}
