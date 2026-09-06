package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// ArchiveWriter stores raw evidence bodies (provider requests and responses,
// unsigned and signed transaction bytes) and returns an opaque reference.
// The production implementation writes to object storage with Object Lock;
// tests use an in-memory fake. Keys are hierarchical paths; a writer may
// prefix them but must return a reference that resolves to exactly the
// stored bytes.
type ArchiveWriter interface {
	Put(ctx context.Context, key, contentType string, body []byte) (ref string, err error)
}

// Evidence is the reference and digest of one archived body. RawRef goes
// into the *_ref columns; Hash is the SHA-256 of the exact bytes stored.
type Evidence struct {
	RawRef string
	Hash   []byte
	Size   int
}

// HexHash renders the digest in lowercase hex.
func (e Evidence) HexHash() string { return hex.EncodeToString(e.Hash) }

// ContentTypeJSON is the content type of encoded evidence.
const ContentTypeJSON = "application/json"

// ContentTypeBinary is the content type of raw transaction bytes.
const ContentTypeBinary = "application/octet-stream"

// StoreEvidence JSON-encodes v, stores it under key and returns the
// reference and hash. json.RawMessage values are stored verbatim (compacted)
// so a provider payload is archived exactly as received.
func StoreEvidence(ctx context.Context, w ArchiveWriter, key string, v any) (Evidence, error) {
	if w == nil {
		return Evidence{}, errs.New(errs.CodeInternal, "execution: evidence archive is not configured")
	}
	var body []byte
	switch x := v.(type) {
	case json.RawMessage:
		body = []byte(x)
	case []byte:
		body = x
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return Evidence{}, errs.Wrap(err, errs.CodeInternal, "execution: encode evidence")
		}
		body = b
	}
	return StoreRaw(ctx, w, key, ContentTypeJSON, body)
}

// StoreRaw stores body under key with the given content type.
func StoreRaw(ctx context.Context, w ArchiveWriter, key, contentType string, body []byte) (Evidence, error) {
	if w == nil {
		return Evidence{}, errs.New(errs.CodeInternal, "execution: evidence archive is not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.Contains(key, "..") {
		return Evidence{}, errs.New(errs.CodeValidationFailed, "execution: evidence key is invalid").WithField("key", key)
	}
	sum := sha256.Sum256(body)
	ref, err := w.Put(ctx, key, contentType, body)
	if err != nil {
		return Evidence{}, errs.Wrap(err, errs.CodeInternal, "execution: archive evidence")
	}
	if ref == "" {
		return Evidence{}, errs.New(errs.CodeInternal, "execution: archive returned an empty reference")
	}
	return Evidence{RawRef: ref, Hash: sum[:], Size: len(body)}, nil
}

// EvidenceKey builds the canonical archive key
// execution/<scope>/<scope id>/<attempt or seq>/<kind>.
func EvidenceKey(scope, scopeID, part, kind string) string {
	return fmt.Sprintf("execution/%s/%s/%s/%s", scope, scopeID, part, kind)
}

// HashBytes returns the SHA-256 of b.
func HashBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
