/**
 * PORTFOLIO (goal §15, USER_JOURNEY §6).
 *
 * One read — `GET /v1/me/portfolio` — carries the Credit breakdown, every
 * position and the totals, all marked at one instant the server states. That
 * shape is the whole design: nothing on this page adds a position to another,
 * derives a P&L from a balance difference, or marks a position at a price of
 * its own choosing.
 *
 * Goal §15 is blunt about why: "Do not infer P&L from superficial balance
 * differences. Use authoritative accounting data." A browser that subtracted
 * cost basis from market value would be a second accounting implementation,
 * and it would disagree with the ledger on exactly the days it mattered — a
 * partial sale, a fee, a reversal. So realised, unrealised and total P&L are
 * three fields the server computed, shown as three fields.
 *
 * # One `as of`, stated once, applying to everything
 *
 * Every mark-to-market figure here was computed at `as_of`. It is on the panel
 * rather than on each row because it is one instant for all of them, and the
 * design system takes the whole region faint when it goes stale rather than
 * blanking it: an absent figure is a different claim from an old one.
 *
 * # Temperature is per position, not per page
 *
 * A sandbox-seeded market and a real one can sit in the same table, so each
 * position renders at the temperature the API gave it. A panel-wide
 * temperature would mislabel every row but one.
 *
 * # What is NOT summed
 *
 * The Credits breakdown and the position totals stay apart, because they are
 * different kinds of value and §46 forbids the interface treating them as one.
 * Credits are what may be spent; a position's market value is what the curve
 * would pay for it right now, which is not the same thing and is not money.
 */
import type { ReactNode } from "react";

import { API_BASE } from "../../api/client.ts";
import {
  usePortfolio,
  type CreditBalance,
  type Portfolio as PortfolioData,
  type PortfolioPosition,
  type PortfolioTotals,
} from "../../api/queries.ts";
import { DownloadLink, LinkButton } from "../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Disclosure, Page, Panel } from "../../components/Layout.tsx";
import { SegmentedBar } from "../../components/SegmentedBar.tsx";
import { SkeletonField } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { Temp, temperatureOf } from "../../components/Temperature.tsx";
import { creditScale, hasCredits } from "../../lib/credits.ts";
import { EMPTY_STATES } from "../../lib/errors.ts";
import { CREDITS_DISCLOSURE, NATIVE_ASSET_RISK, PROVENANCE_NOTE } from "../../lib/honesty.ts";
import { useActiveAccountId } from "../../session.tsx";

const MARKET_TONE: Readonly<Record<string, "good" | "warn" | "bad" | "neutral">> = {
  ACTIVE: "good",
  PENDING: "neutral",
  CLOSE_ONLY: "warn",
  HALTED: "warn",
  FROZEN: "bad",
  DELISTED: "bad",
};

export function Portfolio(): ReactNode {
  const accountId = useActiveAccountId();
  const portfolio = usePortfolio(accountId);

  if (accountId === undefined) {
    return (
      <Page title="Portfolio">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is nothing to hold. That is the backend's answer, not a loading state."
        />
      </Page>
    );
  }

  return (
    <Page
      title="Portfolio"
      lead="What this account holds, what it cost, and what the market would pay for it at the instant below."
      actions={
        <DownloadLink
          href={`${API_BASE}/accounts/${accountId}/export?format=csv`}
          fileName="nodal-account-export.csv"
        >
          Export as CSV
        </DownloadLink>
      }
    >
      <AsyncPanel
        query={portfolio}
        loadingLabel="Asking the backend for this account's portfolio…"
        skeleton={
          <Panel title="Credits">
            <FieldGrid columns={3}>
              <SkeletonField label="Total Credits" />
              <SkeletonField label="Spendable" />
              <SkeletonField label="Frozen" />
            </FieldGrid>
          </Panel>
        }
      >
        {(data: PortfolioData) => <Held data={data} />}
      </AsyncPanel>

      <Disclosure title="What Credits are">
        <p>{CREDITS_DISCLOSURE}</p>
        <p>{PROVENANCE_NOTE}</p>
      </Disclosure>
      <Disclosure title="What a position in a Nodal-native asset is">
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

function Held(props: { readonly data: PortfolioData }): ReactNode {
  const { data } = props;
  const credits = data.credits as CreditBalance;
  const totals = data.totals as PortfolioTotals;
  // The scale the response states, not one this page assumes (F-151).
  const creditScaleNow = creditScale(credits.credit_decimals);
  const creditValue = (base: string): { readonly base: string; readonly scale: number } => ({
    base,
    scale: creditScaleNow,
  });

  const columns: ReadonlyArray<Column<PortfolioPosition>> = [
    {
      key: "asset",
      header: "Asset",
      compare: (a, b) => (a.symbol < b.symbol ? -1 : a.symbol > b.symbol ? 1 : 0),
      cell: (row) => (
        <span>
          <strong>{row.symbol}</strong>
          {row.name === undefined ? "" : ` · ${row.name}`}
        </span>
      ),
    },
    {
      key: "status",
      header: "Market",
      cell: (row) =>
        row.market_status === undefined ? (
          <span className="absent">no market reported</span>
        ) : (
          <StatusBadge tone={MARKET_TONE[row.market_status] ?? "neutral"}>
            {row.market_status}
          </StatusBadge>
        ),
    },
    {
      key: "quantity",
      header: "Quantity",
      numeric: true,
      cell: (row) => (
        <Temp value={row.temperature}>
          <Figure
            kind="units"
            value={{ base: row.quantity, scale: row.asset_decimals }}
            symbol={row.symbol}
          />
        </Temp>
      ),
    },
    {
      key: "average-cost",
      header: "Average cost",
      numeric: true,
      cell: (row) => (
        <Temp value={row.temperature}>
          <Figure
            kind="units"
            value={
              row.average_cost_credits === undefined
                ? null
                : { base: row.average_cost_credits, scale: row.price_scale }
            }
            symbol="Credits"
            absent="position closed"
          />
        </Temp>
      ),
    },
    {
      key: "cost-basis",
      header: "Cost basis",
      numeric: true,
      cell: (row) => (
        <Temp value={row.temperature}>
          <Figure kind="units" value={creditValue(row.cost_basis_credits)} symbol="Credits" />
        </Temp>
      ),
    },
    {
      key: "market-value",
      header: "Market value",
      numeric: true,
      cell: (row) => (
        <Temp value={row.temperature}>
          <Figure kind="units" value={creditValue(row.market_value_credits)} symbol="Credits" />
        </Temp>
      ),
    },
    {
      key: "unrealised",
      header: "Unrealised",
      numeric: true,
      riskMeasure: true,
      cell: (row) => (
        <Temp value={row.temperature}>
          <Figure
            kind="units"
            value={creditValue(row.unrealized_pnl_credits)}
            symbol="Credits"
            signed
          />
        </Temp>
      ),
    },
    {
      key: "realised",
      header: "Realised",
      numeric: true,
      cell: (row) => (
        <Temp value={row.temperature}>
          <Figure
            kind="units"
            value={creditValue(row.realized_pnl_credits)}
            symbol="Credits"
            signed
          />
        </Temp>
      ),
    },
  ];

  return (
    <>
      <Panel
        title="Credits"
        description="What this account holds inside Nodal. Never added to the position values below: they are different kinds of value."
        temp={temperatureOf(data.temperature)}
      >
        <FieldGrid columns={3}>
          <Field label="Total Credits" note="Everything held, whatever may be done with it." emphasis>
            <Figure kind="units" value={creditValue(credits.gross)} symbol="Credits" big />
          </Field>
          <Field label="Spendable" note="Usable inside Nodal right now.">
            <Figure kind="units" value={creditValue(credits.spendable)} symbol="Credits" />
          </Field>
          <Field label="Frozen" note="Held by a restriction or an open dispute.">
            <Figure kind="units" value={creditValue(credits.frozen)} symbol="Credits" />
          </Field>
          {/* Shown only when it is not zero; see Home for why it exists at all
              (F-156). */}
          {hasCredits(credits.reversed) && (
            <Field
              label="Removed"
              note="Removed after a payment was reversed. Not spendable and not withdrawable."
            >
              <Figure kind="units" value={creditValue(credits.reversed)} symbol="Credits" />
            </Field>
          )}
        </FieldGrid>
        <SegmentedBar
          caption="How this balance is held"
          scale={creditScaleNow}
          symbol="Credits"
          total={credits.gross}
          segments={[
            {
              key: "spendable",
              label: "Spendable",
              baseUnits: credits.spendable,
              explanation: "Usable inside Nodal right now.",
              texture: "solid",
            },
            {
              key: "frozen",
              label: "Frozen",
              baseUnits: credits.frozen,
              explanation: "Held by a restriction or an open dispute.",
              texture: "hatch",
            },
            {
              key: "reversed",
              label: "Removed",
              baseUnits: credits.reversed,
              explanation:
                "Removed after a payment was reversed. Not spendable and not withdrawable.",
              texture: "sparse",
            },
          ]}
        />
        <FieldGrid columns={2}>
          <Field label="Payout-eligible" note="What a payout policy would consider.">
            <Figure kind="units" value={creditValue(credits.payout_eligible)} symbol="Credits" />
          </Field>
          <Field label="Not payout-eligible" note="Everything else in this balance.">
            <Figure kind="units" value={creditValue(credits.ineligible)} symbol="Credits" />
          </Field>
        </FieldGrid>
        <p className="field-note">
          Payout-eligible value is a quantity of Credits, not an amount of US dollars. It is the part
          of this balance a payout policy would consider if an approved payout path were active.
        </p>
      </Panel>

      <Panel
        title="Positions"
        description="Marked at each market's marginal price at the instant below. A marginal price is what the NEXT base unit costs, not what the whole position would fetch."
        asOf={data.as_of}
      >
        <DataTable<PortfolioPosition>
          caption="Every position, with quantity, average cost, market value and profit or loss"
          rows={data.positions}
          rowKey={(row) => row.asset_id}
          columns={columns}
          tall
          empty={{ title: EMPTY_STATES.holdings.title, body: EMPTY_STATES.holdings.body }}
        />
        {data.positions.length === 0 && (
          <div className="form-actions">
            <LinkButton to="/markets" variant="primary">
              {EMPTY_STATES.holdings.action ?? "Explore markets"}
            </LinkButton>
          </div>
        )}
      </Panel>

      <Panel
        title="Totals"
        description="Computed by the backend from the same read model the rows came from. Nothing on this page adds the rows up."
        asOf={data.as_of}
      >
        <FieldGrid columns={3}>
          <Field label="Positions held" note="Open positions, of every position ever opened.">
            <Figure kind="count" count={totals.open_position_count} />
            <p className="field-note">of {String(totals.position_count)} ever opened</p>
          </Field>
          <Field label="Cost basis">
            <Figure kind="units" value={creditValue(totals.cost_basis_credits)} symbol="Credits" />
          </Field>
          <Field label="Market value">
            <Figure kind="units" value={creditValue(totals.market_value_credits)} symbol="Credits" />
          </Field>
          <Field label="Unrealised" note="Marked at the instant above. Not money, and not realised.">
            <Figure
              kind="units"
              value={creditValue(totals.unrealized_pnl_credits)}
              symbol="Credits"
              signed
            />
          </Field>
          <Field label="Realised" note="Locked in by trades that have already happened.">
            <Figure
              kind="units"
              value={creditValue(totals.realized_pnl_credits)}
              symbol="Credits"
              signed
            />
          </Field>
          <Field label="Total" note="The backend's own total. This page does not add the two above.">
            <Figure
              kind="units"
              value={creditValue(totals.total_pnl_credits)}
              symbol="Credits"
              signed
            />
          </Field>
          <Field label="Fees paid" note="Platform and creator fees taken on trades.">
            <Figure kind="units" value={creditValue(totals.fees_paid_credits)} symbol="Credits" />
          </Field>
        </FieldGrid>
      </Panel>
    </>
  );
}
