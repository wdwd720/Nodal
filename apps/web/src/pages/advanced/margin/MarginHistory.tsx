/**
 * MARGIN — calls and cases: every margin call and every liquidation case the
 * backend has recorded for the account, newest first, and one case with
 * every deterministic step it executed.
 *
 * A case read by its id is answered by the backend for the case's own
 * account: a case the session may not read is NOT_FOUND, exactly as one that
 * does not exist, and the page shows that answer. An operator can have the
 * insurance fund cover a bankrupt case's deficit; whether that may happen is
 * the backend's decision.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useCoverDeficit,
  useMarginCalls,
  useMarginCase,
  useMarginCases,
  type CoverRequest,
  type MarginCall,
  type MarginCase,
  type MarginCaseDetail,
  type MarginDeficitResult,
} from "../../../api/advanced/margin.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { useAssets } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { CASE_STATUS_MEANING, assetById, callStatusTone, caseStatusTone } from "../../../lib/margin.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { CaseSummary, CommandFlow, MarginNav, StepsTable, caseHref } from "./parts.tsx";

export function MarginHistoryPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  return (
    <Page
      title="Margin calls and liquidation cases"
      lead="Every margin call and liquidation case the backend has recorded for this account, newest first."
    >
      <SimulatedNotice />
      <MarginNav />
      {accountId === undefined ? (
        <Panel title="Margin calls">
          <NoAccount />
        </Panel>
      ) : (
        <>
          <CallsPanel key={`calls-${accountId}`} accountId={accountId} />
          <CasesPanel key={`cases-${accountId}`} accountId={accountId} />
        </>
      )}
    </Page>
  );
}

function useUnit(numeraire: string): string {
  const assets = useAssets();
  return assetById(assets.data ?? [], numeraire)?.symbol ?? "numeraire units";
}

function CallsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const calls = useMarginCalls(props.accountId, trail.current);
  return (
    <Panel
      title="Margin calls"
      description="A call is issued when equity falls below maintenance, and resolved when it is met or a liquidation case takes it over."
    >
      <AsyncPanel
        query={calls}
        loadingLabel="Loading margin calls…"
        empty={{
          isEmpty: (page: CursorPage<MarginCall>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No margin calls",
          body: "The backend has recorded no margin call for this account. That is an empty record, not an error.",
        }}
      >
        {(page: CursorPage<MarginCall>) => (
          <>
            <ScrollTable caption="Margin calls on this page" headers={["Issued", "Status", "Deficiency", "Equity", "Maintenance", "Resolved", "Call"]}>
              {page.items.map((call) => (
                <CallRow key={call.id} call={call} />
              ))}
            </ScrollTable>
            <CursorPager
              noun="calls"
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

export function CallRow(props: { readonly call: MarginCall; readonly showAccount?: boolean }): ReactNode {
  const { call } = props;
  const unit = useUnit(call.numeraire_asset_id);
  return (
    <tr>
      <td>{formatInstant(call.issued_at)}</td>
      <th scope="row">
        <Pill tone={callStatusTone(call.status)}>{call.status}</Pill>
        {props.showAccount === true && (
          <span className="cell-note">
            account <code className="mono-small">{call.account_id}</code>
          </span>
        )}
      </th>
      <td>
        <Qty value={call.deficiency} decimals={call.numeraire_decimals} symbol={unit} />
      </td>
      <td>
        <Qty value={call.equity} decimals={call.numeraire_decimals} symbol={unit} />
      </td>
      <td>
        <Qty value={call.maintenance_margin} decimals={call.numeraire_decimals} symbol={unit} />
      </td>
      <td>{call.resolved_at === undefined ? <span className="absent">open</span> : formatInstant(call.resolved_at)}</td>
      <td>
        <code className="mono-small">{call.id}</code>
      </td>
    </tr>
  );
}

function CasesPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const cases = useMarginCases(props.accountId, trail.current);
  return (
    <Panel
      title="Liquidation cases"
      description="A case runs one deterministic step at a time until the account is restored, bankrupt, or handed to a person."
    >
      <AsyncPanel
        query={cases}
        loadingLabel="Loading liquidation cases…"
        empty={{
          isEmpty: (page: CursorPage<MarginCase>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No liquidation cases",
          body: "The backend has opened no liquidation case for this account.",
        }}
      >
        {(page: CursorPage<MarginCase>) => (
          <>
            <ScrollTable caption="Liquidation cases on this page" headers={["Opened", "Status", "Steps", "Last reason", "Case"]}>
              {page.items.map((kase) => (
                <tr key={kase.id}>
                  <td>{formatInstant(kase.opened_at)}</td>
                  <th scope="row">
                    <Pill tone={caseStatusTone(kase.status)} title={CASE_STATUS_MEANING[kase.status]}>
                      {kase.status}
                    </Pill>
                  </th>
                  <td>
                    {String(kase.steps_executed)} of {String(kase.max_steps)}
                  </td>
                  <td className="cell-prose">{kase.last_reason}</td>
                  <td>
                    <Link to={caseHref(kase.id)}>
                      <code className="mono-small">{kase.id}</code>
                    </Link>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="cases"
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

/** One liquidation case and every step it executed. */
export function MarginCasePage(): ReactNode {
  const { caseId } = useParams();
  const account = useAdvancedAccount();
  const kase = useMarginCase(account.accountId, caseId);
  return (
    <Page title="Liquidation case" lead="One liquidation case, as the backend recorded it, with every step it executed.">
      <SimulatedNotice />
      <MarginNav />
      <Panel title="Liquidation case" {...(caseId === undefined ? {} : { description: `Case ${caseId}` })}>
        <AsyncPanel query={kase} loadingLabel="Loading the liquidation case…">
          {(detail: MarginCaseDetail) => <CaseDetail detail={detail} canOperate={account.canInspect} />}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function CaseDetail(props: { readonly detail: MarginCaseDetail; readonly canOperate: boolean }): ReactNode {
  const { detail } = props;
  const assets = useAssets();
  const unit = useUnit(detail.numeraire_asset_id);
  const coverable = detail.status === "BANKRUPT" && detail.deficit_covered_at === undefined;
  return (
    <>
      <CaseSummary kase={detail} unit={unit} />
      <FieldGrid columns={3}>
        <Field label="Account">
          <Identifier value={detail.account_id} />
        </Field>
        <Field label="Margin call" note="The call this case took over when it opened.">
          <Identifier value={detail.margin_call_id} />
        </Field>
        <Field label="Opened" note={`Policy ${detail.policy_version}`}>
          {formatInstant(detail.opened_at)}
        </Field>
      </FieldGrid>
      <h3>Steps</h3>
      {detail.steps.length === 0 ? (
        <p className="note">The case has executed no step: it is waiting, or it ended without selling anything.</p>
      ) : (
        <StepsTable steps={detail.steps} decimals={detail.numeraire_decimals} unit={unit} assets={assets.data ?? []} />
      )}
      {coverable && props.canOperate && <CoverDeficit detail={detail} unit={unit} />}
    </>
  );
}

function CoverDeficit(props: { readonly detail: MarginCaseDetail; readonly unit: string }): ReactNode {
  const cover = useCoverDeficit();
  const { detail } = props;
  return (
    <>
      <h3>Cover the deficit</h3>
      <p className="note">
        The insurance fund pays the lending pool the deficit and holds the claim; the account keeps owing it. This
        moves simulated value, so the backend asks for a recent strong sign-in.
      </p>
      <CommandFlow<CoverRequest, MarginDeficitResult>
        label="Cover the deficit from the insurance fund"
        variant="danger"
        prepared={{ ok: true, request: { accountId: detail.account_id, caseId: detail.id } }}
        command={cover}
        refusedNote="The deficit was not covered and nothing moved."
        describe={(request) => (
          <Field label="Case">
            <code className="mono-small">{request.caseId}</code>
            {detail.deficit !== undefined && (
              <span className="cell-note">
                deficit <Qty value={detail.deficit} decimals={detail.numeraire_decimals} symbol={props.unit} />
              </span>
            )}
          </Field>
        )}
        renderResult={(result) => (
          <p className="note" role="status">
            {result.replayed ? "Already covered; nothing moved again. " : "Covered. "}
            Deficit <Qty value={result.deficit} decimals={detail.numeraire_decimals} symbol={props.unit} />, journal{" "}
            <code className="mono-small">{result.journal_transaction_id}</code>.
          </p>
        )}
      />
    </>
  );
}
