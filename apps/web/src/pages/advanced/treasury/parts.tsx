/**
 * Pieces the treasury pages share. None of them computes a figure: every
 * amount is a string the treasury computed — base units of the asset named,
 * or of the house book's valuation asset for cost and PnL — rendered through
 * the Money components with the decimals the API sent alongside it.
 */
import type { ReactNode } from "react";
import { Link, NavLink } from "react-router-dom";

import type {
  TreasuryAction,
  TreasuryFunding,
  TreasuryHouseOrder,
  TreasuryLimitEvaluation,
  TreasuryOrderOutcome,
  TreasuryPaperFill,
  TreasuryPaperOutcome,
  TreasuryRiskDecision,
  TreasuryTrade,
} from "../../../api/advanced/treasury.ts";
import type { TreasuryAssetRef, TreasuryPrice } from "../../../api/advanced/fees.ts";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { Absent, Bps, Qty } from "../../../components/Money.tsx";
import { MoneyFormatError } from "../../../lib/money.ts";
import { formatInstant } from "../../../lib/time.ts";
import {
  ACTION_KIND_MEANING,
  formatExactPrice,
  legOutcome,
  orderStatusTone,
  outcomeTone,
  verdictTone,
} from "../../../lib/treasury.ts";
import { advancedHref } from "../area.ts";
import { Codes, ScrollTable } from "../shared.tsx";
import { DisclosureView } from "../fees/parts.tsx";

/* --------------------------------------------------------------------------
 * Addresses and navigation
 * ------------------------------------------------------------------------ */

export const TREASURY_PATH = "treasury";

export function treasuryHref(...segments: readonly string[]): string {
  return advancedHref(TREASURY_PATH, ...segments);
}

export function bookHref(bookId: string, page?: "activity" | "operate"): string {
  return page === undefined ? treasuryHref("books", bookId) : treasuryHref("books", bookId, page);
}

const SECTION_PAGES: ReadonlyArray<{ readonly to: string; readonly label: string }> = [
  { to: "", label: "House books" },
  { to: "crossing", label: "Crossing rules" },
];

/** The section's own pages. */
export function TreasuryNav(): ReactNode {
  return (
    <nav className="subnav" aria-label="Treasury pages">
      <ul>
        {SECTION_PAGES.map((page) => (
          <li key={page.to}>
            <NavLink
              end
              to={page.to === "" ? treasuryHref() : treasuryHref(page.to)}
              className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}
            >
              {page.label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/** One house book's pages. */
export function BookNav(props: { readonly bookId: string }): ReactNode {
  const pages: ReadonlyArray<{ readonly to: string; readonly label: string }> = [
    { to: bookHref(props.bookId), label: "Book, PnL and limits" },
    { to: bookHref(props.bookId, "activity"), label: "Activity" },
    { to: bookHref(props.bookId, "operate"), label: "Operate" },
  ];
  return (
    <nav className="subnav" aria-label="House book pages">
      <ul>
        {pages.map((page) => (
          <li key={page.to}>
            <NavLink end to={page.to} className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}>
              {page.label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/* --------------------------------------------------------------------------
 * Figures
 * ------------------------------------------------------------------------ */

/** An exact price, mantissa × 10^-scale, in the unit named; a malformed one is said to be unreadable. */
export function ExactPrice(props: {
  readonly price: Pick<TreasuryPrice, "mantissa" | "scale"> & { readonly observed_at?: string | undefined; readonly source?: string | undefined };
  readonly unit?: string;
}): ReactNode {
  let text: string;
  try {
    text = formatExactPrice(props.price.mantissa, props.price.scale);
  } catch (error) {
    if (error instanceof MoneyFormatError) {
      return (
        <span className="malformed" role="status">
          unreadable value <span className="malformed-detail">({error.message})</span>
        </span>
      );
    }
    throw error;
  }
  return (
    <span className="num">
      {text}
      {props.unit === undefined ? "" : ` ${props.unit}`}
      {props.price.observed_at !== undefined && (
        <span className="cell-note">
          observed {formatInstant(props.price.observed_at)}
          {props.price.source === undefined ? "" : ` · ${props.price.source}`}
        </span>
      )}
    </span>
  );
}

/** Base units of the asset a reference names, formatted with its decimals. */
export function AssetAmount(props: { readonly value: string | undefined; readonly asset: TreasuryAssetRef; readonly absent?: string }): ReactNode {
  return (
    <Qty
      value={props.value}
      decimals={props.asset.decimals}
      symbol={props.asset.symbol}
      {...(props.absent === undefined ? {} : { absent: props.absent })}
    />
  );
}

/** An asset by its symbol, with its id. */
export function AssetName(props: { readonly asset: TreasuryAssetRef }): ReactNode {
  return (
    <span title={props.asset.asset_id}>
      {props.asset.symbol} <span className="mono-small">{props.asset.asset_id}</span>
    </span>
  );
}

/* --------------------------------------------------------------------------
 * House actions, decisions and orders
 * ------------------------------------------------------------------------ */

export function OutcomePill(props: { readonly outcome: string }): ReactNode {
  return <Pill tone={outcomeTone(props.outcome)}>{props.outcome}</Pill>;
}

/** One recorded house action, as the register states it. */
export function ActionSummary(props: { readonly action: TreasuryAction }): ReactNode {
  const { action } = props;
  return (
    <FieldGrid columns={3}>
      <Field label="Action" note={ACTION_KIND_MEANING[action.kind]}>
        <code className="mono-small">{action.kind}</code>
        {action.activity !== undefined && <span className="cell-note">under {action.activity}</span>}
      </Field>
      <Field label="Outcome" note="A refusal is recorded too, with its codes.">
        <OutcomePill outcome={action.outcome} />
        {action.rejection_code !== undefined && <code className="mono-small"> {action.rejection_code}</code>}
        <Codes codes={action.reason_codes} />
      </Field>
      <Field label="Taken by" note="Under the signing and risk policy versions shown.">
        {action.actor_type} <code className="mono-small">{action.actor_id}</code>
        <span className="cell-note">
          signing policy v{String(action.signing_policy_version)}
          {action.risk_policy_version === undefined ? "" : ` · risk policy v${String(action.risk_policy_version)}`}
        </span>
      </Field>
      <Field label="Recorded">{formatInstant(action.occurred_at)}</Field>
      <Field label="Action id">
        <Identifier value={action.id} />
      </Field>
      <Field label="Evidence" note="The archived evidence document of the action.">
        {action.evidence_ref === undefined ? <span className="absent">none</span> : <code className="mono-small">{action.evidence_ref}</code>}
      </Field>
    </FieldGrid>
  );
}

/** Every action of the register, newest first. */
export function ActionsTable(props: { readonly actions: readonly TreasuryAction[] }): ReactNode {
  return (
    <ScrollTable caption="House actions" headers={["Recorded", "Action", "Outcome", "Codes", "Taken by", "Policies", "Ids"]}>
      {props.actions.map((action) => (
        <tr key={action.id} data-action={action.id}>
          <th scope="row">{formatInstant(action.occurred_at)}</th>
          <td>
            <code className="mono-small">{action.kind}</code>
            {action.activity !== undefined && <span className="cell-note">{action.activity}</span>}
          </td>
          <td>
            <OutcomePill outcome={action.outcome} />
          </td>
          <td>
            {action.rejection_code !== undefined && <code className="mono-small">{action.rejection_code}</code>}
            <Codes codes={action.reason_codes} />
          </td>
          <td>
            {action.actor_type} <code className="mono-small">{action.actor_id}</code>
          </td>
          <td>
            <span className="mono-small">
              signing v{String(action.signing_policy_version)}
              {action.risk_policy_version === undefined ? "" : ` · risk v${String(action.risk_policy_version)}`}
            </span>
          </td>
          <td>
            <Identifier value={action.id} label="action" />
            {action.order_id !== undefined && <Identifier value={action.order_id} label="order" />}
            {action.paper_fill_id !== undefined && <Identifier value={action.paper_fill_id} label="fill" />}
            {action.funding_id !== undefined && <Identifier value={action.funding_id} label="funding" />}
            {action.risk_decision_id !== undefined && <Identifier value={action.risk_decision_id} label="decision" />}
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

/** The treasury's limit evaluation behind a decision. */
export function LimitEvaluationView(props: { readonly limits: TreasuryLimitEvaluation; readonly valuation: TreasuryAssetRef }): ReactNode {
  const { limits, valuation } = props;
  return (
    <FieldGrid columns={3}>
      <Field label="Risk increasing" note="Treasury limits bind only a buy of a risky asset.">
        {limits.risk_increasing ? "yes" : "no"}
      </Field>
      <Field label="Loss measure" note="Realized PnL of the UTC day plus every open loss (negative is a loss).">
        <AssetAmount value={limits.loss_measure} asset={valuation} />
      </Field>
      <Field label="Daily loss limit">
        <AssetAmount value={limits.daily_loss_limit} asset={valuation} />
      </Field>
      <Field label="Inventory after" note="Held plus every working buy plus this request, in base units of the asset.">
        <code className="mono-small">{limits.inventory_after}</code>
        {limits.inventory_cap !== undefined && <span className="cell-note">cap {limits.inventory_cap}</span>}
      </Field>
      <Field label="Exposure after" note="Held at a fresh mark plus working buys plus this request.">
        {limits.exposure_after === undefined ? (
          <Absent what="not evaluated" />
        ) : (
          <AssetAmount value={limits.exposure_after} asset={valuation} />
        )}
        {limits.exposure_cap !== undefined && (
          <span className="cell-note">
            cap <AssetAmount value={limits.exposure_cap} asset={valuation} />
          </span>
        )}
      </Field>
      <Field label="Treasury codes" note="Each one stops new risk.">
        <Codes codes={limits.codes} none="none" />
        {limits.missing_marks.length > 0 && <span className="cell-note">missing marks: {limits.missing_marks.join(", ")}</span>}
      </Field>
    </FieldGrid>
  );
}

/** A Risk Kernel V2 decision about a house order or fill. */
export function DecisionView(props: { readonly decision: TreasuryRiskDecision; readonly valuation: TreasuryAssetRef }): ReactNode {
  const { decision } = props;
  return (
    <div>
      <p className="meta-row">
        <Pill tone={verdictTone(decision.verdict)}>{decision.verdict}</Pill>
        <span className="mono-small">decision {decision.decision_hash}</span>
        <span className="mono-small">input {decision.input_hash}</span>
      </p>
      <Codes codes={[...decision.reason_codes, ...decision.treasury_codes]} />
      {decision.limits !== undefined && <LimitEvaluationView limits={decision.limits} valuation={props.valuation} />}
    </div>
  );
}

/** A house order as the internal book holds it. */
export function OrderView(props: { readonly order: TreasuryHouseOrder; readonly base: TreasuryAssetRef | undefined }): ReactNode {
  const { order, base } = props;
  const qty = (value: string): ReactNode =>
    base === undefined ? <span className="mono-small">{value} base units</span> : <AssetAmount value={value} asset={base} />;
  return (
    <FieldGrid columns={3}>
      <Field label="Order">
        <Identifier value={order.id} />
      </Field>
      <Field label="Status">
        <Pill tone={orderStatusTone(order.status)}>{order.status}</Pill>
        {order.reject_reason !== undefined && <code className="mono-small"> {order.reject_reason}</code>}
        {order.cancel_reason !== undefined && <span className="cell-note">{order.cancel_reason}</span>}
      </Field>
      <Field label="Side, type, time in force">
        {order.side} {order.order_type} {order.time_in_force}
        {order.post_only && <span className="cell-note">post-only</span>}
        {order.reduce_only && <span className="cell-note">reduce-only</span>}
      </Field>
      <Field label="Limit price" note="As the book states it, in the book's valuation asset.">
        <span className="num">{order.price}</span>
      </Field>
      <Field label="Quantity">{qty(order.quantity)}</Field>
      <Field label="Filled / remaining / cancelled">
        {qty(order.filled_quantity)}
        <span className="cell-note">{qty(order.remaining_quantity)}</span>
        <span className="cell-note">{qty(order.cancelled_quantity)}</span>
      </Field>
    </FieldGrid>
  );
}

/** The trades an order made at once. */
export function TradesTable(props: { readonly trades: readonly TreasuryTrade[]; readonly valuation: TreasuryAssetRef }): ReactNode {
  if (props.trades.length === 0) {
    return (
      <p className="note" role="status">
        The order made no trade at once.
      </p>
    );
  }
  return (
    <ScrollTable caption="Trades the order made" headers={["Trade", "Price", "Base quantity", "Notional", "Maker / taker", "Fees", "Crossing rule"]}>
      {props.trades.map((trade) => (
        <tr key={trade.id}>
          <th scope="row">{String(trade.trade_seq)}</th>
          <td>
            <ExactPrice price={trade.price} unit={props.valuation.symbol} />
          </td>
          <td>
            <span className="mono-small">{trade.base_quantity} base units</span>
          </td>
          <td>
            <AssetAmount value={trade.quote_notional} asset={props.valuation} />
          </td>
          <td>
            {trade.maker_owner_type} / {trade.taker_owner_type}
            <span className="cell-note">aggressor {trade.aggressor_side}</span>
          </td>
          <td>
            <AssetAmount value={trade.maker_fee} asset={props.valuation} />
            <span className="cell-note">
              <AssetAmount value={trade.taker_fee} asset={props.valuation} />
            </span>
          </td>
          <td>{trade.crossing_rule_version === 0 ? <span className="absent">no house-versus-customer crossing</span> : `version ${String(trade.crossing_rule_version)}`}</td>
        </tr>
      ))}
    </ScrollTable>
  );
}

/** The answer to a house order or a cancel. */
export function OrderOutcomeView(props: { readonly outcome: TreasuryOrderOutcome; readonly valuation: TreasuryAssetRef }): ReactNode {
  const { outcome } = props;
  return (
    <div>
      <ActionSummary action={outcome.action} />
      {outcome.order !== undefined && <OrderView order={outcome.order} base={undefined} />}
      {outcome.decision !== undefined && <DecisionView decision={outcome.decision} valuation={props.valuation} />}
      <TradesTable trades={outcome.trades} valuation={props.valuation} />
    </div>
  );
}

/** One leg of a two-sided quote: applied, or refused with its codes. */
export function QuoteLegView(props: { readonly title: string; readonly leg: TreasuryOrderOutcome; readonly valuation: TreasuryAssetRef }): ReactNode {
  const outcome = legOutcome(props.leg);
  return (
    <section className="callout" aria-label={props.title} data-leg={props.title} data-outcome={props.leg.action.outcome}>
      <h3>
        {props.title} <OutcomePill outcome={props.leg.action.outcome} />
      </h3>
      {!outcome.applied && (
        <p>
          Refused: <Codes codes={outcome.codes} />
        </p>
      )}
      <OrderOutcomeView outcome={props.leg} valuation={props.valuation} />
    </section>
  );
}

/** A house paper-venue fill with the disclosure it was priced and verified under. */
export function PaperFillView(props: { readonly fill: TreasuryPaperFill }): ReactNode {
  const { fill } = props;
  return (
    <div>
      <FieldGrid columns={3}>
        <Field label="Venue" note="The house paper venue is simulated; nothing reaches a real venue.">
          <code className="mono-small">{fill.venue_label}</code>
        </Field>
        <Field label="Activity and side">
          {fill.activity} · {fill.side}
        </Field>
        <Field label="Filled">{formatInstant(fill.filled_at)}</Field>
        <Field label="Quantity">
          <AssetAmount value={fill.base_quantity} asset={fill.base_asset} />
        </Field>
        <Field label="Reference price" note="A fresh mark of the asset in the valuation asset.">
          <ExactPrice price={fill.reference_price} unit={fill.quote_asset.symbol} />
        </Field>
        <Field label="Notional at the reference">
          <AssetAmount value={fill.notional_at_reference} asset={fill.quote_asset} />
        </Field>
        <Field label="Spread" note="Disclosed, and paid by the house book.">
          <AssetAmount value={fill.spread_amount} asset={fill.quote_asset} /> <Bps value={fill.spread_bps} />
        </Field>
        <Field label="Venue fee">
          <AssetAmount value={fill.fee_amount} asset={fill.quote_asset} />
        </Field>
        <Field label="Total" note="Paid for a buy, received for a sale.">
          <AssetAmount value={fill.total_quote} asset={fill.quote_asset} />
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <span className="mono-small">fill {fill.id}</span>
        <span className="mono-small">journal {fill.journal_transaction_id}</span>
        <span className="mono-small">schedule {fill.schedule_hash}</span>
      </p>
      <DisclosureView disclosure={fill.disclosure} quote={fill.quote_asset} />
    </div>
  );
}

/** The answer to a paper fill, a hedge or a conversion. */
export function PaperOutcomeView(props: { readonly outcome: TreasuryPaperOutcome; readonly valuation: TreasuryAssetRef }): ReactNode {
  const { outcome } = props;
  return (
    <div>
      <ActionSummary action={outcome.action} />
      {outcome.decision !== undefined && <DecisionView decision={outcome.decision} valuation={props.valuation} />}
      {outcome.fill === undefined ? (
        <p className="note" role="status">
          No fill was made.
        </p>
      ) : (
        <PaperFillView fill={outcome.fill} />
      )}
    </div>
  );
}

/** A funding of simulated house capital. */
export function FundingView(props: { readonly funding: TreasuryFunding }): ReactNode {
  const { funding } = props;
  return (
    <div>
      <FieldGrid columns={3}>
        <Field label="Funded" note="Simulated house capital, posted as HOUSE_FUNDING.">
          <AssetAmount value={funding.quantity} asset={funding.asset} />
        </Field>
        <Field label="Cost basis" note="What the funding entered the position register at, in the valuation asset's base units.">
          <code className="mono-small">{funding.cost_basis}</code>
        </Field>
        <Field label="Mark" note="Only a funding in another asset than the valuation asset is marked.">
          {funding.mark === undefined ? <span className="absent">not marked: the valuation asset</span> : <ExactPrice price={funding.mark} />}
        </Field>
        <Field label="Journal">
          <Identifier value={funding.journal_transaction_id} />
        </Field>
        <Field label="Effect">
          <Identifier value={funding.effect_id} />
        </Field>
        <Field label="Funded at">{formatInstant(funding.funded_at)}</Field>
      </FieldGrid>
      <ActionSummary action={funding.action} />
    </div>
  );
}

/** A link to a house book. */
export function BookLink(props: { readonly bookId: string; readonly name: string }): ReactNode {
  return <Link to={bookHref(props.bookId)}>{props.name}</Link>;
}
