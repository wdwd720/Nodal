/**
 * CORPORATE ACTIONS: every split, reverse split, dividend, symbol change and
 * delisting announced, newest effective date first, and what each applied
 * action posted to the account the area reads; and ONE ACTION, with the
 * operator's withdraw and apply.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useCorporateAction,
  useCorporateActions,
  useEquityApplications,
  type EquityApplication,
  type EquityCorporateAction,
} from "../../../api/advanced/equities.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { DecimalValue } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { ACTION_STATUS_MEANING, ACTION_TYPE_LABEL, ROUNDING_LABEL } from "../../../lib/equities.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { AnnounceActionForm, ApplyActionForm, CancelActionForm, OperatorOnly } from "./forms.tsx";
import { EquitiesNav, Recorded, Units, equitiesHref } from "./parts.tsx";

/** What an action does, in its own terms. */
function ActionTerms(props: { readonly action: EquityCorporateAction }): ReactNode {
  const { action } = props;
  switch (action.type) {
    case "SPLIT":
    case "REVERSE_SPLIT":
      return (
        <>
          {action.ratio === undefined
            ? "ratio not reported"
            : `${String(action.ratio.numerator)} new for ${String(action.ratio.denominator)} old`}
          {action.in_lieu_price !== undefined && (
            <span className="cell-note">
              fractions paid at <DecimalValue value={action.in_lieu_price} /> per whole new share
            </span>
          )}
        </>
      );
    case "CASH_DIVIDEND":
      return (
        <>
          <DecimalValue value={action.dividend_per_share} /> per share
          <span className="cell-note">
            ex {action.ex_date ?? "?"} · record {action.record_date ?? "?"} · pay {action.pay_date ?? "?"}
          </span>
        </>
      );
    case "SYMBOL_CHANGE":
      return <>to {action.new_symbol ?? "a symbol not reported"}</>;
    case "DELISTING":
      return <>final {action.final_date ?? "date not reported"}</>;
  }
}

/** A table of actions, as the backend listed them. */
export function ActionRows(props: { readonly actions: readonly EquityCorporateAction[] }): ReactNode {
  return (
    <ScrollTable caption="Corporate actions" headers={["Action", "Status", "Effective", "Terms", "Announced"]}>
      {props.actions.map((action) => (
        <tr key={action.id}>
          <th scope="row">
            <Link to={equitiesHref("corporate-actions", action.id)}>{ACTION_TYPE_LABEL[action.type]}</Link>
            <span className="cell-note mono-small">{action.type}</span>
          </th>
          <td>
            <Pill tone={action.status === "APPLIED" ? "good" : action.status === "CANCELLED" ? "neutral" : "info"}>{action.status}</Pill>
          </td>
          <td>{action.effective_date}</td>
          <td>
            <ActionTerms action={action} />
          </td>
          <td>
            <Recorded at={action.created_at} />
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

export function CorporateActionsPage(): ReactNode {
  const { accountId, canInspect } = useAdvancedAccount();
  return (
    <Page title="Corporate actions" lead="Splits, reverse splits, dividends, symbol changes and delistings, and what they posted to the account.">
      <SimulatedNotice />
      <EquitiesNav />
      <Panel title="Every action" description="Newest effective date first, across every listing.">
        <AllActions />
      </Panel>
      <Panel
        title="Postings to this account"
        description="What each applied action posted to the account the area reads: the shares it was entitled on, the shares it gained or lost, and any amount paid, in the listing's settlement asset."
      >
        {accountId === undefined ? <NoAccount /> : <Applications key={accountId} accountId={accountId} />}
      </Panel>
      {canInspect && (
        <Panel title="Announce a corporate action" description="Recorded as ANNOUNCED; nothing is posted until it is applied.">
          <OperatorOnly>
            <AnnounceActionForm instrumentId={undefined} />
          </OperatorOnly>
        </Panel>
      )}
    </Page>
  );
}

function AllActions(): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const actions = useCorporateActions(undefined, trail.current);
  return (
    <AsyncPanel
      query={actions}
      loadingLabel="Loading corporate actions…"
      empty={{
        isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
        title: "No corporate actions",
        body: "The backend lists no corporate action in this deployment.",
      }}
    >
      {(page) => (
        <>
          <ActionRows actions={page.items} />
          <CursorPager
            noun="actions"
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
  );
}

function Applications(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const applications = useEquityApplications(props.accountId, trail.current);
  return (
    <AsyncPanel
      query={applications}
      loadingLabel="Loading postings…"
      empty={{
        isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
        title: "No postings",
        body: "The backend lists no corporate-action posting for this account.",
      }}
    >
      {(page) => (
        <>
          <ScrollTable caption="Corporate-action postings" headers={["Action", "Entitled", "Shares", "Paid", "Exact amount", "Posted"]}>
            {page.items.map((app) => (
              <ApplicationRow key={app.id} app={app} />
            ))}
          </ScrollTable>
          <CursorPager
            noun="postings"
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
  );
}

function ApplicationRow(props: { readonly app: EquityApplication }): ReactNode {
  const { app } = props;
  return (
    <tr>
      <th scope="row">
        <Link to={equitiesHref("corporate-actions", app.action_id)}>
          {app.action_type === undefined ? "Action" : ACTION_TYPE_LABEL[app.action_type]}
        </Link>
        <span className="cell-note">{ROUNDING_LABEL[app.rounding]}</span>
      </th>
      <td>
        <Units value={app.entitled_quantity} decimals={app.share_decimals} symbol={app.share_symbol} />
      </td>
      <td>
        <Units value={app.shares_delta} decimals={app.share_decimals} symbol={app.share_symbol} />
      </td>
      <td>
        <Units value={app.settlement_amount} decimals={app.settlement_decimals} symbol={app.settlement_symbol} />
      </td>
      <td>
        <span className="mono-small">
          {app.exact_amount_numerator}/{app.exact_amount_denominator}
        </span>
        <span className="cell-note">base units, before rounding</span>
      </td>
      <td>
        <Recorded at={app.created_at} />
      </td>
    </tr>
  );
}

export function CorporateActionDetailPage(): ReactNode {
  const { actionId } = useParams();
  const action = useCorporateAction(actionId);
  return (
    <Page title="Corporate action" lead="One corporate action, as the backend recorded it.">
      <SimulatedNotice />
      <EquitiesNav />
      <AsyncPanel query={action} loadingLabel="Loading the action…">
        {(a) => <ActionPanels action={a} />}
      </AsyncPanel>
    </Page>
  );
}

function ActionPanels(props: { readonly action: EquityCorporateAction }): ReactNode {
  const { action } = props;
  const { canInspect } = useAdvancedAccount();
  return (
    <>
      <Panel title={ACTION_TYPE_LABEL[action.type]} description={ACTION_STATUS_MEANING[action.status]}>
        <FieldGrid columns={3}>
          <Field label="Status">
            <Pill tone={action.status === "APPLIED" ? "good" : "info"}>{action.status}</Pill>
            {action.applied_at !== undefined && (
              <span className="cell-note">
                applied <Recorded at={action.applied_at} />
              </span>
            )}
          </Field>
          <Field label="Listing">
            <Link to={equitiesHref("listings", action.instrument_id)}>
              <code className="mono-small">{action.instrument_id}</code>
            </Link>
          </Field>
          <Field label="Effective">{action.effective_date}</Field>
          <Field label="Terms">
            <ActionTerms action={action} />
          </Field>
          <Field label="Rounding">{ROUNDING_LABEL[action.rounding]}</Field>
          <Field label="Reason">{action.reason}</Field>
          <Field label="Announced by">
            {action.announced_by_actor_type} <code className="mono-small">{action.announced_by_actor_id}</code>
            <span className="cell-note">
              <Recorded at={action.created_at} />
            </span>
          </Field>
          <Field label="Content hash">
            <Identifier value={action.content_hash} />
          </Field>
          <Field label="Assets">
            <Identifier value={action.share_asset_id} label="shares" />
            <span className="cell-note">
              <Identifier value={action.settlement_asset_id} label="settlement" />
            </span>
          </Field>
        </FieldGrid>
      </Panel>
      {canInspect && action.status === "ANNOUNCED" && (
        <Panel title="Withdraw" description="A withdrawn action frees its date; nothing was posted for it.">
          <OperatorOnly>
            <CancelActionForm action={action} />
          </OperatorOnly>
        </Panel>
      )}
      {canInspect && (action.status === "ANNOUNCED" || action.status === "APPLYING") && (
        <Panel
          title="Apply"
          description="On or after the date it takes effect (a dividend on its pay date): one posting per holder, in one transaction. A split needs every order on the listing ended and nothing unsettled."
        >
          <OperatorOnly>
            <ApplyActionForm action={action} />
          </OperatorOnly>
        </Panel>
      )}
    </>
  );
}
