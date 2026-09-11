/**
 * The product screenshots — which are not screenshots.
 *
 * Goal §5 asks for "product screenshots/interactive UI where appropriate" and
 * forbids the generated-looking alternative. The honest way to do that in a
 * single-page application is not to take a picture of the app: it is to render
 * the app. Every composition below is the real `Panel`, the real `Figure` with
 * the real formatter, the real `DataTable` with its sticky header and keyboard
 * navigation, and the real `StatusBadge` — fed from fixed strings in
 * `src/content/example.ts`.
 *
 * That makes three things true at once. A visitor sees the typography and the
 * density they will actually get. The screenshots cannot go stale, because they
 * are the components. And nothing here can quietly become a fake dashboard,
 * because each surface declares `data-temp="simulated"`, which the design
 * system draws with a dashed border, a hatch, desaturated figures and a
 * persistent SIMULATED chip that takes no prop to suppress it.
 *
 * The figures are deliberately unremarkable and one of them is a loss. A
 * marketing surface showing a large gain is a fake metric with extra steps.
 */
import type { ReactNode } from "react";

import { DataTable, type Column } from "../../components/DataTable.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Panel } from "../../components/Panel.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import {
  EXAMPLE_AGENTS,
  EXAMPLE_CREDITS,
  EXAMPLE_CREDITS_GROSS,
  EXAMPLE_LABEL,
  EXAMPLE_MARKETS,
  type ExampleMarketRow,
} from "../../content/example.ts";
import { CREDITS_DISCLOSURE, NATIVE_ASSET_RISK, NATIVE_PRICE_NOTE } from "../../lib/honesty.ts";

/**
 * The Credits panel, as it appears on the dashboard.
 *
 * The three buckets are shown side by side and the gross figure is shown as the
 * one the backend returned — not as a sum of the three, because the browser
 * adding up money is exactly the arithmetic this product refuses to do.
 */
export function ExampleCredits(): ReactNode {
  return (
    <Panel
      title="Credits"
      description={EXAMPLE_LABEL}
      temp="simulated"
      actions={<StatusBadge tone="info">Example</StatusBadge>}
    >
      <FieldGrid columns={3}>
        {EXAMPLE_CREDITS.map((bucket) => (
          <Field key={bucket.label} label={bucket.label} note={bucket.note}>
            <Figure kind="units" value={{ decimal: bucket.decimal }} symbol="Credits" />
          </Field>
        ))}
      </FieldGrid>
      <FieldGrid columns={2}>
        <Field
          label="Total held"
          note="The figure the backend returned. It is not the three above added together in your browser."
          emphasis
        >
          <Figure kind="units" value={{ decimal: EXAMPLE_CREDITS_GROSS }} symbol="Credits" big />
        </Field>
        <Field
          label="What a Credit is"
          note="Shown wherever a Credit figure is, so the number never travels without it."
        >
          <span className="note">{CREDITS_DISCLOSURE}</span>
        </Field>
      </FieldGrid>
    </Panel>
  );
}

const MARKET_COLUMNS: ReadonlyArray<Column<ExampleMarketRow>> = [
  {
    key: "name",
    header: "Market",
    cell: (row) => (
      <span>
        {row.name} <span className="mono-small">{row.symbol}</span>
      </span>
    ),
  },
  {
    key: "price",
    header: "Last price",
    numeric: true,
    cell: (row) => <Figure kind="units" value={{ decimal: row.price }} symbol="Credits" />,
  },
  {
    key: "change",
    header: "24h",
    numeric: true,
    cell: (row) => <Figure kind="percent" value={{ decimal: row.change }} signed />,
  },
  {
    key: "reserve",
    header: "Real reserve",
    numeric: true,
    riskMeasure: true,
    cell: (row) => <Figure kind="units" value={{ decimal: row.reserve }} symbol="Credits" compact />,
  },
  {
    key: "status",
    header: "Status",
    cell: (row) => (
      <StatusBadge tone={row.status === "OPEN" ? "good" : "warn"}>{row.status}</StatusBadge>
    ),
  },
];

/**
 * The market list. One row is halted, because a list where everything is open
 * teaches a visitor something that is not true.
 */
export function ExampleMarkets(): ReactNode {
  return (
    <Panel
      title="Markets"
      description={EXAMPLE_LABEL}
      temp="simulated"
      actions={<StatusBadge tone="info">Example</StatusBadge>}
    >
      <DataTable
        caption="Example internal markets: name, last price in Credits, twenty-four hour change, the Credits that actually back the curve, and whether the market is open."
        columns={MARKET_COLUMNS}
        rows={EXAMPLE_MARKETS}
        rowKey={(row) => row.id}
      />
      <p className="note">{NATIVE_PRICE_NOTE}</p>
      <p className="note">{NATIVE_ASSET_RISK}</p>
    </Panel>
  );
}

/**
 * Two agents at the only authority levels this product permits. The page around
 * this says what levels four to six are, and that they are refused rather than
 * unavailable.
 */
export function ExampleAgents(): ReactNode {
  return (
    <Panel
      title="Agents"
      description={EXAMPLE_LABEL}
      temp="simulated"
      actions={<StatusBadge tone="info">Example</StatusBadge>}
    >
      {EXAMPLE_AGENTS.map((agent) => (
        <FieldGrid columns={4} key={agent.name}>
          <Field label="Agent">{agent.name}</Field>
          <Field label="Authority" note="What it is allowed to do at all.">
            {agent.authority}
          </Field>
          <Field label="Budget used" note="Against the Credit budget you set.">
            <Figure kind="units" value={{ decimal: agent.used }} symbol="Credits" />
          </Field>
          <Field label="State">
            <StatusBadge tone={agent.state === "ACTIVE" ? "good" : "neutral"}>
              {agent.state}
            </StatusBadge>
          </Field>
        </FieldGrid>
      ))}
      <p className="note">{CREDITS_DISCLOSURE}</p>
    </Panel>
  );
}
