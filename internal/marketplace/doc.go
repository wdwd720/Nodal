// Package marketplace is the strategy marketplace (advanced goal §14;
// ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md package map:
// MARKETPLACE, migration 00750, capability STRATEGY_MARKETPLACE, modes PAPER
// and SANDBOX).
//
// A listing is a container owned by its creator; a listing VERSION is the
// unit that is built, reviewed, approved, installed and followed. A version
// is immutable apart from its lifecycle: an artifact digest (from
// internal/sandboxrt), a source tree and its digest, a typed MANIFEST of
// permissions (allowed intent kinds, allowed instruments, a notional cap per
// intent, a cap on intents per run, an execution frequency), a risk class,
// supported instruments, data dependencies, backtest evidence, fee metadata
// and provenance.
//
// # Responsibilities
//
//   - The lifecycle, bound in Go (CanTransition) and in SQL (migration 00750,
//     MK001/AU001): DRAFT -> SUBMITTED (publish) -> VALIDATED (scan and build
//     passed) -> IN_REVIEW -> APPROVED | REJECTED; APPROVED -> DEPRECATED. A
//     new VERSION of a listing starts again at DRAFT. A FORK is a new listing
//     whose provenance names its parent version.
//   - Dual control: reviewers are OPERATOR principals and never the creator;
//     the database refuses an approval by the creator (MK002).
//   - INSTALL is per account and PAPER only; the database refuses any other
//     mode (CHECK marketplace_installs_mode_check). The marketplace never
//     allocates real customer money.
//   - SIMULATE runs a version against a fixed historical snapshot set and
//     records an evaluation (dataset version, snapshot and result digests).
//   - PAPER_FOLLOW runs an installed version in the sandbox, turns the
//     proposals its manifest permits into econ intents (ActorType SERVICE,
//     StrategyRef "<listing>@<version>", sandbox origin in the terms), routes
//     each through the settlement compiler, evaluates each with Risk V2 (the
//     strategy and sandbox sections included) and hands only ALLOWED intents
//     to an injected IntentExecutor, exactly once (econ.EffectStore). Paper
//     performance, drawdown, volatility and turnover are computed exactly
//     from the follow history this package records.
//
// # What this package must never do
//
//   - Let a strategy act: every effect of a strategy's output is an econ
//     intent that faces routing and Risk V2, and only an ALLOW decision
//     reaches the executor (the database refuses a dispatched follow intent
//     without one, MK009).
//   - Grant a strategy more than its manifest: proposals beyond it are
//     dropped and counted, and the manifest of a version can never change.
//   - Install, follow or allocate in any mode but PAPER, or charge a fee: fee
//     fields are metadata.
//   - Let an agent create, review, approve, install or follow a listing.
//   - Import internal/signing, internal/wallet, internal/withdrawal or
//     internal/delegation.
//   - Hold mutable package-level state, read the wall clock implicitly, or
//     use floating point.
package marketplace
