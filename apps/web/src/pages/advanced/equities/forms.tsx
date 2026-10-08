/**
 * The commands of the equities section. Each one is an explicit act with its
 * own idempotency key, created when the person confirms and reused — with
 * the very same request — for every retry, so a double click or a lost reply
 * cannot buy, sell, cancel, list, announce, apply or run twice. Each form
 * shows what will be sent; the backend decides whether it happens, and its
 * refusal is what the form shows when it does not.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useAddHoliday,
  useAnnounceCorporateAction,
  useApplyCorporateAction,
  useCancelCorporateAction,
  useCancelEquityOrder,
  useListEquity,
  useRunSettlements,
  useRunVenue,
  useSubmitEquityOrder,
  type ActionCommand,
  type AddHolidayCommand,
  type AnnounceActionCommand,
  type CancelEquityOrderCommand,
  type EquityCorporateAction,
  type EquityFill,
  type EquityListing,
  type EquityOrder,
  type EquitySessionState,
  type ListEquityCommand,
  type RunCommand,
  type SubmitEquityOrderCommand,
} from "../../../api/advanced/equities.ts";
import { Button } from "../../../components/Button.tsx";
import { Disclosure, Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps, DecimalValue } from "../../../components/Money.tsx";
import { isCanonicalUuid } from "../../../lib/advanced.ts";
import {
  ACTION_TYPE_LABEL,
  EQUITY_ACTION_TYPES,
  EQUITY_KINDS,
  EQUITY_MODES,
  EQUITY_ORDER_TYPES,
  EQUITY_RISK_CLASSES,
  EQUITY_ROUNDINGS,
  EQUITY_SIDES,
  PAPER_VENUE_NOTE,
  RISK_CLASS_LABEL,
  ROUNDING_LABEL,
  SETTLEMENT_STATE_MEANING,
  T_PLUS_ONE_NOTE,
  isLive,
  parseBaseUnitsInput,
  parseDateInput,
  parsePriceInput,
  parseSymbolInput,
  parseWholeNumber,
  reasonProblem,
  sessionNote,
  type EquityActionType,
  type EquityKind,
  type EquityMode,
  type EquityOrderType,
  type EquityRiskClass,
  type EquityRounding,
  type EquitySide,
} from "../../../lib/equities.ts";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import { parseQuantityInput } from "../../../lib/money.ts";
import { useAdvancedAccount } from "../area.ts";
import { ScrollTable } from "../shared.tsx";
import {
  CheckField,
  ChoiceField,
  CommandControls,
  EquityCommandFailure,
  KeyLine,
  Recorded,
  StatusPill,
  TextField,
  Units,
  equitiesHref,
  equityOrderTone,
  renderEquityFailure,
  settlementTone,
  useConfirmation,
} from "./parts.tsx";

/* --------------------------------------------------------------------------
 * A fill, charge by charge
 * ------------------------------------------------------------------------ */

/** A fill with every disclosed charge, in its settlement asset, and its settlement. */
export function FillFields(props: { readonly fill: EquityFill; readonly shareSymbol: string | undefined }): ReactNode {
  const { fill } = props;
  const amount = (value: string): ReactNode => (
    <Units value={value} decimals={fill.settlement_decimals} symbol={fill.settlement_symbol} />
  );
  return (
    <FieldGrid columns={3}>
      <Field label="Filled" note={`At the venue ${fill.venue}, fill ${fill.venue_fill_id}.`}>
        {fill.side} <Units value={fill.quantity} decimals={fill.share_decimals} symbol={props.shareSymbol} />
      </Field>
      <Field label="Price" note="The mark moved against the order by the disclosed slippage, on the tick.">
        <DecimalValue value={fill.price} /> <span className="cell-note">{fill.settlement_symbol} per share</span>
      </Field>
      <Field label="Mark it filled against">
        <DecimalValue value={fill.mark.price} />
        <span className="cell-note">
          {fill.mark.source} · slippage <Bps value={fill.slippage_bps} />
        </span>
      </Field>
      <Field label="Notional">{amount(fill.notional)}</Field>
      <Field label="Spread" note="Charged by the disclosed schedule.">
        {amount(fill.spread)}
      </Field>
      <Field label="Platform fees">{amount(fill.platform_fees)}</Field>
      <Field label="Venue fees" note="Passed through from the venue; the paper venue charges none.">
        {amount(fill.venue_fees)}
      </Field>
      <Field
        label={fill.side === "BUY" ? "Total paid" : "Total received"}
        note={fill.side === "BUY" ? "The notional and every charge above." : "The notional less every charge above."}
        emphasis
      >
        {amount(fill.total)}
      </Field>
      <Field label="Fee schedule" note={`Disclosure hash ${fill.fee_disclosure_hash}.`}>
        {fill.fee_schedule_version}
        <span className="cell-note mono-small">{fill.fee_schedule_hash}</span>
      </Field>
      <Field label="Trade date">{fill.trade_date}</Field>
      <Field label="Settlement date" note={T_PLUS_ONE_NOTE}>
        {fill.settlement_date}
      </Field>
      <Field label="Settlement">
        {fill.settlement === undefined ? (
          <span className="absent">not recorded</span>
        ) : (
          <>
            <StatusPill status={fill.settlement.state} tone={settlementTone(fill.settlement.state)} />
            <span className="cell-note">{SETTLEMENT_STATE_MEANING[fill.settlement.state]}</span>
          </>
        )}
      </Field>
      <Field label="Executed">
        <Recorded at={fill.executed_at} />
      </Field>
      <Field label="Ledger transaction">
        <Identifier value={fill.journal_transaction_id} />
      </Field>
    </FieldGrid>
  );
}

/* --------------------------------------------------------------------------
 * Placing an order
 * ------------------------------------------------------------------------ */

export function SubmitEquityOrderForm(props: {
  readonly listing: EquityListing;
  readonly session: EquitySessionState | undefined;
}): ReactNode {
  const { listing } = props;
  const { ownAccountId, inspecting } = useAdvancedAccount();
  const ids = { side: useId(), type: useId(), quantity: useId(), limit: useId(), extended: useId(), mode: useId() };
  const [side, setSide] = useState<EquitySide>("BUY");
  const [orderType, setOrderType] = useState<EquityOrderType>("MARKET");
  const [quantity, setQuantity] = useState("");
  const [limit, setLimit] = useState("");
  const [extendedHours, setExtendedHours] = useState(false);
  const [mode, setMode] = useState<EquityMode>("PAPER");
  const confirmation = useConfirmation<SubmitEquityOrderCommand>();
  const submit = useSubmitEquityOrder();
  const parsedQuantity = parseQuantityInput(quantity, listing.share_decimals);
  const parsedLimit = parsePriceInput(limit, "a limit price");
  const note = props.session === undefined ? undefined : sessionNote(props.session.session, orderType, extendedHours);
  const problem =
    ownAccountId === undefined
      ? "This session owns no account to trade from."
      : !parsedQuantity.ok
        ? parsedQuantity.error
        : orderType === "LIMIT" && !parsedLimit.ok
          ? parsedLimit.error
          : undefined;
  const startOver = (): void => {
    confirmation.reset();
    submit.reset();
  };
  function changed<T>(set: (value: T) => void): (value: T) => void {
    return (value: T) => {
      set(value);
      startOver();
    };
  }
  return (
    <>
      {inspecting && (
        <p className="note">
          You are reading another account. An order always acts on this session&apos;s own account, never on the account being
          inspected.
        </p>
      )}
      <div className="form">
        <ChoiceField id={ids.side} label="Side" value={side} choices={EQUITY_SIDES} onChange={changed(setSide)} />
        <ChoiceField id={ids.type} label="Order type" value={orderType} choices={EQUITY_ORDER_TYPES} onChange={changed(setOrderType)} />
        <TextField
          id={ids.quantity}
          label={`Shares of ${listing.symbol}`}
          value={quantity}
          problem={parsedQuantity.ok ? undefined : parsedQuantity.error}
          hint={`Fractional shares to ${String(listing.share_decimals)} decimal places, in whole lots; at least the listing's minimum.`}
          onChange={changed(setQuantity)}
        />
        {orderType === "LIMIT" && (
          <TextField
            id={ids.limit}
            label={`Limit price (${listing.settlement_symbol} per share)`}
            value={limit}
            problem={parsedLimit.ok ? undefined : parsedLimit.error}
            hint={`Exact at ${String(listing.price_scale)} decimal places, on the tick of ${listing.tick_price}; never rounded.`}
            onChange={changed(setLimit)}
          />
        )}
        {orderType === "LIMIT" && (
          <CheckField
            id={ids.extended}
            label="Extended hours"
            checked={extendedHours}
            hint="Lets a limit order trade in pre-market and after hours too; it then expires at 20:00 New York time instead of 16:00."
            onChange={changed(setExtendedHours)}
          />
        )}
        <ChoiceField id={ids.mode} label="Mode" value={mode} choices={EQUITY_MODES} onChange={changed(setMode)} />
      </div>
      <p className="note">{ADVANCED_MODE_DESCRIPTIONS[mode]}</p>
      <p className="note">{PAPER_VENUE_NOTE}</p>
      {note !== undefined && (
        <p className="note" role="status">
          {note}
        </p>
      )}
      <FieldGrid columns={3}>
        <Field label="What will be sent">
          {side} {orderType}
          {parsedQuantity.ok ? (
            <>
              {" "}
              · <Units value={parsedQuantity.value} decimals={listing.share_decimals} symbol={listing.symbol} />{" "}
              <BaseUnits value={parsedQuantity.value} />
            </>
          ) : (
            " · no quantity yet"
          )}
          {orderType === "LIMIT" && (parsedLimit.ok ? ` · limit ${parsedLimit.value}` : " · no limit yet")}
          {extendedHours && orderType === "LIMIT" ? " · extended hours" : ""} · {mode}
        </Field>
        <Field label="Account">
          {ownAccountId === undefined ? <span className="absent">none</span> : <code className="mono-small">{ownAccountId}</code>}
        </Field>
        <Field label="Settles in" note={T_PLUS_ONE_NOTE}>
          {listing.settlement_symbol}
        </Field>
      </FieldGrid>
      <CommandControls
        confirmLabel={`Confirm: ${side.toLowerCase()} ${listing.symbol}`}
        busyLabel="Sending…"
        problem={problem}
        mutation={submit}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Start a new order"
        renderFailure={renderEquityFailure}
        build={(key) => ({
          accountId: ownAccountId ?? "",
          instrumentId: listing.instrument_id,
          side,
          orderType,
          quantity: parsedQuantity.value,
          limitPrice: orderType === "LIMIT" ? parsedLimit.value : undefined,
          extendedHours: orderType === "LIMIT" && extendedHours,
          mode,
          idempotencyKey: key,
        })}
        renderResult={(result) => (
          <div className="callout" role="status">
            <p>
              Order{" "}
              <Link to={equitiesHref("orders", result.order.id)}>
                <code className="mono-small">{result.order.id}</code>
              </Link>{" "}
              is <StatusPill status={result.order.status} tone={equityOrderTone(result.order.status)} />.{" "}
              {result.placement.status === "PLACED" ? (
                <>
                  Placed at the venue, which answered{" "}
                  <Pill tone="info">{result.placement.venue_status ?? "nothing yet"}</Pill>.
                </>
              ) : (
                <>
                  <Pill tone="warn">NOT PLACED</Pill> {result.placement.detail}
                  {result.placement.error_code !== undefined && (
                    <span className="mono-small"> (code {result.placement.error_code})</span>
                  )}
                </>
              )}
            </p>
            {result.placement.venue_reason !== undefined && <p className="note">The venue said: {result.placement.venue_reason}</p>}
            {result.order.fill !== undefined && <FillFields fill={result.order.fill} shareSymbol={listing.symbol} />}
          </div>
        )}
      />
    </>
  );
}

/* --------------------------------------------------------------------------
 * Cancelling an order
 * ------------------------------------------------------------------------ */

/** Cancelling one order: asks once more, then sends with the key created at that confirmation. */
export function CancelEquityOrderAction(props: { readonly order: EquityOrder }): ReactNode {
  const { order } = props;
  const { ownAccountId } = useAdvancedAccount();
  const [asking, setAsking] = useState(false);
  const confirmation = useConfirmation<CancelEquityOrderCommand>();
  const cancel = useCancelEquityOrder();
  const owner = ownAccountId !== undefined && ownAccountId.toLowerCase() === order.account_id.toLowerCase();
  const startOver = (): void => {
    confirmation.reset();
    cancel.reset();
    setAsking(false);
  };
  if (cancel.isSuccess) {
    return (
      <span role="status">
        <StatusPill status={cancel.data.status} tone={equityOrderTone(cancel.data.status)} />
        <span className="cell-note mono-small">key {confirmation.key ?? ""}</span>
      </span>
    );
  }
  if (cancel.isError) {
    return (
      <EquityCommandFailure
        error={cancel.error}
        onRetry={() => {
          if (confirmation.command !== undefined) cancel.mutate(confirmation.command);
        }}
        onStartOver={startOver}
      />
    );
  }
  if (!isLive(order.status)) return <span className="absent">{order.status}: nothing left to cancel</span>;
  if (!owner) return <span className="absent">Only the account&apos;s owner cancels its orders.</span>;
  if (!asking) {
    return (
      <Button
        variant="danger"
        onClick={() => {
          setAsking(true);
        }}
      >
        Cancel…
      </Button>
    );
  }
  return (
    <span className="form-actions">
      <Button
        variant="danger"
        busy={cancel.isPending}
        busyLabel="Cancelling…"
        onClick={() => {
          cancel.mutate(
            confirmation.confirm((key) => ({ accountId: ownAccountId ?? "", orderId: order.id, idempotencyKey: key })),
          );
        }}
      >
        Confirm: cancel this order
      </Button>
      <Button variant="quiet" onClick={startOver}>
        Keep it
      </Button>
      <KeyLine actionKey={confirmation.key} />
    </span>
  );
}

/* --------------------------------------------------------------------------
 * The operator's commands
 * ------------------------------------------------------------------------ */

export function ListEquityForm(): ReactNode {
  const ids = {
    kind: useId(),
    symbol: useId(),
    name: useId(),
    mic: useId(),
    settlement: useId(),
    shareDecimals: useId(),
    priceScale: useId(),
    tick: useId(),
    lot: useId(),
    min: useId(),
    age: useId(),
    sources: useId(),
    risk: useId(),
  };
  const [kind, setKind] = useState<EquityKind>("EQUITY");
  const [symbol, setSymbol] = useState("");
  const [name, setName] = useState("");
  const [mic, setMic] = useState("");
  const [settlement, setSettlement] = useState("");
  const [shareDecimals, setShareDecimals] = useState("");
  const [priceScale, setPriceScale] = useState("");
  const [tick, setTick] = useState("");
  const [lot, setLot] = useState("");
  const [min, setMin] = useState("");
  const [age, setAge] = useState("");
  const [sources, setSources] = useState("");
  const [risk, setRisk] = useState<EquityRiskClass>("STANDARD");
  const confirmation = useConfirmation<ListEquityCommand>();
  const list = useListEquity();

  const parsedSymbol = parseSymbolInput(symbol);
  const parsedDecimals = parseWholeNumber(shareDecimals, 0, 18, "the share decimals");
  const parsedScale = parseWholeNumber(priceScale, 0, 18, "the price scale");
  const parsedTick = parseBaseUnitsInput(tick, "the tick size");
  const parsedLot = parseBaseUnitsInput(lot, "the lot size");
  const parsedMin = parseBaseUnitsInput(min, "the minimum quantity");
  const parsedAge = parseWholeNumber(age, 1, 86_400_000, "the oracle's maximum age in milliseconds");
  const sourceList = sources
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
  const settlementId = settlement.trim();
  const problem = !parsedSymbol.ok
    ? parsedSymbol.error
    : name.trim() === ""
      ? "Name the listing."
      : !/^[A-Z0-9]{4}$/.test(mic.trim().toUpperCase())
        ? "The exchange MIC is four letters or digits."
        : settlementId !== "" && !isCanonicalUuid(settlementId)
          ? "The settlement asset id is a UUID, or empty for the sandbox dollar."
          : !parsedDecimals.ok
            ? parsedDecimals.error
            : !parsedScale.ok
              ? parsedScale.error
              : !parsedTick.ok
                ? parsedTick.error
                : !parsedLot.ok
                  ? parsedLot.error
                  : !parsedMin.ok
                    ? parsedMin.error
                    : !parsedAge.ok
                      ? parsedAge.error
                      : sourceList.length === 0
                        ? "Name at least one oracle source."
                        : undefined;
  const startOver = (): void => {
    confirmation.reset();
    list.reset();
  };
  function changed<T>(set: (value: T) => void): (value: T) => void {
    return (value: T) => {
      set(value);
      startOver();
    };
  }
  return (
    <>
      <div className="form">
        <ChoiceField id={ids.kind} label="Kind" value={kind} choices={EQUITY_KINDS} onChange={changed(setKind)} />
        <TextField id={ids.symbol} label="Symbol" value={symbol} problem={parsedSymbol.ok ? undefined : parsedSymbol.error} onChange={changed(setSymbol)} />
        <TextField id={ids.name} label="Name" value={name} onChange={changed(setName)} />
        <TextField id={ids.mic} label="Exchange MIC" value={mic} hint="Four letters or digits, such as XNAS." onChange={changed(setMic)} />
        <TextField
          id={ids.settlement}
          label="Settlement asset id (optional)"
          value={settlement}
          hint="Empty lists the shares in the sandbox dollar."
          onChange={changed(setSettlement)}
        />
        <TextField id={ids.shareDecimals} label="Share decimals" value={shareDecimals} hint="The fractional-share precision." onChange={changed(setShareDecimals)} />
        <TextField id={ids.priceScale} label="Price scale" value={priceScale} hint="Decimal places of a price." onChange={changed(setPriceScale)} />
        <TextField id={ids.tick} label="Tick size" value={tick} hint="In price units at the price scale: at scale 2, a tick of 1 is one hundredth." onChange={changed(setTick)} />
        <TextField id={ids.lot} label="Lot size" value={lot} hint="Base units of the share asset." onChange={changed(setLot)} />
        <TextField id={ids.min} label="Minimum quantity" value={min} hint="Base units of the share asset." onChange={changed(setMin)} />
        <TextField id={ids.age} label="Oracle maximum age (ms)" value={age} hint="A mark older than this is stale and prices nothing." onChange={changed(setAge)} />
        <TextField id={ids.sources} label="Oracle sources" value={sources} hint="Comma-separated." onChange={changed(setSources)} />
        <ChoiceField
          id={ids.risk}
          label="Risk class"
          value={risk}
          choices={EQUITY_RISK_CLASSES}
          describe={(value) => RISK_CLASS_LABEL[value]}
          onChange={changed(setRisk)}
        />
      </div>
      <Disclosure title="What listing does">
        <p>
          It creates the instrument and its share asset — a simulated asset that exists only in this deployment — at the terms
          above. Listing the same symbol again with the same terms returns it. This needs a recent step-up.
        </p>
      </Disclosure>
      <CommandControls
        confirmLabel="Confirm: list it"
        busyLabel="Listing…"
        problem={problem}
        mutation={list}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="List another"
        renderFailure={renderEquityFailure}
        build={(key) => ({
          kind,
          symbol: parsedSymbol.value,
          name: name.trim(),
          exchangeMic: mic.trim().toUpperCase(),
          settlementAssetId: settlementId === "" ? undefined : settlementId.toLowerCase(),
          shareDecimals: parsedDecimals.value,
          priceScale: parsedScale.value,
          tickSize: parsedTick.value,
          lotSize: parsedLot.value,
          minQuantity: parsedMin.value,
          oracleMaxAgeMs: parsedAge.value,
          oracleSources: sourceList,
          riskClass: risk,
          idempotencyKey: key,
        })}
        renderResult={(listing) => (
          <p className="callout" role="status">
            Listed <Link to={equitiesHref("listings", listing.instrument_id)}>{listing.symbol}</Link> ({listing.status}), settling in{" "}
            {listing.settlement_symbol}.
          </p>
        )}
      />
    </>
  );
}

export function AddHolidayForm(): ReactNode {
  const ids = { date: useId(), name: useId() };
  const [date, setDate] = useState("");
  const [name, setName] = useState("");
  const confirmation = useConfirmation<AddHolidayCommand>();
  const add = useAddHoliday();
  const parsedDate = parseDateInput(date);
  const problem = !parsedDate.ok ? parsedDate.error : name.trim() === "" ? "Name the holiday." : undefined;
  const startOver = (): void => {
    confirmation.reset();
    add.reset();
  };
  return (
    <>
      <div className="form">
        <TextField
          id={ids.date}
          label="Date"
          value={date}
          problem={parsedDate.ok ? undefined : parsedDate.error}
          hint="YYYY-MM-DD; a weekday. The market is closed all day."
          onChange={(value) => {
            setDate(value);
            startOver();
          }}
        />
        <TextField
          id={ids.name}
          label="Name"
          value={name}
          onChange={(value) => {
            setName(value);
            startOver();
          }}
        />
      </div>
      <CommandControls
        confirmLabel="Confirm: record the holiday"
        busyLabel="Recording…"
        problem={problem}
        mutation={add}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Record another"
        renderFailure={renderEquityFailure}
        build={(key) => ({ date: parsedDate.value, name: name.trim(), idempotencyKey: key })}
        renderResult={(calendar) => (
          <p className="callout" role="status">
            Recorded. The calendar now lists {String(calendar.holidays.length)} closures.
          </p>
        )}
      />
    </>
  );
}

export function AnnounceActionForm(props: { readonly instrumentId: string | undefined }): ReactNode {
  const ids = {
    instrument: useId(),
    type: useId(),
    reason: useId(),
    effective: useId(),
    numerator: useId(),
    denominator: useId(),
    inLieu: useId(),
    rounding: useId(),
    dividend: useId(),
    ex: useId(),
    record: useId(),
    pay: useId(),
    symbol: useId(),
    final: useId(),
  };
  const [instrumentId, setInstrumentId] = useState(props.instrumentId ?? "");
  const [type, setType] = useState<EquityActionType>("SPLIT");
  const [reason, setReason] = useState("");
  const [effective, setEffective] = useState("");
  const [numerator, setNumerator] = useState("");
  const [denominator, setDenominator] = useState("");
  const [inLieu, setInLieu] = useState("");
  const [rounding, setRounding] = useState<EquityRounding>("SHARES_DOWN_CASH_DOWN");
  const [dividend, setDividend] = useState("");
  const [exDate, setExDate] = useState("");
  const [recordDate, setRecordDate] = useState("");
  const [payDate, setPayDate] = useState("");
  const [newSymbol, setNewSymbol] = useState("");
  const [finalDate, setFinalDate] = useState("");
  const confirmation = useConfirmation<AnnounceActionCommand>();
  const announce = useAnnounceCorporateAction();

  const split = type === "SPLIT" || type === "REVERSE_SPLIT";
  const dividendType = type === "CASH_DIVIDEND";
  const pEffective = parseDateInput(effective);
  const pNumerator = parseWholeNumber(numerator, 1, 1_000_000, "the ratio's numerator");
  const pDenominator = parseWholeNumber(denominator, 1, 1_000_000, "the ratio's denominator");
  const withInLieu = inLieu.trim() !== "";
  const pInLieu = parsePriceInput(inLieu, "the in-lieu price");
  const pDividend = parsePriceInput(dividend, "the amount per share");
  const pEx = parseDateInput(exDate);
  const pRecord = parseDateInput(recordDate);
  const pPay = parseDateInput(payDate);
  const pSymbol = parseSymbolInput(newSymbol);
  const pFinal = parseDateInput(finalDate);

  const typeProblem = split
    ? !pEffective.ok
      ? pEffective.error
      : !pNumerator.ok
        ? pNumerator.error
        : !pDenominator.ok
          ? pDenominator.error
          : withInLieu && !pInLieu.ok
            ? pInLieu.error
            : undefined
    : dividendType
      ? !pDividend.ok
        ? pDividend.error
        : !pEx.ok
          ? `Ex-date: ${pEx.error}`
          : !pRecord.ok
            ? `Record date: ${pRecord.error}`
            : !pPay.ok
              ? `Pay date: ${pPay.error}`
              : undefined
      : type === "SYMBOL_CHANGE"
        ? !pEffective.ok
          ? pEffective.error
          : !pSymbol.ok
            ? pSymbol.error
            : undefined
        : !pEffective.ok
          ? pEffective.error
          : !pFinal.ok
            ? `Final date: ${pFinal.error}`
            : undefined;
  const problem = !isCanonicalUuid(instrumentId.trim())
    ? "The instrument id is a UUID."
    : (typeProblem ?? reasonProblem(reason));

  const startOver = (): void => {
    confirmation.reset();
    announce.reset();
  };
  function changed<T>(set: (value: T) => void): (value: T) => void {
    return (value: T) => {
      set(value);
      startOver();
    };
  }

  return (
    <>
      <div className="form">
        <TextField id={ids.instrument} label="Instrument id" value={instrumentId} onChange={changed(setInstrumentId)} />
        <ChoiceField
          id={ids.type}
          label="Action"
          value={type}
          choices={EQUITY_ACTION_TYPES}
          describe={(value) => ACTION_TYPE_LABEL[value]}
          onChange={changed(setType)}
        />
        {!dividendType && (
          <TextField
            id={ids.effective}
            label="Effective date"
            value={effective}
            problem={pEffective.ok ? undefined : pEffective.error}
            hint={type === "DELISTING" ? "The day the delisting begins: buys stop, holders may sell." : "The first trading day of the change."}
            onChange={changed(setEffective)}
          />
        )}
        {split && (
          <>
            <TextField id={ids.numerator} label="New shares" value={numerator} hint="Per the old shares below." onChange={changed(setNumerator)} />
            <TextField id={ids.denominator} label="Old shares" value={denominator} onChange={changed(setDenominator)} />
            <TextField
              id={ids.inLieu}
              label="In-lieu price per whole new share (optional)"
              value={inLieu}
              problem={!withInLieu || pInLieu.ok ? undefined : pInLieu.error}
              hint="Required when the ratio can leave a fraction of a share; that fraction is paid in the settlement asset at this price."
              onChange={changed(setInLieu)}
            />
            <ChoiceField
              id={ids.rounding}
              label="Rounding"
              value={rounding}
              choices={EQUITY_ROUNDINGS}
              describe={(value) => ROUNDING_LABEL[value]}
              onChange={changed(setRounding)}
            />
          </>
        )}
        {dividendType && (
          <>
            <TextField
              id={ids.dividend}
              label="Amount per whole share"
              value={dividend}
              problem={pDividend.ok ? undefined : pDividend.error}
              hint="Exact, in the listing's settlement asset."
              onChange={changed(setDividend)}
            />
            <TextField id={ids.ex} label="Ex-date" value={exDate} hint="Under T+1, the record date when that is a trading day." onChange={changed(setExDate)} />
            <TextField id={ids.record} label="Record date" value={recordDate} onChange={changed(setRecordDate)} />
            <TextField id={ids.pay} label="Pay date" value={payDate} hint="A trading day on or after the record date." onChange={changed(setPayDate)} />
          </>
        )}
        {type === "SYMBOL_CHANGE" && (
          <TextField id={ids.symbol} label="New symbol" value={newSymbol} problem={pSymbol.ok ? undefined : pSymbol.error} onChange={changed(setNewSymbol)} />
        )}
        {type === "DELISTING" && (
          <TextField id={ids.final} label="Final date" value={finalDate} hint="The day the instrument is delisted for good." onChange={changed(setFinalDate)} />
        )}
        <TextField id={ids.reason} label="Reason" value={reason} onChange={changed(setReason)} />
      </div>
      <CommandControls
        confirmLabel="Confirm: announce the action"
        busyLabel="Announcing…"
        problem={problem}
        mutation={announce}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Announce another"
        renderFailure={renderEquityFailure}
        build={(key) => ({
          instrumentId: instrumentId.trim().toLowerCase(),
          type,
          reason: reason.trim(),
          effectiveDate: dividendType ? undefined : pEffective.value,
          ratio: split ? { numerator: pNumerator.value, denominator: pDenominator.value } : undefined,
          dividendPerShare: dividendType ? pDividend.value : undefined,
          inLieuPrice: split && withInLieu ? pInLieu.value : undefined,
          exDate: dividendType ? pEx.value : undefined,
          recordDate: dividendType ? pRecord.value : undefined,
          payDate: dividendType ? pPay.value : undefined,
          finalDate: type === "DELISTING" ? pFinal.value : undefined,
          newSymbol: type === "SYMBOL_CHANGE" ? pSymbol.value : undefined,
          rounding: split ? rounding : undefined,
          idempotencyKey: key,
        })}
        renderResult={(action) => (
          <p className="callout" role="status">
            Announced <Link to={equitiesHref("corporate-actions", action.id)}>{ACTION_TYPE_LABEL[action.type]}</Link>, effective{" "}
            {action.effective_date}: {action.status}.
          </p>
        )}
      />
    </>
  );
}

/** Withdrawing an announced action, with its reason. */
export function CancelActionForm(props: { readonly action: EquityCorporateAction }): ReactNode {
  const reasonId = useId();
  const [reason, setReason] = useState("");
  const confirmation = useConfirmation<ActionCommand & { readonly reason: string }>();
  const cancel = useCancelCorporateAction();
  const startOver = (): void => {
    confirmation.reset();
    cancel.reset();
  };
  return (
    <>
      <div className="form">
        <TextField
          id={reasonId}
          label="Reason for withdrawing it"
          value={reason}
          onChange={(value) => {
            setReason(value);
            startOver();
          }}
        />
      </div>
      <CommandControls
        confirmLabel="Confirm: withdraw the action"
        busyLabel="Withdrawing…"
        variant="danger"
        problem={props.action.status !== "ANNOUNCED" ? `The action is ${props.action.status}; only an announced one is withdrawn.` : reasonProblem(reason)}
        mutation={cancel}
        confirmation={confirmation}
        onStartOver={startOver}
        renderFailure={renderEquityFailure}
        build={(key) => ({ actionId: props.action.id, reason: reason.trim(), idempotencyKey: key })}
        renderResult={(action) => (
          <p className="callout" role="status">
            The action is now {action.status}.
          </p>
        )}
      />
    </>
  );
}

/** Applying an action on or after its date: one posting per holder, ever. */
export function ApplyActionForm(props: { readonly action: EquityCorporateAction }): ReactNode {
  const confirmation = useConfirmation<ActionCommand>();
  const apply = useApplyCorporateAction();
  const startOver = (): void => {
    confirmation.reset();
    apply.reset();
  };
  const ended = props.action.status === "APPLIED" || props.action.status === "CANCELLED";
  return (
    <CommandControls
      confirmLabel="Confirm: apply the action"
      busyLabel="Applying…"
      variant="danger"
      problem={ended ? `The action is ${props.action.status}.` : undefined}
      mutation={apply}
      confirmation={confirmation}
      onStartOver={startOver}
      renderFailure={renderEquityFailure}
      build={(key) => ({ actionId: props.action.id, idempotencyKey: key })}
      renderResult={(result) => (
        <div className="callout" role="status">
          <p>
            {result.replayed
              ? "The action had already been applied; this is its record, and nothing was posted now."
              : `Applied: ${String(result.applications.length)} holders.`}{" "}
            The action is {result.action.status}.
            {result.instrument_status !== undefined && ` The instrument is ${result.instrument_status}.`}
            {result.terms_version !== undefined && ` Its terms are now version ${String(result.terms_version)}.`}
          </p>
          {result.applications.length > 0 && (
            <ScrollTable caption="Postings of this action" headers={["Account", "Entitled", "Shares", "Paid"]}>
              {result.applications.map((app) => (
                <tr key={app.id}>
                  <th scope="row">
                    <code className="mono-small">{app.account_id}</code>
                  </th>
                  <td>
                    <Units value={app.entitled_quantity} decimals={app.share_decimals} symbol={app.share_symbol} />
                  </td>
                  <td>
                    <Units value={app.shares_delta} decimals={app.share_decimals} symbol={app.share_symbol} />
                  </td>
                  <td>
                    <Units value={app.settlement_amount} decimals={app.settlement_decimals} symbol={app.settlement_symbol} />
                  </td>
                </tr>
              ))}
            </ScrollTable>
          )}
        </div>
      )}
    />
  );
}

function RunLimitField(props: { readonly id: string; readonly value: string; readonly onChange: (value: string) => void; readonly what: string }): ReactNode {
  return (
    <TextField
      id={props.id}
      label="Limit (optional)"
      value={props.value}
      hint={`The most ${props.what} this run works, from 1 to 500; empty for the backend's default.`}
      onChange={props.onChange}
    />
  );
}

function runLimit(raw: string): { readonly value: number | undefined; readonly problem: string | undefined } {
  if (raw.trim() === "") return { value: undefined, problem: undefined };
  const parsed = parseWholeNumber(raw, 1, 500, "the limit");
  return parsed.ok ? { value: parsed.value, problem: undefined } : { value: undefined, problem: parsed.error };
}

/** The settlement worker's step, run when the operator asks: everything due today settles, one account per transaction. */
export function SettlementRunForm(): ReactNode {
  const limitId = useId();
  const [limit, setLimit] = useState("");
  const confirmation = useConfirmation<RunCommand>();
  const run = useRunSettlements();
  const parsed = runLimit(limit);
  const startOver = (): void => {
    confirmation.reset();
    run.reset();
  };
  return (
    <>
      <div className="form">
        <RunLimitField
          id={limitId}
          value={limit}
          what="settlements"
          onChange={(value) => {
            setLimit(value);
            startOver();
          }}
        />
      </div>
      <CommandControls
        confirmLabel="Confirm: run settlement"
        busyLabel="Settling…"
        problem={parsed.problem}
        mutation={run}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Run again"
        renderFailure={renderEquityFailure}
        build={(key) => ({ limit: parsed.value, idempotencyKey: key })}
        renderResult={(result) => (
          <div className="callout" role="status">
            <p>
              Trade date {result.trade_date}: {String(result.settled.length)} settled in {String(result.batches)} transactions (as of{" "}
              <Recorded at={result.as_of} />
              ).
            </p>
            {result.settled.length > 0 && (
              <ScrollTable caption="Settled by this run" headers={["Settlement", "Account", "Amount", "Due"]}>
                {result.settled.map((s) => (
                  <tr key={s.id}>
                    <th scope="row">
                      <Link to={equitiesHref("orders", s.order_id)}>
                        <code className="mono-small">{s.id}</code>
                      </Link>
                    </th>
                    <td>
                      <code className="mono-small">{s.account_id}</code>
                    </td>
                    <td>
                      <Units value={s.quantity} decimals={s.asset_decimals} symbol={s.asset_symbol} />
                    </td>
                    <td>{s.due_date}</td>
                  </tr>
                ))}
              </ScrollTable>
            )}
          </div>
        )}
      />
    </>
  );
}

/** The venue worker's step, run when the operator asks: accepted orders are placed, resting ones polled. */
export function VenueRunForm(): ReactNode {
  const limitId = useId();
  const [limit, setLimit] = useState("");
  const confirmation = useConfirmation<RunCommand>();
  const run = useRunVenue();
  const parsed = runLimit(limit);
  const startOver = (): void => {
    confirmation.reset();
    run.reset();
  };
  return (
    <>
      <div className="form">
        <RunLimitField
          id={limitId}
          value={limit}
          what="orders"
          onChange={(value) => {
            setLimit(value);
            startOver();
          }}
        />
      </div>
      <CommandControls
        confirmLabel="Confirm: run the venue"
        busyLabel="Working orders…"
        problem={parsed.problem}
        mutation={run}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Run again"
        renderFailure={renderEquityFailure}
        build={(key) => ({ limit: parsed.value, idempotencyKey: key })}
        renderResult={(result) => (
          <div className="callout" role="status">
            <p>
              {String(result.items.length)} orders worked (as of <Recorded at={result.as_of} />
              ).
            </p>
            {result.items.length > 0 && (
              <ScrollTable caption="Orders this run worked" headers={["Order", "Step", "Before", "After", "Venue"]}>
                {result.items.map((item) => (
                  <tr key={item.order_id}>
                    <th scope="row">
                      <Link to={equitiesHref("orders", item.order_id)}>
                        <code className="mono-small">{item.order_id}</code>
                      </Link>
                    </th>
                    <td>{item.action}</td>
                    <td>{item.status_before}</td>
                    <td>
                      {item.status_after ?? (
                        <span className="absent">
                          unchanged{item.error_code !== undefined ? ` (code ${item.error_code})` : ""}
                        </span>
                      )}
                    </td>
                    <td>
                      {item.venue_status ?? <span className="absent">no answer</span>}
                      {item.venue_reason !== undefined && <span className="cell-note">{item.venue_reason}</span>}
                      {item.detail !== undefined && <span className="cell-note">{item.detail}</span>}
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
          </div>
        )}
      />
    </>
  );
}

/** Shown when a page needs to state the operator-only nature of what follows. */
export function OperatorOnly(props: { readonly children: ReactNode }): ReactNode {
  return (
    <>
      <p className="note">Operator only: the backend refuses anyone without sandbox:operate and a recent step-up, whatever this page shows.</p>
      {props.children}
    </>
  );
}
