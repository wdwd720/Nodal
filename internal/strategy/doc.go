// Package strategy compiles and validates strategies (goal PARTS 61-65,
// 170). It owns the two authoring paths — natural language through a model,
// and a document produced by the TypeScript SDK — which converge on one
// validated ir.IR before anything is persisted.
//
// The compiler treats model output exactly as it treats any other untrusted
// input: it goes through the same parser, the same structural checks and the
// same effect closure as an SDK document, and it becomes a version only
// after every stage passes.
//
// It must never:
//   - persist a version from a document that failed any stage; an invalid
//     candidate is recorded as a compile attempt and discarded;
//   - retry a rejection that is terminal — a forbidden effect or a risk
//     incompatibility is a decision, not a transient failure, and retrying
//     it would let a caller grind against the policy;
//   - accept a value the model supplied for Hash, Version, BuiltAt or
//     Lineage; those are set by this package from its own state;
//   - let the model, or anything derived from it, widen an effect set: the
//     declared set must equal the set derived from the document's own
//     dependencies and actions;
//   - depend on a live model to be testable: every stage after the provider
//     call is a pure function of the candidate document;
//   - import internal/execution, settlement, signing, wallet, admin, gates,
//     killswitch or capital — agent-side code is structurally unable to
//     reach authority, and test/security enforces it.
package strategy
