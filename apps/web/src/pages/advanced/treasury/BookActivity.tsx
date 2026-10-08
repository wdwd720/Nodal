/**
 * TREASURY — what one house book did: the register of house actions (who did
 * what, under which policies, with what outcome — refusals included), the
 * position register its PnL comes from, its paper-venue fills with the
 * disclosure each was priced under, and the Risk Kernel V2 decisions about
 * its orders and fills with the treasury limit evaluation that fed each.
 */
import { useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";

import {
  useTreasuryActions,
  useTreasuryBook,
  useTreasuryEvents,
  useTreasuryPaperFills,
  useTreasuryRiskDecisions,
  type TreasuryActionList,
  type TreasuryBookDetail,
  type TreasuryPaperFill,
  type TreasuryPositionEvent,
  type TreasuryRiskDecisionRecord,
} from "../../../api/advanced/treasury.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import type { TreasuryAssetRef } from "../../../api/advanced/fees.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { EVENT_KIND_MEANING, verdictTone } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { Codes, CursorPager, ScrollTable } from "../shared.tsx";
import { ActionsTable, AssetAmount, AssetName, BookNav, LimitEvaluationView, PaperFillView, TreasuryNav } from "./parts.tsx";

export function BookActivityPage(): ReactNode {
  const { bookId = "" } = useParams();
  const book = useTreasuryBook(bookId);
  return (
    <Page title="House book activity" lead="Every house action, the position register, the simulated paper fills and the risk decisions of one house book.">
      <SimulatedNotice />
      <TreasuryNav />
      <BookNav bookId={bookId} />
      <AsyncPanel query={book} loadingLabel="Loading the house book…">
        {(detail: TreasuryBookDetail) => (
          <>
            <ActionsPanel bookId={bookId} />
            <EventsPanel bookId={bookId} valuation={detail.valuation_asset} />
            <FillsPanel bookId={bookId} />
            <DecisionsPanel bookId={bookId} valuation={detail.valuation_asset} />
          </>
        )}
      </AsyncPanel>
    </Page>
  );
}

function ActionsPanel(props: { readonly bookId: string }): ReactNode {
  const actions = useTreasuryActions(props.bookId);
  return (
    <Panel
      title="House actions"
      description="The register of every action taken for the book, newest first, with the signing and risk policy versions it was taken under. A refused action is recorded with its codes."
    >
      <AsyncPanel
        query={actions}
        loadingLabel="Loading the house actions…"
        empty={{
          isEmpty: (data: TreasuryActionList) => data.items.length === 0,
          title: "No actions",
          body: "The backend reports no action of this book.",
        }}
      >
        {(data: TreasuryActionList) => <ActionsTable actions={data.items} />}
      </AsyncPanel>
    </Panel>
  );
}

function EventsPanel(props: { readonly bookId: string; readonly valuation: TreasuryAssetRef }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const events = useTreasuryEvents(props.bookId, trail.current);
  const valuation = props.valuation;
  return (
    <Panel
      title="Position register"
      description="Every fill and funding of the book as a gap-free chain, newest first: the position before and after, the cost released at average cost, and the realized PnL. Cost, value and PnL are in the valuation asset."
    >
      <AsyncPanel
        query={events}
        loadingLabel="Loading the position register…"
        empty={{
          isEmpty: (page: CursorPage<TreasuryPositionEvent>) => page.items.length === 0 && trail.previous.length === 0,
          title: "The register is empty",
          body: "The book has held nothing but its valuation asset.",
        }}
      >
        {(page: CursorPage<TreasuryPositionEvent>) => (
          <>
            <ScrollTable
              caption="Position register"
              headers={["Event", "Kind", "Asset and quantity", "Value / fees", "Position before → after", "Cost before → after", "Cost released", "Realized PnL", "Source"]}
            >
              {page.items.map((event) => (
                <tr key={event.id} data-event-seq={String(event.seq)}>
                  <th scope="row">
                    {String(event.seq)}
                    <span className="cell-note">{formatInstant(event.occurred_at)}</span>
                  </th>
                  <td>
                    <code className="mono-small">{event.kind}</code> {event.direction}
                    <span className="cell-note">{EVENT_KIND_MEANING[event.kind]}</span>
                  </td>
                  <td>
                    <AssetName asset={event.asset} />
                    <span className="cell-note">
                      <AssetAmount value={event.quantity} asset={event.asset} />
                    </span>
                  </td>
                  <td>
                    <AssetAmount value={event.value} asset={valuation} />
                    <span className="cell-note">
                      <AssetAmount value={event.fees} asset={valuation} />
                    </span>
                  </td>
                  <td>
                    <AssetAmount value={event.qty_before} asset={event.asset} /> →{" "}
                    <AssetAmount value={event.qty_after} asset={event.asset} />
                  </td>
                  <td>
                    <AssetAmount value={event.cost_before} asset={valuation} /> →{" "}
                    <AssetAmount value={event.cost_after} asset={valuation} />
                  </td>
                  <td>
                    <AssetAmount value={event.cost_removed} asset={valuation} />
                  </td>
                  <td>
                    <AssetAmount value={event.realized_pnl} asset={valuation} />
                  </td>
                  <td>
                    {event.book_trade_id !== undefined && <Identifier label="trade" value={event.book_trade_id} />}
                    {event.paper_fill_id !== undefined && <Identifier label="fill" value={event.paper_fill_id} />}
                    {event.funding_id !== undefined && <Identifier label="funding" value={event.funding_id} />}
                    <Identifier label="journal" value={event.journal_transaction_id} />
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="events"
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

function FillsPanel(props: { readonly bookId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const fills = useTreasuryPaperFills(props.bookId, trail.current);
  return (
    <Panel
      title="Paper venue fills"
      description="Immediate fills on the simulated paper venue, newest first, each with the fee schedule and disclosure it was priced and verified under: every unit of spread and fee the house paid is recomputable from them."
    >
      <AsyncPanel
        query={fills}
        loadingLabel="Loading the paper fills…"
        empty={{
          isEmpty: (page: CursorPage<TreasuryPaperFill>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No paper fills",
          body: "The book has made no fill on the simulated paper venue.",
        }}
      >
        {(page: CursorPage<TreasuryPaperFill>) => (
          <>
            {page.items.map((fill) => (
              <section key={fill.id} className="callout" aria-label={`Paper fill ${fill.id}`} data-fill={fill.id}>
                <h3>
                  {fill.side} {fill.base_asset.symbol} — {formatInstant(fill.filled_at)}
                </h3>
                <PaperFillView fill={fill} />
              </section>
            ))}
            <CursorPager
              noun="fills"
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

function DecisionsPanel(props: { readonly bookId: string; readonly valuation: TreasuryAssetRef }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const decisions = useTreasuryRiskDecisions(props.bookId, trail.current);
  return (
    <Panel
      title="Risk decisions"
      description="Risk Kernel V2's decision about every house order and fill, newest first, with the treasury limits that entered it as circuit breakers. A breached limit never lets new risk through."
    >
      <AsyncPanel
        query={decisions}
        loadingLabel="Loading the risk decisions…"
        empty={{
          isEmpty: (page: CursorPage<TreasuryRiskDecisionRecord>) => page.items.length === 0 && trail.previous.length === 0,
          title: "Nothing evaluated yet",
          body: "No house order or fill of this book has been evaluated.",
        }}
      >
        {(page: CursorPage<TreasuryRiskDecisionRecord>) => (
          <>
            {page.items.map((decision) => (
              <section key={decision.id} className="callout" aria-label={`Risk decision ${decision.id}`} data-decision={decision.id}>
                <h3>
                  <Pill tone={verdictTone(decision.verdict)}>{decision.verdict}</Pill> {decision.intent_kind} — {formatInstant(decision.evaluated_at)}
                </h3>
                <p className="meta-row">
                  <span className="mono-small">stage {decision.stage}</span>
                  <span className="mono-small">risk policy {decision.risk_policy_version}</span>
                  <span className="mono-small">treasury policy v{String(decision.treasury_policy_version)}</span>
                  <span className="mono-small">evaluator {decision.evaluator_version}</span>
                </p>
                <p className="meta-row">
                  <Identifier label="asset" value={decision.asset_id} />
                  {decision.instrument_id !== undefined && <Identifier label="instrument" value={decision.instrument_id} />}
                </p>
                <Codes codes={[...decision.reason_codes, ...decision.treasury_codes]} none="no reason codes: allowed" />
                <LimitEvaluationView limits={decision.limits} valuation={props.valuation} />
                <p className="meta-row">
                  <span className="mono-small">decision {decision.decision_hash}</span>
                  <span className="mono-small">input {decision.input_hash}</span>
                </p>
              </section>
            ))}
            <CursorPager
              noun="decisions"
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
