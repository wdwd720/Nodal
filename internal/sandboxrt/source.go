package sandboxrt

import (
	"crypto/sha256"
	"sort"
)

// Source is a strategy's source tree: file name -> contents. A strategy is a
// single `package main` directory; names are plain file names, never paths.
type Source map[string][]byte

// sourceDigestDomain separates source digests from every other sha256 in the
// system.
const sourceDigestDomain = "nodal-strategy-source/v1\n"

// Paths returns the file names in byte order.
func (s Source) Paths() []string {
	out := make([]string, 0, len(s))
	for p := range s {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// TotalBytes is the sum of every file's length.
func (s Source) TotalBytes() int64 {
	var n int64
	for _, b := range s {
		n += int64(len(b))
	}
	return n
}

// Clone returns a deep copy.
func (s Source) Clone() Source {
	out := make(Source, len(s))
	for p, b := range s {
		out[p] = append([]byte(nil), b...)
	}
	return out
}

// Digest is the canonical digest of the tree:
//
//	sha256("nodal-strategy-source/v1\n" || for each name in byte order:
//	       sha256(name) || sha256(contents))
//
// The construction is unambiguous (every component has a fixed length) and
// simple enough that the database recomputes it from the stored files
// (migration 00750, cp_marketplace_source_digest).
func (s Source) Digest() []byte {
	h := sha256.New()
	h.Write([]byte(sourceDigestDomain))
	for _, p := range s.Paths() {
		name := sha256.Sum256([]byte(p))
		body := sha256.Sum256(s[p])
		h.Write(name[:])
		h.Write(body[:])
	}
	return h.Sum(nil)
}
