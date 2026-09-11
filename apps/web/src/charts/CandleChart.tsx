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
 * marks describable to a screen reader, which is not something the libraries
 * do at all.
 *
 * # What this component is not allowed to do
 *
 * It never formats a figure. Every string it draws — the axis labels, the
 * per-candle readout, the caption — arrives as a prop, already formatted by
 * `lib/format.ts` OUTSIDE this directory. `source-scan.test.ts` enforces that:
 * nothing under `src/charts/` may import the money or format modules, and every
 * `<text>` element here renders a bare prop reference and nothing else.
 *
 * # Accessibility
 *
 * A plot is a picture of a table, and a reader who cannot see it is owed the
 * table. So:
 *
 *   - the whole thing is a `<figure>` with a `<figcaption>` stating the open,
 *     high, low, close and volume of the VISIBLE range as text;
 *   - the plot itself is a focusable `role="img"` with the same summary as its
 *     accessible description, so it is reachable by keyboard rather than being
 *     a region a mouse can inspect and a keyboard cannot;
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

/** A horizontal price line, already labelled by the caller. */
export interface PriceTick {
  readonly y: number;
  readonly label: string;
}

/** A time marker along the bottom, already labelled by the caller. */
export interface TimeTick {
  readonly x: number;
  readonly label: string;
}

export interface CandleChartProps {
  /** The plot's accessible name, e.g. "AGENT price, 1 hour buckets". */
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
  /** Extra content for the caption: the exact figures, as the caller renders them. */
  readonly caption?: ReactNode;
}

/** Room for the price labels down the right-hand edge, in viewBox units. */
const GUTTER = 52;
/** Room for the time labels along the bottom. */
const FOOTER = 16;

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
  const width = box.width + GUTTER;
  const height = box.height + box.volumeHeight + FOOTER;
  const volumeTop = box.height;
  const readout = selected === undefined ? "" : (props.readouts[selected] ?? "");
  const crosshair = selected === undefined ? undefined : geometry.candles[selected];

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
        <svg
          className="chart-svg"
          viewBox={`0 0 ${String(width)} ${String(height)}`}
          focusable="false"
          aria-hidden="true"
        >
          {props.priceTicks.map((tick) => (
            <g key={tick.label + String(tick.y)}>
              <line
                className="chart-grid-line"
                x1={0}
                x2={box.width}
                y1={tick.y}
                y2={tick.y}
              />
              <text className="chart-axis-label" x={box.width + 4} y={tick.y + 3}>
                {tick.label}
              </text>
            </g>
          ))}

          <line
            className="chart-separator"
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
                  x1={candle.x}
                  x2={candle.x}
                  y1={candle.highY}
                  y2={candle.lowY}
                />
                <rect
                  className="chart-body"
                  x={candle.x - candle.halfWidth}
                  y={candle.bodyY}
                  width={candle.halfWidth * 2}
                  height={candle.bodyHeight}
                />
                <rect
                  className="chart-volume"
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
              x1={crosshair.x}
              x2={crosshair.x}
              y1={0}
              y2={volumeTop + box.volumeHeight}
            />
          )}

          {props.timeTicks.map((tick) => (
            <text
              key={tick.label + String(tick.x)}
              className="chart-axis-label"
              x={tick.x}
              y={height - 4}
            >
              {tick.label}
            </text>
          ))}
        </svg>
      </div>

      <p className="chart-readout" role="status" aria-live="polite">
        {readout}
      </p>
      <p className="chart-hint">
        Focus the plot and use the arrow keys to read each bucket; Escape clears the
        selection.
      </p>
      <figcaption className="chart-caption" id={summaryId}>
        {props.summary}
        {props.caption}
      </figcaption>
    </figure>
  );
}
