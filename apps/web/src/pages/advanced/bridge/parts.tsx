/**
 * Pieces the bridge pages share: the section's navigation, a leg's progress
 * through its states, the chains' word on a transaction, and a leg's outcome
 * drawn from its own state. None of them computes a figure.
 */
import type { ReactNode } from "react";
import { NavLink } from "react-router-dom";

import type { BridgeChainTx, BridgeLeg } from "../../../api/advanced/bridge.ts";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { EFFECT_STATUS_MEANING, effectStatusTone } from "../../../lib/advanced.ts";
import {
  HOLDING_MEANING,
  LEG_OUTCOME_LABEL,
  LEG_STATUS_MEANING,
  PROGRESS_WORDS,
  RECONCILIATION_STATE_MEANING,
  TX_STATE_MEANING,
  legOutcome,
  legOutcomeTone,
  legProgress,
  legStatusTone,
  txStateTone,
} from "../../../lib/bridge.ts";
import { reconciliationTone } from "../../../lib/stable.ts";
import { formatInstant } from "../../../lib/time.ts";
import { bridgeHref } from "./paths.ts";

const BRIDGE_PAGES: ReadonlyArray<{ readonly path: string; readonly label: string }> = [
  { path: "", label: "Holdings by chain" },
  { path: "routes", label: "Routes and transfers" },
  { path: "legs", label: "Legs" },
  { path: "chains", label: "Chains" },
  { path: "operator", label: "Operator" },
];

/** The section's own pages. */
export function BridgeNav(): ReactNode {
  return (
    <nav className="subnav" aria-label="Bridge pages">
      <ul>
        {BRIDGE_PAGES.map((page) => (
          <li key={page.path}>
            <NavLink
              end
              to={page.path === "" ? bridgeHref() : bridgeHref(page.path)}
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

/** What the leg's state says about its outcome; an unknown submission is drawn as unknown. */
export function LegOutcomePill(props: { readonly leg: BridgeLeg }): ReactNode {
  const outcome = legOutcome(props.leg.status, props.leg.reconciliation.state);
  return <Pill tone={legOutcomeTone(outcome)}>{LEG_OUTCOME_LABEL[outcome]}</Pill>;
}

/** Says, before any figure, what an unknown submission means for a leg. */
export function LegUnknownNote(props: { readonly leg: BridgeLeg }): ReactNode {
  if (props.leg.reconciliation.state !== "AWAITING") return null;
  return (
    <p className="callout" role="status">
      A submission of this leg to a simulated chain has an unknown outcome. That is neither a success nor a failure: it is
      never resubmitted, and the leg is settled from the chains by transaction id when it is reconciled.
    </p>
  );
}

/**
 * The leg's progress through its states. `visited` are the states its
 * transitions name; without them, what cannot be told is said to be not
 * shown here rather than guessed.
 */
export function LegProgress(props: { readonly status: string; readonly visited?: readonly string[] }): ReactNode {
  const steps = legProgress(props.status, props.visited);
  return (
    <ol className="timeline" aria-label="Leg progress">
      {steps.map((step) => (
        <li
          key={step.status}
          className={step.state === "done" || step.state === "current" ? "step step-present" : "step step-absent"}
          data-step={step.status}
          data-state={step.state}
        >
          <span className="step-marker" aria-hidden="true" />
          <div className="step-body">
            <p className="step-label">
              {step.status} <span className="mono-small">({PROGRESS_WORDS[step.state]})</span>
            </p>
            <p className="step-meta">{LEG_STATUS_MEANING[step.status]}</p>
          </div>
        </li>
      ))}
    </ol>
  );
}

/** What a simulated chain says of one transaction, at the instant of the read. */
export function ChainTxLine(props: { readonly tx: BridgeChainTx }): ReactNode {
  const { tx } = props;
  return (
    <span>
      <Pill tone={txStateTone(tx.state)} title={TX_STATE_MEANING[tx.state]}>
        {tx.state}
        {tx.final ? ", in a finalized block" : ""}
      </Pill>{" "}
      <span className="mono-small">{tx.chain}</span> <Identifier value={tx.tx_id} />
      {tx.height !== undefined && <span className="cell-note">included at height {String(tx.height)}</span>}
      {tx.error !== undefined && <span className="cell-note">chain error {tx.error}</span>}
    </span>
  );
}

function EffectLine(props: { readonly label: string; readonly status: BridgeLeg["source_effect_status"] }): ReactNode {
  if (props.status === undefined) {
    return (
      <li>
        {props.label}: <span className="absent">not claimed</span>
      </li>
    );
  }
  return (
    <li title={EFFECT_STATUS_MEANING[props.status]}>
      {props.label}: <Pill tone={effectStatusTone(props.status)}>{props.status}</Pill>
    </li>
  );
}

/** One leg's figures and state, exactly as the bridge service recorded them. */
export function LegFields(props: { readonly leg: BridgeLeg }): ReactNode {
  const leg = props.leg;
  return (
    <>
      <LegUnknownNote leg={leg} />
      <FieldGrid columns={3}>
        <Field label="Outcome" note="What the leg's state says, and nothing more.">
          <LegOutcomePill leg={leg} />
        </Field>
        <Field label="State" note={LEG_STATUS_MEANING[leg.status]}>
          <Pill tone={legStatusTone(leg.status)}>{leg.status}</Pill>
          {leg.failure_code !== undefined && <code className="mono-small"> {leg.failure_code}</code>}
        </Field>
        <Field label="Reconciliation" note={RECONCILIATION_STATE_MEANING[leg.reconciliation.state]}>
          <Pill tone={reconciliationTone(leg.reconciliation.state)}>{leg.reconciliation.state}</Pill>
          {leg.reconciliation.unknown_since !== undefined && (
            <span className="cell-note">outcome unknown since {formatInstant(leg.reconciliation.unknown_since)}</span>
          )}
          {leg.reconciliation.resolved_at !== undefined && (
            <span className="cell-note">settled from the chains at {formatInstant(leg.reconciliation.resolved_at)}</span>
          )}
        </Field>
        <Field label="Units" note="Of the route's source asset, in base units as recorded.">
          <span data-field="quantity">
            <Qty value={leg.quantity} decimals={leg.decimals} symbol={leg.source_symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={leg.quantity} />
          </span>
        </Field>
        <Field label="From" note="The source chain and asset.">
          <code className="mono-small">{leg.source_chain}</code> {leg.source_symbol}
          <Identifier value={leg.source_owner} label="owner" />
        </Field>
        <Field label="To" note="The destination chain and the bridged twin.">
          <code className="mono-small">{leg.destination_chain}</code> {leg.destination_symbol}
          <Identifier value={leg.destination_owner} label="owner" />
        </Field>
        <Field label="Where the value is held" note={HOLDING_MEANING[leg.holding]}>
          <code className="mono-small">{leg.holding}</code>
        </Field>
        <Field label="Economic effects" note="Each applied exactly once, with the journal that posted it.">
          <ul className="codes">
            <EffectLine label="source" status={leg.source_effect_status} />
            <EffectLine label="destination" status={leg.destination_effect_status} />
            <EffectLine label="refund" status={leg.refund_effect_status} />
          </ul>
        </Field>
        <Field label="Journal transactions" note="Each posted only once the chain transaction it records was final.">
          <ul className="codes">
            <li>
              source{" "}
              {leg.source_journal_transaction_id === undefined ? (
                <span className="absent">not posted</span>
              ) : (
                <code className="mono-small">{leg.source_journal_transaction_id}</code>
              )}
            </li>
            <li>
              destination{" "}
              {leg.destination_journal_transaction_id === undefined ? (
                <span className="absent">not posted</span>
              ) : (
                <code className="mono-small">{leg.destination_journal_transaction_id}</code>
              )}
            </li>
            {leg.refund_journal_transaction_id !== undefined && (
              <li>
                refund <code className="mono-small">{leg.refund_journal_transaction_id}</code>
              </li>
            )}
          </ul>
        </Field>
        <Field label="Lock transaction" note="The holder's BRIDGE_LOCK on the source chain, submitted exactly once.">
          {leg.lock_tx_id === undefined ? <span className="absent">not signed yet</span> : <Identifier value={leg.lock_tx_id} />}
        </Field>
        <Field label="Plan hash and nonce" note="The bridge plan the lock's memo binds, and the lock's single-use nonce.">
          <Identifier value={leg.plan_hash} />
          <span className="cell-note mono-small">nonce {leg.nonce}</span>
        </Field>
        <Field label="Mode and recorded">
          <code className="mono-small">{leg.mode}</code>
          <span className="cell-note">
            {formatInstant(leg.created_at)}, updated {formatInstant(leg.updated_at)}
          </span>
        </Field>
      </FieldGrid>
    </>
  );
}
