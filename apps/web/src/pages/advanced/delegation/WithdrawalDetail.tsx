/**
 * One withdrawal an agent proposed: where it would go, what the account's
 * policy said about it, the plan frozen for it, the inspection of the signed
 * transaction against that plan, and the decision trail.
 *
 * An agent may only propose. A withdrawal awaiting the owner takes a decision
 * bound to the plan hash shown here; an approval past the policy's cooling
 * period is executed at once by the service. Until it is signed, the owner
 * may cancel it. The backend answers NOT_FOUND both for an id it never
 * recorded and for a withdrawal this session may not read.
 */
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useCancelWithdrawal,
  useDecideWithdrawal,
  useDelegationWithdrawal,
  type DelegationWithdrawal,
  type DelegationWithdrawalDecisionResult,
} from "../../../api/advanced/delegation.ts";
import { Button, LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { formatSeconds } from "../../../lib/advanced.ts";
import {
  EVALUATION_OUTCOME_MEANING,
  WITHDRAWAL_STATUS_MEANING,
  awaitsDecision,
  cancellable,
  noDecisionReason,
  notCancellableReason,
  planPriceText,
  subjectStatusTone,
} from "../../../lib/delegation.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { Codes, ModePill, ValueDomainPill } from "../shared.tsx";
import {
  ApprovalFields,
  DecisionForm,
  DelegationNav,
  ExecutionOutcome,
  InspectingNote,
  InspectionFields,
  ReasonCommand,
  TransitionTable,
  delegationHref,
} from "./parts.tsx";

export function WithdrawalDetail(): ReactNode {
  const { withdrawalId } = useParams();
  const account = useAdvancedAccount();
  const withdrawal = useDelegationWithdrawal(account.accountId, withdrawalId);
  return (
    <Page
      title="Withdrawal"
      lead="A withdrawal an agent proposed: its destination, what your policy said, the plan frozen for it, the inspection of the signed transaction, and who decided what."
      actions={<LinkButton to={delegationHref()}>Back to agent authority</LinkButton>}
    >
      <SimulatedNotice />
      <DelegationNav />
      <Panel title="Withdrawal" {...(withdrawalId === undefined ? {} : { description: `Requested by id ${withdrawalId}.` })}>
        <AsyncPanel
          query={withdrawal}
          loadingLabel="Loading the withdrawal…"
          errorExtra={
            <p className="explain-body">
              The backend answers NOT_FOUND both for an id it has never recorded and for a withdrawal this session may not read.
            </p>
          }
        >
          {(found: DelegationWithdrawal) => <WithdrawalFields withdrawal={found} />}
        </AsyncPanel>
      </Panel>
      {withdrawal.isSuccess && (
        <>
          <DecisionPanel withdrawal={withdrawal.data} />
          <EvaluationPanel withdrawal={withdrawal.data} />
          <PlanPanel withdrawal={withdrawal.data} />
          <Panel
            title="Inspection"
            description="The signing provider's one decision: the capability checked again, the compiled transaction inspected against the frozen plan and the allow-listed destination, then the signing policy."
          >
            <InspectionFields inspection={withdrawal.data.inspection} />
          </Panel>
          <Panel title="Decision trail" description="The decision -- yours, or the policy's in a test environment -- and every status change.">
            <ApprovalFields approval={withdrawal.data.approval} />
            <TransitionTable transitions={withdrawal.data.transitions} />
          </Panel>
          <CancelPanel withdrawal={withdrawal.data} />
        </>
      )}
    </Page>
  );
}

function WithdrawalFields(props: { readonly withdrawal: DelegationWithdrawal }): ReactNode {
  const { withdrawal } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Status" note={WITHDRAWAL_STATUS_MEANING[withdrawal.status]} emphasis>
          <span data-field="withdrawal-status">
            <Pill tone={subjectStatusTone(withdrawal.status)}>{withdrawal.status}</Pill>
          </span>
          {withdrawal.failure_code !== undefined && <span className="cell-note mono-small">{withdrawal.failure_code}</span>}
        </Field>
        <Field label="Amount">
          <Qty value={withdrawal.amount} decimals={withdrawal.asset.decimals} symbol={withdrawal.asset.symbol} />
          <span className="cell-note">
            <BaseUnits value={withdrawal.amount} />
          </span>
        </Field>
        <Field label="Valued at">
          <span data-field="notional">
            <Usd value={withdrawal.notional_usd} />
          </span>
        </Field>
        <Field label="Destination" note={withdrawal.destination_address}>
          {withdrawal.destination.label}{" "}
          <Pill tone={withdrawal.destination.status === "ACTIVE" ? "good" : "warn"}>{withdrawal.destination.status}</Pill>
        </Field>
        <Field label="Chain and mode">
          <code className="mono-small">{withdrawal.chain}</code> <ModePill mode={withdrawal.mode} />
        </Field>
        <Field label="Environment">{withdrawal.environment}</Field>
        <Field label="Proposed" note={exactInstant(withdrawal.proposed_at)}>
          {formatInstant(withdrawal.proposed_at)}
        </Field>
        <Field label="Decide before" note="A withdrawal nobody decides by then is ended and its funds returned.">
          {formatInstant(withdrawal.approval_expires_at)}
        </Field>
        <Field label="Signed no earlier than" note="The end of your policy's cooling period.">
          {formatInstant(withdrawal.executable_at)}
        </Field>
        <Field label="Capability">
          <Link to={delegationHref("capabilities", withdrawal.capability_id)}>{withdrawal.capability_id}</Link>
        </Field>
        <Field label="Proposed by agent">
          <Identifier value={withdrawal.agent_id} />
        </Field>
        <Field label="Transaction" note="On the simulated chain; present once signed.">
          <Identifier value={withdrawal.tx_id} />
        </Field>
        <Field label="Journal" note="The posting of a settled withdrawal.">
          <Identifier value={withdrawal.journal_transaction_id} />
        </Field>
        <Field label="Delegated wallet">
          <Identifier value={withdrawal.wallet} />
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <ValueDomainPill value={withdrawal.value_domain} />
        <span className="mono-small">intent {withdrawal.intent_id}</span>
      </p>
    </>
  );
}

function EvaluationPanel(props: { readonly withdrawal: DelegationWithdrawal }): ReactNode {
  const { evaluation } = props.withdrawal;
  return (
    <Panel
      title="What your policy said"
      description="The withdrawal policy's evaluation, recorded with the proposal: hard limits refuse, and anything else either waits for you or, in a test environment with every automatic bound met, is approved by the policy."
    >
      <FieldGrid columns={3}>
        <Field label="Outcome" note={EVALUATION_OUTCOME_MEANING[evaluation.outcome]} emphasis>
          <span data-field="evaluation-outcome">
            <Pill tone={evaluation.outcome === "AUTO_APPROVE" ? "info" : "warn"}>{evaluation.outcome}</Pill>
          </span>
        </Field>
        <Field label="Risk score" note="0 to 100, the sum of the policy's measured risk factors.">
          <span className="num">{String(evaluation.risk_score)}</span>
        </Field>
        <Field label="Reason codes">
          <Codes codes={evaluation.reason_codes} none="none" />
        </Field>
        <Field label="Anomalies">
          <Codes codes={evaluation.anomalies} none="none measured" />
        </Field>
        <Field label="Destination age">{formatSeconds(evaluation.destination_age_seconds)}</Field>
        <Field label="Account age">{formatSeconds(evaluation.account_age_seconds)}</Field>
        <Field label="Withdrawn earlier that day">
          <Usd value={evaluation.day_total_before_usd} />
        </Field>
        <Field label="Proposals in the 24 hours before">
          <span className="num">{String(evaluation.count_last_24h)}</span>
        </Field>
        <Field label="Compliance clear" note="A verified identity and a clear sanctions screen.">
          {evaluation.compliance_clear ? "yes" : "no"}
        </Field>
        <Field label="Policy version" note={`hash ${evaluation.policy_hash}`}>
          <code className="mono-small">{evaluation.policy_version}</code>
        </Field>
        <Field label="Environment">{evaluation.environment}</Field>
      </FieldGrid>
    </Panel>
  );
}

function PlanPanel(props: { readonly withdrawal: DelegationWithdrawal }): ReactNode {
  const { plan } = props.withdrawal;
  return (
    <Panel
      title="Plan"
      description="The plan frozen for the withdrawal, exactly as hashed: the destination and amount cannot change after a decision, and the transaction is signed only if it is exactly this plan."
    >
      <FieldGrid columns={3}>
        <Field label="Plan hash" emphasis>
          <span data-field="plan-hash">
            <Identifier value={props.withdrawal.plan_hash} />
          </span>
        </Field>
        <Field label="Destination address">
          <Identifier value={plan.destination_address} />
        </Field>
        <Field label="Amount (base units)">
          <BaseUnits value={plan.amount} />
        </Field>
        <Field label="Valued at">
          <Usd value={plan.notional_usd} />
        </Field>
        <Field label="Price" note={`${plan.price.source}, observed ${formatInstant(plan.price.observed_at)}`}>
          <code className="mono-small">{planPriceText(plan.price)}</code>
        </Field>
        <Field label="Network fee (native base units)">
          <BaseUnits value={plan.fee_per_tx} />
        </Field>
        <Field label="Policy" note={`hash ${plan.policy_hash}`}>
          <code className="mono-small">
            version {plan.policy_version} · {plan.evaluation_outcome} · risk {plan.risk_score}
          </code>
        </Field>
        <Field label="Capability hash">
          <Identifier value={plan.capability_hash} />
        </Field>
        <Field label="Chain, mint and nonce">
          <code className="mono-small">
            {plan.chain} · {plan.mint} · proposal {plan.proposal_nonce}
          </code>
        </Field>
      </FieldGrid>
    </Panel>
  );
}

function DecisionPanel(props: { readonly withdrawal: DelegationWithdrawal }): ReactNode {
  const { withdrawal } = props;
  const account = useAdvancedAccount();
  const decide = useDecideWithdrawal();
  const own = account.ownAccountId !== undefined && account.ownAccountId.toLowerCase() === withdrawal.account_id.toLowerCase();
  return (
    <Panel
      title="Your decision"
      description="Approve or decline this exact plan. It needs your recent strong sign-in -- and, when your policy asks for one, a sign-in within its own window. An approved withdrawal is signed once its cooling period has passed, only after the inspector proves the transaction."
    >
      {!own ? (
        <InspectingNote what="decide a withdrawal" />
      ) : !awaitsDecision(withdrawal.status) && !decide.isSuccess ? (
        <div className="form-actions">
          <Button variant="primary" disabledReason={noDecisionReason(withdrawal.status)}>
            Approve this plan
          </Button>
        </div>
      ) : (
        <DecisionForm
          id={withdrawal.id}
          planHash={withdrawal.plan_hash}
          what="withdrawal"
          approveNote="The service then inspects, signs, submits once and settles it when its cooling period has passed; its answer says how that ended."
          command={decide}
          done={(result: DelegationWithdrawalDecisionResult) => (
            <ExecutionOutcome execution={result.execution} status={result.withdrawal.status} />
          )}
        />
      )}
    </Panel>
  );
}

function CancelPanel(props: { readonly withdrawal: DelegationWithdrawal }): ReactNode {
  const { withdrawal } = props;
  const account = useAdvancedAccount();
  const cancel = useCancelWithdrawal();
  const own = account.ownAccountId !== undefined && account.ownAccountId.toLowerCase() === withdrawal.account_id.toLowerCase();
  return (
    <Panel
      title="Cancel"
      description="Until it is signed, you may stop a withdrawal and have its funds returned. Cancelling only reduces risk, so it needs no fresh strong sign-in."
    >
      {!own ? (
        <InspectingNote what="cancel a withdrawal" />
      ) : !cancellable(withdrawal.status) && !cancel.isSuccess ? (
        <div className="form-actions">
          <Button variant="danger" disabledReason={notCancellableReason(withdrawal.status)}>
            Cancel this withdrawal
          </Button>
        </div>
      ) : (
        <ReasonCommand
          id={withdrawal.id}
          label="Cancel this withdrawal"
          confirmLabel="Confirm cancellation"
          reasonNote="Recorded with the cancellation."
          command={cancel}
          done={(cancelled) => (
            <p className="callout" role="status" data-cancelled={cancelled.id}>
              The backend recorded the withdrawal as <Pill tone={subjectStatusTone(cancelled.status)}>{cancelled.status}</Pill>.
            </p>
          )}
        />
      )}
    </Panel>
  );
}
