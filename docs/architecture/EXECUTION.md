# EXECUTION (SOLANA)

Status: design fixed 2026-09-05; implementation tracked in `docs/build/REQUIREMENTS_TRACEABILITY.md`. Contract for Stage 6. Covers PARTS 34, 43–49, 78, 79, 95, 96, 107, 152, 196, 227, 228.

## 1. Flow (PART 44)

```
quote → freshness check → build candidate tx (adapter) → deserialize → inspect instructions → simulate (RPC) →
compare predicted state changes to approved plan → enforce fee/slippage/program limits → bounded signing → submit →
persist submission evidence → observe status (Helius + fallback RPC) → reconcile independently
```

Every arrow persists state in Postgres before the next side effect. No step trusts the previous provider response without recording it.

## 2. Transaction inspection (PART 34) — NO BLIND SIGNING

`signing/inspect` is a pure package: `Inspect(tx DecodedTransaction, exp Expectations) Result`. It decodes legacy and v0 (versioned) Solana transactions, resolves address-lookup-table accounts from expectations (the executor fetches ALT contents and passes them in; the inspector never does I/O), and evaluates every check below. Any failed check, any unknown instruction, and any decode error → `REJECTED`. Reason codes are sorted and stable.

| Check | Rule |
|---|---|
| `FEE_PAYER` | fee payer == expected wallet |
| `SIGNERS` | the only required signer is the expected wallet (no extra signers) |
| `PROGRAM_ALLOWLIST` | every top-level and inner-instruction program id ∈ allowlist (System, ComputeBudget, Token, Associated Token, Jupiter v6 program ids from configuration, plus per-plan additions) |
| `TOKEN_PROGRAM` | token instructions target the expected mints only; Token-2022 mints rejected unless the asset's `token_extensions` is empty and Token-2022 is explicitly enabled for that asset |
| `INPUT_DEBIT_BOUND` | total possible debit of the input token from the wallet's token accounts ≤ `MaxInputDebit` (from reservation) |
| `OUTPUT_TOKEN` | output token account belongs to the wallet and mint == expected output mint |
| `MIN_OUTPUT` | swap instruction's encoded minimum out ≥ plan `MinOutputQuantity` |
| `NO_SYSTEM_TRANSFER` | no `SystemProgram.Transfer` from the wallet except: rent for creating the wallet's own ATA (bounded), wrapped-SOL handling within the plan's allowance |
| `NO_UNEXPECTED_DESTINATION` | every account that can receive value is the wallet, its ATAs, or a venue program-owned account referenced by the route |
| `NO_AUTHORITY_CHANGE` | no `SetAuthority`, `CloseAccount` to a foreign destination, `Approve`/`ApproveChecked` (delegation), `FreezeAccount`, `Assign`, `Allocate`, nonce authority changes, or account ownership changes |
| `NO_ARBITRARY_CPI` | no instruction whose program id is outside the allowlist, including inner instructions revealed by simulation |
| `COMPUTE_BUDGET` | priority fee ≤ `MaxPriorityFeeLamports`; compute-unit limit ≤ `MaxComputeUnits` |
| `BLOCKHASH` | recent blockhash matches the adapter's reported blockhash and `LastValidBlockHeight` is in the future by ≥ policy margin |
| `PLAN_IDENTITY` | `Expectations.PlanHash` and `QuoteID` match the persisted approved plan and its quote |
| `SLIPPAGE` | encoded slippage bps ≤ plan `MaxSlippageBPS` |
| `SIMULATION` | simulation succeeded and predicted token balance deltas satisfy `INPUT_DEBIT_BOUND` and `MIN_OUTPUT` |

Fuzzing (PART 152): `FuzzInspect` feeds random bytes and mutated real transactions (unknown programs, extra transfers, malicious delegate, authority updates, unexpected token accounts, malformed instructions, Token-2022 extensions); expected: never panic, reject.

## 3. Signing boundary (PART 95, 96)

```go
// internal/signing
type Request struct {
  AttemptID, PlanID, IntentID, RiskDecisionID, WalletID string
  UnsignedTx []byte; ExpectedTxHash []byte
  Expectations inspect.Expectations   // derived by the executor from the approved plan + quote + reservation
}
type Decision struct { ID string; Approved bool; ReasonCodes []string; Checks []inspect.Check; InspectorVersion string; DecidedAt time.Time }
type Service interface { Sign(ctx context.Context, req Request) (Decision, SignedTx []byte, err error) }
```

`Service.Sign` independently re-loads the approved plan, quote, risk decision, and reservation from Postgres, rebuilds `Expectations` from persisted data (never trusting the caller's copy beyond identifiers), runs the inspector, inserts a `signing_decisions` row and an audit event in one transaction, and only if approved calls `wallet.SigningProvider.SignTransaction`. Rejections emit `signing_rejection` security events. **CORRECTION (F-51).** This sentence used to read "the signing package is importable only by `cmd/execution-worker` (depguard rule)", and both halves were false. The only depguard rule naming `internal/signing` is `agent-authority`, which denies it to `internal/agent`, `internal/strategy`, `internal/prediction`, `internal/model` and `cmd/agent-worker` — a different restriction, in the opposite direction. And `cmd/execution-worker` does not import the package: `internal/signing` has no non-test importer at all, and the only thing importing any part of it is `internal/provider/privy/config.go`, which uses `internal/signing/inspect`. The boundary the sentence described is one nobody has drawn; what exists is the agent prohibition. `proto/signing/v1` defines the gRPC contract for the future out-of-process deployment and the in-process implementation satisfies the same interface.

```go
// internal/wallet
type WalletProvider interface { CreateWallet(ctx, accountID string, chain string) (Wallet, error); GetWallet(ctx, providerWalletID string) (Wallet, error); VerifyDelegation(ctx, walletID string) (DelegationStatus, error) }
type SigningProvider interface { SignTransaction(ctx, req SignRequest) (SignResult, error) }  // SignRequest{ProviderWalletID, Chain, UnsignedTx []byte, IdempotencyKey, Purpose}
```

Production startup fails closed if the configured signing provider's capability for delegated signing is not `VERIFIED` in `wallets.delegation_verified_at`/provider capability probe (PART 96).

## 4. Submission and the timeout rule (PART 48)

`Submit` errors are classified: definitive rejection (provider says invalid/expired **and** the signature is not on chain) → `FAILED` and reservation released; transport timeout / ambiguous → `SUBMISSION_UNKNOWN`:

1. keep the reservation and lock it to the order (`locked_by_order_id`);
2. never submit a duplicate for the same plan;
3. query `Status(signature)` on the execution provider;
4. query the chain observer (Helius) and the fallback RPC for the signature;
5. scan wallet activity since the attempt's `created_at` for a transaction matching the plan's expected instructions (the signed transaction bytes are persisted, so the signature is known);
6. if found: adopt it (`ADOPTED`), observe finality, continue the plan;
7. if proven absent — signature not found by both observers **and** the chain height > `LastValidBlockHeight` + margin — the transaction can never land; a new attempt with a fresh blockhash may be built under the same plan (attempt_no + 1) after a fresh risk `FINAL` check;
8. if observers disagree or evidence is incomplete: `RECONCILIATION_REQUIRED`, operator path, no new attempt.

The crash test (PART 49) exercises steps 1–8 with a fake chain where the transaction landed but the process died before persisting success.

## 5. Finality (PART 45)

`FinalityPolicy{UIProvisional: OBSERVED, PositionProvisional: CONFIRMED, LedgerPosting: CONFIRMED, FundingAvailability: FINALIZED, Withdrawal: FINALIZED}` per action class, configurable. Fills carry `finality`; ledger posting uses fills at `CONFIRMED` or better and records the level; a `FINALIZED` observation later upgrades the fill's finality without changing economics. If a `CONFIRMED` transaction is later absent from a finalized block (reorg), reconciliation opens a `MISMATCH` and compensating postings are made under the standard resolution flow.

## 6. Observation providers (PART 78, 79, 196)

```go
// internal/execution/observe
type ChainObserver interface { GetTransaction(ctx, sig string) (TxObservation, error); GetBalances(ctx, wallet string, mints []string) ([]BalanceObservation, error); GetBlockHeight(ctx) (uint64, error); SearchWalletActivity(ctx, wallet string, since time.Time, limit int) ([]TxObservation, error); Name() string }
type SolanaDataProvider interface { ChainObserver; StreamWalletEvents(ctx, wallets []string, sink func(Event) error) error }   // Helius
```

Confidence policy when Helius and the fallback RPC disagree: `AgreementPolicy{RequireBothFor: FINALIZED; PreferChainRPCFor: CONFIRMED_CONFLICT; OnDisagreement: BLOCK_DEPENDENT_ACTIVITY}`. Never pick the optimistic answer.

Provider health (`HEALTHY/DEGRADED/UNHEALTHY/DISABLED`) is computed from error rate, latency, and staleness windows; `Risk`/`Settlement` consume it. A `PROVIDER_DISABLE_NEW_ACTIONS` kill switch or an `UNHEALTHY` execution provider stops new submissions but never stops `Status`, `Reconcile`, or observation (PART 107).

## 7. Order state machine (PART 47, 227, 228)

Transition table in `execution.OrderTransitions`. `CANCEL_REQUESTED` only moves to `CANCELLED` on external confirmation; a fill arriving while cancel is requested wins and the order proceeds to `FILLED`. Partial fills are modelled generically (`PARTIALLY_FILLED` with cumulative filled quantities) even though Jupiter swaps are atomic.

## 8. Jupiter adapter (PART 43)

Implemented against the official API documentation fetched at implementation time (`docs/api/providers/jupiter.md` records the endpoints, versions, and the date verified). Request and response bodies are archived (`raw_response_ref`) with hashes. Jupiter types never leave `internal/provider/jupiter`; the adapter maps them to `execution` types.

## 9. Tests required before Stage 6 exit

- Inspector: table tests per check; fuzz corpus (PART 152); golden real-transaction fixtures (recorded devnet/mainnet Jupiter swap transactions) approved; mutated variants rejected.
- Adapter contract tests (PART 148): valid, invalid, timeout, rate limit, 5xx, stale quote, unexpected state, missing required field.
- Submission unknown-state recovery tests with fake observers: found/adopted; proven absent → new attempt; disagreement → RECONCILIATION_REQUIRED.
- Crash test (PART 49).
- Provider health state machine tests; kill switch never blocks observation/reconciliation.
