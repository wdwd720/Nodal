/**
 * The owner's controls over where an agent's proposed withdrawals may go and
 * how they are decided: the allow-listed destinations and the versioned
 * withdrawal policy.
 *
 * A withdrawal goes only to an active destination on this list (and on the
 * capability's), under the policy version in force when it was proposed.
 * Adding or removing a destination and writing a policy version each need a
 * recent strong sign-in; every rule is the backend's, and its refusal is shown
 * with its code.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useAddDestination,
  useDelegationDestinations,
  useDelegationPolicy,
  useRemoveDestination,
  useSetWithdrawalPolicy,
  type DelegationDestination,
  type DelegationWithdrawalPolicy,
  type DelegationWithdrawalPolicyState,
  type DestinationRequest,
} from "../../../api/advanced/delegation.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel, Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { formatSeconds } from "../../../lib/advanced.ts";
import {
  CHAINS,
  CHAIN_LABEL,
  checkAddress,
  checkPolicy,
  checkText,
  confirmAttempt,
  policyDraftFrom,
  type ConfirmedAttempt,
  type DelegationChain,
  type PolicyBody,
  type PolicyDraft,
} from "../../../lib/delegation.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { NoAccount, ScrollTable, ValueDomainPill } from "../shared.tsx";
import { ActorText, AttemptKey, Checkbox, DelegationNav, InspectingNote, ReasonCommand, TextInput } from "./parts.tsx";

export function Destinations(): ReactNode {
  const account = useAdvancedAccount();
  const { accountId } = account;
  const ownAccount = accountId !== undefined && !account.inspecting ? accountId : undefined;
  return (
    <Page
      title="Destinations and withdrawal policy"
      lead="Where your agents' proposed withdrawals may go, and the rules that decide them. An agent may only propose; it never withdraws, and it never changes these."
    >
      <SimulatedNotice />
      <DelegationNav />
      {accountId === undefined ? (
        <Panel title="Destinations">
          <NoAccount />
        </Panel>
      ) : (
        <>
          <DestinationsPanel accountId={accountId} ownAccount={ownAccount} />
          <PolicyPanel accountId={accountId} ownAccount={ownAccount} />
        </>
      )}
    </Page>
  );
}

function DestinationsPanel(props: { readonly accountId: string; readonly ownAccount: string | undefined }): ReactNode {
  const destinations = useDelegationDestinations(props.accountId);
  return (
    <Panel
      title="Allow-listed destinations"
      description="Every destination this account allow-listed, newest first. A destination's age starts when it is added; a removed one receives nothing that was not already signed."
    >
      <AsyncPanel
        query={destinations}
        loadingLabel="Loading the account's destinations…"
        empty={{
          isEmpty: (items: readonly DelegationDestination[]) => items.length === 0,
          title: "No destinations",
          body: "This account has allow-listed no destination, so no withdrawal can go anywhere yet.",
        }}
      >
        {(items: readonly DelegationDestination[]) => (
          <ScrollTable caption="Allow-listed destinations" headers={["Label", "Chain", "Address", "Status", "Added", "Removed"]}>
            {items.map((destination) => (
              <tr key={destination.id} data-destination-id={destination.id}>
                <td>{destination.label}</td>
                <td>{destination.chain}</td>
                <td>
                  <code className="mono-small">{destination.address}</code>
                </td>
                <td>
                  <Pill tone={destination.status === "ACTIVE" ? "good" : "neutral"}>{destination.status}</Pill>
                </td>
                <td>
                  {formatInstant(destination.added_at)}
                  <span className="cell-note">
                    <ActorText actor={destination.added_by} />
                  </span>
                </td>
                <td>
                  {destination.removed_at === undefined ? (
                    props.ownAccount !== undefined && destination.status === "ACTIVE" ? (
                      <RemoveDestination destination={destination} />
                    ) : (
                      <span className="absent">not removed</span>
                    )
                  ) : (
                    <>
                      {formatInstant(destination.removed_at)}
                      <span className="cell-note">
                        <ActorText actor={destination.removed_by} />
                      </span>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </AsyncPanel>
      {props.ownAccount === undefined ? (
        <InspectingNote what="change the allow-list" />
      ) : (
        <AddDestinationForm key={`add-${props.ownAccount}`} accountId={props.ownAccount} />
      )}
    </Panel>
  );
}

function RemoveDestination(props: { readonly destination: DelegationDestination }): ReactNode {
  const remove = useRemoveDestination();
  const [open, setOpen] = useState(false);
  if (!open) {
    return (
      <Button
        variant="danger"
        onClick={() => {
          setOpen(true);
        }}
      >
        {`Remove ${props.destination.label}`}
      </Button>
    );
  }
  return (
    <ReasonCommand
      id={props.destination.id}
      label={`Remove ${props.destination.label}`}
      confirmLabel="Confirm removal"
      reasonNote="Recorded with the removal. It needs your recent strong sign-in."
      command={remove}
      done={(removed) => (
        <p className="callout" role="status" data-removed={removed.id}>
          The backend recorded the destination as <Pill>{removed.status}</Pill>.
        </p>
      )}
    />
  );
}

function AddDestinationForm(props: { readonly accountId: string }): ReactNode {
  const add = useAddDestination();
  const [chain, setChain] = useState<DelegationChain>("sbx-solana");
  const [address, setAddress] = useState("");
  const [label, setLabel] = useState("");
  const [reviewing, setReviewing] = useState(false);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<DestinationRequest> | undefined>(undefined);
  const where = checkAddress(address);
  const name = checkText(label, "a label", 120);
  const ready = where.ok && name.ok;

  const edit = (apply: () => void): void => {
    apply();
    setReviewing(false);
    add.reset();
  };
  const confirm = (): void => {
    if (!ready) return;
    const next = confirmAttempt(attempt, { account_id: props.accountId, chain, address: where.value, label: name.value }, newIdempotencyKey);
    setAttempt(next);
    add.mutate(next);
  };

  if (add.isSuccess) {
    return (
      <div className="form">
        <p className="callout" role="status" aria-label="The backend's answer" data-added-destination={add.data.id}>
          The backend allow-listed <strong>{add.data.label}</strong> as <Pill tone="good">{add.data.status}</Pill>. Its age starts now.
        </p>
        <AttemptKey attempt={attempt} />
        <div className="form-actions">
          <Button
            onClick={() => {
              setAddress("");
              setLabel("");
              setAttempt(undefined);
              setReviewing(false);
              add.reset();
            }}
          >
            Add another destination
          </Button>
        </div>
      </div>
    );
  }
  return (
    <form
      className="form"
      aria-label="Add a destination"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (ready) setReviewing(true);
      }}
    >
      <fieldset className="form-row">
        <legend>Chain</legend>
        {CHAINS.map((option) => (
          <label key={option} className="radio">
            <input
              type="radio"
              name="destination-chain"
              checked={chain === option}
              onChange={() => {
                edit(() => {
                  setChain(option);
                });
              }}
            />{" "}
            {CHAIN_LABEL[option]}
          </label>
        ))}
      </fieldset>
      <TextInput
        label="Address"
        value={address}
        onChange={(value) => {
          edit(() => {
            setAddress(value);
          });
        }}
        note="An address on the simulated chain. The delegated wallet itself cannot be its own destination."
        problem={where.ok ? undefined : where.error}
      />
      <TextInput
        label="Label"
        value={label}
        onChange={(value) => {
          edit(() => {
            setLabel(value);
          });
        }}
        note="How you will recognise it, up to 120 characters."
        problem={name.ok ? undefined : name.error}
      />
      {!reviewing ? (
        <div className="form-actions">
          {ready ? (
            <Button variant="primary" submit>
              Review the destination
            </Button>
          ) : (
            <Button variant="primary" disabledReason={!where.ok ? where.error : name.error}>
              Review the destination
            </Button>
          )}
        </div>
      ) : (
        <div className="callout" role="group" aria-label="Confirm the destination">
          <p>
            Allow-list <code className="mono-small">{where.value}</code> on {chain} as &quot;{name.value}&quot;? Withdrawals may then be
            proposed to it under capabilities that name it.
          </p>
          <div className="form-actions">
            <Button variant="primary" onClick={confirm} busy={add.isPending} busyLabel="Adding…">
              Confirm and add
            </Button>
            <Button
              variant="quiet"
              onClick={() => {
                setReviewing(false);
              }}
            >
              Not now
            </Button>
          </div>
        </div>
      )}
      <AttemptKey attempt={attempt} />
      {add.isError && <Explanation error={add.error} onRetry={confirm} retryLabel="Try again with the same key" />}
    </form>
  );
}

function PolicyPanel(props: { readonly accountId: string; readonly ownAccount: string | undefined }): ReactNode {
  const policy = useDelegationPolicy(props.accountId);
  return (
    <Panel
      title="Withdrawal policy"
      description="The version in force decides every new proposal: hard limits refuse it, anything unusual waits for you, and only in a test environment may the policy approve one automatically. Versions are never edited; a change is a new version."
    >
      <AsyncPanel query={policy} loadingLabel="Loading the withdrawal policy…">
        {(state: DelegationWithdrawalPolicyState) => (
          <>
            {state.current === undefined ? (
              <p className="note" data-policy="none">
                This account has no withdrawal policy, so nothing can be withdrawn: the backend refuses every proposal without one.
              </p>
            ) : (
              <PolicyFields policy={state.current} />
            )}
            {state.versions.length > 1 && (
              <ScrollTable caption="Earlier versions" headers={["Version", "Effective", "Per withdrawal", "Per day", "Policy hash"]}>
                {state.versions.slice(1).map((version) => (
                  <tr key={version.id}>
                    <td>{String(version.version)}</td>
                    <td>{formatInstant(version.effective_at)}</td>
                    <td>
                      <Usd value={version.per_withdrawal_limit_usd} />
                    </td>
                    <td>
                      <Usd value={version.daily_limit_usd} />
                    </td>
                    <td>
                      <code className="mono-small">{version.policy_hash}</code>
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
            {props.ownAccount === undefined ? (
              <InspectingNote what="write a withdrawal policy" />
            ) : (
              <PolicyForm key={`policy-${props.ownAccount}`} accountId={props.ownAccount} current={state.current} />
            )}
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function PolicyFields(props: { readonly policy: DelegationWithdrawalPolicy }): ReactNode {
  const { policy } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Version in force" note={`Effective ${exactInstant(policy.effective_at)}`} emphasis>
          <span data-field="policy-version">{String(policy.version)}</span>
        </Field>
        <Field label="Per withdrawal at most">
          <span data-field="policy-per-withdrawal">
            <Usd value={policy.per_withdrawal_limit_usd} />
          </span>
        </Field>
        <Field label="Per day at most">
          <Usd value={policy.daily_limit_usd} />
        </Field>
        <Field label="Withdrawals per 24 hours at most">{String(policy.velocity_max_count)}</Field>
        <Field label="Highest risk score accepted">{String(policy.max_risk_score)}</Field>
        <Field label="Cooling period" note="Nothing is signed before it has passed.">
          {formatSeconds(policy.cooling_period_seconds)}
        </Field>
        <Field label="Destination at least">{formatSeconds(policy.min_destination_age_seconds)} old</Field>
        <Field label="Account at least">{formatSeconds(policy.min_account_age_seconds)} old</Field>
        <Field label="Your decision" note="Within this window of a strong sign-in, when required.">
          {policy.strong_auth_required ? `needs a strong sign-in within ${formatSeconds(policy.strong_auth_max_age_seconds)}` : "no extra sign-in window"}
        </Field>
        <Field
          label="Automatic approval"
          note="Honoured only in LOCAL, TEST and DEV, and only with every automatic bound met; everywhere else you decide."
        >
          {policy.auto_approve_enabled ? (
            <>
              up to <Usd value={policy.auto_approve_max_usd} /> and a risk score of {String(policy.auto_approve_max_risk_score)}
            </>
          ) : (
            "off"
          )}
        </Field>
        <Field label="Written by">
          <ActorText actor={policy.created_by} />
        </Field>
        <Field label="Policy hash">
          <Identifier value={policy.policy_hash} />
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <ValueDomainPill value={policy.value_domain} />
      </p>
    </>
  );
}

/** The fields of a policy draft that are typed as text. */
type PolicyTextField = { [K in keyof PolicyDraft]: PolicyDraft[K] extends string ? K : never }[keyof PolicyDraft];

function PolicyForm(props: { readonly accountId: string; readonly current: DelegationWithdrawalPolicy | undefined }): ReactNode {
  const save = useSetWithdrawalPolicy();
  const [draft, setDraft] = useState<PolicyDraft>(policyDraftFrom(props.current));
  const [review, setReview] = useState<PolicyBody | undefined>(undefined);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<PolicyBody> | undefined>(undefined);
  const check = checkPolicy(props.accountId, draft);

  const edit = (change: Partial<PolicyDraft>): void => {
    setDraft({ ...draft, ...change });
    setReview(undefined);
    save.reset();
  };
  const confirm = (): void => {
    if (review === undefined) return;
    const next = confirmAttempt(attempt, review, newIdempotencyKey);
    setAttempt(next);
    save.mutate(next);
  };

  if (save.isSuccess) {
    return (
      <div className="form">
        <p className="callout" role="status" aria-label="The backend's answer" data-policy-version={String(save.data.version)}>
          The backend recorded withdrawal policy version {String(save.data.version)}; it decides every proposal from now on.
        </p>
        <AttemptKey attempt={attempt} />
      </div>
    );
  }
  const text = (field: PolicyTextField, label: string, note: string): ReactNode => (
    <TextInput
      label={label}
      value={draft[field]}
      onChange={(value) => {
        setDraft({ ...draft, [field]: value });
        setReview(undefined);
        save.reset();
      }}
      note={note}
      problem={check.problems[field]}
    />
  );
  return (
    <form
      className="form"
      aria-label="Write a new policy version"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (check.ok && check.value !== undefined) setReview(check.value);
      }}
    >
      {text("perWithdrawalLimit", "Per withdrawal at most (sandbox dollars)", "With exactly two digits of cents.")}
      {text("dailyLimit", "Per day at most (sandbox dollars)", "At least the per-withdrawal limit.")}
      {text("velocityMaxCount", "Withdrawals per 24 hours at most", "From 1 to 1000.")}
      {text("maxRiskScore", "Highest risk score accepted", "From 0 to 100; above it a proposal is refused.")}
      {text("coolingPeriodSeconds", "Cooling period (seconds)", "From 0 to 604800; nothing is signed before it has passed.")}
      {text("minDestinationAgeSeconds", "Destination at least this old (seconds)", "A younger destination waits for you.")}
      {text("minAccountAgeSeconds", "Account at least this old (seconds)", "A younger account waits for you.")}
      <Checkbox
        label="My decision needs a strong sign-in within the window below"
        checked={draft.strongAuthRequired}
        onChange={(strongAuthRequired) => {
          edit({ strongAuthRequired });
        }}
      />
      {text("strongAuthMaxAgeSeconds", "Strong sign-in window (seconds)", "From 1 to 86400.")}
      <Checkbox
        label="Let the policy approve automatically (test environments only)"
        checked={draft.autoApproveEnabled}
        onChange={(autoApproveEnabled) => {
          edit({ autoApproveEnabled });
        }}
      />
      {text("autoApproveMax", "Automatic approval at most (sandbox dollars)", "At most the per-withdrawal limit; ignored outside a test environment.")}
      {text("autoApproveMaxRiskScore", "Automatic approval risk score at most", "At most the highest risk score accepted.")}
      {review === undefined ? (
        <div className="form-actions">
          {check.ok ? (
            <Button variant="primary" submit>
              Review the new version
            </Button>
          ) : (
            <Button variant="primary" disabledReason={Object.values(check.problems)[0] ?? "Complete the policy."}>
              Review the new version
            </Button>
          )}
        </div>
      ) : (
        <div className="callout" role="group" aria-label="Confirm the new policy version">
          <p>
            Write a new version: per withdrawal <span className="num">{review.per_withdrawal_limit_usd}</span>, per day{" "}
            <span className="num">{review.daily_limit_usd}</span> sandbox dollars, {String(review.velocity_max_count)} per 24 hours, cooling{" "}
            {formatSeconds(review.cooling_period_seconds)}, automatic approval {review.auto_approve_enabled ? "on" : "off"}. It applies to every
            proposal from now on.
          </p>
          <div className="form-actions">
            <Button variant="primary" onClick={confirm} busy={save.isPending} busyLabel="Writing…">
              Confirm the new version
            </Button>
            <Button
              variant="quiet"
              onClick={() => {
                setReview(undefined);
              }}
            >
              Change it
            </Button>
          </div>
        </div>
      )}
      <AttemptKey attempt={attempt} />
      {save.isError && <Explanation error={save.error} onRetry={confirm} retryLabel="Try again with the same key" />}
    </form>
  );
}
