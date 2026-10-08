/**
 * THE ACCOUNT'S EQUITY FILLS, newest first: every trade the paper venue
 * filled for the account, charge by charge in the settlement asset, with the
 * date it settles and whether it has.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { useEquityFills, type EquityFill } from "../../../api/advanced/equities.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel } from "../../../components/Layout.tsx";
import { DecimalValue } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { T_PLUS_ONE_NOTE } from "../../../lib/equities.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { EquitiesNav, Recorded, StatusPill, Units, equitiesHref, settlementTone } from "./parts.tsx";

export function FillsPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  return (
    <Page title="Equity fills" lead="Every fill of the account's equity orders, newest first, with every disclosed charge.">
      <SimulatedNotice />
      <EquitiesNav />
      <p className="note">{T_PLUS_ONE_NOTE}</p>
      {accountId === undefined ? (
        <Panel title="Fills">
          <NoAccount />
        </Panel>
      ) : (
        <FillsPanel key={accountId} accountId={accountId} />
      )}
    </Page>
  );
}

function FillsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const fills = useEquityFills(props.accountId, trail.current);
  return (
    <Panel title="Fills" description="Amounts are in each fill's settlement asset.">
      <AsyncPanel
        query={fills}
        loadingLabel="Loading fills…"
        empty={{
          isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
          title: "No fills",
          body: "The backend lists no equity fill for this account.",
        }}
      >
        {(page) => (
          <>
            <ScrollTable
              caption="Fills on this page"
              headers={["Order", "Side", "Shares", "Price", "Notional", "Spread and fees", "Total", "Settlement", "Executed"]}
            >
              {page.items.map((fill) => (
                <FillRow key={fill.id} fill={fill} />
              ))}
            </ScrollTable>
            <CursorPager
              noun="fills"
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

function FillRow(props: { readonly fill: EquityFill }): ReactNode {
  const { fill } = props;
  const amount = (value: string): ReactNode => (
    <Units value={value} decimals={fill.settlement_decimals} symbol={fill.settlement_symbol} />
  );
  return (
    <tr>
      <th scope="row">
        <Link to={equitiesHref("orders", fill.order_id)}>
          <code className="mono-small">{fill.order_id}</code>
        </Link>
      </th>
      <td>{fill.side}</td>
      <td>
        <Units value={fill.quantity} decimals={fill.share_decimals} symbol={undefined} />
      </td>
      <td>
        <DecimalValue value={fill.price} />
      </td>
      <td>{amount(fill.notional)}</td>
      <td>
        spread {amount(fill.spread)}
        <span className="cell-note">
          platform {amount(fill.platform_fees)} · venue {amount(fill.venue_fees)}
        </span>
      </td>
      <td>{amount(fill.total)}</td>
      <td>
        {fill.settlement === undefined ? (
          <span className="absent">not recorded</span>
        ) : (
          <StatusPill status={fill.settlement.state} tone={settlementTone(fill.settlement.state)} />
        )}
        <span className="cell-note">due {fill.settlement_date}</span>
      </td>
      <td>
        <Recorded at={fill.executed_at} />
      </td>
    </tr>
  );
}
