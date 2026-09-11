/**
 * PIXEL GEOMETRY — the one place in this application where a figure becomes a
 * number, and the reasons that is allowed here and nowhere else.
 *
 * Everywhere else in `src/` a monetary value is a string from the moment it
 * leaves the API until the moment it is drawn, and `src/lib/source-scan.test.ts`
 * refuses the numeric-coercion built-ins outright so that the rule has no grey
 * area to argue about. A candlestick chart cannot obey that rule literally: an
 * SVG coordinate IS a number, and there is no way to place a wick at a price
 * without eventually producing one.
 *
 * So the rule is kept where it matters and relaxed exactly where it cannot be
 * kept, under four constraints that `source-scan.test.ts` enforces for this
 * directory:
 *
 *   1. THE RATIO IS EXACT. Every scaling division below is BigInt division of
 *      exact base units. Nothing is converted to a double until the ratio has
 *      already been computed, so no price is ever COMPARED or ORDERED in
 *      floating point — only positioned.
 *   2. THE COERCION IS THE LAST STEP. The numeric constructor appears only on an
 *      already-computed integer number of hundredths of a pixel, whose
 *      magnitude is bounded by the plot size. The two string-to-number parsers,
 *      fixed-point rounding and locale number formatting stay refused in this
 *      directory as they are everywhere else.
 *   3. NOTHING HERE FORMATS. This directory may not import `lib/money.ts` or
 *      `lib/format.ts`, and may not render a money string as text. Axis labels
 *      are formatted by `format.ts` OUTSIDE the chart and handed in as opaque
 *      strings, so a figure a reader can read was never computed here.
 *   4. A ROUNDED PIXEL IS NOT A ROUNDED PRICE. The exact open, high, low, close
 *      and volume of the visible range are stated as text beside the chart, by
 *      the caller, from the same strings the API sent.
 *
 * PART 110's division, applied to drawing: the backend computes, the chart
 * places.
 */

/**
 * Hundredths of a pixel. The ratio is computed in BigInt at this precision and
 * divided down once, at the boundary, which puts every coordinate within one
 * hundredth of a pixel of exact — far below anything a display can resolve, and
 * far above anything a reader could misread as a different price.
 */
const SUB = 100n;
const SUB_PIXELS = 100;

/** One candle, exactly as the API sent it: integer base units at `priceScale`. */
export interface CandleInput {
  readonly openTime: string;
  readonly open: string;
  readonly high: string;
  readonly low: string;
  readonly close: string;
  readonly creditVolume: string;
  readonly trades: number;
}

export interface PlotBox {
  readonly width: number;
  readonly height: number;
  /** Height of the volume strip under the price plot, in pixels. */
  readonly volumeHeight: number;
}

/** Where one candle is drawn. Every field is pixels in the plot's own space. */
export interface CandleGeometry {
  readonly key: string;
  /** Centre of the candle. The wick is drawn on this line. */
  readonly x: number;
  /** Half the body width. */
  readonly halfWidth: number;
  readonly highY: number;
  readonly lowY: number;
  /** Top of the body: the higher of open and close on screen. */
  readonly bodyY: number;
  /** Always at least one pixel, so a doji is visible rather than invisible. */
  readonly bodyHeight: number;
  /** Top of the volume bar, in the volume strip's own space. */
  readonly volumeY: number;
  readonly volumeHeight: number;
  /** `up` when close is at or above open, decided by exact integer comparison. */
  readonly direction: "up" | "down";
}

export interface PriceGrid {
  /** Distance from the top of the price plot, in pixels. */
  readonly y: number;
  /** Exact base units of the price this line stands at, for the caller to format. */
  readonly baseUnits: string;
}

export interface CandleGeometrySet {
  readonly candles: readonly CandleGeometry[];
  readonly grid: readonly PriceGrid[];
  /** Exact base units. The caller formats them; this module never does. */
  readonly low: string;
  readonly high: string;
  readonly volumeHigh: string;
  readonly box: PlotBox;
}

function biggest(values: readonly bigint[]): bigint {
  let best = values[0] ?? 0n;
  for (const value of values) {
    if (value > best) best = value;
  }
  return best;
}

function smallest(values: readonly bigint[]): bigint {
  let best = values[0] ?? 0n;
  for (const value of values) {
    if (value < best) best = value;
  }
  return best;
}

/**
 * Maps an exact value onto a pixel offset from the top of a box of `height`.
 *
 * The division is BigInt: the whole ratio is computed in integers and only the
 * final scaling back down to pixels is a double. A span of zero is a flat
 * range, and a flat range is drawn down the middle rather than at an edge,
 * because a market that has not moved is not a market at the top of its range.
 */
function offset(value: bigint, low: bigint, high: bigint, height: number): number {
  const span = high - low;
  if (span <= 0n) return height / 2;
  const scaled = ((high - value) * BigInt(height) * SUB) / span;
  return Number(scaled) / SUB_PIXELS;
}

/** The same, measured up from zero: how tall a bar standing on the axis is. */
function extent(value: bigint, high: bigint, height: number): number {
  if (high <= 0n) return 0;
  const scaled = (value * BigInt(height) * SUB) / high;
  return Number(scaled) / SUB_PIXELS;
}

/**
 * Turns a window of candles into the pixels that draw them.
 *
 * `gridLines` is how many horizontal price lines to place, evenly through the
 * range. Their labels are NOT produced here: each carries the exact base units
 * it stands at, and the caller formats those with `lib/format.ts`.
 */
export function candleGeometry(
  candles: readonly CandleInput[],
  box: PlotBox,
  gridLines = 4,
): CandleGeometrySet {
  if (candles.length === 0) {
    return { candles: [], grid: [], low: "0", high: "0", volumeHigh: "0", box };
  }

  const high = biggest(candles.map((candle) => BigInt(candle.high)));
  const low = smallest(candles.map((candle) => BigInt(candle.low)));
  const volumeHigh = biggest(candles.map((candle) => BigInt(candle.creditVolume)));

  // One slot per bucket that came back. The API omits a bucket with no trades
  // rather than filling it forward, so the axis is the sequence of prints, and
  // the caption says which instants the window actually spans.
  const slot = box.width / candles.length;
  const halfWidth = slot > 3 ? (slot - 2) / 2 : slot / 2;

  const placed = candles.map((candle, index): CandleGeometry => {
    const open = BigInt(candle.open);
    const close = BigInt(candle.close);
    const openY = offset(open, low, high, box.height);
    const closeY = offset(close, low, high, box.height);
    const top = openY < closeY ? openY : closeY;
    const bottom = openY < closeY ? closeY : openY;
    const bodyHeight = bottom - top;
    const volume = extent(BigInt(candle.creditVolume), volumeHigh, box.volumeHeight);
    return {
      key: candle.openTime,
      x: index * slot + slot / 2,
      halfWidth,
      highY: offset(BigInt(candle.high), low, high, box.height),
      lowY: offset(BigInt(candle.low), low, high, box.height),
      bodyY: top,
      bodyHeight: bodyHeight < 1 ? 1 : bodyHeight,
      volumeY: box.volumeHeight - volume,
      volumeHeight: volume,
      direction: close >= open ? "up" : "down",
    };
  });

  const grid: PriceGrid[] = [];
  const steps = gridLines < 1 ? 1 : gridLines;
  for (let i = 0; i <= steps; i = i + 1) {
    // The line's price is an exact integer: the range is divided in BigInt and
    // the remainder is dropped, so a grid label is always a price the market
    // could actually print rather than a rounded double.
    const at = high - ((high - low) * BigInt(i)) / BigInt(steps);
    grid.push({ y: offset(at, low, high, box.height), baseUnits: String(at) });
  }

  return {
    candles: placed,
    grid,
    low: String(low),
    high: String(high),
    volumeHigh: String(volumeHigh),
    box,
  };
}

/**
 * The open, high, low, close and volume of a whole window, as exact strings.
 *
 * This is what the caption states out loud, and every field of it is produced
 * by comparing or adding exact integers. The volume sum is BigInt, which makes
 * it the one arithmetic operation performed on a monetary value anywhere in
 * this application — and it is exact, which is the only reason it is allowed.
 */
export interface RangeSummary {
  readonly open: string;
  readonly high: string;
  readonly low: string;
  readonly close: string;
  readonly creditVolume: string;
  readonly trades: number;
  readonly firstOpenTime: string;
  readonly lastOpenTime: string;
}

export function summariseRange(candles: readonly CandleInput[]): RangeSummary | undefined {
  const first = candles[0];
  const last = candles[candles.length - 1];
  if (first === undefined || last === undefined) return undefined;

  let high = BigInt(first.high);
  let low = BigInt(first.low);
  let volume = 0n;
  let trades = 0;
  for (const candle of candles) {
    const candleHigh = BigInt(candle.high);
    const candleLow = BigInt(candle.low);
    if (candleHigh > high) high = candleHigh;
    if (candleLow < low) low = candleLow;
    volume = volume + BigInt(candle.creditVolume);
    trades = trades + candle.trades;
  }

  return {
    open: first.open,
    high: String(high),
    low: String(low),
    close: last.close,
    creditVolume: String(volume),
    trades,
    firstOpenTime: first.openTime,
    lastOpenTime: last.openTime,
  };
}
