/**
 * Granting a capability: the account's owner gives one of the account's own
 * agents a scoped, expiring capability. Two steps -- the grant is reviewed,
 * then confirmed -- and the confirmation creates the idempotency key, which
 * the backend also keeps as the grant's own key.
 *
 * Only shapes are checked here. Whether the agent is the account's, whether
 * the limits are in order (single request <= daily <= lifetime), whether the
 * expiry is within 90 days and whether the session's strong sign-in is recent
 * enough are the backend's to decide, and its refusal is shown with its code.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useDelegationAgents,
  useDelegationDestinations,
  useDelegationVenues,
  useGrantCapability,
  type DelegationCapability,
} from "../../../api/advanced/delegation.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import {
  ACTION_CLASSES,
  ACTION_CLASS_LABEL,
  CHAINS,
  CHAIN_LABEL,
  DELEGATION_MODES,
  INSTRUMENT_CLASSES,
  INSTRUMENT_CLASS_LABEL,
  checkAddressList,
  checkGrant,
  confirmAttempt,
  grantBody,
  standingTone,
  toggled,
  type ConfirmedAttempt,
  type DelegationMode,
  type GrantBody,
  type GrantDraft,
} from "../../../lib/delegation.ts";
import { formatInstant } from "../../../lib/time.ts";
import { AttemptKey, Checkbox, TextInput, delegationHref } from "./parts.tsx";

const EMPTY_DRAFT: GrantDraft = {
  agentId: "",
  mode: "SANDBOX",
  instrumentClasses: ["STABLE_VALUE", "SPOT_TOKEN"],
  actionClasses: ["ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"],
  maxNotional: "",
  maxDailyNotional: "",
  maxSingleTrade: "",
  destinations: "",
  chains: ["sbx-solana"],
  days: "7",
  requiresHumanApproval: true,
};

export function GrantForm(props: { readonly accountId: string }): ReactNode {
  const grant = useGrantCapability();
  const agents = useDelegationAgents(props.accountId);
  const venues = useDelegationVenues();
  const destinations = useDelegationDestinations(props.accountId);
  const [draft, setDraft] = useState<GrantDraft>(EMPTY_DRAFT);
  const [review, setReview] = useState<GrantBody | undefined>(undefined);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<GrantBody> | undefined>(undefined);
  const check = checkGrant(draft);

  /** Any change is a different grant: it needs a new review and, when confirmed, a new key. */
  const edit = (change: Partial<GrantDraft>): void => {
    setDraft({ ...draft, ...change });
    setReview(undefined);
    grant.reset();
  };
  const addDestination = (address: string): void => {
    const current = checkAddressList(draft.destinations);
    const list = current.ok ? current.value : [];
    if (list.includes(address)) return;
    edit({ destinations: [...list, address].join("\n") });
  };
  const confirm = (): void => {
    if (review === undefined) return;
    const next = confirmAttempt(attempt, review, newIdempotencyKey);
    setAttempt(next);
    grant.mutate(next);
  };

  if (grant.isSuccess) {
    return (
      <div className="form">
        <GrantResult capability={grant.data} />
        <AttemptKey attempt={attempt} />
        <div className="form-actions">
          <Button
            variant="primary"
            onClick={() => {
              setDraft(EMPTY_DRAFT);
              setReview(undefined);
              setAttempt(undefined);
              grant.reset();
            }}
          >
            Grant another capability
          </Button>
        </div>
      </div>
    );
  }

  if (review !== undefined) {
    return (
      <div className="form" role="group" aria-label="Review the grant">
        <FieldGrid columns={3}>
          <Field label="Agent">
            <Identifier value={review.agent_id} />
          </Field>
          <Field label="Mode" note={ADVANCED_MODE_DESCRIPTIONS[review.mode] ?? review.mode}>
            <Pill tone="info">{review.mode}</Pill>
          </Field>
          <Field label="Expires" note="The backend refuses an expiry more than 90 days ahead.">
            {formatInstant(review.expires_at)}
          </Field>
          <Field label="Assets">{review.instrument_classes.map((kind) => INSTRUMENT_CLASS_LABEL[kind]).join("; ")}</Field>
          <Field label="Actions">{review.action_classes.map((action) => ACTION_CLASS_LABEL[action]).join("; ")}</Field>
          <Field label="Chains">{review.chain_allowlist.join(", ")}</Field>
          <Field label="Single request at most" note="Sent exactly as typed, in sandbox dollars.">
            <span className="num">{review.max_single_trade_usd}</span>
          </Field>
          <Field label="Per day at most">
            <span className="num">{review.max_daily_notional_usd}</span>
          </Field>
          <Field label="In all at most">
            <span className="num">{review.max_notional_usd}</span>
          </Field>
          <Field label="Destinations" note="A swap's venue must be among them, and a withdrawal goes only to one of them.">
            <ul className="codes">
              {review.destination_allowlist.map((address) => (
                <li key={address}>
                  <code className="mono-small">{address}</code>
                </li>
              ))}
            </ul>
          </Field>
          <Field label="Your approval">
            {review.requires_human_approval ? "Every swap waits for your approval" : "Swaps within the limits need no approval"}
          </Field>
        </FieldGrid>
        <AttemptKey attempt={attempt} />
        <div className="form-actions">
          <Button variant="primary" onClick={confirm} busy={grant.isPending} busyLabel="Granting…">
            Confirm and grant
          </Button>
          <Button
            variant="quiet"
            onClick={() => {
              setReview(undefined);
            }}
          >
            Change the grant
          </Button>
        </div>
        {grant.isError && <Explanation error={grant.error} onRetry={confirm} retryLabel="Try again with the same key" />}
      </div>
    );
  }

  const problem = (field: string): string | undefined => check.problems[field];
  return (
    <form
      className="form"
      aria-label="Grant a capability"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (check.ok) setReview(grantBody(props.accountId, draft, new Date()));
      }}
    >
      <TextInput
        label="Agent id"
        value={draft.agentId}
        onChange={(agentId) => {
          edit({ agentId });
        }}
        note="One of this account's agents. The backend refuses any other."
        problem={problem("agent")}
      />
      {agents.isSuccess && agents.data.length > 0 && (
        <div className="form-actions" role="group" aria-label="Choose one of your agents">
          {agents.data.map((agent) => (
            <Button
              key={agent.id}
              variant="quiet"
              onClick={() => {
                edit({ agentId: agent.id });
              }}
            >
              {`Use ${agent.name}`}
            </Button>
          ))}
        </div>
      )}
      <fieldset className="form-row">
        <legend>Mode</legend>
        {DELEGATION_MODES.map((mode: DelegationMode) => (
          <label key={mode} className="radio">
            <input
              type="radio"
              name="grant-mode"
              checked={draft.mode === mode}
              onChange={() => {
                edit({ mode });
              }}
            />{" "}
            {mode} <span className="field-note">{ADVANCED_MODE_DESCRIPTIONS[mode]}</span>
          </label>
        ))}
      </fieldset>
      <fieldset className="form-row">
        <legend>Assets the agent may touch</legend>
        {INSTRUMENT_CLASSES.map((kind) => (
          <Checkbox
            key={kind}
            label={INSTRUMENT_CLASS_LABEL[kind]}
            checked={draft.instrumentClasses.includes(kind)}
            onChange={() => {
              edit({ instrumentClasses: toggled(INSTRUMENT_CLASSES, draft.instrumentClasses, kind) });
            }}
          />
        ))}
        {problem("instrumentClasses") !== undefined && <p className="field-error">{problem("instrumentClasses")}</p>}
      </fieldset>
      <fieldset className="form-row">
        <legend>What it may do</legend>
        {ACTION_CLASSES.map((action) => (
          <Checkbox
            key={action}
            label={ACTION_CLASS_LABEL[action]}
            checked={draft.actionClasses.includes(action)}
            onChange={() => {
              edit({ actionClasses: toggled(ACTION_CLASSES, draft.actionClasses, action) });
            }}
          />
        ))}
        {problem("actionClasses") !== undefined && <p className="field-error">{problem("actionClasses")}</p>}
      </fieldset>
      <fieldset className="form-row">
        <legend>Chains</legend>
        {CHAINS.map((chain) => (
          <Checkbox
            key={chain}
            label={CHAIN_LABEL[chain]}
            checked={draft.chains.includes(chain)}
            onChange={() => {
              edit({ chains: toggled(CHAINS, draft.chains, chain) });
            }}
          />
        ))}
        {problem("chains") !== undefined && <p className="field-error">{problem("chains")}</p>}
      </fieldset>
      <TextInput
        label="Single request at most (sandbox dollars)"
        value={draft.maxSingleTrade}
        inputMode="decimal"
        onChange={(maxSingleTrade) => {
          edit({ maxSingleTrade });
        }}
        note="With exactly two digits of cents. It may not exceed the daily limit."
        problem={problem("maxSingleTrade")}
      />
      <TextInput
        label="Per day at most (sandbox dollars)"
        value={draft.maxDailyNotional}
        inputMode="decimal"
        onChange={(maxDailyNotional) => {
          edit({ maxDailyNotional });
        }}
        note="Counted on the UTC day. It may not exceed the lifetime limit."
        problem={problem("maxDailyNotional")}
      />
      <TextInput
        label="In all at most (sandbox dollars)"
        value={draft.maxNotional}
        inputMode="decimal"
        onChange={(maxNotional) => {
          edit({ maxNotional });
        }}
        note="Everything the capability may ever be used for."
        problem={problem("maxNotional")}
      />
      <TextInput
        label="Destinations"
        value={draft.destinations}
        multiline
        onChange={(value) => {
          edit({ destinations: value });
        }}
        note="Simulated-chain addresses, one per line: the venues a swap may settle at and the destinations a withdrawal may go to."
        problem={problem("destinations")}
      />
      <QuickAddresses
        venues={venues.isSuccess ? venues.data.items.filter((venue) => venue.co_signer) : []}
        destinations={destinations.isSuccess ? destinations.data.filter((destination) => destination.status === "ACTIVE") : []}
        onAdd={addDestination}
      />
      <TextInput
        label="Expires after (days)"
        value={draft.days}
        inputMode="numeric"
        onChange={(days) => {
          edit({ days });
        }}
        note="From 1 to 90 days. Expiry is decided against the clock every time the capability is used."
        problem={problem("days")}
      />
      <Checkbox
        label="Every swap waits for my approval"
        checked={draft.requiresHumanApproval}
        onChange={(requiresHumanApproval) => {
          edit({ requiresHumanApproval });
        }}
      />
      <div className="form-actions">
        {check.ok ? (
          <Button variant="primary" submit>
            Review the grant
          </Button>
        ) : (
          <Button variant="primary" disabledReason={Object.values(check.problems)[0] ?? "Complete the grant."}>
            Review the grant
          </Button>
        )}
      </div>
    </form>
  );
}

function QuickAddresses(props: {
  readonly venues: ReadonlyArray<{ readonly name: string; readonly address: string }>;
  readonly destinations: ReadonlyArray<{ readonly label: string; readonly address: string }>;
  readonly onAdd: (address: string) => void;
}): ReactNode {
  if (props.venues.length === 0 && props.destinations.length === 0) {
    return (
      <p className="field-note">
        The backend lists no venue this deployment co-signs for and no active destination on this account to add from.
      </p>
    );
  }
  return (
    <div className="form-actions" role="group" aria-label="Add a known address">
      {props.venues.map((venue) => (
        <Button
          key={`venue-${venue.address}`}
          variant="quiet"
          onClick={() => {
            props.onAdd(venue.address);
          }}
        >
          {`Add venue ${venue.name}`}
        </Button>
      ))}
      {props.destinations.map((destination) => (
        <Button
          key={`destination-${destination.address}`}
          variant="quiet"
          onClick={() => {
            props.onAdd(destination.address);
          }}
        >
          {`Add destination ${destination.label}`}
        </Button>
      ))}
    </div>
  );
}

function GrantResult(props: { readonly capability: DelegationCapability }): ReactNode {
  const { capability } = props;
  return (
    <div className="callout" role="status" aria-label="The backend's answer" data-granted-capability={capability.id}>
      <p>
        The backend granted capability <code className="mono-small">{capability.id}</code>, now{" "}
        <Pill tone={standingTone(capability.standing)}>{capability.standing}</Pill> until {formatInstant(capability.expires_at)}.
      </p>
      <p>
        <Link to={delegationHref("capabilities", capability.id)}>Open the capability</Link>
      </p>
    </div>
  );
}
