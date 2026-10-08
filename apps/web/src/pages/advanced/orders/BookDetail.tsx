/**
 * ONE ORDER BOOK: its terms and the disclosures every customer is owed before
 * trading on it — the fee schedules (versioned and hashed) and the crossing
 * rule in force — the depth ladder as persisted, the order form, and for an
 * operator the book's status and a replay of its journal. The crossing rule
 * is read here only: it changes through the treasury service, never here.
 */
import type { ReactNode } from "react";
import { useParams } from "react-router-dom";

import { useBook, useBookDepth, type Book, type BookFeeSchedule } from "../../../api/advanced/orders.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Bps } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  BOOK_STATUS_MEANING,
  CROSSING_RULE_NOTE,
  INTERNAL_BOOK_NOTE,
  TERMS_SOURCE_LABEL,
  ladderRows,
} from "../../../lib/orders.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { ModePill, ScrollTable, ValueDomainPill } from "../shared.tsx";
import { BookStatusForm, JournalVerificationPanel, SubmitOrderForm } from "./forms.tsx";
import { OrderBooksNav, PriceOf, StatusPill, Units, bookStatusTone } from "./parts.tsx";

export function BookDetailPage(): ReactNode {
  const { instrumentId } = useParams();
  const book = useBook(instrumentId);
  const title =
    book.data === undefined ? "Order book" : `${book.data.base_symbol}/${book.data.quote_symbol} order book`;
  return (
    <Page title={title} lead="One internal book: its terms and disclosures, what rests on it, and the order form.">
      <SimulatedNotice />
      <OrderBooksNav />
      <AsyncPanel query={book} loadingLabel="Loading the book…">
        {(b) => <BookPanels book={b} />}
      </AsyncPanel>
    </Page>
  );
}

function BookPanels(props: { readonly book: Book }): ReactNode {
  const { book } = props;
  const { canInspect } = useAdvancedAccount();
  return (
    <>
      <Panel title="Terms" description={INTERNAL_BOOK_NOTE}>
        <FieldGrid columns={3}>
          <Field label="Status" note={BOOK_STATUS_MEANING[book.status]}>
            <StatusPill status={book.status} tone={bookStatusTone(book.status)} />
          </Field>
          <Field label="Mode">
            <ModePill mode={book.mode} />
          </Field>
          <Field label="Value domain">
            <ValueDomainPill value={book.value_domain} />
          </Field>
          <Field label="Base asset">
            {book.base_symbol} <span className="cell-note">{String(book.base_decimals)} decimal places</span>
          </Field>
          <Field label="Quote asset">
            {book.quote_symbol} <span className="cell-note">{String(book.quote_decimals)} decimal places</span>
          </Field>
          <Field label="Terms" note={TERMS_SOURCE_LABEL[book.terms_source]}>
            {book.terms_source}
            {book.terms_version !== undefined && <span className="cell-note">version {String(book.terms_version)}</span>}
          </Field>
          <Field label="Tick" note={`Prices carry ${String(book.price_scale)} decimal places and move in ticks of this size.`}>
            <PriceOf value={book.tick_price} quote={book.quote_symbol} />
          </Field>
          <Field label="Lot" note="Quantities are whole lots of this size.">
            <Units value={book.lot_size} decimals={book.base_decimals} symbol={book.base_symbol} />
          </Field>
          <Field label="Minimum quantity">
            <Units value={book.min_quantity} decimals={book.base_decimals} symbol={book.base_symbol} />
          </Field>
          <Field label="Highest price the book accepts">
            <PriceOf value={book.max_price} quote={book.quote_symbol} />
          </Field>
          <Field label="Trading calendar">{book.trading_calendar}</Field>
          <Field label="Journal" note="The book is a journal of commands; its state hash is what a replay must reproduce.">
            command {String(book.last_command_seq)} · event {String(book.last_event_seq)} · trade {String(book.last_trade_seq)}
            <span className="cell-note">
              <Identifier value={book.state_hash} label="state" />
            </span>
          </Field>
        </FieldGrid>
      </Panel>
      <Panel
        title="Disclosed fees"
        description="The schedules every trade on this book is charged by, in its quote asset. A taker pays the taker schedule; the resting order it trades against pays the maker schedule."
      >
        <FieldGrid columns={2}>
          <ScheduleField label="Taker schedule" schedule={book.taker_fee} />
          <ScheduleField label="Maker schedule" schedule={book.maker_fee} />
        </FieldGrid>
      </Panel>
      <Panel title="Crossing rule" description={CROSSING_RULE_NOTE}>
        <FieldGrid columns={3}>
          <Field label="House-customer crossing">
            {book.crossing_rule.allow_house_customer_crossing ? (
              <Pill tone="warn">ALLOWED</Pill>
            ) : (
              <Pill tone="good">FORBIDDEN</Pill>
            )}
            {book.crossing_rule.is_default && <span className="cell-note">the default: no rule has been recorded</span>}
          </Field>
          <Field label="Version">{String(book.crossing_rule.version)}</Field>
          {!book.crossing_rule.is_default && (
            <Field label="Recorded">
              {formatInstant(book.crossing_rule.created_at)}
              <span className="cell-note">
                {book.crossing_rule.actor_type ?? "actor not reported"} {book.crossing_rule.actor_id ?? ""}
                {book.crossing_rule.reason !== undefined && ` — ${book.crossing_rule.reason}`}
              </span>
            </Field>
          )}
        </FieldGrid>
      </Panel>
      <Panel title="Depth" description="Resting orders aggregated by price, best level of each side next to the spread, as persisted.">
        <DepthLadder book={book} />
      </Panel>
      <Panel title="Place an order" description="On this book, from this session's own account.">
        <SubmitOrderForm book={book} />
      </Panel>
      {canInspect && (
        <>
          <Panel
            title="Book status"
            description="Operator only. Halting stops matching and refuses new orders; closing ends the book once nothing works on it."
          >
            <BookStatusForm book={book} />
          </Panel>
          <Panel
            title="Journal verification"
            description="Operator only, read-only: the backend replays the book's journal from genesis and from its latest snapshot."
          >
            <JournalVerificationPanel book={book} />
          </Panel>
        </>
      )}
    </>
  );
}

function ScheduleField(props: { readonly label: string; readonly schedule: BookFeeSchedule }): ReactNode {
  const { schedule } = props;
  return (
    <Field label={props.label} note={`Version ${schedule.version}, hash ${schedule.hash}.`}>
      <Bps value={schedule.percentage_fee_bps} /> of the notional
      <span className="cell-note">
        strategy orders <Bps value={schedule.strategy_fee_bps} /> more · subscription discount{" "}
        <Bps value={schedule.subscription_discount_bps} /> · capped at <Bps value={schedule.max_total_fee_bps} />
      </span>
    </Field>
  );
}

function DepthLadder(props: { readonly book: Book }): ReactNode {
  const { book } = props;
  const depth = useBookDepth(book.instrument_id);
  return (
    <AsyncPanel
      query={depth}
      loadingLabel="Reading the book…"
      empty={{
        isEmpty: (d) => d.bids.length === 0 && d.asks.length === 0,
        title: "Nothing resting",
        body: "The backend reports no resting order on either side of this book. That is its answer, not a price.",
      }}
    >
      {(d) => (
        <>
          <ScrollTable caption="Depth ladder" headers={["Side", "Price", "Quantity", "Orders"]}>
            {ladderRows(d).map((row) => (
              <tr key={`${row.side}-${String(row.level.price_ticks)}`}>
                <td>
                  <Pill tone={row.side === "SELL" ? "bad" : "good"}>{row.side === "SELL" ? "ASK" : "BID"}</Pill>
                  {row.rank === 1 && <span className="cell-note">best</span>}
                </td>
                <td>
                  <PriceOf value={row.level.price} quote={book.quote_symbol} />
                </td>
                <td>
                  <Units value={row.level.quantity} decimals={d.base_decimals} symbol={book.base_symbol} />
                </td>
                <td>{String(row.level.orders)}</td>
              </tr>
            ))}
          </ScrollTable>
          <p className="note">
            Read at journal command {String(d.last_command_seq)}; state hash <code className="mono-small">{d.state_hash}</code>.{" "}
            {d.status !== "OPEN" && `The book is ${d.status}.`}
          </p>
        </>
      )}
    </AsyncPanel>
  );
}
