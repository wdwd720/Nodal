/**
 * THE ACCOUNT'S ORDERS on the internal books, newest first — the working ones
 * by default, every one on request — each cancellable while it works; and
 * ONE ORDER, with everything the backend recorded about it, its cancel and
 * its replacement.
 *
 * An order is the account's own: another account's list is refused by the
 * backend and another account's order is not found, and that answer is what
 * these pages show.
 */
import { useId, useState, type ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";

import { useBook, useBookOrder, useBookOrders, type BookOrder } from "../../../api/advanced/orders.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { BaseUnits } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  BOOK_ORDER_SCOPES,
  ORDER_STATUS_MEANING,
  STP_LABEL,
  TIME_IN_FORCE_LABEL,
  isWorking,
  type BookOrderScope,
} from "../../../lib/orders.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, ModePill, NoAccount, ScrollTable } from "../shared.tsx";
import { CancelOrderAction, ReplaceOrderForm } from "./forms.tsx";
import { OrderBooksNav, PriceOf, Recorded, StatusPill, Units, booksHref, orderStatusTone } from "./parts.tsx";

function parseScope(raw: string | null): BookOrderScope {
  return BOOK_ORDER_SCOPES.find((scope) => scope === raw) ?? "WORKING";
}

export function OrdersPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  const [params, setParams] = useSearchParams();
  const scope = parseScope(params.get("scope"));
  const selectId = useId();
  return (
    <Page
      title="My book orders"
      lead="The account's orders on the internal books, newest first."
      actions={
        <div className="filter">
          <label htmlFor={selectId}>Show</label>
          <select
            id={selectId}
            className="input"
            value={scope}
            onChange={(event) => {
              setParams(event.target.value === "ALL" ? { scope: "ALL" } : {});
            }}
          >
            <option value="WORKING">Working orders</option>
            <option value="ALL">Every order</option>
          </select>
        </div>
      }
    >
      <SimulatedNotice />
      <OrderBooksNav />
      {accountId === undefined ? (
        <Panel title="Orders">
          <NoAccount />
        </Panel>
      ) : (
        <OrdersPanel key={`${accountId}-${scope}`} accountId={accountId} scope={scope} />
      )}
    </Page>
  );
}

function OrdersPanel(props: { readonly accountId: string; readonly scope: BookOrderScope }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const orders = useBookOrders(props.accountId, props.scope, trail.current);
  return (
    <Panel
      title={props.scope === "WORKING" ? "Working orders" : "Every order"}
      description={
        props.scope === "WORKING"
          ? "Pending, open and partially filled orders: the ones a cancel can still end."
          : "Every order the account placed on an internal book, ended ones included."
      }
    >
      <AsyncPanel
        query={orders}
        loadingLabel="Loading orders…"
        empty={{
          isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
          title: props.scope === "WORKING" ? "No working orders" : "No orders",
          body:
            props.scope === "WORKING"
              ? "The backend lists no pending, open or partially filled order for this account."
              : "The backend lists no order on an internal book for this account.",
        }}
      >
        {(page) => (
          <>
            <ScrollTable caption="Orders on this page" headers={["Order", "Status", "Side", "Price", "Quantity", "Filled", "Placed", "Cancel"]}>
              {page.items.map((order) => (
                <OrderRow key={order.id} order={order} />
              ))}
            </ScrollTable>
            <CursorPager
              noun="orders"
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

function OrderRow(props: { readonly order: BookOrder }): ReactNode {
  const { order } = props;
  return (
    <tr>
      <th scope="row">
        <Link to={booksHref("orders", order.id)}>
          <code className="mono-small">{order.id}</code>
        </Link>
        <span className="cell-note">
          {order.base_symbol ?? "base"}/{order.quote_symbol ?? "quote"}
          {order.client_order_id !== undefined && ` · ${order.client_order_id}`}
        </span>
      </th>
      <td>
        <StatusPill status={order.status} tone={orderStatusTone(order.status)} />
      </td>
      <td>
        {order.side} {order.order_type}
        <span className="cell-note">{order.time_in_force}</span>
      </td>
      <td>
        <PriceOf value={order.price} quote={order.quote_symbol} />
      </td>
      <td>
        <Units value={order.quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
      </td>
      <td>
        <Units value={order.filled_quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
      </td>
      <td>
        <Recorded at={order.created_at} />
      </td>
      <td>{isWorking(order.status) ? <CancelOrderAction order={order} bookStatus={undefined} /> : <span className="absent">ended</span>}</td>
    </tr>
  );
}

export function OrderDetailPage(): ReactNode {
  const { orderId } = useParams();
  const { accountId } = useAdvancedAccount();
  const order = useBookOrder(accountId, orderId);
  return (
    <Page title="Book order" lead="One order on an internal book, as the backend recorded it.">
      <SimulatedNotice />
      <OrderBooksNav />
      <AsyncPanel query={order} loadingLabel="Loading the order…">
        {(o) => <OrderPanels order={o} />}
      </AsyncPanel>
    </Page>
  );
}

function OrderPanels(props: { readonly order: BookOrder }): ReactNode {
  const { order } = props;
  const book = useBook(order.instrument_id);
  const bookStatus = book.data?.status;
  return (
    <>
      <Panel title="Order" description={ORDER_STATUS_MEANING[order.status]}>
        <FieldGrid columns={3}>
          <Field label="Status">
            <StatusPill status={order.status} tone={orderStatusTone(order.status)} />
            {order.reject_reason !== undefined && (
              <span className="cell-note">
                refused: <code className="mono-small">{order.reject_reason}</code>
              </span>
            )}
            {order.cancel_reason !== undefined && (
              <span className="cell-note">
                cancelled: <code className="mono-small">{order.cancel_reason}</code>
              </span>
            )}
          </Field>
          <Field label="Book">
            <Link to={booksHref("books", order.instrument_id)}>
              {order.base_symbol ?? "base"}/{order.quote_symbol ?? "quote"}
            </Link>
            <span className="cell-note">{order.venue_label}</span>
          </Field>
          <Field label="Mode">
            <ModePill mode={order.mode} />
          </Field>
          <Field label="Order">
            {order.side} {order.order_type}
            <span className="cell-note">{TIME_IN_FORCE_LABEL[order.time_in_force]}</span>
          </Field>
          <Field label={order.order_type === "LIMIT" ? "Limit price" : "Protection price"}>
            <PriceOf value={order.price} quote={order.quote_symbol} />
            <span className="cell-note">{String(order.price_ticks)} ticks</span>
          </Field>
          <Field label="Quantity">
            <Units value={order.quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
            <span className="cell-note">
              <BaseUnits value={order.quantity} />
            </span>
          </Field>
          <Field label="Filled">
            <Units value={order.filled_quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
          </Field>
          <Field label="Resting">
            <Units value={order.remaining_quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
          </Field>
          <Field label="Cancelled">
            <Units value={order.cancelled_quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
          </Field>
          <Field label="Liquidity intent">{order.liquidity_intent}</Field>
          <Field label="Self-trade prevention">{STP_LABEL[order.stp]}</Field>
          <Field label="Flags">
            {order.post_only ? "post only" : "may take liquidity"} · {order.reduce_only ? "reduce only" : "not reduce only"}
            {order.strategy_originated && " · placed by a strategy"}
          </Field>
          <Field label="Client order id">
            {order.client_order_id === undefined ? <span className="absent">none given</span> : <code className="mono-small">{order.client_order_id}</code>}
          </Field>
          <Field label="Placed">
            <Recorded at={order.created_at} />
          </Field>
          <Field label="Last changed">
            <Recorded at={order.updated_at} />
          </Field>
          <Field label="Intent">
            <Identifier value={order.intent_id} />
          </Field>
          <Field label="Reservation">
            <Identifier value={order.reservation_id} />
          </Field>
          <Field label="Journal" note="Commands of the book's journal that accepted and last changed the order.">
            {order.accepted_command_seq === undefined ? "not on the book" : `accepted at ${String(order.accepted_command_seq)}`}
            {order.last_command_seq !== undefined && ` · last ${String(order.last_command_seq)}`}
          </Field>
        </FieldGrid>
      </Panel>
      {isWorking(order.status) && (
        <>
          <Panel title="Cancel" description="Ends the order; what has not traded is released.">
            <CancelOrderAction order={order} bookStatus={bookStatus} />
          </Panel>
          <Panel title="Replace" description="A new price and a new total quantity for the resting order, in one command that may trade.">
            <ReplaceOrderForm order={order} bookStatus={bookStatus} />
          </Panel>
        </>
      )}
    </>
  );
}
