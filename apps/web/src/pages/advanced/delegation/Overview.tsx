/**
 * The section's first page: the account's capabilities with their scope,
 * limits, use and expiry; its agents and the grant form; the signing requests
 * its agents made and the withdrawals they proposed, with their status.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import type { CursorPage } from "../../../api/advanced/core.ts";
import {
  useDelegationAgents,
  useDelegationCapabilities,
  useDelegationSwaps,
  useDelegationWithdrawals,
  type DelegationCapability,
  type DelegationSwap,
  type DelegationWithdrawal,
} from "../../../api/advanced/delegation.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  ACTION_CLASS_LABEL,
  INSTRUMENT_CLASS_LABEL,
  STANDING_MEANING,
  SWAP_STATUSES,
  SWAP_STATUS_MEANING,
  WITHDRAWAL_STATUSES,
  WITHDRAWAL_STATUS_MEANING,
  standingTone,
  subjectStatusTone,
  type SwapStatus,
  type WithdrawalStatus,
} from "../../../lib/delegation.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { GrantForm } from "./GrantForm.tsx";
import { DelegationNav, InspectingNote, delegationHref } from "./parts.tsx";

export function Overview(): ReactNode {
  const account = useAdvancedAccount();
  const { accountId } = account;
  const ownAccount = accountId !== undefined && !account.inspecting ? accountId : undefined;
  return (
    <Page
      title="Agent authority"
      lead="What you delegated to your account's own agents, and what they asked for under it. The backend decides every request; an agent can never sign, execute, decide or read a key."
    >
      <SimulatedNotice />
      <DelegationNav />
      {accountId === undefined ? (
        <Panel title="Agent authority">
          <NoAccount />
        </Panel>
      ) : (
        <>
          <CapabilitiesPanel key={`capabilities-${accountId}`} accountId={accountId} />
          <AgentsPanel accountId={accountId} />
          <Panel
            title="Grant a capability"
            description="Give one of your agents a scoped, expiring capability: which assets and actions, which chains and destinations, and how much. It never widens itself, and only you can revoke it. A recent strong sign-in is required."
          >
            {ownAccount === undefined ? (
              <InspectingNote what="grant a capability" />
            ) : (
              <GrantForm key={`grant-${ownAccount}`} accountId={ownAccount} />
            )}
          </Panel>
          <SwapsPanel key={`swaps-${accountId}`} accountId={accountId} />
          <WithdrawalsPanel key={`withdrawals-${accountId}`} accountId={accountId} />
        </>
      )}
    </Page>
  );
}

function CapabilitiesPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const capabilities = useDelegationCapabilities(props.accountId, trail.current);
  return (
    <Panel
      title="Capabilities"
      description="Every capability granted on this account, newest first, with what it allows, its limits and how much of them has been used. A use is counted when a request is admitted, even if it is later refused."
    >
      <AsyncPanel
        query={capabilities}
        loadingLabel="Loading the account's capabilities…"
        empty={{
          isEmpty: (page: CursorPage<DelegationCapability>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No capabilities",
          body: "This account has granted no capability to any agent. That is an empty list, not an error.",
        }}
      >
        {(page: CursorPage<DelegationCapability>) => (
          <>
            <ScrollTable
              caption="Capabilities on this page"
              headers={["Agent", "Allows", "Single request", "Today used / left", "In all used / left", "Expires", "Standing", "Capability"]}
            >
              {page.items.map((capability) => (
                <tr key={capability.id} data-capability-id={capability.id}>
                  <td>
                    {capability.agent_name ?? <span className="absent">unnamed</span>}
                    <span className="cell-note mono-small">{capability.agent_id}</span>
                  </td>
                  <td>
                    {capability.action_classes.map((action) => ACTION_CLASS_LABEL[action]).join("; ")}
                    <span className="cell-note">
                      {capability.instrument_classes.map((kind) => INSTRUMENT_CLASS_LABEL[kind]).join("; ")} on{" "}
                      {capability.chain_allowlist.join(", ")}
                    </span>
                  </td>
                  <td>
                    <Usd value={capability.max_single_trade_usd} />
                  </td>
                  <td>
                    <Usd value={capability.usage.used_today_usd} /> / <Usd value={capability.usage.remaining_today_usd} />
                    <span className="cell-note">
                      of <Usd value={capability.max_daily_notional_usd} /> on {capability.usage.day} (UTC)
                    </span>
                  </td>
                  <td>
                    <Usd value={capability.usage.used_total_usd} /> / <Usd value={capability.usage.remaining_total_usd} />
                    <span className="cell-note">
                      of <Usd value={capability.max_notional_usd} />
                    </span>
                  </td>
                  <td>
                    <time dateTime={capability.expires_at}>{formatInstant(capability.expires_at)}</time>
                  </td>
                  <td>
                    <span title={STANDING_MEANING[capability.standing]} data-field="standing">
                      <Pill tone={standingTone(capability.standing)}>{capability.standing}</Pill>
                    </span>
                    {capability.requires_human_approval && <span className="cell-note">every swap needs your approval</span>}
                  </td>
                  <td>
                    <Link to={delegationHref("capabilities", capability.id)}>Open</Link>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="capabilities"
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

function AgentsPanel(props: { readonly accountId: string }): ReactNode {
  const agents = useDelegationAgents(props.accountId);
  return (
    <Panel
      title="Your agents"
      description="The account's own agents. A capability is granted to one of them, for that agent's own strategy; the backend refuses a grant to an agent that is paused, failed, revoked or superseded."
    >
      <AsyncPanel
        query={agents}
        loadingLabel="Loading the account's agents…"
        empty={{
          isEmpty: (items: readonly unknown[]) => items.length === 0,
          title: "No agents",
          body: "The backend lists no agent on this account, so there is nobody to delegate to yet.",
        }}
      >
        {(items) => (
          <ScrollTable caption="The account's agents" headers={["Name", "Agent id", "Stage", "State", "Created"]}>
            {items.map((agent) => (
              <tr key={agent.id} data-agent-id={agent.id}>
                <td>{agent.name}</td>
                <td>
                  <code className="mono-small">{agent.id}</code>
                </td>
                <td>{agent.stage}</td>
                <td>{agent.state}</td>
                <td>{formatInstant(agent.created_at)}</td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function StatusFilter<S extends string>(props: {
  readonly label: string;
  readonly statuses: readonly S[];
  readonly value: S | undefined;
  readonly onChange: (status: S | undefined) => void;
}): ReactNode {
  return (
    <div className="filter">
      <label>
        {props.label}{" "}
        <select
          className="input"
          value={props.value ?? ""}
          onChange={(event) => {
            const chosen = props.statuses.find((status) => status === event.target.value);
            props.onChange(chosen);
          }}
        >
          <option value="">every status</option>
          {props.statuses.map((status) => (
            <option key={status} value={status}>
              {status}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}

function SwapsPanel(props: { readonly accountId: string }): ReactNode {
  const [status, setStatus] = useState<SwapStatus | undefined>(undefined);
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const swaps = useDelegationSwaps(props.accountId, status, trail.current);
  return (
    <Panel
      title="Signing requests"
      description="The swaps your agents asked for, newest first. Each carries the plan it would execute; nothing is signed until the inspector proves the transaction is that plan, and a capability that needs your approval waits for you."
    >
      <StatusFilter
        label="Show"
        statuses={SWAP_STATUSES}
        value={status}
        onChange={(next) => {
          setStatus(next);
          setTrail(FIRST_PAGE);
        }}
      />
      <AsyncPanel
        query={swaps}
        loadingLabel="Loading the account's signing requests…"
        empty={{
          isEmpty: (page: CursorPage<DelegationSwap>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No signing requests",
          body: "No agent of this account asked for a swap the backend may show here. That is an empty list, not an error.",
        }}
      >
        {(page: CursorPage<DelegationSwap>) => (
          <>
            <ScrollTable
              caption="Signing requests on this page"
              headers={["Requested", "Pays", "Receives", "Valued at", "Venue", "Status", "Request"]}
            >
              {page.items.map((swap) => (
                <tr key={swap.id} data-swap-id={swap.id}>
                  <td>{formatInstant(swap.requested_at)}</td>
                  <td>
                    <Qty value={swap.amount_in} decimals={swap.pay_asset.decimals} symbol={swap.pay_asset.symbol} />
                  </td>
                  <td>
                    <Qty value={swap.amount_out} decimals={swap.receive_asset.decimals} symbol={swap.receive_asset.symbol} />
                  </td>
                  <td>
                    <Usd value={swap.notional_usd} />
                  </td>
                  <td>{swap.venue.name}</td>
                  <td title={SWAP_STATUS_MEANING[swap.status]}>
                    <Pill tone={subjectStatusTone(swap.status)}>{swap.status}</Pill>
                  </td>
                  <td>
                    <Link to={delegationHref("swaps", swap.id)}>Open</Link>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="signing requests"
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

function WithdrawalsPanel(props: { readonly accountId: string }): ReactNode {
  const [status, setStatus] = useState<WithdrawalStatus | undefined>(undefined);
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const withdrawals = useDelegationWithdrawals(props.accountId, status, trail.current);
  return (
    <Panel
      title="Withdrawals"
      description="The withdrawals your agents proposed, newest first. An agent may only propose: a withdrawal goes only to an allow-listed destination, under your policy, with your decision unless a test environment's policy approves it automatically."
    >
      <StatusFilter
        label="Show"
        statuses={WITHDRAWAL_STATUSES}
        value={status}
        onChange={(next) => {
          setStatus(next);
          setTrail(FIRST_PAGE);
        }}
      />
      <AsyncPanel
        query={withdrawals}
        loadingLabel="Loading the account's withdrawals…"
        empty={{
          isEmpty: (page: CursorPage<DelegationWithdrawal>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No withdrawals",
          body: "No agent of this account proposed a withdrawal the backend may show here. That is an empty list, not an error.",
        }}
      >
        {(page: CursorPage<DelegationWithdrawal>) => (
          <>
            <ScrollTable
              caption="Withdrawals on this page"
              headers={["Proposed", "Amount", "Valued at", "Destination", "Policy said", "Status", "Withdrawal"]}
            >
              {page.items.map((withdrawal) => (
                <tr key={withdrawal.id} data-withdrawal-id={withdrawal.id}>
                  <td>{formatInstant(withdrawal.proposed_at)}</td>
                  <td>
                    <Qty value={withdrawal.amount} decimals={withdrawal.asset.decimals} symbol={withdrawal.asset.symbol} />
                  </td>
                  <td>
                    <Usd value={withdrawal.notional_usd} />
                  </td>
                  <td>
                    {withdrawal.destination.label}
                    <span className="cell-note mono-small">{withdrawal.destination_address}</span>
                  </td>
                  <td>
                    <code className="mono-small">{withdrawal.evaluation.outcome}</code>
                  </td>
                  <td title={WITHDRAWAL_STATUS_MEANING[withdrawal.status]}>
                    <Pill tone={subjectStatusTone(withdrawal.status)}>{withdrawal.status}</Pill>
                  </td>
                  <td>
                    <Link to={delegationHref("withdrawals", withdrawal.id)}>Open</Link>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="withdrawals"
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
