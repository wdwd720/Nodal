// Package nativeasset is the registry for Nodal-native assets: the things a
// user creates inside the platform and other users can buy with Credits
// (gola.md PARTS XIII, XXXIV, LI).
//
// A native asset is not a token. It has no chain, no mint and no existence
// outside Nodal, and migration 00710 refuses to let it share a transaction
// with any real-capital domain. What it does have is a creator, a fixed
// supply, an immutable economic configuration, a moderation record and a
// status that governs whether anyone may increase exposure to it.
//
// # Identity
//
// A native asset IS a row in the shared asset registry: this package's table
// is keyed by asset_id. There is one identity from draft to delisting, so
// every ledger account, balance and journal entry that ever refers to the
// asset refers to the same row. The registry row is created at DRAFT with
// status RESTRICTED, which already means "may not increase exposure", so a
// draft cannot be traded even by a path that forgot to consult this package.
//
// # The lifecycle, and what it protects
//
//	DRAFT ──▶ PENDING_REVIEW ──▶ ACTIVE ──▶ CLOSE_ONLY ──▶ DELISTED
//	  │              │             │  ▲         │
//	  ▼              ▼             ▼  └─────────┘
//	REJECTED     REJECTED       HALTED
//
// The economics — supply, allocations, symbol, policy profile — are frozen at
// the moment of activation and the database enforces it (SQLSTATE NM003).
// PART XIII is explicit that a creator must not be able to change economics
// after buyers enter, and a comment saying so would not have stopped it.
//
// CLOSE_ONLY lets holders sell and nobody buy. That asymmetry is deliberate:
// freezing people into a position is worse than stopping new ones from
// forming, so a market being wound down still lets its holders out.
//
// # Moderation
//
// User-generated asset creation is user-generated content, and the screening
// here is deterministic and local: name and symbol shape, control and
// bidirectional characters, confusable-character normalisation against
// existing assets, reserved symbols, URL scheme safety, metadata size. It is
// the floor, not the ceiling. A Screener may be supplied for external
// classification, and the result of any external call is recorded rather than
// merged silently into a local verdict.
//
// # What this package must never do
//
//   - Let a creator change supply, allocation, symbol or policy after
//     activation.
//   - Activate an asset whose moderation state is not APPROVED.
//   - Describe an asset as an investment, or record a return expectation. The
//     marketing_restrictions column exists to say what may not be claimed.
//   - Mint units. Supply is minted into the market's pool by
//     internal/nativemarket in one transaction with the market's creation;
//     there is no other mint path, which is what makes "no invisible supply
//     changes" checkable.
//   - Assume a symbol is unique across the world. It is unique among native
//     assets, case-insensitively, and that is a statement about this economy
//     and not about anyone else's trademark.
package nativeasset
