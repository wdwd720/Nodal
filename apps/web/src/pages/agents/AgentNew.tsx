/**
 * `/agents/new` — describe, compile, review, then grant.
 *
 * Goal §18 states the rule this screen exists to enforce:
 *
 *   Do not silently turn natural-language text directly into financial
 *   authority. Show a human-understandable compiled strategy before
 *   activation.
 *
 * So the four steps are four separate acts with four separate confirmations,
 * and each one stays on the page after it is done. A wizard that discards the
 * step behind it is a wizard in which nobody can check what they agreed to.
 *
 *   1. DESCRIBE — `POST /v1/strategies` records the words, exactly as written.
 *      Nothing is compiled and nothing is activated.
 *   2. COMPILE — `POST /v1/strategies/{id}/compile`. Every attempt is recorded,
 *      including one on a deployment with no compiler backend, which comes back
 *      with outcome MODEL_UNAVAILABLE and the failure code COMPILER_UNAVAILABLE.
 *      That code is rendered as exactly that. Nothing is inferred in its place.
 *   3. REVIEW — the compiled version's effects, its hash and the compiler's own
 *      words. This screen never writes that text itself.
 *   4. GRANT — `POST /v1/agents` with an authority level, a Credit budget, a
 *      per-trade cap, a daily loss stop and the assets the agent may touch.
 *
 * WITHOUT A COMPILED VERSION THERE IS NO STEP 4. The agent resource is created
 * from a strategy VERSION, so an account on a tier with no compiler cannot make
 * an agent at all — and the button says so, with the reason, rather than being
 * absent or, worse, present and lying.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useCompileStrategy,
  useCreateAgent,
  useCreateStrategy,
  useAgents,
  useAssets,
  useStrategies,
  type AgentLimits,
  type CompileResult,
  type Asset,
  type Strategy,
  type StrategyVersion,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { EmptyState, Explanation } from "../../components/DataState.tsx";
import { Figure } from "../../components/Figure.tsx";
import {
  Disclosure,
  Field,
  FieldGrid,
  FormField,
  Identifier,
  Page,
  Panel,
  StatusBadge,
} from "../../components/Layout.tsx";
import { Refusal } from "../../components/Refusal.tsx";
import { CREDITS_DISCLOSURE, NATIVE_ASSET_RISK } from "../../lib/honesty.ts";
import { parseQuantityInput } from "../../lib/money.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { formatInstant } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";
import { CREDIT_DECIMALS, Credits } from "./parts.tsx";

/* --------------------------------------------------------------------------
 * The two drafts, and why they are integers
 * ------------------------------------------------------------------------ */

interface Description {
  readonly name: string;
  readonly text: string;
}

const NO_DESCRIPTION: Description = { name: "", text: "" };

interface GrantDraft {
  readonly agentName: string;
  readonly level: number;
  readonly budget: string;
  readonly perTrade: string;
  readonly dailyLossStop: string;
  readonly shareBps: number;
  readonly assets: readonly string[];
  readonly interval: number;
}

/**
 * `interval` of zero means MANUAL: the agent evaluates only when asked. It is an
 * integer rather than a separate flag so the whole choice is one control, and
 * because every other figure on this form is an exact integer too.
 */
const MANUAL = 0;

const NO_GRANT: GrantDraft = {
  agentName: "",
  level: 1,
  budget: "",
  perTrade: "",
  dailyLossStop: "",
  shareBps: 2_500,
  assets: [],
  interval: MANUAL,
};

/** The share of one position an agent may hold, in basis points. */
const SHARE_CHOICES: readonly number[] = [500, 1_000, 2_500, 5_000, 10_000];

/** How often an agent may evaluate, in minutes. Zero is "only when you ask". */
const INTERVAL_CHOICES: readonly number[] = [MANUAL, 5, 15, 60, 240, 1_440];

function intervalLabel(minutes: number): string {
  if (minutes === MANUAL) return "Only when I ask";
  if (minutes === 1_440) return "Once a day";
  if (minutes === 60) return "Every hour";
  return `Every ${String(minutes)} minutes`;
}

/**
 * Why a declared level cannot be granted here.
 *
 * The sentence names the capability rather than saying "unavailable", because a
 * level that is off for a stated reason is checkable, and a level that is off
 * for no stated reason is indistinguishable from one that was never built.
 */
function disabledLevelNote(capability: string | undefined): string {
  const gate =
    capability === undefined || capability === ""
      ? ""
      : ` and would need the ${capability} capability`;
  return ` This level is disabled by policy in this deployment${gate}, so it cannot be granted here.`;
}

/** Picks the chosen integer back out of a select without parsing a number. */
function chosen(choices: readonly number[], raw: string, fallback: number): number {
  return choices.find((value) => String(value) === raw) ?? fallback;
}

/* --------------------------------------------------------------------------
 * Step 2's answer
 * ------------------------------------------------------------------------ */

/**
 * What a compile attempt produced, in the API's own words.
 *
 * MODEL_UNAVAILABLE is not an error and is not rendered as one: the request
 * succeeded, the attempt was recorded, and the answer is that this deployment
 * has no compiler backend. That is a refusal — a boundary working correctly —
 * so it wears the refusal pattern, with the stable code shown verbatim.
 */
function CompileAnswer(props: { readonly result: CompileResult }): ReactNode {
  const { result } = props;
  const codes = result.failure_codes ?? [];
  const clarifications = result.clarifications ?? [];

  if (result.outcome === "SUCCESS" && result.version !== undefined) {
    return (
      <p className="note">
        Attempt {String(result.attempt_no)} compiled. What it produced is below; read it before you
        grant anything.
      </p>
    );
  }

  return (
    <Refusal
      what="This description was not turned into a strategy."
      rule={result.detail}
      code={codes.length > 0 ? codes.join(" · ") : result.outcome}
      remedy={
        codes.includes("COMPILER_UNAVAILABLE")
          ? "Nothing you can change in the description alters this. No compiler backend is configured in this deployment, so no strategy can be compiled and no agent can be created from one. The attempt is recorded against this strategy either way."
          : clarifications.length > 0
            ? "The compiler does not guess. Describe the strategy again, answering the questions below in the text."
            : "Describe the strategy again. Nothing was activated and nothing was inferred from the text."
      }
    >
      <FieldGrid columns={2}>
        <Field label="Outcome" note="What the compiler recorded for this attempt.">
          <StatusBadge tone="warn">{result.outcome}</StatusBadge>
        </Field>
        <Field label="Attempt">
          <Figure kind="count" count={result.attempt_no} />
        </Field>
      </FieldGrid>
      {codes.length > 0 && (
        <ul className="explain-fields">
          {codes.map((code) => (
            <li key={code}>
              <span className="mono-small">{code}</span>
            </li>
          ))}
        </ul>
      )}
      {clarifications.length > 0 && (
        <>
          <p className="note">The compiler could not decide these, and it never guesses:</p>
          <ul className="explain-fields">
            {clarifications.map((question) => (
              <li key={question}>{question}</li>
            ))}
          </ul>
        </>
      )}
    </Refusal>
  );
}

/** The compiled strategy, as a person reads it before approving it. */
function CompiledVersion(props: { readonly version: StrategyVersion }): ReactNode {
  const { version } = props;
  return (
    <>
      <FieldGrid columns={2}>
        <Field label="Version">
          <Figure kind="count" count={version.version} />
        </Field>
        <Field label="Status">
          <StatusBadge tone="info">{version.status}</StatusBadge>
        </Field>
        <Field label="Built">
          {version.built_at === undefined ? (
            <span className="absent">not reported</span>
          ) : (
            formatInstant(version.built_at)
          )}
        </Field>
        <Field
          label="Semantic hash"
          note="Identifies this exact compiled document. An agent is bound to this version, never to whatever the strategy compiles to next."
        >
          <Identifier value={version.ir_hash} label="hash" />
        </Field>
      </FieldGrid>

      <h4>What it is allowed to do</h4>
      {version.effect_set.length === 0 ? (
        <p className="note">
          The compiler recorded no effects for this version. An empty effect set means it claims the
          authority to do nothing at all.
        </p>
      ) : (
        <ul className="explain-fields">
          {version.effect_set.map((effect) => (
            <li key={effect}>
              <span className="mono-small">{effect}</span>
            </li>
          ))}
        </ul>
      )}

      <h4>What it says, in words</h4>
      <p className="cell-prose">{version.human_readable}</p>

      {version.ir !== undefined && (
        <details className="raw">
          <summary>The compiled document itself</summary>
          <pre>{JSON.stringify(version.ir, null, 2)}</pre>
        </details>
      )}
    </>
  );
}

/* --------------------------------------------------------------------------
 * The page
 * ------------------------------------------------------------------------ */

export function AgentNew(): ReactNode {
  const accountId = useActiveAccountId();
  const description = useSurvivesSignIn<Description>("agents.description", NO_DESCRIPTION);
  const grant = useSurvivesSignIn<GrantDraft>("agents.grant", NO_GRANT);
  const [strategy, setStrategy] = useState<Strategy | undefined>(undefined);

  const strategies = useStrategies(accountId);
  const agents = useAgents(accountId);
  const assets = useAssets();
  const createStrategy = useCreateStrategy();
  const compile = useCompileStrategy();
  const createAgent = useCreateAgent();

  if (accountId === undefined) {
    return (
      <Page title="Create an agent">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is nothing to create an agent under."
        />
      </Page>
    );
  }

  if (createAgent.isSuccess) {
    const agent = createAgent.data;
    return (
      <Page title="Create an agent" lead="Created, and stopped.">
        <Panel title={agent.name} description="What the backend recorded.">
          <FieldGrid columns={2}>
            <Field label="Status" note="An agent is created stopped. Enabling it is a separate act.">
              <StatusBadge tone="neutral">{agent.status}</StatusBadge>
            </Field>
            <Field label="Authority">
              <StatusBadge tone="info">
                Level {String(agent.authority.level)} · {agent.authority.name}
              </StatusBadge>
            </Field>
            <Field label="Budget granted" note="A ceiling. No Credits moved when this was created.">
              <Credits base={agent.limits.budget_credits} />
            </Field>
            <Field label="Agent id">
              <Identifier value={agent.id} />
            </Field>
          </FieldGrid>
          <p className="note">{CREDITS_DISCLOSURE}</p>
          <div className="panel-actions">
            <LinkButton to={`/agents/${agent.id}`} variant="primary">
              Open this agent
            </LinkButton>
            <LinkButton to="/agents">Back to the list</LinkButton>
          </div>
        </Panel>
      </Page>
    );
  }

  const compilerConfigured = strategies.data?.compilerConfigured;
  const version = strategy?.current_version ?? compile.data?.version;
  const levels = agents.data?.authorityLevels ?? [];
  const enabledLevels = levels.filter((level) => level.enabled);
  const parsedBudget = parseQuantityInput(grant.value.budget, CREDIT_DECIMALS);
  const parsedPerTrade = parseQuantityInput(grant.value.perTrade, CREDIT_DECIMALS);
  const parsedStop = parseQuantityInput(grant.value.dailyLossStop, CREDIT_DECIMALS);
  const nameOk = grant.value.agentName.trim().length > 0;
  const assetsOk = grant.value.assets.length > 0;
  const grantReady =
    version !== undefined &&
    nameOk &&
    assetsOk &&
    parsedBudget.ok &&
    parsedPerTrade.ok &&
    parsedStop.ok;

  return (
    <Page
      title="Create an agent"
      lead="Four steps, four decisions. Nothing you write here becomes authority over your Credits until you grant it in the last one."
      actions={<LinkButton to="/agents">Back to the list</LinkButton>}
    >
      {compilerConfigured === false && (
        <Refusal
          what="No agent can be created on this deployment."
          rule="The API reports that no compiler backend is configured, so a compile attempt will produce no strategy version."
          code="COMPILER_UNAVAILABLE"
          remedy="An agent is created from a compiled strategy version. Without one there is nothing to create an agent from, and nothing you enter below changes that. You can still describe a strategy and record a compile attempt; both are kept."
        />
      )}

      {/* ---------------------------------------------------------------- */}
      <Panel
        title="1 · Describe what you want"
        description="In your own words. This records the text and nothing else."
      >
        {strategy === undefined ? (
          <form
            className="form"
            onSubmit={(event) => {
              event.preventDefault();
              createStrategy.mutate(
                {
                  accountId,
                  name: description.value.name.trim(),
                  description: description.value.text.trim(),
                  // Minted at the moment of confirmation, so a double submit
                  // records one strategy and a retry after a sign-in records
                  // the same one.
                  idempotencyKey: newIdempotencyKey(),
                },
                {
                  onSuccess: (created) => {
                    setStrategy(created);
                    description.clear();
                  },
                },
              );
            }}
          >
            <FormField label="A name for this strategy" hint="Something you will recognise in a list.">
              {(field) => (
                <input
                  className="input"
                  type="text"
                  maxLength={120}
                  value={description.value.name}
                  onChange={(event) => {
                    description.set({ ...description.value, name: event.target.value });
                  }}
                  {...field}
                />
              )}
            </FormField>
            <FormField
              label="What should it do?"
              hint="What it should watch, when it should act and what it must never do. The compiler does not guess: anything you leave out comes back as a question."
            >
              {(field) => (
                <textarea
                  className="input"
                  rows={8}
                  maxLength={8_000}
                  value={description.value.text}
                  onChange={(event) => {
                    description.set({ ...description.value, text: event.target.value });
                  }}
                  {...field}
                />
              )}
            </FormField>
            {createStrategy.isError && (
              <Explanation error={createStrategy.error} onRetry={createStrategy.reset} />
            )}
            <div className="panel-actions">
              {description.value.name.trim() === "" || description.value.text.trim() === "" ? (
                <Button disabledReason="Give the strategy a name and say what it should do before recording it.">
                  Record this description
                </Button>
              ) : (
                <Button variant="primary" submit busy={createStrategy.isPending} busyLabel="Recording…">
                  Record this description
                </Button>
              )}
            </div>
            <p className="note">
              Recording a description grants nothing. It is kept exactly as you wrote it, and it is
              never read as an instruction by anything that can act.
            </p>
          </form>
        ) : (
          <>
            <FieldGrid columns={2}>
              <Field label="Strategy">{strategy.name}</Field>
              <Field label="Recorded">{formatInstant(strategy.created_at)}</Field>
              <Field label="Source">
                <StatusBadge tone="neutral">{strategy.source_kind}</StatusBadge>
              </Field>
              <Field label="Strategy id">
                <Identifier value={strategy.id} />
              </Field>
            </FieldGrid>
            <p className="cell-prose">{strategy.description}</p>
            <div className="panel-actions">
              <Button
                variant="quiet"
                onClick={() => {
                  setStrategy(undefined);
                  compile.reset();
                  createStrategy.reset();
                }}
              >
                Describe a different strategy
              </Button>
            </div>
          </>
        )}

        {strategy === undefined && (strategies.data?.items.length ?? 0) > 0 && (
          <Disclosure title="Strategies you have already described">
            <ul className="explain-fields">
              {(strategies.data?.items ?? []).map((item: Strategy) => (
                <li key={item.id}>
                  <Button
                    variant="quiet"
                    onClick={() => {
                      setStrategy(item);
                      compile.reset();
                    }}
                  >
                    Continue with “{item.name}”
                  </Button>
                </li>
              ))}
            </ul>
          </Disclosure>
        )}
      </Panel>

      {/* ---------------------------------------------------------------- */}
      <Panel
        title="2 · Ask for it to be compiled"
        description="Describing and compiling are separate acts. Every attempt is recorded, successful or not."
      >
        {compile.isError && <Explanation error={compile.error} onRetry={compile.reset} />}
        {compile.data !== undefined && <CompileAnswer result={compile.data} />}
        <div className="panel-actions">
          {strategy === undefined ? (
            <Button disabledReason="Record a description first. There is nothing to compile until there is.">
              Compile this strategy
            </Button>
          ) : (
            <Button
              variant="primary"
              busy={compile.isPending}
              busyLabel="Compiling…"
              onClick={() => {
                compile.mutate({
                  strategyId: strategy.id,
                  // A new key is a new request, which is the right reading:
                  // asking again is a decision, not a retry of the last one.
                  idempotencyKey: newIdempotencyKey(),
                });
              }}
            >
              {compile.data === undefined ? "Compile this strategy" : "Try compiling again"}
            </Button>
          )}
        </div>
      </Panel>

      {/* ---------------------------------------------------------------- */}
      <Panel
        title="3 · Review what was compiled"
        description="The compiled strategy in words and in effects, as the compiler produced it."
      >
        {version === undefined ? (
          <EmptyState
            title="There is nothing to review"
            body="No compiled version exists for this strategy. This page will not show you a strategy that was never produced, and an agent cannot be created without one."
          />
        ) : (
          <CompiledVersion version={version} />
        )}
      </Panel>

      {/* ---------------------------------------------------------------- */}
      <Panel
        title="4 · Grant authority and limits"
        description="What the agent may do, with how much, and over which assets."
      >
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (version === undefined || strategy === undefined) return;
            const limits: AgentLimits = {
              budget_credits: parsedBudget.value,
              per_trade_cap_credits: parsedPerTrade.value,
              daily_loss_stop_credits: parsedStop.value,
              max_position_share_bps: grant.value.shareBps,
              allowed_asset_ids: [...grant.value.assets],
              schedule:
                grant.value.interval === MANUAL
                  ? { kind: "MANUAL" }
                  : { kind: "INTERVAL", interval_minutes: grant.value.interval },
            };
            createAgent.mutate(
              {
                accountId,
                strategyId: strategy.id,
                strategyVersionId: version.id,
                name: grant.value.agentName.trim(),
                authorityLevel: grant.value.level,
                limits,
                idempotencyKey: newIdempotencyKey(),
              },
              { onSuccess: () => grant.clear() },
            );
          }}
        >
          <FormField label="A name for this agent">
            {(field) => (
              <input
                className="input"
                type="text"
                maxLength={120}
                value={grant.value.agentName}
                onChange={(event) => {
                  grant.set({ ...grant.value, agentName: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>

          <fieldset className="form-row">
            <legend>Authority — who decides</legend>
            {levels.length === 0 && (
              <p className="note">
                The authority ladder has not been read from the API yet, so no level can be chosen.
              </p>
            )}
            {levels.map((level) => {
              const reasonId = `authority-${String(level.level)}-reason`;
              return (
                <label className="radio" key={level.level}>
                  <input
                    type="radio"
                    name="authority"
                    value={String(level.level)}
                    checked={grant.value.level === level.level}
                    disabled={!level.enabled}
                    aria-describedby={reasonId}
                    onChange={() => {
                      grant.set({ ...grant.value, level: level.level });
                    }}
                  />
                  <span>
                    Level {String(level.level)} · {level.name}
                    <span className="field-note" id={reasonId}>
                      {level.summary}
                      {!level.enabled && disabledLevelNote(level.required_capability)}
                    </span>
                  </span>
                </label>
              );
            })}
          </fieldset>

          <FieldGrid columns={3}>
            <Field label="Budget you are granting" note="A ceiling on Credits at risk. Nothing moves now.">
              <Credits base={parsedBudget.ok ? parsedBudget.value : undefined} />
            </Field>
            <Field label="Most per trade">
              <Credits base={parsedPerTrade.ok ? parsedPerTrade.value : undefined} />
            </Field>
            <Field label="Daily loss stop">
              <Credits base={parsedStop.ok ? parsedStop.value : undefined} />
            </Field>
          </FieldGrid>

          <FormField
            label="Credit budget"
            hint="The most this agent may ever put at risk."
            {...(grant.value.budget === "" || parsedBudget.ok ? {} : { error: parsedBudget.error })}
          >
            {(field) => (
              <input
                className="input"
                type="text"
                inputMode="decimal"
                value={grant.value.budget}
                onChange={(event) => {
                  grant.set({ ...grant.value, budget: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField
            label="Cap per trade"
            hint="The most it may commit in any single trade."
            {...(grant.value.perTrade === "" || parsedPerTrade.ok
              ? {}
              : { error: parsedPerTrade.error })}
          >
            {(field) => (
              <input
                className="input"
                type="text"
                inputMode="decimal"
                value={grant.value.perTrade}
                onChange={(event) => {
                  grant.set({ ...grant.value, perTrade: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField
            label="Daily loss stop"
            hint="It stops for the day once losses reach this."
            {...(grant.value.dailyLossStop === "" || parsedStop.ok
              ? {}
              : { error: parsedStop.error })}
          >
            {(field) => (
              <input
                className="input"
                type="text"
                inputMode="decimal"
                value={grant.value.dailyLossStop}
                onChange={(event) => {
                  grant.set({ ...grant.value, dailyLossStop: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>

          <FormField
            label="Largest share of one position"
            hint="As a share of the budget above, in basis points."
          >
            {(field) => (
              <select
                className="input"
                value={String(grant.value.shareBps)}
                onChange={(event) => {
                  grant.set({
                    ...grant.value,
                    shareBps: chosen(SHARE_CHOICES, event.target.value, grant.value.shareBps),
                  });
                }}
                {...field}
              >
                {SHARE_CHOICES.map((bps) => (
                  <option key={bps} value={String(bps)}>
                    {String(bps)} bps
                  </option>
                ))}
              </select>
            )}
          </FormField>

          <FormField label="How often it may evaluate">
            {(field) => (
              <select
                className="input"
                value={String(grant.value.interval)}
                onChange={(event) => {
                  grant.set({
                    ...grant.value,
                    interval: chosen(INTERVAL_CHOICES, event.target.value, grant.value.interval),
                  });
                }}
                {...field}
              >
                {INTERVAL_CHOICES.map((minutes) => (
                  <option key={minutes} value={String(minutes)}>
                    {intervalLabel(minutes)}
                  </option>
                ))}
              </select>
            )}
          </FormField>

          <fieldset className="form-row">
            <legend>Assets it may touch</legend>
            <p className="field-note">
              The universe is never empty and never implied. An agent may only ever touch an asset
              you named here, and it can never add one to this list itself.
            </p>
            {assets.isError && (
              <Explanation
                error={assets.error}
                onRetry={() => {
                  void assets.refetch();
                }}
              />
            )}
            {assets.data !== undefined && assets.data.length === 0 && (
              <p className="note">
                The backend returned no assets at all, so there is nothing to name and no agent can
                be created. That is the registry being empty rather than a universe of nothing.
              </p>
            )}
            {(assets.data ?? []).map((asset: Asset) => (
              <label className="checkbox" key={asset.id}>
                <input
                  type="checkbox"
                  checked={grant.value.assets.includes(asset.id)}
                  onChange={(event) => {
                    const next = event.target.checked
                      ? [...grant.value.assets, asset.id]
                      : grant.value.assets.filter((id) => id !== asset.id);
                    grant.set({ ...grant.value, assets: next });
                  }}
                />
                <span>
                  {asset.name} <span className="mono-small">{asset.symbol}</span>
                  <span className="field-note">
                    {asset.kind} · {asset.status}
                    {asset.status === "ACTIVE"
                      ? ""
                      : " — the backend restricts this asset, so naming it here does not make it tradable."}
                  </span>
                </span>
              </label>
            ))}
            <p className="field-note">
              This is the asset registry the backend checks a grant against. A Nodal-native asset
              somebody creates is a row in that same registry, so one appears here once it exists.
            </p>
            <p className="field-note">{NATIVE_ASSET_RISK}</p>
          </fieldset>

          {createAgent.isError && <Explanation error={createAgent.error} onRetry={createAgent.reset} />}

          <div className="panel-actions">
            {grantReady ? (
              <Button variant="primary" submit busy={createAgent.isPending} busyLabel="Creating…">
                Create this agent, stopped
              </Button>
            ) : (
              <Button
                disabledReason={
                  version === undefined
                    ? "An agent is created from a compiled strategy version, and there is not one. Nothing on this form can substitute for it."
                    : !nameOk
                      ? "Give the agent a name first."
                      : !assetsOk
                        ? "Name at least one asset this agent may touch. An agent with no universe cannot be granted anything."
                        : "Enter a budget, a per-trade cap and a daily loss stop as whole Credit amounts."
                }
              >
                Create this agent, stopped
              </Button>
            )}
          </div>
          <p className="note">{CREDITS_DISCLOSURE}</p>
          <p className="note">
            Creating an agent does not start it. It is created stopped, and enabling it is a
            separate decision made on its own page — where you can also read what, if anything, is
            actually evaluating agents in this deployment.
          </p>
        </form>

        {enabledLevels.length === 0 && levels.length > 0 && (
          <p className="note">
            No authority level is enabled in this deployment, so no agent can be granted one.{" "}
            <Link to="/agents">The full ladder is on the agents page.</Link>
          </p>
        )}
      </Panel>
    </Page>
  );
}
