/**
 * Pieces the fees pages share. None of them computes a fee: every charged
 * unit is a string the fee engine computed and the fee ledger stored, shown
 * line by line exactly as the disclosure itemizes it — the reference price,
 * the spread, every explicit fee with its rate, rounding and any cap, the
 * network estimate in its own asset, the slippage reserve that is held and
 * never charged, the totals and the effective all-in price.
 */
import type { ReactNode } from "react";
import { NavLink } from "react-router-dom";

import type { Asset } from "../../../api/queries.ts";
import type { FeeCharges, FeeDisclosure, FeeSpreadLine, FeeVerification, TreasuryAssetRef } from "../../../api/advanced/fees.ts";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps, Qty } from "../../../components/Money.tsx";
import { componentLabel, verificationSummary } from "../../../lib/fees.ts";
import { assetById } from "../../../lib/margin.ts";
import { MoneyFormatError } from "../../../lib/money.ts";
import { formatExactPrice } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useSession } from "../../../session.tsx";
import { advancedHref } from "../area.ts";
import { ScrollTable } from "../shared.tsx";

/* --------------------------------------------------------------------------
 * Addresses and navigation
 * ------------------------------------------------------------------------ */

export const FEES_PATH = "fees";

export function feesHref(...segments: readonly string[]): string {
  return advancedHref(FEES_PATH, ...segments);
}

/** True when the session is an operator's: the operator panels are offered, and the backend decides. */
export function useIsOperator(): boolean {
  return useSession().principal?.actor_type === "OPERATOR";
}

/** The section's own pages. The spread audit is listed for operators, whose page it is. */
export function FeesNav(): ReactNode {
  const operator = useIsOperator();
  const pages: ReadonlyArray<{ readonly to: string; readonly label: string }> = [
    { to: feesHref(), label: "Schedules" },
    { to: feesHref("quotes"), label: "Quote disclosures" },
    { to: feesHref("bookings"), label: "Fee bookings" },
    { to: feesHref("periods"), label: "Performance fees" },
    { to: feesHref("house-trades"), label: "House trades" },
    ...(operator ? [{ to: feesHref("spread-audit"), label: "Spread audit" }] : []),
  ];
  return (
    <nav className="subnav" aria-label="Fees pages">
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

/** A unit to render quote-asset base units in: its decimals, and a name when known. */
export interface QuoteUnit {
  readonly decimals: number;
  readonly symbol: string | undefined;
}

/** Base units of an asset in the unit given. */
export function UnitAmount(props: { readonly value: string; readonly unit: QuoteUnit }): ReactNode {
  return (
    <Qty value={props.value} decimals={props.unit.decimals} {...(props.unit.symbol === undefined ? {} : { symbol: props.unit.symbol })} />
  );
}

/** Base units of an asset the registry may know; raw base units when it does not. */
export function RegistryAmount(props: { readonly assets: readonly Asset[]; readonly id: string | undefined; readonly value: string }): ReactNode {
  const asset = props.id === undefined ? undefined : assetById(props.assets, props.id);
  if (asset === undefined) return <BaseUnits value={props.value} />;
  return <Qty value={props.value} decimals={asset.decimals} symbol={asset.symbol} />;
}

/** An exact price (mantissa × 10^-scale), or a stated fault when it is unreadable. */
export function PriceValue(props: { readonly mantissa: string; readonly scale: number; readonly unit?: string | undefined }): ReactNode {
  try {
    return (
      <span className="num">
        {formatExactPrice(props.mantissa, props.scale)}
        {props.unit === undefined ? "" : ` ${props.unit}`}
      </span>
    );
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
}

/** Whether a stored record still proves out, as the backend re-checked it. */
export function VerificationView(props: { readonly verification: FeeVerification }): ReactNode {
  const summary = verificationSummary(props.verification);
  return (
    <div data-field="verification">
      <Pill tone={summary.tone}>{summary.label}</Pill>
      {props.verification.reason !== undefined && <code className="mono-small"> {props.verification.reason}</code>}
      {props.verification.detail !== undefined && <p className="field-note">{props.verification.detail}</p>}
    </div>
  );
}

/** Everything a disclosure charges, component by component, as the ledger books it. */
export function ChargesView(props: { readonly charges: FeeCharges; readonly unit: QuoteUnit; readonly assets: readonly Asset[] }): ReactNode {
  const { charges, unit } = props;
  return (
    <FieldGrid columns={3}>
      <Field label="Spread" note="Paid to the platform as spread revenue.">
        <span data-field="charge:spread">
          <UnitAmount value={charges.spread} unit={unit} />
        </span>
      </Field>
      <Field label="Platform fee" note="Fixed, percentage and any minimum-fee top-up.">
        <span data-field="charge:platform">
          <UnitAmount value={charges.platform} unit={unit} />
        </span>
      </Field>
      <Field label="Strategy fee" note="Booked on its own pair of accounts.">
        <UnitAmount value={charges.strategy} unit={unit} />
      </Field>
      <Field label="Venue pass-through" note="Passed through to the venue; not platform revenue.">
        <UnitAmount value={charges.venue} unit={unit} />
      </Field>
      <Field label="Network fee" note={charges.network_in_quote ? "In the quote asset." : "In its own asset, never converted."}>
        {charges.network_in_quote ? (
          <UnitAmount value={charges.network} unit={unit} />
        ) : (
          <RegistryAmount assets={props.assets} id={charges.network_asset_id} value={charges.network} />
        )}
      </Field>
    </FieldGrid>
  );
}

/** A charged spread: the schedule version that priced it and the prices it moved between. */
export function SpreadLineView(props: { readonly line: FeeSpreadLine; readonly assets: readonly Asset[] }): ReactNode {
  const { line } = props;
  const quote = assetById(props.assets, line.quote_asset_id);
  return (
    <FieldGrid columns={3}>
      <Field label="Schedule" note="The version in force when the order was priced.">
        <code className="mono-small">{line.schedule_key}</code> v{String(line.schedule_version)}
      </Field>
      <Field label="Reference price" note="The price before the spread.">
        <PriceValue mantissa={line.reference_price.mantissa} scale={line.reference_price.scale} unit={quote?.symbol} />
      </Field>
      <Field label="Execution price" note="The reference moved by exactly the spread.">
        <PriceValue mantissa={line.execution_price.mantissa} scale={line.execution_price.scale} unit={quote?.symbol} />
      </Field>
      <Field label="Spread charged">
        <RegistryAmount assets={props.assets} id={line.quote_asset_id} value={line.spread_amount} /> <Bps value={line.spread_bps} />
      </Field>
      <Field label="Side and quantity" note="The base quantity, in base units.">
        {line.side} <code className="mono-small">{line.base_quantity}</code>
      </Field>
      <Field label="Disclosure">
        <span className="mono-small">{line.disclosure_hash}</span>
      </Field>
    </FieldGrid>
  );
}

/**
 * A disclosure, line by line. Quote-asset amounts are base units of the
 * schedule's quote asset, in its decimals as the disclosure states them.
 */
export function DisclosureView(props: { readonly disclosure: FeeDisclosure; readonly quote?: TreasuryAssetRef | undefined }): ReactNode {
  const d = props.disclosure;
  const unit: QuoteUnit = { decimals: d.input.quote_decimals, symbol: props.quote?.symbol };
  const amount = (value: string): ReactNode => <UnitAmount value={value} unit={unit} />;
  const buy = d.input.side === "BUY";
  return (
    <section className="callout" aria-label="Disclosure" data-disclosure={d.schedule_hash}>
      <h3>Disclosure — simulated</h3>
      <p className="meta-row">
        <span className="mono-small">schedule {d.schedule_version}</span>
        <span className="mono-small">hash {d.schedule_hash}</span>
        <span className="mono-small">tier {d.tier.index < 0 ? "base schedule" : String(d.tier.index)}</span>
      </p>
      <FieldGrid columns={3}>
        <Field label="Side and quantity" note="Base units of the asset priced.">
          {d.input.side} <code className="mono-small">{d.input.base_qty}</code>
        </Field>
        <Field label="Reference price" note={`${d.reference_price.source}, ${formatInstant(d.reference_price.at)}`}>
          <span data-field="reference_price">
            <PriceValue mantissa={d.reference_price.mantissa} scale={d.reference_price.scale} unit={unit.symbol} />
          </span>
        </Field>
        <Field label="Execution price" note="The reference moved by exactly the spread.">
          <span data-field="execution_price">
            <PriceValue mantissa={d.execution_price.mantissa} scale={d.execution_price.scale} unit={unit.symbol} />
          </span>
        </Field>
        <Field label="Notional at the reference">
          <span data-field="notional_at_reference">{amount(d.notional_at_reference)}</span>
        </Field>
        <Field label="Spread" note={`Rounded ${d.spread.rounding}.`}>
          <span data-field="spread">{amount(d.spread.amount)}</span> <Bps value={d.spread.bps} />
        </Field>
        <Field label="Notional at execution" note="The reference notional plus or minus exactly the spread.">
          {amount(d.notional_at_execution)}
        </Field>
      </FieldGrid>
      <ScrollTable caption="Explicit fees" headers={["Fee", "Rate", "Rounding", "Computed", "Cap reduction", "Charged"]}>
        {d.fees.lines.map((line) => (
          <tr key={line.component} data-fee={line.component}>
            <th scope="row">{componentLabel(line.component)}</th>
            <td>
              {line.component === "FIXED_FEE" ? amount(line.fixed_amount) : <Bps value={line.rate_bps} />}
              {line.discount_bps !== 0 && (
                <span className="cell-note">
                  discount <Bps value={line.discount_bps} />
                </span>
              )}
            </td>
            <td>
              <code className="mono-small">{line.rounding}</code>
            </td>
            <td>{amount(line.computed)}</td>
            <td>{amount(line.cap_reduction)}</td>
            <td data-field={`fee:${line.component}`}>{amount(line.charged)}</td>
          </tr>
        ))}
      </ScrollTable>
      <FieldGrid columns={3}>
        <Field label="Explicit fees" note="Every fee line charged, plus any minimum-fee top-up.">
          <span data-field="fees_total">{amount(d.fees.total)}</span>
          {d.fees.minimum_applied && (
            <span className="cell-note">
              the minimum fee raised the total by {amount(d.fees.minimum_fee_top_up)}
            </span>
          )}
          {(d.fees.max_bps_applied || d.fees.max_absolute_applied) && <span className="cell-note">a fee cap limited the total</span>}
        </Field>
        <Field label="Network estimate" note={d.network.in_quote_asset ? "In the quote asset, part of the total." : "In its own asset, outside the total, never converted."}>
          <code className="mono-small">{d.network.amount}</code>
          <span className="cell-note">asset {d.network.asset}</span>
        </Field>
        <Field label="Slippage reserve" note="Held against adverse execution and released if unused; never charged.">
          {amount(d.slippage_reserve.amount)} <Bps value={d.slippage_reserve.bps} />
        </Field>
        <Field label={buy ? "Total debited" : "Total credited"} emphasis note="Assembled from the itemized components only.">
          <span data-field="total">{amount(buy ? d.total_debit : d.total_credit)}</span>
        </Field>
        <Field label="Worst case" note="With the slippage reserve: the most debited for a buy, the least credited for a sale.">
          {amount(d.worst_case_total)}
        </Field>
        <Field label="Effective all-in price" note="The total over the quantity, rounded against flattering the platform.">
          <span data-field="effective_price">
            <PriceValue mantissa={d.effective_price.mantissa} scale={d.effective_price.scale} unit={unit.symbol} />
          </span>
        </Field>
      </FieldGrid>
      <p className="field-note">
        Rounding:{" "}
        {d.rounding.components.map((c) => `${componentLabel(c.component)} ${c.mode}${c.favours_platform ? " (can favour the platform)" : ""}`).join("; ")}.{" "}
        {d.rounding.any_favours_platform
          ? d.rounding.platform_favourable_allowed
            ? "The schedule explicitly allows a platform-favourable rounding."
            : "A mode can favour the platform."
          : "No component is rounded in the platform's favour."}
      </p>
    </section>
  );
}

/** One identifier with a label, for the record ids a page lists. */
export function RecordIds(props: { readonly ids: ReadonlyArray<readonly [string, string | undefined]> }): ReactNode {
  return (
    <p className="meta-row">
      {props.ids.map(([label, value]) => (value === undefined ? null : <Identifier key={label} label={label} value={value} />))}
    </p>
  );
}
