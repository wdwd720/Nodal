/**
 * THE ACCOUNT'S EQUITY ORDERS, newest first, each cancellable while it is
 * live; and ONE ORDER, with everything the backend recorded about it — its
 * reservation, its protection price, why it was refused if it was — and its
 * fill, charge by charge, with the fill's T+1 settlement.
 *
 * An order is the account's own: another account's list is refused by the
 * backend and another account's order is not found, and that answer is what
 * these pages show.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { useEquityOrder, useEquityOrders, type EquityOrder } from "../../../api/advanced/equities.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { BaseUnits, DecimalValue } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { ORDER_STATUS_MEANING, SESSION_LABEL, isLive } from "../../../lib/equities.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, ModePill, NoAccount, ScrollTable } from "../shared.tsx";
import { CancelEquityOrderAction, FillFields } from "./forms.tsx";
import { EquitiesNav, Recorded, StatusPill, Units, equitiesHref, equityOrderTone, settlementTone } from "./parts.tsx";

export function EquityOrdersPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  return (
    <Page title="My equity orders" lead="The account's equity and ETF orders, newest first, with each fill and its settlement.">
      <SimulatedNotice />
      <EquitiesNav />
      {accountId === undefined ? (
        <Panel title="Orders">
          <NoAccount />
        </Panel>
      ) : (
        <OrdersPanel key={accountId} accountId={accountId} />
      )}
    </Page>
  );
}

function OrdersPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const orders = useEquityOrders(props.accountId, trail.current);
  return (
    <Panel title="Orders" description="Every order is a day order: a live one ends filled, cancelled or expired by the end of its day.">
      <AsyncPanel
        query={orders}
        loadingLabel="Loading orders…"
        empty={{
          isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
          title: "No orders",
          body: "The backend lists no equity order for this account.",
        }}
      >
        {(page) => (
          <>
            <ScrollTable caption="Orders on this page" headers={["Order", "Status", "Side", "Quantity", "Fill", "Settlement", "Trade date", "Cancel"]}>
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

function OrderRow(props: { readonly order: EquityOrder }): ReactNode {
  const { order } = props;
  const fill = order.fill;
  return (
    <tr>
      <th scope="row">
        <Link to={equitiesHref("orders", order.id)}>{order.symbol}</Link>
        <span className="cell-note mono-small">{order.id}</span>
      </th>
      <td>
        <StatusPill status={order.status} tone={equityOrderTone(order.status)} />
        {order.rejection_code !== undefined && <span className="cell-note mono-small">{order.rejection_code}</span>}
      </td>
      <td>
        {order.side} {order.order_type}
        {order.limit_price !== undefined && (
          <span className="cell-note">
            limit <DecimalValue value={order.limit_price} />
          </span>
        )}
      </td>
      <td>
        <Units value={order.quantity} decimals={order.share_decimals} symbol={order.symbol} />
      </td>
      <td>
        {fill === undefined ? (
          <span className="absent">no fill</span>
        ) : (
          <>
            at <DecimalValue value={fill.price} />
            <span className="cell-note">
              total <Units value={fill.total} decimals={fill.settlement_decimals} symbol={fill.settlement_symbol} />
            </span>
          </>
        )}
      </td>
      <td>
        {fill?.settlement === undefined ? (
          <span className="absent">none</span>
        ) : (
          <>
            <StatusPill status={fill.settlement.state} tone={settlementTone(fill.settlement.state)} />
            <span className="cell-note">due {fill.settlement.due_date}</span>
          </>
        )}
      </td>
      <td>{order.trade_date}</td>
      <td>{isLive(order.status) ? <CancelEquityOrderAction order={order} /> : <span className="absent">ended</span>}</td>
    </tr>
  );
}

export function EquityOrderDetailPage(): ReactNode {
  const { orderId } = useParams();
  const { accountId } = useAdvancedAccount();
  const order = useEquityOrder(accountId, orderId);
  return (
    <Page title="Equity order" lead="One equity order, as the backend recorded it, with its fill and settlement.">
      <SimulatedNotice />
      <EquitiesNav />
      <AsyncPanel query={order} loadingLabel="Loading the order…">
        {(o) => <OrderPanels order={o} />}
      </AsyncPanel>
    </Page>
  );
}

function OrderPanels(props: { readonly order: EquityOrder }): ReactNode {
  const { order } = props;
  return (
    <>
      <Panel title={`${order.side} ${order.symbol}`} description={ORDER_STATUS_MEANING[order.status]}>
        <FieldGrid columns={3}>
          <Field label="Status">
            <StatusPill status={order.status} tone={equityOrderTone(order.status)} />
            {order.rejection_code !== undefined && (
              <span className="cell-note">
                refused: <code className="mono-small">{order.rejection_code}</code>
              </span>
            )}
          </Field>
          <Field label="Mode">
            <ModePill mode={order.mode} />
          </Field>
          <Field label="Listing">
            <Link to={equitiesHref("listings", order.instrument_id)}>{order.symbol}</Link>
            <span className="cell-note">{order.exchange_mic}</span>
          </Field>
          <Field label="Order">
            {order.side} {order.order_type}
            {order.extended_hours && <span className="cell-note">extended hours</span>}
          </Field>
          <Field label="Quantity">
            <Units value={order.quantity} decimals={order.share_decimals} symbol={order.symbol} />
            <span className="cell-note">
              <BaseUnits value={order.quantity} />
            </span>
          </Field>
          <Field label="Limit price">
            {order.limit_price === undefined ? <span className="absent">a market order has none</span> : <DecimalValue value={order.limit_price} />}
          </Field>
          <Field label="Protection price" note="The worst price a market order may fill at; its reservation is sized on it.">
            {order.protection_price === undefined ? <span className="absent">none</span> : <DecimalValue value={order.protection_price} />}
          </Field>
          <Field label="Reserved" note="Held against this order while it was live; released or consumed when it ended.">
            {order.reserved_quantity === undefined ? (
              <span className="absent">nothing reserved</span>
            ) : order.side === "SELL" ? (
              <Units value={order.reserved_quantity} decimals={order.share_decimals} symbol={order.symbol} />
            ) : (
              <>
                <BaseUnits value={order.reserved_quantity} />
                <span className="cell-note">of the settlement asset</span>
              </>
            )}
            {order.reservation_id !== undefined && (
              <span className="cell-note">
                <Identifier value={order.reservation_id} />
              </span>
            )}
          </Field>
          <Field label="Session at acceptance" note={SESSION_LABEL[order.session]}>
            {order.session}
          </Field>
          <Field label="Trade date">{order.trade_date}</Field>
          <Field label="Expires" note="A day order ends at the close of the last session it may trade in.">
            <Recorded at={order.expires_at} />
          </Field>
          <Field label="Venue">
            {order.venue}
            {order.venue_order_id !== undefined && <span className="cell-note mono-small">{order.venue_order_id}</span>}
          </Field>
          <Field label="Intent">
            <Identifier value={order.intent_id} />
          </Field>
          <Field label="Risk decision">
            <Identifier value={order.risk_decision_id} />
          </Field>
          <Field label="Placed">
            <Recorded at={order.created_at} />
          </Field>
        </FieldGrid>
        {order.rejection_reasons.length > 0 && (
          <ul className="codes">
            {order.rejection_reasons.map((reason, index) => (
              <li key={`${reason}-${String(index)}`}>
                <code className="mono-small">{reason}</code>
              </li>
            ))}
          </ul>
        )}
      </Panel>
      {order.fill !== undefined && (
        <Panel title="Fill" description="Every charge the disclosed schedule produced, in the settlement asset.">
          <FillFields fill={order.fill} shareSymbol={order.symbol} />
        </Panel>
      )}
      {isLive(order.status) && (
        <Panel title="Cancel" description="Cancels the order at the venue; its reservation is released.">
          <CancelEquityOrderAction order={order} />
        </Panel>
      )}
    </>
  );
}
