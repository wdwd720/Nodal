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
  useDecideSettlementOverride,
  useExerciseDerivativePosition,
  useOpenDerivativePosition,
  useProposeSettlementOverride,
  useRecordDerivativeMark,
  useRunFunding,
  useRunVariationMargin,
  useSettleDerivative,
  type DerivativeContract,
  type DerivativePolicy,
  type DerivativePosition,
  type DerivativeRunResult,
  type DerivativeSettlementOverride,
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
  mantissaDecimal,
  parseContractsInput,
  parseDecimalInput,
  parseInstantInput,
  parseReasonInput,
  parseSha256Input,
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

/**
 * Settles an expired contract by the settlement rule. The operator does not
 * type a price: the form confirms the price and source the backend's preview
 * states, and the backend refuses the settlement if its rule now says
 * anything else.
 */
export function SettleForm(props: { readonly contract: DerivativeContract }): ReactNode {
  const { contract } = props;
  const preview = contract.settlement_preview;
  const action = useActionKey();
  const settle = useSettleDerivative();
  const confirmed =
    preview?.reference_price_mantissa === undefined ? undefined : mantissaDecimal(preview.reference_price_mantissa, preview.price_scale);
  const blocked = contract.settled
    ? "The contract has already been settled."
    : preview === undefined
      ? "The backend stated no settlement preview for this contract."
      : !preview.expired
        ? "The contract has not expired."
        : !preview.has_reference
          ? "No mark was observed and recorded by expiry, so the rule has no price."
          : !preview.settleable
            ? "The last mark at expiry is stale, so the rule does not settle it. An override is the way."
            : confirmed === undefined
              ? "The preview's price could not be read."
              : undefined;

  return (
    <>
      <FieldGrid columns={3}>
        <Field label="The rule's price" note="Confirmed as sent; never typed.">
          {preview?.reference_price_mantissa === undefined ? (
            <span className="absent">none</span>
          ) : (
            <Price mantissa={preview.reference_price_mantissa} scale={preview.price_scale} />
          )}
        </Field>
        <Field label="Its source">
          {preview?.price_source === undefined ? <span className="absent">none</span> : <code className="mono-small">{preview.price_source}</code>}
        </Field>
        <ActionKey actionKey={action.key} />
      </FieldGrid>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={settle.isPending}
          busyLabel="Settling…"
          variant="danger"
          onConfirm={() => {
            if (blocked !== undefined || confirmed === undefined) return;
            settle.mutate({
              instrumentId: contract.id,
              price: confirmed,
              priceSource: preview?.price_source,
              idempotencyKey: action.confirm(),
            });
          }}
        >
          Confirm: settle by the rule
        </ConfirmButton>
      </div>
      {settle.isError && <CommandFailure error={settle.error} />}
      {settle.isSuccess && settle.data.settlement !== undefined && (
        <div className="callout" role="status">
          <p>
            {settle.data.settlement.replay ? "The contract had already been settled; this is that settlement." : "Settled."}{" "}
            {settle.data.settlement.kind} settlement at{" "}
            <Price mantissa={settle.data.settlement.price_mantissa} scale={settle.data.settlement.price_scale} /> covering{" "}
            {String(settle.data.settlement.positions)} positions.
          </p>
        </div>
      )}
    </>
  );
}

/**
 * Proposes, as this operator, a settlement price for an expired contract the
 * rule cannot settle, within the bound the preview states, with a reason and
 * evidence. A proposal settles nothing: a second operator decides.
 */
export function ProposeOverrideForm(props: { readonly contract: DerivativeContract }): ReactNode {
  const { contract } = props;
  const preview = contract.settlement_preview;
  const ids = { price: useId(), reason: useId(), ref: useId(), digest: useId() };
  const [price, setPrice] = useState("");
  const [reason, setReason] = useState("");
  const [evidenceRef, setEvidenceRef] = useState("");
  const [digest, setDigest] = useState("");
  const action = useActionKey();
  const propose = useProposeSettlementOverride();
  const parsedPrice = parseDecimalInput(price);
  const parsedReason = parseReasonInput(reason, 1000);
  const parsedRef = parseReasonInput(evidenceRef, 500);
  const parsedDigest = parseSha256Input(digest);
  const blocked = contract.settled
    ? "The contract has already been settled."
    : preview === undefined || !preview.expired
      ? "Only an expired contract's settlement is overridden."
      : !preview.has_reference
        ? "No mark was observed and recorded by expiry, so there is no reference to bound an override by."
        : !parsedPrice.ok
          ? price === "" ? "Enter the proposed price." : parsedPrice.error
          : !parsedReason.ok
            ? parsedReason.error
            : !parsedRef.ok
              ? "Name the evidence the proposal rests on."
              : !parsedDigest.ok
                ? parsedDigest.error
                : undefined;
  const changed = (): void => {
    action.reset();
    propose.reset();
  };

  return (
    <>
      {preview?.override_min_price_mantissa !== undefined && preview.override_max_price_mantissa !== undefined && (
        <p className="note">
          The backend accepts a price on the tick grid from{" "}
          <Price mantissa={preview.override_min_price_mantissa} scale={preview.price_scale} /> to{" "}
          <Price mantissa={preview.override_max_price_mantissa} scale={preview.price_scale} />: within{" "}
          <Bps value={preview.max_deviation_bps} /> of the rule&apos;s reference price.
        </p>
      )}
      <div className="form">
        <TextField
          id={ids.price}
          label="Proposed settlement price"
          value={price}
          problem={parsedPrice.ok ? undefined : parsedPrice.error}
          hint="Exact at the contract's price scale and on its tick grid; never rounded."
          onChange={(value) => {
            setPrice(value);
            changed();
          }}
        />
        <TextField
          id={ids.reason}
          label="Reason"
          value={reason}
          problem={parsedReason.ok ? undefined : parsedReason.error}
          onChange={(value) => {
            setReason(value);
            changed();
          }}
        />
        <TextField
          id={ids.ref}
          label="Evidence reference"
          value={evidenceRef}
          hint="Where the document the proposal rests on is kept."
          onChange={(value) => {
            setEvidenceRef(value);
            changed();
          }}
        />
        <TextField
          id={ids.digest}
          label="Evidence SHA-256"
          value={digest}
          problem={parsedDigest.ok ? undefined : parsedDigest.error}
          onChange={(value) => {
            setDigest(value);
            changed();
          }}
        />
        <ActionKey actionKey={action.key} />
      </div>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={propose.isPending}
          busyLabel="Proposing…"
          onConfirm={() => {
            if (blocked !== undefined) return;
            propose.mutate({
              instrumentId: contract.id,
              price: parsedPrice.value,
              reason: parsedReason.value,
              evidenceRef: parsedRef.value,
              evidenceSha256: parsedDigest.value,
              idempotencyKey: action.confirm(),
            });
          }}
        >
          Confirm: propose the override
        </ConfirmButton>
      </div>
      {propose.isError && <CommandFailure error={propose.error} />}
      {propose.isSuccess && propose.data.override !== undefined && (
        <p className="callout" role="status">
          Proposed <code className="mono-small">{propose.data.override.id}</code> at{" "}
          <Price mantissa={propose.data.override.price_mantissa} scale={propose.data.override.price_scale} />. Nothing has
          settled: a second operator approves or rejects it.
        </p>
      )}
    </>
  );
}

/**
 * A second operator's decision on one proposal: approving it settles the
 * contract at the proposal's price (confirmed, never typed); rejecting it
 * settles nothing. The backend refuses an approval by the proposer.
 */
export function DecideOverrideForm(props: {
  readonly contract: DerivativeContract;
  readonly override: DerivativeSettlementOverride;
}): ReactNode {
  const { contract, override } = props;
  const ids = { reason: useId(), decision: useId() };
  const [decision, setDecision] = useState<"APPROVE_OVERRIDE" | "REJECT_OVERRIDE">("APPROVE_OVERRIDE");
  const [reason, setReason] = useState("");
  const action = useActionKey();
  const decide = useDecideSettlementOverride();
  const parsedReason = parseReasonInput(reason, 1000);
  const confirmed = mantissaDecimal(override.price_mantissa, override.price_scale);
  const blocked =
    override.status !== "PROPOSED"
      ? "This proposal has been decided."
      : contract.settled
        ? "The contract has already been settled."
        : !parsedReason.ok
          ? parsedReason.error
          : undefined;

  return (
    <>
      <div className="form">
        <ChoiceField
          id={ids.decision}
          label="Decision"
          value={decision}
          choices={["APPROVE_OVERRIDE", "REJECT_OVERRIDE"] as const}
          describe={(value) => (value === "APPROVE_OVERRIDE" ? "Approve, and settle at its price" : "Reject")}
          onChange={(value) => {
            setDecision(value);
            action.reset();
            decide.reset();
          }}
        />
        <TextField
          id={ids.reason}
          label="Reason"
          value={reason}
          problem={parsedReason.ok ? undefined : parsedReason.error}
          onChange={(value) => {
            setReason(value);
            action.reset();
            decide.reset();
          }}
        />
        <ActionKey actionKey={action.key} />
      </div>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={decide.isPending}
          busyLabel="Deciding…"
          variant={decision === "APPROVE_OVERRIDE" ? "danger" : "primary"}
          onConfirm={() => {
            if (blocked !== undefined) return;
            decide.mutate({
              instrumentId: contract.id,
              overrideId: override.id,
              decision,
              reason: parsedReason.value,
              price: decision === "APPROVE_OVERRIDE" ? confirmed : undefined,
              idempotencyKey: action.confirm(),
            });
          }}
        >
          {decision === "APPROVE_OVERRIDE" ? "Confirm: approve and settle" : "Confirm: reject"}
        </ConfirmButton>
      </div>
      {decide.isError && <CommandFailure error={decide.error} />}
      {decide.isSuccess && (
        <p className="callout" role="status">
          {decide.data.settlement === undefined ? (
            "Rejected. Nothing settled."
          ) : (
            <>
              Approved: the contract settled at{" "}
              <Price mantissa={decide.data.settlement.price_mantissa} scale={decide.data.settlement.price_scale} /> covering{" "}
              {String(decide.data.settlement.positions)} positions.
            </>
          )}
        </p>
      )}
    </>
  );
}
