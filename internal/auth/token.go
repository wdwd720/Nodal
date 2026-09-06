package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	tokenBytes      = 32
	tokenEncodedLen = 43 // base64url without padding of 32 bytes
	tokenHashHexLen = 64 // hex of sha256
)

// NewToken returns a fresh session token: 32 bytes from crypto/rand,
// base64url-encoded without padding (43 characters). It is the only value
// that ever leaves the server, inside the session cookie.
func NewToken() (string, error) {
	var b [tokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken returns hex(sha256(raw)), the value stores are keyed by. A
// database leak therefore exposes no usable tokens, and lookups are exact
// (no timing side channel on the raw token).
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ValidateToken rejects anything that is not the exact shape NewToken
// produces, so malformed cookies never reach the store.
func ValidateToken(raw string) error {
	if len(raw) != tokenEncodedLen {
		return fmt.Errorf("%w: length %d", ErrInvalidToken, len(raw))
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != tokenBytes {
		return fmt.Errorf("%w: not base64url", ErrInvalidToken)
	}
	return nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
