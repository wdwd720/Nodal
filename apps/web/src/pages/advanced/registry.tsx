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
import { MarginSection } from "./margin/MarginSection.tsx";

// >>> advanced:derivatives import
import { DerivativesSection } from "./derivatives/DerivativesSection.tsx";

// >>> advanced:equities import
import { EquitiesSection } from "./equities/EquitiesSection.tsx";

// >>> advanced:orders import
import { OrderBooksSection } from "./orders/OrderBooksSection.tsx";

// >>> advanced:transfers import
import { TransfersSection } from "./transfers/Transfers.tsx";

// >>> advanced:treasury import
import { TreasurySection } from "./treasury/TreasurySection.tsx";
import { FeesSection } from "./fees/FeesSection.tsx";

// >>> advanced:stable import
import { StableAssetsSection } from "./stable/StableSection.tsx";

// >>> advanced:bridge import
import { BridgeSection } from "./bridge/BridgeSection.tsx";

// >>> advanced:predmarket import
import { PredictionMarketsSection } from "./predmarket/PredictionMarkets.tsx";

// >>> advanced:marketplace import
import { StrategiesSection } from "./marketplace/StrategiesSection.tsx";

// >>> advanced:delegation import

// >>> advanced:operator import
import { OperatorConsole } from "./operator/OperatorConsole.tsx";

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
  {
    path: "margin",
    title: "Margin",
    description:
      "The account's margin account as the margin engine evaluates it — collateral and haircuts, equity, margin, leverage, loans, calls and liquidation cases — its commands, and the operator's liquidation queue.",
    element: <MarginSection />,
  },

  // >>> advanced:derivatives section
  {
    path: "derivatives",
    title: "Derivatives",
    description:
      "Futures, perpetuals and options in paper and sandbox mode: contracts and marks, positions valued by the backend, the clearing-house flow register, funding, and reference option analytics.",
    element: <DerivativesSection />,
  },

  // >>> advanced:equities section
  {
    path: "equities",
    title: "Equities",
    description:
      "Equities and ETFs in paper and sandbox mode: listings with the session in force, orders filled by a deterministic paper venue, every disclosed charge, T+1 settlement and corporate actions.",
    element: <EquitiesSection />,
  },

  // >>> advanced:orders section
  {
    path: "order-books",
    title: "Order books",
    description:
      "The internal order books: disclosed fee schedules and crossing rule, depth, limit and market orders between accounts, the account's orders and trades, and each trade's audit proof.",
    element: <OrderBooksSection />,
  },

  // >>> advanced:transfers section
  {
    path: "transfers",
    title: "Transfers",
    description:
      "Simulated value sent between customers by handle, every transfer the account sent or received with its status and evidence, and sandbox dollars from the faucet.",
    element: <TransfersSection />,
  },

  // >>> advanced:treasury section
  {
    path: "treasury",
    title: "Treasury",
    description:
      "The operator's house books: simulated house capital only, each under its own signing policy and risk limits — positions and PnL at fresh marks, limits in use, balances, reconciliation, house orders, quotes and paper-venue fills, and the disclosed house-versus-customer crossing rules.",
    element: <TreasurySection />,
  },
  {
    path: "fees",
    title: "Fees",
    description:
      "Every published fee schedule with its version history and an exact quote calculator, this account's quote disclosures, fee bookings with their spread lines, performance-fee periods with their high-water marks, and the house trades against it.",
    element: <FeesSection />,
  },

  // >>> advanced:stable section
  {
    path: "stable-assets",
    title: "Stable assets",
    description:
      "The platform's sandbox stable assets on a simulated chain: issuing and redeeming against a fully reserved policy in the sandbox dollar, the reserve and its coverage, signed reserve snapshots anyone can verify, and the operator's controls.",
    element: <StableAssetsSection />,
  },

  // >>> advanced:bridge section
  {
    path: "bridge",
    title: "Bridge",
    description:
      "Simulated holdings by chain, value in flight between the simulated chains and the haircut it counts at, bridge routes and transfers with every leg's progress on both chains, the chains' state, and the operator's controls.",
    element: <BridgeSection />,
  },

  // >>> advanced:predmarket section
  {
    path: "prediction-markets",
    title: "Prediction markets",
    description:
      "Markets on the outcome of an event: complete sets against the sandbox dollar, resolved by a verified oracle report and settled from each market's own escrow.",
    element: <PredictionMarketsSection />,
  },

  // >>> advanced:marketplace section
  {
    path: "strategies",
    title: "Strategies",
    description:
      "Sandboxed strategies: the approved catalogue, publishing and forking, PAPER installs with their paper-follow history and risk decisions, sandbox runs, and the operators' review queue.",
    element: <StrategiesSection />,
  },

  // >>> advanced:delegation section

  // >>> advanced:operator section
  {
    path: "operator",
    title: "Operator console",
    description:
      "For operators: the dual-control items awaiting a second operator, the sandbox reference marks and their freshness, circuit breakers, capability gates, the liquidation queue, reconciliation and reserve results, the simulated chains, and each subsystem's operator page.",
    element: <OperatorConsole />,
  },
];
