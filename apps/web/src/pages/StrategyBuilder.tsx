/**
 * STRATEGY BUILDER (PART 111): natural language, structured editor, compile,
 * validation, effects, capital, risk.
 *
 * The compile on this page is real. `@controlplane/strategy-sdk` is the same IR
 * the Go compiler consumes, with the same structural checks and the same
 * semantic hash, so pressing Compile genuinely parses, normalises, derives the
 * effect set from the document rather than trusting a declaration, and produces
 * the hash the server would have to reproduce. Nothing about that is a mock.
 *
 * Two things this page cannot do, and says so instead of pretending:
 *
 *   - natural-language compilation is a server stage, and the v1 contract has
 *     no endpoint for it, so the description field is a place to write the
 *     intent down rather than a button that quietly does nothing;
 *   - there is no endpoint to save, version or deploy a strategy, so the
 *     compiled document is offered as a download rather than saved to an
 *     account it cannot reach.
 */
import { useMemo, useState, type ReactNode } from "react";
import {
  decimal,
  strategy,
  type Dependency,
  type DependencyKind,
  type Direction,
} from "@controlplane/strategy-sdk";

import { useInstruments } from "../api/queries.ts";
import { AsyncPanel, EmptyState } from "../components/DataState.tsx";
import { Button, DownloadLink } from "../components/Button.tsx";
import {
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  NoEndpoint,
  Page,
  Panel,
  Pill,
  Table,
} from "../components/Layout.tsx";
import { CONFIDENCE_DISCLAIMER } from "../lib/honesty.ts";
import { parseUsdAmountInput } from "../lib/money.ts";
import {
  BPS_CHOICES,
  COUNT_CHOICES,
  DEPENDENCY_KINDS,
  DIRECTIONS,
  DURATION_CHOICES,
  INTERVAL_CHOICES,
  STRATEGY_FORM_DEFAULTS,
  renderDuration,
  type StrategyFormState,
} from "../lib/strategy-form.ts";
import {
  prepare,
  semanticHashInBrowser,
  type PreparedStrategy,
} from "../lib/strategy-compile.ts";
import { useActiveAccountId, useSession } from "../session.tsx";

interface CompileOutcome {
  readonly prepared: PreparedStrategy | undefined;
  readonly hash: string | undefined;
  readonly issues: readonly string[];
}

export function StrategyBuilder(): ReactNode {
  const session = useSession();
  const accountId = useActiveAccountId();
  const instruments = useInstruments();
  const [form, setForm] = useState<StrategyFormState>(STRATEGY_FORM_DEFAULTS);
  const [description, setDescription] = useState("");
  const [outcome, setOutcome] = useState<CompileOutcome | undefined>(undefined);

  const set = <K extends keyof StrategyFormState>(key: K, value: StrategyFormState[K]): void => {
    setForm((current) => ({ ...current, [key]: value }));
    setOutcome(undefined);
  };

  const instrumentId =
    form.instrumentId !== "" ? form.instrumentId : (instruments.data?.[0]?.id ?? "");

  const usdFields = useMemo(
    () =>
      ({
        notionalUsd: parseUsdAmountInput(form.notionalUsd),
        minAllocation: parseUsdAmountInput(form.minAllocation),
        maxSingleTrade: parseUsdAmountInput(form.maxSingleTrade),
        maxPosition: parseUsdAmountInput(form.maxPosition),
        maxDailyLoss: parseUsdAmountInput(form.maxDailyLoss),
      }) as const,
    [form],
  );

  const usdProblems = Object.entries(usdFields)
    .filter(([, parsed]) => !parsed.ok)
    .map(([name, parsed]) => `${name}: ${parsed.error}`);

  const canCompile =
    accountId !== undefined &&
    session.principal !== undefined &&
    instrumentId !== "" &&
    usdProblems.length === 0;

  const compile = (): void => {
    if (!canCompile || accountId === undefined || session.principal === undefined) return;
    try {
      const dependency: Dependency = {
        name: "price_feed",
        kind: form.dependencyKind,
        tool_code: form.toolCode,
        tool_version: 1,
        dependency_version: 1,
        params: {},
        max_age_ms: form.maxAgeMs,
        required: true,
      };
      const builder = strategy({
        strategyId: form.strategyId,
        accountId,
        userId: session.principal.subject_id,
        riskPolicyVersion: form.riskPolicyVersion,
        riskPolicyHash: form.riskPolicyHash,
        sdkVersion: "web-builder",
      })
        .instrument("target", instrumentId)
        .onInterval("tick", form.intervalMs)
        .dependency(dependency)
        .signal(
          "trend",
          {
            window: {
              fn: "RETURN",
              dependency: "price_feed",
              path: "price",
              lookback_ms: form.lookbackMs,
              scale: 4,
              rounding: "half_even",
            },
          },
          4,
        )
        .when("trend_is_strong", {
          cmp: { op: "GT", l: { signal: "trend" }, r: { const: decimal(form.thresholdLiteral) } },
        })
        .predict(
          "call_it",
          {
            instrument: "target",
            horizon_ms: form.horizonMs,
            direction: form.direction,
            probability: { const: decimal(form.probabilityLiteral) },
            expected_return_bps: { const: decimal(String(form.expectedReturnBps)) },
            downside_probability: { const: decimal(form.downsideProbabilityLiteral) },
            max_downside_bps: { const: decimal(String(form.maxDownsideBps)) },
            confidence: { const: decimal(form.confidenceLiteral) },
          },
          "trend_is_strong",
        )
        .intent(
          "act_on_it",
          {
            action: "ACQUIRE_NOTIONAL",
            instrument: "target",
            sizing: { kind: "FIXED_NOTIONAL", notional_usd: usdFields.notionalUsd.value },
            constraints: {
              max_slippage_bps: form.slippageBps,
              max_fee_bps: form.feeBps,
              max_price_impact_bps: form.impactBps,
              quote_freshness_ms: form.maxAgeMs,
              allowed_venues: [],
            },
            deadline_ms: form.deadlineMs,
            prediction: "call_it",
          },
          "trend_is_strong",
        )
        .envelope({
          min_allocation: usdFields.minAllocation.value,
          max_single_trade: usdFields.maxSingleTrade.value,
          max_position: usdFields.maxPosition.value,
          max_daily_loss: usdFields.maxDailyLoss.value,
          instruments: [instrumentId],
          asset_classes: [],
          venues: [],
          max_intents_per_hour: form.maxIntentsPerHour,
          max_runs_per_minute: form.maxRunsPerMinute,
        });

      const prepared = prepare(builder);
      if (prepared.issues.length > 0) {
        setOutcome({ prepared: undefined, hash: undefined, issues: prepared.issues });
        return;
      }
      // The digest is asynchronous in a browser, so the result lands in a
      // second render. Until it does, nothing claims to have compiled.
      setOutcome({ prepared, hash: undefined, issues: [] });
      void semanticHashInBrowser(prepared).then(
        (hash) => {
          setOutcome({ prepared, hash, issues: [] });
        },
        (error: unknown) => {
          setOutcome({
            prepared: undefined,
            hash: undefined,
            issues: [
              `the document compiled but its semantic hash could not be computed: ${
                error instanceof Error ? error.message : String(error)
              }`,
            ],
          });
        },
      );
    } catch (error) {
      setOutcome({
        prepared: undefined,
        hash: undefined,
        issues: [error instanceof Error ? error.message : String(error)],
      });
    }
  };

  return (
    <Page
      title="Strategy builder"
      lead="Write a strategy as a structured document, compile it against the same intermediate representation the platform's compiler uses, and see exactly what it would be allowed to do."
    >
      <Panel
        title="Describe it in words"
        description="Kept with the strategy. Not compiled here."
      >
        <div className="form-row">
          <label htmlFor="nl">What should this strategy do?</label>
          <textarea
            id="nl"
            className="input"
            rows={4}
            value={description}
            aria-describedby="nl-help"
            onChange={(event) => {
              setDescription(event.target.value);
            }}
          />
          <p id="nl-help" className="field-note">
            Natural-language compilation is a server stage that parses a description into the same
            document the editor below produces. The v1 contract exposes no endpoint for it, so this
            text is not sent anywhere.
          </p>
        </div>
        <Button
          disabledReason="Natural-language compilation happens on the server and the v1 API exposes no endpoint for it. Use the structured editor below, which compiles for real."
        >
          Compile from description
        </Button>
      </Panel>

      <Panel title="Structured editor" description="Every field here becomes part of the compiled document.">
        {accountId === undefined ? (
          <EmptyState
            title="No account"
            body="A strategy document names the account that owns it, and the backend returned no account for this session."
          />
        ) : (
          <AsyncPanel
            query={instruments}
            loadingLabel="Loading instruments…"
            empty={{
              isEmpty: (list) => list.length === 0,
              title: "No instruments",
              body: "A strategy must name an instrument, and the backend listed none.",
            }}
          >
            {(list) => (
              <form
                className="form"
                onSubmit={(event) => {
                  event.preventDefault();
                  compile();
                }}
              >
                <h3>Identity</h3>
                <FieldGrid columns={2}>
                  <Field label="Strategy id">
                    <input
                      className="input"
                      aria-label="Strategy id"
                      value={form.strategyId}
                      onChange={(event) => {
                        set("strategyId", event.target.value);
                      }}
                    />
                  </Field>
                  <Field label="Instrument">
                    <select
                      className="input"
                      aria-label="Instrument"
                      value={instrumentId}
                      onChange={(event) => {
                        set("instrumentId", event.target.value);
                      }}
                    >
                      {list.map((instrument) => (
                        <option key={instrument.id} value={instrument.id}>
                          {instrument.canonical_name}
                        </option>
                      ))}
                    </select>
                  </Field>
                </FieldGrid>

                <h3>Trigger and data</h3>
                <FieldGrid columns={3}>
                  <Field label="Run">
                    <select
                      className="input"
                      aria-label="Run interval"
                      value={String(form.intervalMs)}
                      onChange={(event) => {
                        const choice = INTERVAL_CHOICES.find(
                          (option) => String(option.ms) === event.target.value,
                        );
                        if (choice !== undefined) set("intervalMs", choice.ms);
                      }}
                    >
                      {INTERVAL_CHOICES.map((choice) => (
                        <option key={choice.ms} value={String(choice.ms)}>
                          {choice.label}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field label="Data dependency kind" note="Decides which read effect the document earns.">
                    <select
                      className="input"
                      aria-label="Dependency kind"
                      value={form.dependencyKind}
                      onChange={(event) => {
                        set("dependencyKind", event.target.value as DependencyKind);
                      }}
                    >
                      {DEPENDENCY_KINDS.map((kind) => (
                        <option key={kind} value={kind}>
                          {kind}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field label="Tool code">
                    <input
                      className="input"
                      aria-label="Tool code"
                      value={form.toolCode}
                      onChange={(event) => {
                        set("toolCode", event.target.value);
                      }}
                    />
                  </Field>
                  <Field label="Maximum data age" note="Older than this and the run refuses to use it.">
                    <DurationField
                      label="Maximum data age"
                      value={form.maxAgeMs}
                      onChange={(value) => {
                        set("maxAgeMs", value);
                      }}
                    />
                  </Field>
                  <Field label="Lookback for the trend signal">
                    <DurationField
                      label="Lookback"
                      value={form.lookbackMs}
                      onChange={(value) => {
                        set("lookbackMs", value);
                      }}
                    />
                  </Field>
                  <Field label="Trend threshold" note="A decimal literal, kept exact; never a float.">
                    <input
                      className="input"
                      aria-label="Trend threshold"
                      value={form.thresholdLiteral}
                      onChange={(event) => {
                        set("thresholdLiteral", event.target.value);
                      }}
                    />
                  </Field>
                </FieldGrid>

                <h3>Prediction</h3>
                <p className="field-note">{CONFIDENCE_DISCLAIMER}</p>
                <FieldGrid columns={3}>
                  <Field label="Direction">
                    <select
                      className="input"
                      aria-label="Direction"
                      value={form.direction}
                      onChange={(event) => {
                        set("direction", event.target.value as Direction);
                      }}
                    >
                      {DIRECTIONS.map((direction) => (
                        <option key={direction} value={direction}>
                          {direction}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field label="Prediction horizon">
                    <DurationField
                      label="Prediction horizon"
                      value={form.horizonMs}
                      onChange={(value) => {
                        set("horizonMs", value);
                      }}
                    />
                  </Field>
                  <Field label="Stated probability" note="The model's own number, committed before the outcome.">
                    <input
                      className="input"
                      aria-label="Stated probability"
                      value={form.probabilityLiteral}
                      onChange={(event) => {
                        set("probabilityLiteral", event.target.value);
                      }}
                    />
                  </Field>
                  <Field label="Confidence" note="A self-rating. See the note above for what it is not.">
                    <input
                      className="input"
                      aria-label="Confidence"
                      value={form.confidenceLiteral}
                      onChange={(event) => {
                        set("confidenceLiteral", event.target.value);
                      }}
                    />
                  </Field>
                  <Field label="Expected move">
                    <BpsField
                      label="Expected move"
                      value={form.expectedReturnBps}
                      onChange={(value) => {
                        set("expectedReturnBps", value);
                      }}
                    />
                  </Field>
                  <Field label="Downside probability">
                    <input
                      className="input"
                      aria-label="Downside probability"
                      value={form.downsideProbabilityLiteral}
                      onChange={(event) => {
                        set("downsideProbabilityLiteral", event.target.value);
                      }}
                    />
                  </Field>
                  <Field label="Maximum downside">
                    <BpsField
                      label="Maximum downside"
                      value={form.maxDownsideBps}
                      onChange={(value) => {
                        set("maxDownsideBps", value);
                      }}
                    />
                  </Field>
                </FieldGrid>

                <h3>Capital envelope</h3>
                <FieldGrid columns={3}>
                  <UsdField
                    label="Size of each trade"
                    value={form.notionalUsd}
                    parsed={usdFields.notionalUsd}
                    onChange={(value) => {
                      set("notionalUsd", value);
                    }}
                  />
                  <UsdField
                    label="Minimum allocation"
                    value={form.minAllocation}
                    parsed={usdFields.minAllocation}
                    onChange={(value) => {
                      set("minAllocation", value);
                    }}
                  />
                  <UsdField
                    label="Maximum single trade"
                    value={form.maxSingleTrade}
                    parsed={usdFields.maxSingleTrade}
                    onChange={(value) => {
                      set("maxSingleTrade", value);
                    }}
                  />
                  <UsdField
                    label="Maximum position"
                    value={form.maxPosition}
                    parsed={usdFields.maxPosition}
                    onChange={(value) => {
                      set("maxPosition", value);
                    }}
                  />
                  <UsdField
                    label="Maximum daily loss"
                    value={form.maxDailyLoss}
                    parsed={usdFields.maxDailyLoss}
                    onChange={(value) => {
                      set("maxDailyLoss", value);
                    }}
                  />
                  <Field label="Maximum intents per hour">
                    <CountField
                      label="Maximum intents per hour"
                      value={form.maxIntentsPerHour}
                      onChange={(value) => {
                        set("maxIntentsPerHour", value);
                      }}
                    />
                  </Field>
                  <Field label="Maximum runs per minute">
                    <CountField
                      label="Maximum runs per minute"
                      value={form.maxRunsPerMinute}
                      onChange={(value) => {
                        set("maxRunsPerMinute", value);
                      }}
                    />
                  </Field>
                </FieldGrid>

                <h3>Risk and execution constraints</h3>
                <FieldGrid columns={3}>
                  <Field label="Maximum slippage">
                    <BpsField
                      label="Maximum slippage"
                      value={form.slippageBps}
                      onChange={(value) => {
                        set("slippageBps", value);
                      }}
                    />
                  </Field>
                  <Field label="Maximum fee">
                    <BpsField
                      label="Maximum fee"
                      value={form.feeBps}
                      onChange={(value) => {
                        set("feeBps", value);
                      }}
                    />
                  </Field>
                  <Field label="Maximum price impact">
                    <BpsField
                      label="Maximum price impact"
                      value={form.impactBps}
                      onChange={(value) => {
                        set("impactBps", value);
                      }}
                    />
                  </Field>
                  <Field label="Execution deadline">
                    <DurationField
                      label="Execution deadline"
                      value={form.deadlineMs}
                      onChange={(value) => {
                        set("deadlineMs", value);
                      }}
                    />
                  </Field>
                  <Field label="Risk policy version">
                    <input
                      className="input"
                      aria-label="Risk policy version"
                      value={form.riskPolicyVersion}
                      onChange={(event) => {
                        set("riskPolicyVersion", event.target.value);
                      }}
                    />
                  </Field>
                  <Field label="Risk policy hash" note="Pins the policy the document was written against.">
                    <input
                      className="input"
                      aria-label="Risk policy hash"
                      value={form.riskPolicyHash}
                      onChange={(event) => {
                        set("riskPolicyHash", event.target.value);
                      }}
                    />
                  </Field>
                </FieldGrid>

                <div className="form-actions">
                  {canCompile ? (
                    <Button submit variant="primary">
                      Compile
                    </Button>
                  ) : (
                    <Button
                      variant="primary"
                      disabledReason={
                        usdProblems.length > 0
                          ? usdProblems.join("; ")
                          : "Choose an instrument the backend actually lists."
                      }
                    >
                      Compile
                    </Button>
                  )}
                </div>
              </form>
            )}
          </AsyncPanel>
        )}
      </Panel>

      {outcome !== undefined && <CompileResult outcome={outcome} description={description} />}

      <Panel title="Deploying this" description="What happens next, and why it cannot happen here.">
        <NoEndpoint
          what="There is no endpoint to save, version, review or deploy a strategy."
          detail="Compiling here proves the document is structurally valid and shows exactly which effects it earns. Promotion to an agent is a server-side flow with its own approvals; when an endpoint exists, this page will call it."
        />
        <Button disabledReason="The v1 API has no strategy-deployment endpoint, so there is nothing for this to call.">
          Deploy strategy
        </Button>
      </Panel>
    </Page>
  );
}

function CompileResult(props: {
  readonly outcome: CompileOutcome;
  readonly description: string;
}): ReactNode {
  const { outcome } = props;
  if (outcome.prepared === undefined) {
    return (
      <Panel title="Validation" description="The compiler refused this document.">
        <p className="banner banner-bad" role="status">
          The document did not compile. Nothing was sent anywhere.
        </p>
        <ul className="issues">
          {outcome.issues.map((issue) => (
            <li key={issue}>{issue}</li>
          ))}
        </ul>
      </Panel>
    );
  }

  const { document, semanticJson } = outcome.prepared;
  const hash = outcome.hash;
  const json = JSON.stringify(document, null, 2);
  const dataUrl = `data:application/json;charset=utf-8,${encodeURIComponent(json)}`;

  return (
    <>
      <Panel title="Validation" description="What the compiler decided.">
        <p className="banner banner-good" role="status">
          The document compiled. Its structure, its references and its sizing all passed the checks
          the platform compiler applies at the structural stage.
        </p>
        <FieldGrid columns={2}>
          <Field label="Semantic hash" note="The server must reproduce this from the same document.">
            {hash === undefined ? (
              <span className="loading">computing the digest…</span>
            ) : (
              <Identifier value={hash} />
            )}
          </Field>
          <Field label="Schema version">{String(document.schema_version)}</Field>
        </FieldGrid>
      </Panel>

      <Panel
        title="Effects this strategy earns"
        description="Derived from the document, never declared by its author."
      >
        <ul className="effects">
          {document.effects.map((effect) => (
            <li key={effect}>
              <Pill tone="info">{effect}</Pill>
            </li>
          ))}
        </ul>
        <p className="note">
          A strategy that could name its own capabilities could name one it should not have, so the
          effect set is recomputed from the dependencies and actions in the document. Signing,
          transferring value, withdrawing, changing risk or capital, and reaching the admin API are
          not expressible in this representation at all.
        </p>
      </Panel>

      <Panel title="Capital and risk as compiled" description="The envelope the document carries.">
        <Table caption="Capital envelope" headers={["Limit", "Value"]}>
          <tr>
            <th scope="row">Minimum allocation</th>
            <td className="num">{document.envelope.min_allocation}</td>
          </tr>
          <tr>
            <th scope="row">Maximum single trade</th>
            <td className="num">{document.envelope.max_single_trade}</td>
          </tr>
          <tr>
            <th scope="row">Maximum position</th>
            <td className="num">{document.envelope.max_position}</td>
          </tr>
          <tr>
            <th scope="row">Maximum daily loss</th>
            <td className="num">{document.envelope.max_daily_loss}</td>
          </tr>
          <tr>
            <th scope="row">Maximum intents per hour</th>
            <td className="num">{String(document.envelope.max_intents_per_hour)}</td>
          </tr>
          <tr>
            <th scope="row">Maximum runs per minute</th>
            <td className="num">{String(document.envelope.max_runs_per_minute)}</td>
          </tr>
          <tr>
            <th scope="row">Risk policy</th>
            <td className="mono-small">
              {document.risk_policy.version === "" ? "not pinned" : document.risk_policy.version}
              {document.risk_policy.hash === "" ? "" : ` · ${document.risk_policy.hash}`}
            </td>
          </tr>
        </Table>
        <p className="note">
          These are the amounts the document commits to. The backend enforces its own limits on top
          of them; an envelope written here can only ever be narrower than what the platform allows,
          never wider.
        </p>
      </Panel>

      <Panel title="The compiled document" description="Exactly what a server-side compile would receive.">
        <DownloadLink href={dataUrl} fileName={`${document.strategy_id}.strategy.json`}>
          Download the compiled document
        </DownloadLink>
        {props.description !== "" && (
          <p className="note">
            Your description is not part of this document. Lineage records that it came from the
            TypeScript SDK, which is what actually happened.
          </p>
        )}
        <details className="raw">
          <summary>Document JSON</summary>
          <pre>{json}</pre>
        </details>
        <details className="raw">
          <summary>Semantic projection the hash covers</summary>
          <pre>{semanticJson}</pre>
        </details>
      </Panel>

      <Disclosure title="What compiling does and does not mean">
        <p>
          A document that compiles is structurally valid and has a known effect set. It has not been
          reviewed, approved, funded or deployed, and compiling says nothing at all about whether it
          would make money.
        </p>
      </Disclosure>
    </>
  );
}

/**
 * An integer chosen from literal constants.
 *
 * These are durations, basis points and rate limits rather than money, but the
 * app still never parses a typed string into a number anywhere: the selected
 * value *is* one of the constants below, matched by its own rendering, so no
 * numeric conversion happens at all.
 */
function IntegerChoice(props: {
  readonly label: string;
  readonly value: number;
  readonly choices: readonly number[];
  readonly render: (value: number) => string;
  readonly onChange: (value: number) => void;
}): ReactNode {
  return (
    <select
      className="input"
      aria-label={props.label}
      value={String(props.value)}
      onChange={(event) => {
        const chosen = props.choices.find((choice) => String(choice) === event.target.value);
        if (chosen !== undefined) props.onChange(chosen);
      }}
    >
      {props.choices.map((choice) => (
        <option key={choice} value={String(choice)}>
          {props.render(choice)}
        </option>
      ))}
    </select>
  );
}

function DurationField(props: {
  readonly label: string;
  readonly value: number;
  readonly onChange: (value: number) => void;
}): ReactNode {
  return (
    <IntegerChoice
      label={props.label}
      value={props.value}
      choices={DURATION_CHOICES}
      render={renderDuration}
      onChange={props.onChange}
    />
  );
}

function BpsField(props: {
  readonly label: string;
  readonly value: number;
  readonly onChange: (value: number) => void;
}): ReactNode {
  return (
    <IntegerChoice
      label={props.label}
      value={props.value}
      choices={BPS_CHOICES}
      render={(bps) => `${String(bps)} bps`}
      onChange={props.onChange}
    />
  );
}

function CountField(props: {
  readonly label: string;
  readonly value: number;
  readonly onChange: (value: number) => void;
}): ReactNode {
  return (
    <IntegerChoice
      label={props.label}
      value={props.value}
      choices={COUNT_CHOICES}
      render={(count) => String(count)}
      onChange={props.onChange}
    />
  );
}

function UsdField(props: {
  readonly label: string;
  readonly value: string;
  readonly parsed: { readonly ok: boolean; readonly value: string; readonly error: string };
  readonly onChange: (value: string) => void;
}): ReactNode {
  return (
    <Field
      label={props.label}
      note={props.parsed.ok ? `compiled as ${props.parsed.value}` : props.parsed.error}
    >
      <input
        className="input"
        aria-label={props.label}
        inputMode="decimal"
        value={props.value}
        {...(props.parsed.ok ? {} : { "aria-invalid": true })}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
    </Field>
  );
}
