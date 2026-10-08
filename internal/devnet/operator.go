package devnet

import (
	"github.com/nodal/controlplane/internal/errs"
)

// Operator controls set up and steer the sandbox. They take effect
// immediately, belong to no block, and are never reverted by Reorg. They are
// the sandbox equivalent of a faucet, genesis configuration and an issuer's
// freeze switch, and exist only because this is a simulation.

// MaxDecimals bounds a mint's decimals; 10^18 base units still leave room in
// a uint64 for more than 18 whole units.
const MaxDecimals = 18

// MintConfig describes a mint to create.
type MintConfig struct {
	// Address is the mint's address. It need not be a key anyone holds.
	Address PublicKey
	// Decimals is the number of decimal places of one whole unit (at most
	// MaxDecimals). It is informational: every amount is in base units.
	Decimals uint8
	// Authority is the only key whose TOKEN_MINT_TO succeeds.
	Authority PublicKey
	// Frozen stops every transfer, mint, burn, lock and release of the mint.
	Frozen bool
}

// MintInfo is a mint's configuration and current supply.
type MintInfo struct {
	MintConfig
	Supply uint64
}

// Airdrop credits lamports of new native supply to an account, like a devnet
// faucet. Total native supply must stay within the uint64 range (OVERFLOW
// otherwise), which also guarantees that no single balance can overflow.
func (c *Chain) Airdrop(to PublicKey, lamports uint64) error {
	if to.IsZero() {
		return invalid(CodeInvalidArgument, "airdrop recipient is the zero key")
	}
	if lamports == 0 {
		return invalid(CodeInvalidArgument, "airdrop amount must be greater than zero")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	supply, ok := add64(c.state.nativeSupply, lamports)
	if !ok {
		return newError(errs.CodeOverflow, CodeArithmeticOverflow, "native supply would exceed the uint64 range")
	}
	t := c.state.begin()
	if code := credit(&t.native, to, lamports); code != "" {
		// Unreachable: a balance never exceeds the supply that just fit.
		return newError(errs.CodeInternal, CodeInvariantViolated, "airdrop overflowed a balance")
	}
	t.commit()
	c.state.nativeSupply = supply
	return nil
}

// CreateMint creates a mint with zero supply. Issuance is a TOKEN_MINT_TO
// transaction signed by the authority, so it goes through blocks and
// finality like any other transfer of value.
func (c *Chain) CreateMint(cfg MintConfig) error {
	switch {
	case cfg.Address.IsZero():
		return invalid(CodeInvalidArgument, "mint address is the zero key")
	case cfg.Authority.IsZero():
		return invalid(CodeInvalidArgument, "mint authority is the zero key")
	case cfg.Decimals > MaxDecimals:
		return invalid(CodeInvalidArgument, "mint decimals must be at most 18")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.state.mints[cfg.Address]; exists {
		return newError(errs.CodeConflict, CodeMintExists, "mint "+cfg.Address.String()+" already exists")
	}
	c.state.mints[cfg.Address] = mintInfo{decimals: cfg.Decimals, authority: cfg.Authority, frozen: cfg.Frozen}
	return nil
}

// SetMintFrozen freezes or unfreezes a mint, the way a stable-asset issuer
// can. Pending transactions are affected when they execute; blocks already
// produced are not.
func (c *Chain) SetMintFrozen(mint PublicKey, frozen bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.state.mints[mint]
	if !ok {
		return newError(errs.CodeNotFound, CodeMintNotFound, "mint "+mint.String()+" not found")
	}
	m.frozen = frozen
	c.state.mints[mint] = m
	return nil
}
