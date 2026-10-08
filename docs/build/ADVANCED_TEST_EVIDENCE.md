# ADVANCED TEST EVIDENCE

The recorded test results for the advanced capabilities (ADR-0020), with the commit and date of each run.
Nothing here is a claim about a run that did not happen: a tier that has not been run as a whole at one
commit says so. Counts of declared tests come from `go run ./scripts/testcensus`, which counts and runs
nothing; whether tests pass is what the commands below report.

## How to reproduce

| Tier | Command |
|---|---|
| Unit, property and fuzz seeds (whole repository) | `go test -count=1 ./internal/... ./cmd/... ./scripts/... ./test/...` |
| Race | `go test -race -count=1 ./internal/<pkg>/...` |
| Integration (fresh, fully migrated database per package) | `go run ./scripts/inttest -pkg '^\./internal/<pkg>$'` (add `-race`, `-run`) |
| Enum CHECK ↔ Go list pairing | `go run ./scripts/inttest -pkg '^\./test/integration/enums$'` |
| Fuzz campaign | `go run ./scripts/fuzzall -fuzztime 30s -pkg '<regexp>'` |
| End-to-end scenarios | `make advanced-demo` (`go run ./scripts/advanceddemo`), report in `dist/advanced-demo.json` |
| Performance targets | `make perf-advanced` (`go run ./scripts/advperf`), report in `dist/advanced-perf.md` |
| Backup and restore | `go run ./scripts/restoredrill -pg-bin <PostgreSQL 16 client bin>` |
| Structural guards | `go test ./test/reachability/ ./test/docs/ ./test/security/ ./test/source/` |
| Web | `pnpm --filter @controlplane/web typecheck`, `... test`, `... build`; Playwright specs in `apps/web/e2e/` |
| Declared-test census | `go run ./scripts/testcensus -advanced` |

## Declared tests (census at `f6c2c5f`, 2026-10-08)

`go run ./scripts/testcensus -advanced` — the advanced packages (23 present at this commit; `internal/ledger`
and `internal/risk` include the base kernel's tests as well):

| Category | Count |
|---|---:|
| Unit | 553 |
| Property (`TestProp_`, rapid) | 110 |
| Fuzz targets | 27 |
| Integration (real PostgreSQL) | 333 |
| **Total** | **1,023** |

The whole repository (`go run ./scripts/testcensus`): 3,580 declared Go tests in 126 packages (2,440 unit,
179 property, 70 fuzz, 891 integration); web: 137 unit tests and 88 Playwright tests. HTTP handler and
over-HTTP integration tests of the advanced routes live in `internal/httpapi` and are counted there, not in
the advanced packages. Subtests and rapid cases are not counted separately.

## Recorded runs

| Date | Commit | Tier | Result |
|---|---|---|---|
| 2026-10-08 | `c3115ac` | Unit, whole repository | 116 packages ok, 44 without tests, **0 failures** |
| 2026-10-08 | `53b3480` | `make advanced-demo` | Scenarios 0, A, B, C, D, G, H, I: **79 checks, all hold** |
| 2026-10-08 | `f46d780` | Integration: `internal/stableasset`, `internal/bridge` | all passed (incl. `TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld`) |
| 2026-10-08 | `17b2982` | Integration + race: `internal/sandboxchain` | all passed |
| 2026-10-08 | `e434367` | Integration: `internal/httpapi -run 'Marketplace\|Strateg\|Deriv\|Predict\|Margin\|Advanced\|Transfer'`; `test/security -run IDOR` | all passed |
| 2026-10-08 | `e434367` | Web typecheck, unit (137), build | all passed |
| 2026-10-08 | `d8cbf00` | Integration: `internal/margin`, `test/integration/enums`; `internal/httpapi -run Margin` | all passed |
| 2026-10-08 | `d8cbf00` | Restore drill | version 754, 220 tables, row counts identical, 0 drift, journal hashes equal |
| 2026-10-07 | `c4defed` | `make perf-advanced` (smoke, shared machine) | every measured target met; a recorded run on an idle machine is pending |

Each domain lane ran its package's unit, race and integration suites green before it was merged, and each
API lane ran its handler tests, its over-HTTP integration test and the IDOR probe. Playwright specs were run
against a live API on separate ports by the prediction-market (with the core spec), derivatives (11/11) and
marketplace (19/19) lanes; the margin lane's spec was typechecked but not run against a live API. Those runs
are recorded in the lanes' reports, not repeated here.

## Not yet run as a whole

- The integration suite of every package in one run at one commit (it has run package by package at merge).
- `-race` across every advanced package in one run.
- A bounded fuzz campaign over the advanced fuzz targets (`scripts/fuzzall`).
- The Playwright suite as a whole against one live API on main.
- A recorded `make perf-advanced` on an otherwise idle machine.

These are run, and this table updated, when the lanes in flight (MASTER_BUILD_STATE PART -2) have merged.
