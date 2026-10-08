/**
 * One capability: what it allows, its limits and how much of them its agent
 * has used, its expiry, every use recorded under it, and its revocation.
 *
 * The backend answers NOT_FOUND both for an id it never recorded and for a
 * capability this session may not read, so the page neither confirms nor
 * denies that the id exists. Revocation is the owner's alone, with a recent
 * strong sign-in; a revoked capability admits nothing more, and nothing not
 * yet signed under it is ever signed.
 */
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useDelegationCapability,
  useRevokeCapability,
  type DelegationCapability,
  type DelegationCapabilityDetail,
} from "../../../api/advanced/delegation.ts";
import { Button, LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import {
  ACTION_CLASS_LABEL,
  CHAIN_LABEL,
  INSTRUMENT_CLASS_LABEL,
  STANDING_MEANING,
  standingTone,
} from "../../../lib/delegation.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { ModePill, ScrollTable, ValueDomainPill } from "../shared.tsx";
import { ActorText, DelegationNav, InspectingNote, ReasonCommand, delegationHref } from "./parts.tsx";

export function CapabilityDetail(): ReactNode {
  const { capabilityId } = useParams();
  const account = useAdvancedAccount();
  const capability = useDelegationCapability(account.accountId, capabilityId);
  return (
    <Page
      title="Capability"
      lead="One capability granted to one of the account's agents: its scope, its limits and their use, its expiry, and every use recorded under it."
      actions={<LinkButton to={delegationHref()}>Back to agent authority</LinkButton>}
    >
      <SimulatedNotice />
      <DelegationNav />
      <Panel title="Capability" {...(capabilityId === undefined ? {} : { description: `Requested by id ${capabilityId}.` })}>
        <AsyncPanel
          query={capability}
          loadingLabel="Loading the capability…"
          errorExtra={
            <p className="explain-body">
              The backend answers NOT_FOUND both for an id it has never recorded and for a capability this session may not
              read, so the answer neither confirms nor denies that it exists.
            </p>
          }
        >
          {(found: DelegationCapabilityDetail) => <CapabilityFields capability={found} />}
        </AsyncPanel>
      </Panel>
      {capability.isSuccess && (
        <>
          <UsesPanel capability={capability.data} />
          <RevokePanel capability={capability.data} />
        </>
      )}
    </Page>
  );
}

export function CapabilityFields(props: { readonly capability: DelegationCapability }): ReactNode {
  const { capability } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Standing" note={STANDING_MEANING[capability.standing]} emphasis>
          <span data-field="standing">
            <Pill tone={standingTone(capability.standing)}>{capability.standing}</Pill>
          </span>
        </Field>
        <Field label="Expires" note={`Read at ${exactInstant(capability.as_of)}; expiry is decided against the backend's clock.`}>
          <time dateTime={capability.expires_at}>{formatInstant(capability.expires_at)}</time>
        </Field>
        <Field label="Agent" note={`Acting for strategy ${capability.strategy_id}.`}>
          {capability.agent_name ?? <span className="absent">unnamed</span>} <Identifier value={capability.agent_id} />
        </Field>
        <Field label="Mode" note={ADVANCED_MODE_DESCRIPTIONS[capability.mode] ?? capability.mode}>
          <ModePill mode={capability.mode} />
        </Field>
        <Field label="Environment">{capability.environment}</Field>
        <Field label="Your approval">
          {capability.requires_human_approval ? "Every swap waits for your approval" : "Swaps within the limits need no approval"}
        </Field>
        <Field label="Actions">
          <ul className="codes">
            {capability.action_classes.map((action) => (
              <li key={action}>{ACTION_CLASS_LABEL[action]}</li>
            ))}
          </ul>
        </Field>
        <Field label="Assets">
          <ul className="codes">
            {capability.instrument_classes.map((kind) => (
              <li key={kind}>{INSTRUMENT_CLASS_LABEL[kind]}</li>
            ))}
          </ul>
        </Field>
        <Field label="Chains">
          <ul className="codes">
            {capability.chain_allowlist.map((chain) => (
              <li key={chain}>{CHAIN_LABEL[chain]}</li>
            ))}
          </ul>
        </Field>
      </FieldGrid>
      <FieldGrid columns={3}>
        <Field label="Single request at most">
          <span data-field="max-single">
            <Usd value={capability.max_single_trade_usd} />
          </span>
        </Field>
        <Field label={`Used on ${capability.usage.day} (UTC)`} note={`Of a daily limit; ${String(capability.usage.uses)} uses in all.`}>
          <span data-field="used-today">
            <Usd value={capability.usage.used_today_usd} />
          </span>{" "}
          of <Usd value={capability.max_daily_notional_usd} />
        </Field>
        <Field label="Left today" note="The daily limit less today's use, never below zero.">
          <span data-field="remaining-today">
            <Usd value={capability.usage.remaining_today_usd} />
          </span>
        </Field>
        <Field label="Used in all">
          <span data-field="used-total">
            <Usd value={capability.usage.used_total_usd} />
          </span>{" "}
          of <Usd value={capability.max_notional_usd} />
        </Field>
        <Field label="Left in all" note="The lifetime limit less everything used, never below zero.">
          <span data-field="remaining-total">
            <Usd value={capability.usage.remaining_total_usd} />
          </span>
        </Field>
        <Field label="Content hash" note="Covers every term of the grant; every plan signed under it names it.">
          <Identifier value={capability.content_hash} />
        </Field>
      </FieldGrid>
      <FieldGrid columns={3}>
        <Field label="Granted" note={exactInstant(capability.granted_at)}>
          {formatInstant(capability.granted_at)} by <ActorText actor={capability.granted_by} />
        </Field>
        <Field label="Revoked">
          {capability.revoked_at === undefined ? (
            <span className="absent">not revoked</span>
          ) : (
            <>
              {formatInstant(capability.revoked_at)} by <ActorText actor={capability.revoked_by} />
              {capability.revocation_reason !== undefined && <span className="cell-note">{capability.revocation_reason}</span>}
            </>
          )}
        </Field>
        <Field label="Destinations" note="The addresses a swap may settle at and a withdrawal may go to.">
          <ul className="codes">
            {capability.destination_allowlist.map((address) => (
              <li key={address}>
                <code className="mono-small">{address}</code>
              </li>
            ))}
          </ul>
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <ValueDomainPill value={capability.value_domain} />
      </p>
    </>
  );
}

function UsesPanel(props: { readonly capability: DelegationCapabilityDetail }): ReactNode {
  const { uses } = props.capability;
  return (
    <Panel
      title="Uses"
      description="Every request admitted under this capability, newest first, at the value it was admitted at. A use stays counted whatever became of the request."
    >
      {uses.length === 0 ? (
        <p className="note">Nothing has been admitted under this capability.</p>
      ) : (
        <ScrollTable caption="Uses of this capability" headers={["When", "Kind", "Action", "Valued at", "Request"]}>
          {uses.map((use) => (
            <tr key={use.id}>
              <td>{formatInstant(use.used_at)}</td>
              <td>{use.subject_kind}</td>
              <td>{ACTION_CLASS_LABEL[use.action_class]}</td>
              <td>
                <Usd value={use.notional_usd} />
              </td>
              <td>
                <Link to={delegationHref(use.subject_kind === "SWAP" ? "swaps" : "withdrawals", use.subject_id)}>Open</Link>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </Panel>
  );
}

function RevokePanel(props: { readonly capability: DelegationCapability }): ReactNode {
  const { capability } = props;
  const account = useAdvancedAccount();
  const revoke = useRevokeCapability();
  const own = account.ownAccountId !== undefined && account.ownAccountId.toLowerCase() === capability.account_id.toLowerCase();
  return (
    <Panel
      title="Revoke"
      description="Revocation is the one change a capability ever makes. It needs your recent strong sign-in, and nothing not yet signed under it is signed afterwards."
    >
      {!own ? (
        <InspectingNote what="revoke a capability" />
      ) : capability.status === "REVOKED" ? (
        <div className="form-actions">
          <Button variant="danger" disabledReason="This capability is already revoked.">
            Revoke this capability
          </Button>
        </div>
      ) : (
        <ReasonCommand
          id={capability.id}
          label="Revoke this capability"
          confirmLabel="Confirm revocation"
          reasonNote="Recorded with the revocation and its audit record."
          command={revoke}
          done={(revoked) => (
            <p className="callout" role="status" data-revoked={revoked.id}>
              The backend recorded the capability as <Pill tone={standingTone(revoked.standing)}>{revoked.standing}</Pill>.
            </p>
          )}
        />
      )}
    </Panel>
  );
}
