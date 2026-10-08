// Package marketplace is the strategy marketplace (advanced goal §14;
// ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md package map:
// MARKETPLACE, migrations 00750 and 00757, capability STRATEGY_MARKETPLACE,
// modes PAPER and SANDBOX).
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
//     the database refuses an approval by the creator (MK002), and the
//     creator's own transitions into review or approval and any moderation of
//     their own version -- re-clearing it after a flag included -- (MK011).
//   - Actors act for accounts they own: a listing's creator, an install's
//     installer, an explicit follow's actor and a simulation's actor are the
//     owner of the account they name; a scheduled follow is the SYSTEM
//     scheduler's (Go and MK012).
//   - VALIDATION builds between transactions (RunValidation): the version is
//     claimed in one transaction under a lease, built in none -- under the
//     builder's bounds on time, module size, disk and memory, in a directory
//     of its own that the build does not outlive -- and the outcome is
//     recorded in another, with the bounded compiler output kept for the
//     author. A build past a bound is a recorded rejection; a build that could
//     not run (no toolchain, the caller went away, every build slot stayed
//     busy) releases its claim (MK015). The transactional Validate refuses.
//   - INSTALL is per account and PAPER only, with a PAPER BOOK that starts at
//     the configured paper capital; the database refuses any other mode
//     (CHECK marketplace_installs_mode_check). The marketplace never allocates
//     real customer money.
//   - SIMULATE runs a version against a fixed historical snapshot set and
//     records an evaluation (dataset version, snapshot and result digests),
//     charged to the creator's account under the sandbox budget and admitted
//     whole or not at all.
//   - PAPER_FOLLOW runs an installed version in the sandbox on an input the
//     service builds itself: the install's paper book and, for every
//     instrument the version names, the freshest reference price in the price
//     store (asset_prices) no older than MaxPriceAge, with its history. It
//     turns the proposals its manifest permits -- valued at those reference
//     prices, never at a strategy's limit price -- into econ intents
//     (ActorType SERVICE, StrategyRef "<listing>@<version>", sandbox origin in
//     the terms), routes each through the settlement compiler, evaluates each
//     with Risk V2 (the strategy, sandbox and paper-book balance sections
//     included) and hands only ALLOWED intents to an injected IntentExecutor,
//     exactly once (econ.EffectStore). An applied dispatch is a fill of the
//     paper book at its reference price. Paper performance, drawdown,
//     volatility and turnover are computed exactly from the equities of the
//     paper book, which the database recomputes from the recorded fills and
//     marks (MK013, MK014).
//
// # What this package must never do
//
//   - Let a strategy act: every effect of a strategy's output is an econ
//     intent that faces routing and Risk V2, and only an ALLOW decision
//     reaches the executor (the database refuses a dispatched follow intent
//     without one, MK009).
//   - Dispatch an intent whose route may not execute now: a route the legal
//     router permits only once a person confirms it or an operator reviews
//     it is refused (ROUTE_REFUSED, REQUIRES_USER_CONFIRMATION or
//     REQUIRES_MANUAL_REVIEW), because a follow has nobody to ask.
//   - Grant a strategy more than its manifest: proposals beyond it are
//     dropped and counted, and the manifest of a version can never change.
//   - Take a follow's input, a mark, an equity or a performance figure from a
//     caller: they come from the paper book and the price store only.
//   - Build inside a database transaction, or let a build past its bounds
//     leave a version SUBMITTED.
//   - Install, follow or allocate in any mode but PAPER, or charge a fee: fee
//     fields are metadata.
//   - Let an agent create, review, approve, install or follow a listing, or a
//     person act for an account they do not own.
//   - Import internal/signing, internal/wallet, internal/withdrawal or
//     internal/delegation.
//   - Hold mutable package-level state, read the wall clock implicitly, or
//     use floating point.
package marketplace
