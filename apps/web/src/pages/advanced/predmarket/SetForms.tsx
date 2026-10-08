/**
 * Issuing and redeeming complete sets on the session's own account.
 *
 * A write only ever acts on an account the principal owns: an operator who is
 * inspecting a customer's account still issues on their own account, if any,
 * and the form says which account it will act on. The number of sets is typed
 * as a whole number and sent as share base units by moving digits; the backend
 * decides everything else -- the market's state, Risk V2, the reservation of
 * the collateral -- and its answer, or its refusal with the code, is shown.
 */
import { useId, useState, type ReactNode } from "react";

import {
  useCompleteSets,
  type PredictionMarket,
  type PredictionSetResult,
  type SetCommand,
} from "../../../api/advanced/predmarket.ts";
import { Button } from "../../../components/Button.tsx";
import { Field, FieldGrid, Identifier, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { intentStatusTone, effectStatusTone } from "../../../lib/advanced.ts";
import { COMPLETE_SET_NOTE, SET_ACTIONS, parseCompleteSets, type SetAction } from "../../../lib/predmarket.ts";
import { useAdvancedAccount } from "../area.ts";
import { useConfirmation } from "./confirm.ts";
import { CommandFailure, KeyLine } from "./parts.tsx";

const ACTION_COPY: Readonly<Record<SetAction, { readonly label: string; readonly effect: string }>> = {
  ISSUE: {
    label: "Issue complete sets",
    effect:
      "Each set takes one collateral unit from the account into the market's escrow and gives the account one share of every outcome.",
  },
  REDEEM: {
    label: "Redeem complete sets",
    effect:
      "Each set returns one share of every outcome to the escrow and gives the account one collateral unit back.",
  },
};

export function CompleteSetsPanel(props: { readonly market: PredictionMarket }): ReactNode {
  const { market } = props;
  const account = useAdvancedAccount();
  const own = account.ownAccountId;
  const sets = useCompleteSets();
  const confirmation = useConfirmation<SetCommand>();
  const [action, setAction] = useState<SetAction>("ISSUE");
  const [raw, setRaw] = useState("");
  const [reviewing, setReviewing] = useState(false);
  const actionId = useId();
  const setsId = useId();
  const helpId = useId();
  const parsed = parseCompleteSets(raw, market.share_decimals);

  const unavailable =
    own === undefined
      ? "This session owns no account. Complete sets are issued and redeemed only on an account the session owns."
      : market.status !== "OPEN"
        ? `The service issues and redeems complete sets only while a market is OPEN and before its close time; this market is ${market.status}.`
        : undefined;

  const startOver = (): void => {
    confirmation.reset();
    sets.reset();
    setReviewing(false);
  };

  const submit = (): void => {
    if (own === undefined || !parsed.ok) return;
    sets.mutate(
      confirmation.confirm((key) => ({
        action,
        marketId: market.id,
        accountId: own,
        quantity: parsed.value,
        idempotencyKey: key,
      })),
    );
  };

  return (
    <Panel title="Complete sets" description={COMPLETE_SET_NOTE}>
      {own !== undefined && account.inspecting && (
        <p className="note">
          These forms act on your own account <code className="mono-small">{own}</code>, never on the account being
          inspected.
        </p>
      )}
      {sets.isSuccess ? (
        <SetOutcome result={sets.data} market={market} onAnother={startOver} />
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
          <fieldset disabled={reviewing || unavailable !== undefined}>
            <div className="form-row">
              <label htmlFor={actionId}>Action</label>
              <select
                id={actionId}
                className="input"
                value={action}
                onChange={(event) => {
                  const chosen = SET_ACTIONS.find((candidate) => candidate === event.target.value);
                  if (chosen !== undefined) setAction(chosen);
                }}
              >
                {SET_ACTIONS.map((candidate) => (
                  <option key={candidate} value={candidate}>
                    {ACTION_COPY[candidate].label}
                  </option>
                ))}
              </select>
              <p className="field-note">{ACTION_COPY[action].effect}</p>
            </div>
            <div className="form-row">
              <label htmlFor={setsId}>Complete sets</label>
              <input
                id={setsId}
                className="input"
                inputMode="numeric"
                autoComplete="off"
                value={raw}
                aria-describedby={helpId}
                {...(parsed.ok || raw === "" ? {} : { "aria-invalid": true })}
                onChange={(event) => {
                  setRaw(event.target.value);
                }}
              />
              <p id={helpId} className="field-note">
                {raw === "" ? (
                  "A whole number of sets. The backend takes share base units; the amount is sent exactly as shown."
                ) : parsed.ok ? (
                  <>
                    Will be sent as <BaseUnits value={parsed.value} />.
                  </>
                ) : (
                  parsed.error
                )}
              </p>
            </div>
          </fieldset>

          {reviewing && parsed.ok && own !== undefined && (
            <div className="callout" role="status">
              <p>
                {ACTION_COPY[action].label}: <strong>{parsed.sets}</strong> on account{" "}
                <code className="mono-small">{own}</code> in market {market.symbol}. One set is one share of each of{" "}
                {market.outcomes.map((outcome) => outcome.symbol).join(", ")}, against one collateral unit of{" "}
                {market.collateral_symbol} (<BaseUnits value={market.unit_quantity} /> per set, as the market states).
              </p>
            </div>
          )}
          <KeyLine confirmation={confirmation} />
          {/* After a failure only the failure's own choices remain. */}
          {!sets.isError && (
            <div className="form-actions">
              {unavailable !== undefined ? (
                <Button variant="primary" disabledReason={unavailable}>
                  {ACTION_COPY[action].label}
                </Button>
              ) : !parsed.ok ? (
                <Button variant="primary" disabledReason={raw === "" ? "Enter a number of complete sets." : parsed.error}>
                  Review
                </Button>
              ) : reviewing ? (
                <Button variant="primary" submit busy={sets.isPending} busyLabel="Asking the backend…">
                  Confirm: {ACTION_COPY[action].label.toLowerCase()}
                </Button>
              ) : (
                <Button variant="primary" submit>
                  Review
                </Button>
              )}
              {reviewing && !sets.isPending && (
                <Button variant="quiet" onClick={startOver}>
                  Change it
                </Button>
              )}
            </div>
          )}
          {sets.isError && (
            <CommandFailure
              error={sets.error}
              onRetry={submit}
              onStartOver={startOver}
              nothingHappened="The backend did not carry this out: no collateral and no share moved. A refused request is kept on record as a rejected economic intent."
            />
          )}
        </form>
      )}
    </Panel>
  );
}

function SetOutcome(props: {
  readonly result: PredictionSetResult;
  readonly market: PredictionMarket;
  readonly onAnother: () => void;
}): ReactNode {
  const { result, market } = props;
  return (
    <div role="status">
      {result.replay && (
        <p className="callout">
          The backend had already carried out this request; this is its first answer, and nothing happened twice.
        </p>
      )}
      <FieldGrid columns={3}>
        <Field label="Action">{result.action}</Field>
        <Field label="Shares of each outcome" note="As the backend recorded the request, in share base units.">
          <Qty value={result.quantity} decimals={market.share_decimals} /> <BaseUnits value={result.quantity} />
        </Field>
        <Field label="Economic intent">
          <Pill tone={intentStatusTone(result.intent.status)}>{result.intent.status}</Pill>
          <Identifier value={result.intent.id} />
        </Field>
        <Field label="Economic effect" note="Applied exactly once, with the journal transaction that posted it.">
          {result.effect === undefined ? (
            <span className="absent">none reported</span>
          ) : (
            <>
              <Pill tone={effectStatusTone(result.effect.status)}>{result.effect.status}</Pill>{" "}
              <code className="mono-small">{result.effect.effect_kind}</code>
            </>
          )}
        </Field>
        <Field label="Journal transaction">
          <Identifier value={result.journal_transaction_id} />
        </Field>
        <Field label="Risk Kernel V2 decision">
          <Identifier value={result.risk_decision_hash} />
        </Field>
      </FieldGrid>
      <div className="form-actions">
        <Button onClick={props.onAnother}>Another request</Button>
      </div>
    </div>
  );
}
