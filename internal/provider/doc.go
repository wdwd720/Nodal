// Package provider holds the provider-neutral vocabulary shared by every
// external integration: health states (PART 79), the retry classification of
// operations (PART 106), and the verification label attached to each
// adapter (PART 208).
//
// Health is derived deterministically from observed samples with hysteresis
// so that the risk kernel and the settlement compiler see stable inputs. A
// provider that is DISABLED by a kill switch or UNHEALTHY by observation
// stops NEW external actions; it never stops status reads, observation, or
// reconciliation (PART 107) — that separation is enforced by the callers
// through the ActionClass they pass, not by this package hiding data.
//
// This package must never: contain provider-specific types, make network
// calls, or let an AGENT actor change a provider's state.
package provider
