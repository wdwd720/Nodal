/**
 * FEES — trades in which a house book was this account's counterparty.
 *
 * The house (the platform's own principal book) never trades with a customer
 * unless an instrument's book has a crossing rule that allows it, and every
 * such rule carries a customer disclosure that says the activity is
 * simulated. Each trade here names the rule version that allowed it and the
 * disclosure recorded with that version; the disclosures of an instrument's
 * book can be read in full below.
 */
import { useState, type ReactNode } from "react";

import { useHouseTrades, type TreasuryHouseTrade } from "../../../api/advanced/treasury.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { CrossingDisclosuresPanel } from "../treasury/Crossing.tsx";
import { AssetAmount, ExactPrice } from "../treasury/parts.tsx";
import { FeesNav } from "./parts.tsx";

export function HouseTradesPage(): ReactNode {
  const account = useAdvancedAccount();
  return (
    <Page
      title="House trades"
      lead="Every trade in which a house book was this account's counterparty, with the crossing rule that allowed it and what customers were told about it."
    >
      <SimulatedNotice />
      <FeesNav />
      {account.accountId === undefined ? <NoAccount /> : <TradesPanel accountId={account.accountId} />}
      <CrossingDisclosuresPanel />
    </Page>
  );
}

function TradesPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const trades = useHouseTrades(props.accountId, trail.current);
  return (
    <Panel
      title="House trades against this account — simulated"
      description="Newest first. A house book trades against a customer only where a disclosed crossing rule allows it."
    >
      <AsyncPanel
        query={trades}
        loadingLabel="Loading the house trades…"
        empty={{
          isEmpty: (page: CursorPage<TreasuryHouseTrade>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No house trades",
          body: "No house book has been this account's counterparty.",
        }}
      >
        {(page: CursorPage<TreasuryHouseTrade>) => (
          <>
            <ScrollTable
              caption="House trades"
              headers={["Traded", "Instrument", "This account / the house", "Price", "Quantity", "Notional", "Fee this account paid", "Crossing rule and disclosure"]}
            >
              {page.items.map((trade) => (
                <tr key={trade.trade_id} data-trade={trade.trade_id}>
                  <th scope="row">
                    {formatInstant(trade.traded_at)}
                    <span className="cell-note">trade {String(trade.trade_seq)}</span>
                  </th>
                  <td>
                    <Identifier value={trade.instrument_id} />
                  </td>
                  <td>
                    {trade.account_side} / {trade.house_side}
                    <span className="cell-note">
                      house book <code className="mono-small">{trade.house_account_id}</code>
                    </span>
                  </td>
                  <td>
                    <ExactPrice price={trade.price} unit={trade.quote_asset.symbol} />
                  </td>
                  <td>
                    <AssetAmount value={trade.base_quantity} asset={trade.base_asset} />
                  </td>
                  <td>
                    <AssetAmount value={trade.quote_notional} asset={trade.quote_asset} />
                  </td>
                  <td>
                    <AssetAmount value={trade.account_fee} asset={trade.quote_asset} />
                  </td>
                  <td className="cell-prose">
                    version {String(trade.crossing_rule_version)}
                    {trade.disclosure === undefined ? (
                      <span className="cell-note absent">no disclosure text was returned</span>
                    ) : (
                      <span className="cell-note">{trade.disclosure}</span>
                    )}
                  </td>
                </tr>
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
