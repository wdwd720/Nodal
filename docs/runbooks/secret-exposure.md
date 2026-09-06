# Runbook: secret exposure

Severity: SEV1 for signing/wallet-provider credentials, database credentials, KMS audit-signing key access, webhook secrets that can credit; SEV2 for read-only or rate-limited keys · Owner: SECURITY · Related: [wallet-provider-compromise.md](./wallet-provider-compromise.md), [funding-provider-compromise.md](./funding-provider-compromise.md), [admin-compromise.md](./admin-compromise.md), [database-corruption.md](./database-corruption.md), `docs/security/SECURITY.md` §8, §14

## Trigger

- `make secrets` (gitleaks) or the CI `security-scans` job reports a finding; a secret pasted in a ticket, chat, log line, screenshot, or a public repository.
- A log line that should have been redacted (the denylist in `observability.deniedKeys` covers `private_key`, `seed`, `mnemonic`, `secret`, `token`, `api_key`, `webhook_secret`, `signing_token`, PEM blocks, `Bearer` values, 87–88-char base58 strings; a struct dump bypasses it by design).
- Provider notice of credential misuse, `provider_credential_change` (PENDING emitter), unexpected usage/billing on a provider key.
- A Helius URL shared with its API key (the key travels in the query string on every host; `docs/api/providers/helius.md`).
- Compromised developer or operator machine holding `.env`/`file://` secrets (LOCAL/DEV only by validation rule `SECRET_REF_SCHEME`; plain values are rejected outside LOCAL/TEST).
- PENDING: the `aws-sm://` resolver, IAM task roles and Terraform secrets module wiring (EB-012); CI has never run (SB-004), so gitleaks has never scanned history in CI.

## Blast radius

By secret class:

| Secret | Holder (design, SYSTEM.md §2) | If exposed |
|---|---|---|
| wallet/signing provider credential (bounded signer) | `cmd/execution-worker` only | arbitrary signing requests to the provider: SEV1, run [wallet-provider-compromise.md](./wallet-provider-compromise.md) |
| database credentials (`cp_app`, `cp_migrate`, `cp_readonly`, `cp_ops`) | per binary / migrations | `cp_app` can read everything and write state through triggers; `cp_migrate` can bypass triggers: SEV1, run [database-corruption.md](./database-corruption.md) diagnosis |
| funding webhook secret / provider API key | `cmd/api` | forged status events (cannot credit without chain receipt) and session creation: run [funding-provider-compromise.md](./funding-provider-compromise.md) |
| execution provider key (Jupiter `x-api-key`) | `cmd/execution-worker` | quota abuse; no signing authority: SEV2 |
| chain/market data keys (Helius, RPC) | ingest/execution/reconciliation workers | quota abuse, data poisoning attempts: SEV2; observation uses two vendors |
| model provider key | `cmd/agent-worker` via ToolBroker | billable usage; no money authority: SEV2 |
| KMS audit signing key permission | `cmd/audit-worker` | forged Merkle signatures: SEV1 for evidence integrity |
| OIDC client secret | `cmd/api` | token exchange on our behalf: SEV1 for identity, bounded by PKCE/nonce/step-up |
| session cookie | a customer/operator | that principal's session until revoked: [admin-compromise.md](./admin-compromise.md) if an operator |

Nothing in this table can enable live money by itself: a capability gate needs a persisted dual-approved row (POLICY_AUTHORITY.md §1), and no kill switch, reconciliation, settlement or ledger path stops because a secret rotates.

## Immediate actions (first 10 minutes)

1. Classify the secret using the table; for SEV1 classes activate the containing switch now: signing ⇒ `GLOBAL_NEW_RISK_KILL` + `PROVIDER_DISABLE_NEW_ACTIONS(<wallet provider>)`; funding ⇒ `FUNDING_DISABLE(*)`; execution ⇒ `PROVIDER_DISABLE_NEW_ACTIONS(jupiter)`; database ⇒ `GLOBAL_NEW_RISK_KILL`. All `kill:activate`, `POST /admin/kill-switches` (PENDING `cmd/api`).
2. Rotate at the source of truth first (provider console, RDS/Secrets Manager, IdP), then update the `SecretRef` target (`aws-sm://name` — PENDING resolver; `env://NAME` in the task definition), then restart the holding binaries. Never edit images, never commit values; `Config.Hash` excludes secret values so `/version` will not change (`TestHash_StableAndExcludesSecrets`).
3. Revoke the old credential explicitly (deleting a Secrets Manager version does not revoke a provider key).
4. Identify where the value travelled: git history (`make secrets` over the full history; `.gitleaks.toml` allow-lists only `cp_*_local`), logs (search the log store for the value's prefix; the redaction denylist is enforced in `NewLogger`, so a hit means a struct dump or a non-logger sink), tickets/chat, provider dashboards, CI artifacts.
5. For operator/customer sessions: `auth.Manager.RevokeAllForSubject` (`session:revoke_any`).
6. Announce the class, switches activated, and the rotation status; open the provider ticket.

## Diagnosis

- Provider-side usage logs for the exposure window: source IPs, request counts, endpoints; compare with our egress. Signing provider: every request must correspond to a `signing_decisions` row with `provider_sign_ref`.
- Database: `pg_stat_activity` history and RDS logs for the role; `pg_stat_user_tables` mutation counters on journal tables; audit chain verification (`make verify-audit`, PENDING `cmd/audit-worker`; `audit.Verifier.VerifyStream`).
- Gates/kill switches/admin actions in the window (a leaked `cp_app` credential could attempt bare state changes; migration 00603 refuses them with `AU001`, so failures are visible in DB logs).
- How it leaked: `file://` or plain secrets outside LOCAL/TEST are rejected by `config.Validate`, so a production leak implies the environment store, a developer machine with production access, or a provider dashboard.

## Containment and recovery

1. Complete rotation for every binary that held the secret (SYSTEM.md §2 table: one task role per binary; PENDING Terraform).
2. Run the class-specific runbook to the end: chain scan for signing, webhook/deposit invariants for funding, reconciliation for database.
3. If git history contains the secret: rewrite is optional (the credential is already revoked), but the finding stays in the incident record; add the pattern to gitleaks if it was missed.
4. If logs contained it: purge per retention policy (`Retention.OperationalLogDays`), fix the logging site to log through attributes (`observability.Secret` never renders), add a redaction test.
5. Release switches via their paths (SEVERE ones through the dual-controlled `KILL_SWITCH_RELEASE`), attaching the rotation log and provider usage review.

## What NOT to do

- Never rotate by committing a new value, baking it into an image, or pasting it in the ticket that reports the leak.
- Never keep the old credential "for rollback".
- Never disable redaction, the `NO_FAKE_PROVIDERS`/`SECRET_REF_SCHEME` validation rules, or TLS requirements to ease rotation.
- Never assume a leaked read-only key is harmless without checking the provider's actual scope (some keys are read/write by default).
- Never let the migrate role's credential be used from anywhere but the migration job.

## Verification / exit criteria

- Old credential confirmed revoked at the provider/store; new credential in use by every holder (restart timestamps, `/version` config hash unchanged, `/readyz` green — PENDING `cmd/api`).
- Provider usage after revocation shows zero requests with the old credential.
- Class-specific runbook exit criteria met (chain scan clean, deposit invariants hold, audit chain verifies).
- Log store and repositories searched; findings recorded; gitleaks pattern updated if needed.

## Post-incident

- Archive the rotation log (who, when, which stores), provider usage export, gitleaks output and the log search results.
- Security review: THREAT_MODEL.md §3.8 rows; implement the `provider_credential_change` emitter (R-130-1); connect the git remote so the `security-scans` job runs (SB-004).
- Update `docs/build/BLOCKERS.md` (EB-012 for the `aws-sm://` resolver) and `docs/build/REQUIREMENTS_TRACEABILITY.md` R-099-1, R-104-1.
