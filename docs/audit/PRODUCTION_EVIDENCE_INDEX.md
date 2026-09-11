# Production evidence index

Where the evidence for every claim the productization goal's final report makes
actually lives, and what kind of evidence it is. This is an index, not a
narrative: each row names a claim, the artefact that proves it (a file, a test,
a command, a commit, an observed response), and the class of that evidence.
Where there is no evidence yet it says so, because a row that says UNKNOWN is
worth more than a row that rounds up.

**Evidence classes** (the same vocabulary as `FINAL_CHECKPOINT_2026-09-10.md`):

| Class | Meaning |
|---|---|
| `LIVE_OBSERVED` | A person or this session saw it happen on a running system — a command's output, a deployed endpoint's response, a browser run. The row names the run. |
| `STATIC_PROOF` | The tree itself proves it — a test that a CI or local run holds green, a constraint in a migration, a declaration in the blueprint. The row names the file or test. |
| `BLOCKED_EXTERNAL` | Only a person, a provider or counsel outside this repository can produce it. The row names who, and the queue item. |
| `UNKNOWN` | Not yet observed and not provable from the tree. The row says what would make it known. |

Last written: **2026-09-10, late evening** (Pacific), while the productization
wave's last audits were running. Rows marked *(pending)* are refreshed when they
report; the matrix rows name the commit each run was made at, because a hash
written into the object it hashes is wrong the moment anything lands after it.

---

## 1 · The tree, its checkpoints and its branches

| Claim | Evidence | Class |
|---|---|---|
| The pre-productization checkpoint is `024c691` (2026-09-10 14:39 PDT); `SOFTWARE_COMPLETE` was true there for the backend as then audited | `git log -1 024c691`; `docs/audit/FINAL_CHECKPOINT_2026-09-10.md` | `STATIC_PROOF` |
| `main` stands at `9906c9f` (15:06 PDT) locally and is **not pushed**; the push is the human's (queue item 3). The `productization` branch IS pushed (`origin/productization`, 23:49 PDT) as the §63 safe checkpoint, with draft PR #1 open for CI; neither deploys anything (Render deploys `main` only) | `git log -1 9906c9f`; `git ls-remote origin productization`; `docs/build/HUMAN_ACTIONS_QUEUE.md` §3, §3b | `LIVE_OBSERVED` / `BLOCKED_EXTERNAL` |
| Everything productization built is on branch `productization`, 240-odd commits after `main`, merged from twenty-five feature and fix branches, each merge followed by build, vet, lint, unit, the touched integration suites and a restore drill | `git log --oneline 9906c9f..productization`; the `Merge branch` commits (`wt/*` product branches, `fix/*` audit-fix branches); `MASTER_BUILD_STATE.md` "RESUME HERE" | `STATIC_PROOF` |
| Every auditor's reproductions are preserved, tests only, on their own branch | `audit/config-deploy` `4d5c323`, `audit/credits-payments` `0e79145`, `audit/governance` `05e4024`, `audit/platform` `453bf14`, `audit/accounts-auth` `b6a68ab`, `audit/agents-notifications` `8a69aaa`, `audit/markets` `1681ee2`, `audit/frontend` `68f23f9`; wave B: `audit/withdrawal-verification`, `audit/docs-vs-reality`, `audit/e2e-browser` *(pending)* | `STATIC_PROOF` |
| The schema head is migration **00805**, 116 migration files, applied ones never edited (checksummed) | `ls migrations/`; `internal/migrate`'s checksum journal; `TestDocs_CountsMatchTheCode` | `STATIC_PROOF` |
| The customer app's route map is D-077's: 34 `<Route>` declarations in `apps/web/src/App.tsx`; the API publishes 98 paths | `apps/web/src/App.tsx`; `openapi/openapi.yaml`; `docs/build/CURRENT_SYSTEM_INVENTORY.md` | `STATIC_PROOF` |

## 2 · Build and static gates

| Claim | Evidence | Class |
|---|---|---|
| The tree builds and vets clean | `go build ./... && go vet ./...` at every merge (this session); `make lint` (`fmtcheck`, vet, staticcheck, golangci-lint, `lintfin`) **0 issues at `0ea3f05`** (23:00 PDT) and re-running at `60de57d` *(pending)* | `LIVE_OBSERVED` |
| The web app typechecks under both tsconfigs, its unit suite passes, and its main chunk is under the 180 kB gzipped budget | `pnpm --filter @controlplane/web typecheck` / `test` / `build` at `60de57d`: clean, **141/141**, **160.20 kB** (the markets and agents areas are lazy chunks, F-web fix `1507f58`) | `LIVE_OBSERVED` |
| Generated artefacts match the contract | `make openapi-server && make openapi-client` re-run at every merge; `git status` clean afterwards; the admin console's generated-artefact guard | `LIVE_OBSERVED` |
| Money never passes through a float in the browser; no page hardcodes a figure; the Credit scale is declared once | `apps/web/src/lib/source-scan.test.ts`, `honesty.test.ts`, `audit-frontend.test.ts` ("the scale of a Credit is stated in exactly one place": `src/lib/credits.ts` only) | `STATIC_PROOF` |
| No `TODO`, `FIXME`, stub, mock, placeholder or "coming soon" is left unexplained in customer-facing code (§62) | Scan of 2026-09-10 22:50 over `apps/web/src`, `internal`, `cmd`: 0 TODO/FIXME/stub/coming-soon; every "placeholder"/"not implemented"/"mock"/"temporary" is a comment, a redaction regex, a declared-and-refused rail, or the sandbox fee model's own `SANDBOX-PLACEHOLDER-NOT-A-PRICE` rendered as such by `Withdraw.tsx` | `LIVE_OBSERVED` |

## 3 · The test matrix on the merged tree

| Tier | Result | At | Class |
|---|---|---|---|
| `go test ./...` (unit, every package) | pass, 0 failures | `6c5a3f3` (the web merge), from a verification worktree | `LIVE_OBSERVED` |
| `go run ./scripts/inttest` (every integration package, one fresh database each) | **58 packages, all passed, 15m42s** | `6c5a3f3` | `LIVE_OBSERVED` |
| Integration packages touched after that (ledger, commerce, demo, credit, nativemarket, payout, cmd/api, security, migrations) | 9 packages, all passed, 2m18s | `f7328ab` | `LIVE_OBSERVED` |
| `make contract` (provider contract tests against recorded fixtures) | pass | `f7328ab` | `LIVE_OBSERVED` |
| `go test -race` on stream, notifications, agents (the packages the concurrency fixes touched) | pass | `0ea3f05`, with `CC=C:/toolchain/mingw64/bin/gcc.exe` (F-125's recipe) | `LIVE_OBSERVED` |
| `make lint` (fmtcheck, vet, staticcheck, golangci-lint, lintfin) | **0 issues** | `a4830b9` (main tree) | `LIVE_OBSERVED` |
| `go test ./...` | pass, 0 failures | `60de57d` | `LIVE_OBSERVED` |
| `make integration-race` (the financial core under the race detector, integration-tagged, one database each) | **12 packages, all passed, 8m02s** (capital, buyingpower, commerce, credit, event, execution, ledger, nativemarket, payout, reconciliation, settlement, signing) | `60de57d`, with the `C:/toolchain/mingw64` GCC | `LIVE_OBSERVED` |
| `make e2e` (the API binary driven end to end over HTTP, own database) | **14 passed, 0 skipped, 18.5 s** with a provisioned database (`go run ./scripts/testdb -name e2ehead -export`). Note: without `CP_TEST_DATABASE_URL` the suite skips every case in 0.3 s and reports `ok`; a green `make e2e` is evidence only with the database — CI provisions one, the verification worktree's first pass did not | `a4830b9` | `LIVE_OBSERVED` |
| `make chaos` (fault injection, own database) | pass, 5.2 s; two cases skip by design on this host — the archive-refused case (`CP_TEST_ARCHIVE_ENDPOINT` unset) and the broker-stall case (`CP_TEST_REDPANDA_BROKERS` unset) — each naming its reason in the spec | `a4830b9` | `LIVE_OBSERVED` |
| `make race` (capital, ledger, execution, reconciliation, event, settlement, signing under the race detector, unit-tagged) | **10 packages ok, exit 0** | `a4830b9`, with the `C:/toolchain/mingw64` GCC | `LIVE_OBSERVED` |
| `make fuzz` (every fuzz target, 15 s each) | *(running in the main tree)* | `a4830b9` | *(pending)* |
| CI on GitHub (the whole matrix on Linux runners) | draft PR **#1** (`productization` → `main`) opened at 23:49 PDT to run it. **It could not run:** every job failed in seconds with GitHub's annotation *"The job was not started because recent account payments have failed or your spending limit needs to be increased"* — the private repository's Actions minutes are exhausted. The last run that did execute on `main` (`16cba60`, before this goal) was red for reasons fixed on this branch (F-134, the capacity test, `make fmt` drift). Queue item 7 names the three ways to restore CI; none is Claude's to take (§42) | `0bf8330` | `BLOCKED_EXTERNAL` |
| The pre-audit matrix (before the fix wave) | lint, unit, race, integration-race, fuzz, contract, full inttest (57 packages), e2e and chaos all green at 19:00 PDT, the last two after fixing two baseline test defects that also fail on the audited checkpoint (F-134, F-135) | `ef5d9ae`-era tree | `LIVE_OBSERVED` |

## 4 · Browser evidence

| Claim | Evidence | Class |
|---|---|---|
| The merged web app's Playwright suite passes against a sandbox-tier API on a fresh database with demo markets | **154 passed / 3 skipped / 1 failed** at `f7328ab` (23:12 PDT; one worker, real OIDC dev flow, demo data seeded at boot, one declared operator). The one failure was the reversed-bucket reproduction's precondition, which now skips with its reason at `60de57d`. Every skip names its cause in the spec (the Stripe webhook leg without a key; the agent compiler wiring). Recipe: `docs/product/STAGING_E2E.md` "How the automated suite runs"; CI: `.github/workflows/ci.yml` `web-e2e` | `LIVE_OBSERVED` |
| The first merged run (23:00) failed at its sign-in setup — a client race the load exposed — and the demo seeder refused every trade | F-222 and F-223 in `AUDIT_FINDINGS.md`, fixed in `669f454`; the API log of that run is quoted in F-222 | `LIVE_OBSERVED` |
| Scenarios A–J are proven step by step by an independent auditor as a stranger reading the screen | *(pending — `audit/e2e-browser`, `apps/web/e2e/audit-journey.spec.ts`)* | *(pending)* |
| The live staging deployment was walked in Chrome (§56) | **Not yet.** The deployed API is still the pre-productization build (see §7); the walk needs the human's queue items 1–4 first | `UNKNOWN` → `BLOCKED_EXTERNAL` |

## 5 · Restore drills

Every drill is `make restore-drill`: dump the live local database, restore into
a fresh one, compare table counts, row counts, journal hashes and the
`ledger_balances` drift, boot the API against the restored copy and make one
state change. The recorded claim is the one line in
`docs/operations/BACKUP_RESTORE.md`, and `TestDocs_CountsMatchTheCode` holds
it to the migration head so a merge that adds a migration cannot leave a stale
drill claim behind.

| After | Head | Result | Commit |
|---|---|---|---|
| the notifications merge | 00786 | OK, 153 tables | `de7f377` |
| the profile / markets / verification merges | 00786 | OK | `22d311c`, `67040b4`, `bab7759` |
| the governance fix | 00791 | OK | `08345e9` |
| the accounts fix, the credits fix | 00800 | OK | `5ab91f3`, `5c4caff` |
| the platform fix (00796 added) | 00800 | OK, 154 tables, 15.4 s | `ccd2235` |
| the markets fix | 00805 | OK, 154 tables, 14.7 s | `4ef6945` |
| the agents-notifications fix (00801–00803 added) | 00805 | OK, 154 tables, 15.5 s | `0ea3f05` |

Class: `LIVE_OBSERVED` (each run's `dist/restore-drill.json` on this host; the
line in `BACKUP_RESTORE.md` is the committed record).

## 6 · The adversarial audit (§54)

| Claim | Evidence | Class |
|---|---|---|
| Eight independent auditors (one per area, none the author of what they audited) reported with reproductions | Their branches (§1); `docs/audit/AUDIT_FINDINGS.md` F-136–F-220 with **Found by** lines naming the audit; the fix branches merged: governance `294204c`, config-deploy `95c6a3e`, accounts `9d63b7c`, credits-payments `12601f7`, platform `4352777`, markets `0103d5e`, agents-notifications `c09408e`, web `6c5a3f3` | `STATIC_PROOF` |
| Every finding fixed has a regression test that fails on the defect and passes on the fix | Each F-entry's **Evidence** line names the test; the auditors' own reproductions (renamed where they asserted the defect) run in the merged suites; the docs suite refuses a FIXED entry without a summary row | `STATIC_PROOF` |
| Findings the merge itself produced were registered, not absorbed | F-221 (destination key), F-222 (onboarding race), F-223 (demodata resolver); D-118 for the one rule two fix branches disagreed on | `STATIC_PROOF` |
| Wave B: withdrawal-verification, docs-vs-reality, browser end-to-end | *(pending)* | *(pending)* |
| "Findings flatten": the last audit round finds fewer, and lower, than the one before | *(pending — stated in the §65 report with the wave-B counts against wave A's 89)* | *(pending)* |

## 7 · Deployment, governance state and money

| Claim | Evidence | Class |
|---|---|---|
| The staging API is alive at `https://api-nodal.actorvia.xyz` | `GET /v1/version` at 22:50 PDT answered `{"build_version":"dev","config_hash":"e1ad81b6…","environment":"STAGING"}` (HTTP 200) | `LIVE_OBSERVED` |
| …but it is the **pre-productization build**: `build_version` is `dev` (D-117's commit stamp is not deployed) and its config hash predates the sandbox tier | the same response; `render.yaml` at `productization` vs the dashboard | `LIVE_OBSERVED` |
| STAGING will refuse to boot the productization build until two secrets exist | `internal/config` validation (F-136 family); queue items 1–2 name the variables (`NODAL_ALERT_WEBHOOK_URL`, `NODAL_PII_KEYRING`) and where the generated values are | `STATIC_PROOF` / `BLOCKED_EXTERNAL` |
| The blueprint declares both services on the free plan, the API's secrets as dashboard-only (`sync: false`), the custom domain, and the static site's CSP and headers | `render.yaml` (`plan: free` on `nodal-api`; a static site has no paid plan; `NODAL_STRIPE_API_KEY`, `NODAL_STRIPE_WEBHOOK_SECRET`, `NODAL_PII_KEYRING`, `NODAL_ALERT_WEBHOOK_URL` all `sync: false`); `test/infra` blueprint tests | `STATIC_PROOF` |
| No paid tier, no new infrastructure, no cost was added by this goal (§42) | Only `render.yaml` changed (a free static site, a domain); no Neon, ZITADEL, AWS or observability change; `git log --stat -- infra/ render.yaml` | `STATIC_PROOF`; the account's billing page is the human's to confirm (`BLOCKED_EXTERNAL`) |
| `CREDIT_PURCHASE` and the five other product gates carry no approval anywhere | Local sandbox tier: the six gates are `SANDBOX` (boot log: "capability sandbox-activated at boot: this gate carries no approval"); STAGING: sandbox activation happens at the first boot of the productization build; PROD: `config.Validate` refuses `SANDBOX`, migration 00755's CHECK refuses a sandbox row, `gates.Admin.Sandbox` refuses outside a sandbox tier (ADR-0023, D-052, F-160–F-162) | `LIVE_OBSERVED` / `STATIC_PROOF` |
| No real card was charged, no real USD moved, no payout was initiated, no KYC/AML/provider approval was fabricated (§59) | No live Stripe key exists in the tree or the scratchpad (publishable key only, `VITE_STRIPE_PUBLISHABLE_KEY`, D-078); the payout provider on every non-PROD tier is `sandbox_payout`, which moves nothing; the verification provider is `sandbox_verification`, which decides nothing; every sandbox outcome is labelled as such in the API, the log and the page | `STATIC_PROOF` |
| `LEGAL_APPROVED` stays false; the served documents say they are drafts pending counsel | `internal/terms/documents/*.md` (`counsel_review_required`), `GET /v1/terms`; the onboarding page's own sentence | `STATIC_PROOF` |

## 8 · Product documentation (§53)

| Document | Exists | Reconciled against the tree |
|---|---|---|
| `docs/product/PRODUCT_ARCHITECTURE.md`, `USER_JOURNEY.md`, `CREDIT_ECONOMY.md`, `VERIFICATION_AND_WITHDRAWAL.md`, `UI_UX_SYSTEM.md`, `PROVIDER_BOUNDARY.md`, `STAGING_E2E.md` | yes | by the docs-vs-reality audit *(pending)*; `STAGING_E2E.md` corrected at `f7328ab` |
| `MASTER_BUILD_STATE.md`, `REQUIREMENTS_TRACEABILITY.md`, `BLOCKERS.md`, `DECISION_REGISTER.md` (D-052–D-118), `CURRENT_SYSTEM_INVENTORY.md`, `LAUNCH_GATE_MATRIX.md`, `AUDIT_FINDINGS.md` (F-134–F-223), this index | yes | `test/docs` (counts, references, findings shape, decision evidence) green at `60de57d`; the docs-vs-reality audit *(pending)* |

## 9 · Human actions

Everything a person must do, with the exact place and field, is in
`docs/build/HUMAN_ACTIONS_QUEUE.md`: two Render secrets, the push of `main`,
the `app-nodal` CNAME, one stale dashboard variable to delete, and the first
operator's ZITADEL subject. None of them is software work, and every one of
them is `BLOCKED_EXTERNAL` for the duration of the human's absence.
