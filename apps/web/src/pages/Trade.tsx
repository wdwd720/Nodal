/**
 * TRADE (PART 111): instrument, exact mint identity and details, current quote,
 * expected receive, fees, price impact, slippage, amount, risk and error
 * messages, order state.
 *
 * Two structural decisions:
 *
 *   1. The quote is requested when the customer asks for it, and every figure
 *      in the disclosure comes from that one response. If the backend cannot
 *      price the trade — in this deployment it answers PROVIDER_UNAVAILABLE
 *      because no execution venue adapter is configured — the panel says so and
 *      shows nothing. It does not fall back to zeros, dashes or a last-known
 *      price, because those read like a price.
 *   2. Submitting is a separate, explicit act with its own idempotency key,
 *      created once and reused for every retry so a double click cannot place
 *      two orders.
 */
import { useMemo, useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useAssets,
  useCancelIntent,
  useInstrument,
  useInstruments,
  useIntent,
  useQuotePreview,
  useSubmitIntent,
  type Asset,
  type InstrumentDetail,
  type QuoteDisclosure,
  type TradeIntentDetail,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState, Explanation } from "../components/DataState.tsx";
import { Button } from "../components/Button.tsx";
import {
  AsOf,
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  Page,
  Panel,
  Pill,
  Table,
  type Tone,
} from "../components/Layout.tsx";
import { AssetIdentity } from "../components/MintIdentity.tsx";
import { Bps, DecimalValue, Qty, Usd } from "../components/Money.tsx";
import { MODE_DESCRIPTIONS, USDC_DISCLOSURE, modeBadge } from "../lib/honesty.ts";
import { parseUsdAmountInput } from "../lib/money.ts";
import { formatInstant, isPast } from "../lib/time.ts";
import { useActiveAccountId } from "../session.tsx";

/** Slippage choices in whole basis points; the contract's unit, not a percentage typed by hand. */
const SLIPPAGE_CHOICES = [10, 25, 50, 100, 200] as const;

const TERMINAL_INTENT_STATES = new Set([
  "COMPLETED",
  "REJECTED",
  "EXPIRED",
  "CANCELLED",
  "FAILED",
  "NO_VALID_PLAN",
]);

function intentTone(status: string): Tone {
  if (status === "COMPLETED") return "good";
  if (["REJECTED", "EXPIRED", "CANCELLED", "FAILED", "NO_VALID_PLAN"].includes(status)) return "bad";
  return "info";
}

export function Trade(): ReactNode {
  const accountId = useActiveAccountId();
  const instruments = useInstruments();
  const assets = useAssets();

  const [instrumentId, setInstrumentId] = useState("");
  const selectedId = instrumentId !== "" ? instrumentId : (instruments.data?.[0]?.id ?? "");
  const detail = useInstrument(selectedId === "" ? undefined : selectedId);

  const [amount, setAmount] = useState("");
  const [slippageBps, setSlippageBps] = useState<number>(SLIPPAGE_CHOICES[1]);
  const [mode, setMode] = useState<"PAPER" | "LIVE">("PAPER");
  const [confirmLive, setConfirmLive] = useState(false);
  const [idempotencyKey, setIdempotencyKey] = useState(() => newIdempotencyKey());
  const [submittedIntentId, setSubmittedIntentId] = useState<string | undefined>(undefined);

  const quote = useQuotePreview();
  const submit = useSubmitIntent();
  const parsed = parseUsdAmountInput(amount);

  const assetById = useMemo(() => {
    const map = new Map<string, Asset>();
    for (const asset of assets.data ?? []) map.set(asset.id, asset);
    return map;
  }, [assets.data]);

  if (accountId === undefined) {
    return (
      <Page title="Trade">
        <EmptyState title="No account" body="The backend returned no accounts for this session." />
      </Page>
    );
  }

  const readyToQuote = parsed.ok && selectedId !== "";
  const liveBlocked = mode === "LIVE" && !confirmLive;

  const requestQuote = (): void => {
    if (!readyToQuote) return;
    quote.mutate({
      accountId,
      instrumentId: selectedId,
      action: "ACQUIRE_NOTIONAL",
      notionalUsd: parsed.value,
      maxSlippageBps: slippageBps,
    });
  };

  const placeIntent = (): void => {
    if (!readyToQuote || liveBlocked) return;
    submit.mutate(
      {
        accountId,
        instrumentId: selectedId,
        action: "ACQUIRE_NOTIONAL",
        notionalUsd: parsed.value,
        maxSlippageBps: slippageBps,
        mode,
        idempotencyKey,
        ...(quote.data === undefined ? {} : { quoteId: quote.data.quote_id }),
      },
      {
        onSuccess: (intent) => {
          setSubmittedIntentId(intent.id);
        },
      },
    );
  };

  return (
    <Page
      title="Trade"
      lead="Every number on this screen comes from a backend response. Nothing is estimated in the browser."
    >
      <Panel title="Instrument" description="What you would be trading, down to the mint.">
        <AsyncPanel
          query={instruments}
          loadingLabel="Loading tradable instruments…"
          empty={{
            isEmpty: (list) => list.length === 0,
            title: "No instruments are listed",
            body: "The backend returned an empty instrument list, so there is nothing to trade in this deployment.",
          }}
        >
          {(list) => (
            <div className="form-row">
              <label htmlFor="instrument">Instrument</label>
              <select
                id="instrument"
                className="input"
                value={selectedId}
                onChange={(event) => {
                  setInstrumentId(event.target.value);
                  quote.reset();
                  submit.reset();
                  setSubmittedIntentId(undefined);
                }}
              >
                {list.map((instrument) => (
                  <option key={instrument.id} value={instrument.id}>
                    {instrument.canonical_name} — {instrument.type} — {instrument.status}
                  </option>
                ))}
              </select>
            </div>
          )}
        </AsyncPanel>

        <AsyncPanel query={detail} loadingLabel="Loading instrument details…">
          {(data: InstrumentDetail) => <InstrumentFacts detail={data} />}
        </AsyncPanel>
      </Panel>

      <Panel title="Amount and constraints" description="What you are committing and what you will tolerate.">
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            requestQuote();
          }}
        >
          <div className="form-row">
            <label htmlFor="notional">Amount to commit, in US dollars of value</label>
            <input
              id="notional"
              className="input"
              inputMode="decimal"
              autoComplete="off"
              value={amount}
              aria-describedby="notional-help"
              {...(parsed.ok || amount === "" ? {} : { "aria-invalid": true })}
              onChange={(event) => {
                setAmount(event.target.value);
                quote.reset();
              }}
            />
            <p id="notional-help" className="field-note">
              {amount === ""
                ? "Sent to the backend as a decimal string, exactly as typed. It is never converted to a number here."
                : parsed.ok
                  ? `Will be sent as ${parsed.value}`
                  : parsed.error}
            </p>
          </div>

          <div className="form-row">
            <label htmlFor="slippage">Maximum slippage you will accept</label>
            <select
              id="slippage"
              className="input"
              value={String(slippageBps)}
              onChange={(event) => {
                const chosen = SLIPPAGE_CHOICES.find((bps) => String(bps) === event.target.value);
                if (chosen !== undefined) setSlippageBps(chosen);
                quote.reset();
              }}
            >
              {SLIPPAGE_CHOICES.map((bps) => (
                <option key={bps} value={String(bps)}>
                  {String(bps)} bps
                </option>
              ))}
            </select>
            <p className="field-note">
              Sent as an integer basis-point constraint. The backend enforces it; this is not a
              display preference.
            </p>
          </div>

          <fieldset className="form-row">
            <legend>Mode</legend>
            {(["PAPER", "LIVE"] as const).map((option) => (
              <label className="radio" key={option}>
                <input
                  type="radio"
                  name="mode"
                  value={option}
                  checked={mode === option}
                  onChange={() => {
                    setMode(option);
                    setConfirmLive(false);
                    submit.reset();
                  }}
                />
                <span>
                  <strong>{modeBadge(option)}</strong>
                  <span className="field-note">{MODE_DESCRIPTIONS[option]}</span>
                </span>
              </label>
            ))}
            {mode === "LIVE" && (
              <label className="checkbox">
                <input
                  type="checkbox"
                  checked={confirmLive}
                  onChange={(event) => {
                    setConfirmLive(event.target.checked);
                  }}
                />
                <span>
                  I understand this commits real capital. Manual live trading also requires the
                  LIVE_MANUAL_TRADING capability to be active; if it is not, the backend will refuse
                  this and say so.
                </span>
              </label>
            )}
          </fieldset>

          <div className="form-actions">
            {readyToQuote ? (
              <Button submit variant="secondary" busy={quote.isPending} busyLabel="Asking for a quote…">
                Get a quote
              </Button>
            ) : (
              <Button
                variant="secondary"
                disabledReason={
                  selectedId === ""
                    ? "Choose an instrument first."
                    : amount === ""
                      ? "Enter an amount to price."
                      : parsed.error
                }
              >
                Get a quote
              </Button>
            )}
          </div>
        </form>
      </Panel>

      <Panel
        title="Quote"
        description="Non-binding, fully disclosed, and only ever shown when the backend actually produced one."
      >
        {quote.isIdle && (
          <EmptyState
            title="No quote has been requested"
            body="Nothing is priced until you ask. There is no cached or indicative price behind this panel."
          />
        )}
        {quote.isPending && <p className="loading">Asking the backend to price this…</p>}
        {quote.isError && (
          <Explanation error={quote.error} onRetry={requestQuote} retryLabel="Ask again">
            <p>
              No price is being shown in place of the one the backend could not produce. A zero here
              would read like a free trade and a dash would read like a small number; neither is
              true.
            </p>
          </Explanation>
        )}
        {quote.isSuccess && <QuotePanel quote={quote.data} assetById={assetById} />}
      </Panel>

      <Panel title="Place the order" description="An explicit, idempotent command.">
        <FieldGrid columns={3}>
          <Field label="Mode">{modeBadge(mode)}</Field>
          <Field label="Amount">{parsed.ok ? parsed.value : "not set"}</Field>
          <Field label="Idempotency key" note="Created once for this attempt; reused on every retry.">
            <code className="mono-small">{idempotencyKey}</code>
          </Field>
        </FieldGrid>

        <div className="form-actions">
          {readyToQuote && !liveBlocked ? (
            <Button
              variant="primary"
              onClick={placeIntent}
              busy={submit.isPending}
              busyLabel="Submitting the intent…"
            >
              Submit {mode === "LIVE" ? "live" : "simulated"} order
            </Button>
          ) : (
            <Button
              variant="primary"
              disabledReason={
                selectedId === ""
                  ? "Choose an instrument first."
                  : !parsed.ok
                    ? amount === ""
                      ? "Enter an amount."
                      : parsed.error
                    : "Confirm you understand this commits real capital."
              }
            >
              Submit {mode === "LIVE" ? "live" : "simulated"} order
            </Button>
          )}
          <Button
            variant="quiet"
            onClick={() => {
              setIdempotencyKey(newIdempotencyKey());
              setSubmittedIntentId(undefined);
              submit.reset();
              quote.reset();
            }}
          >
            Start a new order
          </Button>
        </div>

        <p className="note">
          A quote is not required to submit: the planner re-quotes and re-validates freshness at
          execution time, so the price you were shown is a disclosure rather than a commitment. When
          a quote is present its id is sent along so the backend can compare what you saw against
          what it gets.
        </p>

        {submit.isError && (
          <Explanation error={submit.error}>
            <p>
              No intent was created. Retrying uses the same idempotency key, so a request the backend
              already accepted cannot be applied twice.
            </p>
          </Explanation>
        )}
      </Panel>

      {submittedIntentId !== undefined && (
        <OrderState intentId={submittedIntentId} accountId={accountId} assetById={assetById} />
      )}

      <Disclosure title="What settles">
        <p>{USDC_DISCLOSURE}</p>
      </Disclosure>
    </Page>
  );
}

function InstrumentFacts(props: { readonly detail: InstrumentDetail }): ReactNode {
  const { detail } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Canonical name">{detail.canonical_name}</Field>
        <Field label="Type">{detail.type}</Field>
        <Field label="Status">
          <Pill tone={detail.status === "ACTIVE" ? "good" : "warn"}>{detail.status}</Pill>
        </Field>
        <Field label="Risk class">{detail.risk_class}</Field>
        <Field label="Instrument id">
          <Identifier value={detail.id} />
        </Field>
        <Field label="Settlement asset">
          <Identifier value={detail.settlement_asset} />
        </Field>
      </FieldGrid>

      <h3>Base asset</h3>
      <AssetIdentity asset={detail.base} />
      <h3>Quote asset</h3>
      <AssetIdentity asset={detail.quote} />

      <h3>Venue listings</h3>
      {detail.listings.length === 0 ? (
        <EmptyState
          title="No venue listings"
          body="The backend returned no venue listing for this instrument, so there is nowhere for an order to go."
        />
      ) : (
        <Table
          caption="Venue listings"
          headers={["Venue", "Venue status", "Listing status", "Network", "Base mint", "Quote mint", "Minimum notional"]}
        >
          {detail.listings.map((listing) => (
            <tr key={listing.id}>
              <th scope="row">{listing.venue}</th>
              <td>
                <Pill tone={listing.venue_status === "ACTIVE" ? "good" : "warn"}>
                  {listing.venue_status}
                </Pill>
              </td>
              <td>
                <Pill tone={listing.status === "ACTIVE" ? "good" : "warn"}>{listing.status}</Pill>
              </td>
              <td>{listing.network}</td>
              <td>
                <code className="mono-small">{listing.base_mint ?? "not reported"}</code>
              </td>
              <td>
                <code className="mono-small">{listing.quote_mint ?? "not reported"}</code>
              </td>
              <td>
                <Qty value={listing.min_notional_quote} decimals={listing.quote_precision} />
              </td>
            </tr>
          ))}
        </Table>
      )}
    </>
  );
}

function QuotePanel(props: {
  readonly quote: QuoteDisclosure;
  readonly assetById: ReadonlyMap<string, Asset>;
}): ReactNode {
  const { quote, assetById } = props;
  const input = assetById.get(quote.input_asset);
  const output = assetById.get(quote.output_asset);
  const venueFeeAsset = quote.venue_fee_asset === undefined ? undefined : assetById.get(quote.venue_fee_asset);
  const networkFeeAsset =
    quote.network_fee_asset === undefined ? undefined : assetById.get(quote.network_fee_asset);
  const expired = isPast(quote.expires_at);

  return (
    <>
      {expired && (
        <p className="banner banner-warn" role="status">
          This quote passed its expiry at {formatInstant(quote.expires_at)}. It is shown for the
          record; submitting will make the backend re-quote.
        </p>
      )}
      <FieldGrid columns={3}>
        <Field label="You send">
          <Qty value={quote.input_quantity} decimals={input?.decimals} symbol={input?.symbol ?? ""} />
        </Field>
        <Field label="Expected receive" emphasis note="The backend's estimate for this route right now.">
          <Qty value={quote.expected_output} decimals={output?.decimals} symbol={output?.symbol ?? ""} />
        </Field>
        <Field
          label="Minimum receive"
          emphasis
          note="The floor your slippage constraint sets. Below this the trade does not execute."
        >
          <Qty value={quote.minimum_output} decimals={output?.decimals} symbol={output?.symbol ?? ""} />
        </Field>
        <Field label="Effective price" note="Quote asset per unit of base, as the backend computed it.">
          <DecimalValue value={quote.effective_price} />
        </Field>
        <Field label="Price impact">
          <Bps value={quote.price_impact_bps} />
        </Field>
        <Field label="Slippage allowance">
          <Bps value={quote.slippage_bps} />
        </Field>
      </FieldGrid>

      <h3>Fees</h3>
      <Table caption="Fee disclosure" headers={["Fee", "Amount", "Asset", "Notes"]}>
        <tr>
          <th scope="row">Venue fee</th>
          <td>
            <Qty value={quote.venue_fee} decimals={venueFeeAsset?.decimals} />
          </td>
          <td>{venueFeeAsset?.symbol ?? "not reported"}</td>
          <td className="cell-prose">Charged by {quote.venue}.</td>
        </tr>
        <tr>
          <th scope="row">Network fee estimate</th>
          <td>
            <Qty value={quote.network_fee_estimate} decimals={networkFeeAsset?.decimals} />
          </td>
          <td>{networkFeeAsset?.symbol ?? "not reported"}</td>
          <td className="cell-prose">An estimate. The chain decides the actual cost.</td>
        </tr>
        <tr>
          <th scope="row">Platform fee</th>
          <td>
            <Qty value={quote.platform_fee} decimals={output?.decimals} />
          </td>
          <td>{output?.symbol ?? "not reported"}</td>
          <td className="cell-prose">
            <Bps value={quote.platform_fee_bps} />
            {quote.fee_policy_version === undefined ? null : (
              <span className="mono-small"> · policy {quote.fee_policy_version}</span>
            )}
          </td>
        </tr>
        <tr>
          <th scope="row">Total estimated cost</th>
          <td>
            <Usd value={quote.total_estimated_cost_usd} />
          </td>
          <td>USD valuation</td>
          <td className="cell-prose">Every fee above, valued in US dollars by the backend.</td>
        </tr>
      </Table>

      <FieldGrid columns={3}>
        <Field label="Provider">{quote.provider}</Field>
        <Field label="Venue">{quote.venue}</Field>
        <Field label="Quote id">
          <Identifier value={quote.quote_id} />
        </Field>
      </FieldGrid>
      <AsOf at={quote.received_at} label="Quoted at" />
      <p className="note">Expires {formatInstant(quote.expires_at)}.</p>
      {quote.route !== undefined && quote.route.length > 0 && (
        <details className="raw">
          <summary>Route the backend returned</summary>
          <pre>{JSON.stringify(quote.route, null, 2)}</pre>
        </details>
      )}
    </>
  );
}

/** Live state of the submitted intent, polled from the authoritative REST resource. */
function OrderState(props: {
  readonly intentId: string;
  readonly accountId: string;
  readonly assetById: ReadonlyMap<string, Asset>;
}): ReactNode {
  const intent = useIntent(props.intentId, { refetchMs: 3000 });
  const cancel = useCancelIntent();
  const [cancelKey] = useState(() => newIdempotencyKey());

  return (
    <Panel title="Order state" description="Polled from the intent resource, which is the authoritative record.">
      <AsyncPanel query={intent} loadingLabel="Loading the intent…">
        {(data: TradeIntentDetail) => {
          const terminal = TERMINAL_INTENT_STATES.has(data.status);
          return (
            <>
              <FieldGrid columns={3}>
                <Field label="Status" emphasis>
                  <Pill tone={intentTone(data.status)}>{data.status}</Pill>
                </Field>
                <Field label="Mode">{modeBadge(data.mode)}</Field>
                <Field label="Action">{data.action}</Field>
                <Field label="Intent id">
                  <Identifier value={data.id} />
                </Field>
                <Field label="Correlation id" note="Ties every row on the activity timeline together.">
                  <Identifier value={data.correlation_id} />
                </Field>
                <Field label="Order id">
                  <Identifier value={data.order_id} />
                </Field>
                <Field label="Requested at">{formatInstant(data.requested_at)}</Field>
                <Field label="Received at">{formatInstant(data.received_at)}</Field>
                <Field label="Terminal at">
                  {data.terminal_at === undefined ? (
                    <span className="absent">still in flight</span>
                  ) : (
                    formatInstant(data.terminal_at)
                  )}
                </Field>
              </FieldGrid>

              {data.rejection_code !== undefined && (
                <p className="banner banner-bad" role="status">
                  The backend rejected this intent with code{" "}
                  <span className="mono-small">{data.rejection_code}</span>.
                </p>
              )}

              <h3>Decisions</h3>
              {data.eligibility === undefined && data.risk === undefined ? (
                <EmptyState
                  title="No decisions recorded yet"
                  body="The backend has not attached an eligibility or risk decision to this intent. That is an absence of a record, not an approval."
                />
              ) : (
                <Table caption="Decisions" headers={["Stage", "Decision", "Policy", "Reasons", "Evaluated"]}>
                  {([["Eligibility", data.eligibility], ["Risk", data.risk]] as const).map(
                    ([label, decision]) =>
                      decision === undefined ? null : (
                        <tr key={label}>
                          <th scope="row">{label}</th>
                          <td>
                            <Pill tone={decision.decision === "ALLOW" ? "good" : "bad"}>
                              {decision.decision}
                            </Pill>
                          </td>
                          <td className="mono-small">{decision.policy_version}</td>
                          <td>{decision.reason_codes.join(", ")}</td>
                          <td>{formatInstant(decision.evaluated_at)}</td>
                        </tr>
                      ),
                  )}
                </Table>
              )}

              {data.order !== undefined && (
                <>
                  <h3>Order</h3>
                  <FieldGrid columns={3}>
                    <Field label="Order status">
                      <Pill tone="info">{data.order.status}</Pill>
                    </Field>
                    <Field label="Side">{data.order.side}</Field>
                    <Field label="Filled in">
                      <Qty
                        value={data.order.filled_input_quantity}
                        decimals={props.assetById.get(data.order.input_asset)?.decimals}
                        symbol={props.assetById.get(data.order.input_asset)?.symbol ?? ""}
                      />
                    </Field>
                    <Field label="Filled out">
                      <Qty
                        value={data.order.filled_output_quantity}
                        decimals={props.assetById.get(data.order.output_asset)?.decimals}
                        symbol={props.assetById.get(data.order.output_asset)?.symbol ?? ""}
                      />
                    </Field>
                    <Field label="Minimum output">
                      <Qty
                        value={data.order.min_output_quantity}
                        decimals={props.assetById.get(data.order.output_asset)?.decimals}
                      />
                    </Field>
                    <Field label="Created">{formatInstant(data.order.created_at)}</Field>
                  </FieldGrid>
                </>
              )}

              {data.transitions !== undefined && data.transitions.length > 0 && (
                <>
                  <h3>Transitions</h3>
                  <Table caption="State transitions" headers={["From", "To", "Reason", "When"]}>
                    {data.transitions.map((transition) => (
                      <tr key={`${transition.from}-${transition.to}-${transition.occurred_at}`}>
                        <td>{transition.from}</td>
                        <td>{transition.to}</td>
                        <td>{transition.reason ?? "not reported"}</td>
                        <td>{formatInstant(transition.occurred_at)}</td>
                      </tr>
                    ))}
                  </Table>
                </>
              )}

              <div className="form-actions">
                {terminal ? (
                  <Button
                    variant="danger"
                    disabledReason={`This intent is already ${data.status}; there is nothing left to cancel.`}
                  >
                    Request cancellation
                  </Button>
                ) : (
                  <Button
                    variant="danger"
                    busy={cancel.isPending}
                    busyLabel="Requesting cancellation…"
                    onClick={() => {
                      cancel.mutate({
                        intentId: data.id,
                        accountId: props.accountId,
                        idempotencyKey: cancelKey,
                      });
                    }}
                  >
                    Request cancellation
                  </Button>
                )}
              </div>
              <p className="note">
                Cancellation is a request. The intent moves to CANCEL_REQUESTED and only external
                confirmation from the venue can make it CANCELLED, so this button never claims to
                have stopped anything on its own.
              </p>
              {cancel.isError && <Explanation error={cancel.error} />}
            </>
          );
        }}
      </AsyncPanel>
    </Panel>
  );
}
