// Package model is the boundary between this platform and a language model
// provider (goal PARTS 65, 67, 177).
//
// Everything here treats model output as untrusted input. A Request is
// assembled from three structurally separate segments — SYSTEM POLICY (the
// only place instructions may come from, versioned and checked into the
// repository), TOOL RESULTS (typed data with provenance ids) and UNTRUSTED
// CONTENT (user text, posts, token metadata) — and the response is
// constrained by a JSON schema and validated by the caller before anything
// acts on it.
//
// It must never:
//   - let content from the TOOL RESULTS or UNTRUSTED CONTENT segments reach
//     the system section, add a tool, widen an effect set or name a
//     destination; the segments are concatenated in a fixed order with
//     fixed labels and nothing else;
//   - place secret material in a prompt: Guard refuses a request whose
//     content matches the credential denylist rather than redacting it
//     silently;
//   - request, store or expose hidden chain-of-thought (PART 178); only the
//     structured rationale and evidence references the output schema
//     declares are persisted;
//   - invent output when a provider fails (PART 177): an error is an error,
//     and there is no heuristic fallback;
//   - dial a provider without first checking the Budget, which is enforced
//     from persisted counters rather than in-memory state;
//   - use floating point for cost: usage is priced from an integer cents
//     table into money.USD minor units;
//   - be constructed with the modeltest fake outside LOCAL, TEST or DEV.
package model
