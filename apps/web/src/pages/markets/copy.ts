/**
 * What the markets screens say about a status, a sort and a window of history.
 *
 * It is a module rather than three literals in three components because the
 * same status appears on the list, on the trade screen and on the ticket's
 * disabled reason, and three copies of "HALTED" mean three chances for one of
 * them to say something the others do not.
 */
import type { CandleInterval, MarketSort, MarketStatus } from "../../api/queries.ts";
import type { Tone } from "../../components/Layout.tsx";

export interface StatusCopy {
  readonly tone: Tone;
  /** What this status means for somebody deciding whether to trade. */
  readonly text: string;
  readonly buy: boolean;
  readonly sell: boolean;
}

/**
 * The market's own status, which is what decides whether an order is accepted.
 *
 * `buy` and `sell` mirror `internal/nativemarket`'s own rule rather than
 * guessing at it: the backend refuses a side it does not accept with
 * ASSET_RESTRICTED, and the ticket disables the control with this reason so a
 * customer is not asked to fill in a form that cannot be sent. The refusal is
 * still rendered if the backend disagrees — this is a courtesy, not authority.
 */
export const MARKET_STATUS: Readonly<Record<string, StatusCopy>> = {
  PENDING: {
    tone: "neutral",
    text: "The market exists but has not opened. Nothing trades yet.",
    buy: false,
    sell: false,
  },
  ACTIVE: {
    tone: "good",
    text: "Open. Both directions are accepted.",
    buy: true,
    sell: true,
  },
  CLOSE_ONLY: {
    tone: "warn",
    text: "Holders can sell. Nobody new can buy.",
    buy: false,
    sell: true,
  },
  HALTED: {
    tone: "warn",
    text: "An operator stopped trading. Holdings already open are untouched.",
    buy: false,
    sell: false,
  },
  FROZEN: {
    tone: "bad",
    text: "Everything is stopped, selling included, while an incident is looked at.",
    buy: false,
    sell: false,
  },
  DELISTED: {
    tone: "bad",
    text: "Permanently removed. It will not reopen.",
    buy: false,
    sell: false,
  },
};

/** The asset's status, which is a different fact from the market's. */
export const ASSET_STATUS: Readonly<Record<string, { readonly tone: Tone; readonly text: string }>> = {
  DRAFT: { tone: "neutral", text: "Not published." },
  PENDING_REVIEW: { tone: "info", text: "Submitted and awaiting a moderation decision." },
  ACTIVE: { tone: "good", text: "Published." },
  CLOSE_ONLY: { tone: "warn", text: "Published, and the market takes sells only." },
  HALTED: { tone: "warn", text: "Published, and trading is stopped." },
  DELISTED: { tone: "bad", text: "Permanently removed." },
  REJECTED: { tone: "bad", text: "Refused at moderation." },
};

/** What moderation decided about the content. It never by itself opens a market. */
export const MODERATION_STATE: Readonly<Record<string, { readonly tone: Tone; readonly text: string }>> = {
  PENDING: { tone: "info", text: "Nobody has looked at this yet." },
  APPROVED: { tone: "good", text: "A moderator allowed it to be published." },
  REJECTED: { tone: "bad", text: "A moderator refused it." },
  FLAGGED: { tone: "warn", text: "Reported and under review. It is still visible." },
};

export function statusCopy(status: string): StatusCopy {
  return (
    MARKET_STATUS[status] ?? {
      tone: "neutral",
      text: "The backend reported a status this build does not have words for.",
      buy: false,
      sell: false,
    }
  );
}

/** The orderings the API offers, and the honest warning that goes with four of them. */
export const SORTS: ReadonlyArray<{ readonly value: MarketSort; readonly label: string }> = [
  { value: "NEWEST", label: "Newest" },
  { value: "VOLUME_24H", label: "Volume, 24 hours" },
  { value: "CHANGE_24H", label: "Change, 24 hours" },
  { value: "LIQUIDITY", label: "Liquidity" },
  { value: "PRICE", label: "Price" },
];

/**
 * The status filters, as sets rather than as one status each.
 *
 * "Stopped" is two statuses because a customer thinks of it as one thing, and
 * offering six single-status options would make them run the filter six times
 * to answer one question.
 */
export const STATUS_FILTERS: ReadonlyArray<{
  readonly key: string;
  readonly label: string;
  readonly statuses: readonly MarketStatus[];
}> = [
  { key: "all", label: "Every status", statuses: [] },
  { key: "trading", label: "Trading now", statuses: ["ACTIVE"] },
  { key: "sell-only", label: "Sell only", statuses: ["CLOSE_ONLY"] },
  { key: "stopped", label: "Stopped", statuses: ["HALTED", "FROZEN"] },
  { key: "pending", label: "Not open yet", statuses: ["PENDING"] },
  { key: "delisted", label: "Delisted", statuses: ["DELISTED"] },
];

/* --------------------------------------------------------------------------
 * Chart windows
 *
 * Goal §14 asks for 1H, 1D, 1W, 1M and ALL; the API offers five bucket widths
 * and demands an explicit `from` and `to`. A range here is therefore a pair —
 * the window the customer asked for, and the bucket width that draws it at a
 * sensible density. Choosing them together is why the control says "last week"
 * rather than "1h", which is a unit, not a question anybody asked.
 * ------------------------------------------------------------------------ */

export interface ChartRange {
  readonly key: string;
  readonly label: string;
  readonly interval: CandleInterval;
  /** Milliseconds one bucket covers. Used to align the window to a boundary. */
  readonly bucketMs: number;
  /**
   * How many buckets back the window reaches, or `undefined` for ALL, which
   * starts at the market's own creation instant.
   */
  readonly buckets: number | undefined;
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

export const CHART_RANGES: readonly ChartRange[] = [
  { key: "1h", label: "1 hour", interval: "1m", bucketMs: MINUTE, buckets: 60 },
  { key: "6h", label: "6 hours", interval: "5m", bucketMs: 5 * MINUTE, buckets: 72 },
  { key: "1d", label: "1 day", interval: "15m", bucketMs: 15 * MINUTE, buckets: 96 },
  { key: "1w", label: "1 week", interval: "1h", bucketMs: HOUR, buckets: 168 },
  { key: "1m", label: "1 month", interval: "1d", bucketMs: DAY, buckets: 30 },
  { key: "all", label: "All", interval: "1d", bucketMs: DAY, buckets: undefined },
];

/** The API refuses a window wider than this many buckets. */
const MAX_BUCKETS = 1500;

/**
 * The exact window to ask for, aligned to a bucket boundary.
 *
 * Alignment is not tidiness: an unaligned `to` would move on every render, and
 * a query key that moves on every render is a request that never resolves. The
 * edge advances when a bucket closes, which is also the only moment a new
 * bucket can exist.
 */
export function chartWindow(
  range: ChartRange,
  createdAt: string,
  now: number,
): { readonly from: string; readonly to: string } {
  const to = now - (now % range.bucketMs) + range.bucketMs;
  if (range.buckets !== undefined) {
    return {
      from: new Date(to - range.buckets * range.bucketMs).toISOString(),
      to: new Date(to).toISOString(),
    };
  }
  const created = new Date(createdAt).getTime();
  const earliest = to - MAX_BUCKETS * range.bucketMs;
  const start = isNaN(created) || created < earliest ? earliest : created;
  return {
    from: new Date(start - (start % range.bucketMs)).toISOString(),
    to: new Date(to).toISOString(),
  };
}

/* --------------------------------------------------------------------------
 * Standing copy
 * ------------------------------------------------------------------------ */

/** The sentence a quote must never be read without. */
export const QUOTE_IS_NOT_A_PRICE =
  "A quote is a record of what this market said at one state version. It does not price your " +
  "execution: the order is priced again when it runs, and the only number you are agreeing to is " +
  "the minimum you set below.";

/** Said wherever the list is ordered by something that moves. */
export const UNSTABLE_SORT_NOTE =
  "This ordering ranks by a figure that changes when somebody trades, so paging it can show a " +
  "market twice or miss one. Order by newest to page through every market exactly once.";

/** Said on a market the sandbox demo seeder created. */
export const DEMO_MARKET_NOTE =
  "This market was created by the sandbox demo seeder. It exists so the screens have something to " +
  "show and it represents nothing: no Credits behind it are anybody's, and its price is not an " +
  "opinion about anything.";

/** Said where the safety limits are shown. */
export const LIMITS_NOTE =
  "These are the limits in force on this market right now, from two documents: the market-safety " +
  "policy, which is about the venue, and the risk policy, which is about an account. A limit the " +
  "backend did not report is shown as not reported rather than as a zero.";

/** Said on the chart, wherever a bucket is missing. */
export const CANDLE_GAPS_NOTE =
  "A period with no trades has no candle. It is left out rather than drawn flat, because a flat " +
  "candle would show a trade at a price nobody paid.";
