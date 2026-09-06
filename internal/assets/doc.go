// Package assets is the asset registry: the canonical identity of every
// on-chain asset the platform can hold, price, or trade (PART 32, 33).
//
// Identity is (chain, mint_address). A symbol is display metadata only and is
// never used as an identifier. Every asset carries a lifecycle status
// (ACTIVE, CLOSE_ONLY, RESTRICTED, HALTED, DELISTING, DELISTED) and a risk
// class; the risk kernel and settlement compiler distinguish "prevent
// increasing exposure" (CLOSE_ONLY/RESTRICTED) from "prevent all
// transactions" (HALTED).
//
// This package must never: use a ticker as identity, silently create assets
// from provider metadata, or let an AGENT actor change asset status.
package assets
