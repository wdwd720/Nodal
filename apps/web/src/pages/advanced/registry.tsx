/**
 * The sections of the advanced (sandbox) area.
 *
 * AdvancedArea renders exactly this list: one sub-navigation link per entry,
 * in this order, and one route per entry at `/advanced/<path>/*`, so a
 * section may own nested routes (a list and a detail) with its own <Routes>.
 *
 * How a subsystem adds its section, and the only two places it touches here:
 *
 *   1. ONE import line, directly below its `// >>> advanced:<sub> import`
 *      marker;
 *   2. ONE entry, a single object literal, directly below its
 *      `// >>> advanced:<sub> section` marker, e.g.
 *      { path: "margin", title: "Margin", description: "…", element: <MarginSection /> },
 *
 * Never edit, reorder or remove a marker: `advanced-registry.test.ts` checks
 * that every marker is present, in order, in both places, and that every path
 * is unique. Every page a section renders must show <SimulatedNotice />;
 * `honesty.test.ts` checks that too.
 */
import type { ReactElement } from "react";

import { BuyingPowerSection } from "./core/BuyingPower.tsx";
import { EconomicActivitySection } from "./core/EconomicActivity.tsx";
import { InstrumentsSection } from "./core/Instruments.tsx";
import { Overview } from "./core/Overview.tsx";

// >>> advanced:margin import

// >>> advanced:derivatives import
import { DerivativesSection } from "./derivatives/DerivativesSection.tsx";

// >>> advanced:equities import

// >>> advanced:orders import

// >>> advanced:transfers import

// >>> advanced:treasury import

// >>> advanced:stable import

// >>> advanced:bridge import

// >>> advanced:predmarket import

// >>> advanced:marketplace import

// >>> advanced:delegation import

// >>> advanced:operator import

/** One section of the advanced area. A registry entry is a single object literal of this type. */
export interface AdvancedSection {
  /** URL segment under /advanced/: lowercase letters, digits and hyphens, unique in the registry. */
  readonly path: string;
  /** The sub-navigation label, and the heading of the section's landing page. */
  readonly title: string;
  /** One sentence on what the section shows, listed on the overview. */
  readonly description: string;
  /** What the route renders; a section with sub-pages renders its own <Routes>. */
  readonly element: ReactElement;
}

/** The section `/advanced` opens on. */
export const ADVANCED_DEFAULT_SECTION = "overview";

export const ADVANCED_SECTIONS: readonly AdvancedSection[] = [
  {
    path: "overview",
    title: "Overview",
    description:
      "Every advanced capability, its gate and kill switch, and the decision for every mode in this deployment, LIVE included.",
    element: <Overview />,
  },
  {
    path: "buying-power",
    title: "Buying power V2",
    description:
      "What the account's simulated holdings can do, with every component, the backend's explanation lines and its reasons.",
    element: <BuyingPowerSection />,
  },
  {
    path: "activity",
    title: "Economic activity",
    description:
      "The account's economic intents, its exactly-once economic effects and the Risk Kernel V2 decisions about them.",
    element: <EconomicActivitySection />,
  },
  {
    path: "instruments",
    title: "Instruments",
    description: "The canonical instruments, the terms in force for each, and every version those terms have had.",
    element: <InstrumentsSection />,
  },

  // >>> advanced:margin section

  // >>> advanced:derivatives section
  {
    path: "derivatives",
    title: "Derivatives",
    description:
      "Futures, perpetuals and options in paper and sandbox mode: contracts and marks, positions valued by the backend, the clearing-house flow register, funding, and reference option analytics.",
    element: <DerivativesSection />,
  },

  // >>> advanced:equities section

  // >>> advanced:orders section

  // >>> advanced:transfers section

  // >>> advanced:treasury section

  // >>> advanced:stable section

  // >>> advanced:bridge section

  // >>> advanced:predmarket section

  // >>> advanced:marketplace section

  // >>> advanced:delegation section

  // >>> advanced:operator section
];
