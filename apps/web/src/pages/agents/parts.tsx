/**
 * The pieces the three agent screens share.
 *
 * They live together because each one carries a rule that has to hold on every
 * screen an agent appears on, and a rule copied three times is a rule that will
 * hold on two of them:
 *
 *   - AUTHORITY IS ALWAYS A SENTENCE ABOUT WHO DECIDES. Goal §17 asks a
 *     customer to be able to tell research from recommendation from prepared
 *     action from rule-based execution, and the distinction survives only if
 *     the words are the API's own summary rather than a label somebody shortens
 *     to "automated" on the list page.
 *   - THE LADDER IS SHOWN WHOLE. Levels 4 to 6 are declared in the architecture
 *     and disabled by policy. They are rendered as disabled, with the
 *     capability each would need, rather than omitted — a product that hides
 *     them teaches nobody that they exist and are switched off.
 *   - RUNTIME IS NOT LIFECYCLE. An agent can be enabled, correct, and evaluated
 *     by nothing at all. `AgentRuntime.evaluator` says NOT_DEPLOYED on this
 *     tier and the interface says exactly that, next to the status, so nobody
 *     reads "Enabled" as "running".
 *   - A CREDIT FIGURE CARRIES WHAT A CREDIT IS. Every Credit figure in the
 *     agent screens is rendered here, so the disclosure travels with it.
 */
import type { ReactNode } from "react";

import type { Agent, AuthorityLevel } from "../../api/queries.ts";
import { DataTable } from "../../components/DataTable.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Field, FieldGrid, StatusBadge, type Tone } from "../../components/Layout.tsx";
import { CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { formatInstant } from "../../lib/time.ts";
import { CREDIT_DECIMALS } from "../../lib/credits.ts";

/** Every Credit figure on these screens. Exact base units in, tabular figure out. */
export function Credits(props: {
  readonly base: string | undefined;
  readonly big?: boolean;
}): ReactNode {
  return (
    <Figure
      kind="money"
      value={props.base === undefined ? null : { base: props.base, scale: CREDIT_DECIMALS }}
      symbol="Credits"
      {...(props.big === true ? { big: true } : {})}
    />
  );
}

/** What a Credit is, said wherever one is shown. */
export function CreditsNote(): ReactNode {
  return <p className="note">{CREDITS_DISCLOSURE}</p>;
}

/**
 * The product's own word for the lifecycle state.
 *
 * STOPPED means created and never enabled. ENABLED means the owner granted the
 * right to be evaluated, which is a statement about permission and not about
 * anything running.
 */
export function statusTone(status: string): Tone {
  switch (status) {
    case "ENABLED":
      return "good";
    case "PAUSED":
      return "warn";
    case "FAILED":
      return "bad";
    case "DISABLED":
      return "bad";
    default:
      return "neutral";
  }
}

export function AgentStatus(props: { readonly agent: Agent }): ReactNode {
  const enabled = props.agent.status === "ENABLED";
  return (
    <StatusBadge
      tone={statusTone(props.agent.status)}
      {...(enabled
        ? {
            title:
              "The owner granted this agent the right to be evaluated. Whether anything is evaluating it is the runtime state, shown separately.",
          }
        : {})}
    >
      {props.agent.status}
    </StatusBadge>
  );
}

/** The authority level, as the level's own one-sentence summary. */
export function AuthorityLine(props: { readonly authority: AuthorityLevel }): ReactNode {
  const { authority } = props;
  return (
    <>
      <StatusBadge tone={authority.enabled ? "info" : "warn"}>
        Level {String(authority.level)} · {authority.name}
      </StatusBadge>
      <p className="field-note">{authority.summary}</p>
    </>
  );
}

/**
 * What is actually evaluating and executing.
 *
 * NOT_DEPLOYED is the honest answer on this tier and it is rendered as a
 * statement rather than as an absence: no worker process evaluates an agent in
 * this deployment, and an interface that left the row blank would let a reader
 * assume something was quietly working.
 */
export function RuntimeState(props: { readonly agent: Agent }): ReactNode {
  const { runtime } = props.agent;
  const deployed = runtime.evaluator !== "NOT_DEPLOYED" || runtime.executor !== "NOT_DEPLOYED";
  return (
    <>
      <FieldGrid columns={2}>
        <Field
          label="Evaluator"
          note="What would read data and decide whether this agent has anything to propose."
        >
          <StatusBadge tone={runtime.evaluator === "NOT_DEPLOYED" ? "warn" : "info"}>
            {runtime.evaluator === "NOT_DEPLOYED" ? "not deployed" : runtime.evaluator}
          </StatusBadge>
        </Field>
        <Field label="Executor" note="What would carry a decision to a venue.">
          <StatusBadge tone={runtime.executor === "NOT_DEPLOYED" ? "warn" : "info"}>
            {runtime.executor === "NOT_DEPLOYED" ? "not deployed" : runtime.executor}
          </StatusBadge>
        </Field>
        <Field label="Last heartbeat" note="When a worker last reported it was alive.">
          {runtime.last_heartbeat === undefined ? (
            <span className="absent">never</span>
          ) : (
            formatInstant(runtime.last_heartbeat)
          )}
        </Field>
      </FieldGrid>
      <p className="note">{runtime.detail}</p>
      {!deployed && (
        <p className="note">
          Nothing evaluates an agent in this deployment. Enabling one records that you granted it
          authority; it does not start anything, and no order can come out of it while this says
          not deployed.
        </p>
      )}
    </>
  );
}

/**
 * Every declared authority level, enabled or not.
 *
 * The rows come from the API rather than from a list written beside it, so the
 * day a level is approved this table changes without anybody editing a page.
 */
export function AuthorityLadder(props: { readonly levels: readonly AuthorityLevel[] }): ReactNode {
  return (
    <DataTable
      caption="Every declared authority level, whether this deployment permits it, and the capability a disabled level would need"
      rows={[...props.levels]}
      rowKey={(level: AuthorityLevel) => String(level.level)}
      columns={[
        {
          key: "level",
          header: "Level",
          cell: (level: AuthorityLevel) => (
            <span className="mono-small">
              {String(level.level)} · {level.name}
            </span>
          ),
        },
        {
          key: "permitted",
          header: "In this deployment",
          cell: (level: AuthorityLevel) =>
            level.enabled ? (
              <StatusBadge tone="good">available</StatusBadge>
            ) : (
              <StatusBadge tone="warn">disabled by policy</StatusBadge>
            ),
        },
        {
          key: "summary",
          header: "Who decides",
          cell: (level: AuthorityLevel) => <span className="cell-prose">{level.summary}</span>,
        },
        {
          key: "capability",
          header: "Capability it would need",
          cell: (level: AuthorityLevel) =>
            level.required_capability === undefined || level.required_capability === "" ? (
              <span className="absent">none</span>
            ) : (
              <span className="mono-small">{level.required_capability}</span>
            ),
        },
      ]}
    />
  );
}

/** The bounds the owner granted, and how much of the budget has been used. */
export function LimitsGrid(props: { readonly agent: Agent }): ReactNode {
  const { limits, budget } = props.agent;
  const schedule =
    limits.schedule.kind === "INTERVAL" && limits.schedule.interval_minutes !== undefined
      ? `every ${String(limits.schedule.interval_minutes)} minutes`
      : "only when you ask";
  return (
    <>
      <FieldGrid columns={2}>
        <Field
          label="Budget granted"
          note="A ceiling on Credits at risk. No Credits moved when this agent was created."
        >
          <Credits base={limits.budget_credits} />
        </Field>
        <Field label="Budget used" note={budgetSourceNote(budget.source)}>
          <Credits base={budget.used_credits} />
        </Field>
        <Field label="Most per trade">
          <Credits base={limits.per_trade_cap_credits} />
        </Field>
        <Field label="Daily loss stop" note="The agent stops for the day at this loss.">
          <Credits base={limits.daily_loss_stop_credits} />
        </Field>
        <Field label="Largest share of one position">
          <Figure kind="bps" bps={limits.max_position_share_bps} />
        </Field>
        <Field label="How often it may evaluate">{schedule}</Field>
      </FieldGrid>
      <CreditsNote />
    </>
  );
}

/**
 * Where "used" came from.
 *
 * A zero that was measured and a zero that was never measured are different
 * facts, and the API distinguishes them so the interface can too.
 */
export function budgetSourceNote(source: string): string {
  switch (source) {
    case "NO_RUNS_RECORDED":
      return "No run of this agent has been recorded, so nothing has been measured. This zero is an absence of activity, not a measurement of spending.";
    case "NO_INTENTS_CREATED":
      return "This agent has run and created no intent, so nothing has been committed.";
    default:
      return "Derived from the intents this agent's runs created that reached a state where value is committed.";
  }
}
