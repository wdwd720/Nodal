/**
 * The temperature-aware container.
 *
 * Nodal holds three kinds of value that the spec forbids ever summing, and the
 * arithmetic side of that is already solved: there is no query hook that
 * produces a combined figure, so no component can render one by accident.
 *
 * This solves the other half — the perception problem. A label is one word in a
 * viewport full of numbers, and the moment a customer is moving fast, which is
 * exactly the moment the distinction matters most, a label is the first thing
 * that stops being read. So each pot renders at its own temperature, set by a
 * `data-temp` attribute that cascades to everything inside it:
 *
 *   real       Real Capital. A solid hairline, a 40px row, and NO motion on a
 *              value, ever.
 *   economy    The Nodal economy. Solid, denser rows, and a value is allowed to
 *              flash once when it changes, because the thing it describes is
 *              genuinely moving.
 *   simulated  A replay or an observation-only run. Dashed border, a 3%
 *              diagonal hatch, a persistent chip, and every figure inside
 *              desaturated.
 *
 * Three redundant channels — border, texture, chip — because hue alone fails a
 * colourblind reader, texture alone fails at small sizes and a chip alone is one
 * word among many. A future engineer will notice that two of the three are
 * redundant. They are. That is the point.
 *
 * The simulated chip takes no prop to suppress it. The case it exists for is a
 * screenshot pasted into a conversation with no surrounding context, and a
 * suppressible marker is a marker that will be suppressed.
 */
import type { ReactNode } from "react";

import { AsOf, useStaleness } from "./AsOf.tsx";
import { SimulatedChip } from "./StatusBadge.tsx";

export type Temperature = "economy" | "real" | "simulated";

export interface PanelProps {
  readonly title: string;
  readonly description?: string;
  readonly actions?: ReactNode;
  readonly children: ReactNode;
  readonly id?: string;
  /**
   * Which kind of value this panel holds. Omitted means the surrounding
   * temperature, and the document default is `economy`.
   */
  readonly temp?: Temperature;
  /**
   * The instant the backend stamped on the figures inside. Renders the stamp,
   * escalates it, and takes the whole panel faint when it goes stale.
   */
  readonly asOf?: string | undefined;
}

export function Panel(props: PanelProps): ReactNode {
  const tier = useStaleness(props.asOf);
  const simulated = props.temp === "simulated";
  const classes = ["panel", tier === "stale" ? "is-stale" : ""].filter((c) => c !== "").join(" ");

  return (
    <section
      className={classes}
      aria-label={props.title}
      {...(props.id === undefined ? {} : { id: props.id })}
      {...(props.temp === undefined ? {} : { "data-temp": props.temp })}
    >
      <div className="panel-head">
        <div>
          <h2>{props.title}</h2>
          {props.description !== undefined && (
            <p className="panel-description">{props.description}</p>
          )}
        </div>
        {(props.actions !== undefined || simulated) && (
          <div className="panel-actions">
            {simulated && <SimulatedChip />}
            {props.actions}
          </div>
        )}
      </div>
      <div className="panel-body">
        {props.children}
        {props.asOf !== undefined && <AsOf at={props.asOf} />}
      </div>
    </section>
  );
}

/**
 * A card inside a panel: one product, one market, one agent — the unit a
 * customer acts on.
 *
 * It is an `<article>` with its own accessible name because that is what it is,
 * and because the end-to-end suite locates a product by that name and then
 * reads that product's own price and clicks that product's own button. The
 * first version of that test read a price from one card and clicked another's,
 * and nothing caught it until a balance assertion did.
 *
 * Nesting stops here. A card inside a card inside a panel is the "everything in
 * a bordered box" pattern this design refuses.
 */
export function PanelCard(props: {
  readonly title: string;
  readonly description?: string;
  readonly actions?: ReactNode;
  readonly children: ReactNode;
  readonly temp?: Temperature;
  readonly headingLevel?: 3 | 4;
}): ReactNode {
  const simulated = props.temp === "simulated";
  const Heading = props.headingLevel === 4 ? "h4" : "h3";
  return (
    <article
      className="panel-nested"
      aria-label={props.title}
      {...(props.temp === undefined ? {} : { "data-temp": props.temp })}
    >
      <div className="panel-head">
        <div>
          <Heading>{props.title}</Heading>
          {props.description !== undefined && (
            <p className="panel-description">{props.description}</p>
          )}
        </div>
        {(props.actions !== undefined || simulated) && (
          <div className="panel-actions">
            {simulated && <SimulatedChip />}
            {props.actions}
          </div>
        )}
      </div>
      {props.children}
    </article>
  );
}
