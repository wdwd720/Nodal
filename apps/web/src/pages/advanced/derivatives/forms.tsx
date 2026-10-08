/**
 * The commands of the derivatives section. Each one is an explicit act with
 * its own idempotency key, created when the person confirms and reused for
 * every retry of that same act, so a double click or a lost reply cannot
 * trade twice. Each form shows exactly what will be sent; the backend decides
 * whether it happens, and its refusal — recorded, with the intent it names —
 * is what the form shows when it does not.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useCloseDerivativePosition,
  useExerciseDerivativePosition,
  useOpenDerivativePosition,
  useRecordDerivativeMark,
  useRunFunding,
  useRunVariationMargin,
  useSettleDerivative,
  type DerivativeContract,
  type DerivativePolicy,
  type DerivativePosition,
  type DerivativeRunResult,
  type DerivativeTradeResult,
} from "../../../api/advanced/derivatives.ts";
import { Field, FieldGrid, Pill } from "../../../components/Layout.tsx";
import { Bps } from "../../../components/Money.tsx";
import {
  DERIVATIVE_SIDES,
  PAPER_FILL_NOTE,
  TRADE_MODES,
  formatRateE8,
  instantOf,
  latestEndedIntervalStart,
  parseContractsInput,
  parseDecimalInput,
  parseInstantInput,
  type Availability,
  type DerivativeSide,
  type TradeMode,
} from "../../../lib/derivatives.ts";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import {
  ActionKey,
  Amount,
  ChoiceField,
  CommandFailure,
  ConfirmButton,
  Price,
  TextField,
  derivativesHref,
  useActionKey,
} from "./parts.tsx";

/* --------------------------------------------------------------------------
 * Opening a position
 * ------------------------------------------------------------------------ */

export function OpenPositionForm(props: {
  readonly contract: DerivativeContract;
  readonly policy: DerivativePolicy | undefined;
}): ReactNode {
  const { contract } = props;
  const { ownAccountId, inspecting } = useAdvancedAccount();
  const ids = { side: useId(), quantity: useId(), mode: useId() };
  const [side, setSide] = useState<DerivativeSide>("LONG");
  const [quantity, setQuantity] = useState("");
  const [mode, setMode] = useState<TradeMode>("PAPER");
  const action = useActionKey();
  const open = useOpenDerivativePosition();
  const parsed = parseContractsInput(quantity);

  const changed = (): void => {
    action.reset();
    open.reset();
  };

  const blocked =
    ownAccountId === undefined
      ? "This session owns no account to trade from."
      : contract.settled
        ? "The contract has been finally settled; it no longer trades."
        : !parsed.ok
          ? quantity === ""
            ? "Enter a number of contracts."
            : parsed.error
          : undefined;

  const confirm = (): void => {
    if (blocked !== undefined || ownAccountId === undefined) return;
    open.mutate({
      accountId: ownAccountId,
      instrumentId: contract.id,
      side,
      quantity: parsed.value,
      mode,
      idempotencyKey: action.confirm(),
    });
  };

  return (
    <>
      {inspecting && (
        <p className="note">
          You are reading another account. Opening a position always acts on this session&apos;s own account, never on the
          account being inspected.
        </p>
      )}
      <div className="form">
        <ChoiceField
          id={ids.side}
          label="Side"
          value={side}
          choices={DERIVATIVE_SIDES}
          onChange={(value) => {
            setSide(value);
            changed();
          }}
        />
        <TextField
          id={ids.quantity}
          label="Contracts"
          value={quantity}
          problem={parsed.ok ? undefined : parsed.error}
          hint={`A whole number of lots of ${contract.terms.lot_size}, at least ${contract.terms.min_quantity}.`}
          onChange={(value) => {
            setQuantity(value);
            changed();
          }}
        />
        <ChoiceField
          id={ids.mode}
          label="Mode"
          value={mode}
          choices={TRADE_MODES}
          onChange={(value) => {
            setMode(value);
            changed();
          }}
        />
      </div>
      <p className="note">{ADVANCED_MODE_DESCRIPTIONS[mode]}</p>
      <FieldGrid columns={3}>
        <Field label="What will be sent">
          {side} {parsed.ok ? parsed.value : "(no quantity yet)"} of {contract.terms.symbol} in {mode}
        </Field>
        <Field label="Account">
          {ownAccountId === undefined ? <span className="absent">none</span> : <code className="mono-small">{ownAccountId}</code>}
        </Field>
        <Field label="Disclosed paper slippage" note={PAPER_FILL_NOTE}>
          {props.policy === undefined ? <span className="absent">policy not loaded</span> : <Bps value={props.policy.paper_slippage_bps} />}
        </Field>
        <ActionKey actionKey={action.key} />
      </FieldGrid>
      <div className="form-actions">
        <ConfirmButton blockedReason={blocked} busy={open.isPending} busyLabel="Opening…" onConfirm={confirm}>
          Confirm: open {side.toLowerCase()} {parsed.ok ? parsed.value : ""}
        </ConfirmButton>
      </div>
      {open.isError && (
        <CommandFailure error={open.error}>
          <p>Retrying uses the same idempotency key, so an order the backend already carried out is not carried out twice.</p>
        </CommandFailure>
      )}
      {open.isSuccess && <TradeOutcome result={open.data} verb="opened" />}
    </>
  );
}

/* --------------------------------------------------------------------------
 * Closing or exercising
 * ------------------------------------------------------------------------ */

export function ReducePositionForm(props: {
  readonly position: DerivativePosition;
  readonly verb: "close" | "exercise";
  readonly availability: Availability;
}): ReactNode {
  const { position, verb } = props;
  const { ownAccountId } = useAdvancedAccount();
  const ids = { quantity: useId(), mode: useId() };
  const [quantity, setQuantity] = useState("");
  const [mode, setMode] = useState<TradeMode>("PAPER");
  const action = useActionKey();
  const close = useCloseDerivativePosition();
  const exercise = useExerciseDerivativePosition();
  const command = verb === "close" ? close : exercise;
  const parsed = parseContractsInput(quantity);
  const all = quantity.trim() === "";

  const changed = (): void => {
    action.reset();
    command.reset();
  };

  const blocked = !props.availability.available
    ? props.availability.reason
    : ownAccountId === undefined || ownAccountId !== position.account_id
      ? "Only the account's owner acts on its positions."
      : !all && !parsed.ok
        ? parsed.error
        : undefined;

  const confirm = (): void => {
    if (blocked !== undefined || ownAccountId === undefined) return;
    command.mutate({
      accountId: ownAccountId,
      positionId: position.id,
      quantity: all ? undefined : parsed.value,
      mode,
      idempotencyKey: action.confirm(),
    });
  };

  const label = verb === "close" ? "Close" : "Exercise";
  return (
    <>
      <div className="form">
        <TextField
          id={ids.quantity}
          label={`Contracts to ${verb}`}
          value={quantity}
          problem={all || parsed.ok ? undefined : parsed.error}
          hint={`Leave empty to ${verb} every contract of the position.`}
          onChange={(value) => {
            setQuantity(value);
            changed();
          }}
        />
        <ChoiceField
          id={ids.mode}
          label="Mode"
          value={mode}
          choices={TRADE_MODES}
          onChange={(value) => {
            setMode(value);
            changed();
          }}
        />
      </div>
      <FieldGrid columns={2}>
        <Field label="What will be sent">
          {label} {all ? "every contract" : parsed.ok ? `${parsed.value} contracts` : "(no quantity yet)"} of {position.symbol} in {mode}
        </Field>
        <ActionKey actionKey={action.key} />
      </FieldGrid>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={command.isPending}
          busyLabel={verb === "close" ? "Closing…" : "Exercising…"}
          onConfirm={confirm}
          variant={verb === "close" ? "danger" : "primary"}
        >
          Confirm: {verb} {all ? "the position" : `${parsed.ok ? parsed.value : ""} contracts`}
        </ConfirmButton>
      </div>
      {command.isError && <CommandFailure error={command.error} />}
      {command.isSuccess && <TradeOutcome result={command.data} verb={verb === "close" ? "closed" : "exercised"} />}
    </>
  );
}

/** What a trade did, figure for figure as the backend answered. */
export function TradeOutcome(props: { readonly result: DerivativeTradeResult; readonly verb: string }): ReactNode {
  const { result } = props;
  const position = result.position;
  return (
    <div className="callout" role="status">
      <p>
        {result.replay
          ? "The backend had already carried out this idempotency key; this is its record, and nothing new happened."
          : `The backend ${props.verb} it.`}{" "}
        Intent{" "}
        <Link to={advancedHref("activity", "intents", result.intent.id)}>
          <code className="mono-small">{result.intent.id}</code>
        </Link>{" "}
        is <Pill tone="good">{result.intent.status}</Pill>
      </p>
      {result.fill !== undefined && position !== undefined && (
        <FieldGrid columns={3}>
          <Field label="Fill price">
            <Price mantissa={result.fill.price_mantissa} scale={result.fill.price_scale} />
            <span className="cell-note">
              {result.fill.side} via {result.fill.source}, slippage {String(result.fill.slippage_bps)} bps
            </span>
          </Field>
          <Field label="Fill value">
            <Amount value={result.fill.value} asset={position.settlement_asset} />
          </Field>
          <Field label="Realized by this fill">
            <Amount value={result.fill.realized_pnl} asset={position.settlement_asset} />
          </Field>
          <Field label="Settled with the clearing house" note="Received (positive) or paid (negative) for this fill.">
            <Amount value={result.fill.amount_settled} asset={position.settlement_asset} />
          </Field>
          <Field label="Position now">
            <Link to={derivativesHref("positions", position.id)}>
              {position.status} · {position.quantity} contracts
            </Link>
          </Field>
          <Field label="Margin posted now">
            <Amount value={position.margin_posted} asset={position.settlement_asset} />
          </Field>
        </FieldGrid>
      )}
    </div>
  );
}

/* --------------------------------------------------------------------------
 * The operator's commands
 * ------------------------------------------------------------------------ */

export function RecordMarkForm(props: { readonly contract: DerivativeContract }): ReactNode {
  const { contract } = props;
  const ids = { mark: useId(), index: useId(), source: useId() };
  const [mark, setMark] = useState("");
  const [index, setIndex] = useState("");
  const [source, setSource] = useState("sandbox-operator");
  const action = useActionKey();
  const record = useRecordDerivativeMark();
  const parsedMark = parseDecimalInput(mark);
  const parsedIndex = parseDecimalInput(index);
  const withIndex = index.trim() !== "";

  const changed = (): void => {
    action.reset();
    record.reset();
  };
  const blocked = !parsedMark.ok
    ? mark === ""
      ? "Enter the mark."
      : parsedMark.error
    : withIndex && !parsedIndex.ok
      ? parsedIndex.error
      : source.trim() === ""
        ? "Name the source of the mark."
        : undefined;

  return (
    <>
      <div className="form">
        <TextField
          id={ids.mark}
          label="Mark"
          value={mark}
          problem={parsedMark.ok ? undefined : parsedMark.error}
          hint={`Exact at price scale ${String(contract.terms.price_scale)} and on the tick grid; it is never rounded.`}
          onChange={(value) => {
            setMark(value);
            changed();
          }}
        />
        <TextField
          id={ids.index}
          label="Index (optional)"
          value={index}
          problem={!withIndex || parsedIndex.ok ? undefined : parsedIndex.error}
          hint="Without it the backend derives the index from a fresh spot price, or records none."
          onChange={(value) => {
            setIndex(value);
            changed();
          }}
        />
        <TextField
          id={ids.source}
          label="Source"
          value={source}
          onChange={(value) => {
            setSource(value);
            changed();
          }}
        />
      </div>
      <FieldGrid columns={2}>
        <Field label="Observed" note="The backend records the mark as observed at the moment it receives it.">
          now
        </Field>
        <ActionKey actionKey={action.key} />
      </FieldGrid>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={record.isPending}
          busyLabel="Recording…"
          onConfirm={() => {
            if (blocked !== undefined) return;
            record.mutate({
              instrumentId: contract.id,
              mark: parsedMark.value,
              index: withIndex ? parsedIndex.value : undefined,
              source: source.trim(),
              idempotencyKey: action.confirm(),
            });
          }}
        >
          Confirm: record the mark
        </ConfirmButton>
      </div>
      {record.isError && <CommandFailure error={record.error} />}
      {record.isSuccess && (
        <p className="callout" role="status">
          Recorded mark <code className="mono-small">{record.data.id}</code> at{" "}
          <Price mantissa={record.data.mark_mantissa} scale={record.data.price_scale} />, observed{" "}
          {formatInstant(record.data.observed_at)}.
        </p>
      )}
    </>
  );
}

export function RunForm(props: {
  readonly contract: DerivativeContract;
  readonly kind: "VARIATION_MARGIN" | "FUNDING";
  readonly intervalMs: number | undefined;
}): ReactNode {
  const { contract, kind } = props;
  const inputId = useId();
  // Until the operator types, the field offers the latest interval that has
  // ended — worked out once the interval length is known, which for variation
  // margin arrives with the policy. Confirming pins whatever is shown, so a
  // retry under the same key always sends the same request.
  const suggested =
    props.intervalMs === undefined ? undefined : latestEndedIntervalStart(new Date().getTime(), props.intervalMs);
  const [typed, setTyped] = useState<string | undefined>(undefined);
  const start = typed ?? (suggested === undefined ? "" : instantOf(suggested));
  const setStart = (value: string): void => {
    setTyped(value);
  };
  const action = useActionKey();
  const vm = useRunVariationMargin();
  const funding = useRunFunding();
  const run = kind === "VARIATION_MARGIN" ? vm : funding;
  const parsed = parseInstantInput(start);
  const blocked = parsed.ok ? undefined : start === "" ? "Enter the interval's start." : parsed.error;
  const what = kind === "VARIATION_MARGIN" ? "variation margin" : "funding";

  return (
    <>
      <div className="form">
        <TextField
          id={inputId}
          label="Interval start (UTC)"
          value={start}
          problem={parsed.ok ? undefined : parsed.error}
          hint="On the interval grid. An interval is settled only after it ends, and only once."
          onChange={(value) => {
            setStart(value);
            action.reset();
            run.reset();
          }}
        />
        <ActionKey actionKey={action.key} />
      </div>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={run.isPending}
          busyLabel="Running…"
          onConfirm={() => {
            if (!parsed.ok) return;
            setTyped(start);
            run.mutate({ instrumentId: contract.id, intervalStart: parsed.value, idempotencyKey: action.confirm() });
          }}
        >
          Confirm: run {what}
        </ConfirmButton>
      </div>
      {run.isError && <CommandFailure error={run.error} />}
      {run.isSuccess && <RunOutcome result={run.data} />}
    </>
  );
}

function RunOutcome(props: { readonly result: DerivativeRunResult }): ReactNode {
  const { result } = props;
  const asset = result.settlement_asset;
  return (
    <div className="callout" role="status">
      <p>
        {result.replay
          ? "This interval had already been run. Nothing was written now; its flows are in the register."
          : "The run is posted."}{" "}
        Interval {exactInstant(result.interval_start)} to {exactInstant(result.interval_end)}.
      </p>
      <FieldGrid columns={3}>
        <Field label="Positions">
          {result.positions === undefined ? <span className="absent">not reported for a replay</span> : String(result.positions)}
        </Field>
        <Field label="Collected from payers">
          {result.paid === undefined ? <span className="absent">not reported for a replay</span> : <Amount value={result.paid} asset={asset} />}
        </Field>
        <Field label="Paid to receivers">
          {result.received === undefined ? (
            <span className="absent">not reported for a replay</span>
          ) : (
            <Amount value={result.received} asset={asset} />
          )}
        </Field>
        <Field label="Shortfall absorbed by the clearing house">
          {result.shortfall === undefined ? (
            <span className="absent">not reported for a replay</span>
          ) : (
            <Amount value={result.shortfall} asset={asset} />
          )}
        </Field>
        {result.rate_e8 !== undefined && (
          <Field label="Funding rate" note="Per interval; longs pay a positive rate.">
            {formatRateE8(result.rate_e8)}
            {result.clamped === true && <Pill tone="warn">clamped to the policy maximum</Pill>}
          </Field>
        )}
        {result.per_contract_amount !== undefined && (
          <Field label="Per contract">
            <Amount value={result.per_contract_amount} asset={asset} />
          </Field>
        )}
      </FieldGrid>
    </div>
  );
}

export function SettleForm(props: { readonly contract: DerivativeContract }): ReactNode {
  const { contract } = props;
  const ids = { price: useId(), source: useId() };
  const [price, setPrice] = useState("");
  const [source, setSource] = useState("");
  const action = useActionKey();
  const settle = useSettleDerivative();
  const given = price.trim() !== "";
  const parsed = parseDecimalInput(price);
  const blocked =
    given && !parsed.ok
      ? parsed.error
      : given && source.trim() === ""
        ? "A given settlement price names its source."
        : contract.settled
          ? "The contract has already been settled."
          : undefined;
  const changed = (): void => {
    action.reset();
    settle.reset();
  };

  return (
    <>
      <div className="form">
        <TextField
          id={ids.price}
          label="Settlement price (optional)"
          value={price}
          problem={!given || parsed.ok ? undefined : parsed.error}
          hint="Without it the backend settles at the last fresh mark at or before expiry (an option at that mark's index)."
          onChange={(value) => {
            setPrice(value);
            changed();
          }}
        />
        <TextField
          id={ids.source}
          label="Price source"
          value={source}
          onChange={(value) => {
            setSource(value);
            changed();
          }}
        />
        <ActionKey actionKey={action.key} />
      </div>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={settle.isPending}
          busyLabel="Settling…"
          variant="danger"
          onConfirm={() => {
            if (blocked !== undefined) return;
            settle.mutate({
              instrumentId: contract.id,
              price: given ? parsed.value : undefined,
              priceSource: given ? source.trim() : undefined,
              idempotencyKey: action.confirm(),
            });
          }}
        >
          Confirm: settle the contract
        </ConfirmButton>
      </div>
      {settle.isError && <CommandFailure error={settle.error} />}
      {settle.isSuccess && (
        <div className="callout" role="status">
          <p>
            {settle.data.replay ? "The contract had already been settled; this is that settlement." : "Settled."}{" "}
            {settle.data.kind} settlement at <Price mantissa={settle.data.price_mantissa} scale={settle.data.price_scale} />{" "}
            covering {String(settle.data.positions)} positions.
          </p>
        </div>
      )}
    </>
  );
}
