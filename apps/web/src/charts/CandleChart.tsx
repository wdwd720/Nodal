/**
 * The candlestick plot, as SVG and as text.
 *
 * # Why SVG and not a chart library
 *
 * The whole page budget is 180 kB and the smallest credible candlestick library
 * is a large fraction of it on its own; more to the point, every one of them
 * takes NUMBERS. Handing a charting library a price means converting an exact
 * base-unit integer to a double at the edge of this application and trusting a
 * dependency with the one invariant the product is built on. Two hundred lines
 * of SVG keep the conversion where `geometry.ts` can bound it, and keep the
 * marks describable to a screen reader, which is not something the libraries do
 * at all.
 *
 * # Why the axis labels are HTML and the marks are SVG
 *
 * The plot stretches to whatever column it lands in, from a 1200px desktop to a
 * 375px phone. A label drawn INSIDE a stretched viewBox stretches with it: the
 * first version of this drew its prices as SVG text, and at 375px they came out
 * under five pixels tall — present, and unreadable, which goal §31 asks for the
 * opposite of. So the geometry scales and the type does not. The marks are SVG
 * with `vector-effect="non-scaling-stroke"`, so a wick stays one pixel wide at
 * every width, and the labels are HTML positioned as a percentage of the plot,
 * so they are set at a real font size whatever the viewport is.
 *
 * # What this component is not allowed to do
 *
 * It never formats a figure. Every string it draws — the axis labels, the
 * per-bucket readout, the caption — arrives as a prop, already formatted by
 * `lib/format.ts` OUTSIDE this directory. `source-scan.test.ts` enforces that:
 * nothing under `src/charts/` may import the money or format modules, every
 * text-bearing expression here renders one bare property read, and the word
 * "Credits" may not appear in this directory at all.
 *
 * # Accessibility
 *
 * A plot is a picture of a table, and a reader who cannot see it is owed the
 * table. So:
 *
 *   - the whole thing is a `<figure>` with a `<figcaption>` stating the open,
 *     high, low, close and volume of the VISIBLE range as text;
 *   - the plot is a focusable `role="img"` described by that same caption, so
 *     it is reachable by keyboard rather than being a region a mouse can
 *     inspect and a keyboard cannot;
 *   - arrow keys walk the buckets and each one announces its own readout
 *     through a polite live region — the same sentence a pointer reveals;
 *   - direction is drawn in two channels, hollow-and-positive against
 *     solid-and-negative, so it survives a colourblind reader and forced
 *     colours alike;
 *   - nothing animates. The reduced-motion rule in `chart.css` says so out
 *     loud, because a chart is the surface most likely to grow a transition.
 */
import { useCallback, useId, useState, type KeyboardEvent, type ReactNode } from "react";

import type { CandleGeometrySet } from "./geometry.ts";
import "./chart.css";

/**
 * A horizontal price line, already labelled by the caller.
 *
 * `label` is a ReactNode and not a string, which is the point. A price at
 * eighteen decimal places is unreadable written out and ambiguous written
 * short: the design system renders it as a leading-zero run in a smaller digit,
 * and that notation only survives as markup. So the PAGE renders the figure
 * with the `Figure` primitive it already owns and hands the result here, and
 * this component positions something it cannot read and did not compose.
 */
export interface PriceTick {
  /** Stable identity for the tick. Never derived from the rendered label. */
  readonly key: string;
  readonly y: number;
  readonly label: ReactNode;
}

/** A time marker along the bottom. Times are short, so this one is text. */
export interface TimeTick {
  readonly key: string;
  readonly x: number;
  readonly label: string;
}

export interface CandleChartProps {
  /** The plot's accessible name, e.g. "AGENT price, 1h periods". */
  readonly label: string;
  /**
   * The whole range in words, for the accessible description and the caption.
   * Built by the caller from the API strings; this component never composes it.
   */
  readonly summary: string;
  /** One already-formatted sentence per bucket, in the geometry's own order. */
  readonly readouts: readonly string[];
  readonly geometry: CandleGeometrySet;
  readonly priceTicks: readonly PriceTick[];
  readonly timeTicks: readonly TimeTick[];
}

export function CandleChart(props: CandleChartProps): ReactNode {
  const base = useId();
  const summaryId = `${base}-summary`;
  const [selected, setSelected] = useState<number | undefined>(undefined);
  const { geometry } = props;
  const count = geometry.candles.length;

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>): void => {
      if (count === 0) return;
      const last = count - 1;
      const at = selected ?? last;
      let next: number | undefined = undefined;
      if (event.key === "ArrowRight") next = at >= last ? last : at + 1;
      if (event.key === "ArrowLeft") next = at <= 0 ? 0 : at - 1;
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = last;
      if (event.key === "Escape") {
        setSelected(undefined);
        return;
      }
      if (next === undefined) return;
      event.preventDefault();
      setSelected(next);
    },
    [count, selected],
  );

  const box = geometry.box;
  const height = box.height + box.volumeHeight;
  const volumeTop = box.height;
  const readout = selected === undefined ? "" : (props.readouts[selected] ?? "");
  const crosshair = selected === undefined ? undefined : geometry.candles[selected];

  /** A coordinate as a percentage of the plot, for an HTML label over it. */
  const down = (y: number): string => `${String((y * 100) / height)}%`;
  const across = (x: number): string => `${String((x * 100) / box.width)}%`;

  return (
    <figure className="chart-figure">
      <div
        className="chart-frame"
        role="img"
        aria-label={props.label}
        aria-describedby={summaryId}
        tabIndex={0}
        onKeyDown={onKeyDown}
      >
        <div className="chart-plot">
          <svg
            className="chart-svg"
            viewBox={`0 0 ${String(box.width)} ${String(height)}`}
            preserveAspectRatio="none"
            focusable="false"
            aria-hidden="true"
          >
            {props.priceTicks.map((tick) => (
              <line
                key={tick.key}
                className="chart-grid-line"
                vectorEffect="non-scaling-stroke"
                x1={0}
                x2={box.width}
                y1={tick.y}
                y2={tick.y}
              />
            ))}

            <line
              className="chart-separator"
              vectorEffect="non-scaling-stroke"
              x1={0}
              x2={box.width}
              y1={volumeTop}
              y2={volumeTop}
            />

            {geometry.candles.map((candle, index) => {
              const classes = [
                candle.direction === "up" ? "chart-up" : "chart-down",
                index === selected ? "chart-selected" : "",
              ]
                .filter((part) => part !== "")
                .join(" ");
              return (
                <g
                  key={candle.key}
                  className={classes}
                  onPointerEnter={() => {
                    setSelected(index);
                  }}
                >
                  <line
                    className="chart-wick"
                    vectorEffect="non-scaling-stroke"
                    x1={candle.x}
                    x2={candle.x}
                    y1={candle.highY}
                    y2={candle.lowY}
                  />
                  <rect
                    className="chart-body"
                    vectorEffect="non-scaling-stroke"
                    x={candle.x - candle.halfWidth}
                    y={candle.bodyY}
                    width={candle.halfWidth * 2}
                    height={candle.bodyHeight}
                  />
                  <rect
                    className="chart-volume"
                    vectorEffect="non-scaling-stroke"
                    x={candle.x - candle.halfWidth}
                    y={volumeTop + candle.volumeY}
                    width={candle.halfWidth * 2}
                    height={candle.volumeHeight}
                  />
                </g>
              );
            })}

            {crosshair !== undefined && (
              <line
                className="chart-crosshair"
                vectorEffect="non-scaling-stroke"
                x1={crosshair.x}
                x2={crosshair.x}
                y1={0}
                y2={height}
              />
            )}
          </svg>

          <div className="chart-price-axis" aria-hidden="true">
            {props.priceTicks.map((tick) => (
              <span
                key={tick.key}
                className="chart-axis-label"
                style={{ top: down(tick.y) }}
              >
                {tick.label}
              </span>
            ))}
          </div>

          <div className="chart-time-axis" aria-hidden="true">
            {props.timeTicks.map((tick) => (
              <span
                key={tick.key}
                className="chart-axis-label"
                style={{ left: across(tick.x) }}
              >
                {tick.label}
              </span>
            ))}
          </div>
        </div>
      </div>

      <p className="chart-readout" role="status" aria-live="polite">
        {readout}
      </p>
      <p className="chart-hint">
        Focus the plot and use the arrow keys to read each period; Escape clears the selection.
      </p>
      <figcaption className="chart-caption" id={summaryId}>
        {props.summary}
      </figcaption>
    </figure>
  );
}
