/**
 * ONE PREDICTION MARKET: its definition, its lifecycle, its outcomes, every
 * resolution and dispute, every void proposal, its settlement record, its
 * escrow books, the account's shares and held payout, the complete-set forms
 * and, for an operator, the actions its status admits.
 *
 * Every figure is a base-unit string the backend computed, shown as the
 * decimal it denotes with the asset it is counted in. The escrow books are
 * one snapshot with the backend's own verdict on the escrow identity; the page
 * never adds two figures together.
 */
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  usePredictionBooks,
  usePredictionMarket,
  usePredictionPositions,
  type PredictionEscrowBooks,
  type PredictionMarket,
  type PredictionMarketDetail,
  type PredictionPositionList,
  type PredictionSettlement,
  type PredictionVoidProposal,
} from "../../../api/advanced/predmarket.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { formatMillis } from "../../../lib/advanced.ts";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import {
  ORDER_BOOK_NOTE,
  RESOLUTION_MEANING,
  ROUNDING_MEANING,
  STATUS_MEANING,
  VOID_DUAL_CONTROL_NOTE,
  lifecycleSteps,
  statusTone,
  type LifecycleStep,
} from "../../../lib/predmarket.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useSession } from "../../../session.tsx";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import { Codes, ModePill, NoAccount, ScrollTable } from "../shared.tsx";
import { OperatorActionsPanel } from "./OperatorActions.tsx";
import { HeldPayoutSection } from "./HeldPayout.tsx";
import { ActorLine } from "./parts.tsx";
import { marketsHref } from "./paths.ts";
import { CompleteSetsPanel } from "./SetForms.tsx";

export function MarketDetailPage(): ReactNode {
  const { marketId } = useParams();
  const market = usePredictionMarket(marketId);

  return (
    <Page
      title={market.isSuccess ? market.data.market.symbol : "Prediction market"}
      lead="One market: its definition and lifecycle, how it resolved and settled, its escrow, and the shares the account holds."
      actions={<LinkButton to={marketsHref()}>All markets</LinkButton>}
    >
      <SimulatedNotice />
      {market.isSuccess ? (
        <MarketPanels detail={market.data} />
      ) : (
        <Panel title="Market">
          <AsyncPanel query={market} loadingLabel="Loading the market…">
            {() => null}
          </AsyncPanel>
        </Panel>
      )}
    </Page>
  );
}

function MarketPanels(props: { readonly detail: PredictionMarketDetail }): ReactNode {
  const { detail } = props;
  const { market } = detail;
  const session = useSession();
  const operator = session.principal?.actor_type === "OPERATOR";
  return (
    <>
      <DefinitionPanel market={market} />
      <LifecyclePanel detail={detail} />
      <OutcomesPanel market={market} />
      <ResolutionsPanel detail={detail} />
      <VoidProposalsPanel proposals={detail.void_proposals} />
      <SettlementPanel market={market} settlement={detail.settlement} />
      <BooksPanel market={market} />
      <PositionsPanel market={market} />
      {/* Keyed by market: a command and its answer belong to one market and
          must not carry over when the route moves to another. */}
      <CompleteSetsPanel key={`sets-${market.id}`} market={market} />
      {operator && (
        <OperatorActionsPanel key={`operator-${market.id}`} market={market} proposals={detail.void_proposals} />
      )}
    </>
  );
}

function DefinitionPanel(props: { readonly market: PredictionMarket }): ReactNode {
  const { market } = props;
  const modeNote = ADVANCED_MODE_DESCRIPTIONS[market.mode];
  return (
    <Panel title="The market" description="Its definition, fixed when it was created; its hash is what the backend compares.">
      <p className="lead">{market.question}</p>
      <FieldGrid columns={3}>
        <Field label="Status" note={STATUS_MEANING[market.status as keyof typeof STATUS_MEANING]}>
          <Pill tone={statusTone(market.status)}>{market.status}</Pill>
        </Field>
        <Field label="Mode" {...(modeNote === undefined ? {} : { note: modeNote })}>
          <ModePill mode={market.mode} />
        </Field>
        <Field label="Collateral" note="Every market is collateralised in the sandbox dollar.">
          {market.collateral_symbol} <Identifier value={market.collateral_asset_id} />
        </Field>
        <Field
          label="One complete set"
          note="Costs this much collateral, and one share of the winning outcome pays it at settlement."
        >
          <Qty value={market.unit_quantity} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />{" "}
          <BaseUnits value={market.unit_quantity} />
        </Field>
        <Field label="Opens">{formatInstant(market.open_at)}</Field>
        <Field label="Closes" note="Nothing is issued or redeemed at or after this time.">
          {formatInstant(market.close_at)}
        </Field>
        <Field label="Dispute window" note="How long after a resolution settlement waits for a dispute.">
          {formatMillis(market.dispute_window_ms)}
        </Field>
        <Field label="Oracle source">{market.oracle_source}</Field>
        <Field label="Oracle key" note="Only a report this Ed25519 key signed can resolve the market.">
          <Identifier value={market.oracle_public_key} />
        </Field>
        <Field label="Defined by" note="The operator who defined it can never resolve it or take part in voiding it.">
          <ActorLine actor={market.created_by} />
        </Field>
        <Field label="Definition hash">
          <Identifier value={market.definition_hash} />
        </Field>
        <Field label="Market id">
          <Identifier value={market.id} />
        </Field>
      </FieldGrid>
    </Panel>
  );
}

/** When each lifecycle step happened, or was scheduled, as far as the market's own records say. */
function stepDetail(step: LifecycleStep, detail: PredictionMarketDetail): string {
  const { market, resolutions, disputes, settlement } = detail;
  switch (step.status) {
    case "DRAFT":
      return `Defined ${formatInstant(market.created_at)}.`;
    case "OPEN":
      return `Scheduled to open ${formatInstant(market.open_at)}.`;
    case "CLOSED":
      return `Scheduled to close ${formatInstant(market.close_at)}.`;
    case "RESOLVING":
      return "An oracle report is requested.";
    case "RESOLVED": {
      const first = resolutions[0];
      return first === undefined
        ? "No resolution has been accepted."
        : `First resolution accepted ${formatInstant(first.accepted_at)}${resolutions.length > 1 ? `; ${String(resolutions.length)} resolutions in all` : ""}.`;
    }
    case "DISPUTED": {
      const raised = disputes[0];
      return raised === undefined ? "No dispute has been raised." : `Disputed ${formatInstant(raised.raised_at)}.`;
    }
    case "SETTLED":
    case "VOIDED":
      return settlement === undefined ? "No settlement record yet." : `Recorded ${formatInstant(settlement.settled_at)}.`;
  }
}

const STEP_STATE_WORDS: Readonly<Record<LifecycleStep["state"], string>> = {
  done: "reached",
  current: "current",
  ahead: "not reached",
  unknown: "not known from the API whether this step was reached before the void",
};

function LifecyclePanel(props: { readonly detail: PredictionMarketDetail }): ReactNode {
  const { detail } = props;
  const steps = lifecycleSteps(detail.market.status, {
    resolved: detail.resolutions.length > 0,
    disputed: detail.disputes.length > 0,
  });
  return (
    <Panel
      title="Lifecycle"
      description="The steps a market takes and where this one is. Open and close times are the schedule; the other times come from the market's resolutions, disputes and settlement record."
    >
      <ol className="timeline">
        {steps.map((step) => (
          <li
            key={step.status}
            className={step.state === "done" || step.state === "current" ? "step step-present" : "step step-absent"}
          >
            <span className="step-marker" aria-hidden="true" />
            <div className="step-body">
              <p className="step-label">
                {step.status} <span className="mono-small">({STEP_STATE_WORDS[step.state]})</span>
              </p>
              <p className="step-meta">{stepDetail(step, detail)}</p>
            </div>
          </li>
        ))}
      </ol>
    </Panel>
  );
}

function OutcomesPanel(props: { readonly market: PredictionMarket }): ReactNode {
  const { market } = props;
  return (
    <Panel title="Outcomes" description="Each outcome is a simulated share asset with its own instrument.">
      <ScrollTable caption="Outcomes of this market" headers={["Outcome", "Share asset", "Instrument"]}>
        {market.outcomes.map((outcome) => (
          <tr key={outcome.index}>
            <th scope="row">{outcome.symbol}</th>
            <td>
              <code className="mono-small">{outcome.share_asset_id}</code>
            </td>
            <td>
              <Link to={advancedHref("instruments", outcome.instrument_id)}>
                <code className="mono-small">{outcome.instrument_id}</code>
              </Link>
            </td>
          </tr>
        ))}
      </ScrollTable>
      <p className="note">{ORDER_BOOK_NOTE}</p>
    </Panel>
  );
}

function ResolutionsPanel(props: { readonly detail: PredictionMarketDetail }): ReactNode {
  const { resolutions, disputes } = props.detail;
  return (
    <Panel
      title="Resolution and disputes"
      description="Every resolution the backend accepted and every dispute raised on one, oldest first. A report is accepted only after it verifies against the market's oracle key."
    >
      {resolutions.length === 0 ? (
        <EmptyState
          title="No resolution"
          body="The backend has accepted no oracle report for this market. It resolves only after close, once a report is requested and verified."
        />
      ) : (
        <ScrollTable
          caption="Resolutions"
          headers={["Accepted", "Kind", "Outcome", "Accepted by", "Reason", "Report", "Evidence"]}
        >
          {resolutions.map((resolution) => (
            <tr key={resolution.id}>
              <td>{formatInstant(resolution.accepted_at)}</td>
              <th scope="row">
                {resolution.kind}
                <span className="cell-note">{RESOLUTION_MEANING[resolution.kind]}</span>
              </th>
              <td>
                <Pill tone="info">{resolution.outcome_symbol}</Pill>
              </td>
              <td>
                <ActorLine actor={resolution.accepted_by} />
              </td>
              <td className="cell-prose">{resolution.reason}</td>
              <td>
                {resolution.report_hash === undefined ? (
                  <span className="absent">a ruling, not a report</span>
                ) : (
                  <>
                    <code className="mono-small">{resolution.report_hash}</code>
                    <span className="cell-note">
                      {resolution.report_source ?? "source not reported"} · observed{" "}
                      {formatInstant(resolution.report_observed_at)}
                    </span>
                  </>
                )}
              </td>
              <td>
                <Identifier value={resolution.evidence_ref} />
                <span className="cell-note">
                  sha256 <code className="mono-small">{resolution.evidence_sha256}</code>
                </span>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      {disputes.length > 0 && (
        <ScrollTable caption="Disputes" headers={["Raised", "Raised by", "Reason", "Disputed resolution", "Evidence"]}>
          {disputes.map((dispute) => (
            <tr key={dispute.id}>
              <td>{formatInstant(dispute.raised_at)}</td>
              <td>
                <ActorLine actor={dispute.raised_by} />
              </td>
              <td className="cell-prose">{dispute.reason}</td>
              <td>
                <code className="mono-small">{dispute.resolution_id}</code>
              </td>
              <td>
                <Identifier value={dispute.evidence_ref} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </Panel>
  );
}

function VoidProposalsPanel(props: { readonly proposals: readonly PredictionVoidProposal[] }): ReactNode {
  const { proposals } = props;
  return (
    <Panel title="Void proposals" description={VOID_DUAL_CONTROL_NOTE}>
      {proposals.length === 0 ? (
        <EmptyState
          title="No void proposal"
          body="No operator has proposed to void this market. A void happens only when a second operator approves a proposal."
        />
      ) : (
        <ScrollTable caption="Void proposals" headers={["Proposed", "Proposed by", "Reason", "Against", "Approval"]}>
          {proposals.map((proposal) => (
            <tr key={proposal.id}>
              <td>{formatInstant(proposal.proposed_at)}</td>
              <td>
                <ActorLine actor={proposal.proposed_by} />
              </td>
              <td className="cell-prose">
                {proposal.reason}
                <span className="cell-note">
                  evidence <Identifier value={proposal.evidence_ref} />
                </span>
              </td>
              <td>
                <Pill tone={statusTone(proposal.market_status)}>{proposal.market_status}</Pill>
                {proposal.resolution_id !== undefined && (
                  <span className="cell-note">
                    resolution <code className="mono-small">{proposal.resolution_id}</code>
                  </span>
                )}
              </td>
              <td>
                {proposal.approval === undefined ? (
                  <span className="absent">awaiting a second operator</span>
                ) : (
                  <>
                    <Pill tone="bad">APPROVED</Pill> <ActorLine actor={proposal.approval.approved_by} />
                    <span className="cell-note">{formatInstant(proposal.approval.approved_at)}</span>
                  </>
                )}
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </Panel>
  );
}

function SettlementPanel(props: {
  readonly market: PredictionMarket;
  readonly settlement: PredictionSettlement | undefined;
}): ReactNode {
  const { market, settlement } = props;
  const collateral = (value: string | undefined): ReactNode => (
    <Qty value={value} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />
  );
  return (
    <Panel
      title="Settlement"
      description="The one record of how the market paid out its escrow, or refunded it when voided. Escrow before = paid + residual, checked by the backend from the books; of what was paid, what could not be delivered is held in the escrow for its holder."
    >
      {settlement === undefined ? (
        <EmptyState
          title="No settlement record"
          body="The backend holds no settlement record for this market. A market settles only once it is resolved, undisputed and past its dispute window, or is voided."
        />
      ) : (
        <FieldGrid columns={3}>
          <Field label="Kind">{settlement.kind}</Field>
          {settlement.winning_outcome_symbol !== undefined && (
            <Field label="Winning outcome">
              <Pill tone="info">{settlement.winning_outcome_symbol}</Pill>
            </Field>
          )}
          {settlement.refund_rate_per_share !== undefined && (
            <Field label="Refund per share" note="Collateral each whole share of any outcome refunded.">
              {collateral(settlement.refund_rate_per_share)}
            </Field>
          )}
          <Field label="Escrow before">{collateral(settlement.escrow_before)}</Field>
          <Field label="Paid to holders" note="Delivered, or held for the holder.">
            {collateral(settlement.total_paid)}
          </Field>
          <Field label="Held for holders" note="Not delivered: kept in the escrow until each holder claims it.">
            {collateral(settlement.total_held)}
          </Field>
          <Field label="Residual kept by the escrow">{collateral(settlement.residual)}</Field>
          <Field label="Holders">{String(settlement.holder_count)}</Field>
          <Field label="Rounding" note={ROUNDING_MEANING[settlement.rounding_policy]}>
            {settlement.rounding_policy}
          </Field>
          <Field label="Settled by">
            <ActorLine actor={settlement.settled_by} />
          </Field>
          <Field label="Settled">{formatInstant(settlement.settled_at)}</Field>
          <Field label="Evidence">
            <Identifier value={settlement.evidence_ref} />
          </Field>
          <Field label="Settlement id">
            <Identifier value={settlement.id} />
          </Field>
        </FieldGrid>
      )}
    </Panel>
  );
}

function BooksPanel(props: { readonly market: PredictionMarket }): ReactNode {
  const books = usePredictionBooks(props.market.id);
  return (
    <Panel
      title="Escrow books"
      description="The market's segregated escrow, read in one snapshot. The identity the backend checks: escrow = collateral = shares outstanding of each outcome + residual + payouts held for their holders, and every share issued is outstanding and held by a customer."
    >
      <AsyncPanel query={books} loadingLabel="Reading the escrow books…">
        {(data: PredictionEscrowBooks) => (
          <>
            <p>
              {data.reconciles ? (
                <Pill tone="good">The books reconcile</Pill>
              ) : (
                <Pill tone="bad">The books do not reconcile</Pill>
              )}
            </p>
            <Codes codes={data.violations} />
            <FieldGrid columns={3}>
              <Field label="Escrow">
                <Qty value={data.escrow} decimals={data.collateral_decimals} symbol={data.collateral_symbol} />
              </Field>
              <Field label="Collateral">
                <Qty value={data.collateral} decimals={data.collateral_decimals} symbol={data.collateral_symbol} />
              </Field>
              <Field label="Disclosed residual" note="Zero unless a void refunded the market.">
                <Qty value={data.residual} decimals={data.collateral_decimals} symbol={data.collateral_symbol} />
              </Field>
              <Field label="Held for holders" note="Payouts not yet claimed by the holders they are held for.">
                <Qty value={data.held} decimals={data.collateral_decimals} symbol={data.collateral_symbol} />
              </Field>
            </FieldGrid>
            <ScrollTable
              caption="Shares of each outcome"
              headers={["Outcome", "Issued", "Outstanding", "Held by customers"]}
            >
              {data.outcomes.map((outcome) => (
                <tr key={outcome.index}>
                  <th scope="row">{outcome.symbol}</th>
                  <td>
                    <Qty value={outcome.shares_issued} decimals={data.share_decimals} />
                  </td>
                  <td>
                    <Qty value={outcome.shares_outstanding} decimals={data.share_decimals} />
                  </td>
                  <td>
                    <Qty value={outcome.held_by_customers} decimals={data.share_decimals} />
                  </td>
                </tr>
              ))}
            </ScrollTable>
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function PositionsPanel(props: { readonly market: PredictionMarket }): ReactNode {
  const { accountId, ownAccountId } = useAdvancedAccount();
  const positions = usePredictionPositions(accountId, props.market.id);
  return (
    <Panel
      title="Shares held"
      description="The outcome shares the account the area is reading holds in this market, as the backend reports them."
    >
      {accountId === undefined ? (
        <NoAccount />
      ) : (
        <AsyncPanel
          query={positions}
          loadingLabel="Reading the account's shares…"
          empty={{
            isEmpty: (list: PredictionPositionList) => list.items.length === 0 && list.held_payout === undefined,
            title: "No shares held",
            body: "The backend reports no share of this market for the account, and no payout held for it. That is a statement about holdings, not a balance of zero standing in for one.",
          }}
        >
          {(list: PredictionPositionList) => (
            <>
              {list.held_payout !== undefined && (
                <HeldPayoutSection
                  market={props.market}
                  held={list.held_payout}
                  accountId={list.account_id}
                  ownAccountId={ownAccountId}
                />
              )}
              {list.items.length === 0 ? (
                <p className="note">
                  The backend reports no share of this market for the account: its shares were retired when the
                  market settled or was voided.
                </p>
              ) : (
                <>
                  <ScrollTable caption="Shares held, by outcome" headers={["Outcome", "Shares", "Base units", "As of"]}>
                    {list.items.map((position) => (
                      <tr key={position.outcome_index}>
                        <th scope="row">{position.outcome_symbol}</th>
                        <td>
                          <Qty value={position.quantity} decimals={list.share_decimals} />
                        </td>
                        <td>
                          <BaseUnits value={position.quantity} />
                        </td>
                        <td>
                          {formatInstant(position.as_of)}
                          <span className="cell-note">{exactInstant(position.as_of)}</span>
                        </td>
                      </tr>
                    ))}
                  </ScrollTable>
                  <AsOf at={list.items[0]?.as_of} label="Balances as of" />
                </>
              )}
            </>
          )}
        </AsyncPanel>
      )}
    </Panel>
  );
}
