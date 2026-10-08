/**
 * Bridge legs waiting in MANUAL_REVIEW for an operator, from the console's
 * overview, each with the operator actions its state allows: relay it again,
 * refund it on the source chain, or resolve the review of a leg whose
 * destination released.
 *
 * Every action states its reason, is confirmed first, and carries the
 * idempotency key created at that confirmation, reused for every retry of the
 * same request. The backend decides every one and refuses what it will not
 * do -- a release that can still land, a stable asset's own units, chains
 * that still disagree -- and the console shows each refusal with its code and
 * request id. A refund that is not final yet answers so; asking again only
 * observes it.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useBridgeLegAction,
  type BridgeLegActionOutcome,
  type BridgeLegActionRequest,
  type OpsBridgeLeg,
  type OpsOverview,
} from "../../../api/advanced/operator.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { bridgeLegOffers, checkOperatorReason, type BridgeLegOffer } from "../../../lib/operator.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { legHref } from "../bridge/paths.ts";
import { CommandFlow, type Prepared } from "../margin/parts.tsx";
import type { OverviewQuery } from "./panels.tsx";

/** The legs in review, as a part of the console's "awaiting an operator" panel. */
export function BridgeLegsInReview(props: { readonly overview: OverviewQuery }): ReactNode {
  return (
    <AsyncPanel
      query={props.overview}
      loadingLabel="Loading the bridge legs in review…"
      empty={{
        isEmpty: (overview: OpsOverview) => overview.bridge_legs_in_review.length === 0,
        title: "No bridge leg awaits an operator",
        body: "The backend reports no bridge leg in MANUAL_REVIEW.",
      }}
    >
      {(overview: OpsOverview) => (
        <div className="stack" data-list="bridge-legs-in-review">
          {overview.bridge_legs_in_review.map((leg) => (
            <BridgeLegCard key={leg.id} leg={leg} />
          ))}
        </div>
      )}
    </AsyncPanel>
  );
}

function BridgeLegCard(props: { readonly leg: OpsBridgeLeg }): ReactNode {
  const { leg } = props;
  const [reason, setReason] = useState("");
  const id = `ops-bridge-leg-${leg.id}`;
  const why = checkOperatorReason(reason);
  return (
    <article
      className="callout"
      aria-label={`Bridge leg ${leg.id} in review`}
      data-bridge-leg={leg.id}
      data-released={String(leg.released)}
      data-refund-requested={String(leg.refund_requested)}
    >
      <FieldGrid columns={3}>
        <Field label="Leg">
          <Link to={legHref(leg.id)}>
            <code className="mono-small">{leg.id}</code>
          </Link>
        </Field>
        <Field label="Route">
          {leg.route_name}
          <span className="cell-note">
            <Identifier value={leg.route_id} />
          </span>
        </Field>
        <Field label="Account">
          <Identifier value={leg.account_id} />
        </Field>
        <Field label="Quantity" note="Units of the route's source asset.">
          <span data-field="bridge-leg-quantity">
            <Qty value={leg.quantity} decimals={leg.decimals} symbol={leg.source_symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={leg.quantity} />
          </span>
        </Field>
        <Field
          label="In review since"
          {...(leg.entered_review_at === undefined ? {} : { note: exactInstant(leg.entered_review_at) })}
        >
          {leg.entered_review_at === undefined ? <span className="absent">not recorded</span> : formatInstant(leg.entered_review_at)}
        </Field>
        <Field label="Why it is in review">
          {leg.review_reason ?? <span className="absent">no reason recorded</span>}
        </Field>
        <Field label="Destination">
          {leg.released ? <Pill tone="warn">released</Pill> : <Pill tone="info">never released</Pill>}
          {leg.refund_requested && (
            <span className="cell-note">
              <Pill tone="warn">refund requested</Pill>
            </span>
          )}
        </Field>
      </FieldGrid>
      <div className="form-row">
        <label htmlFor={`${id}-reason`}>Reason for the action</label>
        <textarea
          id={`${id}-reason`}
          className="input"
          rows={2}
          value={reason}
          aria-describedby={`${id}-reason-note`}
          onChange={(event) => {
            setReason(event.target.value);
          }}
        />
        <p id={`${id}-reason-note`} className="field-note">
          Every action states its reason; it is recorded with the leg's transition.
        </p>
      </div>
      {bridgeLegOffers(leg).map((offer) => (
        <LegAction
          key={offer.action}
          leg={leg}
          offer={offer}
          prepared={
            why.ok
              ? { ok: true, request: { legId: leg.id, action: offer.action, reason: why.value } }
              : { ok: false, reason: why.error }
          }
        />
      ))}
    </article>
  );
}

/** One action on a leg: offered with its review and confirmation, or not offered and saying why. */
function LegAction(props: {
  readonly leg: OpsBridgeLeg;
  readonly offer: BridgeLegOffer;
  readonly prepared: Prepared<BridgeLegActionRequest>;
}): ReactNode {
  const { offer } = props;
  // Each action has its own mutation, so one action's answer is never shown as another's.
  const command = useBridgeLegAction();
  return (
    <div className="form" role="group" aria-label={`${offer.label}: bridge leg ${props.leg.id}`} data-leg-action={offer.action}>
      <p className="field-note">{offer.note}</p>
      {offer.blocked !== undefined ? (
        <div className="form-actions">
          <Button disabledReason={offer.blocked}>{offer.label}</Button>
        </div>
      ) : (
        <CommandFlow<BridgeLegActionRequest, BridgeLegActionOutcome>
          label={offer.label}
          variant={offer.action === "refund" ? "danger" : "primary"}
          prepared={props.prepared}
          command={command}
          refusedNote="The leg is unchanged."
          describe={(request) => (
            <FieldGrid columns={2}>
              <Field label="Action">
                <code className="mono-small">{request.action}</code>
              </Field>
              <Field label="Reason">{request.reason}</Field>
            </FieldGrid>
          )}
          renderResult={(outcome) => <LegOutcome outcome={outcome} />}
        />
      )}
    </div>
  );
}

function LegOutcome(props: { readonly outcome: BridgeLegActionOutcome }): ReactNode {
  const { outcome } = props;
  return (
    <p
      className="note"
      role="status"
      aria-label="The leg after the action"
      data-leg-outcome={outcome.leg.id}
      data-leg-status={outcome.leg.status}
    >
      {outcome.replay === true ? "The backend had already recorded this refund and submitted nothing again. " : ""}
      The leg is <Pill tone="info">{outcome.leg.status}</Pill>
      {outcome.leg.failure_code !== undefined && (
        <>
          {" "}
          with code <code className="mono-small">{outcome.leg.failure_code}</code>
        </>
      )}
      {outcome.action === "refund" &&
        (outcome.settled === true
          ? "; the refund is final."
          : "; the refund is not final yet. Ask again to observe it: a refund is never sent twice.")}
      {outcome.action !== "refund" && "."}
    </p>
  );
}
