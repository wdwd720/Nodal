package sandboxchain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/stableasset"
)

// Purposes name the keys derived from the seed. Changing one changes the key
// derived from it and orphans everything that key controls on an existing
// network, so they are constants and never configuration.
const (
	PurposeCustody   = "custody"
	PurposeIssuer    = "issuer"
	PurposeSnapshots = "reserve-snapshots"
	PurposeAttester  = "bridge-attester"
	// PurposeDelegatedSigner is the master seed of the delegated wallet
	// signer (internal/delegation), which derives one key per account and
	// chain from it; PurposeDelegationVenue prefixes each venue co-signer's
	// purpose (VenueSeed).
	PurposeDelegatedSigner = "delegated-signer"
	PurposeDelegationVenue = "delegation-venue/"
)

// derivationLabel versions the derivation itself.
const derivationLabel = "nodal/sandboxchain/v1/"

// Seed is the 32-byte secret every platform sandbox chain key derives from.
type Seed [32]byte

// ParseSeed decodes a seed from exactly 64 hexadecimal characters
// (surrounding whitespace, such as a file's trailing newline, is ignored). An
// all-zero seed is refused: it is what an unset value looks like.
func ParseSeed(s string) (Seed, error) {
	var seed Seed
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != len(seed) {
		return Seed{}, errs.New(errs.CodeValidationFailed, "sandboxchain: the seed must be exactly 64 hexadecimal characters")
	}
	copy(seed[:], raw)
	if seed == (Seed{}) {
		return Seed{}, errs.New(errs.CodeValidationFailed, "sandboxchain: the seed must not be all zeros")
	}
	return seed, nil
}

// Derive returns the 32-byte key seed for one purpose:
// HMAC-SHA256(seed, "nodal/sandboxchain/v1/" + purpose).
func (s Seed) Derive(purpose string) [32]byte {
	mac := hmac.New(sha256.New, s[:])
	mac.Write([]byte(derivationLabel + purpose))
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// VenueSeed is the co-signer seed of one delegation venue, by its name.
func (s Seed) VenueSeed(venue string) [32]byte { return s.Derive(PurposeDelegationVenue + venue) }

// Keys are the platform's sandbox chain signers.
type Keys struct {
	Custody   *stableasset.CustodialWalletSigner
	Issuer    *stableasset.IssuerSigner
	Snapshots *stableasset.SnapshotSigner
	Attester  *bridge.Attester
}

// DeriveKeys builds every signer from the seed.
func DeriveKeys(seed Seed) Keys {
	return Keys{
		Custody:   stableasset.NewCustodialWalletSigner(seed.Derive(PurposeCustody)),
		Issuer:    stableasset.NewIssuerSigner(seed.Derive(PurposeIssuer)),
		Snapshots: stableasset.NewSnapshotSigner(seed.Derive(PurposeSnapshots)),
		Attester:  bridge.NewAttester(seed.Derive(PurposeAttester)),
	}
}

// Chain is an opened host with the keys that sign on it.
type Chain struct {
	Network string
	Host    *devnethost.Host
	Keys    Keys
}

// Open opens the network's host over the database -- writing its GENESIS on
// first use, replaying and verifying its history otherwise -- with the
// default chain parameters and the attester derived from seed. A history
// recorded under another attester (another seed) is refused.
func Open(ctx context.Context, d *db.DB, clk clock.Clock, env config.Environment, network string, seed Seed) (Chain, error) {
	if d == nil || clk == nil {
		return Chain{}, errs.New(errs.CodeInternal, "sandboxchain: Open requires the database and a clock")
	}
	if seed == (Seed{}) {
		return Chain{}, errs.New(errs.CodeValidationFailed, "sandboxchain: no seed; the sandbox chain stays unwired without one")
	}
	keys := DeriveKeys(seed)
	host, err := devnethost.Open(ctx, devnethost.NewPgStore(d), devnethost.DefaultConfig(network, env, keys.Attester.PublicKey()), clk)
	if err != nil {
		return Chain{}, err
	}
	return Chain{Network: network, Host: host, Keys: keys}, nil
}
