// Package pii encrypts personal data before it reaches the database, and is
// the only thing that writes it.
//
// # What this decides, and why it could not be decided before
//
// `identity_pii` was created by migration 00010 with `email_encrypted`,
// `legal_name_encrypted`, `dob_encrypted` and `key_version`, and for a year
// nothing in this repository wrote or read those columns: the encryption was
// DESIGNED, the table was empty everywhere, and F-47 -- whether `cp_readonly`
// and `cp_ops` may SELECT it -- was recorded as premature rather than open,
// because whether SELECT on a column is an exposure depends on whether the
// column holds ciphertext.
//
// This package is the encryption, so the question can now be answered from the
// architecture instead of by preference: the key never touches the database,
// so a role that can read the table reads bytes it cannot use, and a role that
// has no business with the table should not have SELECT on it at all
// (migration 00754).
//
// # The construction
//
// AES-256-GCM, one random 96-bit nonce per seal, prepended to the ciphertext.
// The additional authenticated data binds the ciphertext to the table, the
// column, the row and the key version:
//
//	identity_pii \x00 <column> \x00 <user_id> \x00 v<version>
//
// so a ciphertext moved to another user's row, another column, or relabelled
// with another key version fails to open. That is the difference between
// "encrypted" and "encrypted, and cannot be rearranged by whoever can write
// the table".
//
// # Keys
//
// A Keyring is several versioned keys and one active version, parsed from a
// single secret:
//
//	{"active": 2, "keys": {"1": "<base64 32 bytes>", "2": "<base64 32 bytes>"}}
//
// New writes use the active version; reads use whichever version the row
// names. Rotation is: add a key, make it active, deploy, then re-seal rows at
// leisure (Store.Reseal) and remove the old key once no row names it. One
// secret rather than one per version because a SecretRef is one thing the
// deployment has to supply, and the launch tier supplies it from an
// environment variable at no cost. KMS envelope encryption is the paid-tier
// upgrade and changes nothing about the row format.
//
// # What this does not do
//
// It does not make `email_hash` unnecessary: that column is a lookup key and
// this package deliberately offers no equality search over ciphertext. It
// does not log plaintext, ever; errors name the column, never the value.
package pii

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// KeySize is the AES-256 key length in bytes.
const KeySize = 32

const nonceSize = 12

// Keyring is a set of versioned keys with one active version.
type Keyring struct {
	active int
	keys   map[int]cipher.AEAD
}

type keyringDoc struct {
	Active int               `json:"active"`
	Keys   map[string]string `json:"keys"`
}

// ParseKeyring parses the keyring document. Every key must decode to exactly
// 32 bytes, every version must be a positive integer, and the active version
// must be one of the keys. A keyring that fails any of these is refused
// whole: a half-usable keyring would write rows nothing can read.
func ParseKeyring(secret string) (*Keyring, error) {
	var doc keyringDoc
	if err := json.Unmarshal([]byte(strings.TrimSpace(secret)), &doc); err != nil {
		return nil, fmt.Errorf("pii: keyring is not the expected JSON document: %w", err)
	}
	if len(doc.Keys) == 0 {
		return nil, errors.New("pii: keyring has no keys")
	}
	kr := &Keyring{active: doc.Active, keys: make(map[int]cipher.AEAD, len(doc.Keys))}
	for vs, b64 := range doc.Keys {
		v, err := strconv.Atoi(vs)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("pii: keyring version %q is not a positive integer", vs)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, fmt.Errorf("pii: keyring version %d is not base64", v)
		}
		if len(raw) != KeySize {
			return nil, fmt.Errorf("pii: keyring version %d is %d bytes, want %d", v, len(raw), KeySize)
		}
		block, err := aes.NewCipher(raw)
		if err != nil {
			return nil, fmt.Errorf("pii: keyring version %d: %w", v, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("pii: keyring version %d: %w", v, err)
		}
		kr.keys[v] = aead
	}
	if _, ok := kr.keys[kr.active]; !ok {
		return nil, fmt.Errorf("pii: keyring active version %d is not among its keys", kr.active)
	}
	return kr, nil
}

// Active is the version new writes are sealed under.
func (k *Keyring) Active() int { return k.active }

// Versions lists the key versions the ring can open, ascending.
func (k *Keyring) Versions() []int {
	out := make([]int, 0, len(k.keys))
	for v := range k.keys {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// aad binds a ciphertext to its place. The user id is the row; the column is
// which of the three; the version is which key -- all three are part of what
// is authenticated, so none can be changed by whoever can write the row.
func aad(userID, column string, version int) []byte {
	return []byte("identity_pii\x00" + column + "\x00" + userID + "\x00v" + strconv.Itoa(version))
}

// Seal encrypts plaintext for one column of one user's row under the active
// key and returns the ciphertext and the version it was sealed under.
func (k *Keyring) Seal(userID, column string, plaintext []byte) ([]byte, int, error) {
	if userID == "" || column == "" {
		return nil, 0, errors.New("pii: seal needs a user id and a column")
	}
	aead := k.keys[k.active]
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, fmt.Errorf("pii: nonce: %w", err)
	}
	out := make([]byte, 0, nonceSize+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, aad(userID, column, k.active))
	return out, k.active, nil
}

// Open decrypts a ciphertext sealed for the same user, column and version. It
// fails if the ring lacks that version, if the ciphertext was moved or
// relabelled, or if it was altered.
func (k *Keyring) Open(userID, column string, version int, ciphertext []byte) ([]byte, error) {
	aead, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("pii: %s: sealed under key version %d, which this keyring does not hold", column, version)
	}
	if len(ciphertext) < nonceSize+aead.Overhead() {
		return nil, fmt.Errorf("pii: %s: ciphertext is too short to be one", column)
	}
	pt, err := aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], aad(userID, column, version))
	if err != nil {
		// The AEAD error is deliberately not wrapped with detail: there is
		// exactly one reason it fails and it is "this is not the ciphertext
		// for this place under this key".
		return nil, fmt.Errorf("pii: %s: ciphertext does not open for this row, column and key version", column)
	}
	return pt, nil
}
