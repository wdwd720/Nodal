# IDENTITY PROVIDER REQUIREMENTS AND RECOMMENDATION

What this codebase actually demands of an OIDC provider, established by reading
`internal/auth/oidc` and `internal/security`, and which providers meet it.

**Date:** 2026-09-09.

---

## 1. Why this is on the critical path

`config.Validate` refuses `CP_AUTH_MODE=dev` in STAGING and PROD, and
`internal/auth/devidp` serves LOCAL and TEST only. A deployed environment with
no issuer has no way for anyone to sign in at all. There is no fallback and no
degraded mode.

## 2. The requirements, with the code that imposes each

| # | Requirement | Imposed by | Consequence if unmet |
|---|---|---|---|
| 1 | OIDC discovery at `<issuer>/.well-known/openid-configuration`, echoing the configured issuer exactly | `oidc.New`, via go-oidc, refusing an issuer mismatch | Will not start |
| 2 | Authorization code flow with **PKCE S256** | `AuthCodeURL` always sends `code_challenge` + `S256`; `New` refuses an issuer advertising a method list without S256 | Will not start, or the code is unbound |
| 3 | `nonce` echoed into the ID token | `verifyIDToken` compares it in constant time | Every login fails |
| 4 | Asymmetric ID token signing | `DefaultSigningAlgs`: RS/PS/ES 256/384/512. **HS256 is not accepted** | Will not start |
| 5 | Non-empty `sub` | `checkClaims` | Every login fails |
| 6 | **`email` and `email_verified`** | The verification resolver derives `NODAL_IDENTITY` from a verified email address | See §3 |
| 7 | **`amr` containing a strong method** | `ExchangeStepUp` requires `security.HasStrongAMR`: one of `mfa`, `otp`, `hwk`, `swk`, `pop`, `webauthn`, `passkey`. `pwd`, `sms` and `kba` do not qualify | See §4 |
| 8 | `auth_time` in the ID token | `security.RequireStepUp` fails closed when it is zero | Every step-up action refused |
| 9 | `prompt=login` and `max_age=0` honoured | `AuthCodeURL` sends both on step-up | Step-up silently reuses an old session |
| 10 | `acr_values` overlapping the configured step-up ACR (default `phr`) | `New` refuses a published list that shares nothing with ours | Will not start |
| 11 | HTTPS issuer and endpoints | `checkURL`, unless `AllowInsecureIssuer` | Will not start |

Requirements 6, 7 and 8 are the ones that eliminate candidates. The rest are
met by any conforming OIDC provider.

## 3. Why `email_verified` is load-bearing

`NODAL_IDENTITY` is the verification level every Domain A action requires, and
the deployment derives it from the identity provider asserting a verified email
address. An issuer that does not emit `email_verified` leaves every account at
`NONE`, and the entire internal economy becomes unreachable in every
deployment whatever its gates say.

That is not hypothetical. It is finding F-26, recorded in
`docs/build/BLOCKERS.md` B-06: a control no user can ever satisfy is not a
control.

## 4. Why `amr` decides the choice

Step-up is verified by the `amr` claim and not by `acr`. The comment in
`ExchangeStepUp` is explicit that "acr is recorded but not sufficient on its
own, so the session's AMR remains the single source".

Step-up gates break-glass elevation and the dual-control approval path. Those
are what activate a capability gate. So an issuer that never emits a strong
`amr` makes `CREDIT_PURCHASE` **permanently impossible to activate**, no matter
what Stripe approves or counsel determines.

## 5. Candidates, checked against primary sources

| Provider | `amr` with a strong value | `auth_time` | `email_verified` | Verdict |
|---|---|---|---|---|
| **Auth0** | **Yes.** After an MFA challenge the ID token carries `amr: ["mfa"]` | Yes | Yes | **Recommended** |
| **AWS Cognito** | **No.** The documented ID token payload has no `amr` claim at all | Yes | Yes | **Rejected** — see below |
| Okta | Yes, RFC 8176 values | Yes | Yes | Viable |
| Microsoft Entra ID | Yes, including `mfa` | Yes | Yes | Viable |

### Cognito is rejected, and it is the one that looked simplest

Cognito would have been the obvious choice: already in AWS, no new vendor, no
separate bill. Its documented ID token payload lists `sub`, `email`,
`email_verified`, `nonce`, `auth_time`, `iss`, `aud`, `exp`, `iat`, `jti` and
the `cognito:*` claims. **There is no `amr`.**

A pre-token-generation Lambda can add claims, so this is not strictly
impossible. But the claim it would have to add is an assertion about how the
user authenticated, and a Lambda that emits `"mfa"` because somebody wrote
`"mfa"` in it is a fabricated security assertion. Adding it honestly means
deriving it from the real authentication event, which is work Cognito does not
do for you and which is the whole reason to use an identity provider.

Choosing Cognito and quietly synthesising `amr` would defeat step-up
everywhere it is used, including the approval path for activating live money
movement. That is the wrong place to be clever.

### Auth0 is the recommendation

- It emits `amr: ["mfa"]` after an MFA challenge, which `HasStrongAMR` accepts.
- It emits `auth_time`, `email_verified` and `nonce`.
- It supports PKCE S256, `prompt=login`, `max_age` and `acr_values`.
- The free tier covers early usage, and a tenant is minutes to create.
- Nothing to run: no container, no database, no patching.

One caveat from Auth0's own documentation, which happens not to bite here:
"If the app uses silent authentication or Refresh Tokens for newly issued ID
tokens, the `amr` claim will not be present." Nodal's step-up sends
`prompt=login` with `max_age=0`, which forces a fresh interactive
authentication, so the claim is present exactly when it is needed.

### What to verify once a tenant exists

The tenant's own discovery document answers most of this, and can be checked
before writing any configuration:

```
curl -s https://<tenant>/.well-known/openid-configuration | jq '{
  issuer,
  id_token_signing_alg_values_supported,
  code_challenge_methods_supported,
  acr_values_supported,
  claims_supported
}'
```

`code_challenge_methods_supported` must contain `S256`, and
`acr_values_supported` — if published at all — must overlap the configured
step-up ACR. `oidc.New` refuses on both, so a mismatch is a startup failure
rather than a runtime surprise.

If the tenant publishes no `acr_values_supported`, set `StepUpACRValues` to
whatever value it does use for MFA, or leave the default: silence is not
refused, and `amr` remains the gate either way.

## 6. Configuration this produces

| Variable | Value |
|---|---|
| `CP_AUTH_MODE` | `oidc` |
| `CP_AUTH_ISSUER` | the tenant issuer, https, exactly as discovery echoes it |
| `CP_AUTH_CLIENT_ID` | the application's client id |
| `CP_AUTH_CLIENT_SECRET_REF` | `aws-sm://...`, never a literal |
| `CP_AUTH_REDIRECT_URL` | `https://<api host>/v1/auth/callback` |

The redirect URL depends on the hostname decision, so it is the one value that
waits on `DEPLOYMENT_GAP_ANALYSIS.md` §6.
