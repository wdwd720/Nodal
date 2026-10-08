// Package nodalsdk is the strategy SDK. It is compiled INTO every sandboxed
// strategy (GOOS=wasip1 GOARCH=wasm) by internal/sandboxrt's Builder, which
// embeds these sources and writes them next to the strategy under the module
// path nodal.dev/sdk. The same sources also compile on the host, where
// internal/sandboxrt reuses the exact decimal grammar so the host and the
// strategy can never disagree about what a number is.
//
// # What a strategy sees
//
// A strategy is a `package main` whose main calls Run with a function of a
// *Session. The Session is built from ONE canonical JSON input document read
// from stdin: a market snapshot (instrument ids, kinds, marks, best bid and
// ask, recent closes, all as exact decimal strings whose fractional digit
// count is their scale), a portfolio snapshot, and the approved configuration
// map. There is no other input: no clock, no environment, no files, no
// network. Time is the snapshot's as-of.
//
// # What a strategy can do
//
// Exactly two things, both written to stdout as newline-delimited JSON:
//
//   - EmitIntent proposes a typed economic intent (kind, instrument, side,
//     exact quantity, optional limit price). A proposal is not an order. The
//     host validates it again, drops anything outside the strategy's declared
//     manifest, and turns what remains into an economic intent that faces the
//     full risk kernel like every other intent.
//   - Log records a structured event.
//
// There is no record type that signs, transfers, withdraws or names a
// destination, and the host refuses every record it does not recognise.
//
// # Arithmetic
//
// Decimal is exact (an arbitrary-precision unscaled integer and a scale).
// Every operation that could lose precision takes an explicit Rounding mode.
// The indicators (SMA, EMA, Min, Max, Returns) are exact under the rounding
// mode the caller names. There is no floating point anywhere in this package.
//
// # What this package must never do
//
//   - Use floating point, read a clock, read the environment or the
//     filesystem, open a socket, or start a process.
//   - Write anything to stdout other than well-formed intent and log records.
//   - Import anything outside the small standard-library set its own import
//     test pins (bufio, bytes, encoding/json, errors, io, math/big, os, sort,
//     strconv, strings, unicode/utf8).
//   - Hold mutable package-level state.
package nodalsdk
