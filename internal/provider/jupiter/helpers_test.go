package jupiter_test

import (
	"crypto/sha256"

	"github.com/gagliardetto/solana-go"
)

func mustPK(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

func hashOf(seed string) solana.Hash {
	sum := sha256.Sum256([]byte(seed))
	return solana.HashFromBytes(sum[:])
}

// objAt walks a decoded JSON value by string keys and integer indices and
// returns the object at the end, failing loudly on a shape mismatch.
func objAt(v any, path ...any) map[string]any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, ok := v.(map[string]any)
			if !ok {
				panic("objAt: expected object at " + k)
			}
			v = mm[k]
		case int:
			arr, ok := v.([]any)
			if !ok {
				panic("objAt: expected array")
			}
			v = arr[k]
		}
	}
	out, ok := v.(map[string]any)
	if !ok {
		panic("objAt: expected object at end of path")
	}
	return out
}
