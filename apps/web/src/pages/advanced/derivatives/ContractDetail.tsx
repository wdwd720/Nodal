/**
 * ONE DERIVATIVE CONTRACT: the terms the backend trades it on, its latest mark
 * and every mark before it, its final settlement, the form that opens a
 * position in it, reference analytics for an option, and — for an operator —
 * the sandbox's marks, runs and settlement.
 *
 * Prices are integers at the contract's price scale and are drawn as the
 * decimal they denote by moving the point, never by dividing. Every figure is
 * the backend's; every command is the backend's to refuse.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useDerivativeContract,
  useDerivativeMarks,
  useDerivativePolicy,
  type DerivativeContract,
  type DerivativeMark,
} from "../../../api/advanced/derivatives.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, formatMillis, settlementStyleLabel, type CursorTrail } from "../../../lib/advanced.ts";
import { KIND_LABEL } from "../../../lib/derivatives.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { AnalyticsPanel } from "./Analytics.tsx";
import { OpenPositionForm, RecordMarkForm, RunForm, SettleForm } from "./forms.tsx";
import { DerivativesNav, MarkFields, Price, derivativesHref } from "./parts.tsx";

export function ContractDetailPage(): ReactNode {
  const { instrumentId } = useParams();
  const contract = useDerivativeContract(instrumentId);
  return (
    <Page
      title={contract.isSuccess ? contract.data.terms.symbol : "Derivative contract"}
      lead="One contract: its terms, its marks, its settlement, and what can be done with it here."
      actions={<LinkButton to={derivativesHref()}>All contracts</LinkButton>}
    >
      <SimulatedNotice />
      <DerivativesNav />
      {contract.isSuccess ? (
        <ContractPanels contract={contract.data} />
      ) : (
        <Panel title="Contract">
          <AsyncPanel query={contract} loadingLabel="Loading the contract…">
            {() => null}
          </AsyncPanel>
        </Panel>
      )}
    </Page>
  );
}

function ContractPanels(props: { readonly contract: DerivativeContract }): ReactNode {
  const { contract } = props;
  const { canInspect } = useAdvancedAccount();
  const policy = useDerivativePolicy();
  const terms = contract.terms;
  return (
    <>
      <Panel title="Contract" description="Its identity and the version of its terms in force.">
        <FieldGrid columns={3}>
          <Field label="Kind">
            <Pill tone="neutral">{KIND_LABEL[contract.kind]}</Pill>
          </Field>
          <Field label="Status">
            {contract.status}
            {contract.settled && <Pill tone="info">settled</Pill>}
          </Field>
          <Field label="Risk class">{contract.risk_class}</Field>
          <Field label="Settles in" note="Every amount of this contract is counted in this asset.">
            {contract.settlement_asset.symbol}
            <span className="cell-note">
              {String(contract.settlement_asset.decimals)} decimals · <code className="mono-small">{contract.settlement_asset.id}</code>
            </span>
          </Field>
          <Field label="Terms in force">
            version {String(contract.terms_version)}
            <span className="cell-note">
              <code className="mono-small">{contract.terms_hash}</code>
            </span>
          </Field>
          <Field label="Instrument">
            <Link to={advancedHref("instruments", contract.id)}>
              <Identifier value={contract.id} />
            </Link>
            <span className="cell-note">Every version of its terms is on the instrument page.</span>
          </Field>
        </FieldGrid>
      </Panel>

      <Panel title="Terms" description="The canonical terms the backend trades this contract on.">
        <FieldGrid columns={3}>
          <Field label="Tick size" note={`Prices are integers at ${String(terms.price_scale)} decimal places.`}>
            <Price mantissa={terms.tick_size} scale={terms.price_scale} />
          </Field>
          <Field label="Contract multiplier" note="Base units of the underlying per contract.">
            <BaseUnits value={terms.contract_multiplier} />
          </Field>
          <Field label="Lot size and minimum" note="Contracts are a whole number of lots, at least the minimum.">
            {terms.lot_size} lot · minimum {terms.min_quantity}
          </Field>
          <Field label="Expiry">
            {terms.expiry === undefined ? (
              <span className="absent">does not expire</span>
            ) : (
              <>
                {formatInstant(terms.expiry)}
                <span className="cell-note">{exactInstant(terms.expiry)}</span>
              </>
            )}
          </Field>
          <Field label="Funding interval">
            {terms.funding_interval_ms === undefined ? (
              <span className="absent">no funding</span>
            ) : (
              <>
                {formatMillis(terms.funding_interval_ms)}
                <span className="cell-note">{String(terms.funding_interval_ms)} ms</span>
              </>
            )}
          </Field>
          <Field label="Option">
            {terms.option_right === undefined ? (
              <span className="absent">not an option</span>
            ) : (
              <>
                {terms.option_right} · {terms.exercise_style ?? "style not stated"} · strike{" "}
                <Price mantissa={terms.strike_mantissa} scale={terms.price_scale} />
              </>
            )}
          </Field>
          <Field label="Settlement">{settlementStyleLabel(terms.settlement_style)}</Field>
          <Field label="Oldest usable mark" note="A mark older than this is stale: it backs no new risk.">
            {formatMillis(terms.oracle_max_age_ms)}
          </Field>
          <Field label="Trading calendar">{terms.trading_calendar}</Field>
        </FieldGrid>
      </Panel>

      <Panel title="Latest mark" description="The mark every valuation and paper fill of this contract starts from.">
        {contract.latest_mark === undefined ? (
          <EmptyState
            title="Never marked"
            body="The backend has recorded no mark for this contract. Without one it cannot be traded or valued, and nothing is shown in its place."
          />
        ) : (
          <MarkFields mark={contract.latest_mark} />
        )}
      </Panel>

      <Panel title="Final settlement" description="Recorded once, for a dated future or an option after its expiry.">
        {contract.settlement === undefined ? (
          <EmptyState title="Not settled" body="The backend has recorded no final settlement for this contract." />
        ) : (
          <FieldGrid columns={3}>
            <Field label="Settled at" note={contract.settlement.kind === "OPTION" ? "The underlying's price." : "The future's final price."}>
              <Price mantissa={contract.settlement.price_mantissa} scale={contract.settlement.price_scale} />
            </Field>
            <Field label="Price source">
              <code className="mono-small">{contract.settlement.price_source}</code>
            </Field>
            <Field label="Positions settled">{String(contract.settlement.positions)}</Field>
            <Field label="When">{formatInstant(contract.settlement.settled_at)}</Field>
          </FieldGrid>
        )}
      </Panel>

      <MarksPanel key={`marks-${contract.id}`} instrumentId={contract.id} />

      <Panel
        title="Open a position"
        description="A paper fill against the clearing house, in this session's own account. The backend decides whether it may happen."
      >
        <OpenPositionForm contract={contract} policy={policy.data} />
      </Panel>

      {contract.kind === "OPTION" && (
        <Panel title="Reference analytics" description="A pricing model's values at the latest mark. Not a quote; not advice.">
          <AnalyticsPanel contract={contract} />
        </Panel>
      )}

      {canInspect && <OperatorPanels contract={contract} variationMarginMs={policy.data?.variation_margin_interval_ms} />}
    </>
  );
}

function MarksPanel(props: { readonly instrumentId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const marks = useDerivativeMarks(props.instrumentId, trail.current);
  return (
    <Panel title="Marks" description="Every mark recorded for this contract, newest observation first.">
      <AsyncPanel
        query={marks}
        loadingLabel="Loading marks…"
        empty={{
          isEmpty: (page: CursorPage<DerivativeMark>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No marks",
          body: "The backend has recorded no mark for this contract.",
        }}
      >
        {(page: CursorPage<DerivativeMark>) => (
          <>
            <ScrollTable caption="Marks on this page" headers={["Observed", "Mark", "Index", "Source", "Recorded"]}>
              {page.items.map((mark) => (
                <tr key={mark.id}>
                  <th scope="row">
                    {formatInstant(mark.observed_at)}
                    <span className="cell-note">{exactInstant(mark.observed_at)}</span>
                  </th>
                  <td>
                    <Price mantissa={mark.mark_mantissa} scale={mark.price_scale} />
                  </td>
                  <td>
                    {mark.has_index ? (
                      <Price mantissa={mark.index_mantissa} scale={mark.index_scale} />
                    ) : (
                      <span className="absent">no index</span>
                    )}
                  </td>
                  <td>
                    <code className="mono-small">{mark.source}</code>
                  </td>
                  <td>{formatInstant(mark.recorded_at)}</td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="marks"
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

/**
 * The operator side of the sandbox for this contract. The backend holds the
 * authority: these forms only ask, and a customer session that reached them
 * would be refused.
 */
function OperatorPanels(props: { readonly contract: DerivativeContract; readonly variationMarginMs: number | undefined }): ReactNode {
  const { contract } = props;
  const linear = contract.kind === "FUTURE" || contract.kind === "PERPETUAL";
  const expires = contract.kind === "FUTURE" || contract.kind === "OPTION";
  return (
    <>
      <Panel title="Operator: record a mark" description="A sandbox mark for this contract. It moves every valuation of it.">
        <RecordMarkForm contract={contract} />
      </Panel>
      {linear && (
        <Panel
          title="Operator: variation margin"
          description="Settles every open position's move to the latest fresh mark through the clearing house. Needs a recent strong sign-in."
        >
          <RunForm contract={contract} kind="VARIATION_MARGIN" intervalMs={props.variationMarginMs} />
        </Panel>
      )}
      {contract.kind === "PERPETUAL" && (
        <Panel
          title="Operator: funding"
          description="Charges one funding interval from the interval's marks. Needs a recent strong sign-in."
        >
          <RunForm contract={contract} kind="FUNDING" intervalMs={contract.terms.funding_interval_ms} />
        </Panel>
      )}
      {expires && (
        <Panel
          title="Operator: final settlement"
          description="Settles the expired contract once and for all. Needs a recent strong sign-in."
        >
          <SettleForm contract={contract} />
        </Panel>
      )}
    </>
  );
}
