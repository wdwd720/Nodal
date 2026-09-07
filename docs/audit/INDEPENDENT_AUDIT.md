# INDEPENDENT ADVERSARIAL AUDIT

Baseline: `b8da0c4ce73687e587b7d20c8abc00440afd694d`.

This document exists because `gola.md` PART VII says to treat the previous effort's readiness claims
as a hypothesis to be disproved. Nothing in `docs/build/MASTER_BUILD_STATE.md`,
`REQUIREMENTS_TRACEABILITY.md` or `PRODUCTION_READINESS_REPORT.md` was accepted as evidence here. Every
row below records a claim, what was actually executed against it, and the result — including the
results that vindicated the previous work, because an audit that only reports failures is not an audit.

Format per PART XCI: claim · evidence · test performed · result · defect · fix · remaining risk.

---

## A. What was executed

| Suite | Command | Result |
|---|---|---|
| Compile | `go build ./...` | exit 0 |
| Vet | `go vet ./...`, `go vet -tags=integration ./...` | exit 0 |
| Unit | `go test -short -count=1 ./internal/...` | 62 packages ok, 0 fail |
| Integration | every `//go:build integration` package, **one fresh database each** (40 packages) | 40/40 ok |
| Migrations | `go test -tags=integration ./test/integration/migrations/...` | ok |
| E2E | `go test -tags=integration,e2e ./test/e2e/...` | ok |
| Chaos | `go test -tags=integration,chaos ./test/chaos/...` | ok |
| Contract | `go test ./test/contract/...` | 6 packages ok (eventtopics, helius, jupiter, privy, solanarpc, stripe) |
| Security | `go test -tags=integration ./test/security/` | **1 failure** → AUD-002 |

## B. Findings

### AUD-001 — `make integration` ran 1 package while claiming to run the integration suite · **FIXED**

**Claim audited.** `MASTER_BUILD_STATE.md` and the Makefile present `make integration` (and therefore
`make test-all`) as the integration proof of the financial core.

**Test performed.** Ran `make integration` and enumerated what it actually executed.

**Result — defect.** The target was `go test -count=1 -timeout=30m -tags=integration
./test/integration/...`, and `./test/integration/` contains exactly one package: the migration suite.
The database-backed proof of the financial core — ledger, capital, settlement, execution,
reconciliation, signing, httpapi, gates, killswitch, and 31 more — lives in `*_test.go` files behind
`//go:build integration` **inside `internal/` and `cmd/`**. 40 packages. `make integration` ran none
of them, and `make test-all` reported success having executed none of them.

This is a partially-known defect: CI had already grown an inline shell loop that enumerates the build
tag correctly, with a comment reading *"until that target takes a package list, the exclusion lives
here"*. So CI was sound and the local developer command was not, and the two definitions of "the
integration tests" could drift silently.

**Fix.** `scripts/inttest` — one Go program that enumerates the build tag, provisions a fresh database
per package (they cannot share one: `internal/event` truncates `outbox_events` while other suites
assert on global row counts, and the migration suite drops the schema mid-run), runs each, and
refuses to run at all if it finds fewer than two packages, so a broken enumeration fails loudly
instead of passing in a tenth of a second. `make integration`, `make integration-list` and
`make integration-race` call it. CI's inline loop can now call the same program.

**Verified.** `go run ./scripts/inttest -list` → 42 packages. Full run: 40/40 internal packages green,
plus the migration and security suites.

**Remaining risk.** None for enumeration. The suites themselves are only as good as their assertions,
which is what AUD-002 is about.

---

### AUD-002 — the cross-tenant ledger cursor test could not reach its own assertion · **FIXED**

**Claim audited.** `test/security/ledger_cursor_test.go` claims to prove SEC-003: a forged pagination
cursor cannot walk another tenant's journal.

**Test performed.** Ran the security suite on a database that other suites had already used.

**Result — defect.**

```
--- FAIL: TestLedger_AForgedCursorCannotCrossTenants
    this test assumes the seeded journal posting belongs to customer-a;
    it belongs to 01a07d53-0eea-7ac8-9efb-496fd9bd743e
```

The helper selected the *first* customer-owned journal transaction in the table
(`ORDER BY posted_at, id LIMIT 1`) and then asserted that it happened to belong to customer-a. The
moment any other suite posts a journal row first, the precondition fails — and it fails **before** the
cross-tenant assertion runs. The property the test exists to prove was therefore unproven.

The product is not defective: the API takes the owner as a bound parameter and the cursor only as a
keyset position, and once the fixture picks the right row the assertion passes. The defect is that
the test could not demonstrate that. Note the asymmetry that made this worth fixing rather than
shrugging at: a security test that stops before its assertion *fails*, which is survivable; the same
test passing while proving nothing would not be.

**Fix.** `aForeignJournalRow` became `aJournalRowOwnedBy(t, accountID)`, selecting by owner instead of
asserting about whichever row sorted first.

**Verified.** Security suite green, and green on **three consecutive runs against the same database**
— the re-runnability check that the previous session's notes record as having caught a real
key-rotation defect once before.

**Remaining risk.** None known. The test retains its negative control (`secBreak`), which proves the
assertion can see a leak rather than merely seeing an empty page.

---

### AUD-003 — the value-domain isolation check was direction-blind · **FIXED (defect in new code)**

Found while building `internal/valuedomain`, and recorded here because it is exactly the class of
defect this audit is looking for, and being self-inflicted does not make it less real.

**Defect.** The first draft of the isolation check took only the *set* of value domains a transaction
touched and permitted the pair if a conversion was declared in **either** direction. But
`PAYOUT_PENDING → INTERNAL_CREDIT` is deliberately ungated, so that an in-flight payout can always be
unwound (PART XXXII: stopping new risk must not stop required unwind workflows). A direction-blind
check therefore let that ungated return path authorise the gated `INTERNAL_CREDIT → PAYOUT_PENDING`
reserve step — defeating the `PAYOUT_RESERVE` capability entirely for anyone able to post a two-domain
transaction.

**Caught by.** `TestIsolation_PayoutPathIsGatedAtBothSteps`, which failed on the assertion that
holding only `PAYOUT_RESERVE` must not permit the settle step. Writing the gating test *before*
believing the design is what surfaced it.

**Fix.** `CheckConversion(from, to, caps)` is now directional and is the authority. `CheckPosting`
additionally requires that a cross-domain transaction **declares** which conversion it is, and that
the declaration matches the domains present — so a cross-domain movement is a stated intent recorded
in the journal, never an emergent property of which accounts happened to be involved. Regression test:
`TestIsolation_DirectionIsLoadBearing`.

---

### AUD-004 — verified: no float in financial code, Redis is not financial authority

**Claim audited.** PART VIII forbids Redis as financial authority and float for money; the previous
docs claim both hold.

**Test performed.** Grep over `internal/` for `float32|float64` excluding latency/metric contexts;
grep for `redis` across `internal/` and `cmd/`. Ran `internal/money`'s own
`TestNoFloatingPointInSource`, which greps the package's source for float literals.

**Result — claim upheld.** The only `float64` occurrences in `internal/` are: a doc comment in
`audit/canonical.go` explaining why floats are rejected, a JWT `exp` claim conversion in
`auth/oidc/verify.go`, a test OIDC server, and `solanarpc/wire.go` where it documents that the decoder
*rejects* floats. No financial quantity touches a float anywhere. Redis appears only in
`internal/ratelimit` and `internal/reality/datasource` — a rate limiter and a cache — never as a
balance, reservation or ledger source.

**Remaining risk.** None. `internal/money` is exact-integer throughout (`int64` minor units for USD,
`big.Int` with explicit scale for quantities, seven rounding modes, no implicit conversion).

---

### AUD-005 — verified: the ledger's invariants are enforced by PostgreSQL, not only by Go

**Claim audited.** That the double-entry invariants hold against anything holding the app credential.

**Test performed.** Read `migrations/00101_ledger.sql` and `00604`, then exercised the triggers by
posting hand-written SQL through the **migration role** (the schema owner, the most privileged
credential in the system) rather than through the service.

**Result — claim upheld, and stronger than documented.** Balanced-per-asset and minimum-entry-count
are deferred constraint triggers checked at commit; negative balances are refused by a
`SECURITY DEFINER` trigger the app role cannot bypass; `journal_transactions` and `journal_entries`
carry immutability triggers; `cp_app` holds `UPDATE` on `ledger_accounts` only for the `status`
column; `ledger_balances` is maintained solely by the trigger. Migration 00701 had already moved the
capability gate's five-condition activation into the database as well.

**Remaining risk.** The chart of accounts is duplicated between Go (`codeRegistry`) and SQL (a `CHECK`
constraint). They are now both extended for the internal economy and a test pins the Go side's shape,
but nothing mechanically proves the two lists are identical. Recorded as a follow-up, not a defect.

---

### AUD-006 — the audit's dominant finding is absence, not defect

40 of 40 integration packages, the migration suite, the e2e suite, the chaos suite and six contract
suites pass on fresh databases. The financial core the previous effort built is sound: exact money,
DB-enforced double entry, transactional reservations, an outbox with inbox dedupe, deterministic
risk/eligibility separation, hash-chained evidence, independent Solana transaction decoding.

Measured against `gola.md`, the problem is that this is Domain B and Domain C only. **Domain A — the
entire Nodal-native economy — does not exist**: no Credits, no native assets, no market engine, no
creator economy, no payout eligibility engine, no value-domain type system, no legal router, no agent
authority levels. See `docs/build/CURRENT_SYSTEM_INVENTORY.md` §8 for the grep evidence.

That is the migration this project is now executing, and it is tracked stage by stage in
`MASTER_BUILD_STATE.md` rather than here.

---

## C. What this audit has NOT yet done

Stated explicitly so that no reader mistakes silence for a pass:

- Load testing (PART LXXIV) — not yet run.
- Backup/restore drill (OPS-001) — `make restore-drill` exists and has not been executed in this session.
- `govulncheck`, `gosec`, `gitleaks`, `trivy`, SBOM — not yet run in this session.
- Terraform validation — not yet run in this session.
- The adversarial test list of PART LXXII (30 scenarios) — items 1–30 are being implemented alongside
  the subsystems they attack; coverage is tracked in `docs/audit/AUDIT_FINDINGS.md`.
- Frontend honesty review (PART LXXV) — the Domain A surfaces do not exist yet.
