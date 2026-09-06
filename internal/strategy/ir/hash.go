package ir

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/nodal/controlplane/internal/audit"
)

// Semantic-hash algorithm (shared verbatim with packages/strategy-sdk, see
// src/hash.ts):
//
//  1. Render the normalized document as JSON (Go: encoding/json on IR; TS:
//     the builder's output). Money is a string with two decimals ("50.00"),
//     basis points are integers, decimals are {"m": "<digits>", "s": <n>},
//     hashes are lowercase hex strings, optional fields are omitted when
//     unset, required lists are present even when empty.
//  2. Remove the top-level keys "hash", "version", "built_at" and
//     "lineage": the hash is over meaning, so provenance, the per-strategy
//     version number and the build time never change it (STRATEGY_IR.md §5:
//     an SDK document and a natural-language compilation with identical
//     semantics dedupe).
//  3. Canonicalize: object keys sorted bytewise at every level, no
//     whitespace, no HTML escaping, integers only (no fractions or
//     exponents), U+2028/U+2029 escaped as  / , strings otherwise
//     escaped as encoding/json does (audit.CanonicalJSON).
//  4. SHA-256 of the canonical bytes.
//
// The parity fixtures under testdata/parity are computed by Go and checked
// by both test suites, so the two implementations cannot drift silently.

// semanticExcluded are the top-level keys removed before hashing.
var semanticExcluded = []string{"hash", "version", "built_at", "lineage"}

// CanonicalJSON renders the whole document canonically (used for storage
// and for the TypeScript parity check of full documents).
func CanonicalJSON(ir *IR) ([]byte, error) {
	if ir == nil {
		return nil, ErrNilIR
	}
	doc, err := genericDocument(ir)
	if err != nil {
		return nil, err
	}
	return audit.CanonicalJSON(doc)
}

// SemanticDocument returns the canonical bytes step 3 hashes.
func SemanticDocument(ir *IR) ([]byte, error) {
	if ir == nil {
		return nil, ErrNilIR
	}
	doc, err := genericDocument(ir)
	if err != nil {
		return nil, err
	}
	for _, k := range semanticExcluded {
		delete(doc, k)
	}
	return audit.CanonicalJSON(doc)
}

// SemanticHash returns sha256(SemanticDocument(ir)).
func SemanticHash(ir *IR) ([]byte, error) {
	doc, err := SemanticDocument(ir)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(doc)
	return sum[:], nil
}

// SemanticHashOfJSON hashes a raw JSON document with the same algorithm
// without decoding it into an IR (used by tests and by the SDK parity
// fixtures, which carry documents rather than Go values).
func SemanticHashOfJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("ir: semantic hash: %w", err)
	}
	// A JSON "null" decodes into a nil map without error. Hashing it would
	// mint a plausible-looking ir_hash for something that is not a document,
	// so an absent object is rejected rather than digested.
	if doc == nil {
		return nil, fmt.Errorf("ir: semantic hash: %w", ErrNilIR)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("ir: semantic hash: trailing data after document")
	}
	for _, k := range semanticExcluded {
		delete(doc, k)
	}
	canon, err := audit.CanonicalJSON(doc)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canon)
	return sum[:], nil
}

// genericDocument marshals the normalized IR and decodes it into a generic
// map so keys can be removed and canonicalized uniformly.
func genericDocument(ir *IR) (map[string]any, error) {
	c, err := ir.Clone()
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("ir: encode: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("ir: decode generic: %w", err)
	}
	return doc, nil
}
