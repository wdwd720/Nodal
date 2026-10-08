/**
 * One issuance (mint) or redemption (burn) request, exactly as the stable
 * asset controller recorded it: the units, the payment or payout, the
 * simulated chain transaction, the request's status and its reconciliation
 * state. A request whose submission outcome is unknown is drawn as unknown,
 * with the reconciliation that will settle it — never as a success and never
 * as a failure.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { StableIssuance, StableRedemption } from "../../../api/advanced/stable.ts";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import {
  ISSUANCE_STATUS_MEANING,
  REDEMPTION_MODE_MEANING,
  REDEMPTION_STATUS_MEANING,
  REQUEST_MODE_MEANING,
  issuanceOutcome,
  redemptionOutcome,
  requestStatusTone,
} from "../../../lib/stable.ts";
import { formatInstant } from "../../../lib/time.ts";
import { ScrollTable } from "../shared.tsx";
import { ChainTx, OutcomePill, PriceLine, ReconciliationLine } from "./parts.tsx";
import { issuanceHref, redemptionHref, stableAssetHref } from "./paths.ts";

/** Says, before any figure, what an unknown outcome means for this request. */
export function UnknownOutcomeNote(props: { readonly unknown: boolean }): ReactNode {
  if (!props.unknown) return null;
  return (
    <p className="callout" role="status">
      The outcome of this request's submission to the simulated chain is unknown. It is neither a success nor a failure:
      the request is never resubmitted, and it is settled from the chain by its own transaction id when it is reconciled.
    </p>
  );
}

export function IssuanceFields(props: { readonly issuance: StableIssuance }): ReactNode {
  const i = props.issuance;
  const outcome = issuanceOutcome(i.status, i.reconciliation.state);
  return (
    <>
      <UnknownOutcomeNote unknown={outcome === "UNKNOWN"} />
      <FieldGrid columns={3}>
        <Field label="Outcome" note="What the request's status says, and nothing more.">
          <OutcomePill outcome={outcome} done="Issued" />
        </Field>
        <Field label="Status" note={ISSUANCE_STATUS_MEANING[i.status]}>
          <Pill tone={requestStatusTone(i.status)}>{i.status}</Pill>
          {i.failure_code !== undefined && <code className="mono-small"> {i.failure_code}</code>}
        </Field>
        <Field label="Reconciliation">
          <ReconciliationLine reconciliation={i.reconciliation} />
        </Field>
        <Field label="Units issued" note="Simulated units of the stable asset, in base units as recorded.">
          <span data-field="quantity">
            <Qty value={i.quantity} decimals={i.decimals} symbol={i.symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={i.quantity} />
          </span>
        </Field>
        <Field label="Payment" note="The reserve asset the issuance is paid in, as the plan priced it.">
          <span data-field="reserve_quantity">
            <Qty value={i.reserve_quantity} decimals={i.reserve_decimals} symbol={i.reserve_symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={i.reserve_quantity} />
          </span>
        </Field>
        <Field label="Payment priced at" note="The verified price the mint plan recorded for one whole unit of the payment.">
          <PriceLine price={i.reserve_price} />
        </Field>
        <Field label="Mode" note={REQUEST_MODE_MEANING[i.mode]}>
          <code className="mono-small">{i.mode}</code>
        </Field>
        <Field label="Simulated chain transaction" note="The planned TOKEN_MINT_TO, submitted exactly once.">
          <ChainTx chain={i.chain} txId={i.tx_id} />
          {i.final_slot !== undefined && <span className="cell-note">final at slot {String(i.final_slot)}</span>}
        </Field>
        <Field label="Recipient" note="The account's custodied wallet on the simulated chain.">
          <Identifier value={i.recipient} />
        </Field>
        <Field label="Plan hash" note="The canonical mint plan the transaction's memo binds.">
          <Identifier value={i.plan_hash} />
        </Field>
        <Field label="Journal transaction" note="The STABLE_ISSUANCE posting, made only once the transaction was final.">
          {i.journal_transaction_id === undefined ? (
            <span className="absent">nothing posted</span>
          ) : (
            <Identifier value={i.journal_transaction_id} />
          )}
        </Field>
        <Field label="Economic intent and effect">
          <Identifier value={i.intent_id} label="intent" />
          <Identifier value={i.effect_id} label="effect" />
        </Field>
        <Field label="Recorded">
          {formatInstant(i.created_at)}
          <span className="cell-note">updated {formatInstant(i.updated_at)}</span>
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <Link to={stableAssetHref(i.stable_asset_id)}>The stable asset</Link>
        <Link to={issuanceHref(i.id)}>This issuance on its own page</Link>
      </p>
    </>
  );
}

export function RedemptionFields(props: { readonly redemption: StableRedemption }): ReactNode {
  const r = props.redemption;
  const outcome = redemptionOutcome(r.status, r.reconciliation.state);
  return (
    <>
      <UnknownOutcomeNote unknown={outcome === "UNKNOWN"} />
      <FieldGrid columns={3}>
        <Field label="Outcome" note="What the request's status says, and nothing more.">
          <OutcomePill outcome={outcome} done="Redeemed" />
        </Field>
        <Field label="Status" note={REDEMPTION_STATUS_MEANING[r.status]}>
          <Pill tone={requestStatusTone(r.status)}>{r.status}</Pill>
          {r.failure_code !== undefined && <code className="mono-small"> {r.failure_code}</code>}
        </Field>
        <Field label="Reconciliation">
          <ReconciliationLine reconciliation={r.reconciliation} />
        </Field>
        <Field label="Units redeemed" note="Simulated units of the stable asset, burned on the simulated chain.">
          <span data-field="quantity">
            <Qty value={r.quantity} decimals={r.decimals} symbol={r.symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={r.quantity} />
          </span>
        </Field>
        <Field label="Payout" note="What the redemption pays at the peg, rounded down, in the policy's dollar-equivalent reserve asset.">
          <span data-field="payout_quantity">
            <Qty value={r.payout_quantity} decimals={r.payout_decimals} symbol={r.payout_symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={r.payout_quantity} />
          </span>
        </Field>
        <Field label="Redemption mode" note={REDEMPTION_MODE_MEANING[r.redemption_mode]}>
          <code className="mono-small">{r.redemption_mode}</code>
        </Field>
        <Field label="Mode" note={REQUEST_MODE_MEANING[r.mode]}>
          <code className="mono-small">{r.mode}</code>
        </Field>
        <Field label="Simulated chain transaction" note="The planned TOKEN_BURN, signed by the custodied key and submitted exactly once.">
          <ChainTx chain={r.chain} txId={r.tx_id} />
          {r.final_slot !== undefined && <span className="cell-note">final at slot {String(r.final_slot)}</span>}
        </Field>
        <Field label="Holder" note="The account's custodied wallet on the simulated chain.">
          <Identifier value={r.holder} />
        </Field>
        <Field label="Plan hash">
          <Identifier value={r.plan_hash} />
        </Field>
        <Field label="Journal transaction" note="The STABLE_REDEMPTION posting, made only once the burn was final.">
          {r.journal_transaction_id === undefined ? (
            <span className="absent">nothing posted</span>
          ) : (
            <Identifier value={r.journal_transaction_id} />
          )}
        </Field>
        <Field label="Economic intent and effect">
          <Identifier value={r.intent_id} label="intent" />
          <Identifier value={r.effect_id} label="effect" />
        </Field>
        <Field label="Recorded">
          {formatInstant(r.created_at)}
          <span className="cell-note">updated {formatInstant(r.updated_at)}</span>
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <Link to={stableAssetHref(r.stable_asset_id)}>The stable asset</Link>
        <Link to={redemptionHref(r.id)}>This redemption on its own page</Link>
      </p>
    </>
  );
}

export function IssuanceTable(props: { readonly items: readonly StableIssuance[] }): ReactNode {
  return (
    <ScrollTable caption="Issuance requests" headers={["Request", "Outcome", "Status", "Units", "Payment", "Reconciliation", "Recorded"]}>
      {props.items.map((i) => (
        <tr key={i.id} data-request={i.id}>
          <th scope="row">
            <Link to={issuanceHref(i.id)}>{i.symbol}</Link>
            <span className="cell-note mono-small">{i.id}</span>
          </th>
          <td>
            <OutcomePill outcome={issuanceOutcome(i.status, i.reconciliation.state)} done="Issued" />
          </td>
          <td>
            <Pill tone={requestStatusTone(i.status)}>{i.status}</Pill>
            {i.failure_code !== undefined && <span className="cell-note mono-small">{i.failure_code}</span>}
          </td>
          <td>
            <Qty value={i.quantity} decimals={i.decimals} symbol={i.symbol} />
          </td>
          <td>
            <Qty value={i.reserve_quantity} decimals={i.reserve_decimals} symbol={i.reserve_symbol} />
          </td>
          <td>
            <code className="mono-small">{i.reconciliation.state}</code>
          </td>
          <td>{formatInstant(i.created_at)}</td>
        </tr>
      ))}
    </ScrollTable>
  );
}

export function RedemptionTable(props: { readonly items: readonly StableRedemption[] }): ReactNode {
  return (
    <ScrollTable caption="Redemption requests" headers={["Request", "Outcome", "Status", "Units", "Payout", "Reconciliation", "Recorded"]}>
      {props.items.map((r) => (
        <tr key={r.id} data-request={r.id}>
          <th scope="row">
            <Link to={redemptionHref(r.id)}>{r.symbol}</Link>
            <span className="cell-note mono-small">{r.id}</span>
          </th>
          <td>
            <OutcomePill outcome={redemptionOutcome(r.status, r.reconciliation.state)} done="Redeemed" />
          </td>
          <td>
            <Pill tone={requestStatusTone(r.status)}>{r.status}</Pill>
            {r.failure_code !== undefined && <span className="cell-note mono-small">{r.failure_code}</span>}
          </td>
          <td>
            <Qty value={r.quantity} decimals={r.decimals} symbol={r.symbol} />
          </td>
          <td>
            <Qty value={r.payout_quantity} decimals={r.payout_decimals} symbol={r.payout_symbol} />
          </td>
          <td>
            <code className="mono-small">{r.reconciliation.state}</code>
          </td>
          <td>{formatInstant(r.created_at)}</td>
        </tr>
      ))}
    </ScrollTable>
  );
}
