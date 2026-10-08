/**
 * THE ACCOUNT'S SETTLEMENTS: what each equity trade still owes the account
 * — the shares a buy is owed, or the proceeds a sale is owed — and when. A
 * PENDING settlement is not spendable, sellable or withdrawable; it becomes
 * so on its settlement date, T+1, when an operator's settlement run settles
 * it. The page never adds pending amounts into anything.
 */
import { useId, useState, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { useEquitySettlements, type EquitySettlement } from "../../../api/advanced/equities.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  EQUITY_SETTLEMENT_FILTERS,
  SETTLEMENT_STATE_MEANING,
  T_PLUS_ONE_NOTE,
  type EquitySettlementFilter,
} from "../../../lib/equities.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { EquitiesNav, Recorded, StatusPill, Units, equitiesHref, settlementTone } from "./parts.tsx";

function parseFilter(raw: string | null): EquitySettlementFilter {
  return EQUITY_SETTLEMENT_FILTERS.find((filter) => filter === raw) ?? "PENDING";
}

export function SettlementsPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  const [params, setParams] = useSearchParams();
  const state = parseFilter(params.get("state"));
  const selectId = useId();
  return (
    <Page
      title="Equity settlements"
      lead="What the account's equity trades are owed, and when: T+1, by the operator's settlement run."
      actions={
        <div className="filter">
          <label htmlFor={selectId}>Show</label>
          <select
            id={selectId}
            className="input"
            value={state}
            onChange={(event) => {
              setParams({ state: event.target.value });
            }}
          >
            <option value="PENDING">Pending</option>
            <option value="SETTLED">Settled</option>
            <option value="ALL">Every settlement</option>
          </select>
        </div>
      }
    >
      <SimulatedNotice />
      <EquitiesNav />
      <p className="note">{T_PLUS_ONE_NOTE}</p>
      {accountId === undefined ? (
        <Panel title="Settlements">
          <NoAccount />
        </Panel>
      ) : (
        <SettlementsPanel key={`${accountId}-${state}`} accountId={accountId} state={state} />
      )}
    </Page>
  );
}

function SettlementsPanel(props: { readonly accountId: string; readonly state: EquitySettlementFilter }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const settlements = useEquitySettlements(props.accountId, props.state, trail.current);
  const title = props.state === "PENDING" ? "Pending" : props.state === "SETTLED" ? "Settled" : "Every settlement";
  return (
    <Panel
      title={title}
      description={props.state === "ALL" ? "Pending and settled, newest first." : SETTLEMENT_STATE_MEANING[props.state]}
    >
      <AsyncPanel
        query={settlements}
        loadingLabel="Loading settlements…"
        empty={{
          isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
          title: props.state === "PENDING" ? "Nothing pending" : "No settlements",
          body:
            props.state === "PENDING"
              ? "The backend lists nothing still to settle for this account."
              : "The backend lists no settlement of this kind for this account.",
        }}
      >
        {(page) => (
          <>
            <ScrollTable caption="Settlements on this page" headers={["Owed", "State", "Trade date", "Due", "Settled", "Order"]}>
              {page.items.map((s) => (
                <SettlementRow key={s.id} settlement={s} />
              ))}
            </ScrollTable>
            <CursorPager
              noun="settlements"
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

function SettlementRow(props: { readonly settlement: EquitySettlement }): ReactNode {
  const { settlement: s } = props;
  return (
    <tr>
      <th scope="row">
        <Units value={s.quantity} decimals={s.asset_decimals} symbol={s.asset_symbol} />
      </th>
      <td>
        <StatusPill status={s.state} tone={settlementTone(s.state)} />
      </td>
      <td>{s.trade_date}</td>
      <td>{s.due_date}</td>
      <td>{s.settled_at === undefined ? <span className="absent">not yet</span> : <Recorded at={s.settled_at} />}</td>
      <td>
        <Link to={equitiesHref("orders", s.order_id)}>
          <code className="mono-small">{s.order_id}</code>
        </Link>
      </td>
    </tr>
  );
}
