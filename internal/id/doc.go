// Package id provides the typed, time-ordered identifiers used for every
// aggregate in the control plane.
//
// # Responsibility
//
// An ID[K] is a 16-byte RFC 9562 UUID version 7: the top 48 bits are the
// creation time in Unix milliseconds, so IDs sort by creation order in byte,
// string and database form. The phantom type parameter K makes identifiers of
// different aggregates distinct compile-time types: an ID[accountKind] cannot
// be passed where an ID[orderKind] is expected, even though both are UUIDs.
//
// Domain kinds are declared next to the aggregate they identify, never here:
//
//	type accountKind struct{}
//	type AccountID = id.ID[accountKind]
//	func NewAccountID() AccountID { return id.New[accountKind]() }
//
// # Guarantees
//
//   - New is monotonic within a process: IDs created in the same millisecond
//     still compare strictly increasing (google/uuid keeps a process-wide
//     sequence in the rand_a bits and bumps the timestamp when it would wrap).
//   - Parse accepts only the canonical 36-character form and only version 7
//     (or the Nil UUID, which is the zero ID). Braces, URNs, 32-hex and other
//     UUID versions are rejected. ParseAny exists for external references.
//   - The zero ID is a first-class "absent" value: it renders as the Nil UUID,
//     parses back to zero, and maps to SQL NULL. Callers that require a real
//     identifier must check IsZero; a missing field and a Nil UUID are treated
//     identically so the same validation covers both.
//   - Parse never panics on any input (see FuzzParse).
//
// # Never
//
// This package must never depend on domain packages, on the database, or on
// configuration. It must never use ticker symbols, emails, wallet addresses or
// provider identifiers as identity; those are external references stored in
// their own columns (goal PART 15).
package id
