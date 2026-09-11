/**
 * privacy-v1 — what the product stores, where it stores it, and what it never
 * has.
 *
 * Every claim here maps to something in the repository: `internal/pii` (the
 * AES-256-GCM sealing of e-mail, legal name and date of birth, under a keyring
 * the database never holds), `internal/auth` and `internal/identity` (sessions,
 * login attempts, the IP and user agent recorded with them), `internal/audit`
 * (the append-only trail a customer can read back), and the provider packages
 * (what is handed out and what comes back).
 *
 * No rights are described that the software cannot honour today, and no
 * jurisdiction is named, because naming one implies a legal analysis nobody has
 * performed.
 */
import type { PolicyDocument } from "./types.ts";

export const PRIVACY_V1: PolicyDocument = {
  id: "privacy-v1",
  slug: "privacy",
  title: "Privacy",
  summary:
    "The personal data the product holds, how it is sealed, who it is shared with, and what the " +
    "product deliberately never has.",
  drafted: "2026-09-10",
  sections: [
    {
      heading: "1. What Nodal never has",
      paragraphs: [
        "It is easier to start with the absences, because they are the strongest part of the " +
          "design and they are structural rather than a matter of policy.",
      ],
      bullets: [
        "Your password. You sign in through an identity provider; the credential is entered on the " +
          "provider's pages and Nodal never receives it.",
        "Your card number. Card details are entered on the payment provider's own form. Nodal " +
          "receives the provider's identifier for the payment and its outcome, nothing more.",
        "Your identity documents. Verification happens on the verification provider's pages. Nodal " +
          "receives the outcome and the kinds of check that were performed, not the document.",
        "Your bank or payout account details. A payout destination is stored as a provider token " +
          "and shown to you masked.",
      ],
    },
    {
      heading: "2. What Nodal does hold",
      paragraphs: [
        "The product stores as little as it can and seals what it must:",
      ],
      bullets: [
        "Identity data — e-mail address, legal name and date of birth, when a flow requires them. " +
          "Each value is encrypted with AES-256-GCM before it reaches the database, under a key " +
          "the database never holds, and bound to the row and column it belongs to so a ciphertext " +
          "cannot be moved to another account.",
        "Your profile — the display name, handle, locale and timezone you choose. This is the part " +
          "you control, and it holds only what you put in it.",
        "Sessions — one row per signed-in session with the time it was created, the address it was " +
          "created from and the browser it reported. You can list your sessions and revoke any of " +
          "them.",
        "Financial records — Credit ledger entries, orders, positions, payments, payout requests, " +
          "and their state history. These are append-only: a correction is a new entry, never an " +
          "edit.",
        "An audit trail of security-relevant actions on your account, which you can read back.",
      ],
    },
    {
      heading: "3. Why the product holds it",
      paragraphs: [
        "Identity data exists to satisfy the checks a payout path requires, and it is asked for at " +
          "the point that path is used rather than at sign-up. Financial records exist because a " +
          "ledger that cannot be reconstructed is not a ledger. Session and audit records exist so " +
          "that you and the operators can see what happened on an account.",
        "None of it is used to profile you for advertising, none of it is sold, and none of it is " +
          "shared with anyone except the providers named below.",
      ],
    },
    {
      heading: "4. Who else sees it",
      paragraphs: [
        "Four kinds of third party are involved, each behind a contract and a capability gate, and " +
          "each receives only what its job needs:",
      ],
      bullets: [
        "The identity provider authenticates you and tells Nodal which subject signed in.",
        "The payment provider takes card details and reports payment outcomes.",
        "The verification provider performs identity, age, jurisdiction and sanctions checks and " +
          "reports the outcome.",
        "The conversion and payout provider, where one is configured, receives what it needs to " +
          "settle an approved payout.",
        "Infrastructure providers host the database and the service itself.",
      ],
    },
    {
      heading: "5. Cookies and what the browser stores",
      paragraphs: [
        "One cookie matters: the session cookie the backend sets after you sign in. It is " +
          "HTTP-only, same-site and scoped to the host, so no script can read it and it is not " +
          "sent to other sites. There are no advertising or analytics cookies in the product.",
        "The application keeps two things in your browser's per-tab storage: the page you were " +
          "trying to reach when a session expired, and anything you had typed into a form at that " +
          "moment, so that signing in again returns you to the same place with your work intact. " +
          "Both are cleared when they are used, when they expire, and when the tab closes, and " +
          "neither is ever sent anywhere.",
      ],
    },
    {
      heading: "6. How long it is kept",
      paragraphs: [
        "Financial and audit records are append-only and are retained: a ledger that forgets is " +
          "not one, and the records exist partly to protect you. Sessions expire and revoked ones " +
          "stop working immediately. Sealed identity data is kept while the account exists and " +
          "while any obligation attached to a payout path requires it.",
        "You can ask to close your account from the account settings page. Closure has a " +
          "cooling-off period, and it does not erase ledger history, because deleting a financial " +
          "record would be the dishonest option, not the private one.",
      ],
    },
    {
      heading: "7. Getting your data",
      paragraphs: [
        "The application shows you what it holds: your profile, your sessions, your own audit " +
          "trail, and an export of your account records. Anything the interface shows you, you can " +
          "take with you.",
        "This document does not claim a statutory right on your behalf, because the product has " +
          "not been assessed against any particular regime and saying otherwise would be a claim " +
          "about the law rather than about the software.",
      ],
    },
    {
      heading: "8. Security, stated plainly",
      paragraphs: [
        "Personal data is sealed before it is written. Authorisation is deny-by-default per " +
          "operation. State changes go through transition functions that write their own history, " +
          "and the application role cannot rewrite a state column. Secrets live only in the " +
          "deployment's secret store and never in the repository.",
        "No system is beyond compromise, and this document does not claim otherwise. What it " +
          "claims is that the design assumes a breach is possible and puts the key somewhere the " +
          "database is not.",
      ],
    },
    {
      heading: "9. Limits of this document",
      paragraphs: [
        "This is a draft, written by the team that built the product. No lawyer has reviewed it, " +
          "no jurisdiction is named, and no statutory rights are asserted or waived. It describes " +
          "the software as it is.",
      ],
    },
  ],
};
