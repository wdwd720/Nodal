// Package fees defines the explicit platform fee policy (PART 126).
//
// The platform fee is a separate, disclosed deduction computed from a
// versioned policy: basis points of the input quantity, bounded by optional
// minimum and maximum amounts, in a named fee asset. Quotes display venue
// fee, network estimate, and platform fee separately; fills post the platform
// fee as its own journal entries. The default policy charges zero.
//
// This package must never: hide a spread inside a quote, use floating point,
// or let an AGENT actor change the policy.
package fees
