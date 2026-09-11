/**
 * The API's temperature, applied to whatever renders it.
 *
 * `UI_UX_SYSTEM.md` gives every financial surface one of three temperatures and
 * makes `data-temp` cascade from any container, so a figure inside a `real`
 * panel renders as real without being told. `Panel` and `PanelCard` set it for
 * a whole region.
 *
 * What was missing is the row-level case, and it is not a corner: the portfolio
 * and the activity feed carry a `ValueTemperature` PER ITEM. One position can
 * be economy and the next simulated, and one activity row can describe a
 * sandbox rehearsal sitting directly above a real purchase. Putting them in one
 * panel at one temperature would mislabel every row but one.
 *
 * So this is the smallest thing that closes it: a span that carries the
 * attribute, with the API's vocabulary mapped to the design system's. It adds
 * no styling of its own — every rule already exists in `base.css`, keyed off
 * `[data-temp]` — and it renders no wrapper at all when the temperature is the
 * document default, because an element that changes nothing is noise in the
 * tree a screen reader walks.
 *
 * `simulated` is deliberately NOT suppressible here either. The API says a
 * figure came from a rehearsal; the interface's job is to show that, not to
 * decide the reader would rather not know.
 */
import type { ReactNode } from "react";

import type { Temperature } from "./Panel.tsx";

/** The API's `ValueTemperature` vocabulary. */
export type ValueTemperatureName = "ECONOMY" | "REAL" | "SIMULATED";

const BY_NAME: Readonly<Record<string, Temperature>> = {
  ECONOMY: "economy",
  REAL: "real",
  SIMULATED: "simulated",
};

/**
 * The design-system temperature for what the API said.
 *
 * An unrecognised value is `simulated` rather than `economy`. That direction is
 * the safe one: labelling a real figure as a rehearsal is a conservative
 * mistake that a reader can check, and labelling a rehearsal as real is the
 * mistake the whole temperature system exists to prevent.
 */
export function temperatureOf(value: string | undefined): Temperature {
  if (value === undefined) return "economy";
  return BY_NAME[value] ?? "simulated";
}

/**
 * Renders its children at the temperature the API declared.
 *
 * ```tsx
 * <Temp value={position.temperature}>
 *   <Figure kind="units" value={...} symbol="Credits" />
 * </Temp>
 * ```
 */
export function Temp(props: {
  readonly value: string | undefined;
  readonly children: ReactNode;
}): ReactNode {
  const temp = temperatureOf(props.value);
  // `economy` is the document default, so wrapping in it would add an element
  // that does nothing.
  if (temp === "economy") return <>{props.children}</>;
  return <span data-temp={temp}>{props.children}</span>;
}
