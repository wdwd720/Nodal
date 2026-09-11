/**
 * Payout destinations (goal §25).
 *
 * The rule that shapes every line of this file:
 *
 *   NODAL NEVER HOLDS AN ACCOUNT NUMBER.
 *
 * What is registered here is the PROVIDER'S token for a destination plus a mask
 * a person recognises. There is no field on this form for a bank account
 * number, a card number, an IBAN, a routing number, a private key or a seed
 * phrase, and the API refuses an input that looks like one rather than storing
 * it. This screen does not pre-empt that check with a regular expression of its
 * own: the refusal is the backend's, and rendering its message is how somebody
 * learns what this product will and will not hold.
 *
 * A destination is born UNUSABLE. Whether value may be sent to it is the
 * provider's decision and not the request's, so a newly added one shows as
 * awaiting the provider rather than as ready.
 *
 * Removing disables rather than deletes, because a destination value has left
 * through is financial history. The list therefore includes disabled and
 * rejected rows: somebody who removed one and cannot see it gone will add it
 * again, and somebody whose destination a provider refused needs to see the
 * refusal rather than an empty list.
 *
 * Both writes need a recent strong sign-in. A STEP_UP_REQUIRED here is the
 * system working, and `Explanation` renders the round trip — the form state is
 * kept, the trip carries the return path, and coming back lands on this page
 * with what was typed still in it.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useAddDestination,
  usePayoutDestinations,
  useRemoveDestination,
  type PayoutDestination,
  type PayoutDestinationKind,
} from "../../api/queries.ts";
import { Button } from "../../components/Button.tsx";
import { AsyncPanel, Explanation } from "../../components/DataState.tsx";
import { DataTable } from "../../components/DataTable.tsx";
import { Disclosure, FormField, Panel, StatusBadge, type Tone } from "../../components/Layout.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { formatInstant } from "../../lib/time.ts";

/** The four shapes a destination can take, in the API's own words. */
const KINDS: readonly PayoutDestinationKind[] = ["BANK", "FIAT_WALLET", "CARD_PUSH", "CRYPTO_WALLET"];

const KIND_LABELS: Readonly<Record<string, string>> = {
  BANK: "Bank account",
  FIAT_WALLET: "Fiat wallet",
  CARD_PUSH: "Push to card",
  CRYPTO_WALLET: "Crypto wallet",
};

const STATUS_COPY: Readonly<Record<string, { readonly tone: Tone; readonly sentence: string }>> = {
  UNVERIFIED: {
    tone: "warn",
    sentence:
      "Registered, and the provider has not confirmed it yet. A destination is born unusable; whether value may go there is the provider's decision.",
  },
  VERIFIED: { tone: "good", sentence: "The provider confirmed it. Value may be sent here." },
  REJECTED: {
    tone: "bad",
    sentence:
      "The provider refused it. This is terminal: adding the same destination again is a new registration with its own creation time.",
  },
  DISABLED: {
    tone: "neutral",
    sentence:
      "You stopped using it. It is kept rather than deleted because value that has left through a destination is financial history.",
  },
};

function statusOf(status: string): { readonly tone: Tone; readonly sentence: string } {
  return (
    STATUS_COPY[status] ?? {
      tone: "neutral",
      sentence: "The backend reported a status this page has no sentence for. It is shown unchanged.",
    }
  );
}

interface DestinationDraft {
  readonly kind: PayoutDestinationKind;
  readonly token: string;
  readonly label: string;
  readonly currency: string;
  readonly country: string;
}

const EMPTY: DestinationDraft = {
  kind: "BANK",
  token: "",
  label: "",
  currency: "",
  country: "",
};

/** Picks a declared kind back out of a select without trusting the string. */
function kindOf(raw: string): PayoutDestinationKind {
  return KINDS.find((kind) => kind === raw) ?? "BANK";
}

export function Destinations(props: {
  readonly accountId: string;
  /** Called when the chosen destination changes, so the amount step can follow. */
  readonly onChoose?: (destinationId: string) => void;
  readonly chosen?: string | undefined;
}): ReactNode {
  const destinations = usePayoutDestinations(props.accountId);
  const add = useAddDestination();
  const remove = useRemoveDestination();
  const draft = useSurvivesSignIn<DestinationDraft>("withdraw.destination", EMPTY);
  /**
   * Open when this mount recovered a draft.
   *
   * Adding a destination needs a strong sign-in, so the common path through
   * this form is: fill it, be told to confirm, leave for the provider, come
   * back. `useSurvivesSignIn` keeps what was typed across that trip, and
   * without this the customer returns to a collapsed form and has to find their
   * own way back to it — which makes the promise that their input was kept true
   * and useless at the same time.
   */
  const [adding, setAdding] = useState(draft.recovered);

  const tokenOk = draft.value.token.trim().length > 0;
  const { onChoose } = props;

  return (
    <Panel
      title="Where value would be sent"
      description="A provider's token and a mask. Nodal holds no account number, here or anywhere."
    >
      <AsyncPanel
        query={destinations}
        loadingLabel="Loading your destinations…"
        skeleton={<Skeleton shape="rows" count={2} label="Your payout destinations are loading" />}
        empty={{
          isEmpty: (rows: PayoutDestination[]) => rows.length === 0,
          title: "No payout destination yet",
          body: "A withdrawal needs somewhere to go. Adding one registers a provider's token, not an account number, and the provider decides whether it may receive value.",
        }}
      >
        {(rows: PayoutDestination[]) => (
          <DataTable
            caption="Your payout destinations, including the ones a provider refused and the ones you stopped using"
            rows={rows}
            rowKey={(row: PayoutDestination) => row.destination_id}
            columns={[
              {
                key: "label",
                header: "Destination",
                cell: (row: PayoutDestination) => (
                  <span>
                    {row.display_label ?? KIND_LABELS[row.kind] ?? row.kind}{" "}
                    <span className="mono-small">{row.masked_display ?? ""}</span>
                  </span>
                ),
              },
              {
                key: "kind",
                header: "Kind",
                cell: (row: PayoutDestination) => (
                  <span>{KIND_LABELS[row.kind] ?? row.kind}</span>
                ),
              },
              {
                key: "status",
                header: "Status",
                cell: (row: PayoutDestination) => (
                  <>
                    <StatusBadge tone={statusOf(row.status).tone} title={statusOf(row.status).sentence}>
                      {row.status}
                    </StatusBadge>
                    {row.sandbox && <StatusBadge tone="warn">rehearsal</StatusBadge>}
                  </>
                ),
              },
              {
                key: "usable",
                header: "May receive value",
                cell: (row: PayoutDestination) =>
                  row.usable === true ? (
                    <StatusBadge tone="good">yes</StatusBadge>
                  ) : (
                    <StatusBadge tone="warn">not yet</StatusBadge>
                  ),
              },
              {
                key: "added",
                header: "Added",
                cell: (row: PayoutDestination) => <span>{formatInstant(row.created_at)}</span>,
              },
              {
                key: "use",
                header: "Use it",
                cell: (row: PayoutDestination) =>
                  onChoose === undefined ? (
                    <span className="absent">—</span>
                  ) : row.usable !== true ? (
                    <Button disabledReason="The provider has not confirmed this destination, so a payout cannot be sent to it yet.">
                      Use this
                    </Button>
                  ) : props.chosen === row.destination_id ? (
                    <StatusBadge tone="good">chosen</StatusBadge>
                  ) : (
                    <Button
                      variant="secondary"
                      onClick={() => {
                        onChoose(row.destination_id);
                      }}
                    >
                      Use this
                    </Button>
                  ),
              },
              {
                key: "remove",
                header: "Stop using",
                cell: (row: PayoutDestination) =>
                  row.status === "DISABLED" || row.status === "REJECTED" ? (
                    <Button disabledReason="This destination is already finished with. It is kept because value that has left through one is financial history.">
                      Remove
                    </Button>
                  ) : (
                    <Button
                      variant="quiet"
                      busy={remove.isPending}
                      busyLabel="Removing…"
                      onClick={() => {
                        remove.mutate({
                          destinationId: row.destination_id,
                          accountId: props.accountId,
                          idempotencyKey: newIdempotencyKey(),
                        });
                      }}
                    >
                      Remove
                    </Button>
                  ),
              },
            ]}
          />
        )}
      </AsyncPanel>

      {remove.isError && <Explanation error={remove.error} onRetry={remove.reset} />}

      {adding ? (
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (!tokenOk) return;
            add.mutate(
              {
                accountId: props.accountId,
                kind: draft.value.kind,
                providerToken: draft.value.token.trim(),
                displayLabel: draft.value.label.trim(),
                currency: draft.value.currency.trim().toUpperCase(),
                country: draft.value.country.trim().toUpperCase(),
                // Minted at confirmation, so the retry after a step-up
                // registers this destination once rather than twice.
                idempotencyKey: newIdempotencyKey(),
              },
              {
                onSuccess: () => {
                  draft.clear();
                  setAdding(false);
                },
              },
            );
          }}
        >
          <FormField label="What kind of destination">
            {(field) => (
              <select
                className="input"
                value={draft.value.kind}
                onChange={(event) => {
                  draft.set({ ...draft.value, kind: kindOf(event.target.value) });
                }}
                {...field}
              >
                {KINDS.map((kind) => (
                  <option key={kind} value={kind}>
                    {KIND_LABELS[kind] ?? kind}
                  </option>
                ))}
              </select>
            )}
          </FormField>
          <FormField
            label="The provider's token"
            hint="On a rehearsal deployment this is a sandbox handle. It is never an account number, a card number, an IBAN, a key or a seed phrase — the backend refuses one of those rather than storing it."
          >
            {(field) => (
              <input
                className="input"
                type="text"
                maxLength={255}
                autoComplete="off"
                value={draft.value.token}
                onChange={(event) => {
                  draft.set({ ...draft.value, token: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField label="A name for it" hint="Only so you recognise it in this list.">
            {(field) => (
              <input
                className="input"
                type="text"
                maxLength={64}
                value={draft.value.label}
                onChange={(event) => {
                  draft.set({ ...draft.value, label: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField label="Currency" hint="Three letters, if the provider needs one.">
            {(field) => (
              <input
                className="input"
                type="text"
                maxLength={3}
                value={draft.value.currency}
                onChange={(event) => {
                  draft.set({ ...draft.value, currency: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField label="Country" hint="Two letters, if the provider needs one.">
            {(field) => (
              <input
                className="input"
                type="text"
                maxLength={2}
                value={draft.value.country}
                onChange={(event) => {
                  draft.set({ ...draft.value, country: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>

          {add.isError && <Explanation error={add.error} onRetry={add.reset} />}

          <div className="form-actions">
            {tokenOk ? (
              <Button variant="primary" submit busy={add.isPending} busyLabel="Registering…">
                Register this destination
              </Button>
            ) : (
              <Button disabledReason="Paste the provider's token for the destination. Nodal has nowhere to put an account number.">
                Register this destination
              </Button>
            )}
            <Button
              variant="quiet"
              onClick={() => {
                setAdding(false);
              }}
            >
              Cancel
            </Button>
          </div>
        </form>
      ) : (
        <div className="form-actions">
          <Button
            variant="secondary"
            onClick={() => {
              setAdding(true);
            }}
          >
            Add a destination
          </Button>
        </div>
      )}

      <Disclosure title="Why there is no account-number field">
        <p>
          Holding an account number would make Nodal responsible for keeping one, and it is not the
          party that moves the money. A licensed provider holds the destination and gives Nodal a
          token for it; a token is useless to anybody who steals it from here, because it only
          means anything to the provider that issued it.
        </p>
        <p>
          Adding or removing a destination needs a recent strong sign-in. If the backend asks for
          one, what you have typed is kept, the trip carries this page as its return path, and you
          come back to it.
        </p>
      </Disclosure>
    </Panel>
  );
}
