/**
 * The payout a settled or voided market holds for the account the area reads,
 * and the claim of it.
 *
 * A settlement never waits for one holder: when the backend could not deliver
 * a holder's payout (a frozen or closed account, an unverified identity), the
 * holder's shares were retired like everyone else's and the amount is held in
 * the market's escrow. The account's owner claims it once eligible; the
 * backend checks the claim again and delivers it once. The claim acts only on
 * the session's own account, never on an account an operator is inspecting.
 * Every figure is the backend's base-unit string.
 */
import type { ReactNode } from "react";

import {
  useClaimHeldPayout,
  type ClaimCommand,
  type PredictionClaimResult,
  type PredictionHeldPayout,
  type PredictionMarket,
} from "../../../api/advanced/predmarket.ts";
import { Button } from "../../../components/Button.tsx";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { intentStatusTone } from "../../../lib/advanced.ts";
import { HELD_PAYOUT_NOTE, claimable } from "../../../lib/predmarket.ts";
import { formatInstant } from "../../../lib/time.ts";
import { Codes } from "../shared.tsx";
import { useConfirmation } from "./confirm.ts";
import { ActorLine, CommandFailure, KeyLine } from "./parts.tsx";

export function HeldPayoutSection(props: {
  readonly market: PredictionMarket;
  readonly held: PredictionHeldPayout;
  /** The account the area reads. */
  readonly accountId: string;
  /** The session's own account; the claim is offered only on it. */
  readonly ownAccountId: string | undefined;
}): ReactNode {
  const { market, held } = props;
  const amount = (value: string): ReactNode => (
    <Qty value={value} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />
  );
  return (
    <section className="callout" aria-label="Held payout">
      <h3>Held payout</h3>
      <p className="panel-description">{HELD_PAYOUT_NOTE}</p>
      <FieldGrid columns={3}>
        <Field label="Held for this account">
          {amount(held.amount)} <BaseUnits value={held.amount} />
        </Field>
        <Field label="From the market's">{held.kind === "REFUND" ? "void refund" : "settlement payout"}</Field>
        <Field label="State">
          <Pill tone={held.claimed ? "good" : "warn"}>{held.claimed ? "CLAIMED" : "HELD"}</Pill>
        </Field>
        <Field label="Why it was held" note="The risk kernel's codes for the delivery it refused.">
          <Codes codes={held.reason_codes} />
        </Field>
        <Field label="Held">{formatInstant(held.held_at)}</Field>
        <Field label="Shares retired by">
          <Identifier value={held.journal_transaction_id} />
        </Field>
        {held.claim !== undefined && (
          <>
            <Field label="Claimed">{formatInstant(held.claim.claimed_at)}</Field>
            <Field label="Claimed by">
              <ActorLine actor={held.claim.claimed_by} />
            </Field>
            <Field label="Delivered by">
              <Identifier value={held.claim.journal_transaction_id} />
            </Field>
          </>
        )}
      </FieldGrid>
      {claimable(held) && <ClaimForm market={market} accountId={props.accountId} ownAccountId={props.ownAccountId} />}
    </section>
  );
}

function ClaimForm(props: {
  readonly market: PredictionMarket;
  readonly accountId: string;
  readonly ownAccountId: string | undefined;
}): ReactNode {
  const claim = useClaimHeldPayout();
  const confirmation = useConfirmation<ClaimCommand>();
  const own = props.ownAccountId;
  const unavailable =
    own === undefined
      ? "This session owns no account. A held payout is claimed only by the account it is held for."
      : own !== props.accountId
        ? "A held payout is claimed only by the account it is held for; you are reading another account."
        : undefined;
  const startOver = (): void => {
    confirmation.reset();
    claim.reset();
  };
  const submit = (): void => {
    if (own === undefined) return;
    claim.mutate(confirmation.confirm((key) => ({ marketId: props.market.id, accountId: own, idempotencyKey: key })));
  };

  if (claim.isSuccess) {
    return <ClaimOutcome result={claim.data} market={props.market} />;
  }
  return (
    <form
      className="form"
      onSubmit={(event) => {
        event.preventDefault();
        submit();
      }}
    >
      <KeyLine confirmation={confirmation} />
      {!claim.isError && (
        <div className="form-actions">
          {unavailable !== undefined ? (
            <Button variant="primary" disabledReason={unavailable}>
              Claim the held payout
            </Button>
          ) : (
            <Button variant="primary" submit busy={claim.isPending} busyLabel="Asking the backend…">
              Claim the held payout
            </Button>
          )}
        </div>
      )}
      {claim.isError && (
        <CommandFailure
          error={claim.error}
          onRetry={submit}
          onStartOver={startOver}
          nothingHappened="Nothing was delivered by this attempt: the payout is still held. A refused claim is kept on record as a rejected economic intent."
        />
      )}
    </form>
  );
}

function ClaimOutcome(props: { readonly result: PredictionClaimResult; readonly market: PredictionMarket }): ReactNode {
  const { result, market } = props;
  return (
    <div role="status">
      {result.replay && (
        <p className="callout">The backend had already carried out this claim; this is its first answer.</p>
      )}
      <FieldGrid columns={3}>
        <Field label="Delivered">
          <Qty value={result.held_payout.amount} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />
        </Field>
        <Field label="Economic intent">
          <Pill tone={intentStatusTone(result.intent.status)}>{result.intent.status}</Pill>
          <Identifier value={result.intent.id} />
        </Field>
        <Field label="Journal transaction">
          <Identifier value={result.journal_transaction_id} />
        </Field>
      </FieldGrid>
    </div>
  );
}
