/**
 * The live market list, on the public site.
 *
 * `GET /v1/native-markets` answers without a session (D-080), so the product
 * page can show the markets that actually exist rather than a picture of some.
 * USER_JOURNEY §0 asked for exactly this and it was not possible until the read
 * was opened; the example composition stays as the fallback for an empty or
 * unreachable list, labelled as it always was.
 *
 * Three rules shape what is rendered:
 *
 *   1. **Nothing beyond what the list carries.** Every figure here is a field
 *      of `NativeMarketSummary`: the marginal price at its own `price_scale`,
 *      the depth the curve prices against, the 24-hour volume in Credits, a
 *      count of trades. Nothing is derived, nothing is summed, and no currency
 *      figure appears beside a Credit one because no approved external value
 *      for a Credit exists.
 *   2. **A market that has not traded has not moved nothing.** The API says so
 *      with `has_24h_change`, and a row without it renders an em dash and the
 *      reason rather than a zero — which would be the interface inventing a
 *      fact about a market nobody touched.
 *   3. **A demo row says it is one.** `demo` is true for anything the sandbox
 *      seeder made, and it is marked per row rather than once at the top,
 *      because a screenshot of one row is the case the label exists for.
 *
 * The only action is to sign in. A signed-out visitor cannot trade, cannot
 * quote, and cannot open a market detail — those reads are still gated — so
 * offering anything else would be offering a door that is locked.
 */
import type { ReactNode } from "react";

import { usePublicMarkets, type NativeMarketSummary, type PublicMarkets } from "../../api/queries.ts";
import { LinkButton } from "../../components/Button.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { Panel } from "../../components/Panel.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { CREDIT_DECIMALS } from "../../content/example.ts";
import { NATIVE_ASSET_RISK, NATIVE_PRICE_NOTE } from "../../lib/honesty.ts";
import { ExampleMarkets } from "./ExampleUI.tsx";

const COLUMNS: ReadonlyArray<Column<NativeMarketSummary>> = [
  {
    key: "name",
    header: "Market",
    cell: (row) => (
      <span>
        {row.name} <span className="mono-small">{row.symbol}</span>
        {row.demo && (
          <>
            {" "}
            <StatusBadge tone="warn" title="Created by the sandbox demo seeder. It represents nothing.">
              DEMO
            </StatusBadge>
          </>
        )}
      </span>
    ),
  },
  {
    key: "price",
    header: "Last price",
    numeric: true,
    cell: (row) => (
      <Figure
        kind="units"
        value={{ base: row.last_price, scale: row.price_scale }}
        symbol="Credits"
      />
    ),
  },
  {
    key: "change",
    header: "24h",
    numeric: true,
    cell: (row) =>
      row.has_24h_change === true ? (
        <Figure kind="bps" bps={row.change_24h_bps ?? null} signed />
      ) : (
        <Figure kind="bps" bps={null} absent="has not traded in the last 24 hours" />
      ),
  },
  {
    key: "liquidity",
    header: "Depth",
    numeric: true,
    riskMeasure: true,
    cell: (row) => (
      <Figure
        kind="units"
        value={{ base: row.liquidity_credits, scale: CREDIT_DECIMALS }}
        symbol="Credits"
        compact
      />
    ),
  },
  {
    key: "volume",
    header: "24h volume",
    numeric: true,
    cell: (row) => (
      <Figure
        kind="units"
        value={{ base: row.credit_volume_24h, scale: CREDIT_DECIMALS }}
        symbol="Credits"
        compact
      />
    ),
  },
  {
    key: "status",
    header: "Status",
    cell: (row) => (
      <StatusBadge tone={row.market_status === "ACTIVE" ? "good" : "warn"}>
        {row.market_status}
      </StatusBadge>
    ),
  },
];

function Live(props: { readonly data: PublicMarkets }): ReactNode {
  const { markets } = props.data;
  const anyDemo = markets.some((market) => market.demo);

  return (
    <Panel
      title="Markets"
      description="The newest markets on this deployment, read from Nodal's public discovery list as this page loaded."
      actions={<LinkButton to="/sign-in">Sign in to trade</LinkButton>}
    >
      <DataTable
        caption="Internal markets: name and symbol, the marginal price in Credits, the twenty-four hour change, the depth the curve prices against, the twenty-four hour volume in Credits, and whether the market is open."
        columns={COLUMNS}
        rows={markets}
        rowKey={(row) => row.market_id}
      />
      {anyDemo && (
        <p className="note">
          Rows marked DEMO were created by the sandbox seeder on a sandbox tier. They exist to make
          the product explorable and they represent nothing.
        </p>
      )}
      <p className="note">{NATIVE_PRICE_NOTE}</p>
      <p className="note">{NATIVE_ASSET_RISK}</p>
      <p className="note">
        Opening a market, taking a quote and trading all need a session. This page shows what is
        listed, and nothing else.
      </p>
    </Panel>
  );
}

/**
 * The live list, or the labelled example when there is nothing to show.
 *
 * The fallback is deliberately the same composition the rest of the site uses
 * and keeps every label it had: a deployment with no markets yet is a fact, and
 * an example that quietly took the place of live data would be the one fake
 * dashboard on a site that has none.
 */
export function LiveMarkets(): ReactNode {
  const markets = usePublicMarkets();

  if (markets.isPending) {
    return (
      <Panel title="Markets" description="Reading Nodal's public market list…">
        <Skeleton shape="rows" label="Loading the market list" />
      </Panel>
    );
  }

  if (markets.isError || markets.data === undefined || markets.data.markets.length === 0) {
    return (
      <>
        <Panel
          title={markets.isError ? "The market list could not be read" : "No markets yet"}
          description={
            markets.isError
              ? "Nodal's public market list did not answer. The example below is an example, and is labelled as one."
              : "This deployment has no internal markets yet. The example below shows what the list looks like when it does."
          }
        >
          <p className="note">
            Nothing here is standing in for live data. What follows is fixed example values, marked
            as such, exactly as it is everywhere else on this site.
          </p>
        </Panel>
        <ExampleMarkets />
      </>
    );
  }

  return <Live data={markets.data} />;
}
