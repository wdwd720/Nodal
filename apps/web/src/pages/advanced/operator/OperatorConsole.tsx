/**
 * OPERATOR CONSOLE: one page for an operator of the advanced (sandbox) area.
 *
 * What needs an operator now -- transfer reversals and margin case
 * resolutions proposed by one operator, decided here by another, and bridge
 * legs in manual review -- the
 * sandbox reference marks every SIMULATED valuation depends on, the circuit
 * breakers, the capability gates, the liquidation queue, the reconciliation
 * mismatches, the stable assets' reserve results, the freshness of the marks
 * and the simulated chains, with a link to each subsystem's operator page.
 *
 * Every figure comes from an API read and every decision is the backend's:
 * a principal without the operator permissions is refused, and each panel
 * shows that refusal with its code rather than hiding itself.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { useMarginReviews, type MarginReviewQueue } from "../../../api/advanced/margin.ts";
import {
  useOpsOverview,
  useRecordReferenceMark,
  useReferenceMarks,
  type MarkRequest,
  type OpsOverview,
  type ReferenceMarkList,
  type ReferenceMarkRecorded,
} from "../../../api/advanced/operator.ts";
import { useTransferReversalProposals, type TransferReversalProposal } from "../../../api/advanced/transfers.ts";
import { useAssets, type Asset } from "../../../api/queries.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { DecimalValue } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { assetById, simulatedAssets } from "../../../lib/margin.ts";
import {
  awaitingSecondOperator,
  checkMarkPrice,
  checkMarkSource,
  checkObservedAt,
  formatAge,
  formatDuration,
} from "../../../lib/operator.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref } from "../area.ts";
import { CommandFlow, caseHref, type Prepared } from "../margin/parts.tsx";
import { DecideResolutionForm } from "../margin/Reviews.tsx";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { ProposalCard } from "../transfers/reversals.tsx";
import { BridgeLegsInReview } from "./BridgeLegs.tsx";
import {
  CapabilityGatesPanel,
  CircuitBreakersPanel,
  DualControlCounts,
  LiquidationQueuePanel,
  OraclePanel,
  OverviewMeta,
  ReconciliationPanel,
  ReservePanel,
  VenueHealthPanel,
} from "./panels.tsx";

/** The operator pages of the other sections, at the routes those sections register. */
const OPERATOR_PAGES: ReadonlyArray<{ readonly label: string; readonly to: string; readonly what: string }> = [
  {
    label: "Margin operations",
    to: advancedHref("margin", "operator"),
    what: "The liquidation queue and its runs, mark to market, the facilities, every margin call, and the manual review queue.",
  },
  {
    label: "Transfers",
    to: advancedHref("transfers"),
    what: "Every transfer; a transfer's own page carries its reversal proposals and their decisions.",
  },
  {
    label: "Prediction markets",
    to: advancedHref("prediction-markets"),
    what: "Markets, their escrow books, the sandbox oracle and each market's operator and resolution actions.",
  },
  {
    label: "Strategy review queue",
    to: advancedHref("strategies", "review"),
    what: "Strategy versions awaiting validation, review and moderation.",
  },
  {
    label: "Derivatives clearing house",
    to: advancedHref("derivatives", "clearing-house"),
    what: "Defining a future, a perpetual or an option; the clearing house's funding and its reconciliation.",
  },
  {
    label: "Capabilities and gates",
    to: advancedHref("overview"),
    what: "Every advanced capability with its gate decision for every mode, LIVE included.",
  },
];

export function OperatorConsole(): ReactNode {
  const overview = useOpsOverview();
  const [markAsset, setMarkAsset] = useState("");
  return (
    <Page
      title="Operator console"
      lead="What needs an operator in the advanced area now, the sandbox reference marks, and a link to every subsystem's operator page. The backend decides who may see and do each of these."
    >
      <SimulatedNotice />
      <Panel title="Operator pages" description="Each subsystem's own operator page, where its full set of actions lives.">
        <ul className="section-list">
          {OPERATOR_PAGES.map((page) => (
            <li key={page.to}>
              <Link to={page.to}>{page.label}</Link>
              <p className="section-description">{page.what}</p>
            </li>
          ))}
        </ul>
      </Panel>
      <DualControlPanel overview={overview} />
      <MarksSection assetId={markAsset} onAsset={setMarkAsset} />
      <OraclePanel overview={overview} onPick={setMarkAsset} />
      <CircuitBreakersPanel overview={overview} />
      <CapabilityGatesPanel />
      <LiquidationQueuePanel />
      <ReconciliationPanel overview={overview} />
      <ReservePanel overview={overview} />
      <VenueHealthPanel />
    </Page>
  );
}

/* --------------------------------------------------------------------------
 * Awaiting a second operator
 * ------------------------------------------------------------------------ */

function DualControlPanel(props: { readonly overview: ReturnType<typeof useOpsOverview> }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const reversals = useTransferReversalProposals({
    status: "PENDING",
    ...(trail.current === undefined ? {} : { cursor: trail.current }),
  });
  const reviews = useMarginReviews();
  return (
    <Panel
      title="Awaiting an operator"
      description="Dual-control items one operator proposed and a second, different operator must decide (the operator who proposed an item can never approve it), and bridge legs a person owns in manual review."
    >
      <AsyncPanel query={props.overview} loadingLabel="Loading the overview…">
        {(overview: OpsOverview) => (
          <>
            <OverviewMeta overview={overview} />
            <DualControlCounts overview={overview} />
          </>
        )}
      </AsyncPanel>
      <h3>Transfer reversals</h3>
      <AsyncPanel
        query={reversals}
        loadingLabel="Loading the pending reversal proposals…"
        empty={{
          isEmpty: (page) => page.items.length === 0,
          title: "No reversal awaiting a decision",
          body: "The backend holds no PENDING transfer reversal proposal.",
        }}
      >
        {(page) => (
          <div className="stack" data-list="pending-reversals">
            {page.items.map((proposal: TransferReversalProposal) => (
              <ProposalCard key={proposal.id} proposal={proposal} showTransfer />
            ))}
            <CursorPager
              noun="proposals"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </div>
        )}
      </AsyncPanel>
      <h3>Margin case resolutions</h3>
      <AsyncPanel
        query={reviews}
        loadingLabel="Loading the margin review queue…"
        empty={{
          isEmpty: (queue: MarginReviewQueue) => awaitingSecondOperator(queue.items).length === 0,
          title: "No margin resolution awaiting a decision",
          body: "No case in manual review has a resolution for its current review that awaits a second operator. Cases with no proposal yet are on the margin operations page.",
        }}
      >
        {(queue: MarginReviewQueue) => (
          <div className="stack" data-list="pending-resolutions">
            {awaitingSecondOperator(queue.items).map(({ item, resolution }) => (
              <article
                key={resolution.id}
                className="callout"
                aria-label={`Resolution ${resolution.id} of case ${item.case.id}`}
                data-pending-resolution={resolution.id}
              >
                <FieldGrid columns={3}>
                  <Field label="Case">
                    <Link to={caseHref(item.case.id)}>
                      <code className="mono-small">{item.case.id}</code>
                    </Link>
                  </Field>
                  <Field label="Account">
                    <Identifier value={item.case.account_id} />
                  </Field>
                  <Field label="Proposed outcome">
                    <code className="mono-small">{resolution.outcome}</code>
                  </Field>
                  <Field label="Reason">{resolution.reason}</Field>
                  <Field label="Proposed by" note={exactInstant(resolution.proposed_at)}>
                    <Identifier value={resolution.proposed_by} />
                  </Field>
                  <Field label="In review since">{formatInstant(item.entered_review_at)}</Field>
                </FieldGrid>
                <DecideResolutionForm resolution={resolution} current />
              </article>
            ))}
          </div>
        )}
      </AsyncPanel>
      <h3>Bridge legs in manual review</h3>
      <BridgeLegsInReview overview={props.overview} />
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Sandbox reference marks
 * ------------------------------------------------------------------------ */

function MarksSection(props: { readonly assetId: string; readonly onAsset: (assetId: string) => void }): ReactNode {
  const assets = useAssets();
  const registry = assets.data ?? [];
  return (
    <>
      <Panel
        title="Record a reference mark"
        description="An operator's price observation of a SIMULATED asset in the sandbox dollar. Margin collateral, prediction-market shares, the stable asset's peg monitor and bridged twins are valued at it. The backend refuses an asset that is not SIMULATED, a price that is not positive or carries more than 18 fractional digits, and an observation in the future or older than the maximum mark age; every mark it records is audited."
      >
        <RecordMarkForm assets={registry} assetId={props.assetId} onAsset={props.onAsset} />
      </Panel>
      <MarksPanel assets={registry} assetId={props.assetId} />
    </>
  );
}

function RecordMarkForm(props: {
  readonly assets: readonly Asset[];
  readonly assetId: string;
  readonly onAsset: (assetId: string) => void;
}): ReactNode {
  const record = useRecordReferenceMark();
  const [price, setPrice] = useState("");
  const [source, setSource] = useState("");
  const [observedAt, setObservedAt] = useState("");
  const choices = simulatedAssets(props.assets);
  const priceCheck = checkMarkPrice(price);
  const sourceCheck = checkMarkSource(source);
  const atCheck = checkObservedAt(observedAt);
  let prepared: Prepared<MarkRequest>;
  if (props.assetId === "") {
    prepared = { ok: false, reason: "Choose the SIMULATED asset the price is of." };
  } else if (!priceCheck.ok) {
    prepared = { ok: false, reason: priceCheck.error };
  } else if (!sourceCheck.ok) {
    prepared = { ok: false, reason: sourceCheck.error };
  } else if (!atCheck.ok) {
    prepared = { ok: false, reason: atCheck.error };
  } else {
    prepared = {
      ok: true,
      request: { assetId: props.assetId, price: priceCheck.value, source: sourceCheck.value, observedAt: atCheck.value },
    };
  }
  const symbol = (id: string): string => assetById(props.assets, id)?.symbol ?? id;
  return (
    <form
      className="form"
      aria-label="Record a reference mark"
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <div className="form-row">
        <label htmlFor="ops-mark-asset">Asset</label>
        <select
          id="ops-mark-asset"
          className="input"
          value={props.assetId}
          onChange={(event) => {
            props.onAsset(event.target.value);
          }}
        >
          <option value="">Choose a SIMULATED asset</option>
          {choices.map((asset) => (
            <option key={asset.id} value={asset.id}>
              {asset.symbol}
            </option>
          ))}
          {props.assetId !== "" && choices.every((asset) => asset.id !== props.assetId) && (
            <option value={props.assetId}>{props.assetId}</option>
          )}
        </select>
      </div>
      <div className="form-row">
        <label htmlFor="ops-mark-price">Price in sandbox dollars per whole unit</label>
        <input
          id="ops-mark-price"
          className="input"
          inputMode="decimal"
          autoComplete="off"
          value={price}
          aria-describedby="ops-mark-price-note"
          onChange={(event) => {
            setPrice(event.target.value);
          }}
        />
        <p id="ops-mark-price-note" className="field-note">
          Written out exactly, with at most 18 fractional digits. It is sent as typed and never rounded.
        </p>
      </div>
      <div className="form-row">
        <label htmlFor="ops-mark-source">Source</label>
        <input
          id="ops-mark-source"
          className="input"
          autoComplete="off"
          value={source}
          aria-describedby="ops-mark-source-note"
          onChange={(event) => {
            setSource(event.target.value);
          }}
        />
        <p id="ops-mark-source-note" className="field-note">
          Who or what observed the price. A stable asset's peg monitor counts only marks from its own oracle source.
        </p>
      </div>
      <div className="form-row">
        <label htmlFor="ops-mark-observed">Observed at (optional)</label>
        <input
          id="ops-mark-observed"
          className="input"
          type="datetime-local"
          step={1}
          value={observedAt}
          aria-describedby="ops-mark-observed-note"
          onChange={(event) => {
            setObservedAt(event.target.value);
          }}
        />
        <p id="ops-mark-observed-note" className="field-note">
          In this browser's time zone. Leave it empty and the backend uses the time it receives the request.
        </p>
      </div>
      <CommandFlow<MarkRequest, ReferenceMarkRecorded>
        label="Record the mark"
        prepared={prepared}
        command={record}
        refusedNote="No mark was recorded."
        describe={(request) => (
          <FieldGrid columns={2}>
            <Field label="Asset">{symbol(request.assetId)}</Field>
            <Field label="Price in sandbox dollars">
              <DecimalValue value={request.price} />
            </Field>
            <Field label="Source">{request.source}</Field>
            <Field label="Observed at">
              {request.observedAt === undefined ? "the time the backend receives it" : exactInstant(request.observedAt)}
            </Field>
          </FieldGrid>
        )}
        renderResult={(recorded) => (
          <p className="note" role="status" aria-label="The recorded mark" data-recorded-mark={recorded.mark.id}>
            {recorded.duplicate
              ? "The backend already held this exact observation and recorded nothing new: "
              : "Recorded: "}
            {recorded.asset_symbol} at <DecimalValue value={recorded.mark.price} /> {recorded.quote_asset_symbol}, observed{" "}
            {formatInstant(recorded.mark.observed_at)} from {recorded.mark.source}.
          </p>
        )}
      />
    </form>
  );
}

function MarksPanel(props: { readonly assets: readonly Asset[]; readonly assetId: string }): ReactNode {
  const marks = useReferenceMarks(props.assetId === "" ? undefined : props.assetId);
  return (
    <Panel
      title="Reference marks"
      description="The latest marks of the chosen asset in the sandbox dollar, newest observation first, with each one's age when the backend read it. A future observation is ignored by every reader."
    >
      {props.assetId === "" ? (
        <EmptyState title="No asset chosen" body="Choose an asset above to read its reference marks." />
      ) : (
        <AsyncPanel
          query={marks}
          loadingLabel="Loading the asset's reference marks…"
          empty={{
            isEmpty: (list: ReferenceMarkList) => list.items.length === 0,
            title: "No reference mark recorded",
            body: "The backend holds no mark of this asset in the sandbox dollar.",
          }}
        >
          {(list: ReferenceMarkList) => (
            <>
              <p className="note">
                {list.asset_symbol} ({list.asset_kind}) in {list.quote_asset_symbol}; read at {formatInstant(list.as_of)}; a
                mark older than {formatDuration(list.max_age_ms)} is stale.
              </p>
              <ScrollTable
                caption={`Reference marks of ${list.asset_symbol}`}
                headers={["Observed", "Price", "Source", "Age", "State", "Received"]}
              >
                {list.items.map((mark) => (
                  <tr key={mark.id} data-mark={mark.id}>
                    <th scope="row">
                      <time dateTime={mark.observed_at} title={exactInstant(mark.observed_at)}>
                        {formatInstant(mark.observed_at)}
                      </time>
                    </th>
                    <td>
                      <DecimalValue value={mark.price} /> {list.quote_asset_symbol}
                      <span className="cell-note mono-small">
                        mantissa {mark.mantissa} · scale {String(mark.scale)}
                      </span>
                    </td>
                    <td>{mark.source}</td>
                    <td>{formatAge(mark.age_ms)}</td>
                    <td>
                      {mark.future ? (
                        <Pill tone="bad">future: ignored</Pill>
                      ) : mark.stale ? (
                        <Pill tone="warn">stale</Pill>
                      ) : (
                        <Pill tone="good">fresh</Pill>
                      )}
                    </td>
                    <td>{formatInstant(mark.received_at)}</td>
                  </tr>
                ))}
              </ScrollTable>
            </>
          )}
        </AsyncPanel>
      )}
    </Panel>
  );
}
