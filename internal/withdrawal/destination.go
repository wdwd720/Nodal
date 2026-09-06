package withdrawal

import (
	"errors"
	"math/big"
)

// Solana addresses are base58 (Bitcoin alphabet) encodings of 32 bytes,
// which render as 32 to 44 characters.
const (
	solanaAddressMinLen = 32
	solanaAddressMaxLen = 44
	solanaKeyBytes      = 32
	base58Alphabet      = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
)

var base58Index = func() [256]int8 {
	var idx [256]int8
	for i := range idx {
		idx[i] = -1
	}
	for i := 0; i < len(base58Alphabet); i++ {
		idx[base58Alphabet[i]] = int8(i)
	}
	return idx
}()

// Destination validation errors.
var (
	ErrAddressLength   = errors.New("address must be 32 to 44 base58 characters")
	ErrAddressAlphabet = errors.New("address contains characters outside the base58 alphabet")
	ErrAddressBytes    = errors.New("address does not decode to a 32-byte key")
	ErrAddressZero     = errors.New("address is the all-zero key")
)

// DecodeBase58 decodes a Bitcoin-alphabet base58 string. It never panics
// on any input.
func DecodeBase58(s string) ([]byte, error) {
	if s == "" {
		return nil, ErrAddressLength
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	n := new(big.Int)
	radix := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		d := base58Index[s[i]]
		if d < 0 {
			return nil, ErrAddressAlphabet
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(d)))
	}
	body := n.Bytes()
	out := make([]byte, zeros+len(body))
	copy(out[zeros:], body)
	return out, nil
}

// ValidateSolanaAddress accepts a well-formed Solana public key: base58,
// 32 decoded bytes, not the all-zero system key. It does not (and cannot)
// prove the key is owned by anyone; ownership is the customer's
// attestation and the approval step's concern.
func ValidateSolanaAddress(s string) error {
	if len(s) < solanaAddressMinLen || len(s) > solanaAddressMaxLen {
		return ErrAddressLength
	}
	raw, err := DecodeBase58(s)
	if err != nil {
		return err
	}
	if len(raw) != solanaKeyBytes {
		return ErrAddressBytes
	}
	allZero := true
	for _, b := range raw {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ErrAddressZero
	}
	return nil
}
