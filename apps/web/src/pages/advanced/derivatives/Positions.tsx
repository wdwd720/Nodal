/**
 * DERIVATIVE POSITIONS of the account the area reads, newest first, each with
 * what the backend says about it: its entry, its unrealized result at the
 * latest mark, its initial and maintenance requirement, the margin it holds
 * and the funding it has paid or received.
 *
 * The backend values the account's open book as a whole and fails closed: if
 * any open position cannot be valued (a stale or missing mark), none is, and
 * the page shows that refusal with its code instead of a figure that could
 * understate the risk.
 */
import { useId, useState, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { useDerivativePositions, type DerivativeBook, type DerivativePosition, type DerivativePositionPage } from "../../../api/advanced/derivatives.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Bps, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { POSITION_STATUSES, VALUATION_MEANING, type PositionStatus } from "../../../lib/derivatives.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { Amount, DerivativesNav, Price, RefusalNote, derivativesHref } from "./parts.tsx";

function parseStatus(raw: string | null): PositionStatus | undefined {
  return POSITION_STATUSES.find((status) => status === raw);
}

export function PositionsPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  const [params, setParams] = useSearchParams();
  const status = parseStatus(params.get("status"));
  const selectId = useId();
  return (
    <Page
      title="Derivative positions"
      lead="The account's futures, perpetuals and options positions, valued by the backend at each contract's latest mark."
      actions={
        <div className="filter">
          <label htmlFor={selectId}>Status</label>
          <select
            id={selectId}
            className="input"
            value={status ?? ""}
            onChange={(event) => {
              const value = event.target.value;
              setParams(value === "" ? {} : { status: value });
            }}
          >
            <option value="">Every status</option>
            {POSITION_STATUSES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>
      }
    >
      <SimulatedNotice />
      <DerivativesNav />
      {accountId === undefined ? (
        <Panel title="Positions">
          <NoAccount />
        </Panel>
      ) : (
        <PositionsPanels key={`${accountId}-${status ?? ""}`} accountId={accountId} status={status} />
      )}
    </Page>
  );
}

function PositionsPanels(props: { readonly accountId: string; readonly status: PositionStatus | undefined }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const positions = useDerivativePositions(props.accountId, props.status, trail.current);
  return (
    <AsyncPanel query={positions} loadingLabel="Loading positions…">
      {(page: DerivativePositionPage) => (
        <>
          <Panel title="The open book" description="The account's open derivative positions as the backend values them, in the asset it states.">
            {page.book_status === "VALUED" && page.book !== undefined ? (
              <BookFields book={page.book} />
            ) : (
              <RefusalNote
                what="The backend could not value this account's open book"
                code={page.book_refusal?.code ?? "not stated"}
                detail={page.book_refusal?.detail ?? "The backend gave no detail."}
              />
            )}
          </Panel>
          <Panel title="Positions" description="Newest first. Amounts are in each contract's settlement asset.">
            {page.items.length === 0 && trail.previous.length === 0 ? (
              <EmptyState
                title="No positions"
                body={`The backend lists no derivative position${
                  props.status === undefined ? "" : ` with status ${props.status}`
                } for this account. That is its answer, not a zero.`}
              />
            ) : (
              <>
                <ScrollTable
                  caption="Positions on this page"
                  headers={[
                    "Contract",
                    "Position",
                    "Average entry",
                    "Mark",
                    "Unrealized",
                    "Initial / maintenance",
                    "Margin posted",
                    "Funding",
                    "Realized",
                  ]}
                >
                  {page.items.map((position) => (
                    <PositionRow key={position.id} position={position} />
                  ))}
                </ScrollTable>
                <CursorPager
                  noun="positions"
                  trail={trail}
                  nextCursor={page.next_cursor}
                  onForward={(cursor) => {
                    setTrail(forward(trail, cursor));
                  }}
                  onBack={() => {
                    setTrail(back(trail));
                  }}
                />
              </>
            )}
          </Panel>
        </>
      )}
    </AsyncPanel>
  );
}

function BookFields(props: { readonly book: DerivativeBook }): ReactNode {
  const { book } = props;
  return (
    <>
      <AsOf at={book.as_of} label="Valued as of" />
      <FieldGrid columns={3}>
        <Field label="Gross notional" note={`Valued in ${book.quote_asset.symbol}, the sandbox dollar: simulated value, not money. Requirements round up.`}>
          <Usd value={book.gross_notional} />
        </Field>
        <Field label="Initial margin required">
          <Usd value={book.initial_margin} />
        </Field>
        <Field label="Maintenance margin required">
          <Usd value={book.maintenance_margin} />
        </Field>
        <Field label="Unrealized result" note="At each contract's latest mark; rounds down.">
          <Usd value={book.unrealized_pnl} signed />
        </Field>
        <Field label="Equity beyond ledger balances" note="What the open positions add to equity that the ledger does not yet hold.">
          <Usd value={book.equity_adjustment} signed />
        </Field>
        <Field label="Highest linear initial rate">
          <Bps value={book.max_initial_bps} />
        </Field>
      </FieldGrid>
      <p className="note">
        {String(book.positions)} open {book.positions === 1 ? "position" : "positions"} valued under policy{" "}
        <code className="mono-small">{book.policy_version}</code>.
      </p>
    </>
  );
}

function PositionRow(props: { readonly position: DerivativePosition }): ReactNode {
  const { position } = props;
  const asset = position.settlement_asset;
  const v = position.valuation;
  return (
    <tr>
      <th scope="row">
        <Link to={derivativesHref("positions", position.id)}>{position.symbol}</Link>
        <span className="cell-note">{position.kind}</span>
      </th>
      <td>
        <Pill tone={position.status === "OPEN" ? "info" : "neutral"}>{position.status}</Pill>
        {position.side !== undefined && <Pill tone="neutral">{position.side}</Pill>}
        <span className="cell-note">{position.quantity} contracts</span>
      </td>
      {v === undefined ? (
        <td colSpan={4}>
          <span className="absent">{VALUATION_MEANING[position.valuation_status]}</span>
          {position.valuation_refusal !== undefined && (
            <span className="cell-note mono-small">code {position.valuation_refusal.code}</span>
          )}
        </td>
      ) : (
        <>
          <td>
            <Price mantissa={v.average_entry_price_mantissa} scale={position.price_scale} />
          </td>
          <td>
            <Price mantissa={v.mark_mantissa} scale={position.price_scale} />
          </td>
          <td>
            <Amount value={v.unrealized_pnl} asset={asset} />
          </td>
          <td>
            <Amount value={v.requirement.initial} asset={asset} />
            <span className="cell-note">
              maintenance <Amount value={v.requirement.maintenance} asset={asset} />
            </span>
          </td>
        </>
      )}
      <td>
        <Amount value={position.margin_posted} asset={asset} />
      </td>
      <td>
        <Amount value={position.funding_pnl} asset={asset} />
      </td>
      <td>
        <Amount value={position.realized_pnl} asset={asset} />
      </td>
    </tr>
  );
}
