/**
 * THE ORDER TICKET (product goal §13; USER_JOURNEY §5).
 *
 * The shape of this is the argument. A quote against an internal market is a
 * RECORD of what the curve said at one state version; the backend re-prices on
 * execution, so the customer is never asked to agree to the quote. They are
 * asked to agree to a MINIMUM they will accept, which travels with the order
 * and is checked against a freshly computed fill.
 *
 * What that means, concretely, and what each rule costs if it is dropped:
 *
 *   - THE QUOTE IS NOT A PRICE, and the sentence saying so is beside it rather
 *     than behind a disclosure. A customer who believes the quote priced their
 *     execution reads every ordinary re-price as a betrayal.
 *   - THE MINIMUM IS THE CUSTOMER'S. It is derived from a tolerance they chose,
 *     in exact BigInt base units, rounded towards them (`lib/min-output.ts`).
 *     Defaulting it to the quote would be a tolerance of zero dressed up as a
 *     protection, and would refuse nearly every order.
 *   - THE KEY IS MINTED AT CONFIRMATION, never on render, and it is kept for
 *     exactly as long as the request it belongs to. A session that expires
 *     between the press and the answer is scenario J: the draft and the key
 *     survive the sign-in, and the retry is the same request rather than a
 *     second one. But a key kept PAST its body is the opposite defect, and it
 *     reads worse — the backend compares the body it recorded, finds a
 *     different `quote_id`, and refuses with INVALID_IDEMPOTENCY_REUSE, so a
 *     customer who has just re-priced and confirmed is told something about a
 *     header instead of something about their order. Every route back to the
 *     confirm button goes through a new quote, so the key belongs to the quote
 *     as much as to the amount: `lib/idempotency.ts` holds it against a
 *     signature of the whole body and mints a new one the moment any part of
 *     it changes.
 *   - AFTER A FILL, THE FILL IS SHOWN. Not the quote. They are different
 *     numbers and the quote is the one that did not happen.
 *   - NOTHING HERE COMPUTES A PRICE, A FEE OR A BALANCE. Every figure except
 *     the minimum is the backend's own, rendered at the scale the response
 *     carries.
 *
 * On a phone the whole ticket is a bottom sheet with 44px controls (goal §31);
 * the parent decides, because it is the thing that knows how wide the viewport
 * is.
 */
import { useEffect, useState, type ReactNode } from "react";

import {
  useCreditBalance,
  usePortfolio,
  useNativeOrder,
  useNativeQuote,
  positionIn,
  type NativeFill,
  type NativeMarketSummary,
  type NativeQuote,
} from "../../api/queries.ts";
import { Button } from "../../components/Button.tsx";
import { FormField } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Disclosure, Field, FieldGrid, Panel } from "../../components/Layout.tsx";
import { fromBaseUnits } from "../../lib/format.ts";
import { parseQuantityInput } from "../../lib/money.ts";
import { useIdempotencyKey, requestSignature } from "../../lib/idempotency.ts";
import { TOLERANCES, meetsMinimum, minimumOutput } from "../../lib/min-output.ts";
import { secondsUntil } from "../../lib/time.ts";
import { CREDITS_DISCLOSURE, NATIVE_ASSET_RISK } from "../../lib/honesty.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { QUOTE_IS_NOT_A_PRICE, statusCopy } from "./copy.ts";
import { TradeRefusal } from "./TradeRefusal.tsx";
import { CREDIT_DECIMALS } from "../../lib/credits.ts";

type Side = "BUY" | "SELL";

interface Draft {
  readonly side: Side;
  /** What the customer typed, as they typed it. */
  readonly amount: string;
  readonly toleranceBps: number;
}

const EMPTY: Draft = { side: "BUY", amount: "", toleranceBps: 100 };

/** Whole seconds left on a quote, ticking. Undefined when there is no quote. */
function useCountdown(expiresAt: string | undefined): number | undefined {
  const [left, setLeft] = useState<number | undefined>(() => secondsUntil(expiresAt));

  useEffect(() => {
    setLeft(secondsUntil(expiresAt));
    if (expiresAt === undefined) return;
    const timer = setInterval(() => {
      const next = secondsUntil(expiresAt);
      setLeft(next);
      if (next === undefined || next === 0) clearInterval(timer);
    }, 1000);
    return () => {
      clearInterval(timer);
    };
  }, [expiresAt]);

  return left;
}

export function Ticket(props: {
  readonly market: NativeMarketSummary;
  readonly accountId: string | undefined;
  /** Called after a fill, so the page can refresh what the fill changed. */
  readonly onFilled: () => void;
  /** A demo or sandbox market renders every figure inside desaturated. */
  readonly simulated: boolean;
}): ReactNode {
  const { market } = props;
  const kept = useSurvivesSignIn<Draft>(`markets.ticket.${market.market_id}`, EMPTY);
  const draft = kept.value;
  const orderKey = useIdempotencyKey(`markets.ticket.${market.market_id}.key`);
  const quote = useNativeQuote();
  const order = useNativeOrder();
  const credits = useCreditBalance(props.accountId);
  const portfolio = usePortfolio(props.accountId);
  const position = positionIn(portfolio.data, market.market_id);

  const status = statusCopy(market.market_status);
  const allowed = draft.side === "BUY" ? status.buy : status.sell;
  const decimals = draft.side === "BUY" ? CREDIT_DECIMALS : market.asset_decimals;
  const parsed = parseQuantityInput(draft.amount, decimals);

  /**
   * Anything that changes WHAT is being ordered invalidates the quote and the
   * key alike. A key kept across a changed amount would make the backend replay
   * the first order and answer with a fill the customer did not ask for.
   */
  const change = (patch: Partial<Draft>): void => {
    quote.reset();
    order.reset();
    orderKey.clear();
    kept.set({ ...draft, ...patch });
  };

  /**
   * Ask the market to price it again.
   *
   * Both routes to the confirm button come through here, and both discard the
   * key: a new quote is a new `quote_id`, a new `quote_id` is a different body,
   * and a different body under the same key is the refusal the customer cannot
   * do anything about. The key is minted again at the next confirmation, which
   * is where it belongs.
   */
  const requote = (): void => {
    order.reset();
    orderKey.clear();
    quote.mutate({
      marketId: market.market_id,
      accountId: props.accountId ?? "",
      side: draft.side,
      amount: parsed.ok ? parsed.value : "",
    });
  };

  if (props.accountId === undefined) {
    return (
      <Panel title="Trade" description="There is no account on this session to trade with.">
        <p className="field-note">
          The backend returned an empty account list, so there is nothing to spend Credits from.
        </p>
      </Panel>
    );
  }

  if (order.isSuccess) {
    return (
      <Filled
        fill={order.data}
        market={market}
        side={draft.side}
        minimum={quote.data === undefined ? undefined : minimumOutput(quote.data.expected_output, draft.toleranceBps)}
        simulated={props.simulated}
        onAgain={() => {
          order.reset();
          quote.reset();
          orderKey.clear();
          kept.set({ ...draft, amount: "" });
        }}
      />
    );
  }

  return (
    <Panel
      title="Trade"
      description="Priced against the pool when the order runs, never against a quote."
      temp={props.simulated ? "simulated" : "economy"}
    >
      <div className="ticket stack">
        <form
          className="stack"
          onSubmit={(event) => {
            event.preventDefault();
            if (!parsed.ok) return;
            requote();
          }}
        >
          <fieldset className="ticket-side">
            <legend>Direction</legend>
            <label>
              <input
                type="radio"
                name={`side-${market.market_id}`}
                value="BUY"
                checked={draft.side === "BUY"}
                onChange={() => {
                  change({ side: "BUY", amount: "" });
                }}
              />
              <span>Buy with Credits</span>
            </label>
            <label>
              <input
                type="radio"
                name={`side-${market.market_id}`}
                value="SELL"
                checked={draft.side === "SELL"}
                onChange={() => {
                  change({ side: "SELL", amount: "" });
                }}
              />
              <span>Sell for Credits</span>
            </label>
          </fieldset>

          <FormField
            label={draft.side === "BUY" ? "Credits to spend" : `${market.symbol} to sell`}
            hint={
              draft.side === "BUY"
                ? `Whole Credits and up to ${String(CREDIT_DECIMALS)} decimal places.`
                : `Up to ${String(market.asset_decimals)} decimal places, which is this asset's own precision.`
            }
            error={draft.amount === "" ? "" : parsed.error}
          >
            {(field) => (
              <input
                {...field}
                value={draft.amount}
                inputMode="decimal"
                autoComplete="off"
                onChange={(event) => {
                  change({ amount: event.target.value });
                }}
              />
            )}
          </FormField>

          <FieldGrid columns={2}>
            <Field
              label="Spendable Credits"
              note="What the ledger says may be spent now. It excludes anything frozen or committed."
            >
              <Figure
                kind="money"
                value={
                  credits.data === undefined
                    ? null
                    : { base: credits.data.spendable, scale: CREDIT_DECIMALS }
                }
                symbol="Credits"
                absent="not read yet"
              />
            </Field>
            <Field label={`${market.symbol} held`} note="What this account holds of this asset.">
              <Figure
                kind="units"
                value={
                  position === undefined
                    ? null
                    : { base: position.quantity, scale: position.asset_decimals }
                }
                symbol={market.symbol}
                absent="none held"
              />
            </Field>
          </FieldGrid>

          {!allowed ? (
            <Button
              variant="secondary"
              fullWidth
              disabledReason={`This market is ${market.market_status}. ${status.text}`}
            >
              Get a quote
            </Button>
          ) : !parsed.ok ? (
            <Button
              variant="secondary"
              fullWidth
              disabledReason="Enter an amount before asking the market to price it."
            >
              Get a quote
            </Button>
          ) : (
            <Button variant="secondary" fullWidth submit busy={quote.isPending} busyLabel="Pricing…">
              Get a quote
            </Button>
          )}
        </form>

        {quote.isError && (
          <TradeRefusal
            what="This quote"
            error={quote.error}
            onRetry={() => {
              quote.reset();
            }}
          />
        )}

        {quote.isSuccess && (
          <Confirm
            quote={quote.data}
            market={market}
            side={draft.side}
            toleranceBps={draft.toleranceBps}
            onTolerance={(bps) => {
              // The tolerance IS `min_output`, which is in the body. A changed
              // tolerance is therefore a changed order, not a retry of the last
              // one — and it does not need a new quote, so the key is dropped
              // here rather than by `requote`.
              order.reset();
              orderKey.clear();
              kept.set({ ...draft, toleranceBps: bps });
            }}
            pending={order.isPending}
            error={order.isError ? order.error : undefined}
            onRequote={requote}
            onConfirm={() => {
              const minOutput = minimumOutput(quote.data.expected_output, draft.toleranceBps);
              // Minted here and nowhere else, against the body the backend will
              // compare it to. Pressing confirm twice on the SAME order reuses
              // it — that is the retry idempotency exists for. Confirming after
              // anything in the body moved mints a new one, because it is a new
              // order and replaying the old one would fill something nobody
              // asked for.
              const key = orderKey.forRequest(
                requestSignature([
                  quote.data.quote_id,
                  draft.side,
                  parsed.value,
                  minOutput,
                ]),
              );
              order.mutate(
                {
                  marketId: market.market_id,
                  accountId: props.accountId ?? "",
                  side: draft.side,
                  amount: parsed.value,
                  minOutput,
                  quoteId: quote.data.quote_id,
                  idempotencyKey: key,
                },
                {
                  onSuccess: () => {
                    // The order is answered; the key has nothing left to
                    // protect and a kept one would only be able to be wrong.
                    orderKey.clear();
                    props.onFilled();
                  },
                },
              );
            }}
          />
        )}

        <Disclosure title="What you are spending">
          <p>{CREDITS_DISCLOSURE}</p>
        </Disclosure>
      </div>
    </Panel>
  );
}

/** The quote, the tolerance, the minimum it produces, and the confirmation. */
function Confirm(props: {
  readonly quote: NativeQuote;
  readonly market: NativeMarketSummary;
  readonly side: Side;
  readonly toleranceBps: number;
  readonly onTolerance: (bps: number) => void;
  readonly onConfirm: () => void;
  readonly onRequote: () => void;
  readonly pending: boolean;
  readonly error: unknown;
}): ReactNode {
  const { quote, market } = props;
  const left = useCountdown(quote.expires_at);
  const expired = left === 0;
  const minimum = minimumOutput(quote.expected_output, props.toleranceBps);

  // On a BUY the output is the asset; on a SELL it is Credits. Nothing on this
  // ticket assumes one scale for both, which was F-44.
  const outScale = props.side === "BUY" ? (quote.asset_decimals ?? market.asset_decimals) : CREDIT_DECIMALS;
  const outSymbol = props.side === "BUY" ? market.symbol : "Credits";

  return (
    <div className="stack">
      <FieldGrid columns={2}>
        <Field
          label="You would receive"
          note="What the curve returned at the state version below. It is not what you will be paid."
          emphasis
        >
          <Figure
            kind="units"
            value={{ base: quote.expected_output, scale: outScale }}
            symbol={outSymbol}
          />
        </Field>
        <Field
          label="Minimum you will receive"
          note="Yours, not the market's. The order is refused if a fresh fill would return less."
          emphasis
        >
          <Figure kind="units" value={{ base: minimum, scale: outScale }} symbol={outSymbol} />
        </Field>
        <Field label="Price impact" note="How far this order moves the price, in basis points.">
          <Figure kind="bps" bps={quote.slippage_bps} absent="not reported" />
        </Field>
        <Field label="Effective price" note="In Credits, at this market's own price scale.">
          <Figure
            kind="money"
            value={
              quote.effective_price === undefined || quote.price_scale === undefined
                ? null
                : { decimal: fromBaseUnits(quote.effective_price, quote.price_scale) }
            }
            symbol="Credits"
            absent="not reported"
          />
        </Field>
        <Field label="Platform fee">
          <Figure
            kind="money"
            value={quote.platform_fee === undefined ? null : { base: quote.platform_fee, scale: CREDIT_DECIMALS }}
            symbol="Credits"
            absent="not reported"
          />
        </Field>
        <Field label="Creator fee" note="Paid to whoever created this asset, on every trade.">
          <Figure
            kind="money"
            value={quote.creator_fee === undefined ? null : { base: quote.creator_fee, scale: CREDIT_DECIMALS }}
            symbol="Credits"
            absent="not reported"
          />
        </Field>
      </FieldGrid>

      <p className={expired ? "quote-expiry quote-expiry-gone" : "quote-expiry"} role="status">
        {left === undefined
          ? "This quote carries no expiry."
          : expired
            ? "This quote has expired. Take a new one."
            : `This quote expires in ${String(left)}s · state version ${String(quote.state_version)}`}
      </p>

      <p className="field-note">{QUOTE_IS_NOT_A_PRICE}</p>

      <fieldset className="ticket-tolerance">
        <legend>How far below the quote you will still accept</legend>
        {TOLERANCES.map((tolerance) => (
          <label key={tolerance.bps} title={tolerance.note}>
            <input
              type="radio"
              name={`tolerance-${market.market_id}`}
              value={String(tolerance.bps)}
              checked={props.toleranceBps === tolerance.bps}
              onChange={() => {
                props.onTolerance(tolerance.bps);
              }}
            />
            <span>{tolerance.label}</span>
          </label>
        ))}
      </fieldset>
      <p className="field-note">
        {TOLERANCES.find((tolerance) => tolerance.bps === props.toleranceBps)?.note ?? ""}
      </p>

      {props.error !== undefined && (
        <TradeRefusal what="This order" error={props.error} onRetry={props.onRequote} />
      )}

      {expired ? (
        <div className="form-actions">
          <Button variant="primary" fullWidth onClick={props.onRequote}>
            Take a new quote
          </Button>
        </div>
      ) : (
        <div className="form-actions">
          <Button
            variant="primary"
            fullWidth
            busy={props.pending}
            busyLabel="Placing…"
            onClick={props.onConfirm}
          >
            {props.side === "BUY" ? "Buy with Credits" : "Sell for Credits"}
          </Button>
        </div>
      )}
    </div>
  );
}

/** What actually happened. Every figure here is the fill's, not the quote's. */
function Filled(props: {
  readonly fill: NativeFill;
  readonly market: NativeMarketSummary;
  readonly side: Side;
  readonly minimum: string | undefined;
  readonly simulated: boolean;
  readonly onAgain: () => void;
}): ReactNode {
  const { fill, market } = props;
  const received = props.side === "BUY" ? fill.assets_out : fill.credits_out;
  const spent = props.side === "BUY" ? fill.credits_in : fill.assets_in;
  const receivedScale = props.side === "BUY" ? (fill.asset_decimals ?? market.asset_decimals) : CREDIT_DECIMALS;
  const spentScale = props.side === "BUY" ? CREDIT_DECIMALS : (fill.asset_decimals ?? market.asset_decimals);
  const met = received === undefined || props.minimum === undefined ? undefined : meetsMinimum(received, props.minimum);

  return (
    <Panel
      title="Filled"
      description="What the market actually did. The quote that preceded it is not shown, because it is not what happened."
      temp={props.simulated ? "simulated" : "economy"}
    >
      <FieldGrid columns={2}>
        <Field label="You received" emphasis>
          <Figure
            kind="units"
            value={received === undefined ? null : { base: received, scale: receivedScale }}
            symbol={props.side === "BUY" ? market.symbol : "Credits"}
            absent="not reported"
          />
        </Field>
        <Field label="You paid" emphasis>
          <Figure
            kind="units"
            value={spent === undefined ? null : { base: spent, scale: spentScale }}
            symbol={props.side === "BUY" ? "Credits" : market.symbol}
            absent="not reported"
          />
        </Field>
        <Field label="Effective price" note="In Credits, at this market's own price scale.">
          <Figure
            kind="money"
            value={
              fill.effective_price === undefined || fill.price_scale === undefined
                ? null
                : { decimal: fromBaseUnits(fill.effective_price, fill.price_scale) }
            }
            symbol="Credits"
            absent="not reported"
          />
        </Field>
        <Field label="Price impact">
          <Figure kind="bps" bps={fill.slippage_bps} absent="not reported" />
        </Field>
        <Field label="Platform fee">
          <Figure
            kind="money"
            value={fill.platform_fee === undefined ? null : { base: fill.platform_fee, scale: CREDIT_DECIMALS }}
            symbol="Credits"
            absent="not reported"
          />
        </Field>
        <Field label="Creator fee">
          <Figure
            kind="money"
            value={fill.creator_fee === undefined ? null : { base: fill.creator_fee, scale: CREDIT_DECIMALS }}
            symbol="Credits"
            absent="not reported"
          />
        </Field>
      </FieldGrid>

      {met === true && (
        <p className="field-note">
          The fill met the minimum you set. Your Credits and your position are read from the ledger
          again, not adjusted in this browser.
        </p>
      )}

      {fill.alerts !== undefined && fill.alerts.length > 0 && (
        <div className="explain explain-flat" role="status">
          <p className="explain-title">This trade raised a surveillance finding</p>
          <p className="explain-body">
            Findings are recorded against the market and never block a trade. They are shown here
            because a market that raises them is a market worth reading carefully.
          </p>
          <ul className="explain-fields">
            {fill.alerts.map((alert, index) => (
              <li key={`${alert.kind ?? "alert"}-${String(index)}`}>
                <span className="mono-small">{alert.severity ?? "INFO"}</span> {alert.reason ?? alert.kind ?? ""}
              </li>
            ))}
          </ul>
        </div>
      )}

      <p className="field-note">
        Recorded at state version {String(fill.state_version_after)}. Fill{" "}
        <span className="mono-small">{fill.fill_id}</span>.
      </p>

      <div className="form-actions">
        <Button variant="secondary" onClick={props.onAgain}>
          Place another order
        </Button>
      </div>

      <Disclosure title="What you just traded">
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>
    </Panel>
  );
}
