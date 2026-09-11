# ADR-0028 — A notification is a fact of the transaction that caused it, and realtime is one process's memory

Status: **Accepted** (2026-09-10)

Supersedes nothing. Constrains how the product tells a person something happened,
and how a browser learns about it without polling. Answers §34 (realtime
updates), §36 (notifications) and the user-visible half of §52 (audit trail) of
the product goal, under §42's constraint that fixed monthly infrastructure cost
stays at zero.

## Context

Three things existed and none of them worked.

**A `notifications` table with no writer.** Migration 00640 created it in
2026-09 with nine kinds, a dedup index, an immutability guard and a
`read_at` column. `internal/notification` implements a repository and a
dispatcher over it. A repository-wide grep for that package's import path
returns exactly one hit outside its own tests, and it is a comment. Nothing on
any code path has ever written a row. The traceability matrix has recorded this
as `IN_PROGRESS` with the words "no domain emits a notification" since the
matrix was written.

**An SSE endpoint with no producer.** `GET /v1/events/stream` is mounted,
authenticated, and re-checks its session on every heartbeat (F-116). The hub is
real, with per-principal filtering, monotonic ids and a bounded replay buffer.
`Hub.Attach` — the one method that subscribes it to the event bus — has no
caller anywhere. `cmd/api`'s own comment says so: *"No producer is attached: the
event bus adapter lives in a package this binary does not yet depend on, so the
stream carries heartbeats only until it is wired."* A client connects, stays
open, receives `: keepalive` every fifteen seconds, and never sees a domain
event.

**An outbox that never drains.** Domain events are written transactionally to
`outbox_events` and published by `event.NewRelay`, whose only production caller
is `cmd/relay-worker`. Render charges for a worker service, so `render.yaml`
deploys one web service and no workers. The rows accumulate.

Above all three sits the tier: **one free Render instance that spins down when
idle**, a free Neon database, and §42's requirement that fixed cost stays at
zero. There is no worker, no Redis, no queue, and adding one is the decision
§42 forbids.

There is also a constraint that is not technical. This work happened alongside
four other agents changing `internal/nativemarket`, `internal/payout`,
`internal/profile` and `internal/verification` in parallel. A design that
required an `Emit` call inside each of those packages could not be built without
editing files somebody else owned.

## Decision

### 1. A notification is written inside the transaction that caused it

`notifications.Producer.Emit(ctx, tx, n)` takes the **caller's** `pgx.Tx`. It
opens no transaction, retries nothing, and touches nothing outside the database.
A notification therefore exists if and only if the state change it describes
committed: there is no window in which somebody has been told about a purchase
that rolled back, and none in which a purchase committed and the telling was
lost to a crash between two transactions.

It is idempotent on `(user, kind, ref, occurrence)`, collapsed into the table's
`dedup_key` and its unique index. A replayed webhook, a retried command and a
follower re-reading its own window all produce one row. **That property, and not
a cursor, is what makes everything below safe.**

### 2. The producer is a transition-table follower running in the API process

`internal/notifications.Follower` reads rows that domain services already write
— `credit_funding_transitions`, `payout_request_transitions`,
`native_market_fills`, `native_market_transitions`,
`account_status_transitions`, and the `login` rows of `security_events` — and
turns the ones a person needs to know about into notifications. It edits no
domain package, which is why it could be built while four of them were being
changed by somebody else.

`cmd/api` runs it on a fifteen-second ticker, alongside `runCreditSettlement`,
`runOpsRetention` and `runInternalVerification`. That is not a new pattern; it
is the tier's established one, and D-046 states the reasoning: **the alternative
to running periodic work in the API process is not running it somewhere better,
it is not running it at all.**

Each pass is one transaction per source containing both its emits and its cursor
advance, so the two cannot disagree. Each takes a try-advisory-lock, so a worker
tier added later runs alongside it with no coordination.

**The cursor is an optimisation, not the correctness argument.** Every table it
follows orders by a timestamp defaulting to `now()`, which is the transaction's
*start* time — so a transaction that began before the cursor passed and
committed after it writes a row the cursor has already gone past. No ordering
fixes that. The follower therefore re-reads a bounded lap (two minutes) behind
its own position on every pass, and the dedup key refuses whatever the lap sees
twice. Late rows are found; duplicates are impossible.

### 3. Realtime is an in-process broadcaster, and it says so

The existing hub gains two event types — `notification.created` and
`data.changed` — and a per-**user** address, which `account:read_any` does not
open: an operator investigating an account reads the audit trail, and a copy of
what a customer was told is a different document.

`data.changed` carries a scope (`balance`, `position`, `market`, `payout`,
`account`) and a reference, and **no values at all**. A client invalidates the
query it names and refetches over REST, which is the only place a figure is ever
authoritative (PART 109).

**Event ids now encode the instant they were published at.** The counter used to
restart with the process, and the resume comparison did not treat a client's
higher position as a gap — so a browser reconnecting after a redeploy was
silently told it was up to date. With a time-encoded id, ids are monotonic across
restarts, `Last-Event-ID` names an instant, and the handler replays from the
`notifications` table what was written since it.

**The table is the durable resume cursor, and it is filtered by the instant the
ROW was written** — `notifications.inserted_at`, added by 00801 — **not by the
instant the thing it describes happened.** Those are two clocks and treating
them as one was F-186: `created_at` is copied from the source row, so a capture
from five minutes ago that the follower wrote thirty seconds ago was stamped
five minutes ago, and every client whose cursor stood between the two was told
about none of it. The live id is a third clock, the publish instant, which is
always at or after the insert. The gap between them is why the handler replays
the buffer's notifications as well as the table's rather than trimming the two
to meet: a notification delivered twice costs one query invalidation, and one
skipped is a person never told. See D-104.

Streams are capped at four per person. One process, one free instance, and a
stream is one request that never ends.

### 4. The outbox relay is NOT run in the API process

Nothing user-facing depends on it. The outbox's topics are ledger, capital,
intent, order, execution-attempt, fill and reconciliation events; the internal
economy — Credits, native markets, payouts, commerce — writes none of them. The
only user-facing consumer that would have existed is `Hub.Attach`, and the
follower replaces it with something that does not need a broker to be running.
Adding a relay loop would drain rows to a Redpanda that this tier does not have.
See D-072.

### 5. There is one delivery channel and the schema says so

No e-mail, SMS or push provider exists anywhere in this codebase.
`notification_preferences.channel` has a CHECK admitting `IN_APP` and nothing
else, and the API reports the channel on every preference. Five kinds are not
suppressible — a new sign-in, an account restriction, a reversed purchase, a
failed payout, a system message — and the read side reports `enforced: false` on
the rest so a settings page never shows a switch that silently does nothing.

## Consequences

- The notification centre is reachable at `GET /v1/me/notifications` and four
  sibling routes, all scoped to the caller with no `account:read_any` mode.
- `GET /v1/events/stream` carries events for the first time since it was built.
- A notification arrives up to fifteen seconds after the fact, plus however long
  the client's own reconnect takes. That is the cost of a poll and it is
  recorded rather than hidden.
- A free instance that is asleep runs no passes. The backlog drains when it
  wakes, because the cursor survives in the database and the first pass runs at
  startup rather than only on the tick.
- **Multiple instances would need Postgres `LISTEN`/`NOTIFY`** to fan a
  notification out to a stream held by a different process. This tier has one
  instance and `CP_HTTP_REPLICAS=1`, so today a client is always connected to
  the process that produced its event. D-071 records why LISTEN was not built
  now and what it would take.
- Preferences suppress creation, not display. A suppressed kind leaves no row,
  and the state change's own record — the transition row, the audit event — is
  untouched. What the customer was told and what the platform did remain two
  separate records, by design.

## What this ADR does not decide

It does not make the stream authoritative; PART 109 stands. It does not give
`internal/notification` (singular) a caller or take its tests away; D-070
records how the two packages coexist and what retiring the older one would
mean. It does not add an external delivery channel, and it does not deploy a
worker.

## Evidence

- `internal/notifications` — unit tests (kinds, dedup keys, validation,
  suppressibility, cursors) and integration tests: emit-in-the-caller's-
  transaction, idempotence per occurrence, preference suppression, follower
  cursor semantics (a re-read window after a crash duplicates nothing; a row
  that committed behind the cursor is not lost; the first pass does not replay
  history), and every source query executed against the migrated schema.
- `internal/stream` — per-user visibility (an operator with `account:read_any`
  is refused another person's notification), ids monotonic across a restart,
  resync whenever continuity cannot be proven, the concurrency cap, durable
  resume, an event delivered to a connected client after a publish, and (F-185)
  a publish that survives the subscriber disconnecting mid-send: the hub never
  closes a channel it also sends on, and reports a departure on a second
  channel instead.
- `internal/httpapi` — the routes, their authorization, the nil-port refusals,
  and the `me/audit` reader against the real `security_events` and
  `audit_events` schema.
- `test/integration/enums` — `notifications.kind` and `notifications.severity`
  are paired against Go for the first time; both leave the `unpaired` inventory.
- Register: D-069, D-070, D-071, D-072. Migrations 00781, 00782, 00783.
