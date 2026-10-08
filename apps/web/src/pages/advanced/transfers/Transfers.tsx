/**
 * TRANSFERS: simulated value sent between customers' accounts, and the
 * sandbox faucet that grants it (ADR-0020; internal/transfers and
 * internal/sandboxfund behind /v1/advanced/transfers).
 *
 * What the backend decides, and this section only shows: whether a transfer
 * may be sent (the P2P_TRANSFERS gate, step-up above the policy's threshold,
 * per-transfer, daily and velocity limits, the sender's recipient rules, Risk
 * Kernel V2, the compliance screen, the sender's available balance), what a
 * reader may see of one, and how much the faucet would still grant.
 *
 * What a reader sees is the backend's choice too, and the page draws exactly
 * what came back: a sender who paid a handle is not told the account behind
 * it; a recipient sees a transfer once it moved value, without the sender's
 * decision record, and never the sender's refused attempts. Rows are never
 * filtered or re-derived here.
 *
 * Every command acts on the session's own account. An operator inspecting
 * another account reads it, and is not offered a form that would act on it.
 */
import { useState, type ReactNode } from "react";
import { Link, Route, Routes } from "react-router-dom";

import { type CursorPage } from "../../../api/advanced/core.ts";
import { useFaucetAllowance, useTransfers, type AdvancedTransfer } from "../../../api/advanced/transfers.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill, Table } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, formatMillis, type CursorTrail } from "../../../lib/advanced.ts";
import { formatInstant } from "../../../lib/time.ts";
import {
  TRANSFER_STATUSES,
  TRANSFER_STATUS_MEANING,
  transferSide,
  transferStatusTone,
} from "../../../lib/transfers.ts";
import { useAdvancedAccount } from "../area.ts";
import { AdvancedNotFound, Codes, CursorPager, ModePill, NoAccount, ScrollTable, ValueDomainPill } from "../shared.tsx";
import { FaucetForm, HandleForm, SendForm, transferHref } from "./forms.tsx";
import { TransferDetail } from "./TransferDetail.tsx";

/** The section: the account's transfers and the faucet, and one transfer by id. */
export function TransfersSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<Transfers />} />
      <Route path=":transferId" element={<TransferDetail />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}

function Transfers(): ReactNode {
  const account = useAdvancedAccount();
  const { accountId } = account;
  // Commands act on the session's own account only; while an operator
  // inspects another account, nothing on this page could act on it.
  const ownAccount = accountId !== undefined && !account.inspecting ? accountId : undefined;
  const allowance = useFaucetAllowance(accountId);
  const symbol = allowance.data?.asset_symbol;

  return (
    <Page
      title="Transfers"
      lead="Simulated value sent between customers, and sandbox dollars from the faucet. The backend decides every transfer; this page shows its answers."
    >
      <SimulatedNotice />
      {accountId === undefined ? (
        <Panel title="Transfers">
          <NoAccount />
        </Panel>
      ) : (
        <>
          <Panel
            title="Sandbox faucet"
            description="Sandbox dollars granted into your own account, within the faucet policy's rolling cap. A grant is a posting of simulated value, not a deposit."
          >
            <AsyncPanel query={allowance} loadingLabel="Asking the backend what the faucet would still grant…">
              {(found) => (
                <>
                  <FieldGrid columns={3}>
                    <Field
                      label="Remaining in this window"
                      note="What the faucet would still grant this account now: the cap less what it granted in the window."
                      emphasis
                    >
                      <span data-field="faucet-remaining">
                        <Qty value={found.remaining} decimals={found.asset_decimals} symbol={found.asset_symbol} />
                      </span>
                      <span className="cell-note">
                        <BaseUnits value={found.remaining} />
                      </span>
                    </Field>
                    <Field label="Granted in this window">
                      <span data-field="faucet-used">
                        <Qty value={found.used} decimals={found.asset_decimals} symbol={found.asset_symbol} />
                      </span>
                    </Field>
                    <Field label="Cap per window" note={`A rolling window of ${formatMillis(found.window_ms)}.`}>
                      <span data-field="faucet-cap">
                        <Qty value={found.cap} decimals={found.asset_decimals} symbol={found.asset_symbol} />
                      </span>
                    </Field>
                  </FieldGrid>
                  <p className="meta-row">
                    <ValueDomainPill value={found.value_domain} />
                    <span className="mono-small">
                      policy {found.policy_version} · environment {found.environment}
                    </span>
                  </p>
                  {ownAccount === undefined ? (
                    <p className="note">
                      You are inspecting another account. The faucet grants only into the session's own account, so
                      no request form is offered here.
                    </p>
                  ) : (
                    <FaucetForm accountId={ownAccount} symbol={found.asset_symbol} />
                  )}
                </>
              )}
            </AsyncPanel>
          </Panel>

          {ownAccount === undefined ? (
            <Panel title="Send">
              <p className="note">
                You are inspecting another account. A transfer is only ever sent from an account the session owns, so
                no send form is offered while inspecting.
              </p>
            </Panel>
          ) : (
            <>
              <Panel
                title="Send"
                description="Send simulated value from your account to another customer. You review the transfer, then confirm it; the backend decides whether it moves."
              >
                <SendForm key={`send-${ownAccount}`} accountId={ownAccount} symbol={symbol} />
              </Panel>
              <Panel
                title="Your handle"
                description="The handle other customers pay this account by. The backend resolves a handle itself and never tells a sender the account behind it."
              >
                <HandleForm key={`handle-${ownAccount}`} accountId={ownAccount} />
              </Panel>
            </>
          )}

          {/* Keyed by account: switching account starts the history again at its first page. */}
          <HistoryPanel key={`history-${accountId}`} accountId={accountId} />
          <StatusGlossary />
        </>
      )}
    </Page>
  );
}

function HistoryPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const transfers = useTransfers(props.accountId, trail.current);
  return (
    <Panel
      title="Transfer history"
      description="Every transfer this account sent or received, newest first, as the backend shows them to you. A refused attempt is listed for its sender only."
    >
      <AsyncPanel
        query={transfers}
        loadingLabel="Loading the account's transfers…"
        empty={{
          isEmpty: (page: CursorPage<AdvancedTransfer>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No transfers",
          body: "The backend has no transfer this account sent or received that it may show you. That is an empty history, not an error.",
        }}
      >
        {(page: CursorPage<AdvancedTransfer>) => (
          <>
            {page.items.length === 0 ? (
              <p className="note">
                Nothing on this page that the backend may show you. It said there may be more, so the next page is
                offered below.
              </p>
            ) : (
              <ScrollTable
                caption="Transfers on this page"
                headers={["Recorded", "Direction", "Counterparty", "Amount", "Status", "Mode", "Memo", "Transfer"]}
              >
                {page.items.map((transfer) => (
                  <HistoryRow key={transfer.id} transfer={transfer} accountId={props.accountId} />
                ))}
              </ScrollTable>
            )}
            <CursorPager
              noun="transfers"
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

function HistoryRow(props: { readonly transfer: AdvancedTransfer; readonly accountId: string }): ReactNode {
  const { transfer } = props;
  const side = transferSide(transfer, props.accountId);
  return (
    <tr data-transfer-id={transfer.id}>
      <td>{formatInstant(transfer.created_at)}</td>
      <th scope="row">{side === "sent" ? "Sent" : side === "received" ? "Received" : "Not this account's side"}</th>
      <td>
        <Counterparty transfer={transfer} side={side} />
      </td>
      <td>
        <Qty value={transfer.quantity} decimals={transfer.asset_decimals} symbol={transfer.asset_symbol} />
      </td>
      <td>
        <Pill tone={transferStatusTone(transfer.status)}>{transfer.status}</Pill>
        {transfer.rejection_code !== undefined && <Codes codes={[transfer.rejection_code]} />}
      </td>
      <td>
        <ModePill mode={transfer.mode} />
      </td>
      <td className="cell-prose">{transfer.memo ?? <span className="absent">none</span>}</td>
      <td>
        <Link to={transferHref(transfer.id)}>
          <code className="mono-small">{transfer.id}</code>
        </Link>
      </td>
    </tr>
  );
}

/** The other party, as the backend shows it to this reader. */
function Counterparty(props: { readonly transfer: AdvancedTransfer; readonly side: string }): ReactNode {
  const { transfer } = props;
  if (props.side === "received") {
    return <Identifier label="from" value={transfer.sender_account_id} />;
  }
  return (
    <>
      {transfer.recipient_handle !== undefined && <code className="mono-small">@{transfer.recipient_handle}</code>}
      {transfer.recipient_account_id !== undefined ? (
        <span className="cell-note">
          <Identifier label="to" value={transfer.recipient_account_id} />
        </span>
      ) : (
        <span className="cell-note">paid by handle: the account behind it is not shown to the sender</span>
      )}
      {props.side === "neither" && (
        <span className="cell-note">
          <Identifier label="from" value={transfer.sender_account_id} />
        </span>
      )}
    </>
  );
}

function StatusGlossary(): ReactNode {
  return (
    <Panel title="What the statuses mean" description="A closed set the database enforces, with every move between them recorded.">
      <Table caption="Transfer statuses and their meaning" headers={["Transfer status", "What it means"]}>
        {TRANSFER_STATUSES.map((status) => (
          <tr key={status}>
            <th scope="row">
              <Pill tone={transferStatusTone(status)}>{status}</Pill>
            </th>
            <td className="cell-prose">{TRANSFER_STATUS_MEANING[status]}</td>
          </tr>
        ))}
      </Table>
    </Panel>
  );
}
