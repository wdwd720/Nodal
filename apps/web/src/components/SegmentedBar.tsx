/**
 * Available, reserved, pending — the central information-design problem on Home.
 *
 * A single balance is four different numbers and the difference between them is
 * the product. "You have $10,000" is true of almost nothing: some of it is
 * spendable now, some is committed to orders that have not settled, and some is
 * a deposit the chain has not confirmed. Collapsing those into one figure is the
 * most convenient number on the page and the most misleading.
 *
 * Three textures rather than three colours: solid for available, a hatch for
 * reserved — the texture says "committed" without spending a second hue — and a
 * sparse hatch for pending. That survives grayscale, forced colours and a
 * colourblind reader, none of which a three-colour bar does.
 *
 * TWO RULES ABOUT THE ARITHMETIC.
 *
 * First, this component NEVER SUMS INTO A FIGURE IT PRESENTS AS A BALANCE. The
 * backend computes; the interface formats. The whole is a figure the caller
 * must take from the same response the parts came from, and if it is absent the
 * bar shows relative proportions and says so rather than inventing a
 * denominator.
 *
 * It does subtract, in one place and for one reason. A bar whose segments do
 * not reach its own total used to draw short and say nothing, so a bucket the
 * API returned and the caller forgot to pass simply vanished from the picture
 * — which is what happened to the `reversed` balance: returned by
 * `GET /v1/credits/balance`, rendered by no page, and therefore able to be
 * wrong forever without anybody seeing it. The unaccounted part is now drawn,
 * in exact integer arithmetic, labelled as unaccounted rather than as anything
 * the holder has, and the legend says the response is the authority. A caller
 * that passes every bucket never sees it.
 *
 * Second, the widths are GEOMETRY, not finance. They are handed to the browser
 * as flex ratios of the exact base-unit strings, so nothing here divides. The
 * only derived number is the whole percentage in the accessible description,
 * computed in exact integer arithmetic, truncated so a segment can never claim
 * a larger share than it holds, and marked "about" because it is.
 *
 * The legend carries the exact figures, unaltered, and one line saying what
 * each segment is waiting on. A bar with no legend is decoration.
 */
import type { ReactNode } from "react";

import { unaccountedBaseUnits } from "../lib/credits.ts";
import { fromBaseUnits, percentOfTotal } from "../lib/format.ts";
import { Figure } from "./Figure.tsx";

export type SegmentTexture = "solid" | "hatch" | "sparse";

export interface Segment {
  readonly key: string;
  /** "Available now". A word, never a colour. */
  readonly label: string;
  /** Exact integer base units, as the backend holds them. */
  readonly baseUnits: string;
  /** One line: what this segment is, or what it is waiting on. */
  readonly explanation: string;
  readonly texture: SegmentTexture;
}

const TEXTURE_CLASS: Readonly<Record<SegmentTexture, string>> = {
  solid: "segment-available",
  hatch: "segment-reserved",
  sparse: "segment-pending",
};

function isAllZero(segments: readonly Segment[]): boolean {
  for (const segment of segments) {
    for (const ch of segment.baseUnits) {
      if (ch >= "1" && ch <= "9") return false;
    }
  }
  return true;
}

/** "Available now, about 62%" — or just the label when there is no whole. */
function describe(segment: Segment, total: string | undefined): string {
  if (total === undefined) return segment.label;
  const share = percentOfTotal(segment.baseUnits, total);
  return share === undefined ? segment.label : `${segment.label}, about ${share}%`;
}

export function SegmentedBar(props: {
  /** What the bar is a picture of. Becomes part of its accessible name. */
  readonly caption: string;
  readonly segments: readonly Segment[];
  /** Decimals of the instrument the base units are denominated in. */
  readonly scale: number;
  readonly symbol?: string;
  /**
   * The whole, exactly as the BACKEND computed it. Never summed here, and
   * omitted rather than guessed when the response does not carry one.
   */
  readonly total?: string;
}): ReactNode {
  const rest = unaccountedBaseUnits(
    props.segments.map((segment) => segment.baseUnits),
    props.total,
  );
  const segments: readonly Segment[] =
    rest === undefined
      ? props.segments
      : [
          ...props.segments,
          {
            key: "unaccounted",
            label: "Not accounted for",
            baseUnits: rest,
            explanation:
              "Part of the total that none of the parts above covers. The response is the authority on both; this is the difference, shown rather than hidden.",
            texture: "sparse",
          },
        ];
  const empty = isAllZero(segments);
  const description = segments
    .map((segment) => `${describe(segment, props.total)}: ${fromBaseUnits(segment.baseUnits, props.scale)}`)
    .join("; ");

  return (
    <div className="segbar-wrap">
      <div
        className="segbar"
        role="img"
        aria-label={
          empty
            ? `${props.caption}: every part is zero.`
            : `${props.caption}. ${description}.`
        }
      >
        {!empty &&
          segments.map((segment) => (
            <span
              key={segment.key}
              className={`segment ${TEXTURE_CLASS[segment.texture]}`}
              style={{ flexGrow: segment.baseUnits }}
            />
          ))}
      </div>
      <ul className="segbar-legend">
        {segments.map((segment) => (
          <li key={segment.key}>
            <span className={`segbar-swatch ${TEXTURE_CLASS[segment.texture]}`} aria-hidden="true" />
            <span className="eyebrow">{segment.label}</span>
            <div>
              <Figure
                kind="money"
                value={{ base: segment.baseUnits, scale: props.scale }}
                {...(props.symbol === undefined ? {} : { symbol: props.symbol })}
              />
            </div>
            <p className="field-note">{segment.explanation}</p>
          </li>
        ))}
      </ul>
      {props.total === undefined && (
        <p className="field-note">
          The proportions above are relative to each other. No total is shown because the response
          did not carry one, and adding these together here would be this interface computing a
          figure the backend owns.
        </p>
      )}
    </div>
  );
}
