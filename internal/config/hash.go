package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
)

// Redacted returns a deep copy that is safe to log: every SecretRef holding a
// plain value is replaced by RedactedMarker; references (env://, aws-sm://,
// file://) are kept because they name a location, not a value.
func (c *Config) Redacted() *Config {
	cp := c.Clone()
	forEachSecretRef(reflect.ValueOf(cp), "", func(_ string, ref *SecretRef) {
		*ref = ref.Redacted()
	})
	return cp
}

// Hash returns the hex SHA-256 of the canonical JSON (sorted keys) of every
// non-secret field, including Env and BuildVersion. Every SecretRef field is
// blanked before hashing, so neither a secret value nor where it lives can
// influence or be inferred from the hash. The hash is exposed by production
// instances and recorded on audited financial decisions (goal PART 222).
func (c *Config) Hash() string {
	sum := sha256.Sum256(c.hashInput())
	return hex.EncodeToString(sum[:])
}

func (c *Config) hashInput() []byte {
	cp := c.Clone()
	forEachSecretRef(reflect.ValueOf(cp), "", func(_ string, ref *SecretRef) { *ref = "" })
	b, err := canonicalJSON(cp)
	if err != nil {
		// Config contains only strings, integers, booleans, durations and
		// string slices; encoding cannot fail. This is a programmer error,
		// not external input.
		panic("config: canonical JSON: " + err.Error())
	}
	return b
}

// canonicalJSON encodes v as JSON with object keys sorted at every level and
// numbers preserved exactly.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	// encoding/json writes map keys in sorted order.
	return json.Marshal(generic)
}
