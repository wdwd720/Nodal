# ADR-0022 — One identity source of truth: ZITADEL for authentication, Neon for the Nodal user

Status: **Accepted** (2026-09-10)

Supersedes nothing. Constrains the productization work that adds signup,
profile, settings and session surfaces: they are built on the identity provider
and the user model that exist, and no second authentication system is added.

## Context

The product goal raises Supabase as a possible home for user accounts and
asks for an explicit decision rather than an assumption. The system today:

- **Authentication** is OIDC against ZITADEL (`CP_AUTH_MODE=oidc`,
  `CP_AUTH_ISSUER=https://nodal-az1hxe.us1.zitadel.cloud`), verified live
  (B-13 resolved: `GET /v1/auth/login` on the deployed API redirects to the
  live issuer with an S256 PKCE challenge and a nonce).
- **The Nodal user** is a row in Neon: `users` keyed by
  `UNIQUE (idp_issuer, idp_subject)` with an `email_hash` for lookup;
  `identity_pii` holds the verified address sealed under `internal/pii`
  (ADR-0021); `sessions` are Nodal's own opaque tokens stored hashed
  (`internal/auth/pgstore`), rotated for break-glass, purged by the ops role.
  Every financial table references `users.id` / `accounts.id`, never the
  provider subject.
- **Step-up** is the control that activates money movement. `RequireStepUp`
  fails closed unless the ID token carries `auth_time` and an `amr` value in
  `security.HasStrongAMR`'s set (`mfa`, `otp`, `hwk`, `swk`, `pop`, `webauthn`,
  `passkey`), and `oidc.New` refuses an issuer that does not advertise PKCE
  S256 or that signs with HS256. `docs/operations/IDENTITY_PROVIDER.md`
  records that these three requirements — a strong `amr`, `auth_time`,
  `email_verified` — are the ones that eliminate candidates: Cognito fell on
  `amr`, Auth0 fell on MFA being a paid feature (a correctness failure, not a
  cost one: a provider that stops emitting a strong `amr` makes
  `CREDIT_PURCHASE` permanently impossible to activate). ZITADEL emits all
  three and its free tier includes TOTP and WebAuthn at 100 daily active users,
  above the 50-account cohort ceiling.

## Options considered

| | Option | One source of truth | Step-up requirements 6–8 | $0, no card | Migration | Verdict |
|---|---|---|---|---|---|---|
| A | **ZITADEL + Neon** (as built) | Yes: ZITADEL authenticates, Neon owns the user, the session and every financial reference | Met, verified live | Yes, both | None | **Chosen** |
| B | Supabase Auth + Neon | No: two user directories, two subjects per person, account linking to invent | Not met as the code stands — Supabase Auth is a JWT-issuing auth service for its own SDK, not an OIDC provider Nodal's relying-party flow can point at; its `amr` is a list of `{method, timestamp}` objects, not RFC 8176 strings, and `auth_time` is not a claim it emits, so `HasStrongAMR` and `RequireStepUp` would have to be rewritten to trust a different shape | Free tier exists | Rewrite `internal/auth/oidc`, the login service, the session model; re-verify every step-up test | Rejected |
| C | Supabase Auth + Supabase Postgres | No, and it moves the authoritative ledger off Neon | As B | Free tier pauses an idle project; the ledger cannot be paused | Migrate 86 migrations, four roles, every trigger; re-run the restore drill against a different host | Rejected |
| D | ZITADEL + Supabase for non-auth services | Yes for identity, but a second Postgres for "services" splits state | Unaffected | Free tier exists | Adds a dependency for capabilities Nodal already has (SSE in `internal/stream`, evidence in S3-compatible archive) or deliberately does not want (Nodal stores no user documents; KYC capture is provider-hosted by ADR-0021 and §20 of the goal) | Rejected: no capability Nodal genuinely needs |
| E | Another architecture | — | — | — | — | None is clearly superior; the built one meets every criterion the goal lists |

The Supabase facts above are from its public documentation as understood on
2026-09-10. They are not the load-bearing part of the decision and were not
re-verified in a browser for this ADR; the load-bearing part is that Nodal
already has an OIDC-certified provider that meets its three eliminating
requirements at no fixed cost, and that any second authentication system —
whatever its claims look like — creates the ambiguous authority §3 of the goal
forbids. If any Supabase detail here is wrong, the verdict does not change.

## Decision

1. **ZITADEL remains the only authenticator.** Signup is "create a ZITADEL
   identity, then a Nodal profile in Neon" — never a second authentication
   account. Registration, e-mail verification, MFA enrolment and passkeys are
   the identity provider's (EB-017), reached through ZITADEL's hosted login
   from `/v1/auth/login`.
2. **Neon owns the Nodal user.** The product surfaces add a `user_profiles`
   table (display name, preferences, product status, onboarding state) keyed
   by `users.id`, separate from `users` (identity), `identity_pii`
   (sealed personal data), `compliance_profiles` / a financial profile
   (eligibility state) and a payout identity (provider-specific), per §4 of
   the goal. None of these grants a financial privilege by existing.
3. **Sessions stay Nodal's.** The browser holds Nodal's `__Host-` session
   cookie issued by `internal/identity`, not a provider token. Session
   restoration, revocation, "recent sessions" and MFA status are read from
   `sessions` and the ID token's `amr`/`auth_time` that the session recorded.
4. **Duplicate-identity protection is the `(idp_issuer, idp_subject)`
   uniqueness plus the e-mail lookup hash.** A second ZITADEL identity for the
   same person is F-64/F-93's identity-model question and stays recorded there;
   this ADR does not pretend two subjects are one person and does not invent
   account linking.
5. **No ambiguous authority.** If a future decision selects a different
   provider, it is a new ADR that supersedes this one and a migration of the
   `users` rows' issuer, not a parallel directory.

## Consequences

- The web app authenticates by redirecting to `/v1/auth/login` and calling the
  API with credentials; it never handles a password, a token or a provider
  secret. Cross-origin cookie, CORS and return-to allowlists are configuration
  on the API, decided in the deployment work.
- Account deactivation is a Nodal `users.status` transition plus session
  revocation; the ZITADEL identity is deactivated by the operator in ZITADEL,
  and a deactivated identity cannot log in because the provider refuses it
  before Nodal is reached.
- Two identity-shaped items stay external: MFA enrolment UX lives in ZITADEL's
  hosted pages, and the identity-verification (KYC) provider is B-06.

## Evidence

`internal/auth/oidc` (`DefaultSigningAlgs`, PKCE S256, nonce, `auth_time`),
`internal/security` (`HasStrongAMR`, `RequireStepUp`), `internal/identity`
(`Begin`/`Complete`/`Logout`, `TestIntegration_Login_FirstLoginCreatesUserAccountSession`,
`TestIntegration_Login_StoresTheVerifiedEmailEncrypted`),
`internal/auth/pgstore` (sessions), `docs/operations/IDENTITY_PROVIDER.md`,
`docs/operations/LAUNCH_TIER.md` §4, the live redirect recorded in
`docs/audit/FINAL_CHECKPOINT_2026-09-10.md` §6.
