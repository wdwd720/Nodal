# ADVERSARIAL VALIDATION — PART 238 final test matrix

**Every number in this document was produced by a command run on this host on 2026-09-06 and pasted from its
output.** Nothing here is copied from another agent's summary, from `MASTER_BUILD_STATE.md`, or from a previous
run. Where a tier could not be executed, or was executed and failed, that is stated with the literal output
rather than smoothed over. §14 — what these results do **not** prove — is the part a reader deciding whether to
move real money should read first.

---

## 1. Run environment

| Item | Value |
|---|---|
| Commit | `bbc12f8c1149cdb347e55e84d4ce4334fb598a7f` ("Stage 15: make format.ts's coverage guarantee real") |
| Working tree | at the time of this run, 123 uncommitted entries (SB-008). **Superseded later the same day:** 1,340 files committed as `673d9bf` and pushed; SB-008 closed |
| Git remotes | none at the time of this run (SB-004). **Superseded later the same day:** `origin` → `https://github.com/wdwd720/Nodal.git`, `main` pushed, and `ci.yml` executed for the first time |
| Date | 2026-09-06 |
| Host | Windows 11 Pro 10.0.26100, AMD Ryzen 9 6900HX, 8 physical / 16 logical cores, 28.7 GB RAM |
| Go | `go1.27.0 windows/amd64`, `GOTOOLCHAIN=local` |
| cgo | `CGO_ENABLED=1` for tests (WinLibs GCC at `C:/Dev/tools/mingw64`); `CGO_ENABLED=0` for the production build |
| Infrastructure | local `docker-compose` stack: `cp-postgres:5433`, `cp-redis:6380`, `cp-redpanda:19092`, `cp-clickhouse:19000`, `cp-minio:9100`, `cp-temporal:7233` |
| Databases | provisioned per suite with `go run ./scripts/testdb -name <x> -export`. The shared `controlplane_test` was **never** used. Databases created for this run: `controlplane_test_adv238{int,int2,mig,sec,e2e,chaos,load,unknown,part49,propint,iso*}` |

The stack was healthy before the run:

```
/cp-redpanda running healthy
/cp-postgres running healthy
/cp-minio running healthy
```

---

## 2. Matrix at a glance

| Tier | Command | Result | Time |
|---|---|---|---|
| build | `go build ./...`, `go mod verify`, `CGO_ENABLED=0 go build -o bin/ ./cmd/...` | **PASS** — exit 0, 9 binaries, all modules verified | 8.3 s |
| unit | `go test -count=1 ./...` | **PASS** — 82 packages, 1,498 top-level tests, 6,474 assertions-level PASS lines, 1 skip, 0 fail | 38.5 s |
| race | `go test -count=1 -race ./...` | **PASS** — 82 packages, 0 failures, **0 data races** | 3 m 31.6 s |
| property | `go test -count=1 -run 'Prop\|Property' ./internal/...` | **PASS** — 31 packages ran matching tests, 0 fail. Plus 4 integration-tagged property tests run separately: PASS | 18.5 s + 68 s |
| fuzz | `go run ./scripts/fuzzall -fuzztime=30s -v` | **PASS** — "28 targets, 0 failed", **18,410,387 executions**, no crasher in budget | 16 m 14.7 s |
| contract | `go test -count=1 ./test/contract/...` | **PASS** — 6 packages, 53 tests, 0 fail | 14.0 s |
| integration | `-tags=integration` over `./internal/... ./cmd/...` | **74 of 75 packages PASS; 1 genuine FAIL** (`internal/risk`, §8) | 5 m 38 s + isolated re-runs |
| E2E (Go) | `-tags='integration e2e' ./test/e2e/` | **PASS** — 6 tests / 26 subtests, cross-process against the real `cmd/api` binary | 20.8 s |
| E2E (browser) | `npx --no-install playwright test` | **PASS** — 51 tests, real Chrome, real OIDC login | 46.4 s |
| chaos | `-tags='integration chaos' ./test/chaos/` | **PASS on the second attempt** — 7 tests. The first attempt correctly *refused* (§10) | 31.2 s |
| security | `-tags=integration ./test/security/` | **PASS** — 39 tests / 462 subtests, twice on one database, **all 16 negative controls fire** | 6.4 s |
| load | k6, 5 scenarios | **3 measured, 1 blocked (503, correctly), 1 runs but its financial assertion is vacuous** (§12) | ~7 min |
| migration | `-tags=integration ./test/integration/migrations/...` (own DB, own invocation) | **PASS** — 3 tests / 11 subtests | 43.3 s |
| restore drill | `go run ./scripts/restoredrill` | **PASS** — journal hash identical across restore, 0 balance drift | 10.1 s |
| audit verification | `go run ./cmd/audit-worker verify` | **PASS** — 304 events, 1 signed checkpoint covering all 304 | < 1 s |
| unknown submission | PART 48/49 named tests | **PASS** — 3 integration tests + 8 executor tests (51 fault-injected subtests) | — |

Negative controls (tests proven able to fail), all executed here: **security 16/16**, **chaos 6/6**, **E2E 7/7**.

---

## 3. build

```
$ CGO_ENABLED=1 go build ./...
exit=0

$ go mod verify
all modules verified
exit=0

$ CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/nodal/controlplane/internal/config.BuildVersion=bbc12f8c1149" -o bin/ ./cmd/...
exit=0

$ go vet ./...
exit=0

$ go vet -tags=integration ./...
exit=0
```

Nine production binaries were produced by the `CGO_ENABLED=0` build:

```
bin/agent-worker.exe        bin/api.exe                  bin/audit-worker.exe
bin/execution-worker.exe    bin/market-ingest-worker.exe bin/migrate.exe
bin/reconciliation-worker.exe bin/relay-worker.exe       bin/workflow-worker.exe
```

The running API reported the version stamped by those `-ldflags`, which is what makes the stamp real rather than
declared:

```
level=INFO msg="api listening" addr=127.0.0.1:18099 env=LOCAL build_version=bbc12f8c1149
  config_hash=b074786717bed91333a61ee53b6bb1d56727ee035304f3a6a71a2486a91981b1
```

---

## 4. unit

```
$ go test -count=1 -timeout=20m -v ./...
...
real    0m38.546s
EXIT=0
```

| Measure | Count |
|---|---|
| Packages with tests, all `ok` | **82** |
| Packages with no test files | 32 |
| Top-level `--- PASS` | **1,498** |
| `PASS:` lines including subtests | **6,474** |
| `--- SKIP` | 1 |
| `--- FAIL` | **0** |

The single skip names its reason and is not a silent hole:

```
=== RUN   TestIntegration_RedisStore
    ratelimit_test.go:162: CP_TEST_REDIS_URL not set; skipping Redis rate-limit test
--- SKIP: TestIntegration_RedisStore (0.00s)
```

(That test does run in the integration tier, where `CP_TEST_REDIS_URL` is set — see §8.)

For scale: the tree contains 1,846 `func Test*`, 40 `func Fuzz*` and 312 `_test.go` files across 787 `.go` files.

---

## 5. race

```
$ go test -count=1 -race ./...
...
real    3m31.619s
EXIT=0
```

- 82 packages `ok`, 32 with no test files, **0 `FAIL`, 0 `DATA RACE`**.
- Slowest: `internal/eligibility` 95.952 s, `internal/withdrawal` 9.617 s, `test/security` 9.221 s,
  `internal/killswitch` 8.395 s, `internal/observability` 7.837 s.

This is the whole module under `-race`, not just the `RACE_PKGS` subset that `make race` uses.

---

## 6. property

```
$ go test -count=1 -timeout=20m -run 'Prop|Property' ./internal/...
...
real    0m18.499s
EXIT=0
```

62 packages reported `ok`; **31 of them actually ran matching tests** (the rest print `[no tests to run]`).
`go test -list` reports 63 `TestProp*` functions. Packages that ran properties:

`adminplane, archive, audit, auth, capital/buyingpower, chain, config, db, db/migrate, eligibility, event,
fees, funding, gates, id, idempotency, intent, killswitch, ledger, money, positions, proof, provider,
provider/jupiter, provider/solanarpc, ratelimit, reality, risk, security, settlement, signing/inspect`

### Gap found while running this tier

`make property` does **not** run every property test. Four database-backed properties live in files carrying
`//go:build integration`, so the untagged `-run 'Prop|Property' ./internal/...` invocation skips them silently.
They are the four that constrain money conservation and reconciliation convergence — arguably the most important
properties in the system. Run separately here on a dedicated fresh database:

```
$ go test -count=1 -tags=integration -run 'Prop|Property' ./internal/capital/ ./internal/reconciliation/
--- PASS: TestProp_CapitalConserved (12.91s)
--- PASS: TestProp_ReservationsNeverOversubscribe (16.68s)
ok      github.com/nodal/controlplane/internal/capital  29.874s
--- PASS: TestProp_ReplayConvergesAndNeverDoublePosts (35.36s)
--- PASS: TestProp_BalanceComparisonConverges (2.91s)
ok      github.com/nodal/controlplane/internal/reconciliation    38.452s
```

Both invocations are needed for "property: green" to mean what a reader assumes it means.

---

## 7. fuzz

28 native Go fuzz targets were discovered by `go run ./scripts/fuzzall -list` and each was run for a 30-second
budget (`scripts/fuzzall` does not include its own self-test fixtures under `scripts/`, which is why the count is
28 and not the 40 `func Fuzz*` in the tree).

```
$ go run ./scripts/fuzzall -fuzztime=30s -v
...
28 targets, 0 failed

real    16m14.749s
EXIT=0
```

| Target | Executions | New interesting | Corpus |
|---|---:|---:|---:|
| `internal/archive.FuzzParseKey` | 75,456 | 29 | 60 |
| `internal/audit.FuzzCanonicalJSON` | 330,102 | 51 | 279 |
| `internal/auth/oidc.FuzzVerifyNeverPanics` | 178,799 | 45 | 156 |
| `internal/config.FuzzParseSecretRef` | 867,309 | 8 | 131 |
| `internal/event.FuzzEnvelopeJSON` | 897,376 | 67 | 518 |
| `internal/funding.FuzzParseDecimalAmount` | 1,241,069 | 68 | 198 |
| `internal/id.FuzzParse` | 2,622,668 | 0 | 45 |
| `internal/idempotency.FuzzCanonicalJSON` | 341,760 | 63 | 454 |
| `internal/intent.FuzzValidate` | 585,090 | 63 | 237 |
| `internal/money.FuzzParseQuantity` | 110,953 | 6 | 66 |
| `internal/money.FuzzParseUSD` | 2,392,464 | 3 | 43 |
| `internal/money.FuzzParseUSDRound` | 2,501,330 | 1 | 168 |
| `internal/money.FuzzQuantityFromDecimalString` | 140,443 | 2 | 800 |
| `internal/money.FuzzScanQuantity` | 40,133 | 8 | 63 |
| `internal/provider/jupiter.FuzzDecodeBuildResponse` | 624,655 | 64 | 377 |
| `internal/provider/jupiter.FuzzDecodeExecuteResponse` | 659,580 | 62 | 457 |
| `internal/provider/jupiter.FuzzDecodeOrderResponse` | 552,076 | 48 | 408 |
| `internal/provider/jupiter.FuzzSummarizeTransaction` | 128,918 | 7 | 60 |
| `internal/provider/solanarpc.FuzzParseTransactionResponse` | 241,538 | 46 | 351 |
| `internal/provider/stripe.FuzzDecodeEvent` | 351,410 | 48 | 339 |
| `internal/provider/stripe.FuzzVerifySignatureHeader` | 879,583 | 24 | 117 |
| `internal/quote.FuzzRouteHash` | 456,100 | 52 | 491 |
| `internal/reality.FuzzDecodeWalletEvent` | 717,694 | 59 | 410 |
| `internal/signing/inspect.FuzzDecode` | 203,860 | 2 | 56 |
| `internal/signing/inspect.FuzzInspect` | **9,479** | 3 | 34 |
| `internal/strategy/ir.FuzzDecode` | 123,225 | 31 | 329 |
| `internal/strategy/ir.FuzzParseDecimalString` | 918,088 | 0 | 118 |
| `internal/strategy/ir.FuzzParseIR` | 219,229 | 29 | 328 |
| **Total** | **18,410,387** | | |

**What this means, stated plainly.** A bounded fuzz run is evidence that **no crashing input was found inside
that budget**. It is not proof that none exists. 30 seconds per target on one machine is a smoke test, not a
campaign. The weakest row is `FuzzInspect` at 9,479 executions — three orders of magnitude below the strongest —
and it happens to be the transaction inspector, the component standing between a signed message and customer
funds. That budget should be hours, not seconds, before any live signing.

**Reporting caveat.** Go's fuzzing coordinator on this host froze its execution counter partway through several
targets (`execs: N (0/sec)` for the remainder of the budget) while the process kept running to the full 30
seconds. The counts above are exactly what the engine printed at the end of each budget; for those targets the
true execution count is unknown and may be higher. No number here has been adjusted.

---

## 8. contract

```
$ go test -count=1 -v ./test/contract/...
ok      github.com/nodal/controlplane/test/contract/eventtopics  0.988s
ok      github.com/nodal/controlplane/test/contract/helius       0.188s
ok      github.com/nodal/controlplane/test/contract/jupiter      4.192s
ok      github.com/nodal/controlplane/test/contract/privy        5.195s
ok      github.com/nodal/controlplane/test/contract/solanarpc    2.189s
ok      github.com/nodal/controlplane/test/contract/stripe       2.210s
EXIT=0
```

53 top-level tests, 68 `PASS:` lines including subtests, **0 failures**.

| Package | Tests | Verification label recorded in its README |
|---|---:|---|
| `eventtopics` | 1 | internal topic registry, no provider |
| `helius` | 5 | fixtures from documented shapes; several fields explicitly labelled **assumed** |
| `jupiter` | 21 | **CODE_COMPLETE**; live verification **BLOCKED_EXTERNAL (EB-011)** — *no fixture was captured from `api.jup.ag`* |
| `privy` | 4 | **CODE_COMPLETE**; EB-005 tracks sandbox |
| `solanarpc` | 10 | fixtures labelled documented vs assumed per field |
| `stripe` | 12 | **CODE_COMPLETE + CONTRACT_TESTED**; `SANDBOX_VERIFIED` impossible until EB-003 |

Every one of these ran against **recorded fixtures**, not a live provider. See §14.

---

## 9. integration

This tier is where the honest answer is more complicated than a single line, so all three invocations are given.

### 9.1 One database, packages in parallel — FAILS, and the failure is the invocation

```
$ go test -count=1 -timeout=40m -tags=integration ./internal/... ./cmd/...
... 64 packages ok, 11 packages FAIL, 43 test failures
real    1m57.751s
EXIT=1
```

Failing packages: `internal/{event,execution,funding,httpapi,proof,reconciliation,risk,settlement,webhook}`,
`cmd/{execution-worker,relay-worker}`. Representative output:

```
--- FAIL: TestIntegration_RelayPublishesOnlyCommittedRows (0.20s)
    relay_integration_test.go:48:
        	Error:      	Not equal:
        	            	expected: 3
        	            	actual  : 12
```

The cause is the invocation, not the product. `internal/event`'s fixture executes
`TRUNCATE outbox_events, inbox_messages` on every test, and several suites assert on **global** row counts. Run
concurrently against one database, packages destroy and pollute each other's state.

### 9.2 One database, serialized (`-p 1`) — better, still not clean

```
$ go test -count=1 -p 1 -timeout=60m -tags=integration ./internal/... ./cmd/...
... 70 packages ok, 18 no test files, 5 packages FAIL, 14 test failures
real    5m37.904s
EXIT=1
```

Remaining failures: `internal/{funding,killswitch,proof,reconciliation,risk}` — caused by rows an earlier
package left behind, e.g.

```
--- FAIL: TestIntegration_CleanCheckpointVerifiesAndRecordsRun (0.30s)
        	Error:      	Received unexpected error:
        	            	CONFLICT: proof: stream account:d27cc9d4-e747-4662-a78e-26b1b7628378
        	            	is not contiguous at seq 3; refusing to checkpoint a broken chain
```

### 9.3 Each remaining package on its own fresh database — the real result

```
=== ./internal/funding on controlplane_test_adv238isofunding ===
ok      github.com/nodal/controlplane/internal/funding          2.210s
=== ./internal/killswitch on controlplane_test_adv238isokillswitch ===
ok      github.com/nodal/controlplane/internal/killswitch       0.338s
=== ./internal/proof on controlplane_test_adv238isoproof ===
ok      github.com/nodal/controlplane/internal/proof            2.232s
=== ./internal/reconciliation on controlplane_test_adv238isoreconciliation ===
ok      github.com/nodal/controlplane/internal/reconciliation   34.326s
=== ./internal/risk on controlplane_test_adv238isorisk ===
FAIL    github.com/nodal/controlplane/internal/risk             7.012s
```

**Net: 74 of 75 integration packages pass. One genuinely fails.**

### 9.4 The one genuine integration failure

```
--- FAIL: TestIntegration_CountOrders_FromPersistedIntents (0.06s)
    integration_test.go:393:
        	Error Trace:	C:/Dev/Nodal/internal/risk/integration_test.go:391
        	            				C:/Dev/Nodal/internal/risk/integration_test.go:393
        	Error:      	Received unexpected error:
        	            	ERROR: null value in column "content_hash" of relation "trade_intents"
        	            	violates not-null constraint (SQLSTATE 23502)
        	Test:       	TestIntegration_CountOrders_FromPersistedIntents
FAIL    github.com/nodal/controlplane/internal/risk     7.012s
```

This reproduces on a clean, isolated, freshly migrated database, so it is not an isolation artifact. The test's
raw `INSERT INTO trade_intents (...)` at `internal/risk/integration_test.go:386` omits `content_hash`, which
migration **00605** made `NOT NULL`. The capital fixture was updated for 00605; the risk fixture was not. It is a
stale test fixture rather than a production defect — but the effect is that **`risk.CountOrders`, which feeds the
order-rate limit, currently has no passing database-backed test**, and that limit is a risk control.

It has been left failing rather than patched, per this document's rule against re-running until green. It is
**invisible to the unit and race tiers** because the file is `//go:build integration`.

---

## 10. E2E

### 10.1 Go, cross-process against the real `cmd/api` binary

```
$ go test -count=1 -timeout=30m -tags='integration e2e' -v ./test/e2e/
--- PASS: TestE2EBreaksAreDeclared (0.00s)
--- PASS: TestE2E_ConcurrentIdenticalCommands (0.39s)
--- PASS: TestE2E_ErrorContract (0.25s)
--- PASS: TestE2E_IdempotencyAcrossProcesses (0.23s)
--- PASS: TestE2E_CustomerJourney (0.29s)
--- PASS: TestE2E_SSEStreamDeliversAcrossProcesses (15.17s)
real    0m20.817s
EXIT=0
```

6 tests, 26 subtests, 0 failures. These build and start a real `cmd/api` child process per test and drive it
over HTTP. Evidence that the process boundary is real rather than simulated, logged by the suite itself:

```
concurrency_test.go:81: burst 1: 16 callers, 15 saw IDEMPOTENCY_IN_PROGRESS
```

**Negative controls — all 7 declared E2E breaks were run and every one made the suite fail:**

```
sse_no_frame               exit=1   FAILED-AS-EXPECTED
logout_not_revoked         exit=1   FAILED-AS-EXPECTED
error_body_leaks           exit=1   FAILED-AS-EXPECTED
journey_wrong_account      exit=1   FAILED-AS-EXPECTED
concurrency_serialized     exit=1   FAILED-AS-EXPECTED
idempotency_two_effects    exit=1   FAILED-AS-EXPECTED
cross_tenant_allowed       exit=1   FAILED-AS-EXPECTED
```

### 10.2 Browser (Playwright)

Run against a production `vite build` served by `vite preview`, proxying `/v1` and `/auth` to the `cmd/api`
binary on `127.0.0.1:18099` with a seeded local database. Sign-in is the real OIDC round trip through the dev
identity provider — no injected cookie.

```
$ npx --no-install playwright test
...
  51 passed (46.4s)
real    0m51.086s
EXIT=0
```

51 tests across `critical-paths.spec.ts` (15), `honesty.spec.ts` (9 + 9 per-route), `accessibility.spec.ts` (4 +
9 per-route), including *"trade: a quote the backend cannot produce is reported, not faked"*, *"no dead controls
anywhere in the application"* and *"no capability that is off is shown as a zero"*.

**Browser caveat:** the config uses `channel: "chrome"` — the host's installed Chrome — because Playwright's own
Chromium download times out on this network. One browser engine, one version, one OS. No Firefox, no WebKit, no
mobile.

---

## 11. chaos

### 11.1 The first attempt refused to run, and that was correct

`cp-redpanda` was **already paused** when this tier started — left that way by something on this host before this
session's chaos work began. The suite detected it and refused rather than producing a meaningless result:

```
PRE-CHAOS container health:
/cp-redpanda paused unhealthy

--- FAIL: TestChaos_BrokerStallNeverMarksOutboxPublished (0.09s)
    broker_stall_test.go:88:
        	Error:      	[]string{"healthy", "running"} does not contain "unhealthy"
        	Messages:   	cp-redpanda is "unhealthy" before the test; chaos tests need a healthy
        	            	stack to mean anything
```

Its `t.Cleanup` unpaused the container, restoring the shared stack.

### 11.2 The real run, on a healthy stack

```
PRE-CHAOS:
/cp-redpanda running healthy
/cp-postgres running healthy
/cp-minio running healthy

$ go test -count=1 -timeout=30m -tags='integration chaos' -v ./test/chaos/
--- PASS: TestChaos_ArchiveWriteRefusedIsNeverRecordedAsStored (10.91s)
--- PASS: TestChaosBreaksAreDeclared (0.01s)
--- PASS: TestChaos_BrokerStallNeverMarksOutboxPublished (16.67s)
--- PASS: TestChaos_ClockJumpNeverDuplicatesOrResurrects (0.26s)
--- PASS: TestChaos_DatabaseDiesMidTransaction (0.36s)
--- PASS: TestChaos_DuplicateDeliveryUnderConcurrencyAndDatabaseFault (0.30s)
--- PASS: TestChaos_RelayInstanceDiesMidPassNeverLosesOrDuplicates (0.25s)
real    0m31.198s
EXIT=0
```

Faults actually applied (from the suite's own log lines):

```
archive_refused_test.go:111: chaos: paused cp-minio
broker_stall_test.go:135:   chaos: paused cp-redpanda
```

### 11.3 Redpanda was returned to healthy — verified, not assumed

Immediately after the run:

```
$ docker inspect --format '{{.Name}} {{.State.Status}} {{.State.Health.Status}}' cp-redpanda cp-postgres cp-minio
/cp-redpanda running healthy
/cp-postgres running healthy
/cp-minio running healthy

$ docker exec cp-redpanda rpk cluster health
CLUSTER HEALTH OVERVIEW
=======================
Healthy:                          true
Unhealthy reasons:                []
Controller ID:                    0
All nodes:                        [0]
Nodes down:                       []
Leaderless partitions (0):        []
Under-replicated partitions (0):  []
```

The stack was re-checked again after the negative-control runs below and was still `running healthy`.

### 11.4 Negative controls — all 6 fire

```
relay_reports_stall_as_success     exit=1   FAILED-AS-EXPECTED
db_partial_write                   exit=1   FAILED-AS-EXPECTED
clock_jump_trusted                 exit=1   FAILED-AS-EXPECTED
rogue_relay_ignores_the_claim      exit=1   FAILED-AS-EXPECTED
archive_failure_swallowed          exit=1   FAILED-AS-EXPECTED
inbox_bypassed                     exit=1   FAILED-AS-EXPECTED
```

---

## 12. security — the strongest evidence in this document

```
$ go test -count=1 -timeout=20m -tags=integration -v ./test/security/
PASS: 39  FAIL: 0  SKIP: 0
ok      github.com/nodal/controlplane/test/security     6.425s
EXIT=0
```

39 top-level tests, 462 `=== RUN` lines including subtests. These are not static analysis: the API-level half
starts the real `cmd/api` binary and drives it over HTTP as two separate tenants plus an admin.

**Run twice against the same database with nothing cleaned in between** (decision D-029 — a suite that only
passes on a fresh database hides defects):

```
run 1: ok  github.com/nodal/controlplane/test/security  6.425s   EXIT=0
run 2: ok  github.com/nodal/controlplane/test/security  5.777s   EXIT=0
```

### 12.1 Sixteen declared negative controls, sixteen observed failures

`test/security/break_test.go` declares a closed catalog of 16 named "breaks". Setting `CP_SEC_BREAK=<name>` makes
the suite deliberately violate the property that guard defends; the run is then **expected to fail**. Every one
was executed here against the same database, scoped with `-run` to the test that reads it:

| Break | Exit | Outcome |
|---|---|---|
| `idor_targets_own_account` | 1 | failed as expected |
| `idor_absent_id_is_own_account` | 1 | failed as expected |
| `admin_probe_uses_privileged_session` | 1 | failed as expected |
| `idem_reuses_fresh_key` | 1 | failed as expected |
| `idem_cross_tenant_replay_by_same_actor` | 1 | failed as expected |
| `idem_concurrency_uses_distinct_keys` | 1 | failed as expected |
| `replay_uses_a_fresh_state` | 1 | failed as expected |
| `logout_probe_uses_a_new_cookie` | 1 | failed as expected |
| `ledger_cursor_targets_the_owning_account` | 1 | failed as expected |
| `injection_probes_unauthenticated` | 1 | failed as expected |
| `redaction_handler_removed` | 1 | failed as expected |
| `problem_body_renders_the_cause` | 1 | failed as expected |
| `sqlscan_treats_parameters_as_constant` | 1 | failed as expected |
| `prompt_user_text_into_system_policy` | 1 | failed as expected |
| `admin_self_approval_uses_a_second_operator` | 1 | failed as expected |
| `admin_step_up_probe_uses_a_stepped_up_session` | 1 | failed as expected |

**16 of 16.** This is the difference between tests that pass and tests that *can fail*. A green suite proves
nothing on its own; a green suite in which every guard has been individually shown to bite proves that the green
is load-bearing. `TestSecurityBreaksAreDeclared` additionally keeps the catalog from rotting — a name a test reads
must be declared, and a declared name must be read by some test.

Combined with §10.1 and §11.4, **29 negative controls were executed for this document and all 29 fired.**

### 12.2 A defect the suite reports rather than hides

`TestIDOR_DEFECT_RecordLookupDistinguishesForeignFromAbsent` passes because it asserts the **currently observed
wrong behaviour**, and logs it:

```
idor_test.go:158: FINDING: GET /v1/intents/{id} answers 403 FORBIDDEN for an existing foreign record
and 404 NOT_FOUND for an absent one; a customer can therefore test whether an id names a real trade intent
```

The same shape exists in `GetOrdersOrderId`, `GetFundingDepositsDepositId` and `PostIntentsIntentIdCancel`. This
is an open enumeration oracle: low severity, but real, and a reader should not mistake "security: 39/39 green"
for "no known IDOR-class issues".

---

## 13. load

`test/load/README.md` was read first. The API was started from the `CGO_ENABLED=0` binary on
`127.0.0.1:18099` against a dedicated seeded database (`controlplane_test_adv238load`, `scripts/seed`), with a
session obtained by driving the real dev-IdP login flow, and the four rate-limit budgets raised to `1000000/1m`
for the duration (they are per-environment configuration; defaults unchanged).

### 13.1 Measured

| Scenario | Load | Result |
|---|---|---|
| `public_surface` | 20 VUs, 30 s | **344,456 requests, 11,478.97 req/s**; 861,140 checks, **0 failed**; liveness p95 **2.27 ms**, readiness p95 **6.17 ms** (includes a database round trip); all 4 thresholds pass |
| `portfolio_read` | ramp to 25 VUs, 3 m 00.9 s | **10,893 requests, 0.00 % failed**; buying-power p95 **19.90 ms**, holdings p95 **24.77 ms**, ledger p95 **7.05 ms**; 21,786 checks, 0 failed |
| `sse_clients` | 200 concurrent, 1 m | **600 held connections, 100 % received a frame** (`sse_frames_received: 600 out of 600`); 1,200 checks, 0 failed |

`public_surface` reports `http_req_failed: 25.00%` by design: one of its four requests per iteration is an
anonymous call that **must** return 401. Thresholding on it would demand that authorization stop working.

### 13.2 `quote_load` — blocked, and blocked correctly

```
$ k6 run test/load/quote_load.js
checks_failed......: 50.00% 2401 out of 4802
✗ no 5xx  ↳  0% — ✓ 0 / ✗ 2401
http_req_failed....: 99.95% 2401 out of 2402
EXIT=99
```

Every one of the 2,401 quote requests was answered 503. Reproduced by hand:

```
$ curl -X POST .../v1/quotes/preview -d '{"account_id":...,"action":"ACQUIRE_NOTIONAL","notional_usd":"25.00"}'
{"type":"urn:problem:provider_unavailable","title":"Provider unavailable","status":503,
 "detail":"no execution venue adapter is configured, so no quote can be produced",
 "instance":"/v1/quotes/preview","code":"PROVIDER_UNAVAILABLE","request_id":"ddaf453d6b0db4e8e083e05df74df409"}
HTTP=503
```

The source is `internal/httpapi/handlers_trading.go:46-49`: with `Ports.Quotes == nil` the only alternatives to
refusing are inventing a quote or pretending success, and both are worse. **This row is blocked on provider
credentials (EB-005/010/011), not on any code change.** Latency was still recorded for the refusal path:
p95 **3.76 ms**.

### 13.3 `reservation_contention` — it runs, but its financial assertion is vacuous here

`test/load/README.md` states this scenario "cannot be measured yet" because it needs a quote. That is **not
correct** — it posts intents, not quotes, and it ran:

```
checks_total.......: 804     checks_failed: 0.00% 0 out of 804
intents_accepted...: 301
http_req_failed....: 24.56% 99 out of 403
http_req_duration..: avg=1.21s med=1.32s p(95)=1.96s
console: buying power before=10000.00 after=10000.00 reserved=0.00
EXIT=99
```

What it did measure, and it is worth having: 100 concurrent virtual users, 400 iterations, of which 100 shared a
single `Idempotency-Key`. The API answered **301 × 202 Accepted and 99 × 409**, and the database afterwards
contained exactly:

```
$ psql -c "select status, mode, count(*) from trade_intents group by status, mode;"
RECEIVED|PAPER|301
```

— i.e. the 100 concurrent callers holding one key produced **exactly one** intent
(`409 IDEMPOTENCY_IN_PROGRESS`). That is the exactly-one-effect property, observed at the HTTP boundary under
real concurrency.

What it did **not** measure is the property the script is named for. `reserved=0.00` and buying power unchanged:
the reservation is taken downstream by the execution worker, which was not running. Its
`http_req_failed < 0.01` threshold fails on the deliberate 409s — the same thresholding mistake the README
documents for `public_surface`. **So: the concurrency/idempotency half is measured; the "reserved never exceeds
buying power" half is not.** The authoritative evidence for that invariant is the PART 23 torture test in
`internal/capital` and `TestProp_ReservationsNeverOversubscribe` (§6), not this script.

---

## 14. migration, restore, audit, unknown submission

### 14.1 Migration — own database, own invocation

Run alone, never in the same `go test` invocation as anything else, because it drops and recreates the schema:

```
$ go test -count=1 -tags=integration -v ./test/integration/migrations/...   # DB: controlplane_test_adv238mig
--- PASS: TestIntegration_Migrations (31.09s)
    --- PASS: .../clean_schema:_everything_pending,_verify_passes_vacuously (5.12s)
    --- PASS: .../up_applies_all_migrations_and_records_checksums_atomically (17.10s)
    --- PASS: .../re-running_up_is_a_no-op (0.07s)
    --- PASS: .../verify_detects_a_tampered_checksum_and_a_missing_one (0.12s)
    --- PASS: .../down-to_guard:_allowed_below_the_protected_version_while_none_is_applied (0.08s)
    --- PASS: .../down-to_rolls_back_foundation_tables_and_their_checksums,_up_restores (7.93s)
    --- PASS: .../role_separation:_cp_app_has_DML_without_DELETE_and_no_DDL (0.30s)
    --- PASS: .../outbox_dedup_index_and_helper_triggers_behave (0.22s)
--- PASS: TestIntegration_ApplicationRolePrivileges (11.31s)
--- PASS: TestIntegration_StateChangeRequiresTransitionRow (0.52s)
ok      github.com/nodal/controlplane/test/integration/migrations       43.268s
EXIT=0
```

### 14.2 Restore drill

```
$ go run ./scripts/restoredrill
restoredrill: provisioned and seeded controlplane_test_drill_src
restoredrill: backup: 476773 bytes sha256=598dafbe91167861
restoredrill: restored into controlplane_test_drill_restored
restoredrill: boot: version source=700 restored=700 verify=ok
restoredrill: reconciliation dry-run: tables=97 rowcounts_match=true balance_drift_accounts=0 journal_hash_match=true
restoredrill: OK (10.087s)
EXIT=0
```

`dist/restore-drill.json`:

```json
{
  "dump_bytes": 476773,
  "dump_sha256": "598dafbe911678610c681f47cd1d23055a457a3e03fbc4a1efb6333222574fe7",
  "source_version": 700, "restored_version": 700,
  "migration_verify_ok": true, "row_counts_match": true,
  "ledger_balances_recomputed_match": true, "journal_hash_match": true,
  "source_journal_hash":   "6c5fd329e7019909e6b380d8806cb318f9fa9b0784f0ea0e0b4c33b849fe5f8b",
  "restored_journal_hash": "6c5fd329e7019909e6b380d8806cb318f9fa9b0784f0ea0e0b4c33b849fe5f8b",
  "ok": true
}
```

Local `pg_dump`/`pg_restore` only. The production procedure (RDS snapshot / PITR, application repointed,
reconciliation against external truth) has **never** been executed — see §15.

### 14.3 Audit verification

The first invocation, with no signer and no archive configured, told the truth about its own limits:

```
level=WARN msg="no audit signer configured: checkpoint/run will fail and verify only succeeds
               while no checkpoint exists"
verify: ok (1 streams, 304 events, 0 checkpoints covering 0 events)
```

Zero checkpoints means the signature and Merkle half of the chain was not exercised. So a LOCAL ECDSA key and a
filesystem archive were configured and a real checkpoint was written:

```
{ "created": true, "seq": 1, "leaf_count": 304, "streams": 1,
  "merkle_root": "yyD0e3HOlo+n3BcQjjWkXLNeKDL4ZPFbiHPga045/HQ=",
  "signer": "local-test", "signing_key_id": "local-test:cbc8fd85d2f6f2dd62440f80c057d73b",
  "archive_uri": "file:///.../audit-checkpoints/000000000001/01a0783c-747a-7b8b-b1b2-98f9ae6c4fb1.json" }
```

and then re-verified:

```
verify: ok (1 streams, 304 events, 1 checkpoints covering 304 events)
```

The chain, the checkpoint signature, the Merkle root and the archived object all verified over **304 real audit
events written by the load run**. The key was a local test key, not KMS, and the archive a directory, not an S3
Object Lock bucket — the tool says so itself in a `WARN` on every run.

### 14.4 Unknown submission (PART 48) and crash recovery (PART 49)

PART 48's rule — *a transport timeout is never an execution failure* — and PART 49's crash recovery, by name:

```
$ go test -tags=integration -run '...' ./cmd/execution-worker/     # own DB: controlplane_test_adv238unknown
--- PASS: TestIntegration_Worker_SubmitTimeoutIsNotFailure (0.90s)
--- PASS: TestIntegration_Worker_UnresolvedSubmissionIsPausedNotFailed (0.61s)

$ go test -tags=integration -run '^TestIntegration_Part49_CrashRecovery$' ./internal/reconciliation/
--- PASS: TestIntegration_Part49_CrashRecovery (0.58s)

$ go test -run '^TestExecutor_(ResumeAfterCrash_.*|SubmitTimeout.*|SubmitRejectedButLanded_.*)$' ./internal/settlement/
--- PASS: TestExecutor_SubmitTimeoutLanded_AdoptedNoDuplicate (0.05s)
--- PASS: TestExecutor_SubmitTimeoutLost_ProvenAbsent_NewAttemptOnce (0.06s)
--- PASS: TestExecutor_SubmitTimeoutLost_BudgetExhausted_OrderExpiresReservationReleased (0.06s)
--- PASS: TestExecutor_SubmitTimeout_Unresolved_ReconciliationRequired (0.07s)
--- PASS: TestExecutor_SubmitRejectedButLanded_IsUnknownNotFailure (0.05s)
--- PASS: TestExecutor_ResumeAfterCrash_EveryStepBoundary (0.01s)
--- PASS: TestExecutor_ResumeAfterCrash_SubmitRunningNeverResubmits (0.06s)
--- PASS: TestExecutor_ResumeAfterCrash_SubmitRunningTxLost (0.10s)
ok      github.com/nodal/controlplane/internal/settlement        0.473s
```

`TestExecutor_ResumeAfterCrash_EveryStepBoundary` alone contributes **51 fault-injected subtests** (17 steps × 3
phases), each crashing the executor at a different persistence boundary and asserting the resumed run neither
re-submits nor double-posts.

---

## 15. What these results do **not** prove

This is the most important section in the document. Everything above is real; none of it means what an
unqualified "all tests pass" would suggest.

### 15.1 The fuzzing is a smoke test, not a campaign

18.4 million executions sounds large and is not. It is 30 seconds per target on one laptop. A bounded fuzz run is
evidence that **no crashing input was found in that budget** — it is not evidence that none exists, and coverage-
guided fuzzing typically needs hours to leave the shallow neighbourhood of its seed corpus. The weakest target,
`internal/signing/inspect.FuzzInspect`, managed 9,479 executions; that function is the last thing standing
between a signed message and customer funds. Go's own counter also stalled on several targets (§7), so some of
these numbers may understate — but none of them can be trusted to overstate a *conclusion*.

### 15.2 The chaos suite tests the faults someone thought of

`test/chaos` injects six faults: a paused Postgres mid-transaction, a paused Redpanda, a paused MinIO, a clock
jump, a duplicated delivery, and a relay dying mid-pass. Every one is a fault a human imagined and encoded. It
contains **no** network partition between two live nodes, no partial write, no disk-full, no corrupted page, no
Byzantine provider, no slow-but-not-stopped dependency, no leader election failure, no DNS failure, no clock
skew *between* two processes. It runs against a single-node Postgres, a single-broker Redpanda and a single MinIO
on one machine, so no failure mode that requires more than one node of anything can even be expressed.

### 15.3 The load numbers are from one developer laptop

11,478 req/s on `public_surface` and 19.9 ms p95 on buying power were measured on a Ryzen 9 laptop talking to a
Postgres container on the same machine over loopback, against a database seeded with **one** customer,
**one** instrument and **one** ledger posting. There is no network, no TLS termination, no load balancer, no
connection-pool exhaustion, no noisy neighbour, no realistic data volume, no index degradation at scale. These
figures characterise the code path, not capacity. **Do not present any number in §13 as a capacity estimate.**
The 3-minute `portfolio_read` run is also far too short to reveal anything about pool leaks, memory growth or
GC behaviour.

### 15.4 The contract tests never touched a provider

All 53 contract tests in §8 replay **recorded fixtures** served by a local `httptest` server. No request left
this machine. The fixtures were authored from published specifications and vendor documentation, and several are
explicitly marked **assumed** in their own READMEs (Helius' `transactionSubscribe` payload shape, its error
bodies, Solana RPC's `addressTableLookups` content). A contract test against a fixture proves the adapter parses
*what we believe the provider sends*. It cannot detect that the belief is wrong. Jupiter, Privy and Stripe are
all still `BLOCKED_EXTERNAL` (EB-011, EB-005, EB-003) precisely because nobody has yet compared these fixtures
with a live response.

### 15.5 SB-007: the Jupiter instruction layout is unverified, so nothing here says anything about real Jupiter transactions

`internal/signing/inspect/jupiter.go` states it in its own header:

> *Source: the jup-ag/jupiter-cpi IDL for program JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4 as reproduced here
> from memory of that IDL; it is NOT verified against a live fetch in this build.*

The Anchor discriminators, account orders and Swap variant payload sizes the inspector parses were reproduced
from memory. `internal/provider/jupiter/transaction.go` carries the same `UNVERIFIED (blocker SB-007)` marker.
A previously believed corroboration was found to be circular and closed on 2026-09-06: the Jupiter fake's route
builder mirrored the same layout the inspector parses, both derived from one unverified source, so the two
agreeing looked like confirmation and proved nothing.

Consequently: **`FuzzDecode`, `FuzzInspect`, `FuzzSummarizeTransaction`, all 21 Jupiter contract tests and every
signing-boundary test in this document prove only that the code is self-consistent with an unverified
assumption.** None of them is evidence about a real Jupiter mainnet transaction. If the layout is wrong, the
inspector either rejects valid swaps (fail closed, tolerable) or misparses a variant and approves a transaction
whose real minimum-output or slippage differs from the one it checked (not tolerable). SB-007 must be closed
against the published IDL and real recorded mainnet transactions before any canary trade.

### 15.6 CI has never executed

`.github/workflows/ci.yml` (19,899 bytes) and `release.yml` (7,964 bytes) exist and have **never run**, because
there is no remote:

```
$ git remote -v
(no output)
```

Everything in this document was executed by hand, once, on one machine, by one operator, in one environment
(Windows). There is no evidence that any of it passes on Linux, in a container, on a clean checkout, or with a
cold module cache. There is no bisect history, no per-commit signal, and no independent reproduction. SB-004.

**Update, same day, after this section was written.** A remote was supplied and `main` pushed (`673d9bf`), and
`ci.yml` ran for the first time. It **failed in 10 of 18 jobs** — a pnpm version declared in two places at once,
a `make staticcheck` target that had never passed anywhere (33 findings, all in generated code or in method
names the generated interface dictates), and an integration job pointing every package at one shared database,
which cannot work for the reason recorded in §14 below. That first red run is the point: none of it was
reachable by reasoning about the workflow files, and every one of those failures was invisible until a real
runner executed them. **Until a green run exists, treat every CI claim in this document as unverified.**

Related, and true when this ran though no longer: SB-008 — the repository had 4 commits and 123 uncommitted entries. **The build
this document measures exists only in one directory on one disk.**

### 15.7 The integration tier has no single green invocation

§9 is not a formatting quirk. There is **no command that runs all 75 integration packages against one database
and passes.** In parallel, 11 packages fail; serialized, 5 fail; only per-package fresh databases give a real
answer. Suites that `TRUNCATE` shared tables and suites that assert on global row counts coexist in one tree.
That means "integration: green" is a claim about ~75 separate runs stitched together by hand — which is exactly
the kind of assembly that hides a regression, and exactly what CI would have caught if CI could run.

### 15.8 One integration test is failing right now

`internal/risk` `TestIntegration_CountOrders_FromPersistedIntents` fails on a clean database (§9.4). The
order-rate limit that feeds `risk` therefore has no passing database-backed test at this commit.

### 15.9 A known enumeration oracle is open

§12.2. `GET /v1/intents/{id}` (and orders, funding deposits, intent cancel) distinguish "exists but is not yours"
(403) from "does not exist" (404). The security suite reports it as a finding rather than hiding it, but it is
open.

### 15.10 Nothing here has touched money

Every execution path exercised in this document ran in `PAPER`/`LOCAL` mode against fake providers. No live
capability gate is ACTIVE; no real wallet was signed with; no real transaction was submitted to any chain; no
real fiat moved. The external P0 gates (EB-001 licensing, EB-002 custody analysis, EB-003 Stripe approval,
EB-005 Privy signing semantics, EB-006 permitted asset universe, EB-007 adviser/CTA implications) are all still
open, and no test result can close any of them.

### 15.11 Tests are written by the same author as the code

Every guard here was designed by someone who already had a model of how the system fails. The 29 negative
controls in §10.1, §11.4 and §12.1 are the strongest available answer to that — they prove the assertions *can*
fire — but they still only cover failures somebody imagined. No external red team, no independent audit, no
adversary with an incentive has looked at this system.

---

## 16. Reproducing this document

```bash
export PATH="/c/Dev/tools/go/bin:/c/Dev/tools/mingw64/bin:$HOME/go/bin:/c/Dev/Nodal/bin:$PATH"
export GOTOOLCHAIN=local CGO_ENABLED=1
docker compose up -d --wait

# build
go build ./... && go mod verify
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/nodal/controlplane/internal/config.BuildVersion=$(git rev-parse --short=12 HEAD)" -o bin/ ./cmd/...

# unit / race / property / fuzz / contract  (no database needed)
go test -count=1 ./...
go test -count=1 -race ./...
go test -count=1 -timeout=20m -run 'Prop|Property' ./internal/...
go run ./scripts/fuzzall -fuzztime=30s -v
go test -count=1 ./test/contract/...

# per-suite databases — never the shared controlplane_test
eval "$(go run ./scripts/testdb -name mysuite -export)"
export CP_TEST_REDIS_URL=redis://127.0.0.1:6380 \
       CP_TEST_REDPANDA_BROKERS=127.0.0.1:19092 \
       CP_TEST_CLICKHOUSE_ADDR=127.0.0.1:19000 \
       CP_TEST_ARCHIVE_ENDPOINT=http://127.0.0.1:9100

# integration: ONE PACKAGE PER FRESH DATABASE (see §9 for why)
go test -count=1 -tags=integration ./internal/<pkg>/

# migrations: its OWN database and its OWN invocation — it drops the schema
eval "$(go run ./scripts/testdb -name mig -export)"
go test -count=1 -tags=integration ./test/integration/migrations/...

# security (+ negative controls), e2e, chaos
go test -count=1 -tags=integration ./test/security/
CP_SEC_BREAK=idor_targets_own_account go test -tags=integration -run TestIDOR ./test/security/   # MUST fail
go test -count=1 -tags='integration e2e' ./test/e2e/
go test -count=1 -tags='integration chaos' ./test/chaos/     # verify `rpk cluster health` afterwards

# load: start bin/api.exe on :18099 against a seeded DB, obtain a session via /v1/auth/login
#       → /auth/dev/login?identity=customer-a&state=… → /v1/auth/callback, then:
BASE_URL=http://127.0.0.1:18099/v1 SESSION=<cp_session> COOKIE_NAME=cp_session ACCOUNT_ID=<uuid> \
  bin/k6.exe run test/load/portfolio_read.js

# migration/restore/audit
go run ./scripts/restoredrill
go run ./cmd/audit-worker verify
```

Playwright: `cd apps/web && npx --no-install playwright test`, with the API already listening on
`127.0.0.1:18099` against a seeded database.
