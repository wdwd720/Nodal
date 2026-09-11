# Human actions queue

Actions only a person can take, in the order to take them. Each item says
where, what, why, and what it unblocks. Values that are secrets are never in
this file or in chat: the item names the scratchpad file that holds the value.
The scratchpad is `%LOCALAPPDATA%\Temp\claude\C--Dev-Nodal\23d7ee3f-915b-46ea-9d9d-bb6284e9504c\scratchpad\`
(session-specific; if it is gone, item 0 says how to regenerate).

Status legend: **OPEN** (waiting on you), **DONE** (you did it; Claude verified), **SUPERSEDED**.

## 0. If the scratchpad secrets are gone — regenerate them (OPEN, conditional)

Only if `scratchpad\secrets\alert_webhook_url.txt` or `scratchpad\secrets\pii_keyring.json`
no longer exists. Run in Git Bash from `C:\Dev\Nodal` (nothing is printed):

```bash
mkdir -p "$SP/secrets"   # SP = the scratchpad path above, or any private folder
printf 'https://ntfy.sh/nodal-alerts-%s' "$(openssl rand -hex 16 | tr -d '\r\n')" > "$SP/secrets/alert_webhook_url.txt"
printf '{"active":1,"keys":{"1":"%s"}}' "$(openssl rand -base64 32 | tr -d '\r\n')" > "$SP/secrets/pii_keyring.json"   # the shape CP_PII_KEYRING_REF documents: one AES-256-GCM key, base64, 72 bytes of JSON
sha256sum "$SP/secrets/"* | tr -d '\\' > "$SP/secrets/fingerprints.txt"
```

## 1. Render: set the alert webhook secret (OPEN)

- **Where:** https://dashboard.render.com/web/srv-dah02lht0dsc73e1la50/env → **Edit** → **Add variable**
- **Key:** `NODAL_ALERT_WEBHOOK_URL`
- **Value:** the single line in `scratchpad\secrets\alert_webhook_url.txt` (paste it; no quotes, no trailing newline)
- **Why:** STAGING config refuses to boot without an alert destination (`RuleAlertDestination`); the topic is a free ntfy.sh topic, $0.
- **Do not press "Save, rebuild, and deploy" yet** — add item 2 first, then save once.

## 2. Render: set the PII keyring secret (OPEN)

- **Where:** same form, **Add variable**
- **Key:** `NODAL_PII_KEYRING`
- **Value:** the entire contents of `scratchpad\secrets\pii_keyring.json` (single-line JSON, 72 bytes; paste as one line)
- **Why:** STAGING config refuses to boot without a keyring (`RulePIIKeyring`); it seals `identity_pii`.
- Then press **Save, rebuild, and deploy** once. Expect one deploy of the currently live commit; the service stays on the free plan.
- **Verify (Claude does this, or you can):** the value fingerprints are in `scratchpad\secrets\fingerprints.txt`; after the deploy, `GET https://api-nodal.actorvia.xyz/v1/readyz` must be 200.

## 3. Push the audited backend HEAD to main (OPEN, after 1–2)

- **Where:** Git Bash, `C:\Dev\Nodal`
- **What:** `git push origin main` — main is at `9906c9f` (ADR-0022 + D-051 on top of the audited checkpoint `024c691`); nothing else is on it.
- **Why:** the goal's §2 orders the push after the secrets exist, because `9906c9f` validates both at boot and would fail its health check without them (Render would keep the old build live, but the failed deploy is avoidable).
- **Unblocks:** live verification of `/v1/healthz`, `/v1/readyz`, `/v1/version` (build commit `9906c9f`, config hash) — tell Claude "secrets set, main pushed" and it verifies and records the evidence in `docs/audit/PRODUCTION_EVIDENCE_INDEX.md`.
- If the push asks for credentials, sign in to GitHub in the credential prompt (account that owns `wdwd720/Nodal`).

## 4. GoDaddy: CNAME for the web app (OPEN, later — after `nodal-web` exists on Render)

- **When:** only after the `productization` branch has merged to `main` and Render's blueprint sync has created the static site `nodal-web` (Claude will move this item to "ready" and fill in the exact target then; the target is the site's `*.onrender.com` hostname shown on its Settings page).
- **Where:** GoDaddy → DNS for `actorvia.xyz` → **Add record**: Type `CNAME`, Name `app-nodal`, Value `<nodal-web's onrender.com hostname>`, TTL 600.
- **Then:** Render → `nodal-web` → Settings → Custom Domains → confirm `app-nodal.actorvia.xyz` verifies (Render issues the certificate itself, $0).
- **Do not** touch `api-nodal`, `api`, `www`, `releases` or any other Actorvia record.

## 5. ZITADEL: the first operator's subject, for CP_AUTH_BOOTSTRAP_OPERATORS (OPEN, before any admin surface is used)

- **Why:** `operator_roles` is the only source of operator authority and nothing in the product writes it, so a deployment that has never had an operator cannot get one -- the gate ceremony, the kill switches and the §38 support surface are all unreachable. ADR-0024 decided the mechanism; it needs one value only a person can read out of the identity provider.
- **Where:** ZITADEL console (`https://nodal-az1hxe.us1.zitadel.cloud`) -> Users -> the person who will be the first operator -> copy their **User ID** (an opaque numeric string, e.g. `284169943049306115`). It is not an e-mail address and not a username.
- **What:** in the Render dashboard for `nodal-api`, set `CP_AUTH_BOOTSTRAP_OPERATORS` to `https://nodal-az1hxe.us1.zitadel.cloud|<that user id>=ADMIN` -- the issuer exactly as `CP_AUTH_ISSUER` has it, a pipe, the subject, `=ADMIN`.
- **Then:** that person signs in once. The row appears in `operator_roles` with `granted_by` NULL and a reason naming the variable, an `operator_role.bootstrapped` audit event is written on the admin stream, and their next session carries ADMIN. The boot log names every declared entry, so the API log line is the check that the value reached the process.
- **Constraints the software enforces:** PROD accepts an empty value or exactly one ADMIN and nothing else; BREAK_GLASS cannot be declared; a grant an operator later revokes does not come back at the next login (remove the declaration to stop offering it).
- **Not yet possible in the product:** granting a SECOND operator. ADR-0024 records that as a known gap; today a second declaration (outside PROD) or an INSERT with the migration credential is the only route.

## Done

(nothing yet)
