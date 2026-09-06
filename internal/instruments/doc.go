// Package instruments is the universal instrument model (PARTS 32, 33, 231):
// Venue, EconomicExposure, Instrument, VenueListing, and ExternalIdentifier.
//
// An Instrument is an internal, UUID-identified tradable definition (V1:
// SPOT_PAIR of two registered assets settled in a settlement asset). A
// VenueListing is how one venue exposes that instrument (native id, mints,
// precision, minimum notional, status, settlement rules). Instrument and
// listing status are distinct from asset status; the settlement compiler
// composes all three. EVENT_OUTCOME instruments can be represented but never
// activated in V1.
//
// This package must never: use a ticker as identity, let provider types leak
// into the model, or let an AGENT actor change any status.
package instruments
