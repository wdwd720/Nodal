/**
 * The chart's arithmetic, checked where it is allowed to exist.
 *
 * `src/charts/geometry.ts` holds the only float conversion in this application,
 * and `source-scan.test.ts` grants it an explicit allowlist. An allowlist is
 * only defensible if the code behind it is tested, so this is that test, and it
 * is aimed at one question: does a price survive the trip to a pixel?
 *
 * The answer has to be yes for values far past what a double can hold exactly.
 * A market price at eighteen decimal places is routinely a nineteen-digit
 * integer, which is already outside `Number.MAX_SAFE_INTEGER`, so the cases
 * below use figures of that size deliberately. Every one of them would pass if
 * the module were sloppy about small numbers and fail the moment it converted
 * a price rather than a ratio.
 *
 * The test file itself lives here rather than beside the module because the
 * unit-test runner globs `src/lib/*.test.ts`.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { candleGeometry, summariseRange, type CandleInput } from "../charts/geometry.ts";

const BOX = { width: 100, height: 200, volumeHeight: 50 };

function candle(over: Partial<CandleInput> & { readonly openTime: string }): CandleInput {
  return {
    open: "100",
    high: "100",
    low: "100",
    close: "100",
    creditVolume: "0",
    trades: 0,
    ...over,
  };
}

test("an empty window draws nothing and claims nothing", () => {
  const geometry = candleGeometry([], BOX);
  assert.deepEqual(geometry.candles, []);
  assert.deepEqual(geometry.grid, []);
  assert.equal(geometry.low, "0");
  assert.equal(geometry.high, "0");
  assert.equal(geometry.volumeHigh, "0");
});

test("the top of the range is the top of the plot and the bottom is the bottom", () => {
  const geometry = candleGeometry(
    [
      candle({ openTime: "a", open: "100", high: "200", low: "100", close: "200" }),
      candle({ openTime: "b", open: "200", high: "200", low: "0", close: "0" }),
    ],
    BOX,
  );
  const first = geometry.candles[0];
  const second = geometry.candles[1];
  assert.ok(first !== undefined && second !== undefined);
  assert.equal(first.highY, 0, "the highest print sits on the top edge");
  assert.equal(second.lowY, BOX.height, "the lowest print sits on the bottom edge");
  assert.equal(geometry.high, "200");
  assert.equal(geometry.low, "0");
});

test("a market that has not moved is drawn down the middle, not at an edge", () => {
  // A flat range has no top and no bottom. Drawing it against the top edge
  // would say the market is at the high of its range, which is a claim, not a
  // layout.
  const geometry = candleGeometry([candle({ openTime: "a" })], BOX);
  const only = geometry.candles[0];
  assert.ok(only !== undefined);
  assert.equal(only.highY, BOX.height / 2);
  assert.equal(only.lowY, BOX.height / 2);
});

test("a bucket that opened and closed at one price is still visible", () => {
  const geometry = candleGeometry(
    [
      candle({ openTime: "a", open: "100", high: "300", low: "100", close: "100" }),
      candle({ openTime: "b", open: "300", high: "300", low: "100", close: "100" }),
    ],
    BOX,
  );
  const doji = geometry.candles[0];
  assert.ok(doji !== undefined);
  assert.equal(doji.bodyHeight, 1, "a zero-height body is drawn one pixel tall");
});

test("direction is decided by exact comparison, at a magnitude a double cannot hold", () => {
  // These two differ by one base unit and are both larger than the largest
  // integer a double represents exactly. Converted to doubles they compare
  // equal, and the candle would be drawn the wrong colour.
  const geometry = candleGeometry(
    [
      candle({
        openTime: "a",
        open: "9007199254740993",
        close: "9007199254740992",
        high: "9007199254740993",
        low: "9007199254740992",
      }),
    ],
    BOX,
  );
  const only = geometry.candles[0];
  assert.ok(only !== undefined);
  assert.equal(only.direction, "down");
});

test("a volume bar stands on the axis and never exceeds the strip", () => {
  const geometry = candleGeometry(
    [
      candle({ openTime: "a", creditVolume: "0" }),
      candle({ openTime: "b", creditVolume: "1000000000000000000" }),
      candle({ openTime: "c", creditVolume: "500000000000000000" }),
    ],
    BOX,
  );
  const [zero, tallest, half] = geometry.candles;
  assert.ok(zero !== undefined && tallest !== undefined && half !== undefined);
  assert.equal(zero.volumeHeight, 0);
  assert.equal(zero.volumeY, BOX.volumeHeight);
  assert.equal(tallest.volumeHeight, BOX.volumeHeight);
  assert.equal(tallest.volumeY, 0);
  assert.equal(half.volumeHeight, BOX.volumeHeight / 2);
  assert.equal(geometry.volumeHigh, "1000000000000000000");
});

test("every grid line stands at a price the market could actually print", () => {
  const geometry = candleGeometry(
    [candle({ openTime: "a", open: "0", high: "400", low: "0", close: "400" })],
    BOX,
    4,
  );
  assert.equal(geometry.grid.length, 5);
  assert.deepEqual(
    geometry.grid.map((line) => line.baseUnits),
    ["400", "300", "200", "100", "0"],
  );
  assert.deepEqual(
    geometry.grid.map((line) => line.y),
    [0, 50, 100, 150, 200],
  );
  for (const line of geometry.grid) {
    assert.match(line.baseUnits, /^-?[0-9]+$/, "a grid label is an exact integer, never a double");
  }
});

test("the range summary is exact where a double would already have rounded", () => {
  const summary = summariseRange([
    candle({
      openTime: "a",
      open: "1000000000000000001",
      high: "1000000000000000009",
      low: "1000000000000000000",
      close: "1000000000000000005",
      creditVolume: "9007199254740993",
      trades: 2,
    }),
    candle({
      openTime: "b",
      open: "1000000000000000005",
      high: "1000000000000000011",
      low: "999999999999999999",
      close: "1000000000000000007",
      creditVolume: "1",
      trades: 3,
    }),
  ]);
  assert.ok(summary !== undefined);
  assert.equal(summary.open, "1000000000000000001", "the open is the first bucket's, untouched");
  assert.equal(summary.close, "1000000000000000007", "the close is the last bucket's, untouched");
  assert.equal(summary.high, "1000000000000000011");
  assert.equal(summary.low, "999999999999999999");
  // 9007199254740993 + 1. A double would answer 9007199254740994 by luck here
  // and the wrong thing one unit further on; BigInt answers it exactly always.
  assert.equal(summary.creditVolume, "9007199254740994");
  assert.equal(summary.trades, 5);
  assert.equal(summary.firstOpenTime, "a");
  assert.equal(summary.lastOpenTime, "b");
});

test("an empty window has no summary rather than a zeroed one", () => {
  assert.equal(summariseRange([]), undefined);
});
