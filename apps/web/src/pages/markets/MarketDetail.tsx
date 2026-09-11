/**
 * ASSET DETAIL / TRADING SCREEN (product goal §13, §14, §47; USER_JOURNEY §5).
 *
 * Everything a buyer needs in order to decide, and the ticket that acts on it,
 * from one projection: `GET /v1/native-markets/{id}/summary` is the same row
 * the markets list reads, so this screen and that one cannot disagree about a
 * price or a volume.
 *
 * The four things this page shows that a market screen is usually shy about:
 *
 *   - WHAT BACKS THE PRICE. `real_credit_reserve` is the only Credits in the
 *     pool that could ever be paid out; the virtual reserve is part of the
 *     pricing formula and no Credits sit behind it. They are drawn as two
 *     segments of one bar rather than added into a single "liquidity" figure,
 *     because a buyer reading one number is reading the wrong one.
 *   - WHO HOLDS IT. Concentration is the number a buyer most needs and the one
 *     a listing page never volunteers. The share is exact integer arithmetic on
 *     base units, truncated, so a holder can never appear to hold less.
 *   - WHAT THE LIMITS ARE. Goal §47's limits in force, rendered exactly as the
 *     backend reports them — including a circuit breaker that is DISARMED,
 *     which arrives as a literal zero and is not the same claim as a breaker
 *     that trips on no movement at all.
 *   - WHAT HISTORY THERE IS. A period with no trades has no candle, so the
 *     chart draws a gap rather than a flat price nobody paid, and a market with
 *     no history at all says so instead of drawing a line at one price.
 *
 * The chart's geometry is `src/charts/`, the one directory allowed to turn an
 * exact figure into a number. Every LABEL on it is formatted here, by
 * `lib/format.ts`, and passed in as an opaque string — the plot never sees a
 * scale and never composes a figure.
 */
import { useEffect, useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";

import {
  useNativeCandles,
  useNativeMarketDetail,
  useNativeTrades,
  useVersion,
  type MarketDetail as MarketDetailData,
  type MarketSafetyLimits,
  type NativeCandlePage,
  type NativeMarketSummary,
  type NativeTradePage,
  type NativeTradePrint,
} from "../../api/queries.ts";
import { CandleChart, type PriceTick, type TimeTick } from "../../charts/CandleChart.tsx";
import { candleGeometry, summariseRange, type CandleInput } from "../../charts/geometry.ts";
import { Button } from "../../components/Button.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { Dialog } from "../../components/Dialog.tsx";
import { Figure } from "../../components/Figure.tsx";
import { SegmentedBar } from "../../components/SegmentedBar.tsx";
import { invalidateScopes } from "../../components/StreamStatus.tsx";
import {
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  IdentifierShort,
  Page,
  Panel,
  Pill,
} from "../../components/Layout.tsx";
import { figureText, formatCount, formatUnits, fromBaseUnits } from "../../lib/format.ts";
import { formatInstant } from "../../lib/time.ts";
import { NATIVE_ASSET_RISK, NATIVE_PRICE_NOTE, CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { useActiveAccountId } from "../../session.tsx";
import { Ticket } from "./Ticket.tsx";
import {
  ASSET_STATUS,
  CANDLE_GAPS_NOTE,
  CHART_RANGES,
  DEMO_MARKET_NOTE,
  LIMITS_NOTE,
  MODERATION_STATE,
  chartWindow,
  statusCopy,
} from "./copy.ts";
import "../../styles/markets.css";
import { CREDIT_DECIMALS } from "../../lib/credits.ts";

/** The plot, in viewBox units. It scales to whatever column it lands in. */
const PLOT = { width: 720, height: 240, volumeHeight: 60 };

/**
 * A price, as the value the money ladder should be applied to.
 *
 * A price on an internal market is an integer at `price_scale`, which is
 * eighteen, so writing it out in full is a zero, a point, four zeros and
 * fourteen more digits. At the emphasised size that is over three hundred
 * pixels of digits, and it pushed the trade screen sideways on a phone. `Figure` applies section 6's ladder -- four significant digits,
 * with a leading-zero run in a smaller digit -- only to a DECIMAL value, so the
 * point is moved here, by `format.ts`, and the ladder does the rest.
 *
 * Quantities do not go through this. An asset amount is exact and is rendered
 * exact, because a truncated holding is a different claim from a shortened
 * price.
 */
function price(baseUnits: string, scale: number): { readonly decimal: string } {
  return { decimal: fromBaseUnits(baseUnits, scale) };
}

const WIDE = "(min-width: 768px)";

/** Tracks a media query. The ticket is one shape or the other, never both. */
function useWide(): boolean {
  const [wide, setWide] = useState<boolean>(() => window.matchMedia(WIDE).matches);
  useEffect(() => {
    const list = window.matchMedia(WIDE);
    const onChange = (): void => {
      setWide(list.matches);
    };
    onChange();
    list.addEventListener("change", onChange);
    return () => {
      list.removeEventListener("change", onChange);
    };
  }, []);
  return wide;
}

function readAt(at: number): string | undefined {
  return at === 0 ? undefined : new Date(at).toISOString();
}

export function MarketDetail(): ReactNode {
  const params = useParams<{ marketId: string }>();
  const marketId = params.marketId ?? "";
  const version = useVersion();
  const accountId = useActiveAccountId();
  // The account is sent so the holder list can mark this person's own row. It
  // is not a filter: the list is the same list for everybody (D-111).
  const detail = useNativeMarketDetail(marketId, accountId);
  const queryClient = useQueryClient();
  const wide = useWide();
  const [ticketOpen, setTicketOpen] = useState(false);

  const sandbox = version.data?.sandbox_tier === true;
  const market = detail.data?.market;
  const simulated = sandbox || market?.demo === true;

  const afterFill = (): void => {
    // A fill must not depend on the event stream being up: the position, the
    // balance and the tape are all things this screen just changed, so it asks
    // for them again itself rather than waiting to be told.
    //
    // WHICH reads those are is the stream's map, imported rather than
    // reproduced. This page used to carry its own four-line copy, and that copy
    // was the only thing making a fill refresh the portfolio at all — the map
    // was missing the key and nobody noticed, because the workaround hid it
    // exactly where a reviewer would look (F-202, D-112).
    invalidateScopes(queryClient, "balance", "position");
  };

  return (
    <Page
      title={market === undefined ? "Market" : `${market.symbol} — ${market.name}`}
      lead={
        market === undefined
          ? "One internal market, its history and its limits."
          : `An internal market created by a user, priced by a formula against a shared pool of Credits.`
      }
    >
      <AsyncPanel
        query={detail}
        loadingLabel="Asking the backend about this market…"
        skeleton={
          <div className="skeleton-set" role="status" aria-live="polite" aria-busy="true">
            <span className="visually-hidden">This market is loading</span>
            <span className="skeleton skeleton-block" aria-hidden="true" />
          </div>
        }
      >
        {(data: MarketDetailData) => (
          <>
            {data.market.demo && (
              <div className="banner banner-warn" role="status">
                <strong>Demo market.</strong> {DEMO_MARKET_NOTE}
              </div>
            )}

            <div className={wide ? "market-columns" : "stack"}>
              <div className="stack">
                <Overview
                  market={data.market}
                  creatorAccountId={data.creatorAccountId}
                  simulated={simulated}
                  asOf={readAt(detail.dataUpdatedAt)}
                />
                <ChartPanel market={data.market} simulated={simulated} />
                <Reserves market={data.market} simulated={simulated} />
                <Limits limits={data.limits} />
                <Tape marketId={marketId} simulated={simulated} />
                <Holders market={data.market} holders={data.topHolders} simulated={simulated} />
                <About market={data.market} />
              </div>

              {wide && (
                <div className="market-ticket-column">
                  <Ticket
                    market={data.market}
                    accountId={accountId}
                    onFilled={afterFill}
                    simulated={simulated}
                  />
                </div>
              )}
            </div>

            {!wide && (
              <>
                <div className="ticket-dock">
                  <span>
                    <span className="eyebrow">Last price</span>
                    <br />
                    <Figure
                      kind="money"
                      value={price(data.market.last_price, data.market.price_scale)}
                      symbol="Credits"
                    />
                  </span>
                  <Button
                    variant="primary"
                    onClick={() => {
                      setTicketOpen(true);
                    }}
                  >
                    Buy or sell
                  </Button>
                </div>
                <Dialog
                  open={ticketOpen}
                  onClose={() => {
                    setTicketOpen(false);
                  }}
                  title={`Trade ${data.market.symbol}`}
                >
                  <Ticket
                    market={data.market}
                    accountId={accountId}
                    onFilled={afterFill}
                    simulated={simulated}
                  />
                </Dialog>
              </>
            )}
          </>
        )}
      </AsyncPanel>
    </Page>
  );
}

/* -------------------------------------------------------------------------- *
 * The header: what this is, who made it, and what it last traded at.
 * -------------------------------------------------------------------------- */

function Overview(props: {
  readonly market: NativeMarketSummary;
  /**
   * The creator, which is a field of the DETAIL response and not of the summary
   * the public markets list serves: an account id on an unauthenticated page is
   * both readable and enumerable (D-110). Absent renders no row rather than a
   * blank one, because "we are not saying" and "nobody" are different facts.
   */
  readonly creatorAccountId: string | undefined;
  readonly simulated: boolean;
  readonly asOf: string | undefined;
}): ReactNode {
  const { market } = props;
  const status = statusCopy(market.market_status);
  const asset = ASSET_STATUS[market.asset_status];
  const moderation =
    market.moderation_state === undefined ? undefined : MODERATION_STATE[market.moderation_state];

  return (
    <Panel
      title="This market"
      description="The figures the markets page shows, from the same projection, so the two cannot disagree."
      temp={props.simulated ? "simulated" : "economy"}
      asOf={props.asOf}
      actions={
        <Pill tone={status.tone} title={status.text}>
          {market.market_status}
        </Pill>
      }
    >
      <FieldGrid columns={3}>
        <Field
          label="Last price"
          note="The marginal price: what the next base unit costs, in Credits, at this market's own scale."
          emphasis
        >
          <Figure
            kind="money"
            value={price(market.last_price, market.price_scale)}
            symbol="Credits"
            big
          />
        </Field>
        <Field
          label="24 hours"
          note={
            market.has_24h_change === true
              ? "The backend's signed move over the window."
              : "No trade printed in the window, which is not the same as having moved nothing."
          }
        >
          {market.has_24h_change === true && market.change_24h_bps !== undefined ? (
            <Figure kind="bps" bps={market.change_24h_bps} signed />
          ) : (
            <span className="absent">not traded</span>
          )}
        </Field>
        <Field label="Volume, 24 hours">
          <Figure
            kind="money"
            value={{ base: market.credit_volume_24h, scale: CREDIT_DECIMALS }}
            symbol="Credits"
          />
        </Field>
        <Field label="Trades, 24 hours" note="A count is never abbreviated.">
          <Figure kind="count" count={market.trades_24h} />
        </Field>
        <Field label="Market status" note={status.text}>
          <Pill tone={status.tone}>{market.market_status}</Pill>
        </Field>
        <Field
          label="Asset status"
          note={asset?.text ?? "The backend reported a status this build has no words for."}
        >
          <Pill tone={asset?.tone ?? "neutral"}>{market.asset_status}</Pill>
        </Field>
        {moderation !== undefined && (
          <Field
            label="Moderation"
            note={`${moderation.text} A moderation decision is about the content and does not by itself open or close the market.`}
          >
            <Pill tone={moderation.tone}>{market.moderation_state}</Pill>
          </Field>
        )}
        {props.creatorAccountId !== undefined && (
          <Field
            label="Created by"
            note="The creator's account. A display name belongs to the profile domain and is not joined here."
          >
            <IdentifierShort value={props.creatorAccountId} what="creator account id" />
          </Field>
        )}
        <Field label="Created">{formatInstant(market.created_at)}</Field>
        {market.activated_at !== undefined && (
          <Field label="Opened for trading">{formatInstant(market.activated_at)}</Field>
        )}
        <Field label="Platform fee" note="Taken on every trade.">
          <Figure kind="bps" bps={market.platform_fee_bps} />
        </Field>
        <Field label="Creator fee" note="Paid to whoever created this asset, on every trade.">
          <Figure kind="bps" bps={market.creator_fee_bps} />
        </Field>
      </FieldGrid>
    </Panel>
  );
}

/* -------------------------------------------------------------------------- *
 * The chart.
 * -------------------------------------------------------------------------- */

/** A short axis label from an RFC 3339 instant, by slicing rather than parsing. */
function axisTime(at: string, daily: boolean): string {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(at)) return at;
  return daily ? at.slice(0, 10) : `${at.slice(11, 16)}Z`;
}

function ChartPanel(props: {
  readonly market: NativeMarketSummary;
  readonly simulated: boolean;
}): ReactNode {
  const { market } = props;
  const [rangeKey, setRangeKey] = useState("1w");
  const range = CHART_RANGES.find((entry) => entry.key === rangeKey) ?? CHART_RANGES[3];
  // Aligned to a bucket boundary, so the window — and therefore the query key —
  // only moves when a bucket closes, which is the only moment a new bucket can
  // exist. An unaligned edge would change on every render and never resolve.
  const bounds =
    range === undefined
      ? { from: market.created_at, to: market.created_at }
      : chartWindow(range, market.created_at, Date.now());
  const candles = useNativeCandles(
    market.market_id,
    range?.interval ?? "1d",
    bounds.from,
    bounds.to,
  );

  return (
    <Panel
      title="Price history"
      description="Computed from this market's own prints. Nothing here is simulated or filled forward."
      temp={props.simulated ? "simulated" : "economy"}
    >
      <fieldset className="ticket-tolerance">
        <legend>Window</legend>
        {CHART_RANGES.map((entry) => (
          <label key={entry.key}>
            <input
              type="radio"
              name={`range-${market.market_id}`}
              value={entry.key}
              checked={rangeKey === entry.key}
              onChange={() => {
                setRangeKey(entry.key);
              }}
            />
            <span>{entry.label}</span>
          </label>
        ))}
      </fieldset>

      <AsyncPanel
        query={candles}
        loadingLabel="Asking the backend for this market's history…"
        skeleton={
          <div className="skeleton-set" role="status" aria-live="polite" aria-busy="true">
            <span className="visually-hidden">The price history is loading</span>
            <span className="skeleton skeleton-block" aria-hidden="true" />
          </div>
        }
        empty={{
          isEmpty: (page: NativeCandlePage) => page.candles.length === 0,
          title: "No trades in this window",
          body:
            "This market printed nothing over the period selected, so there is no candle to draw. " +
            "A wider window may reach the trades it has.",
        }}
      >
        {(page: NativeCandlePage) => (
          <Chart page={page} symbol={market.symbol} daily={range?.interval === "1d"} />
        )}
      </AsyncPanel>

      <p className="field-note">{CANDLE_GAPS_NOTE}</p>
    </Panel>
  );
}

function Chart(props: {
  readonly page: NativeCandlePage;
  readonly symbol: string;
  readonly daily: boolean;
}): ReactNode {
  const { page } = props;
  const scale = page.price_scale;

  const inputs: CandleInput[] = page.candles.map((candle) => ({
    openTime: candle.open_time,
    open: candle.open,
    high: candle.high,
    low: candle.low,
    close: candle.close,
    creditVolume: candle.credit_volume,
    trades: candle.trades,
  }));

  const geometry = candleGeometry(inputs, PLOT);
  const summary = summariseRange(inputs);

  // Every label is produced HERE, outside `src/charts/`. The plot positions
  // them and never sees a scale, so it cannot compose a figure of its own.
  //
  // The axis labels are rendered ELEMENTS rather than strings: a price at scale
  // eighteen only fits an axis in the ladder's leading-zero notation, and that
  // notation is markup -- `figureText` collapses it into digits that read as a
  // different number. Prose gets the exact written-out form instead, below,
  // where there is room for it and no notation to misread.
  const exact = (baseUnits: string): string => figureText(formatUnits(baseUnits, scale));

  const priceTicks: PriceTick[] = geometry.grid.map((line) => ({
    key: line.baseUnits,
    y: line.y,
    label: <Figure kind="money" value={price(line.baseUnits, scale)} compact />,
  }));

  const last = geometry.candles.length - 1;
  const middle = last > 1 ? (last - (last % 2)) / 2 : 0;
  const timeTicks: TimeTick[] = [];
  for (const index of last <= 0 ? [0] : [0, middle, last]) {
    const placed = geometry.candles[index];
    const source = inputs[index];
    if (placed === undefined || source === undefined) continue;
    if (timeTicks.some((tick) => tick.x === placed.x)) continue;
    timeTicks.push({
      key: source.openTime,
      x: placed.x,
      label: axisTime(source.openTime, props.daily),
    });
  }

  const readouts = inputs.map(
    (candle) =>
      `${formatInstant(candle.openTime)} — open ${exact(candle.open)}, high ${exact(candle.high)}, ` +
      `low ${exact(candle.low)}, close ${exact(candle.close)} Credits; volume ` +
      `${figureText(formatUnits(candle.creditVolume, CREDIT_DECIMALS, { compact: true }))} Credits ` +
      `over ${figureText(formatCount(candle.trades))} trades.`,
  );

  const sentence =
    summary === undefined
      ? "There is nothing in this window to summarise."
      : `${figureText(formatCount(inputs.length))} periods, from ${formatInstant(summary.firstOpenTime)} ` +
        `to ${formatInstant(summary.lastOpenTime)}. Open ${exact(summary.open)}, high ` +
        `${exact(summary.high)}, low ${exact(summary.low)}, close ${exact(summary.close)} Credits. ` +
        `Volume ${figureText(formatUnits(summary.creditVolume, CREDIT_DECIMALS, { compact: true }))} ` +
        `Credits over ${figureText(formatCount(summary.trades))} trades.`;

  return (
    <CandleChart
      label={`${props.symbol} price and volume, ${page.interval} periods`}
      summary={sentence}
      readouts={readouts}
      geometry={geometry}
      priceTicks={priceTicks}
      timeTicks={timeTicks}
    />
  );
}

/* -------------------------------------------------------------------------- *
 * Reserves and supply.
 * -------------------------------------------------------------------------- */

function Reserves(props: {
  readonly market: NativeMarketSummary;
  readonly simulated: boolean;
}): ReactNode {
  const { market } = props;
  return (
    <Panel
      title="What backs the price"
      description="The pool the curve prices against, and how much of it is Credits somebody actually paid in."
      temp={props.simulated ? "simulated" : "economy"}
    >
      <SegmentedBar
        caption="The pool behind this market"
        scale={CREDIT_DECIMALS}
        symbol="Credits"
        total={market.liquidity_credits}
        segments={[
          {
            key: "real",
            label: "Real Credits",
            baseUnits: market.real_credit_reserve,
            texture: "solid",
            explanation:
              "The only Credits in this pool that could ever be paid out. Everything a seller receives comes from here.",
          },
          {
            key: "virtual",
            label: "Virtual",
            baseUnits: market.virtual_credit_reserve,
            texture: "hatch",
            explanation:
              "Part of the pricing formula. No Credits sit behind it and none of it can be withdrawn by anyone.",
          },
        ]}
      />

      <FieldGrid columns={3}>
        <Field label="Liquidity" note="Virtual plus real: the depth the curve prices against.">
          <Figure
            kind="money"
            value={{ base: market.liquidity_credits, scale: CREDIT_DECIMALS }}
            symbol="Credits"
          />
        </Field>
        <Field label="Unsold supply" note="Still held by the curve, at the asset's own precision.">
          <Figure
            kind="units"
            value={{ base: market.asset_reserve, scale: market.asset_decimals }}
            symbol={market.symbol}
          />
        </Field>
        <Field label="In circulation" note="Everything the curve has sold, plus any creator allocation.">
          <Figure
            kind="units"
            value={{ base: market.circulating_supply, scale: market.asset_decimals }}
            symbol={market.symbol}
          />
        </Field>
        <Field label="Total supply" note="Fixed at creation. It can never rise.">
          <Figure
            kind="units"
            value={{ base: market.max_supply, scale: market.asset_decimals }}
            symbol={market.symbol}
          />
        </Field>
        <Field label="Decimal places" note="This asset's own precision, chosen by its creator.">
          <Figure kind="count" count={market.asset_decimals} />
        </Field>
        {market.state_version !== undefined && (
          <Field
            label="State version"
            note="Moves once per trade. A quote priced against an older one is re-priced, not honoured."
          >
            <Figure kind="count" count={market.state_version} />
          </Field>
        )}
      </FieldGrid>
    </Panel>
  );
}

/* -------------------------------------------------------------------------- *
 * The limits in force (goal §47).
 * -------------------------------------------------------------------------- */

/**
 * The circuit breaker, which is the one field on this panel where zero is not
 * a limit.
 *
 * The compiled-in safety policy ships with the breaker DISARMED (D-065), and a
 * disarmed breaker arrives as a literal `0` rather than as an absent field —
 * the server distinguishes "unset", which it refuses, from "zero", which is a
 * decision. So zero is rendered as "not armed" and never as a threshold of
 * nothing, which would read as the strictest breaker possible when the truth is
 * that there is no breaker. An absent field is a third thing again: the backend
 * did not say.
 */
function Breaker(props: { readonly limits: MarketSafetyLimits }): ReactNode {
  const move = props.limits.circuit_breaker_move_bps;
  const window = props.limits.circuit_breaker_window_seconds;

  if (move === undefined) {
    return (
      <Field
        label="Circuit breaker"
        note="The backend did not report a breaker setting for this market."
      >
        <span className="absent">not reported</span>
      </Field>
    );
  }

  if (move === 0) {
    return (
      <Field
        label="Circuit breaker"
        note="No breaker is armed on this market. A price move of any size will not pause trading by itself."
      >
        <Pill tone="warn">Not armed</Pill>
      </Field>
    );
  }

  return (
    <Field
      label="Circuit breaker"
      note={
        window === undefined
          ? "A move past this pauses trading to sell-only."
          : `A move past this within ${String(window)} seconds pauses trading to sell-only.`
      }
    >
      <Figure kind="bps" bps={move} />
    </Field>
  );
}

function Limits(props: { readonly limits: MarketSafetyLimits }): ReactNode {
  const { limits } = props;
  const risk = limits.risk_policy_version;

  return (
    <Panel
      title="Limits in force"
      description="What this venue and the risk policy will refuse, as they stand right now."
    >
      <FieldGrid columns={3}>
        <Field
          label="Maximum price impact"
          note="An order that would move the price further than this is refused."
        >
          <Figure kind="bps" bps={limits.max_price_impact_bps} absent="not reported" />
        </Field>
        <Field
          label="Maximum slippage"
          note="An order that would fill further than this from the price on screen is refused."
        >
          <Figure kind="bps" bps={limits.max_slippage_bps} absent="not reported" />
        </Field>
        <Breaker limits={limits} />
        <Field
          label="Minimum opening liquidity"
          note="What a new market must start with before it may open at all."
        >
          <Figure
            kind="money"
            value={
              limits.min_opening_liquidity_credits === undefined
                ? null
                : { base: limits.min_opening_liquidity_credits, scale: CREDIT_DECIMALS }
            }
            symbol="Credits"
            absent="not reported"
          />
        </Field>
        <Field
          label="Creator may buy their own asset"
          note="Self-dealing is a market-safety decision, not a courtesy, and the venue records which way it went."
        >
          {limits.creator_may_buy_own_asset === undefined ? (
            <span className="absent">not reported</span>
          ) : (
            <Pill tone={limits.creator_may_buy_own_asset ? "warn" : "good"}>
              {limits.creator_may_buy_own_asset ? "Permitted" : "Refused"}
            </Pill>
          )}
        </Field>
        <Field label="Market-safety policy">
          <Identifier value={limits.safety_policy_version} copyable={false} />
        </Field>
        <Field
          label="Position concentration"
          note="How much of one market a single account may hold, from the risk policy."
        >
          <Figure
            kind="bps"
            bps={limits.max_native_market_concentration_bps}
            absent={risk === undefined ? "the risk policy is not recorded" : "not reported"}
          />
        </Field>
        <Field label="Creator concentration" note="How much of their own asset a creator may hold.">
          <Figure
            kind="bps"
            bps={limits.max_creator_concentration_bps}
            absent={risk === undefined ? "the risk policy is not recorded" : "not reported"}
          />
        </Field>
        <Field label="Risk policy">
          {risk === undefined ? (
            <span className="absent">none recorded</span>
          ) : (
            <Identifier value={risk} copyable={false} />
          )}
        </Field>
      </FieldGrid>
      <p className="field-note">{LIMITS_NOTE}</p>
    </Panel>
  );
}

/* -------------------------------------------------------------------------- *
 * The tape.
 * -------------------------------------------------------------------------- */

function Tape(props: { readonly marketId: string; readonly simulated: boolean }): ReactNode {
  const trades = useNativeTrades(props.marketId);

  return (
    <Panel
      title="Recent trades"
      description="Every print this market has made, newest first. No account identity appears here, by the API's design."
      temp={props.simulated ? "simulated" : "economy"}
      asOf={readAt(trades.dataUpdatedAt)}
    >
      <AsyncPanel
        query={trades}
        loadingLabel="Asking the backend for this market's tape…"
        skeleton={
          <div className="skeleton-set" role="status" aria-live="polite" aria-busy="true">
            <span className="visually-hidden">The tape is loading</span>
            {["a", "b", "c", "d"].map((key) => (
              <span key={key} className="skeleton skeleton-row" aria-hidden="true" />
            ))}
          </div>
        }
        empty={{
          isEmpty: (page: NativeTradePage) => page.trades.length === 0,
          title: "Nothing has traded yet",
          body: "No print has been made against this market. The first trade will appear here.",
        }}
      >
        {(page: NativeTradePage) => {
          const columns: ReadonlyArray<Column<NativeTradePrint>> = [
            {
              key: "time",
              header: "When",
              cell: (print) => <span className="mono-small">{formatInstant(print.printed_at)}</span>,
            },
            {
              key: "side",
              header: "Side",
              cell: (print) => (
                <Pill tone={print.side === "BUY" ? "good" : "neutral"}>{print.side}</Pill>
              ),
            },
            {
              key: "price",
              header: "Price",
              numeric: true,
              cell: (print) => (
                <Figure
                  kind="money"
                  value={price(print.effective_price, page.price_scale)}
                  symbol="Credits"
                />
              ),
            },
            {
              key: "units",
              header: "Units",
              numeric: true,
              cell: (print) => (
                <Figure
                  kind="units"
                  value={{ base: print.asset_volume, scale: page.asset_decimals }}
                />
              ),
            },
            {
              key: "credits",
              header: "Credits",
              numeric: true,
              cell: (print) => (
                <Figure
                  kind="money"
                  value={{ base: print.credit_volume, scale: CREDIT_DECIMALS }}
                  symbol="Credits"
                />
              ),
            },
            {
              key: "after",
              header: "Price after",
              numeric: true,
              cell: (print) => (
                <Figure
                  kind="money"
                  value={price(print.spot_price_after, page.price_scale)}
                  symbol="Credits"
                />
              ),
            },
          ];
          return (
            <DataTable
              caption="Recent prints against this market, newest first, with the price each one left behind"
              columns={columns}
              rows={page.trades}
              rowKey={(print) => String(print.seq)}
              tall={page.trades.length > 10}
            />
          );
        }}
      </AsyncPanel>
    </Panel>
  );
}

/* -------------------------------------------------------------------------- *
 * Concentration.
 * -------------------------------------------------------------------------- */

/**
 * One place in the concentration, which is all the backend says.
 *
 * There is no account here and there is not meant to be: the holder list used
 * to hand every signed-in caller each top holder's account id, which made a
 * market's largest positions readable by account and watchable trade by trade
 * (D-111). The caller's own row is marked, and that is the whole of what this
 * page knows about a person.
 */
interface Holder {
  readonly rank: number;
  readonly quantity: string;
  readonly share_bps: number;
  readonly is_you?: boolean;
}

function Holders(props: {
  readonly market: NativeMarketSummary;
  readonly holders: readonly Holder[];
  readonly simulated: boolean;
}): ReactNode {
  const { market } = props;

  if (props.holders.length === 0) {
    return (
      <Panel title="Who holds it" temp={props.simulated ? "simulated" : "economy"}>
        <EmptyState
          title="Nobody holds any of it"
          body="The backend reported no holders for this market, so all of the supply is still in the curve."
        />
      </Panel>
    );
  }

  const columns: ReadonlyArray<Column<Holder>> = [
    {
      key: "rank",
      header: "Place",
      cell: (holder) => (
        <span>
          #{holder.rank}
          {holder.is_you === true && <span className="holder-you"> you</span>}
        </span>
      ),
    },
    {
      key: "quantity",
      header: "Holding",
      numeric: true,
      cell: (holder) => (
        <Figure
          kind="units"
          value={{ base: holder.quantity, scale: market.asset_decimals }}
          symbol={market.symbol}
        />
      ),
    },
    {
      key: "share",
      header: "Share of the units accounts hold",
      numeric: true,
      riskMeasure: true,
      // The backend's figure, truncated there on exact integers. Recomputing it
      // here would need a denominator this page does not have: units still in
      // the curve are held by nobody, and a creator's allocation is minted
      // outside it, so a share of the circulating supply can exceed 100%.
      cell: (holder) => <Figure kind="bps" bps={holder.share_bps} />,
    },
  ];

  return (
    <Panel
      title="Who holds it"
      description="Concentration, which is the figure a buyer most needs and the one a listing rarely volunteers."
      temp={props.simulated ? "simulated" : "economy"}
    >
      <DataTable
        caption="The largest holdings of this asset and the share of held units each one is"
        columns={columns}
        rows={props.holders}
        rowKey={(holder) => String(holder.rank)}
      />
      <p className="field-note">
        The backend returns the largest holdings it records, not every one, and it does not say
        whose: concentration is the figure a buyer needs, and who occupies each place is somebody
        else&rsquo;s position. A share is of the units accounts hold, not of the total that will
        ever exist.
      </p>
    </Panel>
  );
}

/* -------------------------------------------------------------------------- *
 * Provenance and risk.
 * -------------------------------------------------------------------------- */

function About(props: { readonly market: NativeMarketSummary }): ReactNode {
  const { market } = props;
  return (
    <Panel title="About this asset" description="What its creator wrote, and what Nodal did not do.">
      {market.description !== undefined && market.description !== "" ? (
        <p>{market.description}</p>
      ) : (
        <p className="field-note">Its creator wrote no description.</p>
      )}

      <FieldGrid columns={2}>
        <Field label="Asset id">
          <Identifier value={market.asset_id} label="asset" />
        </Field>
        <Field label="Market id">
          <Identifier value={market.market_id} label="market" />
        </Field>
      </FieldGrid>

      <Disclosure title="How this price works">
        <p>{NATIVE_PRICE_NOTE}</p>
        <p>{CREDITS_DISCLOSURE}</p>
      </Disclosure>

      <Disclosure title="What you are trading">
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>
    </Panel>
  );
}
