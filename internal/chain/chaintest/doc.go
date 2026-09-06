// Package chaintest provides test doubles for the chain-observation layer:
// a scripted Fake observer with fault injection, a slot-advancing Chain
// simulator that several Fake observers can watch (with independent lag, so
// disagreement and recovery scenarios are reproducible), and an in-memory
// RawArchive.
//
// It is test-only and must never be imported by production code; the
// composition root wires real adapters from config, never a Fake.
package chaintest
