/**
 * TREASURY — the operator's commands on one house book.
 *
 * Every command is reviewed, then confirmed; the idempotency key is created
 * at confirmation and reused by every retry of the same request. The
 * treasury decides each one under the book's lock — the signing policy (an
 * operator may trade only where it allows), the capabilities and kill
 * switches, the risk limits and Risk Kernel V2 — and a refusal is shown with
 * the backend's code and the treasury's reason codes. Anything that moves
 * simulated value or changes a policy asks for a recent strong sign-in.
 *
 * Quantities are typed in base units of the asset named, exactly; prices are
 * plain decimals in the book's valuation asset, sent exactly as typed.
 */
import { useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";

import {
  useCancelHouseOrder,
  useConvert,
  useFundBook,
  useHedge,
  useMarkBookToMarket,
  usePaperFill,
  usePlaceHouseOrder,
  useQuoteTwoSided,
  useRebalance,
  useSetRiskPolicy,
  useSetSigningPolicy,
  useSyncBook,
  useTreasuryBook,
  type BookOnlyRequest,
  type BookRequest,
  type CancelRequest,
  type TreasuryBookDetail,
  type TreasuryConvertRequest,
  type TreasuryFundRequest,
  type TreasuryFunding,
  type TreasuryHedgeRequest,
  type TreasuryOrderOutcome,
  type TreasuryOrderRequest,
  type TreasuryPaperOutcome,
  type TreasuryPaperRequest,
  type TreasuryQuoteOutcome,
  type TreasuryQuoteRequest,
  type TreasuryRebalanceOutcome,
  type TreasuryRebalanceRequest,
  type TreasuryRiskPolicy,
  type TreasuryRiskPolicyRequest,
  type TreasurySigningPolicy,
  type TreasurySigningPolicyRequest,
  type TreasurySnapshot,
  type TreasurySyncResult,
} from "../../../api/advanced/treasury.ts";
import { useAssets } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { Bps } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { simulatedAssets } from "../../../lib/margin.ts";
import {
  QUOTING_ACTIVITIES,
  STRATEGY_MEANING,
  TREASURY_ORDER_TYPES,
  TREASURY_RUN_MODES,
  TREASURY_SIDES,
  TREASURY_TIMES_IN_FORCE,
  parseBaseUnits,
  parseDecimalPrice,
  parseLimitLines,
  parseText,
  parseUuid,
  parseWholeNumber,
  type TreasuryOrderType,
  type TreasuryRunMode,
  type TreasurySide,
  type TreasuryStrategy,
  type TreasuryTimeInForce,
} from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { Choice, CommandFlow, CommandForm, TextArea, TextInput, Toggle, prepare, type Prepared } from "./command.tsx";
import {
  AssetAmount,
  ActionSummary,
  BookNav,
  DecisionView,
  FundingView,
  OrderOutcomeView,
  PaperFillView,
  PaperOutcomeView,
  QuoteLegView,
  TreasuryNav,
} from "./parts.tsx";

const ASSET_LIST = "treasury-operate-assets";

export function BookOperatePage(): ReactNode {
  const { bookId = "" } = useParams();
  const book = useTreasuryBook(bookId);
  return (
    <Page
      title="Operate a house book"
      lead="The operator's commands on one house book. Each is reviewed and confirmed, and the treasury decides it under the book's signing policy, limits and Risk Kernel V2."
    >
      <SimulatedNotice />
      <TreasuryNav />
      <BookNav bookId={bookId} />
      <AssetOptions />
      <AsyncPanel query={book} loadingLabel="Loading the house book…">
        {(detail: TreasuryBookDetail) => (
          <>
            <RegisterUpkeep book={detail} />
            <SigningPolicyPanel book={detail} />
            <RiskPolicyPanel book={detail} />
            <FundPanel book={detail} />
            <OrderPanel book={detail} />
            <CancelPanel book={detail} />
            <QuotePanel book={detail} />
            <HedgePanel book={detail} />
            <ConvertPanel book={detail} />
            <RebalancePanel book={detail} />
            <PaperFillPanel book={detail} />
          </>
        )}
      </AsyncPanel>
    </Page>
  );
}

/** The registry's simulated assets, offered as suggestions in every asset field. */
function AssetOptions(): ReactNode {
  const assets = useAssets();
  return (
    <datalist id={ASSET_LIST}>
      {simulatedAssets(assets.data ?? []).map((asset) => (
        <option key={asset.id} value={asset.id}>
          {asset.symbol}
        </option>
      ))}
    </datalist>
  );
}

const problem = (parsed: { readonly ok: boolean; readonly error?: string }): string | undefined =>
  parsed.ok ? undefined : (parsed.error ?? "This field is not valid.");

/** Base units as typed, and, for the valuation asset, the amount they are. */
function Units(props: { readonly value: string; readonly assetId?: string; readonly book: TreasuryBookDetail }): ReactNode {
  const valuation = props.book.valuation_asset;
  return (
    <>
      <code className="mono-small">{props.value} base units</code>
      {props.assetId !== undefined && props.assetId === valuation.asset_id && (
        <span className="cell-note">
          <AssetAmount value={props.value} asset={valuation} />
        </span>
      )}
    </>
  );
}

/* --------------------------------------------------------------------------
 * Keeping the register current
 * ------------------------------------------------------------------------ */

function RegisterUpkeep(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const sync = useSyncBook();
  const mark = useMarkBookToMarket();
  const request: Prepared<BookOnlyRequest> = { ok: true, request: { bookId: props.book.house_account_id } };
  return (
    <Panel
      title="Keep the register current"
      description="Syncing ingests every internal-book trade the position register has not recorded, exactly once. Marking to market stores the PnL report at fresh marks as a hashed snapshot. Neither moves value, so neither needs a step-up."
    >
      <CommandFlow<BookOnlyRequest, TreasurySyncResult>
        label="Sync the position register"
        prepared={request}
        command={sync}
        refusedNote="Nothing was ingested."
        describe={() => <p>Ingest every internal-book trade of {props.book.name} not yet in its register.</p>}
        renderResult={(result) => (
          <p data-field="ingested">
            {String(result.ingested)} trade{result.ingested === 1 ? "" : "s"} ingested.
          </p>
        )}
      />
      <CommandFlow<BookOnlyRequest, TreasurySnapshot>
        label="Mark to market"
        prepared={request}
        command={mark}
        refusedNote="No snapshot was stored."
        describe={() => <p>Store {props.book.name}'s PnL report at fresh marks as a snapshot.</p>}
        renderResult={(snapshot) => (
          <>
            <FieldGrid columns={3}>
              <Field label="Snapshot">
                <Identifier value={snapshot.id} />
              </Field>
              <Field label="Document hash">
                <span className="mono-small">{snapshot.document_hash}</span>
              </Field>
              <Field label="Loss measure" note="In the valuation asset.">
                <AssetAmount value={snapshot.report.loss_measure} asset={snapshot.report.valuation_asset} />
              </Field>
              <Field label="Realized, all time">
                <AssetAmount value={snapshot.report.realized_total} asset={snapshot.report.valuation_asset} />
              </Field>
              <Field label="Unrealized">
                <AssetAmount value={snapshot.report.unrealized_total} asset={snapshot.report.valuation_asset} />
              </Field>
              <Field label="Stated at">{formatInstant(snapshot.report.at)}</Field>
            </FieldGrid>
            <ActionSummary action={snapshot.action} />
          </>
        )}
      />
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Policies
 * ------------------------------------------------------------------------ */

function SigningPolicyPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useSetSigningPolicy();
  const current = props.book.signing_policy;
  const [service, setService] = useState(current.service_actor_id);
  const [operators, setOperators] = useState(current.operators_allowed);
  const [reason, setReason] = useState("");
  const s = parseText(service, "The house service id", 100);
  const r = parseText(reason, "The reason", 500);
  const prepared = prepare<BookRequest<TreasurySigningPolicyRequest>>([problem(s), problem(r)], () => ({
    bookId: props.book.house_account_id,
    body: { service_actor_id: s.ok ? s.value : "", operators_allowed: operators, reason: r.ok ? r.value : "" },
  }));
  return (
    <Panel
      title="Who may act for the book"
      description={`Records the next version of the book's signing policy (now v${String(current.version)}): the one house service that may act for it, and whether operators may trade. No keys are held. It changes a policy, so the backend asks for a recent strong sign-in.`}
    >
      <CommandForm label="Set who may act for the book">
        <TextInput id="treasury-signing-service" label="House service id" value={service} onChange={setService} />
        <Toggle
          id="treasury-signing-operators"
          label="Operators may place orders and fills"
          checked={operators}
          onChange={setOperators}
        />
        <TextInput id="treasury-signing-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<BookRequest<TreasurySigningPolicyRequest>, TreasurySigningPolicy>
          label="Record who may act for the book"
          prepared={prepared}
          command={command}
          refusedNote="Who may act for the book is unchanged."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="House service">
                <code className="mono-small">{request.body.service_actor_id}</code>
              </Field>
              <Field label="Operators may trade">{request.body.operators_allowed ? "yes" : "no"}</Field>
              <Field label="Reason">{request.body.reason}</Field>
            </FieldGrid>
          )}
          renderResult={(policy) => (
            <FieldGrid columns={3}>
              <Field label="Version">v{String(policy.version)}</Field>
              <Field label="Operators may trade">{policy.operators_allowed ? "yes" : "no"}</Field>
              <Field label="Hash">
                <span className="mono-small">{policy.policy_hash}</span>
              </Field>
            </FieldGrid>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

function RiskPolicyPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useSetRiskPolicy();
  const valuation = props.book.valuation_asset;
  const [label, setLabel] = useState("");
  const [lossLimit, setLossLimit] = useState("");
  const [markAge, setMarkAge] = useState("30000");
  const [spread, setSpread] = useState("0");
  const [fee, setFee] = useState("0");
  const [inventory, setInventory] = useState("");
  const [exposure, setExposure] = useState("");
  const [reason, setReason] = useState("");

  const l = parseText(label, "The label", 100);
  const loss = parseBaseUnits(lossLimit, "The daily loss limit");
  const age = parseWholeNumber(markAge, 1, 86400000, "The stalest mark, in milliseconds,");
  const sp = parseWholeNumber(spread, 0, 9999, "The paper spread");
  const fe = parseWholeNumber(fee, 0, 10000, "The paper fee");
  const inv = parseLimitLines(inventory, "Inventory caps");
  const exp = parseLimitLines(exposure, "Exposure caps");
  const r = parseText(reason, "The reason", 500);
  const prepared = prepare<BookRequest<TreasuryRiskPolicyRequest>>(
    [problem(l), problem(loss), problem(age), problem(sp), problem(fe), problem(inv), problem(exp), problem(r)],
    () => ({
      bookId: props.book.house_account_id,
      body: {
        label: l.ok ? l.value : "",
        daily_loss_limit: loss.ok ? loss.value : "",
        max_mark_age_ms: age.ok ? age.value : 0,
        paper_spread_bps: sp.ok ? sp.value : 0,
        paper_fee_bps: fe.ok ? fe.value : 0,
        max_inventory: inv.ok ? inv.value.map((line) => ({ asset_id: line.id, max_quantity: line.max })) : [],
        max_instrument_exposure: exp.ok ? exp.value.map((line) => ({ instrument_id: line.id, max_exposure: line.max })) : [],
        reason: r.ok ? r.value : "",
      },
    }),
  );
  return (
    <Panel
      title="Risk limits"
      description="Records the next version of the book's limits, stated in its valuation asset. An asset without an inventory cap cannot be accumulated and an instrument without an exposure cap cannot be bought through: the limits fail closed. It changes a policy, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Set the risk limits">
        <TextInput id="treasury-policy-label" label="Label" value={label} onChange={setLabel} />
        <TextInput
          id="treasury-policy-loss"
          label="Daily loss limit, in base units of the valuation asset"
          value={lossLimit}
          onChange={setLossLimit}
          inputMode="numeric"
          note={`${valuation.symbol} has ${String(valuation.decimals)} decimals.`}
        />
        <TextInput id="treasury-policy-age" label="Stalest mark accepted, in milliseconds" value={markAge} onChange={setMarkAge} inputMode="numeric" />
        <TextInput id="treasury-policy-spread" label="Paper venue spread, in basis points" value={spread} onChange={setSpread} inputMode="numeric" />
        <TextInput id="treasury-policy-fee" label="Paper venue fee, in basis points" value={fee} onChange={setFee} inputMode="numeric" />
        <TextArea
          id="treasury-policy-inventory"
          label="Inventory caps: one asset id and its cap in base units per line"
          value={inventory}
          onChange={setInventory}
        />
        <TextArea
          id="treasury-policy-exposure"
          label="Instrument exposure caps: one instrument id and its cap in base units of the valuation asset per line"
          value={exposure}
          onChange={setExposure}
        />
        <TextInput id="treasury-policy-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<BookRequest<TreasuryRiskPolicyRequest>, TreasuryRiskPolicy>
          label="Record the risk limits"
          prepared={prepared}
          command={command}
          refusedNote="The risk limits are unchanged."
          describe={(request) => (
            <>
              <FieldGrid columns={3}>
                <Field label="Label">{request.body.label}</Field>
                <Field label="Daily loss limit">
                  <AssetAmount value={request.body.daily_loss_limit} asset={valuation} />
                </Field>
                <Field label="Stalest mark">{String(request.body.max_mark_age_ms)} ms</Field>
                <Field label="Paper spread and fee">
                  <Bps value={request.body.paper_spread_bps} /> <Bps value={request.body.paper_fee_bps} />
                </Field>
                <Field label="Inventory caps">{String(request.body.max_inventory.length)} named</Field>
                <Field label="Exposure caps">{String(request.body.max_instrument_exposure.length)} named</Field>
              </FieldGrid>
              <p>Reason: {request.body.reason}</p>
            </>
          )}
          renderResult={(policy) => (
            <FieldGrid columns={3}>
              <Field label="Version">v{String(policy.version)}</Field>
              <Field label="Label">{policy.label}</Field>
              <Field label="Hash">
                <span className="mono-small">{policy.policy_hash}</span>
              </Field>
            </FieldGrid>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Capital
 * ------------------------------------------------------------------------ */

function FundPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useFundBook();
  const [asset, setAsset] = useState(props.book.valuation_asset.asset_id);
  const [quantity, setQuantity] = useState("");
  const [mode, setMode] = useState<TreasuryRunMode>("PAPER");
  const [reason, setReason] = useState("");
  const a = parseUuid(asset, "The asset id");
  const q = parseBaseUnits(quantity, "The quantity");
  const r = parseText(reason, "The reason", 500);
  const prepared = prepare<BookRequest<TreasuryFundRequest>>([problem(a), problem(q), problem(r)], () => ({
    bookId: props.book.house_account_id,
    body: { asset_id: a.ok ? a.value : "", quantity: q.ok ? q.value : "", mode, reason: r.ok ? r.value : "" },
  }));
  return (
    <Panel
      title="Fund the book"
      description="Injects simulated house capital (HOUSE_FUNDING). Any asset but the valuation asset enters the register at a fresh mark and is refused without one. It moves simulated value, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Fund the book">
        <TextInput id="treasury-fund-asset" label="Asset id" value={asset} onChange={setAsset} list={ASSET_LIST} />
        <TextInput id="treasury-fund-quantity" label="Quantity, in base units" value={quantity} onChange={setQuantity} inputMode="numeric" />
        <Choice id="treasury-fund-mode" label="Mode" value={mode} options={TREASURY_RUN_MODES} onChange={setMode} />
        <TextInput id="treasury-fund-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<BookRequest<TreasuryFundRequest>, TreasuryFunding>
          label="Fund the book"
          variant="danger"
          prepared={prepared}
          command={command}
          refusedNote="The book was not funded."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="Asset">
                <Identifier value={request.body.asset_id} />
              </Field>
              <Field label="Quantity">
                <Units value={request.body.quantity} assetId={request.body.asset_id} book={props.book} />
              </Field>
              <Field label="Mode">{request.body.mode ?? "PAPER"}</Field>
              <Field label="Reason">{request.body.reason}</Field>
            </FieldGrid>
          )}
          renderResult={(funding) => <FundingView funding={funding} />}
        />
      </CommandForm>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * The internal book
 * ------------------------------------------------------------------------ */

function OrderPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = usePlaceHouseOrder();
  const activities = props.book.permitted_activities;
  const [instrument, setInstrument] = useState("");
  const [activity, setActivity] = useState<TreasuryStrategy>(activities[0] ?? props.book.strategy);
  const [side, setSide] = useState<TreasurySide>("BUY");
  const [orderType, setOrderType] = useState<TreasuryOrderType>("LIMIT");
  const [tif, setTif] = useState<TreasuryTimeInForce>("GTC");
  const [price, setPrice] = useState("");
  const [quantity, setQuantity] = useState("");
  const [postOnly, setPostOnly] = useState(false);
  const [reduceOnly, setReduceOnly] = useState(false);
  const [clientOrderId, setClientOrderId] = useState("");
  const i = parseUuid(instrument, "The instrument id");
  const p = parseDecimalPrice(price, "The price");
  const q = parseBaseUnits(quantity, "The quantity");
  const prepared = prepare<BookRequest<TreasuryOrderRequest>>([problem(i), problem(p), problem(q)], () => ({
    bookId: props.book.house_account_id,
    body: {
      instrument_id: i.ok ? i.value : "",
      activity,
      side,
      order_type: orderType,
      time_in_force: tif,
      price: p.ok ? p.value : "",
      quantity: q.ok ? q.value : "",
      post_only: postOnly,
      reduce_only: reduceOnly,
      ...(clientOrderId.trim() === "" ? {} : { client_order_id: clientOrderId.trim() }),
    },
  }));
  return (
    <Panel
      title="Place a house order"
      description="An order of the house book on the internal book, after every treasury control. The order book then applies its own rules: it reserves house inventory only, and refuses a trade between a house book and a customer unless a disclosed crossing rule allows it. It commits house capital, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Place a house order">
        <TextInput id="treasury-order-instrument" label="Instrument id" value={instrument} onChange={setInstrument} />
        <Choice
          id="treasury-order-activity"
          label="Activity"
          value={activity}
          options={activities}
          onChange={setActivity}
          describe={(value) => STRATEGY_MEANING[value]}
        />
        <Choice id="treasury-order-side" label="Side" value={side} options={TREASURY_SIDES} onChange={setSide} />
        <Choice id="treasury-order-type" label="Order type" value={orderType} options={TREASURY_ORDER_TYPES} onChange={setOrderType} />
        <Choice id="treasury-order-tif" label="Time in force" value={tif} options={TREASURY_TIMES_IN_FORCE} onChange={setTif} />
        <TextInput
          id="treasury-order-price"
          label={`Price, in ${props.book.valuation_asset.symbol}`}
          value={price}
          onChange={setPrice}
          inputMode="decimal"
          note="The limit price, or a market order's protection price, which the book always requires."
        />
        <TextInput id="treasury-order-quantity" label="Quantity, in base units of the base asset" value={quantity} onChange={setQuantity} inputMode="numeric" />
        <Toggle id="treasury-order-post-only" label="Post-only" checked={postOnly} onChange={setPostOnly} />
        <Toggle id="treasury-order-reduce-only" label="Reduce-only" checked={reduceOnly} onChange={setReduceOnly} />
        <TextInput id="treasury-order-client-id" label="Client order id (optional)" value={clientOrderId} onChange={setClientOrderId} />
        <CommandFlow<BookRequest<TreasuryOrderRequest>, TreasuryOrderOutcome>
          label="Place the house order"
          variant="danger"
          prepared={prepared}
          command={command}
          refusedNote="No house order reached the book. A refusal is recorded in the book's action register."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="Instrument">
                <Identifier value={request.body.instrument_id} />
              </Field>
              <Field label="Activity">
                <code className="mono-small">{request.body.activity}</code>
              </Field>
              <Field label="Order">
                {request.body.side} {request.body.order_type} {request.body.time_in_force}
                {request.body.post_only === true && <span className="cell-note">post-only</span>}
                {request.body.reduce_only === true && <span className="cell-note">reduce-only</span>}
              </Field>
              <Field label="Price">
                <span className="num">
                  {request.body.price} {props.book.valuation_asset.symbol}
                </span>
              </Field>
              <Field label="Quantity">
                <code className="mono-small">{request.body.quantity} base units</code>
              </Field>
            </FieldGrid>
          )}
          renderResult={(outcome) => <OrderOutcomeView outcome={outcome} valuation={props.book.valuation_asset} />}
        />
      </CommandForm>
    </Panel>
  );
}

function CancelPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useCancelHouseOrder();
  const [orderId, setOrderId] = useState("");
  const o = parseUuid(orderId, "The order id");
  const prepared = prepare<CancelRequest>([problem(o)], () => ({ bookId: props.book.house_account_id, orderId: o.ok ? o.value : "" }));
  return (
    <Panel
      title="Cancel a house order"
      description="A cancel is never new risk: no capability, limit or kill switch stops it, and it moves no value, so no step-up is needed."
    >
      <CommandForm label="Cancel a house order">
        <TextInput id="treasury-cancel-order" label="Order id" value={orderId} onChange={setOrderId} />
        <CommandFlow<CancelRequest, TreasuryOrderOutcome>
          label="Cancel the house order"
          prepared={prepared}
          command={command}
          refusedNote="The order is unchanged."
          describe={(request) => (
            <p>
              Cancel order <code className="mono-small">{request.orderId}</code> of {props.book.name}.
            </p>
          )}
          renderResult={(outcome) => <OrderOutcomeView outcome={outcome} valuation={props.book.valuation_asset} />}
        />
      </CommandForm>
    </Panel>
  );
}

function QuotePanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useQuoteTwoSided();
  const allowed = QUOTING_ACTIVITIES.filter((a) => props.book.permitted_activities.includes(a));
  const [instrument, setInstrument] = useState("");
  const [activity, setActivity] = useState<(typeof QUOTING_ACTIVITIES)[number]>(allowed[0] ?? "MARKET_MAKING");
  const [bidPrice, setBidPrice] = useState("");
  const [bidQty, setBidQty] = useState("");
  const [askPrice, setAskPrice] = useState("");
  const [askQty, setAskQty] = useState("");
  const i = parseUuid(instrument, "The instrument id");
  const bp = parseDecimalPrice(bidPrice, "The bid price");
  const bq = parseBaseUnits(bidQty, "The bid quantity");
  const ap = parseDecimalPrice(askPrice, "The ask price");
  const aq = parseBaseUnits(askQty, "The ask quantity");
  const prepared = prepare<BookRequest<TreasuryQuoteRequest>>([problem(i), problem(bp), problem(bq), problem(ap), problem(aq)], () => ({
    bookId: props.book.house_account_id,
    body: {
      instrument_id: i.ok ? i.value : "",
      activity,
      bid_price: bp.ok ? bp.value : "",
      bid_quantity: bq.ok ? bq.value : "",
      ask_price: ap.ok ? ap.value : "",
      ask_quantity: aq.ok ? aq.value : "",
    },
  }));
  return (
    <Panel
      title="Quote two-sided"
      description="A post-only GTC bid and ask, each its own house action: the bid is evaluated first and the ask against it, and one leg can be refused while the other rests. The treasury refuses a bid that is not strictly below the ask."
    >
      <CommandForm label="Quote two-sided">
        <TextInput id="treasury-quote-instrument" label="Instrument id" value={instrument} onChange={setInstrument} />
        <Choice id="treasury-quote-activity" label="Activity" value={activity} options={QUOTING_ACTIVITIES} onChange={setActivity} />
        <TextInput id="treasury-quote-bid-price" label={`Bid price, in ${props.book.valuation_asset.symbol}`} value={bidPrice} onChange={setBidPrice} inputMode="decimal" />
        <TextInput id="treasury-quote-bid-qty" label="Bid quantity, in base units" value={bidQty} onChange={setBidQty} inputMode="numeric" />
        <TextInput id="treasury-quote-ask-price" label={`Ask price, in ${props.book.valuation_asset.symbol}`} value={askPrice} onChange={setAskPrice} inputMode="decimal" />
        <TextInput id="treasury-quote-ask-qty" label="Ask quantity, in base units" value={askQty} onChange={setAskQty} inputMode="numeric" />
        <CommandFlow<BookRequest<TreasuryQuoteRequest>, TreasuryQuoteOutcome>
          label="Place the quote"
          variant="danger"
          prepared={prepared}
          command={command}
          refusedNote="Neither leg reached the book."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="Instrument">
                <Identifier value={request.body.instrument_id} />
              </Field>
              <Field label="Bid">
                <code className="mono-small">{request.body.bid_quantity} base units</code> at {request.body.bid_price}
              </Field>
              <Field label="Ask">
                <code className="mono-small">{request.body.ask_quantity} base units</code> at {request.body.ask_price}
              </Field>
            </FieldGrid>
          )}
          renderResult={(quote) => (
            <>
              <QuoteLegView title="Bid" leg={quote.bid} valuation={props.book.valuation_asset} />
              <QuoteLegView title="Ask" leg={quote.ask} valuation={props.book.valuation_asset} />
            </>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * The simulated paper venue
 * ------------------------------------------------------------------------ */

function PaperDescribe(props: {
  readonly book: TreasuryBookDetail;
  readonly rows: ReadonlyArray<readonly [string, ReactNode]>;
}): ReactNode {
  return (
    <FieldGrid columns={3}>
      {props.rows.map(([label, value]) => (
        <Field key={label} label={label}>
          {value}
        </Field>
      ))}
    </FieldGrid>
  );
}

function HedgePanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useHedge();
  const [asset, setAsset] = useState("");
  const [side, setSide] = useState<TreasurySide>("SELL");
  const [quantity, setQuantity] = useState("");
  const [mode, setMode] = useState<TreasuryRunMode>("PAPER");
  const a = parseUuid(asset, "The asset id");
  const q = parseBaseUnits(quantity, "The quantity");
  const prepared = prepare<BookRequest<TreasuryHedgeRequest>>([problem(a), problem(q)], () => ({
    bookId: props.book.house_account_id,
    body: { asset_id: a.ok ? a.value : "", side, quantity: q.ok ? q.value : "", mode },
  }));
  return (
    <PaperPanel
      title="Hedge"
      description="An immediate fill on the simulated paper venue under HEDGING, at a fresh reference mark, its spread and fee disclosed and verified before anything is posted."
    >
      <CommandForm label="Hedge">
        <TextInput id="treasury-hedge-asset" label="Asset id" value={asset} onChange={setAsset} list={ASSET_LIST} />
        <Choice id="treasury-hedge-side" label="Side" value={side} options={TREASURY_SIDES} onChange={setSide} />
        <TextInput id="treasury-hedge-quantity" label="Quantity, in base units" value={quantity} onChange={setQuantity} inputMode="numeric" />
        <Choice id="treasury-hedge-mode" label="Mode" value={mode} options={TREASURY_RUN_MODES} onChange={setMode} />
        <CommandFlow<BookRequest<TreasuryHedgeRequest>, TreasuryPaperOutcome>
          label="Hedge on the paper venue"
          variant="danger"
          prepared={prepared}
          command={command}
          refusedNote="No fill was made. A refusal is recorded in the book's action register."
          describe={(request) => (
            <PaperDescribe
              book={props.book}
              rows={[
                ["Asset", <Identifier key="a" value={request.body.asset_id} />],
                ["Side", request.body.side],
                ["Quantity", <Units key="q" value={request.body.quantity} book={props.book} />],
                ["Mode", request.body.mode ?? "PAPER"],
              ]}
            />
          )}
          renderResult={(outcome) => <PaperOutcomeView outcome={outcome} valuation={props.book.valuation_asset} />}
        />
      </CommandForm>
    </PaperPanel>
  );
}

function ConvertPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useConvert();
  const [from, setFrom] = useState(props.book.valuation_asset.asset_id);
  const [to, setTo] = useState("");
  const [quantity, setQuantity] = useState("");
  const [mode, setMode] = useState<TreasuryRunMode>("PAPER");
  const f = parseUuid(from, "The asset converted from");
  const t = parseUuid(to, "The asset converted to");
  const q = parseBaseUnits(quantity, "The quantity");
  const prepared = prepare<BookRequest<TreasuryConvertRequest>>([problem(f), problem(t), problem(q)], () => ({
    bookId: props.book.house_account_id,
    body: { from_asset_id: f.ok ? f.value : "", to_asset_id: t.ok ? t.value : "", quantity: q.ok ? q.value : "", mode },
  }));
  return (
    <PaperPanel
      title="Convert"
      description="A treasury conversion between the valuation asset and one other sandbox asset on the simulated paper venue. The quantity is of the asset that is not the valuation asset."
    >
      <CommandForm label="Convert">
        <TextInput id="treasury-convert-from" label="From asset id" value={from} onChange={setFrom} list={ASSET_LIST} />
        <TextInput id="treasury-convert-to" label="To asset id" value={to} onChange={setTo} list={ASSET_LIST} />
        <TextInput id="treasury-convert-quantity" label="Quantity, in base units" value={quantity} onChange={setQuantity} inputMode="numeric" />
        <Choice id="treasury-convert-mode" label="Mode" value={mode} options={TREASURY_RUN_MODES} onChange={setMode} />
        <CommandFlow<BookRequest<TreasuryConvertRequest>, TreasuryPaperOutcome>
          label="Convert on the paper venue"
          variant="danger"
          prepared={prepared}
          command={command}
          refusedNote="Nothing was converted. A refusal is recorded in the book's action register."
          describe={(request) => (
            <PaperDescribe
              book={props.book}
              rows={[
                ["From", <Identifier key="f" value={request.body.from_asset_id} />],
                ["To", <Identifier key="t" value={request.body.to_asset_id} />],
                ["Quantity", <Units key="q" value={request.body.quantity} book={props.book} />],
                ["Mode", request.body.mode ?? "PAPER"],
              ]}
            />
          )}
          renderResult={(outcome) => <PaperOutcomeView outcome={outcome} valuation={props.book.valuation_asset} />}
        />
      </CommandForm>
    </PaperPanel>
  );
}

function RebalancePanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = useRebalance();
  const [asset, setAsset] = useState("");
  const [target, setTarget] = useState("");
  const [mode, setMode] = useState<TreasuryRunMode>("PAPER");
  const a = parseUuid(asset, "The asset id");
  const t = parseBaseUnits(target, "The target", true);
  const prepared = prepare<BookRequest<TreasuryRebalanceRequest>>([problem(a), problem(t)], () => ({
    bookId: props.book.house_account_id,
    body: { asset_id: a.ok ? a.value : "", target: t.ok ? t.value : "", mode },
  }));
  return (
    <PaperPanel
      title="Rebalance"
      description="Moves the book's holding of an asset to a target on the simulated paper venue: a sale of the excess, or a buy of the shortfall, which the limits bind. A holding already at its target needs no fill and records nothing."
    >
      <CommandForm label="Rebalance">
        <TextInput id="treasury-rebalance-asset" label="Asset id" value={asset} onChange={setAsset} list={ASSET_LIST} />
        <TextInput id="treasury-rebalance-target" label="Target holding, in base units" value={target} onChange={setTarget} inputMode="numeric" />
        <Choice id="treasury-rebalance-mode" label="Mode" value={mode} options={TREASURY_RUN_MODES} onChange={setMode} />
        <CommandFlow<BookRequest<TreasuryRebalanceRequest>, TreasuryRebalanceOutcome>
          label="Rebalance on the paper venue"
          variant="danger"
          prepared={prepared}
          command={command}
          refusedNote="Nothing was rebalanced. A refusal is recorded in the book's action register."
          describe={(request) => (
            <PaperDescribe
              book={props.book}
              rows={[
                ["Asset", <Identifier key="a" value={request.body.asset_id} />],
                ["Target", <Units key="t" value={request.body.target} book={props.book} />],
                ["Mode", request.body.mode ?? "PAPER"],
              ]}
            />
          )}
          renderResult={(outcome) =>
            outcome.no_op ? (
              <p role="status" data-field="no_op">
                The holding was already at its target: nothing was filled and nothing was recorded.
              </p>
            ) : (
              <>
                {outcome.action !== undefined && <ActionSummary action={outcome.action} />}
                {outcome.decision !== undefined && <DecisionView decision={outcome.decision} valuation={props.book.valuation_asset} />}
                {outcome.fill === undefined ? (
                  <p className="note" role="status">
                    No fill was made.
                  </p>
                ) : (
                  <PaperFillView fill={outcome.fill} />
                )}
              </>
            )
          }
        />
      </CommandForm>
    </PaperPanel>
  );
}

function PaperFillPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const command = usePaperFill();
  const activities = props.book.permitted_activities;
  const [activity, setActivity] = useState<TreasuryStrategy>(activities[0] ?? props.book.strategy);
  const [asset, setAsset] = useState("");
  const [side, setSide] = useState<TreasurySide>("BUY");
  const [quantity, setQuantity] = useState("");
  const [mode, setMode] = useState<TreasuryRunMode>("PAPER");
  const a = parseUuid(asset, "The asset id");
  const q = parseBaseUnits(quantity, "The quantity");
  const prepared = prepare<BookRequest<TreasuryPaperRequest>>([problem(a), problem(q)], () => ({
    bookId: props.book.house_account_id,
    body: { activity, asset_id: a.ok ? a.value : "", side, quantity: q.ok ? q.value : "", mode },
  }));
  return (
    <PaperPanel
      title="Paper execution"
      description="An immediate fill on the simulated paper venue under an activity the book's strategy permits."
    >
      <CommandForm label="Paper execution">
        <Choice
          id="treasury-paper-activity"
          label="Activity"
          value={activity}
          options={activities}
          onChange={setActivity}
          describe={(value) => STRATEGY_MEANING[value]}
        />
        <TextInput id="treasury-paper-asset" label="Asset id" value={asset} onChange={setAsset} list={ASSET_LIST} />
        <Choice id="treasury-paper-side" label="Side" value={side} options={TREASURY_SIDES} onChange={setSide} />
        <TextInput id="treasury-paper-quantity" label="Quantity, in base units" value={quantity} onChange={setQuantity} inputMode="numeric" />
        <Choice id="treasury-paper-mode" label="Mode" value={mode} options={TREASURY_RUN_MODES} onChange={setMode} />
        <CommandFlow<BookRequest<TreasuryPaperRequest>, TreasuryPaperOutcome>
          label="Execute on the paper venue"
          variant="danger"
          prepared={prepared}
          command={command}
          refusedNote="No fill was made. A refusal is recorded in the book's action register."
          describe={(request) => (
            <PaperDescribe
              book={props.book}
              rows={[
                ["Activity", request.body.activity],
                ["Asset", <Identifier key="a" value={request.body.asset_id} />],
                ["Side", request.body.side],
                ["Quantity", <Units key="q" value={request.body.quantity} book={props.book} />],
                ["Mode", request.body.mode ?? "PAPER"],
              ]}
            />
          )}
          renderResult={(outcome) => <PaperOutcomeView outcome={outcome} valuation={props.book.valuation_asset} />}
        />
      </CommandForm>
    </PaperPanel>
  );
}

function PaperPanel(props: { readonly title: string; readonly description: string; readonly children: ReactNode }): ReactNode {
  return (
    <Panel
      title={props.title}
      description={`${props.description} It moves simulated value, so the backend asks for a recent strong sign-in.`}
    >
      {props.children}
    </Panel>
  );
}

