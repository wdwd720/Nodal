/**
 * Every number that represents money, a quantity, a rate or a count.
 *
 * The contract, and the reason this component exists at all:
 *
 *   IT NEVER TAKES A PRE-FORMATTED STRING.
 *
 * A component that accepts `"$1.2M"` is a component that will one day render a
 * rounded number where an exact one was required. This one takes the value the
 * way the backend holds it — an exact decimal string, or exact integer base
 * units plus the instrument's scale — and formats it here, once, through
 * `lib/format.ts`.
 *
 * The five states, because a figure is data-bearing:
 *
 *   present    the figure, tabular, with its exact value in `title`
 *   absent     an em dash and a stated reason. Never a zero.
 *   malformed  a stated fault. Never a number the reader could trust.
 *   stale      `faint` — the figure stays visible and stops looking
 *              authoritative. It is never blanked: an absent figure is a
 *              different claim from an old one.
 *   loading    not this component's job. Use `Skeleton`, which is honest in a
 *              way a stale number is not.
 *
 * Abbreviation is always declared. Whenever the displayed form is not the whole
 * value — compact notation, subscript zeros, a truncated fraction — the exact
 * value goes into `title` AND into a visually-hidden span, and the small
 * zero-run digit is hidden from assistive technology. A screen reader must
 * never read "zero point zero four nine seven two two".
 */
import type { ReactNode } from "react";

import { MoneyFormatError } from "../lib/money.ts";
import {
  absentFigure,
  formatBpsFigure,
  formatCount,
  formatMoney,
  formatPercent,
  formatUnits,
  type Figure as FigureData,
} from "../lib/format.ts";

/**
 * A value exactly as the backend holds it. Either form is exact; neither is
 * formatted. There is no third form.
 */
export type ExactValue =
  | { readonly decimal: string; readonly base?: never; readonly scale?: never }
  | { readonly base: string; readonly scale: number; readonly decimal?: never };

interface Common {
  /** Prefix a positive value with "+". P&L opts in; a balance does not. */
  readonly signed?: boolean;
  /** Allow K/M/B abbreviation. Never on a count. */
  readonly compact?: boolean;
  /** A unit that is part of the figure, e.g. "Credits". */
  readonly symbol?: string;
  /** What to say when the value is missing. */
  readonly absent?: string;
  /** The one big figure on the screen. */
  readonly big?: boolean;
  /** The snapshot behind this figure has gone stale. It goes faint, not blank. */
  readonly faint?: boolean;
  /** This value changed just now. Honoured only where the temperature allows. */
  readonly changed?: boolean;
}

export type FigureProps =
  | (Common & {
      readonly kind: "money";
      readonly value: ExactValue | null | undefined;
      /** Apply the three-decimal band either side of parity. Stablecoins only. */
      readonly stablecoin?: boolean;
    })
  | (Common & { readonly kind: "units"; readonly value: ExactValue | null | undefined })
  | (Common & { readonly kind: "percent"; readonly value: ExactValue | null | undefined })
  | (Common & { readonly kind: "bps"; readonly bps: number | null | undefined })
  | (Common & { readonly kind: "count"; readonly count: number | string | null | undefined });

function exactDecimal(value: ExactValue): string {
  return value.decimal ?? value.base;
}

/** Turns the props into a formatted figure, or reports why it could not. */
function compute(props: FigureProps): FigureData | MoneyFormatError {
  const symbol = props.symbol === undefined ? {} : { symbol: props.symbol };
  const shared = { signed: props.signed === true, compact: props.compact === true, ...symbol };
  try {
    switch (props.kind) {
      case "money": {
        if (props.value === null || props.value === undefined) {
          return absentFigure(props.absent);
        }
        const stable = props.stablecoin === true ? { stablecoin: true } : {};
        if (props.value.base !== undefined) {
          return formatUnits(props.value.base, props.value.scale, { ...shared, ...stable });
        }
        return formatMoney(exactDecimal(props.value), { ...shared, ...stable });
      }
      case "units": {
        if (props.value === null || props.value === undefined) {
          return absentFigure(props.absent);
        }
        if (props.value.base !== undefined) {
          return formatUnits(props.value.base, props.value.scale, shared);
        }
        return formatMoney(exactDecimal(props.value), shared);
      }
      case "percent": {
        if (props.value === null || props.value === undefined) {
          return absentFigure(props.absent);
        }
        return formatPercent(exactDecimal(props.value), { signed: props.signed !== false });
      }
      case "bps": {
        if (props.bps === null || props.bps === undefined) {
          return absentFigure(props.absent);
        }
        return formatBpsFigure(props.bps);
      }
      case "count": {
        if (props.count === null || props.count === undefined) {
          return absentFigure(props.absent);
        }
        return formatCount(props.count);
      }
    }
  } catch (error) {
    if (error instanceof MoneyFormatError) return error;
    throw error;
  }
}

function toneClass(figure: FigureData, signed: boolean): string {
  if (!signed) return "";
  if (figure.direction === "up") return " figure-up";
  if (figure.direction === "down") return " figure-down";
  return " figure-flat";
}

export function Figure(props: FigureProps): ReactNode {
  const computed = compute(props);

  // A value that does not match its contract is refused, not coerced. A
  // malformed figure rendered as a number is indistinguishable from a real one.
  if (computed instanceof MoneyFormatError) {
    return (
      <span className="malformed" role="status">
        unreadable value <span className="malformed-detail">({computed.message})</span>
      </span>
    );
  }

  if (computed.absent) {
    return (
      <span className="absent" title="The backend did not return a value for this field.">
        {props.absent ?? "not reported"}
      </span>
    );
  }

  // Direction is carried by the sign glyph first. Colour is a second channel on
  // top of it, and only where a sign was actually asked for: an unsigned
  // balance is not "positive", it is just a balance.
  const signed = props.signed === true || (props.kind === "percent" && props.signed !== false);
  const classes = [
    "num",
    "figure",
    props.big === true ? "figure-big" : "",
    props.faint === true ? "figure-faint" : "",
    toneClass(computed, signed).trim(),
  ]
    .filter((part) => part !== "")
    .join(" ");

  return (
    <span
      className={classes}
      title={computed.exact}
      {...(props.changed === true ? { "data-changed": "true" } : {})}
    >
      <span aria-hidden={computed.abbreviated ? "true" : undefined}>
        {computed.sign}
        {computed.head}
        {computed.zeroRun !== undefined && (
          // A <span> at 0.8em on the NORMAL baseline, never a <sub>. A <sub>
          // shifts the baseline, and a shifted digit breaks the alignment of a
          // numeric column, which is the one thing tabular figures protect.
          <span className="figure-run">{String(computed.zeroRun)}</span>
        )}
        {computed.tail}
        {/* A unit that is a word is set apart from the digits; a unit that is a
            glyph or an order of magnitude belongs against them. */}
        {computed.suffix.startsWith(" ") ? (
          <>
            {/* A real space, not a margin: it is the only break opportunity in
                the figure, and without it a narrow column spills into the one
                beside it rather than wrapping. */}
            {" "}
            <span className="figure-symbol">{computed.suffix.slice(1)}</span>
          </>
        ) : (
          computed.suffix
        )}
      </span>
      {computed.abbreviated && <span className="visually-hidden">{computed.exact}</span>}
    </span>
  );
}
