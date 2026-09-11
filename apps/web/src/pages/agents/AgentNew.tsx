/**
 * `/agents/new` — state, compile, review, accept, then grant.
 *
 * Goal §18 states the rule this screen exists to enforce:
 *
 *   Do not silently turn natural-language text directly into financial
 *   authority. Show a human-understandable compiled strategy before
 *   activation.
 *
 * So the five steps are five separate acts with five separate confirmations,
 * and each one stays on the page after it is done. A wizard that discards the
 * step behind it is a wizard in which nobody can check what they agreed to.
 *
 *   1. STATE — `POST /v1/strategies`. On a deployment whose compiler reads a
 *      DECLARED strategy the form below is the strategy: an instrument, a
 *      venue, an entry rule, an exit rule, three limits, a capital floor, a
 *      cadence, a mode. The description box is kept beside it and is recorded
 *      exactly as written — and, on such a deployment, never interpreted.
 *   2. COMPILE — `POST /v1/strategies/{id}/compile`. Every attempt is recorded,
 *      including one on a deployment with no compiler backend
 *      (COMPILER_UNAVAILABLE) and one whose declared strategy is incomplete
 *      (STRUCTURED_CONSTRAINTS_REQUIRED, with every missing field named). Those
 *      codes are rendered as exactly that. Nothing is inferred in their place.
 *   3. REVIEW — the compiled strategy in words, the compiler's own rationale
 *      for each element, its effects and its hash. This screen never writes
 *      that text itself.
 *   4. ACCEPT — `POST /v1/strategies/{id}/versions/{n}/accept`, echoing the
 *      hash that was on screen. This is the act goal §18 requires and the only
 *      thing that makes an agent possible; no compiler can perform it.
 *   5. GRANT — `POST /v1/agents` with an authority level, a Credit budget, a
 *      per-trade cap, a daily loss stop and the assets the agent may touch.
 *
 * WITHOUT AN ACCEPTED VERSION THERE IS NO STEP 5. The agent resource is created
 * from a strategy VERSION the owner accepted, so an account on a tier with no
 * compiler cannot make an agent at all — and the button says so, with the
 * reason, rather than being absent or, worse, present and lying.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useAcceptStrategyVersion,
  useCompileStrategy,
  useCreateAgent,
  useCreateStrategy,
  useAgents,
  useAssets,
  useInstrument,
  useInstruments,
  useStrategies,
  type AgentLimits,
  type CompileResult,
  type Asset,
  type Instrument,
  type Strategy,
  type StructuredStrategy,
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
import { fromBaseUnits } from "../../lib/format.ts";
import { parseQuantityInput } from "../../lib/money.ts";
import { useIdempotencyKey, requestSignature } from "../../lib/idempotency.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { formatInstant } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";
import { CREDIT_DECIMALS } from "../../lib/credits.ts";
import { Credits } from "./parts.tsx";

/* --------------------------------------------------------------------------
 * The two drafts, and why they are integers
 * ------------------------------------------------------------------------ */

interface Description {
  readonly name: string;
  readonly text: string;
}

const NO_DESCRIPTION: Description = { name: "", text: "" };

/**
 * The strategy a person states, field by field.
 *
 * Every amount is held as the customer typed it and converted to exact USD
 * MINOR UNITS at the moment of submission, by `parseQuantityInput(raw, 2)` —
 * the same parser every other amount on this site goes through. Nothing here
 * is ever a number: a threshold typed as one hundred and thirty five dollars
 * leaves this screen as the digit string of its cents, and no step in between
 * is a float.
 *
 * The two integers — the cadence and the hourly intent ceiling — are chosen
 * from fixed lists rather than typed, so there is nothing to parse and no
 * rounding to get wrong.
 */
interface StructuredDraft {
  readonly instrumentId: string;
  readonly venue: string;
  readonly entryKind: RuleKind;
  readonly entryComparator: Comparator;
  readonly entryPrice: string;
  readonly exitKind: RuleKind;
  readonly exitComparator: Comparator;
  readonly exitPrice: string;
  readonly maxSingleTrade: string;
  readonly maxPosition: string;
  readonly maxDailyLoss: string;
  readonly minAllocation: string;
  readonly intervalMinutes: number;
  readonly maxIntentsPerHour: number;
}

type RuleKind = "PRICE_THRESHOLD" | "EVERY_INTERVAL";
type Comparator = "LT" | "LTE" | "GT" | "GTE";

const RULE_KINDS: readonly RuleKind[] = ["PRICE_THRESHOLD", "EVERY_INTERVAL"];
const COMPARATORS: readonly Comparator[] = ["LT", "LTE", "GT", "GTE"];

/** USD amounts are exact to the cent, and the API takes them in minor units. */
const USD_DECIMALS = 2;

/** How often the strategy evaluates, in whole minutes. */
const EVALUATE_CHOICES: readonly number[] = [1, 5, 15, 60, 240, 1_440];
/** The most trade intents it may create in an hour. */
const INTENTS_PER_HOUR_CHOICES: readonly number[] = [1, 2, 4, 6, 12, 30];

const NO_STRUCTURED: StructuredDraft = {
  instrumentId: "",
  venue: "",
  entryKind: "PRICE_THRESHOLD",
  entryComparator: "LTE",
  entryPrice: "",
  exitKind: "PRICE_THRESHOLD",
  exitComparator: "GTE",
  exitPrice: "",
  maxSingleTrade: "",
  maxPosition: "",
  maxDailyLoss: "",
  minAllocation: "",
  intervalMinutes: 15,
  maxIntentsPerHour: 2,
};

function comparatorLabel(c: Comparator): string {
  if (c === "LT") return "is below";
  if (c === "LTE") return "is at or below";
  if (c === "GT") return "is above";
  return "is at or above";
}

function ruleKindLabel(k: RuleKind): string {
  return k === "PRICE_THRESHOLD" ? "When the price crosses a threshold I set" : "On every evaluation";
}

function evaluateLabel(minutes: number): string {
  if (minutes === 1) return "Every minute";
  if (minutes === 60) return "Every hour";
  if (minutes === 1_440) return "Once a day";
  return `Every ${String(minutes)} minutes`;
}

/** Picks the chosen member back out of a select without parsing anything. */
function chosenOf<T extends string>(choices: readonly T[], raw: string, fallback: T): T {
  return choices.find((value) => value === raw) ?? fallback;
}

/**
 * What the declared strategy is missing, in this screen's own words.
 *
 * It duplicates nothing the API decides: the API refuses an incomplete document
 * by naming the fields, and that refusal is rendered verbatim when it comes
 * back. This exists so the button can say why it is disabled BEFORE a request
 * is sent, which is the same courtesy every other form on this site extends.
 */
function structuredGaps(draft: StructuredDraft, instruments: readonly Instrument[]): string[] {
  const gaps: string[] = [];
  const named = instruments.find((i) => i.id === draft.instrumentId);
  if (named === undefined) gaps.push("Choose the one instrument this strategy may touch.");
  if (draft.venue === "") gaps.push("Choose the venue it trades on.");
  if (draft.entryKind === "PRICE_THRESHOLD" && !parseQuantityInput(draft.entryPrice, USD_DECIMALS).ok) {
    gaps.push("State the entry price, exactly.");
  }
  if (draft.exitKind === "PRICE_THRESHOLD" && !parseQuantityInput(draft.exitPrice, USD_DECIMALS).ok) {
    gaps.push("State the exit price, exactly.");
  }
  for (const limit of [
    { value: draft.maxSingleTrade, what: "the most it may put into one trade" },
    { value: draft.maxPosition, what: "the largest position it may hold" },
    { value: draft.maxDailyLoss, what: "the loss that stops it for the day" },
    { value: draft.minAllocation, what: "the smallest allocation it needs to run" },
  ]) {
    if (!parseQuantityInput(limit.value, USD_DECIMALS).ok) gaps.push(`State ${limit.what}.`);
  }
  return gaps;
}

/** Turns the draft into the document the API takes, or undefined when it cannot. */
function toStructuredStrategy(
  draft: StructuredDraft,
  instruments: readonly Instrument[],
): StructuredStrategy | undefined {
  const named = instruments.find((i) => i.id === draft.instrumentId);
  if (named === undefined || draft.venue === "") return undefined;
  const amounts = {
    entry: parseQuantityInput(draft.entryPrice, USD_DECIMALS),
    exit: parseQuantityInput(draft.exitPrice, USD_DECIMALS),
    maxSingleTrade: parseQuantityInput(draft.maxSingleTrade, USD_DECIMALS),
    maxPosition: parseQuantityInput(draft.maxPosition, USD_DECIMALS),
    maxDailyLoss: parseQuantityInput(draft.maxDailyLoss, USD_DECIMALS),
    minAllocation: parseQuantityInput(draft.minAllocation, USD_DECIMALS),
  };
  if (
    !amounts.maxSingleTrade.ok ||
    !amounts.maxPosition.ok ||
    !amounts.maxDailyLoss.ok ||
    !amounts.minAllocation.ok
  ) {
    return undefined;
  }
  const rule = (kind: RuleKind, comparator: Comparator, price: { ok: boolean; value: string }) =>
    kind === "EVERY_INTERVAL"
      ? { kind: "EVERY_INTERVAL" as const }
      : { kind: "PRICE_THRESHOLD" as const, comparator, price_usd: price.value };
  if (draft.entryKind === "PRICE_THRESHOLD" && !amounts.entry.ok) return undefined;
  if (draft.exitKind === "PRICE_THRESHOLD" && !amounts.exit.ok) return undefined;

  return {
    schema_version: 1,
    universe: { instrument: named.canonical_name, venue: draft.venue },
    entry: rule(draft.entryKind, draft.entryComparator, amounts.entry),
    exit: rule(draft.exitKind, draft.exitComparator, amounts.exit),
    risk_limits: {
      max_single_trade_usd: amounts.maxSingleTrade.value,
      max_position_usd: amounts.maxPosition.value,
      max_daily_loss_usd: amounts.maxDailyLoss.value,
    },
    capital_limit: { min_allocation_usd: amounts.minAllocation.value },
    frequency: {
      interval_minutes: draft.intervalMinutes,
      max_intents_per_hour: draft.maxIntentsPerHour,
    },
    // The only mode this build compiles. It is shown, not chosen: a control
    // offering a mode the backend refuses would be an offer nobody can take.
    mode: "PAPER",
  };
}

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
function CompiledVersion(props: {
  readonly version: StrategyVersion;
  readonly rationale: CompileResult["rationale"];
}): ReactNode {
  const { version } = props;
  const details = props.rationale?.details ?? [];
  return (
    <>
      <p className="note">
        This is the compiled strategy, in full, before anything acts on your behalf. Read it. An
        agent can only be created from a version you have accepted, and accepting it is the next
        step.
      </p>
      {version.sandbox && (
        <p className="note" data-temp="simulated">
          This strategy was compiled by a sandbox compiler. Everything built from it is a rehearsal:
          no real capital can move, and this version cannot exist on a production deployment.
        </p>
      )}
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
      <pre className="raw-block">{version.human_readable}</pre>

      {props.rationale !== undefined && (
        <>
          <h4>Where each part of it came from</h4>
          <p className="cell-prose">{props.rationale.summary}</p>
          {details.length > 0 && (
            <ul className="explain-fields">
              {details.map((line) => (
                <li key={line}>{line}</li>
              ))}
            </ul>
          )}
        </>
      )}

      {version.ir !== undefined && (
        <details className="raw">
          <summary>The compiled document itself</summary>
          <pre>{JSON.stringify(version.ir, null, 2)}</pre>
        </details>
      )}
    </>
  );
}

/** One entry or exit rule, stated. */
function RuleFields(props: {
  readonly legend: string;
  readonly kind: RuleKind;
  readonly comparator: Comparator;
  readonly price: string;
  readonly onKind: (kind: RuleKind) => void;
  readonly onComparator: (comparator: Comparator) => void;
  readonly onPrice: (price: string) => void;
}): ReactNode {
  return (
    <fieldset className="form-row">
      <legend>{props.legend}</legend>
      <FormField label="The rule">
        {(field) => (
          <select
            className="input"
            value={props.kind}
            onChange={(event) => {
              props.onKind(chosenOf(RULE_KINDS, event.target.value, props.kind));
            }}
            {...field}
          >
            {RULE_KINDS.map((kind) => (
              <option key={kind} value={kind}>
                {ruleKindLabel(kind)}
              </option>
            ))}
          </select>
        )}
      </FormField>
      {props.kind === "PRICE_THRESHOLD" && (
        <>
          <FormField label="When the price">
            {(field) => (
              <select
                className="input"
                value={props.comparator}
                onChange={(event) => {
                  props.onComparator(chosenOf(COMPARATORS, event.target.value, props.comparator));
                }}
                {...field}
              >
                {COMPARATORS.map((comparator) => (
                  <option key={comparator} value={comparator}>
                    {comparatorLabel(comparator)}
                  </option>
                ))}
              </select>
            )}
          </FormField>
          <UsdField
            label="This price"
            hint="Exact, to the cent. The compiler compares the instrument's mid price against it."
            value={props.price}
            onChange={props.onPrice}
          />
        </>
      )}
      {props.kind === "EVERY_INTERVAL" && (
        <p className="field-note">
          No condition: the rule applies on every evaluation, at the cadence you set below.
        </p>
      )}
    </fieldset>
  );
}

/**
 * An exact US dollar amount.
 *
 * The typed text goes through `parseQuantityInput(raw, 2)` — the same parser
 * every other amount on this site uses — and what leaves this screen is the
 * integer string of cents. Nothing here is ever a number.
 */
function UsdField(props: {
  readonly label: string;
  readonly hint?: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
}): ReactNode {
  const parsed = parseQuantityInput(props.value, USD_DECIMALS);
  const hint = props.hint === undefined ? {} : { hint: props.hint };
  const error = props.value === "" || parsed.ok ? {} : { error: parsed.error };
  return (
    <>
      <FormField label={props.label} {...hint} {...error}>
        {(field) => (
          <input
            className="input"
            type="text"
            inputMode="decimal"
            value={props.value}
            onChange={(event) => {
              props.onChange(event.target.value);
            }}
            {...field}
          />
        )}
      </FormField>
      {parsed.ok && (
        <p className="field-note">
          Recorded as{" "}
          <Figure
            kind="money"
            value={{ base: parsed.value, scale: USD_DECIMALS }}
            symbol="USD"
          />{" "}
          — exactly {fromBaseUnits(parsed.value, USD_DECIMALS)} US dollars.
        </p>
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
  const declared = useSurvivesSignIn<StructuredDraft>("agents.structured", NO_STRUCTURED);
  const grant = useSurvivesSignIn<GrantDraft>("agents.grant", NO_GRANT);
  const [strategy, setStrategy] = useState<Strategy | undefined>(undefined);
  const [accepted, setAccepted] = useState<StrategyVersion | undefined>(undefined);

  const strategies = useStrategies(accountId);
  const agents = useAgents(accountId);
  const assets = useAssets();
  const instruments = useInstruments();
  // The venues come from the instrument's own listings, so the choice offered
  // is the set the backend would accept and not a list this page keeps.
  const instrumentDetail = useInstrument(
    declared.value.instrumentId === "" ? undefined : declared.value.instrumentId,
  );
  const createStrategy = useCreateStrategy();
  const compile = useCompileStrategy();
  const acceptVersion = useAcceptStrategyVersion();
  const createAgent = useCreateAgent();

  // Three commands, three keys, each minted at its own confirmation and kept
  // only while the body it belongs to is unchanged. They survive the sign-in
  // round trip with the drafts above, so a session that expires between the
  // press and the answer comes back to a RETRY of the same request rather than
  // to a second strategy or a second agent.
  const strategyKey = useIdempotencyKey("agents.new.strategy.key");
  const compileKey = useIdempotencyKey("agents.new.compile.key");
  const acceptKey = useIdempotencyKey("agents.new.accept.key");
  const agentKey = useIdempotencyKey("agents.new.agent.key");

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
  const compiler = strategies.data?.compiler;
  const structuredCompiler = compiler?.structured === true;
  const instrumentList = instruments.data ?? [];
  const venuesForInstrument = (instrumentDetail.data?.listings ?? [])
    .filter((listing) => listing.venue_status !== "DISABLED" && listing.status !== "DISABLED")
    .map((listing) => listing.venue);
  const gaps = structuredCompiler ? structuredGaps(declared.value, instrumentList) : [];
  const declaredStrategy = structuredCompiler
    ? toStructuredStrategy(declared.value, instrumentList)
    : undefined;

  const compiledVersion = accepted ?? compile.data?.version ?? strategy?.current_version;
  // Step 5 needs an ACCEPTED version, which is the whole point of step 4: an
  // agent is created from a version its owner read and approved, and the
  // backend refuses one that is merely COMPILED.
  const version = compiledVersion?.status === "ACCEPTED" ? compiledVersion : undefined;
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
        title="1 · State the strategy"
        description={
          structuredCompiler
            ? "Field by field. This deployment's compiler builds exactly what you state here and reads nothing else."
            : "In your own words. This records the text and nothing else."
        }
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
                  ...(declaredStrategy === undefined ? {} : { constraints: declaredStrategy }),
                  // Minted at the moment of confirmation and reused for a retry
                  // of this same body, so a double submit records one strategy
                  // and a retry after a sign-in records the same one. Editing
                  // the name, the description or any stated field is a
                  // different strategy, and a different strategy gets its own
                  // key.
                  idempotencyKey: strategyKey.forRequest(
                    requestSignature([
                      accountId,
                      description.value.name.trim(),
                      description.value.text.trim(),
                      declaredStrategy === undefined ? "" : JSON.stringify(declaredStrategy),
                    ]),
                  ),
                },
                {
                  onSuccess: (created) => {
                    setStrategy(created);
                    strategyKey.clear();
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
            {structuredCompiler && (
              <>
                <fieldset className="form-row">
                  <legend>The universe — what it may touch</legend>
                  <p className="field-note">
                    One instrument, on one venue. Both come from this deployment's registry: a
                    strategy cannot name a market the backend does not list.
                  </p>
                  {instruments.isError && (
                    <Explanation
                      error={instruments.error}
                      onRetry={() => {
                        void instruments.refetch();
                      }}
                    />
                  )}
                  {instruments.data !== undefined && instruments.data.length === 0 && (
                    <p className="note">
                      The backend lists no instruments at all, so there is nothing to name and no
                      strategy can be stated. That is the registry being empty rather than a
                      universe of nothing.
                    </p>
                  )}
                  <FormField label="Instrument">
                    {(field) => (
                      <select
                        className="input"
                        value={declared.value.instrumentId}
                        onChange={(event) => {
                          declared.set({
                            ...declared.value,
                            instrumentId: event.target.value,
                            venue: "",
                          });
                        }}
                        {...field}
                      >
                        <option value="">Choose an instrument</option>
                        {instrumentList.map((instrument: Instrument) => (
                          <option key={instrument.id} value={instrument.id}>
                            {instrument.canonical_name} · {instrument.status}
                          </option>
                        ))}
                      </select>
                    )}
                  </FormField>
                  <FormField
                    label="Venue"
                    hint="Only the venues that list this instrument and may take new actions."
                  >
                    {(field) => (
                      <select
                        className="input"
                        value={declared.value.venue}
                        onChange={(event) => {
                          declared.set({ ...declared.value, venue: event.target.value });
                        }}
                        {...field}
                      >
                        <option value="">Choose a venue</option>
                        {venuesForInstrument.map((venue) => (
                          <option key={venue} value={venue}>
                            {venue}
                          </option>
                        ))}
                      </select>
                    )}
                  </FormField>
                  {declared.value.instrumentId !== "" && venuesForInstrument.length === 0 && (
                    <p className="note">
                      No venue lists this instrument in a status that can take new actions, so no
                      strategy can trade it here.
                    </p>
                  )}
                </fieldset>

                <RuleFields
                  legend="Entry — when it may buy"
                  kind={declared.value.entryKind}
                  comparator={declared.value.entryComparator}
                  price={declared.value.entryPrice}
                  onKind={(kind) => {
                    declared.set({ ...declared.value, entryKind: kind });
                  }}
                  onComparator={(comparator) => {
                    declared.set({ ...declared.value, entryComparator: comparator });
                  }}
                  onPrice={(price) => {
                    declared.set({ ...declared.value, entryPrice: price });
                  }}
                />
                <RuleFields
                  legend="Exit — when it may close"
                  kind={declared.value.exitKind}
                  comparator={declared.value.exitComparator}
                  price={declared.value.exitPrice}
                  onKind={(kind) => {
                    declared.set({ ...declared.value, exitKind: kind });
                  }}
                  onComparator={(comparator) => {
                    declared.set({ ...declared.value, exitComparator: comparator });
                  }}
                  onPrice={(price) => {
                    declared.set({ ...declared.value, exitPrice: price });
                  }}
                />

                <fieldset className="form-row">
                  <legend>Limits — the ceilings you set</legend>
                  <p className="field-note">
                    Exact amounts in US dollars, to the cent. They are checked against this
                    deployment's risk policy: a limit looser than the policy's is refused, never
                    quietly tightened.
                  </p>
                  <UsdField
                    label="Most in one trade"
                    hint="Also the size of every trade this strategy proposes."
                    value={declared.value.maxSingleTrade}
                    onChange={(value) => {
                      declared.set({ ...declared.value, maxSingleTrade: value });
                    }}
                  />
                  <UsdField
                    label="Largest position"
                    value={declared.value.maxPosition}
                    onChange={(value) => {
                      declared.set({ ...declared.value, maxPosition: value });
                    }}
                  />
                  <UsdField
                    label="Daily loss stop"
                    hint="It stops for the day once losses reach this."
                    value={declared.value.maxDailyLoss}
                    onChange={(value) => {
                      declared.set({ ...declared.value, maxDailyLoss: value });
                    }}
                  />
                  <UsdField
                    label="Smallest allocation it needs"
                    hint="Below this the strategy will not run at all."
                    value={declared.value.minAllocation}
                    onChange={(value) => {
                      declared.set({ ...declared.value, minAllocation: value });
                    }}
                  />
                </fieldset>

                <fieldset className="form-row">
                  <legend>Cadence and mode</legend>
                  <FormField label="How often it evaluates">
                    {(field) => (
                      <select
                        className="input"
                        value={String(declared.value.intervalMinutes)}
                        onChange={(event) => {
                          declared.set({
                            ...declared.value,
                            intervalMinutes: chosen(
                              EVALUATE_CHOICES,
                              event.target.value,
                              declared.value.intervalMinutes,
                            ),
                          });
                        }}
                        {...field}
                      >
                        {EVALUATE_CHOICES.map((minutes) => (
                          <option key={minutes} value={String(minutes)}>
                            {evaluateLabel(minutes)}
                          </option>
                        ))}
                      </select>
                    )}
                  </FormField>
                  <FormField label="Most trade proposals in an hour">
                    {(field) => (
                      <select
                        className="input"
                        value={String(declared.value.maxIntentsPerHour)}
                        onChange={(event) => {
                          declared.set({
                            ...declared.value,
                            maxIntentsPerHour: chosen(
                              INTENTS_PER_HOUR_CHOICES,
                              event.target.value,
                              declared.value.maxIntentsPerHour,
                            ),
                          });
                        }}
                        {...field}
                      >
                        {INTENTS_PER_HOUR_CHOICES.map((n) => (
                          <option key={n} value={String(n)}>
                            {String(n)}
                          </option>
                        ))}
                      </select>
                    )}
                  </FormField>
                  <Field
                    label="Mode"
                    note="The only mode this build compiles. Anything that moves value is refused with the reason, never downgraded to this one quietly."
                  >
                    <StatusBadge tone="neutral">PAPER</StatusBadge>
                  </Field>
                </fieldset>
              </>
            )}

            <FormField
              label={structuredCompiler ? "Anything else you want recorded" : "What should it do?"}
              hint={
                structuredCompiler
                  ? "Recorded, never interpreted. This deployment's compiler builds the strategy from the fields above and does not read these words — nothing you write here changes what is compiled."
                  : "What it should watch, when it should act and what it must never do. The compiler does not guess: anything you leave out comes back as a question."
              }
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
              {description.value.name.trim() === "" ||
              description.value.text.trim() === "" ||
              gaps.length > 0 ? (
                <Button
                  disabledReason={
                    gaps.length > 0
                      ? `The strategy is not stated yet. ${gaps.join(" ")}`
                      : "Give the strategy a name and say what it should do before recording it."
                  }
                >
                  Record this strategy
                </Button>
              ) : (
                <Button variant="primary" submit busy={createStrategy.isPending} busyLabel="Recording…">
                  Record this strategy
                </Button>
              )}
            </div>
            <p className="note">
              Recording a strategy grants nothing. The words are kept exactly as you wrote them, and
              they are never read as an instruction by anything that can act.
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
                compile.mutate(
                  {
                    strategyId: strategy.id,
                    // "Try compiling again" is a new DECISION, not a retry of the
                    // last one — the attempt number the backend records is the
                    // evidence that it was asked twice — so each attempt carries
                    // its own key, and the answered attempt number is in the
                    // signature that mints it. What the key still buys is the
                    // case a fresh one would get wrong: a reply lost in transit,
                    // where pressing the button again must re-send THIS attempt
                    // rather than start another.
                    idempotencyKey: compileKey.forRequest(
                      requestSignature([strategy.id, String(compile.data?.attempt_no ?? "")]),
                    ),
                  },
                  {
                    // On SUCCESS only: a compile that failed in transit must be
                    // re-sent as the same attempt, not recorded as a second one.
                    onSuccess: () => {
                      compileKey.clear();
                    },
                  },
                );
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
        {...(compiledVersion?.sandbox === true ? { temp: "simulated" as const } : {})}
      >
        {compiledVersion === undefined ? (
          <EmptyState
            title="There is nothing to review"
            body="No compiled version exists for this strategy. This page will not show you a strategy that was never produced, and an agent cannot be created without one."
          />
        ) : (
          <CompiledVersion version={compiledVersion} rationale={compile.data?.rationale} />
        )}
      </Panel>

      {/* ---------------------------------------------------------------- */}
      <Panel
        title="4 · Accept this strategy"
        description="Your approval of the exact document above. Nothing can act on your behalf until you give it."
      >
        {compiledVersion === undefined ? (
          <EmptyState
            title="There is nothing to accept"
            body="Accepting a strategy means approving a compiled document, and none exists yet."
          />
        ) : compiledVersion.status === "ACCEPTED" ? (
          <>
            <FieldGrid columns={2}>
              <Field label="Status">
                <StatusBadge tone="good">ACCEPTED</StatusBadge>
              </Field>
              <Field label="Accepted">
                {compiledVersion.accepted_at === undefined ? (
                  <span className="absent">not reported</span>
                ) : (
                  formatInstant(compiledVersion.accepted_at)
                )}
              </Field>
            </FieldGrid>
            <p className="note">
              You accepted this exact document. An agent created below is bound to it and to nothing
              else — not to whatever this strategy compiles to next.
            </p>
          </>
        ) : (
          <>
            <p className="note">
              Accepting records that you read the strategy above and approve it. It is sent with the
              hash shown in step 3, so what is approved is the document on this screen and not
              whatever may have been compiled since. Accepting grants nothing on its own.
            </p>
            {acceptVersion.isError && (
              <Explanation error={acceptVersion.error} onRetry={acceptVersion.reset} />
            )}
            <div className="panel-actions">
              <Button
                variant="primary"
                busy={acceptVersion.isPending}
                busyLabel="Accepting…"
                onClick={() => {
                  if (strategy === undefined) return;
                  acceptVersion.mutate(
                    {
                      strategyId: strategy.id,
                      version: compiledVersion.version,
                      // The hash this screen DISPLAYED, which is what makes the
                      // approval an approval of this document.
                      irHash: compiledVersion.ir_hash,
                      idempotencyKey: acceptKey.forRequest(
                        requestSignature([strategy.id, compiledVersion.ir_hash]),
                      ),
                    },
                    {
                      onSuccess: (v) => {
                        setAccepted(v);
                        acceptKey.clear();
                      },
                    },
                  );
                }}
              >
                I have read this strategy and accept it
              </Button>
            </div>
          </>
        )}
      </Panel>

      {/* ---------------------------------------------------------------- */}
      <Panel
        title="5 · Grant authority and limits"
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
                // The grant IS the body: the version being given authority, the
                // level, and the three limits. Change any of them and this is a
                // different agent with different authority, which must never be
                // created under the key of the last one.
                idempotencyKey: agentKey.forRequest(
                  requestSignature([
                    version.id,
                    grant.value.agentName.trim(),
                    String(grant.value.level),
                    limits.budget_credits,
                    limits.per_trade_cap_credits,
                    limits.daily_loss_stop_credits,
                    String(limits.max_position_share_bps),
                    [...grant.value.assets].join(","),
                    JSON.stringify(limits.schedule),
                  ]),
                ),
              },
              {
                onSuccess: () => {
                  agentKey.clear();
                  grant.clear();
                },
              },
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
                    ? compiledVersion === undefined
                      ? "An agent is created from a compiled strategy version, and there is not one. Nothing on this form can substitute for it."
                      : "An agent is created from a compiled strategy version you have ACCEPTED. Read the strategy in step 3 and accept it in step 4."
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
