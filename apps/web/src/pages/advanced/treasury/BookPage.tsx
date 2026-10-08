/**
 * TREASURY — one house book: its registration, the signing policy and risk
 * limits in force, its positions and PnL at fresh marks, every limit with
 * what it already uses, its balances and the reconciliation of its register
 * against the ledger.
 *
 * Every figure is the treasury's. PnL is in base units of the book's
 * valuation asset; a position without a fresh mark has no market value and
 * is named, never valued at an old price. A limit's state is the treasury's
 * own evaluation of a request that adds nothing: the codes it names decide.
 */
import type { ReactNode } from "react";
import { useParams } from "react-router-dom";

import {
  useTreasuryBalances,
  useTreasuryBook,
  useTreasuryReconciliation,
  useTreasuryReport,
  type TreasuryBalanceList,
  type TreasuryBookDetail,
  type TreasuryLimitUse,
  type TreasuryReconciliation,
  type TreasuryReport,
} from "../../../api/advanced/treasury.ts";
import type { TreasuryAssetRef } from "../../../api/advanced/fees.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Absent, Bps } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  BOOK_STATUS_MEANING,
  CLASS_MEANING,
  LIMIT_KIND_LABEL,
  LIMIT_KIND_MEANING,
  LIMIT_STATE_LABEL,
  STRATEGY_MEANING,
  bookStatusTone,
  limitState,
  limitStateTone,
} from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { Codes, ScrollTable } from "../shared.tsx";
import { AssetAmount, AssetName, BookNav, ExactPrice, TreasuryNav } from "./parts.tsx";

export function BookPage(): ReactNode {
  const { bookId = "" } = useParams();
  const book = useTreasuryBook(bookId);
  return (
    <Page title="House book" lead="One house book: its policies, its simulated positions and PnL, its limits in use, its balances and its reconciliation.">
      <SimulatedNotice />
      <TreasuryNav />
      <BookNav bookId={bookId} />
      <AsyncPanel query={book} loadingLabel="Loading the house book…">
        {(detail: TreasuryBookDetail) => (
          <>
            <BookPanel book={detail} />
            <PoliciesPanel book={detail} />
            <ReportPanel bookId={bookId} />
            <LimitsPanel bookId={bookId} book={detail} />
            <BalancesPanel bookId={bookId} />
            <ReconciliationPanel bookId={bookId} />
          </>
        )}
      </AsyncPanel>
    </Page>
  );
}

function BookPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const { book } = props;
  return (
    <Panel title={book.name} description="The house book's registration. Its value lives only in house accounts of its own.">
      <FieldGrid columns={3}>
        <Field label="Status" note={BOOK_STATUS_MEANING[book.status]}>
          <span data-field="book_status">
            <Pill tone={bookStatusTone(book.status)}>{book.status}</Pill>
          </span>
        </Field>
        <Field label="Strategy" note={STRATEGY_MEANING[book.strategy]}>
          <code className="mono-small">{book.strategy}</code>
        </Field>
        <Field label="Class" note={CLASS_MEANING[book.class]}>
          <code className="mono-small">{book.class}</code>
        </Field>
        <Field label="Valuation asset" note="Every PnL figure and limit is stated in it.">
          <AssetName asset={book.valuation_asset} />
        </Field>
        <Field label="Permitted activities" note="What a book of this strategy may do.">
          <Codes codes={book.permitted_activities} />
        </Field>
        <Field label="Version" note="Advanced by every command under the book's lock.">
          {String(book.version)}
        </Field>
        <Field label="Registered">
          {formatInstant(book.registered_at)}
          <span className="cell-note">
            {book.registered_by_actor_type} <code className="mono-small">{book.registered_by_actor_id}</code>
          </span>
        </Field>
        <Field label="House book id">
          <Identifier value={book.house_account_id} />
        </Field>
        <Field label="Value domain">
          <Pill tone="warn">{book.value_domain}</Pill>
        </Field>
      </FieldGrid>
    </Panel>
  );
}

function PoliciesPanel(props: { readonly book: TreasuryBookDetail }): ReactNode {
  const { book } = props;
  const sp = book.signing_policy;
  const rp = book.risk_policy;
  const valuation = book.valuation_asset;
  return (
    <Panel title="Policies in force" description="The signing policy says who may act for the book; the risk policy bounds what it may do. Both are versioned and hashed.">
      <h3>Signing policy v{String(sp.version)}</h3>
      <FieldGrid columns={3}>
        <Field label="House service" note="The one service that may act for the book.">
          <code className="mono-small">{sp.service_actor_id}</code>
        </Field>
        <Field label="Operators may trade" note="Operators may always cancel and administer.">
          <span data-field="operators_allowed">{sp.operators_allowed ? "yes" : "no"}</span>
        </Field>
        <Field label="Key custody" note="A sandbox house book holds no keys.">
          <code className="mono-small">{sp.key_custody}</code>
        </Field>
        <Field label="Recorded">
          {formatInstant(sp.effective_at)}
          <span className="cell-note">
            {sp.actor_type} <code className="mono-small">{sp.actor_id}</code>: {sp.reason}
          </span>
        </Field>
        <Field label="Hash">
          <span className="mono-small">{sp.policy_hash}</span>
        </Field>
      </FieldGrid>
      {rp === undefined ? (
        <p className="note" role="status" data-field="risk_policy">
          The book has not recorded a risk policy yet. Until it does, the treasury refuses every house order and fill.
        </p>
      ) : (
        <>
          <h3>Risk limits v{String(rp.version)} — {rp.label}</h3>
          <FieldGrid columns={3}>
            <Field label="Daily loss limit" note="New risk stops while the day's loss measure is below its negative.">
              <AssetAmount value={rp.daily_loss_limit} asset={valuation} />
            </Field>
            <Field label="Stalest mark accepted" note="In milliseconds; an older mark is no mark.">
              <span className="num">{String(rp.max_mark_age_ms)} ms</span>
            </Field>
            <Field label="Paper venue spread and fee" note="What the simulated paper venue charges the house book, disclosed per fill.">
              <Bps value={rp.paper_spread_bps} /> <span className="cell-note">fee <Bps value={rp.paper_fee_bps} /></span>
            </Field>
            <Field label="Recorded">
              {formatInstant(rp.effective_at)}
              <span className="cell-note">
                {rp.actor_type} <code className="mono-small">{rp.actor_id}</code>: {rp.reason}
              </span>
            </Field>
            <Field label="Hash">
              <span className="mono-small">{rp.policy_hash}</span>
            </Field>
          </FieldGrid>
          <ScrollTable caption="Inventory caps" headers={["Asset", "Most the book may hold, with working buys"]}>
            {rp.max_inventory.length === 0 ? (
              <tr>
                <td colSpan={2} className="absent">
                  No asset is named: the book can accumulate nothing.
                </td>
              </tr>
            ) : (
              rp.max_inventory.map((limit) => (
                <tr key={limit.asset_id}>
                  <th scope="row">
                    <Identifier value={limit.asset_id} />
                  </th>
                  <td>
                    <code className="mono-small">{limit.max_quantity} base units</code>
                  </td>
                </tr>
              ))
            )}
          </ScrollTable>
          <ScrollTable caption="Instrument exposure caps" headers={["Instrument", "Most exposure, held at a mark plus working buys"]}>
            {rp.max_instrument_exposure.length === 0 ? (
              <tr>
                <td colSpan={2} className="absent">
                  No instrument is named: the book can buy through none.
                </td>
              </tr>
            ) : (
              rp.max_instrument_exposure.map((limit) => (
                <tr key={limit.instrument_id}>
                  <th scope="row">
                    <Identifier value={limit.instrument_id} />
                  </th>
                  <td>
                    <AssetAmount value={limit.max_exposure} asset={valuation} />
                  </td>
                </tr>
              ))
            )}
          </ScrollTable>
        </>
      )}
    </Panel>
  );
}

function ReportPanel(props: { readonly bookId: string }): ReactNode {
  const report = useTreasuryReport(props.bookId);
  return (
    <Panel
      title="Positions and PnL — simulated"
      description="Realized PnL at average cost and unrealized PnL at fresh marks, as the treasury states them after syncing its register. Figures are base units of the valuation asset."
    >
      <AsyncPanel query={report} loadingLabel="Loading the PnL report…">
        {(data: TreasuryReport) => {
          const valuation = data.valuation_asset;
          return (
            <>
              <AsOf at={data.at} label="Stated at" />
              <p className="meta-row">
                <span className="mono-small">trading day {data.trading_day} (UTC)</span>
                <span className="mono-small">through register event {String(data.through_event_seq)}</span>
              </p>
              <FieldGrid columns={3}>
                <Field label="Realized, all time">
                  <span data-field="realized_total">
                    <AssetAmount value={data.realized_total} asset={valuation} />
                  </span>
                </Field>
                <Field label="Realized today" note="Every register event of the UTC trading day.">
                  <span data-field="realized_today">
                    <AssetAmount value={data.realized_today} asset={valuation} />
                  </span>
                </Field>
                <Field label="Unrealized" note="Positions with a fresh mark only.">
                  <span data-field="unrealized_total">
                    <AssetAmount value={data.unrealized_total} asset={valuation} />
                  </span>
                </Field>
                <Field label="Open losses" note="The sum of every negative unrealized PnL: a gain never offsets a loss.">
                  <span data-field="open_losses">
                    <AssetAmount value={data.open_losses} asset={valuation} />
                  </span>
                </Field>
                <Field label="Loss measure" emphasis note="Realized today plus open losses: what the daily loss limit binds.">
                  <span data-field="loss_measure">
                    <AssetAmount value={data.loss_measure} asset={valuation} />
                  </span>
                </Field>
                <Field label="Positions without a fresh mark" note="Their open loss cannot be known; new risk in them is refused.">
                  <Codes codes={data.missing_marks} none="none" />
                </Field>
              </FieldGrid>
              <PositionsTable report={data} valuation={valuation} />
              <p className="field-note">
                Rounding:{" "}
                {Object.entries(data.rounding)
                  .map(([what, mode]) => `${what} ${mode}`)
                  .join("; ")}
                .
              </p>
            </>
          );
        }}
      </AsyncPanel>
    </Panel>
  );
}

function PositionsTable(props: { readonly report: TreasuryReport; readonly valuation: TreasuryAssetRef }): ReactNode {
  const { report, valuation } = props;
  if (report.assets.length === 0) {
    return (
      <p className="note" role="status">
        The position register holds no position: the book has never held anything but its valuation asset.
      </p>
    );
  }
  return (
    <ScrollTable caption="Positions" headers={["Asset", "Quantity", "Cost", "Mark", "Market value", "Unrealized", "Realized total / today"]}>
      {report.assets.map((pnl) => (
        <tr key={pnl.asset.asset_id} data-position={pnl.asset.asset_id}>
          <th scope="row">
            <AssetName asset={pnl.asset} />
          </th>
          <td>
            <AssetAmount value={pnl.quantity} asset={pnl.asset} />
          </td>
          <td>
            <AssetAmount value={pnl.cost} asset={valuation} />
          </td>
          <td>{pnl.mark === undefined ? <Absent what="no fresh mark" /> : <ExactPrice price={pnl.mark} unit={valuation.symbol} />}</td>
          <td>
            <AssetAmount value={pnl.market_value} asset={valuation} absent="not valued: no fresh mark" />
          </td>
          <td>
            <AssetAmount value={pnl.unrealized} asset={valuation} absent="not known" />
          </td>
          <td>
            <AssetAmount value={pnl.realized_total} asset={valuation} />
            <span className="cell-note">
              <AssetAmount value={pnl.realized_today} asset={valuation} />
            </span>
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

function LimitSubject(props: { readonly limit: TreasuryLimitUse }): ReactNode {
  if (props.limit.asset_id !== undefined) return <Identifier label="asset" value={props.limit.asset_id} />;
  if (props.limit.instrument_id !== undefined) return <Identifier label="instrument" value={props.limit.instrument_id} />;
  return <span>the whole book</span>;
}

function LimitFigure(props: { readonly limit: TreasuryLimitUse; readonly value: string | undefined; readonly valuation: TreasuryAssetRef; readonly absent: string }): ReactNode {
  if (props.value === undefined) return <Absent what={props.absent} />;
  // Inventory is counted in base units of the capped asset, whose decimals the
  // limit does not carry; the other two are in the valuation asset.
  if (props.limit.kind === "INVENTORY") return <code className="mono-small">{props.value} base units</code>;
  return <AssetAmount value={props.value} asset={props.valuation} />;
}

function LimitsPanel(props: { readonly bookId: string; readonly book: TreasuryBookDetail }): ReactNode {
  const report = useTreasuryReport(props.bookId);
  return (
    <Panel
      title="Limits in use"
      description="Every limit of the risk policy in force, its cap, what is already used, and what new risk would meet now — the treasury's own evaluation. Limits bind only risk-increasing activity; a sale is never stopped by them."
    >
      <AsyncPanel query={report} loadingLabel="Loading the limits in use…">
        {(data: TreasuryReport) => {
          const limits = data.limits ?? [];
          if (data.risk_policy_version === undefined || limits.length === 0) {
            return (
              <p className="note" role="status" data-field="limits">
                The book has not recorded a risk policy yet, so there are no limits to evaluate; the treasury refuses every house order and fill until it does.
              </p>
            );
          }
          return (
            <>
              <p className="meta-row">
                <span className="mono-small">risk policy v{String(data.risk_policy_version)}</span>
              </p>
              <ScrollTable caption="Limits in use" headers={["Limit", "Subject", "Cap", "Used", "State", "Codes"]}>
                {limits.map((limit, index) => {
                  const state = limitState(limit);
                  return (
                    <tr key={`${limit.kind}-${limit.asset_id ?? limit.instrument_id ?? String(index)}`} data-limit={limit.kind}>
                      <th scope="row">
                        {LIMIT_KIND_LABEL[limit.kind]}
                        <span className="cell-note">{LIMIT_KIND_MEANING[limit.kind]}</span>
                      </th>
                      <td>
                        <LimitSubject limit={limit} />
                      </td>
                      <td>
                        <LimitFigure limit={limit} value={limit.cap} valuation={props.book.valuation_asset} absent="not stated" />
                      </td>
                      <td>
                        <LimitFigure limit={limit} value={limit.used} valuation={props.book.valuation_asset} absent="not known: no fresh mark" />
                      </td>
                      <td>
                        <Pill tone={limitStateTone(state)}>{LIMIT_STATE_LABEL[state]}</Pill>
                      </td>
                      <td>
                        <Codes codes={limit.codes} none="none" />
                      </td>
                    </tr>
                  );
                })}
              </ScrollTable>
            </>
          );
        }}
      </AsyncPanel>
    </Panel>
  );
}

function BalancesPanel(props: { readonly bookId: string }): ReactNode {
  const balances = useTreasuryBalances(props.bookId);
  return (
    <Panel
      title="Balances"
      description="What the ledger holds for the book, what working orders have reserved, and how fundings and each venue built it up, asset by asset."
    >
      <AsyncPanel
        query={balances}
        loadingLabel="Loading the balances…"
        empty={{
          isEmpty: (data: TreasuryBalanceList) => data.items.length === 0,
          title: "Nothing held",
          body: "The book has never been funded or traded.",
        }}
      >
        {(data: TreasuryBalanceList) => (
          <ScrollTable
            caption="Balances"
            headers={["Asset", "Inventory", "Reserved", "Available", "Funded", "Internal book bought / sold", "Paper venue bought / sold", "Working buys / sells"]}
          >
            {data.items.map((balance) => (
              <tr key={balance.asset.asset_id} data-balance={balance.asset.asset_id}>
                <th scope="row">
                  <AssetName asset={balance.asset} />
                </th>
                <td>
                  <AssetAmount value={balance.inventory} asset={balance.asset} />
                </td>
                <td>
                  <AssetAmount value={balance.reserved} asset={balance.asset} />
                </td>
                <td>
                  <AssetAmount value={balance.available} asset={balance.asset} />
                </td>
                <td>
                  <AssetAmount value={balance.funded} asset={balance.asset} />
                </td>
                <td>
                  <AssetAmount value={balance.internal_book.bought} asset={balance.asset} />
                  <span className="cell-note">
                    <AssetAmount value={balance.internal_book.sold} asset={balance.asset} />
                  </span>
                </td>
                <td>
                  <AssetAmount value={balance.paper_venue.bought} asset={balance.asset} />
                  <span className="cell-note">
                    <AssetAmount value={balance.paper_venue.sold} asset={balance.asset} />
                  </span>
                </td>
                <td>
                  <AssetAmount value={balance.working_buys} asset={balance.asset} />
                  <span className="cell-note">
                    <AssetAmount value={balance.working_sells} asset={balance.asset} />
                  </span>
                </td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function ReconciliationPanel(props: { readonly bookId: string }): ReactNode {
  const reconciliation = useTreasuryReconciliation(props.bookId);
  return (
    <Panel
      title="Reconciliation"
      description="The position register proved against the ledger, asset by asset: positions against inventory, fundings and venues against inventory, the valuation asset against costs and proceeds, and the fees."
    >
      <AsyncPanel query={reconciliation} loadingLabel="Reconciling the register against the ledger…">
        {(data: TreasuryReconciliation) =>
          data.agrees ? (
            <p data-field="reconciliation">
              <Pill tone="good">the register and the ledger agree</Pill>
            </p>
          ) : (
            <>
              <p data-field="reconciliation">
                <Pill tone="bad">the register and the ledger disagree</Pill>
              </p>
              <ScrollTable caption="Discrepancies" headers={["Asset", "What", "Expected (base units)", "Actual (base units)"]}>
                {data.discrepancies.map((d, index) => (
                  <tr key={`${d.asset_id}-${String(index)}`}>
                    <th scope="row">
                      <Identifier value={d.asset_id} />
                    </th>
                    <td className="cell-prose">{d.what}</td>
                    <td>
                      <code className="mono-small">{d.expected}</code>
                    </td>
                    <td>
                      <code className="mono-small">{d.actual}</code>
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            </>
          )
        }
      </AsyncPanel>
    </Panel>
  );
}
