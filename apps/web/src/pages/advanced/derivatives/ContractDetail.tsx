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
  type DerivativeSettlementOverride,
  type DerivativeSettlementPreview,
} from "../../../api/advanced/derivatives.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, formatMillis, settlementStyleLabel, type CursorTrail } from "../../../lib/advanced.ts";
import { KIND_LABEL, OVERRIDE_STATUS_MEANING, PRICE_RULE_LABEL } from "../../../lib/derivatives.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { AnalyticsPanel } from "./Analytics.tsx";
import { DecideOverrideForm, OpenPositionForm, ProposeOverrideForm, RecordMarkForm, RunForm, SettleForm } from "./forms.tsx";
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
            <Field label="What decided the price">
              {contract.settlement.price_rule === undefined ? (
                <span className="absent">recorded before settlements named their rule</span>
              ) : (
                PRICE_RULE_LABEL[contract.settlement.price_rule]
              )}
            </Field>
            <Field label="Price source">
              <code className="mono-small">{contract.settlement.price_source}</code>
            </Field>
            <Field label="Positions settled">{String(contract.settlement.positions)}</Field>
            <Field label="When">{formatInstant(contract.settlement.settled_at)}</Field>
          </FieldGrid>
        )}
      </Panel>

      {contract.settlement_preview !== undefined && <SettlementRulePanel contract={contract} preview={contract.settlement_preview} />}
      {contract.settlement_overrides !== undefined && <OverridesPanel overrides={contract.settlement_overrides} />}

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

/**
 * What the backend's settlement rule says about this contract now: the mark
 * it reads at expiry, the price it derives, whether that mark was fresh — and
 * so whether the rule settles it — and the range an override may propose.
 */
function SettlementRulePanel(props: { readonly contract: DerivativeContract; readonly preview: DerivativeSettlementPreview }): ReactNode {
  const { preview } = props;
  return (
    <Panel title="Settlement rule" description="How the backend prices this contract's final settlement. Nobody chooses the price.">
      <p className="note">{preview.rule_statement}</p>
      <FieldGrid columns={3}>
        <Field label="Expiry">
          {formatInstant(preview.expiry)}
          <span className="cell-note">{exactInstant(preview.expiry)}</span>
        </Field>
        <Field label="State">
          {preview.settled ? (
            <Pill tone="info">settled</Pill>
          ) : !preview.expired ? (
            <Pill tone="neutral">not expired</Pill>
          ) : preview.settleable ? (
            <Pill tone="good">settles by the rule</Pill>
          ) : (
            <Pill tone="warn">waits for an override</Pill>
          )}
        </Field>
        <Field label="Reference mark" note="The last mark observed and recorded by expiry.">
          {preview.reference_mark === undefined ? (
            <span className="absent">{preview.expired ? "none recorded by expiry" : "read at expiry"}</span>
          ) : (
            <>
              <Price mantissa={preview.reference_mark.mark_mantissa} scale={preview.reference_mark.price_scale} />
              <span className="cell-note">observed {exactInstant(preview.reference_mark.observed_at)}</span>
            </>
          )}
        </Field>
        <Field label="The rule's price" note={props.contract.kind === "OPTION" ? "The underlying's price, from the mark's index." : "The future's final price."}>
          {preview.reference_price_mantissa === undefined ? (
            <span className="absent">none</span>
          ) : (
            <Price mantissa={preview.reference_price_mantissa} scale={preview.price_scale} />
          )}
        </Field>
        <Field label="An override may propose" note="On the tick grid, within this bound of the rule's price.">
          {preview.override_min_price_mantissa === undefined || preview.override_max_price_mantissa === undefined ? (
            <span className="absent">no reference to bound it by</span>
          ) : (
            <>
              <Price mantissa={preview.override_min_price_mantissa} scale={preview.price_scale} /> to{" "}
              <Price mantissa={preview.override_max_price_mantissa} scale={preview.price_scale} />
            </>
          )}
        </Field>
        <Field label="Override bound">
          <Bps value={preview.max_deviation_bps} />
        </Field>
      </FieldGrid>
    </Panel>
  );
}

/** Every override proposal for this contract's settlement, newest first, with its decision. */
function OverridesPanel(props: { readonly overrides: readonly DerivativeSettlementOverride[] }): ReactNode {
  return (
    <Panel
      title="Settlement overrides"
      description="Proposed by one operator within the policy's bound, approved or rejected by a second."
    >
      {props.overrides.length === 0 ? (
        <EmptyState title="No overrides" body="No operator has proposed an override of this contract's settlement price." />
      ) : (
        <ScrollTable
          caption="Override proposals"
          headers={["Proposed", "Status", "Price", "Rule's price", "Deviation", "Reason and evidence", "Decision"]}
        >
          {props.overrides.map((o) => (
            <tr key={o.id}>
              <th scope="row">
                {formatInstant(o.proposed_at)}
                <span className="cell-note">by {o.proposed_by.actor_id}</span>
              </th>
              <td>
                <Pill tone={o.status === "APPROVED" ? "good" : o.status === "REJECTED" ? "neutral" : "warn"}>{o.status}</Pill>
                <span className="cell-note">{OVERRIDE_STATUS_MEANING[o.status]}</span>
              </td>
              <td>
                <Price mantissa={o.price_mantissa} scale={o.price_scale} />
              </td>
              <td>
                <Price mantissa={o.reference_mantissa} scale={o.price_scale} />
              </td>
              <td>
                <Bps value={o.deviation_bps} />
                <span className="cell-note">
                  bound <Bps value={o.max_deviation_bps} />
                </span>
              </td>
              <td>
                {o.reason}
                <span className="cell-note">
                  <code className="mono-small">{o.evidence_ref}</code> · sha256 <code className="mono-small">{o.evidence_sha256}</code>
                </span>
              </td>
              <td>
                {o.decision === undefined ? (
                  <span className="absent">waiting for a second operator</span>
                ) : (
                  <>
                    {o.decision.decision} by {o.decision.decided_by.actor_id}
                    <span className="cell-note">
                      {o.decision.reason} · {formatInstant(o.decision.decided_at)}
                    </span>
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
          description="Settles the expired contract once and for all, at the rule's price. Needs a recent strong sign-in."
        >
          <SettleForm contract={contract} />
        </Panel>
      )}
      {expires && !contract.settled && (
        <Panel
          title="Operator: propose a settlement override"
          description="When the rule cannot settle the contract. A second operator decides. Needs a recent strong sign-in."
        >
          <ProposeOverrideForm contract={contract} />
        </Panel>
      )}
      {(contract.settlement_overrides ?? [])
        .filter((o) => o.status === "PROPOSED" && !contract.settled)
        .map((o) => (
          <Panel
            key={o.id}
            title={`Operator: decide override ${o.id}`}
            description="Approving settles the contract at the proposal's price; the proposer cannot approve it. Needs a recent strong sign-in."
          >
            <DecideOverrideForm contract={contract} override={o} />
          </Panel>
        ))}
    </>
  );
}
