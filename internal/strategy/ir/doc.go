// Package ir is the typed strategy intermediate representation (STRATEGY_IR.md
// §2, goal PART 62): pure data that both authoring paths (natural language
// and the TypeScript SDK) converge on, and the only thing the agent runtime
// ever evaluates.
//
// The package holds the IR types, exact fixed-point Decimal arithmetic, the
// effect system (allowed and forbidden effect constants, DeriveEffects), the
// strict parser (Decode, ParseIR), the structural validator (Structural), the
// semantic hash (SemanticHash) and the JSON schema handed to the model.
//
// It must never:
//   - contain floating-point values or arithmetic (money is money.USD, rates
//     are money.BPS, everything else is a scaled Decimal with an explicit
//     rounding mode);
//   - express anything beyond bounded reads, schema-constrained model calls,
//     prediction commits and typed trade intents (no code, no eval, no shell,
//     no file access, no network destinations);
//   - accept a document with unknown fields, unbounded sizes, a forward or
//     self signal reference, or an effect outside the allowed table;
//   - read a clock, iterate a map without sorting, or perform I/O; every
//     function here is deterministic given its arguments;
//   - import execution, settlement, signing, wallet, admin, gates, kill
//     switch or capital packages (agent-side code is structurally unable to
//     reach authority).
package ir
