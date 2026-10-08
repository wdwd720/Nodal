/**
 * The operators' view of delegated authority across accounts: every swap and
 * withdrawal not yet at an end, the capabilities granted, the venues and the
 * co-signers this deployment holds -- and the two operator actions, the
 * execution run and venue registration, each with a recent strong sign-in.
 *
 * Reading is all an operator does to a customer's capability or request
 * here: deciding is the account owner's alone. The backend decides whether
 * the session may see this page at all (sandbox:operate); a customer is
 * answered FORBIDDEN, and that answer is what is shown.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import type { CursorPage } from "../../../api/advanced/core.ts";
import {
  useDelegationPending,
  useDelegationVenues,
  useExecutePending,
  useOperatorCapabilities,
  useRegisterVenue,
  type DelegationCapability,
  type DelegationExecutionRun,
  type DelegationPending,
  type DelegationVenueList,
  type ExecuteRequest,
  type VenueRequest,
} from "../../../api/advanced/delegation.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel, Explanation } from "../../../components/DataState.tsx";
import { AsOf, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  CAPABILITY_STATUSES,
  checkUuid,
  checkWholeNumber,
  confirmAttempt,
  standingTone,
  subjectStatusTone,
  type CapabilityStatus,
  type ConfirmedAttempt,
} from "../../../lib/delegation.ts";
import { formatInstant } from "../../../lib/time.ts";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { AttemptKey, DelegationNav, TextInput } from "./parts.tsx";

export function Operator(): ReactNode {
  return (
    <Page
      title="Delegated authority — operator view"
      lead="Delegated authority across every account, read only, and the two operator actions: running the executions that are ready and registering a venue this deployment co-signs for."
    >
      <SimulatedNotice />
      <DelegationNav />
      <PendingPanel />
      <ExecutePanel />
      <CapabilitiesPanel />
      <VenuesPanel />
    </Page>
  );
}

function PendingPanel(): ReactNode {
  const pending = useDelegationPending();
  return (
    <Panel
      title="Not yet at an end"
      description="Every swap and withdrawal awaiting a decision, awaiting execution, or signed and in flight, oldest first, across accounts. Deciding is the account owner's alone."
    >
      <AsyncPanel
        query={pending}
        loadingLabel="Loading what is pending…"
        empty={{
          isEmpty: (view: DelegationPending) => view.swaps.length === 0 && view.withdrawals.length === 0,
          title: "Nothing pending",
          body: "No swap or withdrawal is waiting on anything. That is an empty queue, not an error.",
        }}
      >
        {(view: DelegationPending) => (
          <>
            <AsOf at={view.as_of} />
            {view.swaps.length > 0 && (
              <ScrollTable caption="Pending swaps" headers={["Requested", "Account", "Pays", "Valued at", "Status"]}>
                {view.swaps.map((swap) => (
                  <tr key={swap.id} data-pending-swap={swap.id}>
                    <td>{formatInstant(swap.requested_at)}</td>
                    <td>
                      <code className="mono-small">{swap.account_id}</code>
                    </td>
                    <td>
                      <Qty value={swap.amount_in} decimals={swap.pay_asset.decimals} symbol={swap.pay_asset.symbol} />
                    </td>
                    <td>
                      <Usd value={swap.notional_usd} />
                    </td>
                    <td>
                      <Pill tone={subjectStatusTone(swap.status)}>{swap.status}</Pill>
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
            {view.withdrawals.length > 0 && (
              <ScrollTable caption="Pending withdrawals" headers={["Proposed", "Account", "Amount", "Valued at", "Policy said", "Status"]}>
                {view.withdrawals.map((withdrawal) => (
                  <tr key={withdrawal.id} data-pending-withdrawal={withdrawal.id}>
                    <td>{formatInstant(withdrawal.proposed_at)}</td>
                    <td>
                      <code className="mono-small">{withdrawal.account_id}</code>
                    </td>
                    <td>
                      <Qty value={withdrawal.amount} decimals={withdrawal.asset.decimals} symbol={withdrawal.asset.symbol} />
                    </td>
                    <td>
                      <Usd value={withdrawal.notional_usd} />
                    </td>
                    <td>
                      <code className="mono-small">{withdrawal.evaluation.outcome}</code>
                    </td>
                    <td>
                      <Pill tone={subjectStatusTone(withdrawal.status)}>{withdrawal.status}</Pill>
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function ExecutePanel(): ReactNode {
  const run = useExecutePending();
  const [limit, setLimit] = useState("10");
  const [reviewing, setReviewing] = useState(false);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<ExecuteRequest> | undefined>(undefined);
  const count = checkWholeNumber(limit, 1, 50, "how many to take");
  const confirm = (): void => {
    if (!count.ok) return;
    const next = confirmAttempt(attempt, { limit: count.value }, newIdempotencyKey);
    setAttempt(next);
    run.mutate(next);
  };
  return (
    <Panel
      title="Run the executions that are ready"
      description="Takes, oldest first, every swap admitted without needing approval or approved by its owner, and every approved withdrawal, and has the service execute each under its own authority: inspected, signed, submitted once, settled. The service refuses what may not go ahead -- a revoked capability, a cooling period, a closed gate -- and each outcome is shown. It needs a recent strong sign-in."
    >
      {run.isSuccess ? (
        <RunResult run={run.data} />
      ) : (
        <form
          className="form"
          aria-label="Run the executions that are ready"
          noValidate
          onSubmit={(event) => {
            event.preventDefault();
            if (count.ok) setReviewing(true);
          }}
        >
          <TextInput
            label="At most this many"
            value={limit}
            inputMode="numeric"
            onChange={(value) => {
              setLimit(value);
              setReviewing(false);
              run.reset();
            }}
            note="From 1 to 50, oldest first."
            problem={count.ok ? undefined : count.error}
          />
          {!reviewing ? (
            <div className="form-actions">
              {count.ok ? (
                <Button variant="primary" submit>
                  Review the run
                </Button>
              ) : (
                <Button variant="primary" disabledReason={count.error}>
                  Review the run
                </Button>
              )}
            </div>
          ) : (
            <div className="callout" role="group" aria-label="Confirm the run">
              <p>Execute up to {String(count.value)} ready swaps and withdrawals now, under the service&apos;s own authority?</p>
              <div className="form-actions">
                <Button variant="primary" onClick={confirm} busy={run.isPending} busyLabel="Executing…">
                  Confirm the run
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
          {run.isError && <Explanation error={run.error} onRetry={confirm} retryLabel="Try again with the same key" />}
        </form>
      )}
    </Panel>
  );
}

function RunResult(props: { readonly run: DelegationExecutionRun }): ReactNode {
  if (props.run.items.length === 0) {
    return (
      <p className="callout" role="status" aria-label="The backend's answer" data-run-items="0">
        Nothing was ready: the run executed nothing.
      </p>
    );
  }
  return (
    <div role="status" aria-label="The backend's answer" data-run-items={String(props.run.items.length)}>
      <ScrollTable caption="What the run did" headers={["Kind", "Request", "Account", "Before", "After", "Stopped with"]}>
        {props.run.items.map((item) => (
          <tr key={item.id}>
            <td>{item.kind}</td>
            <td>
              <code className="mono-small">{item.id}</code>
            </td>
            <td>
              <code className="mono-small">{item.account_id}</code>
            </td>
            <td>{item.status_before}</td>
            <td>
              <Pill tone={subjectStatusTone(item.status)}>{item.status}</Pill>
            </td>
            <td>
              {item.error === undefined ? (
                <span className="absent">ran to its end</span>
              ) : (
                <>
                  <code className="mono-small">{item.error.code}</code>
                  {item.error.reason !== undefined && <span className="cell-note mono-small">{item.error.reason}</span>}
                  <span className="cell-note">{item.error.detail}</span>
                </>
              )}
            </td>
          </tr>
        ))}
      </ScrollTable>
    </div>
  );
}

function CapabilitiesPanel(): ReactNode {
  const [status, setStatus] = useState<CapabilityStatus | undefined>(undefined);
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const capabilities = useOperatorCapabilities(status, trail.current);
  return (
    <Panel title="Capabilities across accounts" description="Every capability granted, newest first. Revocation is the account owner's.">
      <div className="filter">
        <label>
          Show{" "}
          <select
            className="input"
            value={status ?? ""}
            onChange={(event) => {
              setStatus(CAPABILITY_STATUSES.find((candidate) => candidate === event.target.value));
              setTrail(FIRST_PAGE);
            }}
          >
            <option value="">every status</option>
            {CAPABILITY_STATUSES.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
              </option>
            ))}
          </select>
        </label>
      </div>
      <AsyncPanel
        query={capabilities}
        loadingLabel="Loading capabilities across accounts…"
        empty={{
          isEmpty: (page: CursorPage<DelegationCapability>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No capabilities",
          body: "No capability matches. That is an empty list, not an error.",
        }}
      >
        {(page: CursorPage<DelegationCapability>) => (
          <>
            <ScrollTable
              caption="Capabilities across accounts"
              headers={["Account", "Agent", "Single request", "Used today", "Used in all", "Expires", "Standing"]}
            >
              {page.items.map((capability) => (
                <tr key={capability.id} data-ops-capability={capability.id}>
                  <td>
                    <code className="mono-small">{capability.account_id}</code>
                  </td>
                  <td>{capability.agent_name ?? capability.agent_id}</td>
                  <td>
                    <Usd value={capability.max_single_trade_usd} />
                  </td>
                  <td>
                    <Usd value={capability.usage.used_today_usd} />
                  </td>
                  <td>
                    <Usd value={capability.usage.used_total_usd} />
                  </td>
                  <td>{formatInstant(capability.expires_at)}</td>
                  <td>
                    <Pill tone={standingTone(capability.standing)}>{capability.standing}</Pill>
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

function VenuesPanel(): ReactNode {
  const venues = useDelegationVenues();
  return (
    <Panel
      title="Venues"
      description="The sandbox venues swaps settle at, each backed by a house book, and the co-signers this deployment holds. A venue settles at its co-signer's address; without the co-signer no swap there is ever signed."
    >
      <AsyncPanel query={venues} loadingLabel="Loading the venues…">
        {(list: DelegationVenueList) => (
          <>
            {list.items.length === 0 ? (
              <p className="note">No venue is registered, so every swap is refused until one is.</p>
            ) : (
              <ScrollTable caption="Registered venues" headers={["Name", "Chain", "Address", "Spread (bps)", "Co-signer here", "House book"]}>
                {list.items.map((venue) => (
                  <tr key={venue.id} data-venue={venue.name}>
                    <td>{venue.name}</td>
                    <td>{venue.chain}</td>
                    <td>
                      <code className="mono-small">{venue.address}</code>
                    </td>
                    <td>{String(venue.spread_bps)}</td>
                    <td>{venue.co_signer ? "yes" : "no"}</td>
                    <td>
                      <code className="mono-small">{venue.house_account_id}</code>
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
            <ScrollTable caption="Co-signers this deployment holds" headers={["Venue name", "Chain", "Address", "Registered"]}>
              {list.co_signers.map((coSigner) => (
                <tr key={coSigner.name} data-co-signer={coSigner.name}>
                  <td>{coSigner.name}</td>
                  <td>{coSigner.chain}</td>
                  <td>
                    <code className="mono-small">{coSigner.address}</code>
                  </td>
                  <td>{coSigner.registered ? "yes" : "no"}</td>
                </tr>
              ))}
            </ScrollTable>
            <RegisterVenueForm unregistered={list.co_signers.filter((coSigner) => !coSigner.registered).map((coSigner) => coSigner.name)} />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function RegisterVenueForm(props: { readonly unregistered: readonly string[] }): ReactNode {
  const register = useRegisterVenue();
  const [name, setName] = useState(props.unregistered[0] ?? "");
  const [spread, setSpread] = useState("50");
  const [house, setHouse] = useState("");
  const [reviewing, setReviewing] = useState(false);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<VenueRequest> | undefined>(undefined);
  const bps = checkWholeNumber(spread, 0, 1000, "the spread in basis points");
  const houseBook = house.trim() === "" ? undefined : checkUuid(house, "The house book id");
  const ready = name !== "" && bps.ok && (houseBook === undefined || houseBook.ok);

  const edit = (apply: () => void): void => {
    apply();
    setReviewing(false);
    register.reset();
  };
  const request = (): VenueRequest => ({
    name,
    spread_bps: bps.value,
    ...(houseBook === undefined ? {} : { house_account_id: houseBook.value }),
  });
  const confirm = (): void => {
    if (!ready) return;
    const next = confirmAttempt(attempt, request(), newIdempotencyKey);
    setAttempt(next);
    register.mutate(next);
  };

  if (register.isSuccess) {
    return (
      <p className="callout" role="status" aria-label="The backend's answer" data-registered-venue={register.data.name}>
        The backend registered venue <strong>{register.data.name}</strong> at <code className="mono-small">{register.data.address}</code>.
      </p>
    );
  }
  if (props.unregistered.length === 0) {
    return (
      <div className="form-actions">
        <Button disabledReason="Every co-signer this deployment holds already has its venue registered.">Register a venue</Button>
      </div>
    );
  }
  return (
    <form
      className="form"
      aria-label="Register a venue"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (ready) setReviewing(true);
      }}
    >
      <div className="form-row">
        <label>
          Venue{" "}
          <select
            className="input"
            value={name}
            onChange={(event) => {
              edit(() => {
                setName(event.target.value);
              });
            }}
          >
            {props.unregistered.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
              </option>
            ))}
          </select>
        </label>
        <p className="field-note">Only a venue this deployment holds a co-signer for; its address is the co-signer&apos;s.</p>
      </div>
      <TextInput
        label="Spread (basis points)"
        value={spread}
        inputMode="numeric"
        onChange={(value) => {
          edit(() => {
            setSpread(value);
          });
        }}
        note="From 0 to 1000, applied to the venue's quote."
        problem={bps.ok ? undefined : bps.error}
      />
      <TextInput
        label="House book id (optional)"
        value={house}
        onChange={(value) => {
          edit(() => {
            setHouse(value);
          });
        }}
        note="The house book that is the venue's counterparty. Left empty, the venue gets a house book of its own."
        problem={houseBook === undefined || houseBook.ok ? undefined : houseBook.error}
      />
      {!reviewing ? (
        <div className="form-actions">
          {ready ? (
            <Button variant="primary" submit>
              Review the venue
            </Button>
          ) : (
            <Button variant="primary" disabledReason={!bps.ok ? bps.error : (houseBook?.error ?? "Choose a venue.")}>
              Review the venue
            </Button>
          )}
        </div>
      ) : (
        <div className="callout" role="group" aria-label="Confirm the venue">
          <p>
            Register <strong>{name}</strong> with a spread of {String(bps.value)} basis points
            {houseBook === undefined ? " and a house book of its own" : ` against house book ${houseBook.value}`}? A venue is never edited.
          </p>
          <div className="form-actions">
            <Button variant="primary" onClick={confirm} busy={register.isPending} busyLabel="Registering…">
              Confirm and register
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
      {register.isError && <Explanation error={register.error} onRetry={confirm} retryLabel="Try again with the same key" />}
    </form>
  );
}
