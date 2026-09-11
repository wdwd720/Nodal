/**
 * `/welcome` — step one: what Nodal should call you.
 *
 * `docs/product/USER_JOURNEY.md` §1 asks for a display name and an optional
 * handle, with locale and timezone prefilled. The target for the whole journey
 * is sixty to ninety seconds, so this screen asks for four things and three of
 * them are already filled in.
 *
 * What it does NOT ask for is the point of the screen. There is no e-mail
 * address here (the identity provider has it, sealed), no legal name, no date
 * of birth, no country, no financial questionnaire. Goal §6 forbids
 * frontloading KYC and §60 fixes the ordering: identity checks happen when
 * somebody asks for value to leave, not when they arrive.
 *
 * The prefills come from the browser's own `Intl` settings. That is a locale
 * and a timezone, not a number — the source guard's ban on `Intl.NumberFormat`
 * is about money losing exactness, and neither of these is money.
 */
import { useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import { useUpdateProfile } from "../../api/queries.ts";
import { Button } from "../../components/Button.tsx";
import { Explanation } from "../../components/DataState.tsx";
import { FormField } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { useSession } from "../../session.tsx";
import { OnboardingFrame, StepList } from "./OnboardingFrame.tsx";

/** What the browser says, used only as a prefill the customer can change. */
function browserDefaults(): { readonly locale: string; readonly timeZone: string } {
  let locale = "en";
  let timeZone = "UTC";
  try {
    const resolved = Intl.DateTimeFormat().resolvedOptions();
    if (typeof resolved.locale === "string" && resolved.locale !== "") locale = resolved.locale;
    if (typeof resolved.timeZone === "string" && resolved.timeZone !== "") timeZone = resolved.timeZone;
  } catch {
    // A browser that will not resolve its own settings gets the defaults, and
    // the customer can change both.
  }
  return { locale, timeZone };
}

/**
 * The API takes `^[a-z]{2}(-[A-Z]{2})?$`, and a browser can report a longer
 * tag (`en-GB-oxendict`, `zh-Hans-CN`). Trim to the part the contract accepts
 * rather than send something that will be refused: a validation problem on a
 * value the customer never typed is a bad first impression of the product.
 */
function narrowLocale(tag: string): string {
  const parts = tag.split("-");
  const language = (parts[0] ?? "en").toLowerCase();
  const region = parts[1];
  if (region !== undefined && /^[A-Za-z]{2}$/.test(region)) {
    return `${language}-${region.toUpperCase()}`;
  }
  return language;
}

interface Draft {
  readonly displayName: string;
  readonly handle: string;
  readonly locale: string;
  readonly timeZone: string;
}

const HANDLE_PATTERN = /^[a-z][a-z0-9_]{2,29}$/;

export function Welcome(): ReactNode {
  const session = useSession();
  const navigate = useNavigate();
  const update = useUpdateProfile();
  const defaults = browserDefaults();

  const existing = session.profile;
  const kept = useSurvivesSignIn<Draft>("welcome.profile", {
    displayName: existing?.display_name ?? "",
    handle: existing?.handle ?? "",
    locale: narrowLocale(existing?.locale ?? defaults.locale),
    timeZone: existing?.time_zone ?? defaults.timeZone,
  });
  const draft = kept.value;
  const [submitted, setSubmitted] = useState(false);

  const nameProblem =
    draft.displayName.trim().length < 2 ? "A display name needs at least two characters." : undefined;
  const handleProblem =
    draft.handle !== "" && !HANDLE_PATTERN.test(draft.handle)
      ? "A handle is three to thirty characters: lower-case letters, digits and underscores, starting with a letter."
      : undefined;
  const blocked = nameProblem !== undefined || handleProblem !== undefined;

  return (
    <OnboardingFrame
      title="Welcome"
      lead="Two screens, then your dashboard. This one is what Nodal should call you."
      step="PROFILE"
    >
      <StepList current="PROFILE" />

      <Panel
        title="Your profile"
        description="Product state, not identity. Nodal holds no e-mail address, legal name or date of birth here — your identity provider has what it needs, sealed and out of reach of this surface."
      >
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            setSubmitted(true);
            if (blocked) return;
            update.mutate(
              {
                displayName: draft.displayName.trim(),
                // Sent even when empty: an empty handle clears it, which is
                // how somebody removes one they no longer want.
                handle: draft.handle,
                locale: draft.locale,
                timeZone: draft.timeZone,
                // Minted here, at the moment of confirmation, so a retry after
                // a re-authentication updates the profile once.
                idempotencyKey: newIdempotencyKey(),
              },
              {
                onSuccess: () => {
                  kept.clear();
                  void navigate("/welcome/terms");
                },
              },
            );
          }}
        >
          <FormField
            label="Display name"
            hint="What other people see. You can change it whenever you like."
            {...(submitted && nameProblem !== undefined ? { error: nameProblem } : {})}
          >
            {(field) => (
              <input
                className="input"
                value={draft.displayName}
                maxLength={64}
                autoComplete="nickname"
                onChange={(event) => {
                  kept.set({ ...draft, displayName: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>

          <FormField
            label="Handle (optional)"
            hint="Lower-case letters, digits and underscores. Leave it empty if you would rather not have one."
            {...(submitted && handleProblem !== undefined ? { error: handleProblem } : {})}
          >
            {(field) => (
              <input
                className="input"
                value={draft.handle}
                maxLength={30}
                autoComplete="username"
                onChange={(event) => {
                  kept.set({ ...draft, handle: event.target.value.toLowerCase() });
                }}
                {...field}
              />
            )}
          </FormField>

          <div className="form-row">
            <FormField label="Language" hint="Prefilled from your browser.">
              {(field) => (
                <input
                  className="input"
                  value={draft.locale}
                  maxLength={5}
                  onChange={(event) => {
                    kept.set({ ...draft, locale: event.target.value });
                  }}
                  {...field}
                />
              )}
            </FormField>
            <FormField label="Time zone" hint="Every timestamp in the product is also shown in UTC.">
              {(field) => (
                <input
                  className="input"
                  value={draft.timeZone}
                  maxLength={64}
                  onChange={(event) => {
                    kept.set({ ...draft, timeZone: event.target.value });
                  }}
                  {...field}
                />
              )}
            </FormField>
          </div>

          {update.isError && <Explanation error={update.error} />}

          <div className="form-actions">
            <Button variant="primary" submit busy={update.isPending} busyLabel="Saving…">
              Continue
            </Button>
          </div>
        </form>
      </Panel>
    </OnboardingFrame>
  );
}
