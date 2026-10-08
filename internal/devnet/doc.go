// Package devnet is the Nodal sandbox devnet: a deterministic LOCAL
// simulation of a public chain, used for sandbox/devnet-mode testing of
// stable-asset issuance, bridging and delegated signing. It holds no real
// value, talks to no network, and is never a source of truth for real
// capital. Nothing it reports may be booked to the ledger, reconciled against
// a real balance, or shown to anyone as money.
//
// # What it simulates
//
// A Network is a registry of named chains ("sbx-solana", "sbx-ethereum").
// Each Chain has slots and blocks, a recent-blockhash window, a finality
// depth and a flat per-transaction fee in its native unit. Accounts are
// Ed25519 public keys whose text form is base58. A chain keeps a native
// balance per account and a token balance per (owner, mint); a mint has
// decimals, a supply, a mint authority and a freeze flag. Every amount is an
// exact uint64 of base units: additions are overflow-checked, subtractions
// are underflow-checked, and QuantityOf / AmountOf convert to and from
// money.Quantity at the boundary.
//
// Signatures are real. A transaction is signed with crypto/ed25519 over the
// exact canonical message bytes and verified the same way, so code under test
// exercises the signing path it would use against a public chain.
//
// # Wire format (version 1)
//
// All integers are little-endian; every variable-length field is
// length-prefixed; a transaction is at most MaxTransactionSize (4096) bytes.
//
//	Transaction := "ndvt" | u8 version=1 | u16 len | Message[len] | u8 n | n x Signature[64]
//	Message     := "ndvm" | u8 version=1 | u8 len | ChainID[len] | RecentBlockhash[32] | FeePayer[32]
//	               | u8 count (1..16) | count x Instruction
//	Instruction := u8 program | u16 len | data[len]
//
// Program data layouts are fixed per program (see the instruction types);
// a chain id inside data is u8-length-prefixed and a memo is 1..256 raw
// bytes. Decode is strict: it accepts exactly the bytes Encode produces and
// rejects unknown magic, versions and programs, wrong data lengths, invalid
// chain ids, trailing bytes and oversize input, so Encode(Decode(x)) == x
// for every accepted x. Decode never panics; FuzzDecode keeps it that way.
//
// # Signatures
//
// The required signers of a message are its fee payer followed by each
// instruction's authority (SYSTEM_TRANSFER from, TOKEN_TRANSFER from_owner,
// TOKEN_MINT_TO authority, TOKEN_BURN owner, BRIDGE_LOCK owner,
// BRIDGE_RELEASE authority), de-duplicated in order of first appearance.
// Signatures are positional: signature i must verify against signer i over
// the message bytes. Fewer signatures than signers is SIGNATURE_MISSING, more
// is SIGNATURE_EXTRA (an extra or unknown signer), and one that does not
// verify is SIGNATURE_INVALID. The TxID is the base58 form of the first
// signature, which is always the fee payer's. Whether a named authority is
// the one the chain recognises (a mint's authority, the bridge authority) is
// state, so a mismatch is decided at execution and fails the transaction with
// AUTHORITY_SIGNATURE_MISSING.
//
// # Lifecycle and finality
//
// Submit decodes, validates and verifies a transaction, then places it in the
// mempool: PENDING. ProduceBlock advances the slot by one and includes every
// eligible pending transaction in submission order, executing each one
// atomically: the fee is charged first (Solana-like: a failed transaction
// still pays its fee when the fee payer can afford it), then every
// instruction runs against a scratch overlay that is committed only if all of
// them succeed. A transaction whose instructions fail is FAILED with a stable
// ErrorCode and moves nothing but its fee; one whose fee payer cannot pay is
// FAILED with INSUFFICIENT_FUNDS_FOR_FEE and moves nothing at all. Included
// transactions are CONFIRMED, and become FINALIZED once FinalityDepth further
// blocks exist; a FAILED transaction reports Final the same way. Blocks are
// never produced by the passage of time: only ProduceBlock advances a chain.
//
// # Replay protection
//
// A transaction must reference a blockhash among the BlockhashWindow most
// recent canonical blocks, both when submitted and when included; a pending
// transaction whose blockhash ages out is forgotten (NOT_FOUND) and can never
// land. A TxID applies at most once: resubmitting a known transaction is
// idempotent and returns its TxID with no error, leaving its status as it
// was. Because Ed25519 does not make a signature unique for an adversarial
// key, the chain also refuses a second signature over a message it already
// holds (DUPLICATE_MESSAGE) and a known TxID carrying a different message
// (TXID_CONFLICT).
//
// # Fault injection
//
// The sandbox hooks are explicit methods and nothing else triggers them.
// DropNext(n), TimeoutButLand and TimeoutAndDrop arm one-shot faults consumed
// in order by the next newly accepted submissions: a drop returns success but
// the transaction is never included (NOT_FOUND); a timeout returns
// SUBMISSION_STATE_UNKNOWN while the transaction is (TimeoutButLand) or is
// not (TimeoutAndDrop) placed in the mempool, the two halves of the unknown
// submission case. DelayInclusion(blocks) holds every transaction accepted
// afterwards for that many blocks. Reorg(depth, mode) removes up to depth of
// the most recent non-finalized blocks, reverts their effects atomically and
// either returns their transactions to the mempool (ReorgRequeue) or forgets
// them (ReorgDrop); a finalized block is never removed.
//
// # Bridge
//
// BRIDGE_LOCK moves tokens from the owner into the chain's escrow address
// (BridgeEscrowAddress, which no key controls) and consumes the lock nonce on
// that chain. BuildRelease is a pure relay helper: from a FINALIZED lock it
// builds the BRIDGE_RELEASE message for the destination chain, which mints
// the amount to the destination owner once the configured bridge authority
// has signed. A release is single-use per (source chain, source tx id) and per
// (source chain, nonce), so a duplicate relay fails with
// BRIDGE_ALREADY_RELEASED and nothing is minted twice.
//
// # Determinism
//
// There is no wall clock, no randomness and no package-level state. The same
// sequence of calls on a fresh Network produces the same TxIDs, blocks,
// statuses and StateHash on every run, on every machine. Operator calls
// (Airdrop, CreateMint, SetMintFrozen) take effect immediately, belong to no
// block and are never reverted by Reorg.
//
// # What this package must never do
//
//   - Hold, receive or move real value, or connect to any network, RPC or
//     provider. It is in-process memory and nothing else.
//   - Be treated as a source of truth for real capital: its balances,
//     statuses and hashes describe a simulation only.
//   - Read the wall clock or a random source. Time moves only through
//     ProduceBlock; keys come only from caller-supplied seeds.
//   - Custody keys or apply signing policy. It offers Ed25519 helpers
//     (NewKeypairFromSeed, Keypair.Sign, Verify) and nothing that stores,
//     selects or authorises keys; that belongs to the packages that hold
//     keys behind policy.
//   - Use floating point. Every amount is an exact integer of base units.
//   - Panic on external input. Decode and Submit reject malformed bytes with
//     VALIDATION_FAILED.
//   - Revert a FINALIZED transaction, or apply the same TxID twice.
package devnet
