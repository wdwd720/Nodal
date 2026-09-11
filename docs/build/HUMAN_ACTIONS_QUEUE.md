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
- **This is now true of the VALUE, not just the reference.** Until F-137 the rule only saw `env://NODAL_ALERT_WEBHOOK_URL`, which render.yaml always writes, so a deployment that skipped this item booted healthy and alerted nobody. `config.Load` resolves the reference now: with this variable unset, `nodal-api` fails its configuration check and does not serve.
- **Do not press "Save, rebuild, and deploy" yet** — add item 2 first, then save once.

## 2. Render: set the PII keyring secret (OPEN)

- **Where:** same form, **Add variable**
- **Key:** `NODAL_PII_KEYRING`
- **Value:** the entire contents of `scratchpad\secrets\pii_keyring.json` (single-line JSON, 72 bytes; paste as one line)
- **Why:** STAGING config refuses to boot without a keyring (`RulePIIKeyring`); it seals `identity_pii`.
- **Also true of the value now** (F-137): unset, or set to something `internal/pii` cannot parse, and the service refuses to start rather than serving and storing no personal data at all.
- Then press **Save, rebuild, and deploy** once. Expect one deploy of the currently live commit; the service stays on the free plan.
- **Verify (Claude does this, or you can):** the value fingerprints are in `scratchpad\secrets\fingerprints.txt`; after the deploy, `GET https://api-nodal.actorvia.xyz/v1/readyz` must be 200.

## 3. Push the audited backend HEAD to main (OPEN, after 1–2)

- **Where:** Git Bash, `C:\Dev\Nodal`
- **What:** `git push origin main` — main is at `9906c9f` (ADR-0022 + D-051 on top of the audited checkpoint `024c691`); nothing else is on it.
- **Why:** the goal's §2 orders the push after the secrets exist, because `9906c9f` validates both at boot and would fail its health check without them (Render would keep the old build live, but the failed deploy is avoidable).
- **Unblocks:** live verification of `/v1/healthz`, `/v1/readyz`, `/v1/version` (config hash, and `build_version` — which reports the pushed commit as of F-142; before that it was the literal `dev` for every build ever deployed, so the comparison this step promises could not be made) — tell Claude "secrets set, main pushed" and it verifies and records the evidence in `docs/audit/PRODUCTION_EVIDENCE_INDEX.md`.
- If the push asks for credentials, sign in to GitHub in the credential prompt (account that owns `wdwd720/Nodal`).
- **Already done by Claude (2026-09-10 23:49 PDT):** the `productization` branch is pushed to `origin` as a safe checkpoint (§63) and draft PR **#1** (https://github.com/wdwd720/Nodal/pull/1) is open so CI runs on Linux against it. Neither deploys anything: Render deploys `main` only, and a draft PR merges nothing. The PR body says the order.

## 3b. Merge the productization PR (OPEN, after 3 has verified)

- **Where:** https://github.com/wdwd720/Nodal/pull/1 → **Ready for review** → **Merge** (a merge commit, not a squash: the branch's history is the audit trail the registers cite by hash).
- **When:** only after item 3's verification passed on `9906c9f`, and only when Claude has merged the last two fix branches (`fix/withdrawal`, `fix/docs`) and refreshed the PR — the PR body and `docs/build/MASTER_BUILD_STATE.md` "RESUME HERE" both say whether that has happened.
- **Why:** merging deploys the productization build to STAGING (the API from `render.yaml`'s `nodal-api`, and creates the static site `nodal-web`), which is what §2's live verification and item 4's CNAME need.
- **Unblocks:** items 4 and 6; the §56 browser walk of the live staging system.

## 4. GoDaddy: CNAME for the web app (OPEN, later — after `nodal-web` exists on Render)

- **When:** only after the `productization` branch has merged to `main` and Render's blueprint sync has created the static site `nodal-web` (Claude will move this item to "ready" and fill in the exact target then; the target is the site's `*.onrender.com` hostname shown on its Settings page).
- **Where:** GoDaddy → DNS for `actorvia.xyz` → **Add record**: Type `CNAME`, Name `app-nodal`, Value `<nodal-web's onrender.com hostname>`, TTL 600.
- **Then:** Render → `nodal-web` → Settings → Custom Domains → confirm `app-nodal.actorvia.xyz` verifies (Render issues the certificate itself, $0).
- **Do not** touch `api-nodal`, `api`, `www`, `releases` or any other Actorvia record.

## 5. Render: delete `NODAL_DB_MIGRATE_URL` from the `nodal-api` environment (OPEN, any time)

- **Where:** https://dashboard.render.com/web/srv-dah02lht0dsc73e1la50/env → **Edit** → the row `NODAL_DB_MIGRATE_URL` → **Delete** → **Save, rebuild, and deploy**
- **Why:** it is the `cp_migrate` DSN, and `cp_migrate` owns every table. An owner can `ALTER TABLE ... DISABLE TRIGGER`, and since 00743–00753 every state machine in this system is enforced by triggers — the transition bindings, `forbid_mutation` on fifty-three append-only tables, and the eleven that write state columns the application cannot. Nothing in `cmd/api` has ever read it (F-136).
- **Why a person has to do it:** the entry is gone from `render.yaml`, and a blueprint sync does **not** remove a value that already exists in the dashboard. Until somebody deletes it by hand, the credential stays in the container's environment.
- **How migrations are run instead — unchanged, and this is the whole reason the variable bought nothing:** an operator runs `go run ./cmd/migrate up` from their own machine, against Neon, with `CP_DATABASE_MIGRATE_URL` in that shell and nowhere else. `cmd/migrate` reads it straight from its own environment; no service does. This tier deploys no worker and no cron job (both are paid service types on Render, and a fixed monthly charge is the one thing it may not have), so the migrations and the reconciliation sweep are operator-run — `docs/operations/LAUNCH_TIER.md` §10 states it, and §8 is where that stops being true.
- **Verify:** the dashboard shows no variable whose name contains `MIGRATE`; `go test ./test/infra/ -run TestTheWebServiceIsNotGivenTheSchemaOwner` keeps the file that way.

## 6. ZITADEL: the first operator's subject, for CP_AUTH_BOOTSTRAP_OPERATORS (OPEN, before any admin surface is used)

- **Why:** `operator_roles` is the only source of operator authority and nothing in the product writes it, so a deployment that has never had an operator cannot get one -- the gate ceremony, the kill switches and the §38 support surface are all unreachable. ADR-0024 decided the mechanism; it needs one value only a person can read out of the identity provider.
- **Where:** ZITADEL console (`https://nodal-az1hxe.us1.zitadel.cloud`) -> Users -> the person who will be the first operator -> copy their **User ID** (an opaque numeric string, e.g. `284169943049306115`). It is not an e-mail address and not a username.
- **What:** in the Render dashboard for `nodal-api`, set `CP_AUTH_BOOTSTRAP_OPERATORS` to `https://nodal-az1hxe.us1.zitadel.cloud|<that user id>=ADMIN` -- the issuer exactly as `CP_AUTH_ISSUER` has it, a pipe, the subject, `=ADMIN`.
- **Then:** that person signs in once. The row appears in `operator_roles` with `granted_by` NULL and a reason naming the variable, an `operator_role.bootstrapped` audit event is written on the admin stream, and their next session carries ADMIN. The boot log names every declared entry, so the API log line is the check that the value reached the process.
- **Constraints the software enforces:** PROD accepts an empty value or exactly one ADMIN and nothing else; BREAK_GLASS cannot be declared; a grant an operator later revokes does not come back at the next login (remove the declaration to stop offering it).
- **Not yet possible in the product:** granting a SECOND operator. ADR-0024 records that as a known gap; today a second declaration (outside PROD) or an INSERT with the migration credential is the only route.

## 7. GitHub: Actions minutes are exhausted for the private repository (OPEN, your call)

- **What Claude saw (2026-09-10 23:50 PDT):** every job of the draft PR #1 run failed in 2–7 seconds with the annotation *"The job was not started because recent account payments have failed or your spending limit needs to be increased. Please check the 'Billing & plans' section in your settings."* No job ran; no log exists. The runs on `main` earlier today (`16cba60`) did run, so the free allowance was used up between then and now, or a payment method failed.
- **Where:** https://github.com/settings/billing → **Actions** (usage and the spending limit).
- **Options, none taken by Claude because §42 forbids adding cost and making the repository public is a disclosure decision:** (a) wait for the monthly reset of the free-plan minutes (2,000 min/month for private repositories) — the draft PR re-runs on its next push; (b) make `wdwd720/Nodal` public, which makes Actions free and unlimited but publishes the code; (c) raise the spending limit, which is paid and therefore not what the goal wants. If you choose (a) or (b), tell Claude and it re-triggers the run (an empty commit or `gh run rerun`) and records the result in `docs/audit/PRODUCTION_EVIDENCE_INDEX.md`.
- **Meanwhile:** every matrix tier that CI would run has been run on this machine and is recorded in the evidence index; CI is the Linux cross-check, not the only evidence. Every push to the branch will show a red check for the same reason until this is resolved — it costs nothing.

## Done

(nothing yet)
