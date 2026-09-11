/**
 * HOME (goal §8, USER_JOURNEY §3).
 *
 * The modules are in the order §3 sets, and the order is the argument: Credits
 * first, because that is what the product runs on; then what is owned, then
 * what is acting on the owner's behalf, then what is moving, then what has
 * happened. A dashboard that leads with a headline total teaches somebody to
 * read one number and stop, and the one number that would sit there is the one
 * that is true of nothing.
 *
 * # What this page never does
 *
 * It never adds a Credit figure to anything, and it never shows a placeholder
 * where a figure has not arrived — a `Skeleton` says "a balance is coming",
 * a zero says "you have nothing", and only one of those is honest while a
 * request is in flight. Buying power, portfolio value and Credit balances all
 * come from the backend already computed; nothing on this page derives one.
 *
 * # Internal Credits and payout-eligible value are separated structurally
 *
 * Goal §8 asks for the distinction and §46 forbids treating "available",
 * "spendable", "withdrawable" and "settled" as synonyms. So they are two
 * different blocks answering two different questions — what may be SPENT
 * inside Nodal, and what a payout policy would CONSIDER — rather than two
 * labels on one row, and the payout block says in words that it is a quantity
 * of Credits and not an amount of US dollars.
 *
 * # A module whose data does not exist does not appear
 *
 * The portfolio and holdings modules render nothing at all until there is a
 * position — §3 says "only when positions exist", and that is a condition on
 * the data rather than a layout preference. A portfolio panel reading zero on
 * an account that has never traded is not an empty state; it is a claim that
 * something was lost. The invitation to start is the markets module, which is
 * already on the page.
 */
import type { ReactNode } from "react";

import {
  useAgents,
  useCreditBalance,
  useMarketDiscovery,
  useMeActivity,
  usePortfolio,
  type ActivityFeed,
  type Agent,
  type AgentList,
  type CreditBalance,
  type MarketPage,
  type NativeMarketSummary,
  type PortfolioPosition,
  type PortfolioTotals,
} from "../../api/queries.ts";
import { LinkButton } from "../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Disclosure, Page, Panel, PanelCard } from "../../components/Layout.tsx";
import { SegmentedBar } from "../../components/SegmentedBar.tsx";
import { Skeleton, SkeletonField } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { Temp, temperatureOf } from "../../components/Temperature.tsx";
import { CREDIT_DECIMALS } from "../../lib/credits.ts";
import { EMPTY_STATES } from "../../lib/errors.ts";
import { CREDITS_DISCLOSURE, NATIVE_ASSET_RISK, PROVENANCE_NOTE } from "../../lib/honesty.ts";
import { formatInstant } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";

/** How many holdings the dashboard shows before sending the reader to the page. */
const TOP_HOLDINGS = 5;

/**
 * The sentence goal §8 asks for, in the one place it belongs: beside the
 * payout-eligible figure, not in a footnote at the bottom of the page.
 */
const PAYOUT_ELIGIBLE_SENTENCE =
  "Payout-eligible value is a quantity of Credits, not an amount of US dollars. It is the part of " +
  "this balance a payout policy would consider if an approved payout path were active; it is not " +
  "an amount held for you anywhere, it is not a bank deposit, and it is not insured.";

/** The four actions goal §8 names. Withdraw is here for everybody, always. */
function PrimaryActions(): ReactNode {
  return (
    <>
      <LinkButton to="/buy-credits" variant="primary">
        Buy Credits
      </LinkButton>
      <LinkButton to="/markets" variant="secondary">
        Trade
      </LinkButton>
      <LinkButton to="/agents/new" variant="secondary">
        Create Agent
      </LinkButton>
      {/* Never hidden from an unverified customer (goal §8): the page it opens
          explains what verification is for, which is the whole point. */}
      <LinkButton to="/withdraw" variant="secondary">
        Withdraw
      </LinkButton>
    </>
  );
}

export function Home(): ReactNode {
  const accountId = useActiveAccountId();
  const credits = useCreditBalance(accountId);
  const agents = useAgents(accountId);
  const portfolio = usePortfolio(accountId);
  // CHANGE_24H is the ordering §3 means by "movers": what moved, not what is
  // newest. The server applies it and says so in the response, so this page
  // never re-ranks what it was given.
  const movers = useMarketDiscovery({ sort: "CHANGE_24H", limit: 5 });
  const recent = useMeActivity({ accountId, kinds: [], limit: 5 });

  if (accountId === undefined) {
    return (
      <Page title="Home" actions={<PrimaryActions />}>
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is no balance to show. That is the backend's answer, not a loading state."
        />
      </Page>
    );
  }

  return (
    <Page
      title="Home"
      lead="Every figure here was computed by the backend. Nothing on this page adds two kinds of value together."
      actions={<PrimaryActions />}
    >
      {/* 1. Credits. */}
      <Panel
        title="Credits"
        description="What this account holds inside Nodal, and what may be done with each part of it."
        temp="economy"
      >
        <AsyncPanel
          query={credits}
          loadingLabel="Asking the backend for the Credit balance…"
          skeleton={
            <FieldGrid columns={3}>
              <SkeletonField label="Total Credits" />
              <SkeletonField label="Spendable" />
              <SkeletonField label="Frozen" />
            </FieldGrid>
          }
        >
          {(balance: CreditBalance) => <Credits balance={balance} />}
        </AsyncPanel>
      </Panel>

      {/* 2 and 3. Portfolio value, today's change and the top holdings.
          USER_JOURNEY §3 says "only when positions exist", and that is a
          condition on the DATA rather than a layout preference: a portfolio
          panel reading zero on an account that has never traded teaches
          somebody that they lost something. */}
      <PortfolioModules query={portfolio} />

      {/* 4. Active agents. */}
      <Panel
        title="Agents"
        description="What is acting on this account's behalf, and the authority each one was granted."
        temp="economy"
      >
        <AsyncPanel
          query={agents}
          loadingLabel="Asking the backend for this account's agents…"
          skeleton={<Skeleton shape="rows" count={3} label="The agent list is loading" />}
          empty={{
            // `useAgents` answers with the whole page — the roster AND the
            // declared authority levels, which only the page response carries.
            // This panel wants the roster; `/agents` wants both.
            isEmpty: (page: AgentList) => page.items.length === 0,
            title: EMPTY_STATES.agents.title,
            body: EMPTY_STATES.agents.body,
          }}
        >
          {(page: AgentList) => <Agents list={page.items} />}
        </AsyncPanel>
      </Panel>

      {/* 5. Markets: the movers. */}
      <Panel
        title="Markets"
        description="The biggest moves over the last 24 hours, in the ordering the backend applied."
        temp="economy"
      >
        <AsyncPanel
          query={movers}
          loadingLabel="Asking the backend which markets moved…"
          skeleton={<Skeleton shape="rows" count={5} label="The market list is loading" />}
          empty={{
            isEmpty: (page: MarketPage) => page.markets.length === 0,
            title: EMPTY_STATES.markets.title,
            body: EMPTY_STATES.markets.body,
          }}
        >
          {(page: MarketPage) => <Movers page={page} />}
        </AsyncPanel>
      </Panel>

      {/* 6. Recent activity. */}
      <Panel
        title="Recent activity"
        description="The last few things that happened, each as the backend summarised it."
        temp="economy"
      >
        <AsyncPanel
          query={recent}
          loadingLabel="Asking the backend what has happened…"
          skeleton={<Skeleton shape="rows" count={5} label="Recent activity is loading" />}
          empty={{
            isEmpty: (page: ActivityFeed) => page.items.length === 0,
            title: EMPTY_STATES.activity.title,
            body: EMPTY_STATES.activity.body,
          }}
        >
          {(page: ActivityFeed) => <Recent page={page} />}
        </AsyncPanel>
      </Panel>

      <Disclosure title="What Credits are">
        <p>{CREDITS_DISCLOSURE}</p>
        <p>{PROVENANCE_NOTE}</p>
      </Disclosure>
      <Disclosure title="What a Nodal-native asset is">
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

/**
 * Portfolio value, the change since the position was opened, and the top
 * holdings — rendered ONLY when there are positions.
 *
 * USER_JOURNEY §3 makes that conditional and it is the honest reading: a
 * portfolio panel showing zero on an account that has never traded is not an
 * empty state, it is a claim that something was lost. The invitation to trade
 * belongs on the markets module below, which is already there.
 *
 * "Today's change" is not what is shown, and the difference matters. The API
 * carries unrealised P&L against cost basis, which is the change since each
 * position was OPENED, and there is no since-midnight figure anywhere in the
 * response. Labelling the one as the other would be the interface renaming a
 * number to match a heading somebody wanted.
 */
function PortfolioModules(props: {
  readonly query: ReturnType<typeof usePortfolio>;
}): ReactNode {
  const data = props.query.data;
  // Nothing at all while it loads: a panel that appears and then vanishes is
  // worse than one that arrives late, and this panel's whole point is that its
  // presence means something.
  if (data === undefined || data.positions.length === 0) return null;

  const totals = data.totals as PortfolioTotals;
  const top = [...data.positions]
    .filter((position) => /[1-9]/.test(position.quantity))
    .slice(0, TOP_HOLDINGS);

  return (
    <>
      <Panel
        title="Portfolio value"
        description="What the markets would pay for these positions at the instant below. Not Credits, and never added to them."
        temp={temperatureOf(data.temperature)}
        asOf={data.as_of}
        actions={
          <LinkButton to="/portfolio" variant="secondary">
            Open portfolio
          </LinkButton>
        }
      >
        <FieldGrid columns={3}>
          <Field label="Market value" note="Marked at each market's marginal price." emphasis>
            <Figure
              kind="units"
              value={{ base: totals.market_value_credits, scale: CREDIT_DECIMALS }}
              symbol="Credits"
              big
            />
          </Field>
          <Field
            label="Change since opened"
            note="Unrealised, against what these positions cost. It is not a since-midnight figure; the API reports no such thing."
          >
            <Figure
              kind="units"
              value={{ base: totals.unrealized_pnl_credits, scale: CREDIT_DECIMALS }}
              symbol="Credits"
              signed
            />
          </Field>
          <Field label="Realised" note="Locked in by trades that have already happened.">
            <Figure
              kind="units"
              value={{ base: totals.realized_pnl_credits, scale: CREDIT_DECIMALS }}
              symbol="Credits"
              signed
            />
          </Field>
        </FieldGrid>
      </Panel>

      <Panel
        title="Holdings"
        description="The largest positions by market value, as the backend ordered them."
        temp="economy"
        asOf={data.as_of}
      >
        <DataTable<PortfolioPosition>
          caption="Top positions, with quantity and what the market would pay for them"
          rows={top}
          rowKey={(row) => row.asset_id}
          columns={[
            {
              key: "asset",
              header: "Asset",
              cell: (row) => (
                <span>
                  <strong>{row.symbol}</strong>
                  {row.name === undefined ? "" : ` · ${row.name}`}
                </span>
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
              key: "value",
              header: "Market value",
              numeric: true,
              cell: (row) => (
                <Temp value={row.temperature}>
                  <Figure
                    kind="units"
                    value={{ base: row.market_value_credits, scale: CREDIT_DECIMALS }}
                    symbol="Credits"
                  />
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
                    value={{ base: row.unrealized_pnl_credits, scale: CREDIT_DECIMALS }}
                    symbol="Credits"
                    signed
                  />
                </Temp>
              ),
            },
          ]}
        />
      </Panel>
    </>
  );
}

/** The movers, in the ordering the server applied and said it applied. */
function Movers(props: { readonly page: MarketPage }): ReactNode {
  return (
    <div className="stack">
      <DataTable<NativeMarketSummary>
        caption="Markets that moved in the last 24 hours, with price, change, volume and liquidity"
        rows={props.page.markets}
        rowKey={(row) => row.market_id}
        columns={[
          {
            key: "market",
            header: "Market",
            cell: (row) => (
              <span>
                <strong>{row.symbol}</strong> · {row.name}
                {row.demo && (
                  <>
                    {" "}
                    <StatusBadge
                      tone="warn"
                      title="Created by the sandbox demo seeder. It represents nothing."
                    >
                      Demo
                    </StatusBadge>
                  </>
                )}
              </span>
            ),
          },
          {
            key: "price",
            header: "Price",
            numeric: true,
            cell: (row) => (
              <Figure
                kind="units"
                value={{ base: row.last_price, scale: row.price_scale }}
                symbol="Credits"
              />
            ),
          },
          {
            key: "change",
            header: "24h change",
            numeric: true,
            riskMeasure: true,
            cell: (row) =>
              row.has_24h_change === true && row.change_24h_bps !== undefined ? (
                <Figure kind="bps" bps={row.change_24h_bps} signed />
              ) : (
                // Not traded in the window is a different fact from not having
                // moved, and a zero here would state the second.
                <span className="absent">no trade in the window</span>
              ),
          },
          {
            key: "volume",
            header: "24h volume",
            numeric: true,
            cell: (row) => (
              <Figure
                kind="units"
                value={{ base: row.credit_volume_24h, scale: CREDIT_DECIMALS }}
                symbol="Credits"
              />
            ),
          },
          {
            key: "liquidity",
            header: "Liquidity",
            numeric: true,
            cell: (row) => (
              <Figure
                kind="units"
                value={{ base: row.liquidity_credits, scale: CREDIT_DECIMALS }}
                symbol="Credits"
              />
            ),
          },
        ]}
      />
      <p className="field-note">
        Ordered by <span className="mono-small">{props.page.sort}</span>, which the backend applied
        and reported.{" "}
        {props.page.stable
          ? "Paging this ordering sees every market exactly once."
          : "This ordering ranks by figures that move when somebody trades, so paging it can show the same market twice."}
      </p>
      <div className="form-actions">
        <LinkButton to="/markets" variant="secondary">
          Explore markets
        </LinkButton>
      </div>
    </div>
  );
}

/** The last few events, each at the temperature the backend gave it. */
function Recent(props: { readonly page: ActivityFeed }): ReactNode {
  return (
    <div className="stack">
      {props.page.items.map((item) => (
        <PanelCard
          key={item.id}
          title={item.summary}
          {...(item.simulated ? ({ temp: "simulated" } as const) : {})}
          headingLevel={3}
          actions={item.status === undefined ? undefined : <StatusBadge>{item.status}</StatusBadge>}
        >
          <p className="field-note">
            <span className="mono-small">{item.kind}</span> · {formatInstant(item.occurred_at)}
          </p>
        </PanelCard>
      ))}
      <div className="form-actions">
        <LinkButton to="/activity" variant="secondary">
          Open activity
        </LinkButton>
      </div>
    </div>
  );
}

/* -------------------------------------------------------------------------- */

function Credits(props: { readonly balance: CreditBalance }): ReactNode {
  const { balance } = props;
  const value = (base: string): { readonly base: string; readonly scale: number } => ({
    base,
    scale: CREDIT_DECIMALS,
  });

  return (
    <div className="stack">
      <FieldGrid columns={3}>
        <Field label="Total Credits" note="Everything this account holds, whatever may be done with it." emphasis>
          <Figure kind="units" value={value(balance.gross)} symbol="Credits" big />
        </Field>
        <Field label="Spendable" note="Usable inside Nodal right now.">
          <Figure kind="units" value={value(balance.spendable)} symbol="Credits" />
        </Field>
        <Field
          label="Frozen"
          note="Held by a restriction or an open dispute. Not spendable until that clears."
        >
          <Figure kind="units" value={value(balance.frozen)} symbol="Credits" />
        </Field>
      </FieldGrid>

      <SegmentedBar
        caption="How this balance is held"
        scale={CREDIT_DECIMALS}
        symbol="Credits"
        total={balance.gross}
        segments={[
          {
            key: "spendable",
            label: "Spendable",
            baseUnits: balance.spendable,
            explanation: "Usable inside Nodal right now.",
            texture: "solid",
          },
          {
            key: "frozen",
            label: "Frozen",
            baseUnits: balance.frozen,
            explanation: "Held by a restriction or an open dispute.",
            texture: "hatch",
          },
        ]}
      />

      {/* A second axis, deliberately its own block: "may be spent" and "may be
          paid out" are different questions with different answers, and §46
          forbids the interface treating them as one. */}
      <div className="stack">
        <h3>Payout-eligible value</h3>
        <FieldGrid columns={2}>
          <Field label="Payout-eligible" note="What a payout policy would consider.">
            <Figure kind="units" value={value(balance.payout_eligible)} symbol="Credits" />
          </Field>
          <Field label="Not payout-eligible" note="Everything else in this balance.">
            <Figure kind="units" value={value(balance.ineligible)} symbol="Credits" />
          </Field>
        </FieldGrid>
        <p className="field-note">{PAYOUT_ELIGIBLE_SENTENCE}</p>
        <p className="field-note">{EMPTY_STATES.verification.body}</p>
        {balance.ineligible_reasons !== undefined && balance.ineligible_reasons.length > 0 && (
          <ul className="explain-fields">
            {balance.ineligible_reasons.map((reason) => (
              <li key={reason}>
                <span className="mono-small">{reason}</span>
              </li>
            ))}
          </ul>
        )}
      </div>

      <Origins balance={balance} />

      <p className="field-note">
        Policy <span className="mono-small">{balance.policy_version}</span>. The response carries no
        snapshot instant, so none is shown; this figure is refetched on every visit and again
        whenever the event stream reports the balance stale.
      </p>
    </div>
  );
}

/** Where the Credits came from. Origin is what decides eligibility, not the total. */
function Origins(props: { readonly balance: CreditBalance }): ReactNode {
  const byOrigin = props.balance.by_origin;
  if (byOrigin === undefined) return null;
  const rows = Object.entries(byOrigin);
  if (rows.length === 0) return null;

  return (
    <div className="stack">
      <h3>Where these Credits came from</h3>
      <DataTable<readonly [string, string]>
        caption="Credits held, by the origin that created them"
        rows={rows}
        rowKey={(row) => row[0]}
        columns={[
          { key: "origin", header: "Origin", cell: (row) => <span>{row[0]}</span> },
          {
            key: "quantity",
            header: "Credits",
            numeric: true,
            cell: (row) => (
              <Figure
                kind="units"
                value={{ base: row[1], scale: CREDIT_DECIMALS }}
                symbol="Credits"
              />
            ),
          },
        ]}
      />
      <p className="field-note">{PROVENANCE_NOTE}</p>
    </div>
  );
}

/* -------------------------------------------------------------------------- */

/** What the product's own word for an agent's state means, in a badge tone. */
function statusTone(status: string): "good" | "warn" | "bad" | "neutral" {
  if (status === "ENABLED") return "good";
  if (status === "PAUSED") return "warn";
  if (status === "FAILED" || status === "DISABLED") return "bad";
  return "neutral";
}

function Agents(props: { readonly list: readonly Agent[] }): ReactNode {
  const columns: ReadonlyArray<Column<Agent>> = [
    {
      key: "name",
      header: "Agent",
      cell: (agent) => <span>{agent.name}</span>,
    },
    {
      key: "status",
      header: "Status",
      cell: (agent) => (
        <StatusBadge tone={statusTone(agent.status)}>{agent.status}</StatusBadge>
      ),
    },
    {
      key: "authority",
      header: "Authority",
      cell: (agent) => (
        <span title={agent.authority.summary}>
          {agent.authority.level} · {agent.authority.name}
        </span>
      ),
    },
    {
      key: "budget",
      header: "Budget used",
      numeric: true,
      cell: (agent) => (
        <Figure
          kind="units"
          value={{ base: agent.budget.used_credits, scale: CREDIT_DECIMALS }}
          symbol="Credits"
        />
      ),
    },
    {
      key: "granted",
      header: "Budget granted",
      numeric: true,
      cell: (agent) => (
        <Figure
          kind="units"
          value={{ base: agent.budget.granted_credits, scale: CREDIT_DECIMALS }}
          symbol="Credits"
        />
      ),
    },
    {
      key: "runtime",
      header: "Runtime",
      cell: (agent) => <span title={agent.runtime.detail}>{agent.runtime.evaluator}</span>,
    },
  ];

  return (
    <div className="stack">
      <DataTable<Agent>
        caption="Agents on this account, with the authority granted to each and the budget it has used"
        rows={props.list}
        rowKey={(agent) => agent.id}
        columns={columns}
      />
      <p className="field-note">
        An agent being ENABLED is the owner granting it the right to be evaluated. Whether anything
        is evaluating it is the runtime column, which is a separate fact.
      </p>
      <div className="form-actions">
        <LinkButton to="/agents" variant="secondary">
          Open agents
        </LinkButton>
      </div>
    </div>
  );
}
