// Package stream delivers realtime client updates over Server-Sent Events
// (PART 109): balances/buying power, intents, orders, deposits and agent state.
//
// The stream is a hint, never truth. Every event carries a monotonically
// increasing id; a client resumes with Last-Event-ID and, when the requested
// position has fallen out of the bounded replay buffer, receives a `resync`
// event telling it to refetch canonical REST state. Events are filtered per
// principal: customers see only their own accounts' events; operators with
// account:read_any see everything.
//
// This package must never: compute financial figures (it relays the ids and
// state names the backend already persisted), buffer without bound, or leak
// another tenant's events.
package stream
