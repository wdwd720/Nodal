/**
 * `/agents/:agentId` — one agent: what it may do, what it has been granted,
 * what has actually happened, and what is evaluating it.
 *
 * The distinction this screen exists to keep is the one goal §17 asks for and
 * the one this category usually collapses:
 *
 *   STATUS is permission. RUNTIME is machinery. They are different facts and
 *   they get different rows.
 *
 * An agent whose status is ENABLED and whose evaluator is NOT_DEPLOYED has been
 * granted the right to be evaluated by something that is not running. Rendering
 * that as one green word would be the interface asserting a thing it has no
 * evidence for, so it renders as two, and the second one says "not deployed"
 * with the API's own detail beneath it.
 *
 * The five lifecycle actions are all offered, and each one is either live or
 * disabled with the reason the backend would have given. Hiding an action the
 * backend would refuse hides the rule; offering one that dies in a toast hides
 * it just as well.
 *
 * Every panel reads the same query, so the loading state and the failure state
 * belong to the page and not to each panel. The first version let each region
 * handle the shared query itself, and a single 404 then rendered the same
 * explanation five times down the screen: regions hydrate independently when
 * they have independent sources, and these do not.
 */
import { useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";

import { useAgent, useAgentAction, type Agent, type AgentAction } from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { EmptyState, Explanation } from "../../components/DataState.tsx";
import { Figure } from "../../components/Figure.tsx";
import { useIdempotencyKey, requestSignature } from "../../lib/idempotency.ts";
import {
  Disclosure,
  Field,
  FieldGrid,
  FormField,
  Identifier,
  NoEndpoint,
  Page,
  Panel,
  StatusBadge,
} from "../../components/Layout.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { modeBadge } from "../../lib/honesty.ts";
import { formatInstant } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";
import { AgentStatus, AuthorityLine, LimitsGrid, RuntimeState } from "./parts.tsx";

/**
 * Why an action cannot be taken, in the backend's own terms.
 *
 * `undefined` means "offer it". Everything else is the sentence rendered beside
 * the disabled control, and each one mirrors the refusal the service would
 * produce, so somebody who presses ahead anyway is not told something different
 * from what this said.
 */
function refusalFor(action: AgentAction, agent: Agent): string | undefined {
  const status = agent.status;
  switch (action) {
    case "enable":
      if (status === "PAUSED") return "This agent is paused. Resume it rather than enabling it.";
      if (status === "ENABLED") return "This agent is already enabled.";
      if (status === "DISABLED") {
        return "Disabling an agent is final. A disabled agent cannot be enabled again.";
      }
      if (status === "FAILED") return "This agent has failed, and a failed agent cannot be enabled.";
      return undefined;
    case "pause":
      if (status === "PAUSED") return "This agent is already paused.";
      if (status !== "ENABLED") {
        return "Only an enabled agent can be paused, and this one is not running.";
      }
      return undefined;
    case "resume":
      if (status !== "PAUSED") {
        return `This agent is ${status.toLowerCase()} rather than paused, so there is nothing to resume.`;
      }
      return undefined;
    case "disable":
      if (status === "DISABLED") return "This agent is already disabled, and disabling is final.";
      return undefined;
    case "archive":
      if (status !== "DISABLED" && status !== "FAILED") {
        return "Disable this agent before archiving it. Archiving only hides one that is already finished.";
      }
      if (agent.archived) return "This agent is already archived.";
      return undefined;
  }
}

const ACTION_LABELS: Readonly<Record<AgentAction, string>> = {
  enable: "Enable",
  pause: "Pause",
  resume: "Resume",
  disable: "Disable, permanently",
  archive: "Archive",
};

const ACTION_NOTES: Readonly<Record<AgentAction, string>> = {
  enable:
    "Grants this agent the right to be evaluated at the first rung of the ladder. It needs a recent strong sign-in, and a level that acts without you confirming each action also needs the capability gate for agent trading to be active.",
  pause:
    "Opens a pause record. Open orders are left exactly as they are; nothing is cancelled implicitly.",
  resume: "Closes the pause and returns the agent to the rung it was on.",
  disable:
    "Revokes the authority you granted. This is final: a disabled agent cannot be enabled again.",
  archive: "Hides an agent that is already finished. It changes no authority and no limit.",
};

const ACTIONS: readonly AgentAction[] = ["enable", "pause", "resume", "disable", "archive"];

function Lifecycle(props: { readonly agent: Agent; readonly accountId: string }): ReactNode {
  const { agent, accountId } = props;
  const act = useAgentAction();
  // One key per agent. The signature below carries the action and the reason,
  // so pausing and then disabling the same agent are two requests.
  const actionKey = useIdempotencyKey(`agents.action.${agent.id}`);
  const [reason, setReason] = useState("");
  const [pending, setPending] = useState<AgentAction | undefined>(undefined);

  return (
    <>
      {act.isError && <Explanation error={act.error} onRetry={act.reset} />}
      <FormField
        label="Why (optional)"
        hint="Recorded against this agent's own history, so you can read later why it stopped."
      >
        {(field) => (
          <input
            className="input"
            type="text"
            maxLength={500}
            value={reason}
            onChange={(event) => {
              setReason(event.target.value);
            }}
            {...field}
          />
        )}
      </FormField>
      <div className="form-actions">
        {ACTIONS.map((action) => {
          const refusal = refusalFor(action, agent);
          if (refusal !== undefined) {
            return (
              <Button key={action} disabledReason={refusal}>
                {ACTION_LABELS[action]}
              </Button>
            );
          }
          return (
            <Button
              key={action}
              variant={
                action === "disable" ? "danger" : action === "enable" ? "primary" : "secondary"
              }
              busy={act.isPending && pending === action}
              busyLabel="Working…"
              onClick={() => {
                setPending(action);
                act.mutate(
                  {
                    agentId: agent.id,
                    accountId,
                    action,
                    ...(reason.trim() === "" ? {} : { reason: reason.trim() }),
                    // Minted at the first confirmation and REUSED while the
                    // decision is the same one, so a retry after a stronger
                    // sign-in or a lost reply replays this decision instead of
                    // making a second. Changing the action, or the reason
                    // recorded with it, is a different decision and mints a new
                    // key.
                    idempotencyKey: actionKey.forRequest(
                      requestSignature([agent.id, action, reason.trim()]),
                    ),
                  },
                  // On SUCCESS only. A failed attempt keeps the key, so
                  // pressing the same action again is a retry of the same
                  // request rather than a second decision — which is the whole
                  // reason the key exists. Changing the action, or the reason
                  // recorded with it, changes the signature and mints a new one
                  // without anybody having to remember to.
                  { onSuccess: () => { actionKey.clear(); } },
                );
              }}
            >
              {ACTION_LABELS[action]}
            </Button>
          );
        })}
      </div>
      <Disclosure title="What each of these does">
        <ul className="explain-fields">
          {ACTIONS.map((action) => (
            <li key={action}>
              <strong>{ACTION_LABELS[action]}</strong> — {ACTION_NOTES[action]}
            </li>
          ))}
        </ul>
      </Disclosure>
    </>
  );
}

export function AgentDetail(): ReactNode {
  const { agentId } = useParams<{ readonly agentId: string }>();
  const accountId = useActiveAccountId();
  const agent = useAgent(agentId);

  if (agent.isPending) {
    return (
      <Page title="Agent" actions={<LinkButton to="/agents">Back to the list</LinkButton>}>
        <Panel title="Reading this agent" description="From the backend, which is the only source.">
          <Skeleton shape="rows" count={4} label="This agent is loading" />
        </Panel>
      </Page>
    );
  }

  const data = agent.data;
  if (agent.isError || data === undefined) {
    return (
      <Page title="Agent" actions={<LinkButton to="/agents">Back to the list</LinkButton>}>
        <Panel
          title="This agent could not be read"
          description="Nothing about it is shown, because nothing about it is known."
        >
          <Explanation
            error={agent.error}
            onRetry={() => {
              void agent.refetch();
            }}
          />
        </Panel>
      </Page>
    );
  }

  return (
    <Page
      title={data.name}
      lead="What this agent may do, what it has been granted, and what is actually evaluating it."
      actions={<LinkButton to="/agents">Back to the list</LinkButton>}
    >
      <Panel title="Status and authority" description="Permission first; machinery below it.">
        <FieldGrid columns={2}>
          <Field label="Status" note="What you granted. It is not a claim that anything is running.">
            <AgentStatus agent={data} />
          </Field>
          <Field label="Mode" note="What kind of capital a run of this agent would use.">
            {data.mode === undefined ? (
              <span className="absent">no mode yet</span>
            ) : (
              <StatusBadge tone="neutral">{modeBadge(data.mode)}</StatusBadge>
            )}
          </Field>
          <Field label="Lifecycle state" note="The internal rung, shown so the status is checkable.">
            <span className="mono-small">
              {data.state} · stage {data.stage}
            </span>
          </Field>
          <Field label="Created">{formatInstant(data.created_at)}</Field>
        </FieldGrid>
        <AuthorityLine authority={data.authority} />
        {data.pause !== undefined && (
          <Disclosure title="This agent is paused">
            <p>
              {data.pause.reason} — opened by {data.pause.paused_by_actor_type.toLowerCase()} at{" "}
              {formatInstant(data.pause.paused_at)}.
            </p>
            <p className="mono-small">{data.pause.reason_code}</p>
            <p>
              Open orders were left alone. Nothing is cancelled when an agent is paused, because
              cancelling is a separate decision with its own consequences.
            </p>
          </Disclosure>
        )}
      </Panel>

      <Panel title="Limits and budget" description="The bounds you granted, and how much has been used.">
        <LimitsGrid agent={data} />
        <Disclosure title="Which assets it may touch">
          {data.limits.allowed_asset_ids.length === 0 ? (
            <p>
              The backend returned an empty universe for this agent, which it should never do: an
              agent whose universe is empty has authority over nothing and should not exist. Treat
              this as a fault rather than as a permission.
            </p>
          ) : (
            <ul className="explain-fields">
              {data.limits.allowed_asset_ids.map((id) => (
                <li key={id}>
                  <Identifier value={id} label="asset" copyable={false} />
                </li>
              ))}
            </ul>
          )}
        </Disclosure>
      </Panel>

      <Panel
        title="What is evaluating it"
        description="Derived from agent runs and from which worker processes this deployment runs."
      >
        <RuntimeState agent={data} />
      </Panel>

      <Panel title="Runs" description="What this agent has actually done.">
        <FieldGrid columns={3}>
          <Field label="Runs recorded">
            <Figure kind="count" count={data.runs_total ?? null} absent="not reported" />
          </Field>
          <Field label="Last run">
            {data.last_run_at === undefined ? (
              <span className="absent">never run</span>
            ) : (
              formatInstant(data.last_run_at)
            )}
          </Field>
          <Field label="Last run status">
            {data.last_run_status === undefined ? (
              <span className="absent">not reported</span>
            ) : (
              <StatusBadge tone="neutral">{data.last_run_status}</StatusBadge>
            )}
          </Field>
        </FieldGrid>
        {(data.runs_total ?? 0) === 0 && (
          <EmptyState
            title="This agent has not run"
            body="No run has been recorded for it. That is a statement about activity rather than a score: nothing has evaluated this agent, so there is nothing to report and nothing is being inferred."
          />
        )}
        <NoEndpoint
          what="The per-run decision history is not on the customer API."
          detail="An agent run records what it observed, what it decided and why. The API exposes the counters above and no route that lists those decisions, so rather than draw a decision log out of counters, this page names the part that is missing."
        />
      </Panel>

      <Panel title="Lifecycle" description="Enable, pause, resume, disable, archive.">
        {accountId === undefined ? (
          <EmptyState
            title="Nothing to act on yet"
            body="The account this agent belongs to has to be read from the backend before an action can be taken on it."
          />
        ) : (
          <Lifecycle agent={data} accountId={accountId} />
        )}
      </Panel>
    </Page>
  );
}
