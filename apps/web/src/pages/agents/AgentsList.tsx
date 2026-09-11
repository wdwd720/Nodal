/**
 * `/agents` — every agent this account owns, and the authority each one holds.
 *
 * The previous version of this screen derived "agents" from the actor recorded
 * on trade intents, because v1 had no agent resource. It does now: `GET
 * /v1/agents` returns the roster, the grant, the budget and the honest runtime
 * state, so this is a list of agents that EXIST rather than a list of agents
 * that have acted, and the difference is the whole point of the resource.
 *
 * Two columns here are doing work that the rest of the category gets wrong.
 *
 * `Authority` is the level's own sentence about WHO DECIDES, not a word like
 * "auto". `Runtime` is separate from `Status`, because an agent can be enabled,
 * correct, and evaluated by nothing at all — and on this deployment every one
 * of them is, which the page says out loud rather than leaving to be inferred
 * from a table of zeroes.
 */
import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { useAgents, useVersion, type Agent, type AgentList } from "../../api/queries.ts";
import { LinkButton } from "../../components/Button.tsx";
import { AsyncPanel } from "../../components/DataState.tsx";
import { DataTable } from "../../components/DataTable.tsx";
import { Disclosure, Page, Panel, StatusBadge } from "../../components/Layout.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { EMPTY_STATES } from "../../lib/errors.ts";
import { formatInstant } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";
import { SANDBOX_TIER_NOTE } from "../../lib/honesty.ts";
import { AgentStatus, AuthorityLadder, Credits, CreditsNote } from "./parts.tsx";

function runtimeWord(agent: Agent): string {
  if (agent.runtime.evaluator === "NOT_DEPLOYED") return "evaluator not deployed";
  if (agent.runtime.evaluator === "RUNNING") return "evaluating";
  return "idle";
}

export function AgentsList(): ReactNode {
  const accountId = useActiveAccountId();
  const agents = useAgents(accountId);
  // The same tier answer `/markets` reads. An agent's budget and its loss stop
  // are Credits, and on a rehearsal deployment they are rehearsal Credits — so
  // the panels that show them carry the same temperature the market pages do.
  const version = useVersion();
  const sandbox = version.data?.sandbox_tier === true;
  const navigate = useNavigate();

  return (
    <Page
      title="Agents"
      lead="An agent acts inside limits you set, at an authority level you choose. Creating one grants nothing that runs."
      actions={
        <LinkButton to="/agents/new" variant="primary">
          Create an agent
        </LinkButton>
      }
    >
      {sandbox && <p className="field-note">{SANDBOX_TIER_NOTE}</p>}

      <Panel
        title="Your agents"
        description="What each one may do, what it has been granted, and what is actually evaluating it."
        temp={sandbox ? "simulated" : "economy"}
      >
        <AsyncPanel
          query={agents}
          loadingLabel="Loading agents…"
          skeleton={<Skeleton shape="rows" count={3} label="Your agents are loading" />}
          empty={{
            isEmpty: (data: AgentList) => data.items.length === 0,
            title: EMPTY_STATES.agents.title,
            body: EMPTY_STATES.agents.body,
          }}
        >
          {(data: AgentList) => (
            <>
              <DataTable
                caption="Your agents, with the authority level each holds, the Credit budget granted to it and whether anything is evaluating it"
                rows={data.items}
                rowKey={(agent: Agent) => agent.id}
                onOpenRow={(agent: Agent) => {
                  void navigate(`/agents/${agent.id}`);
                }}
                columns={[
                  {
                    key: "name",
                    header: "Agent",
                    compare: (a: Agent, b: Agent) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0),
                    cell: (agent: Agent) => <span>{agent.name}</span>,
                  },
                  {
                    key: "status",
                    header: "Status",
                    cell: (agent: Agent) => <AgentStatus agent={agent} />,
                  },
                  {
                    key: "authority",
                    header: "Authority",
                    cell: (agent: Agent) => (
                      <span className="mono-small">
                        {String(agent.authority.level)} · {agent.authority.name}
                      </span>
                    ),
                  },
                  {
                    key: "budget",
                    header: "Budget granted",
                    numeric: true,
                    cell: (agent: Agent) => <Credits base={agent.limits.budget_credits} />,
                  },
                  {
                    key: "used",
                    header: "Budget used",
                    numeric: true,
                    riskMeasure: true,
                    cell: (agent: Agent) => <Credits base={agent.budget.used_credits} />,
                  },
                  {
                    key: "runtime",
                    header: "Runtime",
                    cell: (agent: Agent) => (
                      <StatusBadge tone={agent.runtime.evaluator === "NOT_DEPLOYED" ? "warn" : "info"}>
                        {runtimeWord(agent)}
                      </StatusBadge>
                    ),
                  },
                  {
                    key: "last-run",
                    header: "Last run",
                    cell: (agent: Agent) =>
                      agent.last_run_at === undefined ? (
                        <span className="absent">never run</span>
                      ) : (
                        <span>{formatInstant(agent.last_run_at)}</span>
                      ),
                  },
                ]}
              />
              <CreditsNote />
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel
        title="Authority levels"
        description="What each level means, and which ones this deployment permits."
        temp={sandbox ? "simulated" : "economy"}
      >
        <AsyncPanel
          query={agents}
          loadingLabel="Loading the authority ladder…"
          skeleton={<Skeleton shape="rows" count={4} label="The authority ladder is loading" />}
        >
          {(data: AgentList) => <AuthorityLadder levels={data.authorityLevels} />}
        </AsyncPanel>
        <Disclosure title="Why the disabled levels are listed at all">
          <p>
            Levels 4 to 6 exist in the architecture and are switched off by policy. Listing them,
            with the capability each would need, is how you can tell that they are off rather than
            absent — and it is how you can check, later, that one is still off.
          </p>
          <p>
            No level lets an agent raise its own limits, choose an asset you did not list, or act
            outside the budget you granted it.
          </p>
        </Disclosure>
      </Panel>
    </Page>
  );
}
