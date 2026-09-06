// Package ratelimit implements infrastructure rate limits (PART 180): user
// API limits, authentication limits, quote limits, and provider protection.
//
// Rate limiting is NOT financial authority (PART 181): the risk kernel
// enforces order frequency from persisted state independently, so a Redis
// reset can never let an agent exceed its deterministic budget. Redis is an
// acceptable store for these counters because losing them only loosens
// transport-level protection temporarily.
//
// Two stores exist: Redis (shared across replicas; production) and an
// in-memory store (single process; LOCAL/TEST and the fallback when Redis is
// unavailable — fail open on the limiter, never on money).
package ratelimit
