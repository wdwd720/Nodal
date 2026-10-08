package devnet

import (
	"crypto/sha256"
	"math/big"
	"math/bits"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// ChainID names a simulated chain, e.g. "sbx-solana" or "sbx-ethereum".
type ChainID string

// MaxChainIDLen bounds a ChainID; on the wire it is u8-length-prefixed.
const MaxChainIDLen = 32

// Validate checks that id is 1..MaxChainIDLen bytes of [a-z0-9-].
func (id ChainID) Validate() error {
	if len(id) == 0 || len(id) > MaxChainIDLen {
		return invalid(CodeInvalidChainID, "chain id must be 1 to 32 bytes")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return invalid(CodeInvalidChainID, "chain id may contain only a-z, 0-9 and '-'")
		}
	}
	return nil
}

// TxID identifies a transaction: the base58 form of its first signature,
// which is always the fee payer's.
type TxID string

// TxIDFromSignature returns the TxID of a transaction whose first signature
// is sig.
func TxIDFromSignature(sig Signature) TxID { return TxID(sig.String()) }

// Signature returns the signature id names, insisting on the canonical
// base58 form.
func (id TxID) Signature() (Signature, error) {
	return ParseSignature(string(id))
}

// Domain-separation tags for derived addresses and hashes. Each names its
// purpose and version so no two derivations can collide.
const (
	feeCollectorDomain = "nodal-devnet/fee-collector/v1"
	bridgeEscrowDomain = "nodal-devnet/bridge-escrow/v1"
	genesisDomain      = "nodal-devnet/genesis/v1"
	blockDomain        = "nodal-devnet/block/v1"
	stateDomain        = "nodal-devnet/state/v1"
	networkDomain      = "nodal-devnet/network/v1"
)

// deriveAddress returns SHA-256(domain || 0x00 || chain) as an address. It is
// a hash output, not a key derivation: nobody knows a private key for it, so
// nothing can sign as it.
func deriveAddress(domain string, chain ChainID) PublicKey {
	buf := make([]byte, 0, len(domain)+1+len(chain))
	buf = append(buf, domain...)
	buf = append(buf, 0)
	buf = append(buf, chain...)
	return PublicKey(sha256.Sum256(buf))
}

// FeeCollectorAddress is the default account that receives a chain's fees.
// No key controls it, so collected fees stay put and native supply is
// conserved.
func FeeCollectorAddress(chain ChainID) PublicKey {
	return deriveAddress(feeCollectorDomain, chain)
}

// BridgeEscrowAddress is the account that holds tokens locked by BRIDGE_LOCK
// on chain. No key controls it, so locked tokens cannot be moved by a
// transaction.
func BridgeEscrowAddress(chain ChainID) PublicKey {
	return deriveAddress(bridgeEscrowDomain, chain)
}

// QuantityOf converts an exact on-chain amount to a money.Quantity.
func QuantityOf(amount uint64) money.Quantity {
	return money.QuantityFromBigInt(new(big.Int).SetUint64(amount))
}

// AmountOf converts a money.Quantity to an on-chain amount. Negative values
// and values above the uint64 range fail with OVERFLOW; nothing is rounded or
// truncated.
func AmountOf(q money.Quantity) (uint64, error) {
	b := q.BigInt()
	if b.Sign() < 0 || !b.IsUint64() {
		return 0, newError(errs.CodeOverflow, CodeArithmeticOverflow, "amount is outside the uint64 range")
	}
	return b.Uint64(), nil
}

// add64 returns a+b and whether it fit in a uint64.
func add64(a, b uint64) (uint64, bool) {
	sum, carry := bits.Add64(a, b, 0)
	return sum, carry == 0
}

// addSat returns a+b, saturating at the uint64 maximum.
func addSat(a, b uint64) uint64 {
	sum, ok := add64(a, b)
	if !ok {
		return ^uint64(0)
	}
	return sum
}
