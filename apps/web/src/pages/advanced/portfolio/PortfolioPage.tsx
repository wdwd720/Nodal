/**
 * PORTFOLIO (advanced goal §26): one page of everything an account holds in
 * the advanced (sandbox) area -- balances by asset and chain, the capital not
 * yet free to use, buying power V2 and equity, positions in every subsystem
 * and what stands in the way of new risk -- each section linking to the page
 * it summarises.
 *
 * Everything comes from one read (GET /advanced/portfolio) whose sections are
 * the subsystems' own reads, so a figure here is the figure on that
 * subsystem's page. The page formats; it never computes, never sums two
 * figures and never draws a zero for a section the backend could not answer:
 * a subsystem this deployment does not run says so, and a refusal is shown
 * with its stable code.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { problemCode } from "../../../api/advanced/core.ts";
import { usePortfolio, type Portfolio } from "../../../api/advanced/portfolio.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { componentLabel, formatMillis } from "../../../lib/advanced.ts";
import { useAdvancedAccount } from "../area.ts";
import { Codes, NoAccount, ScrollTable, ValueDomainPill } from "../shared.tsx";
import {
  BalancesPanel,
  EquitiesPanel,
  MarginPanel,
  OrderBooksPanel,
  PendingCapitalPanel,
  PredictionMarketsPanel,
  RiskPanel,
  SeeAlso,
  SectionAbsent,
  StatePill,
  routeHref,
} from "./parts.tsx";

export function PortfolioPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  const portfolio = usePortfolio(accountId);

  return (
    <Page
      title="Portfolio"
      lead="Everything this account holds in the sandbox, read by the backend for this request: balances by asset and chain, capital not yet free to use, buying power, positions and what stands in the way of new risk. Each section links to the page it summarises."
    >
      <SimulatedNotice />

      {accountId === undefined ? (
        <Panel title="Portfolio">
          <NoAccount />
        </Panel>
      ) : portfolio.isSuccess ? (
        <PortfolioView portfolio={portfolio.data} />
      ) : (
        <Panel title="Portfolio" description="As the backend reads it for this account.">
          <AsyncPanel
            query={portfolio}
            loadingLabel="Asking the backend for this account's portfolio…"
            errorExtra={refusalNote(portfolio.error)}
          >
            {() => null}
          </AsyncPanel>
        </Panel>
      )}
    </Page>
  );
}

/** What a refusal of the whole read means here. The backend's own words are shown above it. */
function refusalNote(error: unknown): ReactNode {
  switch (problemCode(error)) {
    case "FORBIDDEN":
      return <p className="explain-body">Only the account's owner, or an operator who may read any account, can read its portfolio.</p>;
    case "UNSUPPORTED":
      return <p className="explain-body">This deployment does not serve the portfolio, so there is nothing to show in its place.</p>;
    default:
      return undefined;
  }
}

function PortfolioView(props: { readonly portfolio: Portfolio }): ReactNode {
  const p = props.portfolio;
  return (
    <>
      <Panel title="This read" description="When the backend read the balances and the risk records, and the unit every value is stated in.">
        <p className="meta-row">
          <ValueDomainPill value={p.value_domain} />
          <span className="mono-small">values in {p.quote_symbol}</span>
          <span className="mono-small">a mark values an asset for {formatMillis(p.max_mark_age_ms)}</span>
        </p>
        {!p.quote_provisioned && (
          <p className="banner banner-warn" role="status">
            This deployment has not provisioned {p.quote_symbol}, the unit every value is stated in, so no asset is valued. An
            asset the backend cannot value is not an asset worth zero.
          </p>
        )}
        <AsOf at={p.as_of} label="Read at" />
      </Panel>
      <BalancesPanel portfolio={p} />
      <PendingCapitalPanel portfolio={p} />
      <BuyingPowerPanel portfolio={p} />
      <MarginPanel portfolio={p} />
      <DerivativesPanel portfolio={p} />
      <PredictionMarketsPanel portfolio={p} />
      <EquitiesPanel portfolio={p} />
      <OrderBooksPanel portfolio={p} />
      <RiskPanel portfolio={p} />
    </>
  );
}

/* --------------------------------------------------------------------------
 * Buying power V2 and equity
 * ------------------------------------------------------------------------ */

/** The figures of buying power V2 this summary shows, in the backend's own field names. */
const BUYING_POWER_SUMMARY = [
  "trade_buying_power",
  "risk_increasing_buying_power",
  "margin_available",
  "withdrawable",
  "gross_equity",
  "eligible_equity",
  "borrow_liabilities",
  "maintenance_margin",
] as const;

function BuyingPowerPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const section = props.portfolio.buying_power;
  const bp = section.buying_power;
  return (
    <Panel
      title="Buying power and equity"
      description="Buying power V2 exactly as the buying power page computes it: what the holdings above can do, and the equity behind it."
      actions={<StatePill state={section.state} />}
    >
      {bp === undefined ? (
        <SectionAbsent section={section} subsystem="Buying power V2" />
      ) : (
        <>
          <FieldGrid columns={4}>
            {BUYING_POWER_SUMMARY.map((field) => (
              <Field key={field} label={componentLabel(field)}>
                <span data-field={field}>
                  <Usd value={bp[field]} />
                </span>
              </Field>
            ))}
          </FieldGrid>
          <p className="meta-row">
            <span className="mono-small">valued in {bp.quote_symbol} at par</span>
            <span className="mono-small">policy {bp.policy_version}</span>
          </p>
          {bp.reasons.length > 0 && (
            <>
              <h3>Reasons</h3>
              <Codes codes={bp.reasons.map((reason) => reason.code)} />
            </>
          )}
          <AsOf at={bp.as_of} label="Computed at" />
        </>
      )}
      <SeeAlso route={["buying-power"]}>Every component and explanation line on the buying power page</SeeAlso>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Derivatives
 * ------------------------------------------------------------------------ */

function DerivativesPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const section = props.portfolio.positions.derivatives;
  const page = section.positions;
  return (
    <Panel
      title="Derivative positions"
      description="The OPEN positions and the backend's valuation of the open book, as the derivatives page answers them."
      actions={<StatePill state={section.state} />}
    >
      {page === undefined ? (
        <SectionAbsent section={section} subsystem="Derivatives" />
      ) : (
        <>
          {page.book !== undefined ? (
            <FieldGrid columns={4}>
              <Field label="Gross notional">
                <Usd value={page.book.gross_notional} />
              </Field>
              <Field label="Initial margin">
                <Usd value={page.book.initial_margin} />
              </Field>
              <Field label="Maintenance margin">
                <Usd value={page.book.maintenance_margin} />
              </Field>
              <Field label="Unrealised P&L" note={`At the latest marks, in ${page.book.quote_asset.symbol}.`}>
                <Usd value={page.book.unrealized_pnl} signed />
              </Field>
            </FieldGrid>
          ) : page.book_refusal !== undefined ? (
            <div className="explain" role="status">
              <p className="explain-title">The open book could not be valued</p>
              <p className="explain-body">{page.book_refusal.detail}</p>
              <p className="explain-meta mono-small">code {page.book_refusal.code}</p>
            </div>
          ) : null}
          {page.items.length === 0 ? (
            <EmptyState title="No open positions" body="The account holds no open futures, perpetuals or options." />
          ) : (
            <ScrollTable caption="Open derivative positions" headers={["Contract", "Side", "Contracts", "Margin posted", "Unrealised P&L"]}>
              {page.items.map((pos) => (
                <tr key={pos.id} data-position={pos.id}>
                  <th scope="row">
                    <Link to={routeHref(["derivatives", "positions", pos.id])}>{pos.symbol}</Link>{" "}
                    <Pill tone="info">{pos.kind}</Pill>
                  </th>
                  <td>{pos.side ?? "flat"}</td>
                  <td>{pos.quantity}</td>
                  <td>
                    <Qty value={pos.margin_posted} decimals={pos.settlement_asset.decimals} symbol={pos.settlement_asset.symbol} />
                  </td>
                  <td>
                    {pos.valuation === undefined ? (
                      <span className="absent">{pos.valuation_status === "REFUSED" ? "valuation refused" : "not valued"}</span>
                    ) : (
                      <Qty
                        value={pos.valuation.unrealized_pnl}
                        decimals={pos.settlement_asset.decimals}
                        symbol={pos.settlement_asset.symbol}
                      />
                    )}
                  </td>
                </tr>
              ))}
            </ScrollTable>
          )}
          {page.next_cursor !== null && <p className="note">More open positions exist than are listed here.</p>}
        </>
      )}
      <SeeAlso route={["derivatives", "positions"]}>Every position, flow and funding accrual on the derivatives page</SeeAlso>
    </Panel>
  );
}
