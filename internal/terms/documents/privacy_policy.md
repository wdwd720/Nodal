# Nodal Privacy Notice

**Status: draft, pending review by qualified counsel. This document has not been
reviewed by a lawyer and is not a final privacy notice.**

This draft describes what the system actually stores, because a privacy notice
that does not match the schema is worthless. Each claim below corresponds to
something a reviewer can check in the code.

## What Nodal collects

- **From the identity provider:** your subject identifier, and your e-mail
  address when the provider has verified it. Nodal keeps a one-way hash of the
  address for lookup, and the address itself encrypted under a key the database
  does not hold.
- **From you:** a display name, an optional handle, a locale and a time zone, if
  you choose to set them. Nodal accepts no avatar upload; your avatar is drawn
  from a random seed.
- **From your use of the product:** sessions (device, address, and when they
  were last seen), security events such as logins and session revocations, and
  an append-only audit record of consequential actions.

## What Nodal does not collect

Nodal stores no identity documents, no payment card details, and no government
identifier. Where identity verification is required, it is performed by a
provider on its own pages and Nodal records the provider's decision, not the
documents behind it.

## Who can read what

Personal data is encrypted in the application. Database roles that have no use
for it cannot read it, and this is enforced by grants rather than by policy.

## Retention

Financial and audit records are append-only and are retained; they are not
deleted when an account is closed. Sessions and login attempts are removed on a
schedule. Security events — the record of sign-ins and session revocations — are
retained until a retention period has been chosen for them, and none has been
chosen yet, so nothing removes them today.

## What this draft does not yet say

The lawful bases for processing, the international transfer mechanism, the
retention periods expressed as commitments rather than as current behaviour, and
the rights and contact routes required in each jurisdiction. Those need counsel.
