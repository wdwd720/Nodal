/**
 * AGENTS (PART 111): agent status, strategy version, capital envelope, live
 * positions, decisions, predictions, costs, risk events, P&L.
 *
 * The v1 contract has no agent resource. There is no endpoint that lists
 * agents, reports their state, returns their strategy version or capital
 * envelope, or exposes predictions, model costs or risk events. What it does
 * carry is the actor on a trade intent, so an agent is visible here exactly to
 * the extent that it has acted: its id, the intents it created, the modes it
 * ran in, the decisions attached to those intents, and the orders that came out.
 *
 * Every heading the goal document asks for is present. The ones the API cannot
 * answer say what is missing and why, which is the only honest thing a screen
 * can do when the alternative is a chart drawn from nothing.
 */
import { useMemo, type ReactNode } from "react";

import {
  useHoldings,
  useIntents,
  useOrders,
  type HoldingsResponse,
  type Order,
  type TradeIntent,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState } from "../components/DataState.tsx";
import {
  AsOf,
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  NoEndpoint,
  Page,
  Panel,
  Pill,
  Table,
  type Tone,
} from "../components/Layout.tsx";
import { Usd } from "../components/Money.tsx";
import {
  CONFIDENCE_DISCLAIMER,
  MODE_DESCRIPTIONS,
  USD_VALUATION_NOTE,
  modeBadge,
  modeUsesRealCapital,
} from "../lib/honesty.ts";
import { formatInstant } from "../lib/time.ts";
import { useActiveAccountId } from "../session.tsx";

interface AgentView {
  readonly agentId: string;
  readonly intents: readonly TradeIntent[];
  readonly modes: readonly string[];
  readonly lastActivity: string | undefined;
}

function intentTone(status: string): Tone {
  if (status === "COMPLETED") return "good";
  if (["REJECTED", "EXPIRED", "CANCELLED", "FAILED", "NO_VALID_PLAN"].includes(status)) return "bad";
  return "info";
}

export function Agents(): ReactNode {
  const accountId = useActiveAccountId();
  const intents = useIntents(accountId);
  const orders = useOrders(accountId);
  const holdings = useHoldings(accountId);

  const agents = useMemo<AgentView[]>(() => {
    const byAgent = new Map<string, TradeIntent[]>();
    for (const intent of intents.data ?? []) {
      if (intent.actor_type !== "AGENT") continue;
      const key = intent.agent_id ?? "unidentified agent";
      const list = byAgent.get(key) ?? [];
      list.push(intent);
      byAgent.set(key, list);
    }
    return [...byAgent.entries()].map(([agentId, list]) => ({
      agentId,
      intents: list,
      modes: [...new Set(list.map((intent) => intent.mode))],
      lastActivity: list
        .map((intent) => intent.received_at)
        .sort()
        .at(-1),
    }));
  }, [intents.data]);

  if (accountId === undefined) {
    return (
      <Page title="Agents">
        <EmptyState title="No account" body="The backend returned no accounts for this session." />
      </Page>
    );
  }

  const agentOrders = (orders.data ?? []).filter((order) =>
    agents.some((agent) => agent.intents.some((intent) => intent.id === order.intent_id)),
  );

  return (
    <Page
      title="Agents"
      lead="What autonomous activity on this account looks like from the v1 API, and what the v1 API cannot yet tell you."
    >
      <Panel title="Agent status" description="Derived from the actor recorded on each trade intent.">
        <AsyncPanel
          query={intents}
          loadingLabel="Loading intents…"
          empty={{
            isEmpty: () => agents.length === 0,
            title: "No agent has acted on this account",
            body: "Every trade intent the backend returned was created by a user, not by an agent. That is a real answer: there is no agent activity to show, and nothing is being invented in its place.",
          }}
        >
          {() => (
            <Table
              caption="Agents that have acted"
              headers={["Agent", "Intents created", "Modes used", "Last activity"]}
            >
              {agents.map((agent) => (
                <tr key={agent.agentId}>
                  <th scope="row">
                    <Identifier value={agent.agentId} />
                  </th>
                  <td className="num">{String(agent.intents.length)}</td>
                  <td>
                    {agent.modes.map((mode) => (
                      <Pill key={mode} tone={modeUsesRealCapital(mode) ? "warn" : "neutral"}>
                        {modeBadge(mode)}
                      </Pill>
                    ))}
                  </td>
                  <td>{formatInstant(agent.lastActivity)}</td>
                </tr>
              ))}
            </Table>
          )}
        </AsyncPanel>
        <p className="note">
          There is no agent lifecycle endpoint in v1, so an agent that exists but has never created
          an intent is invisible here. This table is "agents that have acted", not "agents that
          exist", and it is labelled that way rather than presented as a roster.
        </p>
      </Panel>

      <Panel title="Decisions on agent intents" description="Eligibility and risk, as recorded per intent.">
        {agents.length === 0 ? (
          <EmptyState
            title="No agent intents to decide on"
            body="No agent has created an intent on this account, so there are no decisions attached to one."
          />
        ) : (
          <Table
            caption="Agent intents"
            headers={["Intent", "Mode", "Status", "Action", "Rejection code", "Received"]}
          >
            {agents.flatMap((agent) =>
              agent.intents.map((intent) => (
                <tr key={intent.id}>
                  <th scope="row">
                    <Identifier value={intent.id} />
                  </th>
                  <td>
                    <Pill tone={modeUsesRealCapital(intent.mode) ? "warn" : "neutral"}>
                      {modeBadge(intent.mode)}
                    </Pill>
                  </td>
                  <td>
                    <Pill tone={intentTone(intent.status)}>{intent.status}</Pill>
                  </td>
                  <td>{intent.action}</td>
                  <td>
                    {intent.rejection_code === undefined ? (
                      <span className="absent">none</span>
                    ) : (
                      <span className="mono-small">{intent.rejection_code}</span>
                    )}
                  </td>
                  <td>{formatInstant(intent.received_at)}</td>
                </tr>
              )),
            )}
          </Table>
        )}
        <p className="note">
          The per-intent eligibility and risk decisions, with their policy versions and reason codes,
          are on the intent detail resource. Open any intent from the activity timeline to see them.
        </p>
      </Panel>

      <Panel title="Live positions" description="Positions on this account, whoever opened them.">
        <AsyncPanel
          query={holdings}
          loadingLabel="Loading positions…"
          empty={{
            isEmpty: (data: HoldingsResponse) => data.holdings.length === 0,
            title: "No open positions",
            body: "The backend reported no holdings for this account.",
          }}
        >
          {(data: HoldingsResponse) => (
            <>
              <Table caption="Positions" headers={["Asset", "USD mark", "Unrealized P&L", "Realized P&L"]}>
                {data.holdings.map((holding) => (
                  <tr key={holding.asset}>
                    <th scope="row">{holding.symbol}</th>
                    <td>
                      <Usd value={holding.usd_mark} />
                    </td>
                    <td>
                      <Usd value={holding.unrealized_pnl_usd} signed />
                    </td>
                    <td>
                      <Usd value={holding.realized_pnl_usd} signed absent="not reported" />
                    </td>
                  </tr>
                ))}
              </Table>
              <AsOf at={data.as_of} />
              <p className="note">
                The holdings endpoint reports positions per account, not per agent, so these figures
                cannot be attributed to an agent. Presenting them as an agent's profit and loss
                would be an attribution the API never made.
              </p>
            </>
          )}
        </AsyncPanel>
        <Disclosure title="About these valuations">
          <p>{USD_VALUATION_NOTE}</p>
        </Disclosure>
      </Panel>

      <Panel title="Orders from agent intents" description="What actually reached execution.">
        {agentOrders.length === 0 ? (
          <EmptyState
            title="No orders from agent intents"
            body="No order on this account traces back to an intent an agent created."
          />
        ) : (
          <Table caption="Agent orders" headers={["Order", "Status", "Side", "Mode", "Created"]}>
            {agentOrders.map((order: Order) => (
              <tr key={order.id}>
                <th scope="row">
                  <Identifier value={order.id} />
                </th>
                <td>
                  <Pill tone="info">{order.status}</Pill>
                </td>
                <td>{order.side}</td>
                <td>
                  <Pill tone={modeUsesRealCapital(order.mode) ? "warn" : "neutral"}>
                    {modeBadge(order.mode)}
                  </Pill>
                </td>
                <td>{formatInstant(order.created_at)}</td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>

      <Panel
        title="Strategy version, capital envelope, predictions, costs and risk events"
        description="The five headings the API cannot answer."
      >
        <NoEndpoint
          what="v1 exposes no agent, strategy-version, prediction, model-cost or risk-event resource."
          detail="Each of these is real inside the platform — a deployed agent pins a strategy version and a capital envelope, commits predictions before outcomes, and accrues model and data costs against a budget — but none of it is reachable over the customer API, so this screen will not draw it."
        />
        <FieldGrid columns={2}>
          <Field label="Strategy version" note="Would be the pinned, immutable version an agent runs.">
            <span className="absent">no endpoint</span>
          </Field>
          <Field label="Capital envelope" note="Would be the agent's own limits, separate from the account's.">
            <span className="absent">no endpoint</span>
          </Field>
          <Field label="Predictions" note="Committed before the outcome is knowable, then scored.">
            <span className="absent">no endpoint</span>
          </Field>
          <Field label="Model and data costs" note="Spend against the strategy's declared budget.">
            <span className="absent">no endpoint</span>
          </Field>
          <Field label="Risk events" note="Breaches, pauses and envelope reductions.">
            <span className="absent">no endpoint</span>
          </Field>
          <Field label="Per-agent P&L" note="Attribution of profit and loss to an agent.">
            <span className="absent">no endpoint</span>
          </Field>
        </FieldGrid>
        <p className="note">{CONFIDENCE_DISCLAIMER}</p>
      </Panel>

      <Panel title="Execution modes" description="What each mode means for your money.">
        <Table caption="Modes" headers={["Mode", "Real capital", "What it means"]}>
          {Object.entries(MODE_DESCRIPTIONS).map(([mode, description]) => (
            <tr key={mode}>
              <th scope="row">{mode}</th>
              <td>
                {modeUsesRealCapital(mode) ? <Pill tone="warn">yes</Pill> : <Pill tone="neutral">no</Pill>}
              </td>
              <td className="cell-prose">{description}</td>
            </tr>
          ))}
        </Table>
      </Panel>
    </Page>
  );
}
