// Package inspect is the pure transaction inspector of the bounded signing
// boundary (goal PARTS 34, 152; EXECUTION.md §2): NO BLIND SIGNING.
//
// # Responsibility
//
//   - Decode decodes a legacy or v0 Solana transaction from raw bytes with
//     github.com/gagliardetto/solana-go and never panics on malformed input
//     (FuzzDecode). v1 transactions and trailing bytes are rejected.
//   - Expectations is everything the inspector is allowed to know about the
//     approved plan, quote, reservation, wallet and chain state. The signing
//     service rebuilds it from persisted rows; the inspector never trusts a
//     caller-supplied copy and never does I/O (address-lookup-table contents
//     are passed in by the caller and resolved here).
//   - Inspect evaluates every check of EXECUTION.md §2 by name (FEE_PAYER,
//     SIGNERS, PROGRAM_ALLOWLIST, TOKEN_PROGRAM, INPUT_DEBIT_BOUND,
//     OUTPUT_TOKEN, MIN_OUTPUT, NO_SYSTEM_TRANSFER, NO_UNEXPECTED_DESTINATION,
//     NO_AUTHORITY_CHANGE, NO_ARBITRARY_CPI, COMPUTE_BUDGET, BLOCKHASH,
//     PLAN_IDENTITY, SLIPPAGE, SIMULATION) and returns a deterministic Result:
//     checks in canonical order, reason codes sorted and de-duplicated.
//   - Instruction layouts for the System, SPL Token / Token-2022, Compute
//     Budget, Associated Token Account and Jupiter v6 programs are decoded by
//     minimal hand-written decoders documented against the SPL sources; the
//     solana-go program packages are used only in tests to cross-check them.
//
// # Fail-closed rules
//
//   - Any decode error, any unknown instruction of an allowed program, any
//     instruction of a program outside the allowlist (including inner
//     instructions revealed by simulation), any unresolvable lookup table,
//     any malformed expectation, and any panic inside the inspector rejects.
//   - Token-2022 instructions are rejected unless Expectations.Token2022Allowed
//     is set by the signing service (asset explicitly enabled, no extensions).
//   - Exactly one venue route instruction (Jupiter v6 route or
//     shared_accounts_route, ExactIn) is required; exact-out and token-ledger
//     variants are recognized and rejected.
//
// # What this package must never do
//
//   - Perform I/O of any kind (network, database, clock, environment).
//   - Approve a transaction when any check has not been evaluated.
//   - Use floating point for amounts, fees or slippage (money.Quantity and
//     integer lamports only).
//   - Trust a caller-supplied expectation over the persisted plan; that
//     responsibility belongs to internal/signing, which owns the rebuild.
//   - Depend on internal/execution or internal/settlement.
package inspect
