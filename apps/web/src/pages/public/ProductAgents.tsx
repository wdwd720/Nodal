/**
 * `/product/agents` — what a delegation actually is here.
 *
 * The temptation on a page about agents is to describe autonomy. This one
 * describes **bounds**, because that is what the software implements: an agent
 * is a set of limits with a strategy attached, and every one of those limits is
 * checked by the backend rather than by the agent.
 *
 * The page also states the two things a marketing page would omit: authority
 * levels four to six are refused by policy, and the runtime deliberately has no
 * caller in this deployment. Both are true, both are checkable, and a customer
 * who finds out either of them after signing up would be right to be annoyed.
 */
import type { ReactNode } from "react";

import { LinkButton } from "../../components/Button.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { CONFIDENCE_DISCLAIMER } from "../../lib/honesty.ts";
import { ExampleAgents } from "./ExampleUI.tsx";
import { SiteItem, SitePageHead, SiteSection } from "./SiteChrome.tsx";

interface Level {
  readonly level: string;
  readonly name: string;
  readonly body: string;
  readonly permitted: boolean;
}

const LEVELS: readonly Level[] = [
  {
    level: "1",
    name: "Observe",
    body: "Reads markets and records what it would have done. Nothing it decides reaches an order.",
    permitted: true,
  },
  {
    level: "2",
    name: "Propose",
    body: "Records a proposal you can act on. It cannot submit one itself.",
    permitted: true,
  },
  {
    level: "3",
    name: "Execute within limits",
    body:
      "Submits orders inside its budget, its per-trade cap, its allowlist and its daily loss stop. " +
      "Every one of those is enforced by the backend, not by the agent.",
    permitted: true,
  },
  {
    level: "4 – 6",
    name: "Refused by policy",
    body:
      "Broader authority is defined in the domain and refused by policy. The interface shows these " +
      "as disabled with the reason rather than hiding them, because a limit you cannot see is a " +
      "limit you will assume does not exist.",
    permitted: false,
  },
];

const BOUNDS: ReadonlyArray<{ readonly title: string; readonly body: string }> = [
  {
    title: "A Credit budget",
    body:
      "The total the agent may ever commit. Spent budget is shown against it, and an agent that " +
      "reaches its budget stops rather than asking for more.",
  },
  {
    title: "A cap per trade",
    body: "The largest single order it may place, whatever the budget still allows.",
  },
  {
    title: "An allowlist of assets",
    body:
      "Markets it may touch at all. An order against anything else is refused by the backend with " +
      "that reason, even if the strategy asked for it.",
  },
  {
    title: "A daily loss stop",
    body:
      "The loss over a day that ends the agent's activity. It is a stop, not a floor: a position " +
      "can still move after the agent has stopped trading it.",
  },
];

export function ProductAgents(): ReactNode {
  return (
    <>
      <SitePageHead
        title="Agents"
        lead="An agent is a bounded delegation: a strategy plus limits the backend enforces. It is not a manager, it does not exercise judgement, and it cannot do anything you have not authorised."
        actions={
          <>
            <LinkButton to="/get-started" variant="primary">
              Get started
            </LinkButton>
            <LinkButton to="/product/markets">Markets</LinkButton>
          </>
        }
      />

      <SiteSection
        title="Authority levels"
        lead="What an agent is allowed to do at all, before any limit applies."
      >
        <Panel
          title="The six levels, and the three you can use"
          description="Levels are a property of the delegation, not a setting the agent can change."
        >
          <FieldGrid columns={2}>
            {LEVELS.map((level) => (
              <Field key={level.level} label={`Level ${level.level} — ${level.name}`}>
                <p className="note">{level.body}</p>
                <StatusBadge tone={level.permitted ? "good" : "warn"}>
                  {level.permitted ? "Available" : "Refused by policy"}
                </StatusBadge>
              </Field>
            ))}
          </FieldGrid>
        </Panel>
      </SiteSection>

      <SiteSection
        title="The four bounds"
        lead="Set when you create the agent. Checked by the backend on every order, not by the agent on its own behalf."
      >
        <div className="site-grid-2">
          {BOUNDS.map((bound) => (
            <SiteItem key={bound.title} title={bound.title}>
              <p>{bound.body}</p>
            </SiteItem>
          ))}
        </div>
      </SiteSection>

      <SiteSection
        title="An agent list"
        lead="The product's own components, rendered from fixed example values."
      >
        <ExampleAgents />
      </SiteSection>

      <SiteSection
        title="What an agent will not do"
        lead="Stated here rather than discovered later."
      >
        <div className="site-grid-2">
          <SiteItem title="It does not run unattended in this deployment">
            <p>
              The agent runtime is built and deliberately has no caller: nothing schedules a run.
              An agent page says exactly that where it is true, instead of showing an idle status
              that implies something is waiting to happen.
            </p>
          </SiteItem>
          <SiteItem title="It does not know whether it is right">
            <p>{CONFIDENCE_DISCLAIMER}</p>
          </SiteItem>
          <SiteItem title="It does not widen its own limits">
            <p>
              Budget, cap, allowlist and loss stop are yours to change. An agent that reaches one
              stops and records why; it has no path to ask for more.
            </p>
          </SiteItem>
          <SiteItem title="It does not reach anything outside Nodal">
            <p>
              Agents trade the same internal markets you do. No order reaches an external venue or
              a blockchain, because the product has no path to one.
            </p>
          </SiteItem>
        </div>
      </SiteSection>
    </>
  );
}
