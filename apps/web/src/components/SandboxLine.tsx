/**
 * The standing sandbox statement, on both shells.
 *
 * `GET /v1/version` says whether this deployment is a sandbox tier; the client
 * never infers it from the environment name, because a build that guessed would
 * label the wrong deployment — and the only thing worse than an unlabelled
 * rehearsal is a real deployment labelled as one. An absent flag means the API
 * did not say, and that is not "sandbox" either.
 *
 * It is part of the document rather than a dismissible banner. A rehearsal a
 * customer can dismiss is a rehearsal they will forget they are in.
 *
 * It lives here rather than inside `AppShell` because the public site needs it
 * too, and needs it more: `USER_JOURNEY.md` §0 promises the label "on the
 * landing page", and the landing page is where somebody decides whether this is
 * a real product before they have a session to be told anything else. The
 * public shell read `/v1/version` for the build tag in its footer and discarded
 * `sandbox_tier` — so every word of the marketing site read as a live product
 * on a deployment where nothing moves value.
 */
import type { ReactNode } from "react";

export function SandboxLine(props: { readonly sandbox: boolean | undefined }): ReactNode {
  if (props.sandbox !== true) return null;
  return (
    <p className="sandbox-line" role="note">
      <span className="sandbox-word">Sandbox</span>
      <span>
        Credits, verification and payouts here are rehearsals; nothing moves real value.
      </span>
    </p>
  );
}
