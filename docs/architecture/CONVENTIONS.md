# ENGINEERING CONVENTIONS

Binding for every package in this repository. Subagents and humans alike read this before writing code.

## Toolchain on the primary dev host (Windows)

- Go 1.27.0 at `C:/Dev/tools/go/bin` (also installed by winget at `C:/Program Files/Go/bin`); GCC 16.2 (WinLibs) at `C:/Dev/tools/mingw64/bin` for cgo/`-race`; GNU make and Terraform installed by winget under `%LOCALAPPDATA%/Microsoft/WinGet/Packages/{ezwinports.make_*/bin,Hashicorp.Terraform_*}` (new shells pick them up via the WinGet Links directory). Git Bash: `export PATH="/c/Dev/tools/go/bin:/c/Dev/tools/mingw64/bin:$HOME/go/bin:/c/Dev/Nodal/bin:$LOCALAPPDATA/Microsoft/WinGet/Links:$PATH" GOTOOLCHAIN=local CGO_ENABLED=1`. Production binaries are still built with `CGO_ENABLED=0`; cgo is only for the race detector and tooling.
- Do **not** run `go get` or `go mod tidy` in parallel work streams: dependencies are pre-pinned in `go.mod` by the integrator. If you need a new module, stop and report it.
- Format with `gofmt` (gofumpt when available in `./bin`).
- Local infra: `docker compose up -d --wait` (Postgres `127.0.0.1:5433`, Redis `6380`, Redpanda `19092`, ClickHouse `18123`, Temporal `7233`, MinIO `9100`).
- Test database DSNs (LOCAL only):
  - migrate role: `postgres://cp_migrate:cp_migrate_local@127.0.0.1:5433/controlplane_test?sslmode=disable`
  - app role: `postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane_test?sslmode=disable`
  - Integration tests read `CP_TEST_DATABASE_URL` (app) and `CP_TEST_MIGRATE_DATABASE_URL` (migrate); when unset they `t.Skip` with an explicit reason. Build tag `integration`.

## Module and layout

- Module: `github.com/nodal/controlplane`. Go `1.27`.
- Binaries in `cmd/<name>/main.go`. Domain packages in `internal/<domain>`. Test-only fakes in `internal/testkit/...` or `<pkg>/<pkg>test` packages, never importable by production wiring in PROD (enforced by `config.Environment` checks and a `go vet`-style test in `test/security`).
- Scripts are Go programs under `scripts/<name>/main.go`.
- Package doc comment in `doc.go` for every package stating its responsibility and what it must never do.

## Non-negotiables (from the goal)

1. **No floating-point financial values.** Money is `money.USD` (int64 minor units), `money.BPS` (int64 basis points), `money.Quantity` (arbitrary-precision base units), `money.Price` (mantissa/scale/quote/source/timestamp). Every rounding call names a `money.RoundingMode`.
2. **Context first**: every I/O function takes `ctx context.Context` as first arg; honour cancellation; set timeouts at boundaries.
3. **Errors**: return `*errs.Error` (or wrap with `errs.Wrap`) for any error that can reach an API boundary. Stable `errs.Code` constants. Never expose stack traces. Never panic on external input; fuzz parsers.
4. **No global mutable state** (no package-level vars holding connections/config). Dependency injection via constructors taking interfaces.
5. **Time**: UTC everywhere; `clock.Clock` injected; DB columns `timestamptz`; JSON RFC3339 with nanosecond precision where needed. Distinguish `occurred_at`, `provider_published_at`, `received_at`, `processed_at`, `available_at`, `settled_at`, `finalized_at` where relevant.
6. **IDs**: `id.ID[K]` UUIDv7, typed per aggregate. Provider IDs are external references stored in separate columns. Never use ticker/email/address/provider id as primary identity.
7. **Idempotency**: every money-affecting command carries an idempotency key; every inbound provider event is inserted into the inbox under a unique constraint before processing.
8. **Transactions**: financial state changes + outbox event in the same Postgres transaction via `db.InTx` / `db.Serializable`. Never Redis/Temporal/ClickHouse/memory as financial truth.
9. **Agent authority**: agent-facing packages import only read tools, prediction, and intent creation. They never import `signing`, `wallet`, `admin`, `capital` (mutation), `risk` (policy mutation).
10. **Logging**: `slog` via `observability.Logger`; never log secrets (redaction handler); always attach `request_id`/`correlation_id` and domain ids from context.
11. **No TODO/FIXME** in ledger, capital, risk, eligibility, execution, signing, reconciliation, auth, security, gates, audit. Model unresolved external dependencies as typed capability/provider state instead.

## Shared foundation APIs (Stage 1 contracts)

These signatures are fixed so packages can be written in parallel. Implementers may add methods, not change these.

### `internal/id`

```go
type ID[K any] struct{ /* 16 bytes, UUIDv7 */ }
func New[K any]() ID[K]                      // UUIDv7 from crypto/rand + wall clock (ms), monotonic within process
func NewAt[K any](t time.Time) ID[K]
func Parse[K any](s string) (ID[K], error)   // canonical 36-char form only
func MustParse[K any](s string) ID[K]
func (ID[K]) String() string; IsZero() bool; Time() time.Time; Bytes() [16]byte
// encoding.TextMarshaler/TextUnmarshaler, json, database/sql driver.Valuer + sql.Scanner ([16]byte and string forms)
// Domain kinds are declared where the aggregate lives, e.g. in internal/accounts:
//   type accountKind struct{}; type AccountID = id.ID[accountKind]; func NewAccountID() AccountID { return id.New[accountKind]() }
```

### `internal/clock`

```go
type Clock interface { Now() time.Time }          // always UTC, wall clock
func System() Clock
type Fake struct{ ... }                           // NewFake(t), Set(t), Advance(d), Now()
func Elapsed(start time.Time) time.Duration       // monotonic-safe duration (uses time.Since semantics)
```

### `internal/errs`

```go
type Code string
// Codes (stable, machine-readable). At minimum:
// INSUFFICIENT_BUYING_POWER, ACCOUNT_FROZEN, ASSET_RESTRICTED, VENUE_UNAVAILABLE, QUOTE_EXPIRED, RISK_MAX_POSITION,
// RISK_DAILY_LOSS, ELIGIBILITY_JURISDICTION, CAPABILITY_NOT_APPROVED, SUBMISSION_STATE_UNKNOWN, RECONCILIATION_REQUIRED,
// PROVIDER_UNAVAILABLE, STALE_MARKET_DATA, INVALID_IDEMPOTENCY_REUSE, IDEMPOTENCY_IN_PROGRESS, VALIDATION_FAILED, NOT_FOUND,
// UNAUTHENTICATED, FORBIDDEN, STEP_UP_REQUIRED, CONFLICT, RATE_LIMITED, INTERNAL, INVALID_STATE_TRANSITION, OVERFLOW,
// PRECISION_LOSS, UNSUPPORTED, KILL_SWITCH_ACTIVE, NO_VALID_PLAN
type Error struct { Code Code; Detail string; Fields map[string]any; RetryAfter *time.Duration; cause error }
func New(code Code, detail string) *Error
func Newf(code Code, format string, a ...any) *Error
func Wrap(err error, code Code, detail string) *Error   // preserves cause; errors.Is/As/Unwrap supported
func (e *Error) WithField(k string, v any) *Error
func CodeOf(err error) Code                              // INTERNAL for unknown errors
func HTTPStatus(code Code) int                           // deterministic mapping
type Problem struct { Type, Title string; Status int; Detail string; Instance string; Code Code; Fields map[string]any; RequestID string }  // application/problem+json
func ToProblem(err error, instance, requestID string) Problem   // never leaks internal detail for INTERNAL
```

### `internal/money`

```go
type RoundingMode int // RoundDown (toward zero), RoundUp (away from zero), RoundHalfEven, RoundHalfUp, RoundFloor, RoundCeil — every rounding API takes one explicitly
type USD struct{ minor int64 }           // exact minor units; checked arithmetic returns error on overflow
func USDFromMinor(int64) USD; func ParseUSD(s string) (USD, error) // "1234.56", "-0.01"; rejects >2 decimals unless mode given via ParseUSDRound
func (USD) Minor() int64; String() string /* "1234.56" */; IsZero/IsNegative/IsPositive; Neg() USD
func (USD) Add(USD) (USD, error); Sub; MulBPS(bps BPS, mode RoundingMode) (USD, error); MulRatio(num, den int64, mode) (USD, error); Cmp(USD) int; Abs
const MaxUSD / MinUSD bounds (int64 range) with tests
type BPS int64                            // basis points; 10_000 == 100%
type Quantity struct{ /* big.Int */ }     // exact base units; immutable value semantics
func QuantityFromInt64(int64) Quantity; ParseQuantity(s string) (Quantity, error) // decimal digits only, optional leading '-'
func (Quantity) Add/Sub/Mul(Quantity) Quantity; Div(Quantity, mode) (Quantity, error); MulBPS(bps, mode) Quantity; Cmp; Sign; IsZero; String; Int64() (int64, error); BigInt() *big.Int (copy)
func (Quantity) ToDecimalString(decimals uint8) string  // human rendering; never used for arithmetic
func QuantityFromDecimalString(s string, decimals uint8, mode RoundingMode) (Quantity, error)
type Price struct{ Mantissa Quantity; Scale int32; QuoteAsset string; Source string; At time.Time }  // value = Mantissa × 10^-Scale quote per 1 base unit
func (Price) Validate() error
func Notional(qty Quantity, baseDecimals uint8, p Price, quoteDecimals uint8, mode RoundingMode) (Quantity, error)   // exact quote-asset base units
func QuoteQuantityToUSD(q Quantity, quoteDecimals uint8, mode RoundingMode) (USD, error)  // for USD-pegged quote assets; caller decides eligibility
// JSON: USD as string "1234.56"; Quantity as decimal-string; never numbers. sql: USD -> BIGINT, Quantity -> NUMERIC(38,0) text form.
```

### `internal/db`

```go
type Config struct { URL string; MaxConns, MinConns int32; AppName string; RequireTLS bool; StatementTimeout, LockTimeout time.Duration }
type DB struct{ ... }
func Open(ctx context.Context, cfg Config) (*DB, error)   // pgxpool + otelpgx tracer; fails if RequireTLS and sslmode not verify-*
func (db *DB) Pool() *pgxpool.Pool; Close(); Ping(ctx) error
type Querier interface { Exec(ctx, sql string, args ...any) (pgconn.CommandTag, error); Query(ctx, sql string, args ...any) (pgx.Rows, error); QueryRow(ctx, sql string, args ...any) pgx.Row }
type TxOptions struct { Isolation pgx.TxIsoLevel; ReadOnly bool; MaxRetries int }
func (db *DB) InTx(ctx, opts TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error   // retries on SQLSTATE 40001/40P01 up to MaxRetries with jitter; never retries fn side effects outside tx
func (db *DB) Serializable(ctx, fn func(ctx, tx pgx.Tx) error) error   // InTx with Serializable + MaxRetries=5
func IsUniqueViolation(err error) bool; IsSerializationFailure(err error) bool
// package internal/db/migrate: embedded goose migrations from /migrations (go:embed via a root-level embed.go: package migrations)
func Up(ctx, migrateURL string) error; Status(ctx, migrateURL string) ([]MigrationStatus, error); UpTo / DownTo (down refuses ledger-protected versions)
```

### `internal/event`

```go
type Envelope struct {
  ID string /* id.EventID string form */; Type string; SchemaVersion int; Source string
  AggregateType, AggregateID string; CorrelationID, CausationID string
  OccurredAt, RecordedAt time.Time; DedupKey string; Headers map[string]string; Payload json.RawMessage
}
type Outbox struct{...}; func NewOutbox(clk clock.Clock) *Outbox
func (o *Outbox) Enqueue(ctx, tx pgx.Tx, topic string, events ...Envelope) error   // INSERT INTO outbox in caller's tx
type Bus interface { Publish(ctx, topic string, key string, value []byte, headers map[string]string) error; Subscribe(ctx, topic, group string, h Handler) error; Close(ctx) error }
type Relay struct{...}; func NewRelay(db *db.DB, bus Bus, log *slog.Logger, opts RelayOptions) *Relay; func (r *Relay) Run(ctx) error; RunOnce(ctx) (int, error)  // SELECT ... FOR UPDATE SKIP LOCKED, publish, mark published_at; at-least-once
type Inbox struct{...}; func NewInbox(clk) *Inbox
func (i *Inbox) Process(ctx, tx pgx.Tx, source, messageID string, schemaVersion int, fn func(ctx, tx pgx.Tx) error) (Outcome, error) // Outcome: Processed | Duplicate; duplicate → fn not called
// In-memory Bus lives in internal/event/eventtest and is rejected in PROD by config validation.
```

### `internal/config`

```go
type Environment string // LOCAL, TEST, DEV, STAGING, PROD; ParseEnvironment fails closed on unknown
type ProviderMode string // "fake", "sandbox", "live"
type Config struct { Env Environment; ServiceName string; PublicProductName string; BuildVersion string; HTTP HTTPConfig; Database DatabaseConfig; Redis RedisConfig; Redpanda RedpandaConfig; ClickHouse ClickHouseConfig; Temporal TemporalConfig; Archive ArchiveConfig; KMS KMSConfig; Auth AuthConfig; Providers ProvidersConfig; Telemetry TelemetryConfig; Seed SeedConfig }
func Load(ctx, lookup func(string) (string, bool)) (*Config, error)  // env vars prefixed CP_; Validate() applied
func (c *Config) Validate() error      // PROD rules: no fake providers, no debug auth, no seed, TLS required for DB/Redis/Redpanda, no CORS "*", archive+KMS configured, capability DB configured
func (c *Config) Hash() string          // sha256 over non-secret canonical JSON
type SecretRef string                   // "env://NAME" | "aws-sm://arn-or-name" ; Resolver interface { Resolve(ctx, SecretRef) (string, error) }
var BuildVersion = "dev"                // set via -ldflags
```

### `internal/observability`

```go
func Setup(ctx, cfg config.TelemetryConfig, service, version string, env config.Environment) (shutdown func(context.Context) error, err error)
func NewLogger(env config.Environment, w io.Writer) *slog.Logger   // JSON handler + redaction (keys: private_key, seed, mnemonic, token, secret, authorization, cookie, password, card, ssn, api_key…) + context attrs
func WithRequestID(ctx, string) context.Context; RequestID(ctx) string  // also CorrelationID, IntentID, OrderID, WorkflowID
func LoggerFrom(ctx) *slog.Logger / WithLogger(ctx, l)
type FinancialMetrics struct{ ... } // instruments named exactly per goal PART 132–134
func NewFinancialMetrics(meter metric.Meter) (*FinancialMetrics, error)
```

### `internal/security` and `internal/auth`

```go
// security
type Role string // CUSTOMER, SUPPORT_READ_ONLY, OPERATIONS, RISK, COMPLIANCE, FINANCE, SECURITY, ADMIN, BREAK_GLASS (time-boxed)
type ActorType string // USER, OPERATOR, SERVICE, AGENT, SYSTEM
type Principal struct { SubjectID string; ActorType ActorType; Roles []Role; AccountIDs []string; SessionID string; AuthTime time.Time; AMR []string; BreakGlassUntil *time.Time }
func WithPrincipal(ctx, Principal) context.Context; PrincipalFrom(ctx) (Principal, bool)
type Permission string // e.g. "account:read", "trade:create", "admin:kill", "gate:approve", "ledger:read"...
func Require(ctx, Permission) error                 // FORBIDDEN / UNAUTHENTICATED
func RequireAccount(ctx, accountID string) error    // tenant scoping: principal owns account OR operator role with account:read_any
func RequireStepUp(ctx, maxAge time.Duration, clk clock.Clock) error  // STEP_UP_REQUIRED unless AuthTime recent and AMR includes mfa/passkey
// Agents never receive a Principal with roles; ActorType AGENT gets only agent:* permissions.
// auth
type IdentityProvider interface { AuthCodeURL(state, nonce, codeChallenge string, stepUp bool) string; Exchange(ctx, code, codeVerifier, nonce string) (Identity, error); Name() string }
type Identity struct { Subject string; Email string; EmailVerified bool; AMR []string; AuthTime time.Time; ACR string; Claims map[string]any }
type SessionStore interface { Create(ctx, tx, Session) error; Get(ctx, q Querier, token string) (Session, error); Touch; Revoke(ctx, q, id) error; RevokeAllForSubject; ListForSubject }
// Middleware for chi: Session (loads Principal), RequireAuth, CSRF (Origin/Sec-Fetch-Site check for unsafe methods), SecureHeaders.
// Dev IdP: internal/auth/devidp — constructor returns error unless env ∈ {LOCAL, TEST, DEV}.
```

## Identity of principals

- `security.Principal.SubjectID` for USER/OPERATOR actors is the `users.id` UUID (string form), never the identity-provider subject: the login handler resolves `(issuer, subject)` to a user via `accounts.Repository.GetUserBySubject` before issuing a session (`auth/pgstore` enforces it). Tables that record actors store `actor_id text` = this UUID string; agents use their `agents.id`.

## State changes must carry a transition row (migration 00603)

- For gates, kill switches, accounts, assets, instruments, deposits, withdrawals, trade intents, orders, reconciliation records, and admin actions, a change to the state column is refused at COMMIT (SQLSTATE `AU001`) unless a row was inserted into the entity's `*_transitions` table in the same transaction with the same target state. Statement order inside the transaction does not matter. Repositories therefore expose a single `Transition(...)` that writes both, plus the outbox event and the audit event.

## Migrations

- Files: `migrations/NNNNN_name.sql` using goose annotations (`-- +goose Up` / `-- +goose Down`). **New migrations always take the next number after the highest existing one** (goose applies in version order and refuses out-of-order versions below the current database version). The historical ranges below describe how the initial schema was drafted; they are not a rule for new files: 00001–00099 foundation (extensions, outbox/inbox, idempotency, identity/accounts/sessions — everything here has a real, reversible Down), 00100–00149 financial core (assets, ledger, capital, funding, positions, valuation, audit events, provider events — every history table lives at or above `ProtectedVersion` = 100 so `DownTo` can never drop it), 00150–00199 policy/authority (capability gates, kill switches, eligibility/risk policies and decisions, admin actions), 00200–00299 instruments/intents/quotes/plans/orders, 00300–00399 wallets/execution/reconciliation, 00400–00499 funding-provider specifics, 00500–00599 strategy/agent/prediction, 00600–00699 reality/backtest/performance, 00700–00799 audit proof (Merkle checkpoints, signatures, archive manifests).
- Every table: `id uuid primary key`, `created_at timestamptz not null default now()`, and `updated_at` where mutable.
- Posted ledger tables: no UPDATE/DELETE grants to `cp_app`; triggers raise on update/delete of posted rows.
- Down migrations for ledger-protected tables are `-- +goose Down` with `SELECT 1; -- ledger history is never dropped` and the runner refuses `DownTo` below the protected version.

## Testing

- `testing` + `github.com/stretchr/testify` (require/assert) + `pgregory.net/rapid` for properties + native fuzzing (`FuzzXxx`).
- Table tests. Each package ≥ the tests named in `docs/build/REQUIREMENTS_TRACEABILITY.md` for its requirements.
- Property tests named `TestProp_*`; fuzz targets `Fuzz*`; integration tests under build tag `integration`.

## HTTP API

- chi router; `/v1/...`; commands POST with `Idempotency-Key`; queries GET with cursor pagination; errors `application/problem+json` via `errs.ToProblem`.
- OpenAPI in `openapi/openapi.yaml`; server interfaces generated with oapi-codegen (strict server); client generated for the web app.
