// Package signingtest holds test-only helpers for the signing boundary:
// deterministic Solana swap transaction fixtures (a golden Jupiter-shaped
// swap and the building blocks for mutated variants), instruction data
// encoders written from the SPL/Anchor specifications, and expectation
// builders matching the fixtures.
//
// It is imported only by tests (inspect, signing, wallettest, contract
// tests) and must never be imported by production wiring (lintfin rule for
// <pkg>test packages).
package signingtest
