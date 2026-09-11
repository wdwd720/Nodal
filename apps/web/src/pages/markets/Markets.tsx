/**
 * MARKETS — discovery (product goal §12, §35; USER_JOURNEY §5).
 *
 * One page of `GET /v1/native-markets`, with the search, the ordering, the
 * status filter and the cursor paging the API offers, and nothing the API does
 * not offer. Three rules shape what is on it:
 *
 *   1. EVERY FIGURE IS THE BACKEND'S. The 24-hour move is `change_24h_bps` as
 *      the server computed it, not a subtraction of two prices that are also on
 *      the row; liquidity is `liquidity_credits`, not `virtual + real`. Two
 *      answers to one question is one answer too many, and the second one is
 *      always the wrong one to debug.
 *   2. NOT TRADED IS NOT UNCHANGED. `has_24h_change` is false when the market
 *      has not printed in the window, which is a different fact from having
 *      moved nothing, and the column says so rather than drawing a flat zero.
 *   3. THE ORDERING SAYS WHETHER IT CAN BE PAGED. Only NEWEST is stable; the
 *      other four rank by figures that move when somebody trades, and the
 *      response carries `stable` so the page can say so out loud instead of
 *      quietly showing a market twice.
 *
 * The table does not sort itself. `DataTable` can sort client-side, and using
 * it here would order the fifty rows this page happens to hold while claiming
 * to order the market — the ordering is the server's, and the control that
 * changes it is the one that re-asks.
 */
import { useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import {
  useNativeMarkets,
  useVersion,
  type MarketSort,
  type MarketsPage,
  type NativeMarketSummary,
} from "../../api/queries.ts";
import { Button } from "../../components/Button.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { AsyncPanel } from "../../components/DataState.tsx";
import { Figure } from "../../components/Figure.tsx";
import { FormField } from "../../components/Field.tsx";
import { Disclosure, Page, Panel, Pill } from "../../components/Layout.tsx";
import { fromBaseUnits } from "../../lib/format.ts";
import { EMPTY_STATES } from "../../lib/errors.ts";
import { CREDITS_DISCLOSURE, NATIVE_ASSET_RISK, NATIVE_PRICE_NOTE } from "../../lib/honesty.ts";
import { MarketsNav } from "./MarketsNav.tsx";
import { SORTS, STATUS_FILTERS, UNSTABLE_SORT_NOTE, statusCopy } from "./copy.ts";
import "../../styles/markets.css";

/**
 * The scale a Credit is held at.
 *
 * It is a constant because the markets projection does not carry it: the row
 * names `credit_asset_id` but not that asset's decimals, so there is nothing to
 * read it from. Six is what the rest of this application already assumes and
 * what the economy seeder writes. It is recorded as a gap rather than hidden:
 * a deployment that minted Credits at another scale would render every Credit
 * figure on this page wrong by a factor of ten to the difference.
 *
 * Note what this is NOT used for. A price is at the market's own `price_scale`
 * and an asset quantity at its own `asset_decimals`, both of which every row
 * carries. Using this for either of those was F-44.
 */
const CREDIT_DECIMALS = 6;

/** One page of results, and where the cursor stack has got to. */
interface Paging {
  /** The cursor for the page being shown. Empty for the first. */
  readonly cursor: string;
  /** The cursors of the pages walked through to get here. */
  readonly history: readonly string[];
}

const FIRST_PAGE: Paging = { cursor: "", history: [] };

/** The instant a response was read, for the panel's stamp. */
function readAt(at: number): string | undefined {
  return at === 0 ? undefined : new Date(at).toISOString();
}

export function Markets(): ReactNode {
  const [draft, setDraft] = useState("");
  const [q, setQ] = useState("");
  const [sort, setSort] = useState<MarketSort>("NEWEST");
  const [statusKey, setStatusKey] = useState("all");
  const [paging, setPaging] = useState<Paging>(FIRST_PAGE);
  const version = useVersion();

  const filter = STATUS_FILTERS.find((entry) => entry.key === statusKey) ?? STATUS_FILTERS[0];
  const markets = useNativeMarkets({
    q,
    sort,
    status: filter?.statuses ?? [],
    ...(paging.cursor === "" ? {} : { cursor: paging.cursor }),
  });

  /**
   * A sandbox tier is simulated value throughout, which is the API's own answer
   * on the portfolio (`Portfolio.temperature`) rather than something inferred
   * from an environment name. A market row additionally carries `demo`, and a
   * demo market is simulated on any tier. Absence of the version flag is NOT
   * "sandbox": an unlabelled rehearsal is bad and a real deployment labelled as
   * one is worse.
   */
  const sandbox = version.data?.sandbox_tier === true;

  // Changing what is being asked for always returns to the first page. A cursor
  // is a position in one ordering of one filter; carrying it across a change
  // would page through a list nobody asked for.
  const reset = (): void => {
    setPaging(FIRST_PAGE);
  };

  return (
    <Page
      title="Markets"
      lead="Assets other people created, priced by a formula against a shared pool of Credits."
    >
      <MarketsNav />

      <Panel
        title="Internal markets"
        description="Every market the backend returned for this search, in the ordering you chose."
        temp={sandbox ? "simulated" : "economy"}
        asOf={readAt(markets.dataUpdatedAt)}
      >
        <form
          className="market-filters"
          onSubmit={(event) => {
            event.preventDefault();
            setQ(draft.trim());
            reset();
          }}
        >
          <FormField
            label="Search"
            hint="Matches a name, a symbol or a description. The backend searches; this page does not."
          >
            {(field) => (
              <input
                {...field}
                type="search"
                value={draft}
                maxLength={128}
                placeholder="name, symbol or description"
                onChange={(event) => {
                  setDraft(event.target.value);
                }}
              />
            )}
          </FormField>

          <FormField label="Order by">
            {(field) => (
              <select
                {...field}
                value={sort}
                onChange={(event) => {
                  setSort(event.target.value as MarketSort);
                  reset();
                }}
              >
                {SORTS.map((entry) => (
                  <option key={entry.value} value={entry.value}>
                    {entry.label}
                  </option>
                ))}
              </select>
            )}
          </FormField>

          <FormField label="Status">
            {(field) => (
              <select
                {...field}
                value={statusKey}
                onChange={(event) => {
                  setStatusKey(event.target.value);
                  reset();
                }}
              >
                {STATUS_FILTERS.map((entry) => (
                  <option key={entry.key} value={entry.key}>
                    {entry.label}
                  </option>
                ))}
              </select>
            )}
          </FormField>

          <Button variant="secondary" submit busy={markets.isFetching} busyLabel="Searching…">
            Search
          </Button>
        </form>

        <AsyncPanel
          query={markets}
          loadingLabel="Asking the backend which internal markets exist…"
          skeleton={<TableSkeleton />}
          empty={{
            isEmpty: (page: MarketsPage) => page.markets.length === 0,
            title: q === "" ? EMPTY_STATES.markets.title : "Nothing matched that search",
            body:
              q === ""
                ? EMPTY_STATES.markets.body
                : "The backend found no market whose name, symbol or description matches. That is the " +
                  "catalogue answering, not a page still loading.",
          }}
        >
          {(page: MarketsPage) => (
            <MarketList page={page} paging={paging} onPage={setPaging} sandbox={sandbox} />
          )}
        </AsyncPanel>
      </Panel>

      <Disclosure title="How these prices work">
        <p>{NATIVE_PRICE_NOTE}</p>
        <p>{CREDITS_DISCLOSURE}</p>
      </Disclosure>

      <Disclosure title="What you are looking at">
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>
    </Page>
  );
}

function TableSkeleton(): ReactNode {
  // Shape-accurate rather than a spinner: the geometry of the table that is
  // coming, so the page does not jump when it arrives.
  return (
    <div className="skeleton-set" role="status" aria-live="polite" aria-busy="true">
      <span className="visually-hidden">The markets list is loading</span>
      {["a", "b", "c", "d", "e", "f"].map((key) => (
        <span key={key} className="skeleton skeleton-row" aria-hidden="true" />
      ))}
    </div>
  );
}

/** The 24-hour move, or the fact that there was no trade to measure one from. */
function Change(props: { readonly market: NativeMarketSummary }): ReactNode {
  const { market } = props;
  if (market.has_24h_change !== true || market.change_24h_bps === undefined) {
    return (
      <span className="absent" title="This market has not printed a trade in the last 24 hours.">
        not traded
      </span>
    );
  }
  // Basis points are a percentage with the point moved two places, which
  // `fromBaseUnits` does by padding and slicing digits. The percentage is then
  // formatted by `Figure`, which puts the sign glyph in front of the colour.
  return (
    <Figure kind="percent" value={{ decimal: fromBaseUnits(String(market.change_24h_bps), 2) }} signed />
  );
}

function MarketList(props: {
  readonly page: MarketsPage;
  readonly paging: Paging;
  readonly onPage: (paging: Paging) => void;
  readonly sandbox: boolean;
}): ReactNode {
  const navigate = useNavigate();
  const { page } = props;

  const columns: ReadonlyArray<Column<NativeMarketSummary>> = [
    {
      key: "market",
      header: "Market",
      cell: (market) => (
        <>
          <strong>{market.symbol}</strong>
          <p className="field-note">{market.name}</p>
          {market.demo && (
            <Pill tone="warn" title="Created by the sandbox demo seeder. It represents nothing.">
              Demo
            </Pill>
          )}
        </>
      ),
    },
    {
      key: "price",
      header: "Last price",
      numeric: true,
      // A price goes through the money ladder rather than being written out:
      // at a price scale of eighteen the exact fraction is twenty digits wide
      // and would push this column past a phone. `Figure` applies the ladder to
      // a DECIMAL value, so the point is moved by `format.ts` first.
      cell: (market) => (
        <Figure
          kind="money"
          value={{ decimal: fromBaseUnits(market.last_price, market.price_scale) }}
          symbol="Credits"
        />
      ),
    },
    {
      key: "change",
      header: "24h",
      numeric: true,
      cell: (market) => <Change market={market} />,
    },
    {
      key: "volume",
      header: "Volume 24h",
      numeric: true,
      cell: (market) => (
        <Figure
          kind="money"
          value={{ base: market.credit_volume_24h, scale: CREDIT_DECIMALS }}
          symbol="Credits"
          compact
        />
      ),
    },
    {
      key: "liquidity",
      header: "Liquidity",
      numeric: true,
      // Liquidity is the depth the curve prices against, and most of it is
      // usually virtual. Naming it a risk measure puts it in the caption a
      // screen reader hears before the rows.
      riskMeasure: true,
      cell: (market) => (
        <Figure
          kind="money"
          value={{ base: market.liquidity_credits, scale: CREDIT_DECIMALS }}
          symbol="Credits"
          compact
        />
      ),
    },
    {
      key: "trades",
      header: "Trades 24h",
      numeric: true,
      cell: (market) => <Figure kind="count" count={market.trades_24h} />,
    },
    {
      key: "status",
      header: "Status",
      cell: (market) => {
        const copy = statusCopy(market.market_status);
        return (
          <>
            <Pill tone={copy.tone} title={copy.text}>
              {market.market_status}
            </Pill>
            <p className="field-note">{copy.text}</p>
          </>
        );
      },
    },
  ];

  return (
    <>
      <DataTable
        caption="Internal markets, with the last price, the 24-hour move, the volume and the liquidity behind each one"
        columns={columns}
        rows={page.markets}
        rowKey={(market) => market.market_id}
        onOpenRow={(market) => {
          void navigate(`/markets/${market.market_id}`);
        }}
        tall={page.markets.length > 12}
      />

      {!page.stable && <p className="field-note">{UNSTABLE_SORT_NOTE}</p>}

      {props.sandbox && (
        <p className="field-note">
          This deployment is a sandbox tier, so every figure on this page is simulated. Nothing here
          is anybody&apos;s money and no trade on it moves value anywhere.
        </p>
      )}

      <div className="form-actions">
        {props.paging.history.length === 0 ? (
          <Button variant="quiet" disabledReason="This is the first page of results.">
            Previous page
          </Button>
        ) : (
          <Button
            variant="quiet"
            onClick={() => {
              const history = props.paging.history.slice(0, props.paging.history.length - 1);
              const previous = props.paging.history[props.paging.history.length - 1] ?? "";
              props.onPage({ cursor: previous, history });
            }}
          >
            Previous page
          </Button>
        )}
        {page.nextCursor === null ? (
          <Button variant="quiet" disabledReason="The backend returned no cursor, so this is the last page.">
            Next page
          </Button>
        ) : (
          <Button
            variant="quiet"
            onClick={() => {
              props.onPage({
                cursor: page.nextCursor ?? "",
                history: [...props.paging.history, props.paging.cursor],
              });
            }}
          >
            Next page
          </Button>
        )}
      </div>
    </>
  );
}
