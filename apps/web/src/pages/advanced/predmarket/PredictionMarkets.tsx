/**
 * PREDICTION MARKETS (internal/predmarket): markets on the outcome of an
 * event, in PAPER or SANDBOX mode, collateralised in the sandbox dollar.
 *
 * A complete set -- one share of every outcome -- costs one collateral unit,
 * which goes into the market's own escrow; redeeming a set returns it. A
 * market closes at its close time, is resolved by a report its oracle signed
 * and the backend verified, may be disputed, and settles from the escrow:
 * each share of the winning outcome pays one unit and every share is retired.
 * A voided market refunds every share instead. Every figure here is the
 * backend's, in base units, and simulated.
 *
 * Trading shares on an order book belongs to the internal order book, which
 * this build does not have yet; the pages say so where a reader could
 * otherwise assume it.
 */
import { useId, type ReactNode } from "react";
import { Link, Route, Routes, useSearchParams } from "react-router-dom";

import {
  MARKET_LIST_LIMIT,
  usePredictionMarkets,
  type PredictionMarketList,
} from "../../../api/advanced/predmarket.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { formatMillis } from "../../../lib/advanced.ts";
import {
  COMPLETE_SET_NOTE,
  MARKET_STATUSES,
  ORDER_BOOK_NOTE,
  STATUS_MEANING,
  parseMarketStatus,
  statusTone,
} from "../../../lib/predmarket.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useSession } from "../../../session.tsx";
import { AdvancedNotFound, ModePill, ScrollTable } from "../shared.tsx";
import { CreateMarketPanel } from "./CreateMarket.tsx";
import { MarketDetailPage } from "./MarketDetail.tsx";
import { marketHref } from "./paths.ts";

/** The section: the list by status, and one market by id. */
export function PredictionMarketsSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<MarketList />} />
      <Route path=":marketId" element={<MarketDetailPage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}

function MarketList(): ReactNode {
  const [params, setParams] = useSearchParams();
  const filter = parseMarketStatus(params.get("status"));
  const markets = usePredictionMarkets(filter.status);
  const session = useSession();
  const operator = session.principal?.actor_type === "OPERATOR";
  const selectId = useId();

  return (
    <Page
      title="Prediction markets"
      lead="Markets on the outcome of an event, in simulated value: complete sets against the sandbox dollar, resolved by a verified oracle report and settled from each market's own escrow."
    >
      <SimulatedNotice />
      <Panel
        title="Markets"
        description="One lifecycle status at a time, soonest close first, as the backend lists them. Choosing a status asks the backend for that status; nothing is filtered in the browser."
        actions={
          <div className="filter">
            <label htmlFor={selectId}>Market status</label>
            <select
              id={selectId}
              className="input"
              value={filter.status}
              onChange={(event) => {
                setParams({ status: event.target.value });
              }}
            >
              {MARKET_STATUSES.map((status) => (
                <option key={status} value={status}>
                  {status}
                </option>
              ))}
            </select>
          </div>
        }
      >
        {filter.ignored !== undefined && (
          <p className="note" role="status">
            This address asked for the status “{filter.ignored}”, which prediction markets do not have, so the{" "}
            {filter.status} markets are listed instead.
          </p>
        )}
        <p className="note">
          <Pill tone={statusTone(filter.status)}>{filter.status}</Pill> {STATUS_MEANING[filter.status]}
        </p>
        <AsyncPanel
          query={markets}
          loadingLabel="Loading prediction markets…"
          empty={{
            isEmpty: (list: PredictionMarketList) => list.items.length === 0,
            title: `No ${filter.status} markets`,
            body: "The backend returned no markets in this status. That is its answer; nothing is listed in their place.",
          }}
        >
          {(list: PredictionMarketList) => (
            <>
              <ScrollTable
                caption="Markets in this status"
                headers={["Market", "Question", "Mode", "Outcomes", "Opens", "Closes", "Dispute window"]}
              >
                {list.items.map((market) => (
                  <tr key={market.id}>
                    <th scope="row">
                      <Link to={marketHref(market.id)}>{market.symbol}</Link>
                    </th>
                    <td className="cell-prose">{market.question}</td>
                    <td>
                      <ModePill mode={market.mode} />
                    </td>
                    <td>
                      {market.outcomes.map((outcome) => (
                        <Pill key={outcome.index} tone="neutral">
                          {outcome.symbol}
                        </Pill>
                      ))}
                    </td>
                    <td>{formatInstant(market.open_at)}</td>
                    <td>{formatInstant(market.close_at)}</td>
                    <td>{formatMillis(market.dispute_window_ms)}</td>
                  </tr>
                ))}
              </ScrollTable>
              <p className="note">
                {list.items.length === MARKET_LIST_LIMIT
                  ? `The backend returned ${String(list.items.length)} markets, the most it lists in one answer. More may exist in this status; the API has no cursor to reach them.`
                  : `${String(list.items.length)} ${list.items.length === 1 ? "market" : "markets"} returned by the backend.`}
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel title="How a market works" description="The rules the backend applies, stated once.">
        <p>{COMPLETE_SET_NOTE}</p>
        <p>{ORDER_BOOK_NOTE}</p>
      </Panel>

      {operator && <CreateMarketPanel />}
    </Page>
  );
}
