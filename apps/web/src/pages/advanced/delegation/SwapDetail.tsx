/**
 * One signing request: the swap an agent asked for, the plan it would
 * execute (exactly as hashed), the signing provider's inspection of the
 * compiled transaction against that plan, and the decision trail.
 *
 * A swap awaiting the owner takes a decision bound to the plan hash shown
 * here; an approval is executed at once by the service, and the answer says
 * how that execution ended. The backend answers NOT_FOUND both for an id it
 * never recorded and for a swap this session may not read.
 */
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useDecideSwap,
  useDelegationSwap,
  type DelegationSwap,
  type DelegationSwapDecisionResult,
} from "../../../api/advanced/delegation.ts";
import { Button, LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  ACTION_CLASS_LABEL,
  SWAP_STATUS_MEANING,
  awaitsDecision,
  noDecisionReason,
  planPriceText,
  subjectStatusTone,
} from "../../../lib/delegation.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { ModePill, ValueDomainPill } from "../shared.tsx";
import {
  ApprovalFields,
  DecisionForm,
  DelegationNav,
  ExecutionOutcome,
  InspectingNote,
  InspectionFields,
  TransitionTable,
  delegationHref,
} from "./parts.tsx";

export function SwapDetail(): ReactNode {
  const { swapId } = useParams();
  const account = useAdvancedAccount();
  const swap = useDelegationSwap(account.accountId, swapId);
  return (
    <Page
      title="Signing request"
      lead="A swap an agent asked for, the plan it would execute, the inspection of the signed transaction against that plan, and who decided what."
      actions={<LinkButton to={delegationHref()}>Back to agent authority</LinkButton>}
    >
      <SimulatedNotice />
      <DelegationNav />
      <Panel title="Request" {...(swapId === undefined ? {} : { description: `Requested by id ${swapId}.` })}>
        <AsyncPanel
          query={swap}
          loadingLabel="Loading the signing request…"
          errorExtra={
            <p className="explain-body">
              The backend answers NOT_FOUND both for an id it has never recorded and for a request this session may not read.
            </p>
          }
        >
          {(found: DelegationSwap) => <SwapFields swap={found} />}
        </AsyncPanel>
      </Panel>
      {swap.isSuccess && (
        <>
          <DecisionPanel swap={swap.data} />
          <PlanPanel swap={swap.data} />
          <Panel
            title="Inspection"
            description="The signing provider's one decision: the capability checked again, the compiled transaction inspected field by field against the plan, then the signing policy."
          >
            <InspectionFields inspection={swap.data.inspection} />
          </Panel>
          <Panel title="Decision trail" description="The owner's decision, when there is one, and every status change.">
            <ApprovalFields approval={swap.data.approval} />
            <TransitionTable transitions={swap.data.transitions} />
          </Panel>
        </>
      )}
    </Page>
  );
}

function SwapFields(props: { readonly swap: DelegationSwap }): ReactNode {
  const { swap } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Status" note={SWAP_STATUS_MEANING[swap.status]} emphasis>
          <span data-field="swap-status">
            <Pill tone={subjectStatusTone(swap.status)}>{swap.status}</Pill>
          </span>
          {swap.failure_code !== undefined && <span className="cell-note mono-small">{swap.failure_code}</span>}
        </Field>
        <Field label="Pays">
          <Qty value={swap.amount_in} decimals={swap.pay_asset.decimals} symbol={swap.pay_asset.symbol} />
          <span className="cell-note">
            <BaseUnits value={swap.amount_in} />
          </span>
        </Field>
        <Field label="Receives" note="At the venue's quote; never less than the minimum below.">
          <Qty value={swap.amount_out} decimals={swap.receive_asset.decimals} symbol={swap.receive_asset.symbol} />
          <span className="cell-note">
            at least <Qty value={swap.min_amount_out} decimals={swap.receive_asset.decimals} symbol={swap.receive_asset.symbol} />
          </span>
        </Field>
        <Field label="Valued at" note="What the request counts against the capability's limits.">
          <span data-field="notional">
            <Usd value={swap.notional_usd} />
          </span>
        </Field>
        <Field label="Action">{ACTION_CLASS_LABEL[swap.action_class]}</Field>
        <Field label="Mode">
          <ModePill mode={swap.mode} />
        </Field>
        <Field label="Venue" note={swap.venue.address}>
          {swap.venue.name}
        </Field>
        <Field label="Requested" note={exactInstant(swap.requested_at)}>
          {formatInstant(swap.requested_at)}
        </Field>
        <Field label="Quote valid until" note="Nothing is signed against an expired quote.">
          {formatInstant(swap.quote_expires_at)}
        </Field>
        <Field label="Capability">
          <Link to={delegationHref("capabilities", swap.capability_id)}>{swap.capability_id}</Link>
        </Field>
        <Field label="Agent">
          <Identifier value={swap.agent_id} />
        </Field>
        <Field label="Your approval">{swap.requires_approval ? "Required by the capability" : "Not required"}</Field>
        <Field label="Transaction" note="On the simulated chain; present once signed.">
          <Identifier value={swap.tx_id} />
        </Field>
        <Field label="Journal" note="The posting of a settled swap.">
          <Identifier value={swap.journal_transaction_id} />
        </Field>
        <Field label="Delegated wallet">
          <Identifier value={swap.wallet} />
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <ValueDomainPill value={swap.value_domain} />
        <span className="mono-small">intent {swap.intent_id}</span>
      </p>
    </>
  );
}

function PlanPanel(props: { readonly swap: DelegationSwap }): ReactNode {
  const { plan } = props.swap;
  return (
    <Panel
      title="Plan"
      description="The settlement plan exactly as it was hashed: a decision binds this hash, and the transaction is signed only if it is exactly this plan."
    >
      <FieldGrid columns={3}>
        <Field label="Plan hash" emphasis>
          <span data-field="plan-hash">
            <Identifier value={props.swap.plan_hash} />
          </span>
        </Field>
        <Field label="Plan version">
          <code className="mono-small">{plan.version}</code>
        </Field>
        <Field label="Capability hash" note="The grant the plan was made under.">
          <Identifier value={plan.capability_hash} />
        </Field>
        <Field label="Amount in (base units)">
          <BaseUnits value={plan.amount_in} />
        </Field>
        <Field label="Amount out (base units)">
          <BaseUnits value={plan.amount_out} />
        </Field>
        <Field label="Minimum out (base units)">
          <BaseUnits value={plan.min_amount_out} />
        </Field>
        <Field label="Pay price" note={`${plan.pay_price.source}, observed ${formatInstant(plan.pay_price.observed_at)}`}>
          <code className="mono-small">{planPriceText(plan.pay_price)}</code>
        </Field>
        <Field label="Receive price" note={`${plan.receive_price.source}, observed ${formatInstant(plan.receive_price.observed_at)}`}>
          <code className="mono-small">{planPriceText(plan.receive_price)}</code>
        </Field>
        <Field label="Spread and slippage" note="Basis points, as the plan states them.">
          <code className="mono-small">
            spread {plan.spread_bps} · slippage at most {plan.max_slippage_bps}
          </code>
        </Field>
        <Field label="Valued at">
          <Usd value={plan.notional_usd} />
        </Field>
        <Field label="Network fee (native base units)">
          <BaseUnits value={plan.fee_per_tx} />
        </Field>
        <Field label="Chain and nonce">
          <code className="mono-small">
            {plan.chain} · request {plan.request_nonce}
          </code>
        </Field>
        <Field label="Wallet pays from">
          <Identifier value={plan.wallet} />
        </Field>
        <Field label="Venue settles at">
          <Identifier value={plan.venue_address} />
        </Field>
        <Field label="Mints">
          <code className="mono-small">pay {plan.pay_mint}</code>
          <span className="cell-note mono-small">receive {plan.receive_mint}</span>
        </Field>
      </FieldGrid>
    </Panel>
  );
}

function DecisionPanel(props: { readonly swap: DelegationSwap }): ReactNode {
  const { swap } = props;
  const account = useAdvancedAccount();
  const decide = useDecideSwap();
  const own = account.ownAccountId !== undefined && account.ownAccountId.toLowerCase() === swap.account_id.toLowerCase();
  return (
    <Panel
      title="Your decision"
      description="Approve or decline this exact plan. Approving needs your recent strong sign-in; the service then signs it only after the inspector proves the transaction is this plan, submits it once and settles it."
    >
      {!own ? (
        <InspectingNote what="decide a signing request" />
      ) : !awaitsDecision(swap.status) && !decide.isSuccess ? (
        <div className="form-actions">
          <Button variant="primary" disabledReason={noDecisionReason(swap.status)}>
            Approve this plan
          </Button>
        </div>
      ) : (
        <DecisionForm
          id={swap.id}
          planHash={swap.plan_hash}
          what="signing request"
          approveNote="The service then inspects, signs, submits once and settles it; its answer says how that ended."
          command={decide}
          done={(result: DelegationSwapDecisionResult) => (
            <ExecutionOutcome execution={result.execution} status={result.swap.status} />
          )}
        />
      )}
    </Panel>
  );
}
