// Package sandboxrt is the sandboxed strategy runtime (advanced goal §15;
// ADR-0020 decision 4; docs/architecture/ADVANCED_FINANCIAL_MODEL.md, package
// map: SANDBOX_RUNTIME, migrations 00751 and 00757, capability
// USER_CODE_SANDBOX, mode SANDBOX).
//
// It is a separate trust boundary. User strategy code is untrusted, and the
// only thing that ever leaves it is a typed intent PROPOSAL:
//
//	immutable source -> Scan -> Builder (go, wasip1) -> artifact digest
//	  -> Runner (wazero, WASI preview1 only) -> stdout records
//	  -> ParseOutput -> proposal validation -> manifest Filter
//	  -> econ.Intent (ActorType SERVICE, SandboxOriginated) -> Risk V2
//
// # Responsibilities
//
//   - Scan parses every source file and refuses, with stable violation codes,
//     any import outside a small allowlist (the SDK, errors, sort, strconv,
//     strings, unicode/utf8, math/big, encoding/json), every escape hatch
//     (unsafe, reflect, runtime, os, os/exec, net, syscall, plugin, io/fs,
//     embed, cgo), every compiler directive (//go:linkname, //go:wasmimport,
//     //go:wasmexport, //go:embed, build constraints), non-Go files and
//     oversized trees.
//   - Builder compiles scanned source in a fresh temporary directory with a
//     generated go.mod whose only dependency is the embedded SDK, under an
//     environment that contains nothing from the parent process: a secret in
//     the parent's environment is absent from the build. Builds are
//     reproducible; the artifact digest is the sha256 of the wasm bytes. A
//     build is bounded in wall time, in the size of the module it writes, in
//     the disk its directory takes and in the resident memory of its process
//     group (BuilderConfig, with safe defaults and hard ceilings); a build
//     past a bound is stopped and is a typed refusal (BUILD_TIMEOUT,
//     BUILD_OUTPUT_LIMIT, BUILD_MEMORY_LIMIT), never an artifact. The
//     compiler's output is kept bounded, with the build directory replaced,
//     so an author can read why a build failed.
//   - Every build lives in one directory of the builder's work area -- its
//     sources, its own build cache, the toolchain's temporary files, its
//     module -- removed when the build ends, whatever the outcome; at most
//     MaxConcurrentBuilds such directories exist at once (a build waits for
//     a slot, and a directory that cannot be removed keeps its slot), and
//     one a dead process left is removed by the next builder. The shared
//     cache directory holds only the builder's seed -- the standard library
//     and the SDK, compiled once from the package's own warm-up program and
//     published whole -- which a build copies and never writes (review 3,
//     F4).
//   - Runner instantiates an artifact with wazero and WASI preview1 and
//     nothing else: no filesystem, no environment, no arguments beyond the
//     program name, no sockets, stdin = the input document, bounded stdout
//     and stderr, a per-run memory limit enforced by a metered allocator, a
//     wall-time limit that closes the module (infinite loops die), a virtual
//     clock derived from the input's as-of time and a seeded deterministic
//     random source. Same artifact plus same input gives the same output
//     bytes and the same run digest.
//   - ParseOutput accepts exactly two record types, intent and log, and
//     nothing else; an unknown record makes the whole run INVALID_OUTPUT.
//   - Filter drops every proposal beyond the strategy's manifest and records a
//     violation for it. A proposal's notional is computed at a REFERENCE PRICE
//     the caller read from its own records (FilterAt), never at the limit
//     price the strategy chose; a proposal for an instrument without one is
//     refused and recorded (REFERENCE_PRICE_UNAVAILABLE). Intent carries that
//     reference price in the terms, and refuses without one.
//   - Budget bounds the sandbox per call and per account per window: runs,
//     CPU and wall time. A call is admitted whole or refused before anything
//     runs, and every run is charged to its account under an advisory lock, so
//     concurrent callers cannot overspend; a refusal is RATE_LIMITED with a
//     stable SANDBOX_BUDGET_* reason. The database refuses a run nobody was
//     charged for (SX002) and a charge beyond the budget it carries (SX003).
//   - Service persists artifacts (immutable), runs, violations and usage
//     (append-only), and refuses to run at all while CAPABILITY_DISABLE for
//     USER_CODE_SANDBOX (or the global kill) is active.
//
// # What this package must never do
//
//   - Give strategy code a filesystem, an environment variable, a socket, a
//     host function outside WASI preview1, a real clock or real randomness.
//   - Turn strategy output into anything but an econ.Intent proposal: there
//     is no record type and no code path that signs, transfers, withdraws,
//     bridges or names a destination, and TRANSFER_VALUE, WITHDRAW,
//     BRIDGE_VALUE, BORROW, MINT and BURN are not proposable kinds.
//   - Apply an economic effect or post to the ledger. A proposal becomes an
//     intent, and the intent faces the risk kernel like every other.
//   - Import internal/signing, internal/wallet, internal/withdrawal or
//     internal/delegation (TestImportBoundary_UntrustedOutputCannotReachAuthority).
//   - Build source that has not passed Scan. The only unscanned build path is
//     in a _test.go file and exists so the runtime's isolation can be proved
//     independently of the scanner.
//   - Let a build write outside its own directory, or leave anything behind
//     it: not in the shared cache (which a build only copies), not in the
//     system's temporary directory, not through a process that outlives the
//     build (the toolchain's telemetry is off in a build's HOME).
//   - Value a proposal at a price the strategy chose, or run code for an
//     account beyond its budget.
//   - Hold mutable package-level state, read the wall clock implicitly, or
//     use floating point.
package sandboxrt
