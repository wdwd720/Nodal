/**
 * One transfer, as the backend shows it to this reader, with the evidence
 * trail that stands behind it: the economic intent it was recorded under,
 * the exactly-once effect that applied it, the journal postings, the decision
 * evidence and the Risk Kernel V2 decision.
 *
 * The backend answers NOT_FOUND both for an id it never recorded and for a
 * transfer this session may not read -- a stranger's, or a refused attempt
 * addressed to the reader -- so the page neither confirms nor denies that the
 * id exists.
 *
 * An operator is offered the one correction a transfer has: reversal, a new
 * compensating journal, under dual control -- one operator proposes it and a
 * second, different operator approves it (./reversals.tsx). Whether either
 * may (the operator's permissions, a recent strong sign-in, the recipient
 * still holding the funds) is the backend's decision, and its refusal is
 * shown with its code.
 */
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { useTransfer, type AdvancedTransfer } from "../../../api/advanced/transfers.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import {
  TRANSFER_STATUS_MEANING,
  transferSide,
  transferStatusTone,
  type TransferSide,
} from "../../../lib/transfers.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import { Codes, ModePill, ValueDomainPill } from "../shared.tsx";
import { ReversalPanel } from "./reversals.tsx";

export function TransferDetail(): ReactNode {
  const { transferId } = useParams();
  const account = useAdvancedAccount();
  const transfer = useTransfer(account.accountId, transferId);
  return (
    <Page
      title="Transfer"
      lead="One transfer, exactly as the backend shows it to you, and the records that stand behind it."
      actions={<LinkButton to={advancedHref("transfers")}>Back to transfers</LinkButton>}
    >
      <SimulatedNotice />
      <Panel title="Transfer" {...(transferId === undefined ? {} : { description: `Requested by id ${transferId}.` })}>
        <AsyncPanel
          query={transfer}
          loadingLabel="Loading the transfer…"
          errorExtra={
            <p className="explain-body">
              The backend answers NOT_FOUND both for an id it has never recorded and for a transfer this session may
              not read, so the answer neither confirms nor denies that the transfer exists.
            </p>
          }
        >
          {(found: AdvancedTransfer) => <TransferFields transfer={found} side={transferSide(found, account.accountId)} />}
        </AsyncPanel>
      </Panel>
      {transfer.isSuccess && (
        <>
          <EvidencePanel transfer={transfer.data} side={transferSide(transfer.data, account.accountId)} />
          {account.canInspect && <ReversalPanel transfer={transfer.data} />}
        </>
      )}
    </Page>
  );
}

function TransferFields(props: { readonly transfer: AdvancedTransfer; readonly side: TransferSide }): ReactNode {
  const { transfer } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Status" note={TRANSFER_STATUS_MEANING[transfer.status]}>
          <span data-field="transfer-status">
            <Pill tone={transferStatusTone(transfer.status)}>{transfer.status}</Pill>
          </span>
        </Field>
        <Field label="Amount" emphasis>
          <span data-field="transfer-quantity">
            <Qty value={transfer.quantity} decimals={transfer.asset_decimals} symbol={transfer.asset_symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={transfer.quantity} />
          </span>
        </Field>
        <Field
          label="Valued for the transfer limits at"
          note="The backend's valuation of the amount, rounded up, which the policy's limits are checked against. Simulated value, not money."
        >
          <Usd value={transfer.notional_usd} />
        </Field>
        <Field label="Your side">
          {props.side === "sent" ? "You sent it" : props.side === "received" ? "You received it" : "Read as an operator"}
        </Field>
        <Field label="From">
          <Identifier value={transfer.sender_account_id} />
        </Field>
        <Field
          label="To"
          {...(transfer.recipient_account_id === undefined
            ? { note: "Paid by handle: the backend does not show the sender the account behind it." }
            : {})}
        >
          {transfer.recipient_handle !== undefined && <code className="mono-small">@{transfer.recipient_handle}</code>}
          {transfer.recipient_account_id !== undefined && <Identifier value={transfer.recipient_account_id} />}
        </Field>
        <Field label="Mode" note={ADVANCED_MODE_DESCRIPTIONS[transfer.mode] ?? transfer.mode}>
          <ModePill mode={transfer.mode} />
        </Field>
        <Field label="Memo" note="Visible to the two parties only.">
          {transfer.memo ?? <span className="absent">none</span>}
        </Field>
        <Field label="Recorded" note={exactInstant(transfer.created_at)}>
          {formatInstant(transfer.created_at)}
        </Field>
        <Field label="Decided" {...noteFor(transfer.decided_at === undefined ? undefined : exactInstant(transfer.decided_at))}>
          {transfer.decided_at === undefined ? <span className="absent">not recorded</span> : formatInstant(transfer.decided_at)}
        </Field>
      </FieldGrid>
      {transfer.status === "REJECTED" && (
        <div className="callout" role="note" aria-label="Why it was refused">
          <p>
            Refused with <code className="mono-small">{transfer.rejection_code ?? "no code recorded"}</code>. Every
            code that applied, primary first:
          </p>
          <Codes codes={transfer.reason_codes} none="none recorded" />
        </div>
      )}
      <p className="meta-row">
        <ValueDomainPill value={transfer.value_domain} />
        <span className="mono-small">
          policy {transfer.policy_version} · hash {transfer.policy_hash}
        </span>
      </p>
    </>
  );
}

/**
 * The records behind the transfer. A link is drawn where the API can answer
 * it for this reader: the intent belongs to the sender's account, so it is
 * linked for the sender (an operator reads it too).
 */
function EvidencePanel(props: { readonly transfer: AdvancedTransfer; readonly side: TransferSide }): ReactNode {
  const { transfer } = props;
  const readsIntent = props.side !== "received";
  return (
    <Panel
      title="Evidence trail"
      description="The economic intent, the exactly-once effect, the journal postings and the archived decision evidence this transfer is recorded with."
    >
      <FieldGrid columns={3}>
        <Field label="Economic intent" note="The intent the sender's request was recorded as, with its content hash and status history.">
          {readsIntent ? (
            <Link to={advancedHref("activity", "intents", transfer.intent_id)} data-evidence="intent">
              <code className="mono-small">{transfer.intent_id}</code>
            </Link>
          ) : (
            <Identifier value={transfer.intent_id} />
          )}
        </Field>
        <Field label="Economic effect" note="The exactly-once register entry that applied the posting.">
          {transfer.effect_id === undefined ? (
            <span className="absent">none: nothing was applied</span>
          ) : (
            <Link to={advancedHref("activity")} data-evidence="effect">
              <code className="mono-small">{transfer.effect_id}</code>
            </Link>
          )}
        </Field>
        <Field label="Journal" note="The P2P_TRANSFER posting that moved the value.">
          {transfer.journal_transaction_id === undefined ? (
            <span className="absent">none: nothing was posted</span>
          ) : (
            <Identifier value={transfer.journal_transaction_id} />
          )}
        </Field>
        <Field label="Decision evidence" note="The archived, hashed document of everything the decision depended on.">
          {transfer.evidence_ref === undefined ? (
            <span className="absent">not shown to you</span>
          ) : (
            <>
              <Identifier value={transfer.evidence_ref} />
              <span className="cell-note">
                sha256 <Identifier value={transfer.evidence_sha256} />
              </span>
            </>
          )}
        </Field>
        <Field label="Risk decision" note="Risk Kernel V2's verdict on the intent, listed with its hashes on the economic activity page.">
          {transfer.risk_decision_id === undefined ? (
            <span className="absent">not shown to you</span>
          ) : (
            <Link to={advancedHref("activity")} data-evidence="risk-decision">
              <code className="mono-small">{transfer.risk_decision_id}</code>
            </Link>
          )}
        </Field>
        <Field label="Reversal" {...noteFor(transfer.reversed_at === undefined ? undefined : exactInstant(transfer.reversed_at))}>
          {transfer.reversal_journal_transaction_id === undefined ? (
            <span className="absent">not reversed</span>
          ) : (
            <>
              <Identifier label="compensating journal" value={transfer.reversal_journal_transaction_id} />
              {transfer.reversal_reason_code !== undefined && (
                <span className="cell-note">
                  reason <code className="mono-small">{transfer.reversal_reason_code}</code>
                </span>
              )}
              {transfer.reversal_proposal_id !== undefined && (
                <span className="cell-note" data-field="reversal-proposal">
                  approved proposal <code className="mono-small">{transfer.reversal_proposal_id}</code>
                </span>
              )}
            </>
          )}
        </Field>
      </FieldGrid>
    </Panel>
  );
}

/** A Field note only when there is one: an absent note is omitted, not passed as undefined. */
function noteFor(note: string | undefined): { readonly note?: string } {
  return note === undefined ? {} : { note };
}
