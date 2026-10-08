/**
 * ONE DERIVATIVE POSITION: what it holds, what it cost, what it has realized
 * and paid or received in funding, the margin it holds, the backend's
 * valuation of it (or the backend's refusal to value it), the forms that
 * close or exercise it, and every flow and funding accrual it has had.
 *
 * The backend scopes the position by its own account: a reader who may not
 * see it is told it does not exist, and that answer is what is shown.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import type { CursorPage } from "../../../api/advanced/core.ts";
import {
  useDerivativeContract,
  useDerivativeFlows,
  useDerivativeFundingAccruals,
  useDerivativePosition,
  type DerivativeFlow,
  type DerivativeFundingAccrual,
  type DerivativePosition,
} from "../../../api/advanced/derivatives.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { FLOW_KIND_LABEL, VALUATION_MEANING, exerciseAvailability, formatRateE8 } from "../../../lib/derivatives.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { ReducePositionForm } from "./forms.tsx";
import { Amount, DerivativesNav, Price, RefusalNote, derivativesHref } from "./parts.tsx";

export function PositionDetailPage(): ReactNode {
  const { positionId } = useParams();
  const { accountId } = useAdvancedAccount();
  const position = useDerivativePosition(accountId, positionId);
  return (
    <Page
      title={position.isSuccess ? `${position.data.symbol} position` : "Derivative position"}
      lead="One position, as the backend holds and values it."
      actions={<LinkButton to={derivativesHref("positions")}>All positions</LinkButton>}
    >
      <SimulatedNotice />
      <DerivativesNav />
      {position.isSuccess ? (
        <PositionPanels position={position.data} />
      ) : (
        <Panel title="Position">
          <AsyncPanel query={position} loadingLabel="Loading the position…">
            {() => null}
          </AsyncPanel>
        </Panel>
      )}
    </Page>
  );
}

function PositionPanels(props: { readonly position: DerivativePosition }): ReactNode {
  const { position } = props;
  const asset = position.settlement_asset;
  const contract = useDerivativeContract(position.instrument_id);
  const v = position.valuation;
  const exercise = exerciseAvailability(position, contract.data?.terms.exercise_style);
  return (
    <>
      <Panel title="Position" description="What it holds now.">
        <FieldGrid columns={3}>
          <Field label="Contract">
            <Link to={derivativesHref("contracts", position.instrument_id)}>{position.symbol}</Link>
            <span className="cell-note">{position.kind}</span>
          </Field>
          <Field label="Status">
            <Pill tone={position.status === "OPEN" ? "info" : "neutral"}>{position.status}</Pill>
            {position.side !== undefined && <Pill tone="neutral">{position.side}</Pill>}
          </Field>
          <Field label="Contracts" note="Signed: long above zero, short below.">
            {position.quantity}
          </Field>
          <Field label="Opened">
            {formatInstant(position.opened_at)}
            <span className="cell-note">{exactInstant(position.opened_at)}</span>
          </Field>
          <Field label="Closed">
            {position.closed_at === undefined ? <span className="absent">still open</span> : formatInstant(position.closed_at)}
          </Field>
          <Field label="Account">
            <Identifier value={position.account_id} />
          </Field>
        </FieldGrid>
      </Panel>

      <Panel title="Figures" description={`Exact amounts of ${asset.symbol}, the asset this contract settles in.`}>
        <FieldGrid columns={3}>
          <Field label="Cost value" note="What the open contracts were opened at.">
            <Amount value={position.cost_value} asset={asset} />
          </Field>
          <Field label="Settled value" note="The value variation margin last settled the contracts at.">
            <Amount value={position.settled_value} asset={asset} />
          </Field>
          <Field label="Margin posted" note="Initial margin held for this position.">
            <Amount value={position.margin_posted} asset={asset} />
          </Field>
          <Field label="Realized" note="On reducing trades, exercise, assignment and settlement.">
            <Amount value={position.realized_pnl} asset={asset} />
          </Field>
          <Field label="Funding" note="Received (positive) or paid (negative) over the position's life.">
            <Amount value={position.funding_pnl} asset={asset} />
          </Field>
          <Field label="Shortfall" note="Obligations the account could not pay; the clearing house absorbed them.">
            <Amount value={position.shortfall} asset={asset} />
          </Field>
        </FieldGrid>
      </Panel>

      <Panel title="Valuation" description={VALUATION_MEANING[position.valuation_status]}>
        {v !== undefined ? (
          <FieldGrid columns={3}>
            <Field label="Average entry" note="Display only; no amount is computed from it.">
              <Price mantissa={v.average_entry_price_mantissa} scale={position.price_scale} />
            </Field>
            <Field label="Mark">
              <Price mantissa={v.mark_mantissa} scale={position.price_scale} />
              <span className="cell-note">observed {formatInstant(v.mark_observed_at)}</span>
            </Field>
            <Field label="Unrealized" note="The contracts at the mark, less what they cost.">
              <Amount value={v.unrealized_pnl} asset={asset} />
            </Field>
            <Field label="Initial requirement">
              <Amount value={v.requirement.initial} asset={asset} />
            </Field>
            <Field label="Maintenance requirement">
              <Amount value={v.requirement.maintenance} asset={asset} />
            </Field>
            <Field label="Notional">
              <Amount value={v.requirement.notional} asset={asset} />
            </Field>
            <Field label="Funding buffer" note="A perpetual's expected funding, carried in its initial margin.">
              <Amount value={v.requirement.funding_buffer} asset={asset} />
            </Field>
            <Field label="Scenario loss" note="A written option's worst loss on the grid, or a future's expiry gap.">
              <Amount value={v.requirement.scenario_loss} asset={asset} />
            </Field>
            <Field label="Equity beyond ledger balances">
              <Amount value={v.equity_adjustment} asset={asset} />
            </Field>
          </FieldGrid>
        ) : position.valuation_refusal !== undefined ? (
          <RefusalNote
            what="The backend could not value this account's open book"
            code={position.valuation_refusal.code}
            detail={position.valuation_refusal.detail}
          />
        ) : (
          <p className="note">{VALUATION_MEANING[position.valuation_status]}</p>
        )}
      </Panel>

      {position.status === "OPEN" && (
        <Panel
          title="Close"
          description="A paper fill at the latest mark, moved by the disclosed slippage. Closing reduces risk: a stale mark does not prevent it."
        >
          <ReducePositionForm position={position} verb="close" availability={{ available: true, reason: "" }} />
        </Panel>
      )}

      {position.kind === "OPTION" && position.status === "OPEN" && (
        <Panel
          title="Exercise"
          description="Held American options settle at the fresh index and are assigned to writers first in, first out."
        >
          <ReducePositionForm position={position} verb="exercise" availability={exercise} />
        </Panel>
      )}

      <FlowsPanel key={`flows-${position.id}`} position={position} />
      {position.kind === "PERPETUAL" && <AccrualsPanel key={`accruals-${position.id}`} position={position} />}
    </>
  );
}

function FlowsPanel(props: { readonly position: DerivativePosition }): ReactNode {
  const { position } = props;
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const flows = useDerivativeFlows(position.account_id, { positionId: position.id }, trail.current);
  return (
    <Panel title="Flows of this position" description="Every flow between this position and the clearing house, newest first.">
      <AsyncPanel
        query={flows}
        loadingLabel="Loading flows…"
        empty={{
          isEmpty: (page: CursorPage<DerivativeFlow>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No flows",
          body: "Nothing has moved between this position and the clearing house. Opening a future moves only margin.",
        }}
      >
        {(page: CursorPage<DerivativeFlow>) => (
          <>
            <ScrollTable caption="Flows of this position" headers={["When", "Flow", "Amount", "Shortfall", "Journal"]}>
              {page.items.map((flow) => (
                <tr key={flow.id}>
                  <td>{formatInstant(flow.occurred_at)}</td>
                  <th scope="row">{FLOW_KIND_LABEL[flow.kind]}</th>
                  <td>
                    <Amount value={flow.amount} asset={flow.settlement_asset} />
                  </td>
                  <td>
                    <Amount value={flow.shortfall} asset={flow.settlement_asset} />
                  </td>
                  <td>
                    <Identifier value={flow.journal_transaction_id} />
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="flows"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function AccrualsPanel(props: { readonly position: DerivativePosition }): ReactNode {
  const { position } = props;
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const accruals = useDerivativeFundingAccruals(position.account_id, position.id, trail.current);
  return (
    <Panel title="Funding of this position" description="One row per funding interval the position was open at the end of.">
      <AsyncPanel
        query={accruals}
        loadingLabel="Loading funding…"
        empty={{
          isEmpty: (page: CursorPage<DerivativeFundingAccrual>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No funding yet",
          body: "No funding interval has been charged while this position was open.",
        }}
      >
        {(page: CursorPage<DerivativeFundingAccrual>) => <AccrualsTable page={page} trail={trail} onTrail={setTrail} />}
      </AsyncPanel>
    </Panel>
  );
}

/** A page of funding accruals. Shared with the account's flows page. */
export function AccrualsTable(props: {
  readonly page: CursorPage<DerivativeFundingAccrual>;
  readonly trail: CursorTrail;
  readonly onTrail: (trail: CursorTrail) => void;
}): ReactNode {
  const { page, trail } = props;
  return (
    <>
      <ScrollTable
        caption="Funding accruals on this page"
        headers={["Interval", "Contract", "Contracts", "Direction", "Amount", "Rate", "Per contract", "Mark"]}
      >
        {page.items.map((a) => (
          <tr key={a.id}>
            <td>
              {formatInstant(a.interval_start)}
              <span className="cell-note">to {formatInstant(a.interval_end)}</span>
            </td>
            <th scope="row">
              <Link to={derivativesHref("positions", a.position_id)}>{a.symbol}</Link>
            </th>
            <td>{a.quantity}</td>
            <td>
              <Pill tone={a.direction === "PAY" ? "warn" : "good"}>{a.direction}</Pill>
            </td>
            <td>
              <Amount value={a.amount} asset={a.settlement_asset} />
            </td>
            <td>
              {formatRateE8(a.rate_e8)}
              {a.clamped && <span className="cell-note">clamped from {formatRateE8(a.premium_index_e8)}</span>}
            </td>
            <td>
              <Amount value={a.per_contract_amount} asset={a.settlement_asset} />
            </td>
            <td>
              <Price mantissa={a.mark_mantissa} scale={a.price_scale} />
            </td>
          </tr>
        ))}
      </ScrollTable>
      <CursorPager
        noun="accruals"
        trail={trail}
        nextCursor={page.nextCursor}
        onForward={(cursor) => {
          props.onTrail(forward(trail, cursor));
        }}
        onBack={() => {
          props.onTrail(back(trail));
        }}
      />
    </>
  );
}
