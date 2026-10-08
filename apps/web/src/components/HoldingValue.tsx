/**
 * A holding's USD figures, or why it has none.
 *
 * The holdings answer says whether the backend values a holding in USD. When
 * it does, its mark, cost basis and profit and loss are drawn as sent; when it
 * does not, the cell where the mark would be says so and why, and the other
 * cells say there is no figure. Nothing is drawn as a zero in place of a
 * valuation that does not exist (src/lib/honesty.ts).
 */
import type { ReactNode } from "react";

import type { Holding } from "../api/queries.ts";
import { UNVALUED_REASON_COPY, unvaluedFigureNote } from "../lib/holdings.ts";
import { Pill } from "./Layout.tsx";
import { Usd } from "./Money.tsx";

/** The USD mark, or the reason the holding has none. */
export function HoldingMark(props: { readonly holding: Holding }): ReactNode {
  const h = props.holding;
  if (h.valuation_status === "VALUED") {
    return (
      <span data-valuation="VALUED">
        <Usd value={h.usd_mark} />
      </span>
    );
  }
  const reason = h.unvalued_reason;
  return (
    <div data-valuation="UNVALUED" {...(reason === undefined ? {} : { "data-reason": reason })}>
      <Pill tone="warn">Unvalued</Pill>
      {reason !== undefined && (
        <>
          <p className="cell-prose">
            <strong>{UNVALUED_REASON_COPY[reason].label}.</strong> {UNVALUED_REASON_COPY[reason].detail}
          </p>
          <p className="mono-small">reason {reason}</p>
        </>
      )}
    </div>
  );
}

/** One of the other USD figures of a holding: as sent, or what its absence means. */
export function HoldingFigure(props: {
  readonly holding: Holding;
  readonly figure: "basis" | "unrealized" | "realized";
}): ReactNode {
  const h = props.holding;
  const reason = h.valuation_status === "UNVALUED" ? h.unvalued_reason : undefined;
  switch (props.figure) {
    case "basis":
      return <Usd value={h.cost_basis_usd} {...(reason === undefined ? {} : { absent: unvaluedFigureNote(reason, "basis") })} />;
    case "unrealized":
      return (
        <Usd
          value={h.unrealized_pnl_usd}
          signed
          {...(reason === undefined ? {} : { absent: unvaluedFigureNote(reason, "unrealized") })}
        />
      );
    case "realized":
      return (
        <Usd
          value={h.realized_pnl_usd}
          signed
          absent={reason === "SIMULATED_VALUE" ? unvaluedFigureNote(reason, "basis") : "not reported for this holding"}
        />
      );
  }
}

/** The price reference a mark came from, or that no price was used. */
export function HoldingPriceRef(props: { readonly holding: Holding }): ReactNode {
  const h = props.holding;
  if (h.price_ref !== undefined && h.price_ref !== "") return <span className="mono-small">{h.price_ref}</span>;
  const reason = h.valuation_status === "UNVALUED" ? h.unvalued_reason : undefined;
  return <span className="absent">{reason === undefined ? "not reported" : unvaluedFigureNote(reason, "price")}</span>;
}
