/**
 * SETTINGS (goal §37, USER_JOURNEY §9).
 *
 * What a customer can change about themselves, what they have agreed to, and
 * where the two heavier surfaces are. Security and account standing are their
 * own pages rather than panels here, because both hold irreversible controls
 * and a page a person arrived at on purpose is a better place for those than a
 * panel they scrolled past.
 *
 * Two things this page is careful about.
 *
 * # The profile is product state, not identity
 *
 * `UserProfile` carries a display name, a handle, a locale, a time zone and a
 * seed for an identicon. It carries no e-mail address, no phone number, no
 * legal name and no date of birth, because those are sealed in the identity
 * store and this surface has no route to them. So the page does not show an
 * "e-mail" row with a dash in it: a field that does not exist here is not a
 * field that is empty.
 *
 * # A handle change can need a stronger sign-in
 *
 * `POST /v1/me/profile` may answer `STEP_UP_REQUIRED`. The form keeps what was
 * typed through `useSurvivesSignIn`, so the round trip through the identity
 * provider — which destroys every value in the tab — comes back to this page
 * with the same draft, and the idempotency key minted at save comes back with
 * it so the retry is the same request rather than a second one.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useTermsState,
  useUpdateProfile,
  type Principal,
  type TermsState,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { Field, FieldGrid, FormField } from "../../components/Field.tsx";
import { Identifier, Page, Panel, PanelCard } from "../../components/Layout.tsx";
import { Refused } from "../../components/Refused.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { useToast } from "../../components/Toast.tsx";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { formatInstant } from "../../lib/time.ts";
import { useSession } from "../../session.tsx";

interface Draft {
  readonly displayName: string;
  readonly handle: string;
  readonly locale: string;
  readonly timeZone: string;
}

export function Settings(): ReactNode {
  const session = useSession();
  const principal = session.principal;

  return (
    <Page
      title="Settings and security"
      lead="What Nodal holds about you at the product level, what you have agreed to, and where the rest of it is."
    >
      {principal === undefined ? (
        <Panel title="Profile">
          <Skeleton shape="text" count={4} label="Your profile is loading" />
        </Panel>
      ) : (
        <Profile principal={principal} />
      )}

      <Terms />

      <Panel
        title="The rest of your settings"
        description="Each of these has its own page because each of them holds something that is hard to undo."
      >
        <div className="form-actions">
          <LinkButton to="/settings/security" variant="secondary">
            Security and sessions
          </LinkButton>
          <LinkButton to="/settings/account" variant="secondary">
            Account standing and closure
          </LinkButton>
          <LinkButton to="/notifications" variant="secondary">
            Notification preferences
          </LinkButton>
        </div>
        <p className="field-note">
          Notification preferences live with the notifications themselves, because the only thing
          they change is what appears there. In-app is the only delivery channel that exists.
        </p>
      </Panel>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

function Profile(props: { readonly principal: Principal }): ReactNode {
  const profile = props.principal.profile;
  const onboarding = props.principal.onboarding;
  const update = useUpdateProfile();
  const toast = useToast();

  const draft = useSurvivesSignIn<Draft>("settings.profile", {
    displayName: profile?.display_name ?? "",
    handle: profile?.handle ?? "",
    locale: profile?.locale ?? "",
    timeZone: profile?.time_zone ?? "",
  });
  // Minted when the customer saves, kept across a step-up round trip, so the
  // retry after re-authentication is the same request.
  const key = useSurvivesSignIn<string>("settings.profile.key", "");
  const [editing, setEditing] = useState(false);

  if (profile === undefined) {
    return (
      <Panel title="Profile">
        <EmptyState
          title="No profile has been created yet"
          body="The backend answered with a principal that has no profile row. Onboarding is where one is made; nothing here is broken."
          action={
            <LinkButton to="/welcome" variant="primary">
              Finish setting up
            </LinkButton>
          }
        />
      </Panel>
    );
  }

  const save = (): void => {
    const existing = key.value === "" ? newIdempotencyKey() : key.value;
    key.set(existing);
    update.mutate(
      {
        displayName: draft.value.displayName,
        handle: draft.value.handle,
        locale: draft.value.locale,
        timeZone: draft.value.timeZone,
        idempotencyKey: existing,
      },
      {
        onSuccess: () => {
          key.set("");
          draft.clear();
          setEditing(false);
          toast.notify("Profile saved.");
        },
      },
    );
  };

  return (
    <Panel
      title="Profile"
      description="Product-level state only. Your e-mail address, legal name and date of birth are sealed in the identity store and are not reachable from here."
      actions={
        editing ? undefined : (
          <Button
            variant="secondary"
            onClick={() => {
              setEditing(true);
            }}
          >
            Edit
          </Button>
        )
      }
    >
      {update.isError && (
        <Refused
          what="Saving your profile"
          error={update.error}
          onRetry={() => {
            update.reset();
          }}
        >
          <p className="refusal-body">
            What you typed is kept. Coming back from a stronger sign-in returns to this page with
            the same draft, and the save is retried as the same request rather than a second one.
          </p>
        </Refused>
      )}

      {editing ? (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            save();
          }}
        >
          <FormField label="Display name" hint="What other people see. Up to 64 characters.">
            {(field) => (
              <input
                className="input"
                value={draft.value.displayName}
                maxLength={64}
                onChange={(event) => {
                  draft.set({ ...draft.value, displayName: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField
            label="Handle"
            hint="Lower-case letters, digits and underscores, starting with a letter. Leave it empty to clear it. Changing it can ask for a stronger sign-in."
          >
            {(field) => (
              <input
                className="input"
                value={draft.value.handle}
                maxLength={30}
                onChange={(event) => {
                  draft.set({ ...draft.value, handle: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField label="Locale" hint="A language tag such as en or en-GB.">
            {(field) => (
              <input
                className="input"
                value={draft.value.locale}
                maxLength={5}
                onChange={(event) => {
                  draft.set({ ...draft.value, locale: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField label="Time zone" hint="An IANA name such as Europe/London. Every instant in the product is shown in UTC regardless.">
            {(field) => (
              <input
                className="input"
                value={draft.value.timeZone}
                maxLength={64}
                onChange={(event) => {
                  draft.set({ ...draft.value, timeZone: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <div className="form-actions">
            <Button variant="primary" submit busy={update.isPending} busyLabel="Saving…">
              Save profile
            </Button>
            <Button
              variant="quiet"
              onClick={() => {
                setEditing(false);
                update.reset();
              }}
            >
              Cancel
            </Button>
          </div>
        </form>
      ) : (
        <FieldGrid columns={2}>
          <Field label="Display name">
            <span>{profile.display_name ?? "not set"}</span>
          </Field>
          <Field label="Handle">
            <span>{profile.handle ?? "not set"}</span>
          </Field>
          <Field label="Locale">
            <span>{profile.locale}</span>
          </Field>
          <Field label="Time zone" note="Instants in this product are shown in UTC whatever this says.">
            <span>{profile.time_zone}</span>
          </Field>
          <Field label="Profile created">
            <span>{formatInstant(profile.created_at)}</span>
          </Field>
          <Field label="Your identifier" note="Quote this to support; it names you and nothing else.">
            <Identifier value={profile.user_id} label="user" />
          </Field>
        </FieldGrid>
      )}

      {onboarding !== undefined && (
        <p className="field-note">
          Onboarding {onboarding.complete ? "is complete" : "is not complete"}
          {onboarding.next_step === undefined ? "" : `; the next step is ${onboarding.next_step}`}.
        </p>
      )}
    </Panel>
  );
}

/* -------------------------------------------------------------------------- */

function Terms(): ReactNode {
  const terms = useTermsState();

  return (
    <Panel
      title="What you have agreed to"
      description="Each document, the version you accepted, and the hash of the exact bytes that were shown to you."
    >
      <AsyncPanel
        query={terms}
        loadingLabel="Asking the backend which documents you have accepted…"
        skeleton={<Skeleton shape="rows" count={3} label="Your acceptances are loading" />}
      >
        {(state: TermsState) => (
          <div className="stack">
            {state.outstanding.length > 0 && (
              <EmptyState
                title="Some documents are waiting for you"
                body={`The current version of ${state.outstanding.join(", ")} has not been accepted. Anything financial routes to the terms step until it is.`}
                action={
                  <LinkButton to="/welcome/terms" variant="primary">
                    Read and accept
                  </LinkButton>
                }
              />
            )}
            {state.documents.map((document) => (
              <PanelCard
                key={document.document_id}
                title={document.title}
                description={`Version ${document.version}, required at ${document.requirement.toLowerCase()}.`}
                actions={
                  <StatusBadge tone={document.accepted ? "good" : "warn"}>
                    {document.accepted ? "Accepted" : "Not accepted"}
                  </StatusBadge>
                }
              >
                <FieldGrid columns={2}>
                  <Field label="Accepted">
                    <span>
                      {document.accepted_at === undefined
                        ? "not yet"
                        : formatInstant(document.accepted_at)}
                    </span>
                  </Field>
                  <Field label="Bytes you accepted" note="The sha256 of the exact document text.">
                    <Identifier value={document.content_hash} label="hash" />
                  </Field>
                </FieldGrid>
                {document.counsel_review_required && (
                  <p className="field-note">
                    This document has not been reviewed by a lawyer. It is served as written and is
                    marked so rather than presented as settled.
                  </p>
                )}
              </PanelCard>
            ))}
          </div>
        )}
      </AsyncPanel>
    </Panel>
  );
}
