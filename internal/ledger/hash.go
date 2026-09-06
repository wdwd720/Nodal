package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Content hashing.
//
// content_hash is sha256 over the canonical JSON of the economically
// significant part of a posting:
//
//	{ kind, idempotency_key, reference{type,id}, effective_at (RFC3339Nano UTC),
//	  entries[ {account{owner_type,owner_id,code,asset_id}, side, quantity} ]
//	  (sorted by their own canonical form), reversal_of (or null), metadata }
//
// Canonical JSON has recursively sorted object keys, no insignificant
// whitespace, no HTML escaping and numbers verbatim. Entry order, metadata
// key order, description, correlation id and USD valuations do not affect
// the hash; any change to a quantity, side, account, asset, kind, key,
// reference, time, reversal target or metadata value does. The proof system
// chains these hashes later.

type hashAccount struct {
	OwnerType string `json:"owner_type"`
	OwnerID   string `json:"owner_id"`
	Code      string `json:"code"`
	AssetID   string `json:"asset_id"`
}

type hashEntry struct {
	Account  hashAccount `json:"account"`
	Side     string      `json:"side"`
	Quantity string      `json:"quantity"`
}

type hashReference struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type hashDocument struct {
	Kind           string            `json:"kind"`
	IdempotencyKey string            `json:"idempotency_key"`
	Reference      hashReference     `json:"reference"`
	EffectiveAt    string            `json:"effective_at"`
	Entries        []json.RawMessage `json:"entries"`
	ReversalOf     *string           `json:"reversal_of"`
	Metadata       json.RawMessage   `json:"metadata"`
}

// CanonicalContent returns the canonical JSON that ContentHash hashes. It
// fails with VALIDATION_FAILED when Metadata is not JSON-encodable.
func CanonicalContent(p Posting) ([]byte, error) {
	entries := make([]json.RawMessage, 0, len(p.Entries))
	for _, e := range p.Entries {
		acct := e.Account.normalized()
		raw, err := canonicalJSON(hashEntry{
			Account: hashAccount{
				OwnerType: string(acct.OwnerType),
				OwnerID:   acct.OwnerID,
				Code:      string(acct.Code),
				AssetID:   acct.AssetID.String(),
			},
			Side:     string(e.Side),
			Quantity: e.Quantity.String(),
		})
		if err != nil {
			return nil, err
		}
		entries = append(entries, raw)
	}
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i], entries[j]) < 0 })

	meta, err := canonicalMetadata(p.Metadata)
	if err != nil {
		return nil, err
	}
	var reversalOf *string
	if p.ReversalOf != nil {
		s := p.ReversalOf.String()
		reversalOf = &s
	}
	doc := hashDocument{
		Kind:           string(p.Kind),
		IdempotencyKey: p.IdempotencyKey,
		Reference:      hashReference{Type: p.Reference.Type, ID: p.Reference.ID},
		EffectiveAt:    p.EffectiveAt.UTC().Format(time.RFC3339Nano),
		Entries:        entries,
		ReversalOf:     reversalOf,
		Metadata:       meta,
	}
	return canonicalJSON(doc)
}

// ContentHash returns sha256(CanonicalContent(p)).
func ContentHash(p Posting) ([]byte, error) {
	content, err := CanonicalContent(p)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	return sum[:], nil
}

// canonicalMetadata canonicalises the metadata map; nil and empty both
// become "{}" so they hash identically.
func canonicalMetadata(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	raw, err := canonicalJSON(m)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "metadata is not JSON-encodable")
	}
	return raw, nil
}

// canonicalJSON marshals v with encoding/json, then rewrites the result with
// recursively sorted keys, no whitespace and no HTML escaping. Numbers pass
// through verbatim as json.Number so no binary conversion ever happens.
func canonicalJSON(v any) ([]byte, error) {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("ledger: canonical json: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw.Bytes()))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("ledger: canonical json: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("ledger: canonical json: trailing data")
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, generic); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case json.Number:
		buf.WriteString(x.String())
	case string:
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err != nil {
			return fmt.Errorf("ledger: canonical json: %w", err)
		}
		buf.Truncate(buf.Len() - 1) // Encode appends a newline
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		// Unreachable: json.Decoder into `any` only yields the cases above.
		return fmt.Errorf("ledger: canonical json: unsupported value %T", v)
	}
	return nil
}
