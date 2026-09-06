# Load tests (k6)

Scripts in this directory target the versioned REST API (`openapi/openapi.yaml`). They require a running API (`cmd/api`, pending) with fake or sandbox providers, a LOCAL session cookie, and an account id. **Do not run them against live provider keys.**

| Script | What it measures | PART |
|---|---|---|
| `portfolio_read.js` | authoritative buying power / holdings / ledger reads (never cached as truth) | 25, 154 |
| `quote_load.js` | quote preview throughput through the rate limiter and provider health tracker | 42, 154, 180 |
| `reservation_contention.js` | 100 VUs submitting PAPER intents on one account; idempotent replay every 10th call; reports reserved vs buying power | 22, 23, 36, 154 |
| `sse_clients.js` | 200 concurrent SSE connections; heartbeat keeps them alive | 109, 154 |

```
BASE_URL=http://127.0.0.1:8080/v1 SESSION=<cookie> ACCOUNT_ID=<uuid> go run ./scripts/tool k6 run test/load/portfolio_read.js
```

Results are recorded in `docs/build/MASTER_BUILD_STATE.md` §12 only with commit, host, dataset size, and provider mode. **No numbers have been measured yet**: the API binary does not exist (Stage 14/17 pending). Do not invent figures.

## Measurement status (2026-09-06)

**First real measurements taken.** `cmd/api` now builds to `bin/api.exe`; it was run against a seeded local database with a real dev session, and the rate limits were raised for the duration (they are configurable per environment, defaults unchanged).

### Measured and clean

| Scenario | Load | Result |
|---|---|---|
| `public_surface` | 20 VUs, 30s | 495,868 requests, **16,527 req/s**, liveness p95 **1.62 ms**, readiness p95 **4.56 ms** (includes a database round trip), 1,189,830 checks all passing |
| `portfolio_read` | 25 VUs, 3m | 10,929 requests, **0.00% failed**; buying power p95 **19.79 ms**, holdings p95 **24.07 ms**, ledger p95 **6.55 ms** |
| `sse_clients` | 200 concurrent, 1m | 600 held connections, **100% received a frame**, no client accepted-then-starved |

The error contract held under load: every refusal was `application/problem+json` with a machine-readable code and a request id, and none leaked a DSN, SQL or a secret.

### Blocked, and why — this is not a capacity limit

`quote_load` and `reservation_contention` cannot be measured yet. Both need a quote, and `POST /quotes/preview` answers `503 PROVIDER_UNAVAILABLE — "no execution venue adapter is configured, so no quote can be produced"`. **That is the correct behaviour**: with no venue wired, the only alternatives are inventing a quote or pretending success, and both are worse than refusing. Measurement waits on provider credentials (EB-005/010/011), not on any code change here.

### Three defects in these scripts, found by running them

Recorded because each made a script report something untrue, which is worse than not running it.

1. **`sse_clients` had a check that could never pass and one that hid it.** It asserted `Content-Type` starts with `text/event-stream`, and failed 600 out of 600 times against an endpoint that demonstrably returns exactly that. k6 has no streaming client: it aborts the request with `timeout` and then reports `status: 0` with an **empty header map**, though it does hand back the bytes received. Verified directly — a 20s request returns `status=0 header_count=0 body_len=13`. The header assertions now live in the Go suite, which sees a real connection, and this file asserts only what k6 can observe.
2. **`quote_load` and `reservation_contention` fetched `/instruments` anonymously** in `setup()`, using a bare `http.get` instead of `params()`. The 401 surfaced as `Error: no instruments available`, which reads like missing data rather than an auth failure. `params()` also had to stop referencing `__VU`/`__ITER`, which do not exist in the setup stage.
3. **`lib.js` sent no Fetch Metadata**, so every mutating request was refused `403 cross-site request refused`. CSRF here is `Sec-Fetch-Site`, not a token, and a browser sets it automatically while k6 does not. Sending `same-origin` is accurate emulation of the caller these scenarios represent; the guard still rejects anything claiming a cross-site origin.

Also note `public_surface` deliberately does **not** threshold on `http_req_failed`: one of its four requests per iteration is an anonymous call that must return 401, and thresholding on it would demand that authorization stop working.
