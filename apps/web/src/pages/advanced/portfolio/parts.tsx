/**
 * Pieces of the portfolio page. Each renders figures exactly as the backend
 * sent them -- base units through <Qty> with the decimals the backend named,
 * decimal prices through <DecimalValue> -- and nothing here adds, converts or
 * compares an amount: the page shows the backend's arithmetic, never its own.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type {
  Portfolio,
  PortfolioBalance,
  PortfolioPredictionHolding,
  PortfolioProblem,
  PortfolioValuation,
} from "../../../api/advanced/portfolio.ts";
import { EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Panel, Pill } from "../../../components/Layout.tsx";
import { DecimalValue, Qty } from "../../../components/Money.tsx";
import { formatMillis, verdictTone } from "../../../lib/advanced.ts";
import { accountStatusTone } from "../../../lib/margin.ts";
import {
  LEDGER_CODE_COPY,
  PURPOSE_COPY,
  formatLeverageBps,
  isZeroQuantity,
  ledgerCodeLabel,
  pendingCapital,
  purposeLabel,
  sectionStateCopy,
  sectionStateTone,
  type SectionRoute,
} from "../../../lib/portfolio.ts";
import { statusTone as marketStatusTone } from "../../../lib/predmarket.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref } from "../area.ts";
import { Codes, ScrollTable } from "../shared.tsx";

export function routeHref(route: SectionRoute): string {
  return advancedHref(...route);
}

/** The link from a section to the page it summarises. */
export function SeeAlso(props: { readonly route: SectionRoute; readonly children: ReactNode }): ReactNode {
  return (
    <p className="meta-row">
      <Link to={routeHref(props.route)}>{props.children}</Link>
    </p>
  );
}

/** A section's state, as the backend reported it. */
export function StatePill(props: { readonly state: string }): ReactNode {
  return <Pill tone={sectionStateTone(props.state)}>{props.state}</Pill>;
}

/**
 * What a section that carries no answer shows instead: that the subsystem is
 * not configured here, that the account has nothing there, or the refusal of
 * the subsystem's own read with its stable code. Never a zero.
 */
export function SectionAbsent(props: {
  readonly section: { readonly state: string; readonly problem?: PortfolioProblem };
  readonly subsystem: string;
  readonly children?: ReactNode;
}): ReactNode {
  const copy = sectionStateCopy(props.section.state, props.subsystem);
  const problem = props.section.problem;
  if (props.section.state === "UNAVAILABLE" && problem !== undefined) {
    return (
      <div className="explain" role="status">
        <p className="explain-title">{copy.title}</p>
        <p className="explain-body">{problem.detail}</p>
        <p className="explain-body">{copy.body}</p>
        <p className="explain-meta mono-small">code {problem.code}</p>
      </div>
    );
  }
  return (
    <>
      <EmptyState title={copy.title} body={copy.body} />
      {props.children}
    </>
  );
}

/* --------------------------------------------------------------------------
 * Balances
 * ------------------------------------------------------------------------ */

function QuoteQty(props: { readonly value: string | undefined; readonly portfolio: Portfolio }): ReactNode {
  return <Qty value={props.value} decimals={props.portfolio.quote_decimals} symbol={props.portfolio.quote_symbol} />;
}

/** How an asset is valued in the sandbox dollar, or why it is not. */
export function ValuationCell(props: { readonly valuation: PortfolioValuation; readonly portfolio: Portfolio }): ReactNode {
  const { valuation: v, portfolio } = props;
  const owed =
    v.owed_value === undefined ? null : (
      <p className="cell-note">
        owes <QuoteQty value={v.owed_value} portfolio={portfolio} />
      </p>
    );
  if (v.status === "UNVALUED" || v.value === undefined) {
    return (
      <div>
        <Pill tone="warn">Unvalued</Pill>
        <p className="cell-prose">{v.problem?.detail}</p>
        <p className="mono-small">code {v.problem?.code}</p>
      </div>
    );
  }
  return (
    <div>
      <span data-field="value">
        <QuoteQty value={v.value} portfolio={portfolio} />
      </span>
      {v.mark === undefined ? (
        <p className="cell-note">at par: the unit every value here is stated in</p>
      ) : (
        <p className="cell-note">
          at <DecimalValue value={v.mark.price} /> {portfolio.quote_symbol} per whole unit, marked by {v.mark.source}{" "}
          {formatMillis(v.mark.age_ms)} before this read
        </p>
      )}
      {owed}
    </div>
  );
}

function LedgerList(props: {
  readonly items: PortfolioBalance["held_elsewhere"];
  readonly balance: PortfolioBalance;
  readonly none: string;
}): ReactNode {
  if (props.items.length === 0) return <span className="absent">{props.none}</span>;
  return (
    <ul className="codes">
      {props.items.map((item) => (
        <li key={item.code} data-code={item.code}>
          <Link to={routeHref(LEDGER_CODE_COPY[item.code].route)}>{ledgerCodeLabel(item.code)}</Link>{" "}
          <Qty value={item.quantity} decimals={props.balance.decimals} />
        </li>
      ))}
    </ul>
  );
}

function ReservedCell(props: { readonly balance: PortfolioBalance }): ReactNode {
  const b = props.balance;
  return (
    <div>
      <Qty value={b.reserved} decimals={b.decimals} />
      {b.reservations.length > 0 && (
        <ul className="codes">
          {b.reservations.map((r, index) => (
            <li key={`${r.purpose}-${r.capability ?? ""}-${String(index)}`} data-purpose={r.purpose}>
              <Link to={routeHref(PURPOSE_COPY[r.purpose].route)}>{purposeLabel(r.purpose)}</Link>
              {r.capability !== undefined && <code className="mono-small"> {r.capability}</code>}{" "}
              <Qty value={r.quantity} decimals={b.decimals} />{" "}
              <span className="mono-small">
                ({String(r.reservations)} reservation{r.reservations === 1 ? "" : "s"})
              </span>
            </li>
          ))}
        </ul>
      )}
      {!isZeroQuantity(b.withdrawal_holds) && (
        <p className="cell-note">
          withdrawal holds <Qty value={b.withdrawal_holds} decimals={b.decimals} />
        </p>
      )}
    </div>
  );
}

export function BalancesPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const p = props.portfolio;
  return (
    <Panel
      title="Balances"
      description={`Every simulated asset this account holds, on its chain, as the ledger, the capital reservations and the price store say. Quantities are exact base units in each asset's own decimals; values are in ${p.quote_symbol}.`}
    >
      {p.balances.length === 0 ? (
        <EmptyState
          title="No simulated assets on this account"
          body="The ledger holds no simulated asset for this account in a wallet, as collateral, awaiting settlement or in flight. Sandbox dollars come from the faucet on the transfers page."
        />
      ) : (
        <ScrollTable
          caption="Simulated balances by asset and chain"
          headers={["Asset", "Wallet", "Reserved", "Available", "Held elsewhere", "Owed", "Total", `Value in ${p.quote_symbol}`]}
        >
          {p.balances.map((b) => (
            <tr key={b.asset_id} data-asset={b.asset_id}>
              <th scope="row">
                <strong>{b.symbol}</strong> <Pill tone="info">{b.chain}</Pill>
                <br />
                <Identifier value={b.asset_id} />
              </th>
              <td data-field="wallet">
                <Qty value={b.wallet} decimals={b.decimals} />
              </td>
              <td data-field="reserved">
                <ReservedCell balance={b} />
              </td>
              <td data-field="available">
                <Qty value={b.available} decimals={b.decimals} />
              </td>
              <td data-field="held_elsewhere">
                <LedgerList items={b.held_elsewhere} balance={b} none="none" />
              </td>
              <td data-field="owed">
                <LedgerList items={b.owed} balance={b} none="nothing owed" />
              </td>
              <td data-field="total">
                <Qty value={b.total} decimals={b.decimals} symbol={b.symbol} />
              </td>
              <td data-field="valuation">
                <ValuationCell valuation={b.valuation} portfolio={p} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      <SeeAlso route={["bridge"]}>Holdings by chain and value in flight between chains, on the bridge page</SeeAlso>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Pending capital
 * ------------------------------------------------------------------------ */

export function PendingCapitalPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const p = props.portfolio;
  const rows = pendingCapital(p.balances, p.positions.prediction_markets.markets);
  return (
    <Panel
      title="Pending capital"
      description="What this account has but cannot use freely yet: reservations by purpose, withdrawal holds, value awaiting settlement or in flight between chains, and payouts a prediction market holds for it. Each row is one figure the backend reported; none is added to another."
    >
      {rows.length === 0 ? (
        <EmptyState
          title="Nothing pending"
          body="The backend reports no reservation, hold, unsettled or in-flight balance, and no unclaimed held payout, for this account."
        />
      ) : (
        <ScrollTable caption="Capital not yet free to use" headers={["Asset", "What", "Quantity", "Where"]}>
          {rows.map((row) => (
            <tr key={row.key} data-pending={row.kind}>
              <th scope="row">
                {row.symbol}
                {row.chain !== "" && <span className="mono-small"> on {row.chain}</span>}
              </th>
              <td>
                {row.label}
                {row.code !== undefined && <code className="mono-small"> {row.code}</code>}
              </td>
              <td>
                <Qty value={row.quantity} decimals={row.decimals} symbol={row.symbol} />
              </td>
              <td>
                <Link to={routeHref(row.route)}>Open the page it is on</Link>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Margin
 * ------------------------------------------------------------------------ */

export function MarginPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const p = props.portfolio;
  const section = p.positions.margin;
  const account = section.account;
  return (
    <Panel
      title="Margin account"
      description="As the margin engine evaluates it at the instant of the read: the same answer as the margin page."
      actions={<StatePill state={section.state} />}
    >
      {account === undefined ? (
        <SectionAbsent section={section} subsystem="Margin account">
          {section.state === "NONE" && <SeeAlso route={["margin"]}>Open a margin account on the margin page</SeeAlso>}
        </SectionAbsent>
      ) : (
        <>
          <FieldGrid columns={4}>
            <Field label="Status" note="The stored status; the health beside it is the evaluation now.">
              <Pill tone={accountStatusTone(account.status)}>{account.status}</Pill>
            </Field>
            <Field label="Health">
              <Pill tone={accountStatusTone(account.evaluation.health)}>{account.evaluation.health}</Pill>
            </Field>
            <Field label="Equity">
              <span data-field="margin-equity">
                <Qty
                  value={account.evaluation.equity}
                  decimals={account.evaluation.numeraire_decimals}
                  {...(account.numeraire_asset_id === p.quote_asset_id ? { symbol: p.quote_symbol } : {})}
                />
              </span>
            </Field>
            <Field label="Maintenance margin">
              <span data-field="margin-maintenance">
                <Qty value={account.evaluation.maintenance_margin} decimals={account.evaluation.numeraire_decimals} />
              </span>
            </Field>
            <Field label="Initial margin">
              <Qty value={account.evaluation.initial_margin} decimals={account.evaluation.numeraire_decimals} />
            </Field>
            <Field label="Borrowed">
              <Qty value={account.evaluation.borrow_liabilities} decimals={account.evaluation.numeraire_decimals} />
            </Field>
            <Field label="Effective leverage" note={`Cap ${formatLeverageBps(account.evaluation.leverage_cap_bps)}.`}>
              <span data-field="margin-leverage">
                {account.evaluation.leverage_unbounded
                  ? "unbounded: equity is not positive"
                  : formatLeverageBps(account.evaluation.effective_leverage_bps)}
              </span>
            </Field>
            <Field label="Mode">
              {account.margin_mode} · {account.mode}
            </Field>
          </FieldGrid>
          {account.open_call !== undefined && (
            <p className="banner banner-warn" role="status">
              A margin call is open on this account.
            </p>
          )}
          <SeeAlso route={["margin"]}>Collateral, loans, calls and cases on the margin page</SeeAlso>
        </>
      )}
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Prediction markets
 * ------------------------------------------------------------------------ */

function PredictionMarketHolding(props: { readonly holding: PortfolioPredictionHolding }): ReactNode {
  const { market, positions } = props.holding;
  const held = positions.held_payout;
  return (
    <div className="callout" data-market={market.id}>
      <h3>
        <Link to={routeHref(["prediction-markets", market.id])}>{market.symbol}</Link>{" "}
        <Pill tone={marketStatusTone(market.status)}>{market.status}</Pill>
      </h3>
      <p className="cell-prose">{market.question}</p>
      {positions.items.length === 0 ? (
        <p className="absent">No outcome shares held now.</p>
      ) : (
        <ScrollTable caption={`Shares held in ${market.symbol}`} headers={["Outcome", "Shares", "As of"]}>
          {positions.items.map((item) => (
            <tr key={item.outcome_index}>
              <th scope="row">{item.outcome_symbol}</th>
              <td>
                <Qty value={item.quantity} decimals={positions.share_decimals} />
              </td>
              <td>
                <time dateTime={item.as_of}>{formatInstant(item.as_of)}</time>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      {held !== undefined && (
        <p className="meta-row">
          {held.claimed ? "Held payout, claimed:" : "Held payout, not yet claimed:"}{" "}
          <Qty value={held.amount} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />{" "}
          <Codes codes={held.reason_codes} />
        </p>
      )}
    </div>
  );
}

export function PredictionMarketsPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const section = props.portfolio.positions.prediction_markets;
  return (
    <Panel
      title="Prediction market shares"
      description="Each market this account holds outcome shares in, or is owed a held payout by, with its shares as that market's positions read answers them."
      actions={<StatePill state={section.state} />}
    >
      {section.state !== "PRESENT" ? (
        <SectionAbsent section={section} subsystem="Prediction markets" />
      ) : section.markets.length === 0 ? (
        <EmptyState title="No prediction market shares" body="The account holds no outcome shares and is owed no held payout." />
      ) : (
        <>
          {section.markets.map((holding) => (
            <PredictionMarketHolding key={holding.market.id} holding={holding} />
          ))}
          {section.truncated && <p className="note">More markets exist than are listed here.</p>}
        </>
      )}
      <SeeAlso route={["prediction-markets"]}>Every market on the prediction markets page</SeeAlso>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Equities
 * ------------------------------------------------------------------------ */

export function EquitiesPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const section = props.portfolio.positions.equities;
  const pending = section.pending_settlements?.items ?? [];
  return (
    <Panel
      title="Equities"
      description="The listings whose shares this account holds, settled or awaiting settlement, and its pending settlements, as the equities pages answer them."
      actions={<StatePill state={section.state} />}
    >
      {section.state !== "PRESENT" ? (
        <SectionAbsent section={section} subsystem="Equities" />
      ) : (
        <>
          {section.holdings.length === 0 ? (
            <EmptyState title="No equity holdings" body="The account holds no shares of a listing, settled or unsettled." />
          ) : (
            <ScrollTable caption="Equity holdings" headers={["Listing", "Settled", "Awaiting settlement", "Mark"]}>
              {section.holdings.map((h) => (
                <tr key={h.listing.instrument_id}>
                  <th scope="row">
                    <Link to={routeHref(["equities", "listings", h.listing.instrument_id])}>{h.listing.symbol}</Link>
                  </th>
                  <td>
                    <Qty value={h.settled} decimals={h.listing.share_decimals} symbol={h.listing.share_symbol} />
                  </td>
                  <td>
                    <Qty value={h.unsettled} decimals={h.listing.share_decimals} symbol={h.listing.share_symbol} />
                  </td>
                  <td>
                    {h.listing.mark === undefined ? (
                      <span className="absent">no fresh mark</span>
                    ) : (
                      <>
                        <DecimalValue value={h.listing.mark.price} /> {h.listing.settlement_symbol}
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </ScrollTable>
          )}
          {section.listings_truncated && (
            <p className="note">The listings read returned its maximum; a holding of a later listing is among the balances only.</p>
          )}
          <h3>Pending settlements</h3>
          {pending.length === 0 ? (
            <EmptyState title="Nothing awaiting settlement" body="No fill of this account is waiting for its settlement date." />
          ) : (
            <ScrollTable caption="Pending equity settlements" headers={["Delivers", "Quantity", "Trade date", "Due"]}>
              {pending.map((s) => (
                <tr key={s.id}>
                  <th scope="row">{s.asset_symbol}</th>
                  <td>
                    <Qty value={s.quantity} decimals={s.asset_decimals} symbol={s.asset_symbol} />
                  </td>
                  <td>{s.trade_date}</td>
                  <td>{s.due_date}</td>
                </tr>
              ))}
            </ScrollTable>
          )}
        </>
      )}
      <SeeAlso route={["equities", "settlements"]}>Every settlement on the equities page</SeeAlso>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Orders on the internal books
 * ------------------------------------------------------------------------ */

export function OrderBooksPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const section = props.portfolio.positions.order_books;
  const page = section.orders;
  return (
    <Panel
      title="Resting orders"
      description="The account's working orders on the internal books, as the order books page answers them."
      actions={<StatePill state={section.state} />}
    >
      {page === undefined ? (
        <SectionAbsent section={section} subsystem="Order books" />
      ) : page.items.length === 0 ? (
        <EmptyState title="No working orders" body="No order of this account is pending, open or partly filled on an internal book." />
      ) : (
        <>
          <ScrollTable caption="Working orders" headers={["Order", "Side", "Type", "Price", "Quantity", "Remaining", "Status"]}>
            {page.items.map((o) => (
              <tr key={o.id} data-order={o.id}>
                <th scope="row">
                  <Link to={routeHref(["order-books", "orders", o.id])}>{o.base_symbol ?? "order"}</Link>
                </th>
                <td>{o.side}</td>
                <td>{o.order_type}</td>
                <td>
                  <DecimalValue value={o.price} /> {o.quote_symbol}
                </td>
                <td>
                  <Qty value={o.quantity} decimals={o.base_decimals} />
                </td>
                <td>
                  <Qty value={o.remaining_quantity} decimals={o.base_decimals} />
                </td>
                <td>
                  <Pill>{o.status}</Pill>
                </td>
              </tr>
            ))}
          </ScrollTable>
          {page.next_cursor !== null && <p className="note">More working orders exist than are listed here.</p>}
        </>
      )}
      <SeeAlso route={["order-books", "orders"]}>Every order on the order books page</SeeAlso>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Risk
 * ------------------------------------------------------------------------ */

export function RiskPanel(props: { readonly portfolio: Portfolio }): ReactNode {
  const risk = props.portfolio.risk;
  const decisions = risk.decisions;
  return (
    <Panel
      title="Risk"
      description="What stands in the way of new risk on this account -- unresolved reconciliation records and economic effects whose outcome is unknown -- and the newest Risk Kernel V2 decisions about its intents."
    >
      <FieldGrid columns={3}>
        <Field label="Account status">
          <Pill tone={risk.account_status === "ACTIVE" ? "good" : "bad"}>{risk.account_status}</Pill>
        </Field>
        <Field label="New risk" note="Reconciliation's own answer for this account.">
          <span data-field="blocks-new-risk">
            {risk.blocks_new_risk ? (
              <Pill tone="bad">blocked by an unresolved reconciliation record</Pill>
            ) : (
              <Pill tone="good">not blocked by reconciliation</Pill>
            )}
          </span>
        </Field>
      </FieldGrid>

      <h3>Unresolved reconciliation records</h3>
      {risk.reconciliation.length === 0 ? (
        <EmptyState title="None unresolved" body="No reconciliation record on this account is unresolved." />
      ) : (
        <ScrollTable caption="Unresolved reconciliation records" headers={["Kind", "Status", "Material", "Blocks new risk", "Opened"]}>
          {risk.reconciliation.map((r) => (
            <tr key={r.id} data-record={r.id}>
              <th scope="row">
                <code className="mono-small">{r.kind}</code>
              </th>
              <td>{r.status}</td>
              <td>{r.material ? "yes" : "no"}</td>
              <td>{r.blocks_new_risk ? "yes" : "no"}</td>
              <td>
                <time dateTime={r.opened_at}>{formatInstant(r.opened_at)}</time>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      {risk.reconciliation_truncated && <p className="note">More records exist than are listed here.</p>}

      <h3>Effects of unknown outcome</h3>
      {risk.unknown_effects.length === 0 ? (
        <EmptyState title="None unknown" body="No economic effect on this account has an unknown outcome." />
      ) : (
        <ScrollTable caption="Economic effects whose outcome is unknown" headers={["Subsystem", "Effect", "External reference", "Since"]}>
          {risk.unknown_effects.map((e) => (
            <tr key={e.id} data-effect={e.id}>
              <th scope="row">{e.subsystem}</th>
              <td>
                <code className="mono-small">{e.effect_kind}</code>
              </td>
              <td>{e.external_ref === undefined ? <span className="absent">none</span> : <Identifier value={e.external_ref} />}</td>
              <td>
                <time dateTime={e.updated_at}>{formatInstant(e.updated_at)}</time>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      {risk.unknown_effects_truncated && <p className="note">More effects exist than are listed here.</p>}

      <h3>
        Newest Risk Kernel V2 decisions <StatePill state={decisions.state} />
      </h3>
      {decisions.state !== "PRESENT" ? (
        <SectionAbsent section={decisions} subsystem="Risk decisions" />
      ) : decisions.items.length === 0 ? (
        <EmptyState title="No decisions" body="Risk Kernel V2 has decided nothing about this account's intents yet." />
      ) : (
        <ScrollTable caption="Newest Risk Kernel V2 decisions" headers={["Verdict", "Intent", "Stage", "Reasons", "Evaluated"]}>
          {decisions.items.map((d) => (
            <tr key={d.id}>
              <th scope="row">
                <Pill tone={verdictTone(d.verdict)}>{d.verdict}</Pill>
              </th>
              <td>
                <code className="mono-small">{d.intent_kind}</code>
              </td>
              <td>{d.stage}</td>
              <td>
                <Codes codes={d.reason_codes} none="none" />
              </td>
              <td>
                <time dateTime={d.evaluated_at} title={exactInstant(d.evaluated_at)}>
                  {formatInstant(d.evaluated_at)}
                </time>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      <SeeAlso route={["activity"]}>Every intent, effect and decision on the economic activity page</SeeAlso>
    </Panel>
  );
}
