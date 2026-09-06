package inspect

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

// Jupiter aggregator v6 instruction layout (Anchor).
//
// Instruction data = 8-byte Anchor discriminator sha256("global:<name>")[:8]
// followed by borsh-encoded arguments:
//
//	route(route_plan: Vec<RoutePlanStep>, in_amount: u64, quoted_out_amount: u64,
//	      slippage_bps: u16, platform_fee_bps: u8)
//	shared_accounts_route(id: u8, route_plan, in_amount, quoted_out_amount,
//	      slippage_bps: u16, platform_fee_bps: u8)
//	RoutePlanStep { swap: Swap, percent: u8, input_index: u8, output_index: u8 }
//	Swap = enum (u8 variant tag + variant payload)
//
// Account layouts (fixed prefix, then the route's remaining accounts):
//
//	route: [token_program, user_transfer_authority (signer), user_source_token_account,
//	        user_destination_token_account, destination_token_account (optional),
//	        destination_mint, platform_fee_account (optional), event_authority, program]
//	shared_accounts_route: [token_program, program_authority, user_transfer_authority (signer),
//	        source_token_account, program_source_token_account, program_destination_token_account,
//	        destination_token_account, source_mint, destination_mint, platform_fee_account (optional),
//	        token_2022_program (optional), event_authority, program]
//
// An absent Anchor optional account is encoded as the program id itself.
//
// Source: the jup-ag/jupiter-cpi IDL for program JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4
// as reproduced here from memory of that IDL; it is NOT verified against a
// live fetch in this build (jupiter.md records the program id only). The
// Swap variant table below therefore lists only variants whose payload size
// is stable and well known; any other variant rejects (decode ambiguity),
// and the arguments are additionally required to end exactly at the end of
// the data so a mis-sized table can never shift the parsed amounts. Exact-out
// and token-ledger variants (and the *_v2 instructions) are recognized so the
// rejection names them, but they are never accepted: the platform only quotes
// ExactIn.

// anchorDiscriminator computes sha256("global:<name>")[:8].
func anchorDiscriminator(name string) [8]byte {
	sum := sha256.Sum256([]byte("global:" + name))
	var out [8]byte
	copy(out[:], sum[:8])
	return out
}

var (
	discRoute                              = anchorDiscriminator("route")
	discSharedAccountsRoute                = anchorDiscriminator("shared_accounts_route")
	discRouteWithTokenLedger               = anchorDiscriminator("route_with_token_ledger")
	discSharedAccountsRouteWithTokenLedger = anchorDiscriminator("shared_accounts_route_with_token_ledger")
	discExactOutRoute                      = anchorDiscriminator("exact_out_route")
	discSharedAccountsExactOutRoute        = anchorDiscriminator("shared_accounts_exact_out_route")
	discRouteV2                            = anchorDiscriminator("route_v2")
	discSharedAccountsRouteV2              = anchorDiscriminator("shared_accounts_route_v2")
	discExactOutRouteV2                    = anchorDiscriminator("exact_out_route_v2")
	discSharedAccountsExactOutRouteV2      = anchorDiscriminator("shared_accounts_exact_out_route_v2")
)

// rejectedRouteNames are Jupiter instructions the inspector recognizes but
// never accepts.
var rejectedRouteNames = map[[8]byte]string{
	discRouteWithTokenLedger:               "route_with_token_ledger",
	discSharedAccountsRouteWithTokenLedger: "shared_accounts_route_with_token_ledger",
	discExactOutRoute:                      "exact_out_route",
	discSharedAccountsExactOutRoute:        "shared_accounts_exact_out_route",
	discRouteV2:                            "route_v2",
	discSharedAccountsRouteV2:              "shared_accounts_route_v2",
	discExactOutRouteV2:                    "exact_out_route_v2",
	discSharedAccountsExactOutRouteV2:      "shared_accounts_exact_out_route_v2",
}

// swapPayloadSize maps a Swap enum variant tag to its fixed payload size.
// Variants absent from this table reject. Variable-size variants are handled
// in readSwapVariant.
var swapPayloadSize = map[uint8]int{
	0: 0, 1: 0, 2: 0, 3: 0, 4: 0, 5: 0, 6: 0, 7: 0, // Saber, SaberAddDecimals(Deposit|Withdraw), TokenSwap, Sencha, Step, Cropper, Raydium
	8: 1,               // Crema { a_to_b: bool }
	9: 0, 10: 0, 11: 0, // Lifinity, Mercurial, Cykura
	12: 1,        // Serum { side }
	13: 0, 14: 0, // MarinadeDeposit, MarinadeUnstake
	15: 1, 16: 1, // Aldrin { side }, AldrinV2 { side }
	17: 1,        // Whirlpool { a_to_b }
	18: 1,        // Invariant { x_to_y }
	19: 0, 20: 0, // Meteora, GooseFX
	21: 1,               // DeltaFi { stable }
	22: 0,               // Balansol
	23: 1,               // MarcoPolo { x_to_y }
	24: 1,               // Dradex { side }
	25: 0,               // LifinityV2
	26: 0,               // RaydiumClmm
	27: 1,               // Openbook { side }
	28: 1,               // Phoenix { side }
	29: 16,              // Symmetry { from_token_id: u64, to_token_id: u64 }
	30: 0,               // TokenSwapV2
	31: 0,               // HeliumTreasuryManagementRedeemV0
	32: 0,               // StakeDexStakeWrappedSol
	33: 4,               // StakeDexSwapViaStake { bridge_stake_seed: u32 }
	34: 0,               // GooseFXV2
	35: 0, 36: 0, 37: 0, // Perps, PerpsAddLiquidity, PerpsRemoveLiquidity
	38: 0,                // MeteoraDlmm
	39: 1,                // OpenBookV2 { side }
	40: 0,                // RaydiumClmmV2
	41: 4,                // StakeDexPrefundWithdrawStakeAndDepositStake { bridge_stake_seed: u32 }
	42: 3,                // Clone { pool_index: u8, quantity_is_input: bool, quantity_is_collateral: bool }
	43: 10, 44: 5, 45: 5, // SanctumS {u8,u8,u32,u32}, SanctumSAddLiquidity {u8,u32}, SanctumSRemoveLiquidity {u8,u32}
	46: 0, // RaydiumCP
}

// swapVariantWhirlpoolV2 has a variable payload: { a_to_b: bool,
// remaining_accounts_info: Option<RemainingAccountsInfo { slices: Vec<{accounts_type: u8, length: u8}> }> }.
const swapVariantWhirlpoolV2 uint8 = 47

// maxRoutePlanSteps bounds the route plan; Jupiter routes are a handful of
// hops and a transaction cannot carry more than a few dozen accounts anyway.
const maxRoutePlanSteps = 16

// routeIx is a decoded, accepted Jupiter ExactIn route.
type routeIx struct {
	name           string // "route" | "shared_accounts_route"
	steps          int
	inAmount       uint64
	quotedOut      uint64
	slippageBPS    uint16
	platformFeeBPS uint8

	tokenProgram    solana.PublicKey
	authority       account
	userSource      solana.PublicKey
	userDestination solana.PublicKey
	destinationMint solana.PublicKey
	sourceMint      solana.PublicKey // shared_accounts_route only
	hasSourceMint   bool

	destinationTokenAccount    solana.PublicKey
	hasDestinationTokenAccount bool
	platformFeeAccount         solana.PublicKey
	hasPlatformFeeAccount      bool
	selfProgram                solana.PublicKey
	remaining                  []account
}

// errRouteRejected marks a recognized-but-unsupported Jupiter instruction.
type errRouteRejected struct{ name string }

func (e errRouteRejected) Error() string {
	return "jupiter instruction " + e.name + " is not permitted"
}

// decodeJupiter decodes a Jupiter v6 route instruction. program is the
// instruction's program id (the placeholder for absent optional accounts).
func decodeJupiter(program solana.PublicKey, ix resolvedIx) (routeIx, error) {
	if len(ix.data) < 8 {
		return routeIx{}, fmt.Errorf("%w: Jupiter data shorter than the 8-byte discriminator", errLayout)
	}
	var disc [8]byte
	copy(disc[:], ix.data[:8])
	if name, ok := rejectedRouteNames[disc]; ok {
		return routeIx{}, errRouteRejected{name: name}
	}
	var (
		out    routeIx
		fixed  int
		shared bool
	)
	switch disc {
	case discRoute:
		out.name, fixed = "route", 9
	case discSharedAccountsRoute:
		out.name, fixed, shared = "shared_accounts_route", 13, true
	default:
		return routeIx{}, fmt.Errorf("%w: unknown Jupiter instruction discriminator %x", errLayout, disc)
	}
	if len(ix.accounts) < fixed {
		return routeIx{}, fmt.Errorf("%w: Jupiter %s needs %d fixed accounts, got %d", errLayout, out.name, fixed, len(ix.accounts))
	}
	pos := 8
	if shared {
		pos++ // id: u8
		if len(ix.data) < pos {
			return routeIx{}, fmt.Errorf("%w: Jupiter %s missing id byte", errLayout, out.name)
		}
	}
	steps, next, err := readRoutePlan(ix.data, pos)
	if err != nil {
		return routeIx{}, err
	}
	out.steps = steps
	pos = next
	// Tail: in_amount u64, quoted_out_amount u64, slippage_bps u16, platform_fee_bps u8.
	const tail = 8 + 8 + 2 + 1
	if len(ix.data)-pos != tail {
		return routeIx{}, fmt.Errorf("%w: Jupiter %s arguments end at %d of %d bytes (route plan size ambiguity)", errLayout, out.name, pos+tail, len(ix.data))
	}
	out.inAmount = binary.LittleEndian.Uint64(ix.data[pos : pos+8])
	out.quotedOut = binary.LittleEndian.Uint64(ix.data[pos+8 : pos+16])
	out.slippageBPS = binary.LittleEndian.Uint16(ix.data[pos+16 : pos+18])
	out.platformFeeBPS = ix.data[pos+18]

	a := ix.accounts
	if shared {
		out.tokenProgram = a[0].key
		out.authority = a[2]
		out.userSource = a[3].key
		out.userDestination = a[6].key
		out.sourceMint, out.hasSourceMint = a[7].key, true
		out.destinationMint = a[8].key
		if !a[9].key.Equals(program) {
			out.platformFeeAccount, out.hasPlatformFeeAccount = a[9].key, true
		}
		out.selfProgram = a[12].key
	} else {
		out.tokenProgram = a[0].key
		out.authority = a[1]
		out.userSource = a[2].key
		out.userDestination = a[3].key
		if !a[4].key.Equals(program) {
			out.destinationTokenAccount, out.hasDestinationTokenAccount = a[4].key, true
		}
		out.destinationMint = a[5].key
		if !a[6].key.Equals(program) {
			out.platformFeeAccount, out.hasPlatformFeeAccount = a[6].key, true
		}
		out.selfProgram = a[8].key
	}
	out.remaining = a[fixed:]
	return out, nil
}

// readRoutePlan parses Vec<RoutePlanStep> starting at pos and returns the
// number of steps and the position after the vector.
func readRoutePlan(data []byte, pos int) (int, int, error) {
	if len(data) < pos+4 {
		return 0, 0, fmt.Errorf("%w: Jupiter route plan length missing", errLayout)
	}
	n := binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4
	if n == 0 || n > maxRoutePlanSteps {
		return 0, 0, fmt.Errorf("%w: Jupiter route plan has %d steps", errLayout, n)
	}
	for i := 0; i < int(n); i++ {
		next, err := readSwapVariant(data, pos)
		if err != nil {
			return 0, 0, fmt.Errorf("%w: step %d: %v", errLayout, i, err)
		}
		pos = next
		// percent u8, input_index u8, output_index u8
		if len(data) < pos+3 {
			return 0, 0, fmt.Errorf("%w: Jupiter route plan step %d truncated", errLayout, i)
		}
		if data[pos] == 0 || data[pos] > 100 {
			return 0, 0, fmt.Errorf("%w: Jupiter route plan step %d percent %d", errLayout, i, data[pos])
		}
		pos += 3
	}
	return int(n), pos, nil
}

// readSwapVariant consumes one Swap enum value.
func readSwapVariant(data []byte, pos int) (int, error) {
	if len(data) < pos+1 {
		return 0, fmt.Errorf("swap variant tag missing")
	}
	tag := data[pos]
	pos++
	if tag == swapVariantWhirlpoolV2 {
		// a_to_b: bool, remaining_accounts_info: Option<...>
		if len(data) < pos+2 {
			return 0, fmt.Errorf("WhirlpoolSwapV2 payload truncated")
		}
		pos++ // a_to_b
		opt := data[pos]
		pos++
		switch opt {
		case 0:
			return pos, nil
		case 1:
			if len(data) < pos+4 {
				return 0, fmt.Errorf("WhirlpoolSwapV2 remaining accounts length missing")
			}
			slices := binary.LittleEndian.Uint32(data[pos : pos+4])
			pos += 4
			if slices > 16 {
				return 0, fmt.Errorf("WhirlpoolSwapV2 has %d remaining account slices", slices)
			}
			need := int(slices) * 2
			if len(data) < pos+need {
				return 0, fmt.Errorf("WhirlpoolSwapV2 remaining accounts truncated")
			}
			return pos + need, nil
		default:
			return 0, fmt.Errorf("WhirlpoolSwapV2 option tag %d", opt)
		}
	}
	size, ok := swapPayloadSize[tag]
	if !ok {
		return 0, fmt.Errorf("swap variant %d is not in the verified variant table", tag)
	}
	if len(data) < pos+size {
		return 0, fmt.Errorf("swap variant %d payload truncated", tag)
	}
	return pos + size, nil
}
