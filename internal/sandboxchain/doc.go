// Package sandboxchain opens the simulated chains the sandbox stable asset and
// the bridge run on, and derives every platform key they sign with from one
// configured seed (advanced goal §5, §18; ADR-0020).
//
// The chains are the persistent devnet host (internal/devnethost): sbx-solana
// and sbx-ethereum, journaled event by event in Postgres under one network
// name, so every process that opens the same network sees the same chains.
// The keys are four: the custodial wallet signer that holds every customer's
// sandbox wallet key (stableasset.CustodialWalletSigner, which the bridge also
// uses as its holder signer), the stable asset's mint authority
// (stableasset.IssuerSigner), the reserve snapshot signer
// (stableasset.SnapshotSigner) and the bridge attester (bridge.Attester),
// whose public key the host's GENESIS pins.
//
// # One seed, one network, every binary
//
// The API, the advanced worker and the demo and performance tools all open
// the chain. They must derive exactly the same keys, or the binary that next
// signs would disagree with the assets already minted and with the attester
// the genesis recorded. That is why the derivation lives here and nowhere
// else: each key is HMAC-SHA256(seed, "nodal/sandboxchain/v1/" + purpose), and
// the purposes are fixed constants. A binary given the wrong seed for an
// existing network fails closed when it opens the host, because the host
// refuses a history recorded under a different attester.
//
// # Must never
//
//   - derive a key from anything but the seed and a fixed purpose label;
//   - expose a private key or the seed (the signers hold their keys; this
//     package keeps no copy);
//   - move or represent anything but SIMULATED value: the host is a simulated
//     chain, and every asset on it is value domain SIMULATED;
//   - fall back to a default seed: a missing seed leaves the chain unwired.
package sandboxchain
