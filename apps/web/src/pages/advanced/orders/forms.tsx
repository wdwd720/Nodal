/**
 * The commands of the order-book section. Each one is an explicit act with
 * its own idempotency key, created when the person confirms and reused — with
 * the very same request — for every retry, so a double click or a lost reply
 * cannot place, cancel or change an order twice. Each form shows what will be
 * sent; the backend decides whether it happens, and its refusal is what the
 * form shows when it does not.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useCancelBookOrder,
  useJournalVerification,
  useOpenBook,
  useReplaceBookOrder,
  useSetBookStatus,
  useSubmitBookOrder,
  type Book,
  type BookCommandResult,
  type BookFeeScheduleRequest,
  type BookOrder,
  type CancelBookOrderCommand,
  type BookStatusCommand,
  type OpenBookCommand,
  type ReplaceBookOrderCommand,
  type SubmitBookOrderCommand,
} from "../../../api/advanced/orders.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Disclosure, Field, FieldGrid, Identifier } from "../../../components/Layout.tsx";
import { BaseUnits, Bps } from "../../../components/Money.tsx";
import { isCanonicalUuid } from "../../../lib/advanced.ts";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import { parseQuantityInput } from "../../../lib/money.ts";
import {
  BOOK_MODES,
  BOOK_ORDER_TYPES,
  BOOK_SIDES,
  BOOK_STP_MODES,
  BOOK_TIMES_IN_FORCE,
  STP_LABEL,
  TIME_IN_FORCE_LABEL,
  bookTransitions,
  orderEntryAvailability,
  orderShapeProblem,
  parseClientOrderId,
  parsePriceInput,
  parseTermsJson,
  parseWholeNumber,
  reasonProblem,
  replaceAvailability,
  cancelAvailability,
  type BookMode,
  type BookOrderType,
  type BookSide,
  type BookStatus,
  type BookStp,
  type BookTimeInForce,
} from "../../../lib/orders.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { ScrollTable } from "../shared.tsx";
import {
  CheckField,
  ChoiceField,
  CommandControls,
  CommandFailure,
  KeyLine,
  PriceOf,
  StatusPill,
  TextField,
  Units,
  booksHref,
  orderStatusTone,
  useConfirmation,
} from "./parts.tsx";
import { Button } from "../../../components/Button.tsx";

/* --------------------------------------------------------------------------
 * What a command did
 * ------------------------------------------------------------------------ */

/** An order and its fills, exactly as the backend answered the command. */
export function CommandOutcome(props: { readonly result: BookCommandResult }): ReactNode {
  const { order, fills } = props.result;
  return (
    <div className="callout" role="status">
      <p>
        Order{" "}
        <Link to={booksHref("orders", order.id)}>
          <code className="mono-small">{order.id}</code>
        </Link>{" "}
        is <StatusPill status={order.status} tone={orderStatusTone(order.status)} />
        {props.result.intent_status !== undefined && (
          <>
            {" "}
            and its intent is <code className="mono-small">{props.result.intent_status}</code>
          </>
        )}
        .
      </p>
      <FieldGrid columns={3}>
        <Field label="Filled">
          <Units value={order.filled_quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
        </Field>
        <Field label="Still resting">
          <Units value={order.remaining_quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
        </Field>
        <Field label="Cancelled">
          <Units value={order.cancelled_quantity} decimals={order.base_decimals} symbol={order.base_symbol} />
        </Field>
        {order.reject_reason !== undefined && (
          <Field label="Refused because">
            <code className="mono-small">{order.reject_reason}</code>
          </Field>
        )}
        {props.result.command_seq !== undefined && (
          <Field label="Journal command" note="The command's sequence in the book's journal.">
            {String(props.result.command_seq)}
          </Field>
        )}
      </FieldGrid>
      {fills.length === 0 ? (
        <p className="note">This command produced no trade.</p>
      ) : (
        <ScrollTable caption="Trades of this command" headers={["Trade", "Side", "Liquidity", "Price", "Quantity", "Notional", "Fee"]}>
          {fills.map((fill) => (
            <tr key={`${fill.book_id}-${String(fill.trade_seq)}`}>
              <th scope="row">
                <Link to={booksHref("books", fill.instrument_id, "trades", String(fill.trade_seq))}>#{String(fill.trade_seq)}</Link>
              </th>
              <td>{fill.side}</td>
              <td>{fill.liquidity}</td>
              <td>
                <PriceOf value={fill.price} quote={fill.quote_symbol} />
              </td>
              <td>
                <Units value={fill.quantity} decimals={fill.base_decimals} symbol={fill.base_symbol} />
              </td>
              <td>
                <Units value={fill.quote_notional} decimals={fill.quote_decimals} symbol={fill.quote_symbol} />
              </td>
              <td>
                <Units value={fill.fee} decimals={fill.quote_decimals} symbol={fill.quote_symbol} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </div>
  );
}

/* --------------------------------------------------------------------------
 * Placing an order
 * ------------------------------------------------------------------------ */

export function SubmitOrderForm(props: { readonly book: Book }): ReactNode {
  const { book } = props;
  const { ownAccountId, inspecting } = useAdvancedAccount();
  const ids = {
    side: useId(),
    type: useId(),
    tif: useId(),
    price: useId(),
    quantity: useId(),
    postOnly: useId(),
    reduceOnly: useId(),
    stp: useId(),
    clientOrderId: useId(),
  };
  const [side, setSide] = useState<BookSide>("BUY");
  const [orderType, setOrderType] = useState<BookOrderType>("LIMIT");
  const [timeInForce, setTimeInForce] = useState<BookTimeInForce>("GTC");
  const [price, setPrice] = useState("");
  const [quantity, setQuantity] = useState("");
  const [postOnly, setPostOnly] = useState(false);
  const [reduceOnly, setReduceOnly] = useState(false);
  const [stp, setStp] = useState<BookStp>("CANCEL_TAKER");
  const [clientOrderId, setClientOrderId] = useState("");
  const confirmation = useConfirmation<SubmitBookOrderCommand>();
  const submit = useSubmitBookOrder();

  const parsedPrice = parsePriceInput(price);
  const parsedQuantity = parseQuantityInput(quantity, book.base_decimals);
  const parsedClientId = parseClientOrderId(clientOrderId);
  const entry = orderEntryAvailability(book.status);
  const shape = orderShapeProblem({ side, orderType, timeInForce, postOnly, reduceOnly });
  const problem =
    ownAccountId === undefined
      ? "This session owns no account to trade from."
      : !entry.available
        ? entry.reason
        : shape !== undefined
          ? shape
          : !parsedPrice.ok
            ? parsedPrice.error
            : !parsedQuantity.ok
              ? parsedQuantity.error
              : !parsedClientId.ok
                ? parsedClientId.error
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
        <ChoiceField id={ids.side} label="Side" value={side} choices={BOOK_SIDES} onChange={changed(setSide)} />
        <ChoiceField id={ids.type} label="Order type" value={orderType} choices={BOOK_ORDER_TYPES} onChange={changed(setOrderType)} />
        <ChoiceField
          id={ids.tif}
          label="Time in force"
          value={timeInForce}
          choices={BOOK_TIMES_IN_FORCE}
          describe={(value) => TIME_IN_FORCE_LABEL[value]}
          onChange={changed(setTimeInForce)}
        />
        <TextField
          id={ids.price}
          label={orderType === "LIMIT" ? `Limit price (${book.quote_symbol})` : `Protection price (${book.quote_symbol})`}
          value={price}
          problem={parsedPrice.ok ? undefined : parsedPrice.error}
          hint={
            orderType === "LIMIT"
              ? `The worst price this order trades at. Exact at ${String(book.price_scale)} decimal places, on the tick of ${book.tick_price}; it is never rounded.`
              : `A market order on this book states the worst price it may trade at; nothing trades beyond it. On the tick of ${book.tick_price}.`
          }
          onChange={changed(setPrice)}
        />
        <TextField
          id={ids.quantity}
          label={`Quantity (${book.base_symbol})`}
          value={quantity}
          problem={parsedQuantity.ok ? undefined : parsedQuantity.error}
          hint={`In whole lots of the book's lot size; at least its minimum. ${book.base_symbol} holds ${String(book.base_decimals)} decimal places.`}
          onChange={changed(setQuantity)}
        />
        <ChoiceField
          id={ids.stp}
          label="Self-trade prevention"
          value={stp}
          choices={BOOK_STP_MODES}
          describe={(value) => STP_LABEL[value]}
          onChange={changed(setStp)}
        />
        <CheckField
          id={ids.postOnly}
          label="Post only"
          checked={postOnly}
          hint="Refused instead of trading if it would take liquidity on arrival."
          onChange={changed(setPostOnly)}
        />
        <CheckField
          id={ids.reduceOnly}
          label="Reduce only"
          checked={reduceOnly}
          hint="A sell capped at what the account holds."
          onChange={changed(setReduceOnly)}
        />
        <TextField
          id={ids.clientOrderId}
          label="Client order id (optional)"
          value={clientOrderId}
          problem={parsedClientId.ok ? undefined : parsedClientId.error}
          hint="Your own reference. The book refuses a second order with the same one, and records that refusal."
          onChange={changed(setClientOrderId)}
        />
      </div>
      <p className="note">{ADVANCED_MODE_DESCRIPTIONS[book.mode]}</p>
      <FieldGrid columns={3}>
        <Field label="What will be sent">
          {side} {orderType} {timeInForce}
          {parsedQuantity.ok ? (
            <>
              {" "}
              · <Units value={parsedQuantity.value} decimals={book.base_decimals} symbol={book.base_symbol} />{" "}
              <BaseUnits value={parsedQuantity.value} />
            </>
          ) : (
            " · no quantity yet"
          )}
          {parsedPrice.ok ? ` · at ${parsedPrice.value} ${book.quote_symbol}` : " · no price yet"} · {book.mode}
        </Field>
        <Field label="Account">
          {ownAccountId === undefined ? <span className="absent">none</span> : <code className="mono-small">{ownAccountId}</code>}
        </Field>
        <Field label="Disclosed fees" note="In the book's quote asset, charged on the notional of each trade by the schedules shown on this page.">
          taker <Bps value={book.taker_fee.percentage_fee_bps} />
          <span className="cell-note">
            maker <Bps value={book.maker_fee.percentage_fee_bps} />
          </span>
        </Field>
      </FieldGrid>
      <CommandControls
        confirmLabel={`Confirm: ${side.toLowerCase()} on the book`}
        busyLabel="Placing…"
        problem={problem}
        mutation={submit}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Start a new order"
        build={(key) => ({
          accountId: ownAccountId ?? "",
          instrumentId: book.instrument_id,
          side,
          orderType,
          timeInForce,
          price: parsedPrice.value,
          quantity: parsedQuantity.value,
          postOnly,
          reduceOnly,
          stp,
          clientOrderId: parsedClientId.value === "" ? undefined : parsedClientId.value,
          mode: book.mode,
          idempotencyKey: key,
        })}
        renderResult={(result) => <CommandOutcome result={result} />}
      />
    </>
  );
}

/* --------------------------------------------------------------------------
 * Cancelling and replacing
 * ------------------------------------------------------------------------ */

/**
 * Cancelling one order: a button that asks once more, then sends with the key
 * created at that confirmation. Compact enough for a table row.
 */
export function CancelOrderAction(props: { readonly order: BookOrder; readonly bookStatus: string | undefined }): ReactNode {
  const { order } = props;
  const { ownAccountId } = useAdvancedAccount();
  const [asking, setAsking] = useState(false);
  const confirmation = useConfirmation<CancelBookOrderCommand>();
  const cancel = useCancelBookOrder();
  const availability = cancelAvailability(order.status, props.bookStatus);
  const owner = ownAccountId !== undefined && ownAccountId.toLowerCase() === order.account_id.toLowerCase();

  const startOver = (): void => {
    confirmation.reset();
    cancel.reset();
    setAsking(false);
  };

  if (cancel.isSuccess) {
    return (
      <span role="status">
        <StatusPill status={cancel.data.order.status} tone={orderStatusTone(cancel.data.order.status)} />
        <span className="cell-note mono-small">key {confirmation.key ?? ""}</span>
      </span>
    );
  }
  if (cancel.isError) {
    return (
      <CommandFailure
        error={cancel.error}
        onRetry={() => {
          if (confirmation.command !== undefined) cancel.mutate(confirmation.command);
        }}
        onStartOver={startOver}
      />
    );
  }
  if (!availability.available) {
    return <span className="absent">{availability.reason}</span>;
  }
  if (!owner) {
    return <span className="absent">Only the account&apos;s owner cancels its orders.</span>;
  }
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
            confirmation.confirm((key) => ({
              accountId: ownAccountId ?? "",
              orderId: order.id,
              instrumentId: order.instrument_id,
              idempotencyKey: key,
            })),
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

export function ReplaceOrderForm(props: { readonly order: BookOrder; readonly bookStatus: string | undefined }): ReactNode {
  const { order } = props;
  const { ownAccountId } = useAdvancedAccount();
  const ids = { price: useId(), quantity: useId() };
  const [price, setPrice] = useState("");
  const [quantity, setQuantity] = useState("");
  const confirmation = useConfirmation<ReplaceBookOrderCommand>();
  const replace = useReplaceBookOrder();
  const parsedPrice = parsePriceInput(price);
  const parsedQuantity = parseQuantityInput(quantity, order.base_decimals);
  const availability = replaceAvailability(order.status, props.bookStatus);
  const owner = ownAccountId !== undefined && ownAccountId.toLowerCase() === order.account_id.toLowerCase();
  const problem = !availability.available
    ? availability.reason
    : !owner
      ? "Only the account's owner replaces its orders."
      : !parsedPrice.ok
        ? parsedPrice.error
        : !parsedQuantity.ok
          ? parsedQuantity.error
          : undefined;
  const startOver = (): void => {
    confirmation.reset();
    replace.reset();
  };
  return (
    <>
      <div className="form">
        <TextField
          id={ids.price}
          label={`New price (${order.quote_symbol ?? "quote asset"})`}
          value={price}
          problem={parsedPrice.ok ? undefined : parsedPrice.error}
          hint="Exact, on the book's tick; never rounded."
          onChange={(value) => {
            setPrice(value);
            startOver();
          }}
        />
        <TextField
          id={ids.quantity}
          label={`New total quantity (${order.base_symbol ?? "base asset"})`}
          value={quantity}
          problem={parsedQuantity.ok ? undefined : parsedQuantity.error}
          hint="The order's new TOTAL, what has already filled included; it must exceed what has filled."
          onChange={(value) => {
            setQuantity(value);
            startOver();
          }}
        />
      </div>
      <CommandControls
        confirmLabel="Confirm: replace the order"
        busyLabel="Replacing…"
        problem={problem}
        mutation={replace}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Replace it again"
        build={(key) => ({
          accountId: ownAccountId ?? "",
          orderId: order.id,
          instrumentId: order.instrument_id,
          price: parsedPrice.value,
          quantity: parsedQuantity.value,
          idempotencyKey: key,
        })}
        renderResult={(result) => <CommandOutcome result={result} />}
      />
    </>
  );
}

/* --------------------------------------------------------------------------
 * The operator's commands
 * ------------------------------------------------------------------------ */

interface ScheduleDraft {
  readonly version: string;
  readonly quoteAssetId: string;
  readonly quoteDecimals: string;
  readonly percentageBps: string;
  readonly strategyBps: string;
  readonly discountBps: string;
  readonly capBps: string;
}

const EMPTY_SCHEDULE: ScheduleDraft = {
  version: "",
  quoteAssetId: "",
  quoteDecimals: "",
  percentageBps: "",
  strategyBps: "",
  discountBps: "",
  capBps: "",
};

/** A schedule draft as the request carries it, or the first problem with it. */
function scheduleOf(draft: ScheduleDraft, which: string): { readonly schedule?: BookFeeScheduleRequest; readonly problem?: string } {
  if (draft.version.trim() === "") return { problem: `Name the ${which} schedule's version.` };
  if (!isCanonicalUuid(draft.quoteAssetId.trim())) return { problem: `The ${which} schedule's quote asset is a UUID.` };
  const decimals = parseWholeNumber(draft.quoteDecimals, 0, 18, "the quote asset's decimals");
  if (!decimals.ok) return { problem: `${which}: ${decimals.error}` };
  const pct = parseWholeNumber(draft.percentageBps, 0, 10_000, "the percentage fee in basis points");
  if (!pct.ok) return { problem: `${which}: ${pct.error}` };
  const optional = (raw: string, what: string): { readonly value?: number; readonly problem?: string } => {
    if (raw.trim() === "") return {};
    const parsed = parseWholeNumber(raw, 0, 10_000, what);
    return parsed.ok ? { value: parsed.value } : { problem: `${which}: ${parsed.error}` };
  };
  const strategy = optional(draft.strategyBps, "the strategy fee in basis points");
  const discount = optional(draft.discountBps, "the subscription discount in basis points");
  const cap = optional(draft.capBps, "the fee cap in basis points");
  const first = strategy.problem ?? discount.problem ?? cap.problem;
  if (first !== undefined) return { problem: first };
  return {
    schedule: {
      version: draft.version.trim(),
      quote_asset_id: draft.quoteAssetId.trim().toLowerCase(),
      quote_decimals: decimals.value,
      percentage_fee_bps: pct.value,
      ...(strategy.value === undefined ? {} : { strategy_fee_bps: strategy.value }),
      ...(discount.value === undefined ? {} : { subscription_discount_bps: discount.value }),
      ...(cap.value === undefined ? {} : { max_total_fee_bps: cap.value }),
    },
  };
}

function ScheduleFields(props: {
  readonly title: string;
  readonly draft: ScheduleDraft;
  readonly onChange: (draft: ScheduleDraft) => void;
}): ReactNode {
  const ids = {
    version: useId(),
    asset: useId(),
    decimals: useId(),
    pct: useId(),
    strategy: useId(),
    discount: useId(),
    cap: useId(),
  };
  const set = (field: keyof ScheduleDraft) => (value: string) => {
    props.onChange({ ...props.draft, [field]: value });
  };
  return (
    <fieldset className="form">
      <legend>{props.title}</legend>
      <TextField id={ids.version} label="Version" value={props.draft.version} onChange={set("version")} />
      <TextField
        id={ids.asset}
        label="Quote asset id"
        value={props.draft.quoteAssetId}
        hint="The book's quote asset: a USD-pegged simulated asset."
        onChange={set("quoteAssetId")}
      />
      <TextField id={ids.decimals} label="Quote asset decimals" value={props.draft.quoteDecimals} onChange={set("quoteDecimals")} />
      <TextField id={ids.pct} label="Percentage fee (bps)" value={props.draft.percentageBps} onChange={set("percentageBps")} />
      <TextField id={ids.strategy} label="Strategy fee (bps, optional)" value={props.draft.strategyBps} onChange={set("strategyBps")} />
      <TextField
        id={ids.discount}
        label="Subscription discount (bps, optional)"
        value={props.draft.discountBps}
        onChange={set("discountBps")}
      />
      <TextField id={ids.cap} label="Fee cap (bps, optional)" value={props.draft.capBps} onChange={set("capBps")} />
    </fieldset>
  );
}

export function OpenBookForm(): ReactNode {
  const ids = { instrument: useId(), mode: useId(), terms: useId(), snapshot: useId() };
  const [instrumentId, setInstrumentId] = useState("");
  const [mode, setMode] = useState<BookMode>("PAPER");
  const [maker, setMaker] = useState<ScheduleDraft>(EMPTY_SCHEDULE);
  const [taker, setTaker] = useState<ScheduleDraft>(EMPTY_SCHEDULE);
  const [terms, setTerms] = useState("");
  const [snapshot, setSnapshot] = useState("");
  const confirmation = useConfirmation<OpenBookCommand>();
  const open = useOpenBook();
  const makerSchedule = scheduleOf(maker, "maker");
  const takerSchedule = scheduleOf(taker, "taker");
  const parsedTerms = parseTermsJson(terms);
  const parsedSnapshot = snapshot.trim() === "" ? undefined : parseWholeNumber(snapshot, 1, 1_000_000, "the snapshot interval");
  const problem = !isCanonicalUuid(instrumentId.trim())
    ? "The instrument id is a UUID."
    : (makerSchedule.problem ??
      takerSchedule.problem ??
      (!parsedTerms.ok ? parsedTerms.error : parsedSnapshot !== undefined && !parsedSnapshot.ok ? parsedSnapshot.error : undefined));
  const startOver = (): void => {
    confirmation.reset();
    open.reset();
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
        <ChoiceField id={ids.mode} label="Mode" value={mode} choices={BOOK_MODES} onChange={changed(setMode)} />
        <TextField
          id={ids.snapshot}
          label="Snapshot interval (commands, optional)"
          value={snapshot}
          problem={parsedSnapshot === undefined || parsedSnapshot.ok ? undefined : parsedSnapshot.error}
          onChange={changed(setSnapshot)}
        />
      </div>
      <ScheduleFields title="Maker fee schedule" draft={maker} onChange={changed(setMaker)} />
      <ScheduleFields title="Taker fee schedule" draft={taker} onChange={changed(setTaker)} />
      <div className="form">
        <TextField
          id={ids.terms}
          label="Spot terms (JSON, only for a spot pair)"
          value={terms}
          multiline
          problem={parsedTerms.ok ? undefined : parsedTerms.error}
          hint="A spot pair has no canonical terms row, so its book states them here; any other instrument trades on its canonical terms and refuses these."
          onChange={changed(setTerms)}
        />
      </div>
      <Disclosure title="What opening a book does">
        <p>
          The book trades only simulated assets, in {mode}. Opening the same book again with the same terms returns it; different terms
          are refused as a conflict. This needs a recent step-up.
        </p>
      </Disclosure>
      <CommandControls
        confirmLabel="Confirm: open the book"
        busyLabel="Opening…"
        problem={problem}
        mutation={open}
        confirmation={confirmation}
        onStartOver={startOver}
        againLabel="Open another"
        build={(key) => ({
          instrumentId: instrumentId.trim().toLowerCase(),
          mode,
          makerFee: makerSchedule.schedule as BookFeeScheduleRequest,
          takerFee: takerSchedule.schedule as BookFeeScheduleRequest,
          spotTerms: parsedTerms.value,
          snapshotInterval: parsedSnapshot?.ok === true ? parsedSnapshot.value : undefined,
          idempotencyKey: key,
        })}
        renderResult={(book) => (
          <p className="callout" role="status">
            The book of{" "}
            <Link to={booksHref("books", book.instrument_id)}>
              {book.base_symbol}/{book.quote_symbol}
            </Link>{" "}
            is {book.status} in {book.mode}.
          </p>
        )}
      />
    </>
  );
}

export function BookStatusForm(props: { readonly book: Book }): ReactNode {
  const { book } = props;
  const choices = bookTransitions(book.status);
  const ids = { status: useId(), reason: useId() };
  const [status, setStatus] = useState<BookStatus | undefined>(choices[0]);
  const [reason, setReason] = useState("");
  const confirmation = useConfirmation<BookStatusCommand>();
  const command = useSetBookStatus();
  const startOver = (): void => {
    confirmation.reset();
    command.reset();
  };
  if (choices.length === 0 || status === undefined) {
    return <p className="note">A {book.status} book does not change status again.</p>;
  }
  return (
    <>
      <div className="form">
        <ChoiceField
          id={ids.status}
          label="Move the book to"
          value={status}
          choices={choices}
          onChange={(value) => {
            setStatus(value);
            startOver();
          }}
        />
        <TextField
          id={ids.reason}
          label="Reason"
          value={reason}
          onChange={(value) => {
            setReason(value);
            startOver();
          }}
        />
      </div>
      <CommandControls
        confirmLabel={`Confirm: move the book to ${status}`}
        busyLabel="Recording…"
        variant={status === "OPEN" ? "primary" : "danger"}
        problem={reasonProblem(reason)}
        mutation={command}
        confirmation={confirmation}
        onStartOver={startOver}
        build={(key) => ({ instrumentId: book.instrument_id, status, reason: reason.trim(), idempotencyKey: key })}
        renderResult={(updated) => (
          <p className="callout" role="status">
            The book is now <StatusPill status={updated.status} tone="info" />.
          </p>
        )}
      />
    </>
  );
}

/** The operator's read-only replay of a book's journal. */
export function JournalVerificationPanel(props: { readonly book: Book }): ReactNode {
  const [asked, setAsked] = useState(0);
  const verification = useJournalVerification(props.book.instrument_id, asked);
  return (
    <>
      <div className="form-actions">
        <Button
          onClick={() => {
            setAsked(asked + 1);
          }}
        >
          {asked === 0 ? "Verify the journal" : "Verify again"}
        </Button>
      </div>
      {asked > 0 && (
        <AsyncPanel query={verification} loadingLabel="Replaying the journal…">
          {(v) => (
            <>
              <p className="note">
                Verified at {formatInstant(v.verified_at)} <span className="mono-small">({exactInstant(v.verified_at)})</span>. The
                replay of the whole journal{" "}
                {v.full_replay_hash === v.state_hash ? "reproduces" : "does NOT reproduce"} the book&apos;s recorded state hash, and
                the replay from the latest snapshot {v.snapshot_replay_hash === v.state_hash ? "reproduces" : "does NOT reproduce"} it.
              </p>
              <FieldGrid columns={3}>
                <Field label="Commands">{String(v.commands)}</Field>
                <Field label="Events">{String(v.events)}</Field>
                <Field label="Trades">{String(v.trades)}</Field>
                <Field label="Orders checked">{String(v.orders_checked)}</Field>
                <Field label="Snapshot at command">{String(v.snapshot_seq)}</Field>
                <Field label="Recorded state hash">
                  <Identifier value={v.state_hash} />
                </Field>
                <Field label="Full replay hash">
                  <Identifier value={v.full_replay_hash} />
                </Field>
                <Field label="Snapshot replay hash">
                  <Identifier value={v.snapshot_replay_hash} />
                </Field>
                <Field label="Genesis hash">
                  <Identifier value={v.genesis_hash} />
                </Field>
              </FieldGrid>
            </>
          )}
        </AsyncPanel>
      )}
    </>
  );
}
