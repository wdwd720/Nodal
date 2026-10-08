/**
 * Defining a prediction market (operator). The definition is immutable once
 * the market exists: the backend stores its hash, refuses another definition
 * under the same symbol, and fixes the collateral to the sandbox dollar. The
 * operator who defines a market can never resolve or void it; the backend
 * enforces that, not this form.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useCreatePredictionMarket,
  usePredictionOracle,
  type CreateMarketCommand,
  type PredictionMarket,
} from "../../../api/advanced/predmarket.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Panel } from "../../../components/Layout.tsx";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import {
  DISPUTE_WINDOW_CHOICES,
  PREDICTION_MODES,
  isEd25519KeyHex,
  localInputToInstant,
  parseOutcomes,
} from "../../../lib/predmarket.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useConfirmation } from "./confirm.ts";
import { CommandFailure, KeyLine } from "./parts.tsx";
import { marketHref } from "./paths.ts";

type Mode = (typeof PREDICTION_MODES)[number];

export function CreateMarketPanel(): ReactNode {
  const oracle = usePredictionOracle(true);
  const create = useCreatePredictionMarket();
  const confirmation = useConfirmation<CreateMarketCommand>();
  const ids = {
    symbol: useId(),
    question: useId(),
    outcomes: useId(),
    mode: useId(),
    open: useId(),
    close: useId(),
    window: useId(),
    source: useId(),
    key: useId(),
  };

  const [symbol, setSymbol] = useState("");
  const [question, setQuestion] = useState("");
  const [outcomesText, setOutcomesText] = useState("YES, NO");
  const [mode, setMode] = useState<Mode>("SANDBOX");
  const [openAt, setOpenAt] = useState("");
  const [closeAt, setCloseAt] = useState("");
  const [windowMs, setWindowMs] = useState(DISPUTE_WINDOW_CHOICES[0]?.ms ?? 0);
  // Undefined: use the oracle the backend names. A typed value replaces it.
  const [sourceDraft, setSourceDraft] = useState<string | undefined>(undefined);
  const [keyDraft, setKeyDraft] = useState<string | undefined>(undefined);
  const [reviewing, setReviewing] = useState(false);

  const oracleSource = sourceDraft ?? (oracle.isSuccess ? oracle.data.source : "");
  const oracleKey = keyDraft ?? (oracle.isSuccess ? oracle.data.public_key : "");
  const outcomes = parseOutcomes(outcomesText);
  const closeInstant = localInputToInstant(closeAt);
  const openInstant = openAt === "" ? undefined : localInputToInstant(openAt);

  const problems: string[] = [];
  if (symbol.trim() === "") problems.push("Name the market with a symbol.");
  if (question.trim() === "") problems.push("State the question the market asks.");
  if (outcomes.length < 2) problems.push("A market has at least two outcomes.");
  if (closeInstant === undefined) problems.push("Choose the close time.");
  if (openAt !== "" && openInstant === undefined) problems.push("The open time is not a time.");
  if (oracleSource.trim() === "") problems.push("Name the oracle source.");
  if (!isEd25519KeyHex(oracleKey)) problems.push("The oracle key is an Ed25519 public key: 64 hex characters.");

  const startOver = (): void => {
    confirmation.reset();
    create.reset();
    setReviewing(false);
  };

  const submit = (): void => {
    if (closeInstant === undefined) return;
    create.mutate(
      confirmation.confirm((key) => ({
        idempotencyKey: key,
        body: {
          symbol: symbol.trim(),
          question: question.trim(),
          outcomes,
          mode,
          // Without an open time the market may open as soon as it is
          // defined. Fixed here, once: a retry resends this same instant.
          open_at: openInstant ?? new Date().toISOString(),
          close_at: closeInstant,
          dispute_window_ms: windowMs,
          oracle_source: oracleSource.trim(),
          oracle_public_key: oracleKey.trim(),
        },
      })),
    );
  };

  return (
    <Panel
      title="Define a market"
      description="Operator action. The market is created in DRAFT, collateralised in the sandbox dollar, and opened separately. Its definition can never change."
    >
      {oracle.isError && (
        <Explanation error={oracle.error}>
          <p>
            The backend names no oracle for this deployment, so there is no key to register here. A market can still
            name an oracle by source and key, but no report can resolve it unless that oracle exists.
          </p>
        </Explanation>
      )}
      {create.isSuccess ? (
        <Created market={create.data} onAnother={startOver} />
      ) : (
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (!reviewing) {
              setReviewing(true);
              return;
            }
            submit();
          }}
        >
          <fieldset disabled={reviewing}>
            <div className="form-row">
              <label htmlFor={ids.symbol}>Symbol</label>
              <input
                id={ids.symbol}
                className="input"
                value={symbol}
                autoComplete="off"
                spellCheck={false}
                onChange={(event) => {
                  setSymbol(event.target.value);
                }}
              />
              <p className="field-note">1 to 32 upper-case letters, digits, _ or -, starting with a letter or digit.</p>
            </div>
            <div className="form-row">
              <label htmlFor={ids.question}>Question</label>
              <textarea
                id={ids.question}
                className="input"
                rows={2}
                value={question}
                onChange={(event) => {
                  setQuestion(event.target.value);
                }}
              />
            </div>
            <div className="form-row">
              <label htmlFor={ids.outcomes}>Outcomes</label>
              <input
                id={ids.outcomes}
                className="input"
                value={outcomesText}
                autoComplete="off"
                spellCheck={false}
                onChange={(event) => {
                  setOutcomesText(event.target.value);
                }}
              />
              <p className="field-note">
                Comma-separated, 2 to 64 distinct symbols of upper-case letters, digits, _ or -. Sent as typed.
              </p>
            </div>
            <div className="form-row">
              <label htmlFor={ids.mode}>Mode</label>
              <select
                id={ids.mode}
                className="input"
                value={mode}
                onChange={(event) => {
                  const chosen = PREDICTION_MODES.find((candidate) => candidate === event.target.value);
                  if (chosen !== undefined) setMode(chosen);
                }}
              >
                {PREDICTION_MODES.map((candidate) => (
                  <option key={candidate} value={candidate}>
                    {candidate}
                  </option>
                ))}
              </select>
              <p className="field-note">{ADVANCED_MODE_DESCRIPTIONS[mode]}</p>
            </div>
            <div className="form-row">
              <label htmlFor={ids.open}>Opens at (your local time; optional)</label>
              <input
                id={ids.open}
                className="input"
                type="datetime-local"
                value={openAt}
                onChange={(event) => {
                  setOpenAt(event.target.value);
                }}
              />
              <p className="field-note">Left empty, the open time is the moment you confirm.</p>
            </div>
            <div className="form-row">
              <label htmlFor={ids.close}>Closes at (your local time)</label>
              <input
                id={ids.close}
                className="input"
                type="datetime-local"
                value={closeAt}
                onChange={(event) => {
                  setCloseAt(event.target.value);
                }}
              />
              <p className="field-note">Nothing is issued or redeemed at or after this time; it must be in the future.</p>
            </div>
            <div className="form-row">
              <label htmlFor={ids.window}>Dispute window</label>
              <select
                id={ids.window}
                className="input"
                value={String(windowMs)}
                onChange={(event) => {
                  const chosen = DISPUTE_WINDOW_CHOICES.find((choice) => String(choice.ms) === event.target.value);
                  if (chosen !== undefined) setWindowMs(chosen.ms);
                }}
              >
                {DISPUTE_WINDOW_CHOICES.map((choice) => (
                  <option key={choice.ms} value={String(choice.ms)}>
                    {choice.label}
                  </option>
                ))}
              </select>
              <p className="field-note">How long after a resolution settlement waits for a dispute.</p>
            </div>
            <div className="form-row">
              <label htmlFor={ids.source}>Oracle source</label>
              <input
                id={ids.source}
                className="input"
                value={oracleSource}
                autoComplete="off"
                spellCheck={false}
                onChange={(event) => {
                  setSourceDraft(event.target.value);
                }}
              />
            </div>
            <div className="form-row">
              <label htmlFor={ids.key}>Oracle public key (hex)</label>
              <input
                id={ids.key}
                className="input"
                value={oracleKey}
                autoComplete="off"
                spellCheck={false}
                onChange={(event) => {
                  setKeyDraft(event.target.value);
                }}
              />
              <p className="field-note">
                {oracle.isSuccess && sourceDraft === undefined && keyDraft === undefined
                  ? "Filled from the oracle the backend names. Only reports this key signed can resolve the market."
                  : "Only reports this key signed can resolve the market."}
              </p>
            </div>
          </fieldset>

          {reviewing && (
            <div className="callout" role="status">
              <p>
                Define <strong>{symbol.trim()}</strong> in {mode}, with outcomes {outcomes.join(", ")}, closing{" "}
                {closeInstant === undefined ? "at no valid time" : formatInstant(closeInstant)}. The collateral is the
                sandbox dollar. You will not be able to resolve or void this market yourself.
              </p>
            </div>
          )}
          <KeyLine confirmation={confirmation} />
          {/* After a failure only the failure's own choices remain. */}
          {!create.isError && (
            <div className="form-actions">
              {problems.length > 0 ? (
                <Button variant="primary" disabledReason={problems.join(" ")}>
                  Review the definition
                </Button>
              ) : reviewing ? (
                <Button variant="primary" submit busy={create.isPending} busyLabel="Defining the market…">
                  Confirm and define
                </Button>
              ) : (
                <Button variant="primary" submit>
                  Review the definition
                </Button>
              )}
              {reviewing && !create.isPending && (
                <Button variant="quiet" onClick={startOver}>
                  Change the definition
                </Button>
              )}
            </div>
          )}
          {create.isError && (
            <CommandFailure
              error={create.error}
              onRetry={submit}
              onStartOver={startOver}
              nothingHappened="No market was defined by this attempt."
            />
          )}
        </form>
      )}
    </Panel>
  );
}

function Created(props: { readonly market: PredictionMarket; readonly onAnother: () => void }): ReactNode {
  const { market } = props;
  return (
    <div role="status">
      <FieldGrid columns={3}>
        <Field label="Market">
          <Link to={marketHref(market.id)}>{market.symbol}</Link>
        </Field>
        <Field label="Status" note="Open it from its page at or after its open time.">
          {market.status}
        </Field>
        <Field label="Market id">
          <Identifier value={market.id} />
        </Field>
      </FieldGrid>
      <div className="form-actions">
        <Button onClick={props.onAnother}>Define another market</Button>
      </div>
    </div>
  );
}
