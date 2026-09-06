package inspect

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

// Minimal instruction decoders. Each layout is written out against the
// program's Rust source rather than trusting a generated binding:
//
//   - System program: solana-program/src/system_instruction.rs — bincode with
//     a little-endian u32 discriminator; Pubkey = 32 raw bytes; String =
//     u64 length + bytes.
//   - SPL Token / Token-2022: spl-token/src/instruction.rs — a u8
//     discriminator; u64 amounts little-endian; COption<Pubkey> = 1 byte tag
//     (0 none / 1 some) followed by 32 bytes when some. Token-2022 shares the
//     first 25 discriminators and adds extension instructions from 25 on.
//   - Compute Budget: solana-sdk compute_budget.rs — borsh with a u8
//     discriminator.
//   - Associated Token Account: spl-associated-token-account instruction.rs —
//     empty data or a single u8 (0 Create, 1 CreateIdempotent, 2
//     RecoverNested).
//
// Tests cross-check the System, Compute Budget and ATA layouts against the
// encoders of the solana-go program packages; the token layout is checked
// against hand-built vectors from the SPL specification (the solana-go token
// package does not build with the pinned go.sum and is not used).
//
// Every decoder rejects trailing bytes and short data: a length that does not
// match the layout is an ambiguity, and ambiguity rejects.

var errLayout = errors.New("instruction layout")

// --- System program ---------------------------------------------------------

type systemIx struct {
	tag      uint32
	name     string
	lamports uint64 // Transfer, WithdrawNonceAccount, CreateAccount
}

var systemNames = map[uint32]string{
	0: "CreateAccount", 1: "Assign", 2: "Transfer", 3: "CreateAccountWithSeed", 4: "AdvanceNonceAccount",
	5: "WithdrawNonceAccount", 6: "InitializeNonceAccount", 7: "AuthorizeNonceAccount", 8: "Allocate",
	9: "AllocateWithSeed", 10: "AssignWithSeed", 11: "TransferWithSeed", 12: "UpgradeNonceAccount",
}

// systemFixedLen is the exact data length of fixed-size System instructions.
var systemFixedLen = map[uint32]int{
	0:  4 + 8 + 8 + 32, // lamports, space, owner
	1:  4 + 32,         // owner
	2:  4 + 8,          // lamports
	4:  4,
	5:  4 + 8, // lamports
	6:  4 + 32,
	7:  4 + 32,
	8:  4 + 8, // space
	12: 4,
}

func decodeSystem(data []byte) (systemIx, error) {
	if len(data) < 4 {
		return systemIx{}, fmt.Errorf("%w: System data shorter than the u32 discriminator", errLayout)
	}
	tag := binary.LittleEndian.Uint32(data[:4])
	name, ok := systemNames[tag]
	if !ok {
		return systemIx{}, fmt.Errorf("%w: unknown System instruction %d", errLayout, tag)
	}
	if want, fixed := systemFixedLen[tag]; fixed && len(data) != want {
		return systemIx{}, fmt.Errorf("%w: System.%s expects %d bytes, got %d", errLayout, name, want, len(data))
	}
	ix := systemIx{tag: tag, name: name}
	switch tag {
	case 0, 2, 5:
		ix.lamports = binary.LittleEndian.Uint64(data[4:12])
	}
	return ix, nil
}

// --- SPL Token / Token-2022 --------------------------------------------------

type tokenIx struct {
	tag      uint8
	name     string
	amount   uint64 // Transfer, Approve, MintTo, Burn, *Checked
	decimals uint8  // *Checked
}

var tokenNames = map[uint8]string{
	0: "InitializeMint", 1: "InitializeAccount", 2: "InitializeMultisig", 3: "Transfer", 4: "Approve", 5: "Revoke",
	6: "SetAuthority", 7: "MintTo", 8: "Burn", 9: "CloseAccount", 10: "FreezeAccount", 11: "ThawAccount",
	12: "TransferChecked", 13: "ApproveChecked", 14: "MintToChecked", 15: "BurnChecked", 16: "InitializeAccount2",
	17: "SyncNative", 18: "InitializeAccount3", 19: "InitializeMultisig2", 20: "InitializeMint2",
	21: "GetAccountDataSize", 22: "InitializeImmutableOwner", 23: "AmountToUiAmount", 24: "UiAmountToAmount",
	// Token-2022 only.
	25: "InitializeMintCloseAuthority", 26: "TransferFeeExtension", 27: "ConfidentialTransferExtension",
	28: "DefaultAccountStateExtension", 29: "Reallocate", 30: "MemoTransferExtension", 31: "CreateNativeMint",
	32: "InitializeNonTransferableMint", 33: "InterestBearingMintExtension", 34: "CpiGuardExtension",
	35: "InitializePermanentDelegate", 36: "TransferHookExtension", 37: "ConfidentialTransferFeeExtension",
	38: "WithdrawExcessLamports", 39: "MetadataPointerExtension", 40: "GroupPointerExtension",
	41: "GroupMemberPointerExtension", 42: "ConfidentialMintBurnExtension", 43: "ScaledUiAmountExtension",
	44: "PausableExtension",
}

// Token instruction tags the inspector reasons about.
const (
	tokenTransfer        uint8 = 3
	tokenApprove         uint8 = 4
	tokenRevoke          uint8 = 5
	tokenSetAuthority    uint8 = 6
	tokenCloseAccount    uint8 = 9
	tokenFreezeAccount   uint8 = 10
	tokenThawAccount     uint8 = 11
	tokenTransferChecked uint8 = 12
	tokenApproveChecked  uint8 = 13
	tokenSyncNative      uint8 = 17
)

// tokenFixedLen is the exact data length of fixed-size token instructions
// the inspector may accept. Anything else is decoded by name only and then
// rejected by the rules, so its length is not validated here beyond the tag.
var tokenFixedLen = map[uint8]int{
	tokenTransfer:        1 + 8,
	tokenApprove:         1 + 8,
	tokenRevoke:          1,
	tokenCloseAccount:    1,
	tokenFreezeAccount:   1,
	tokenThawAccount:     1,
	tokenTransferChecked: 1 + 8 + 1,
	tokenApproveChecked:  1 + 8 + 1,
	tokenSyncNative:      1,
	7:                    1 + 8, // MintTo
	8:                    1 + 8, // Burn
	14:                   1 + 8 + 1,
	15:                   1 + 8 + 1,
	1:                    1,
	16:                   1 + 32,
	18:                   1 + 32,
	22:                   1,
}

func decodeToken(data []byte) (tokenIx, error) {
	if len(data) < 1 {
		return tokenIx{}, fmt.Errorf("%w: empty token instruction data", errLayout)
	}
	tag := data[0]
	name, ok := tokenNames[tag]
	if !ok {
		return tokenIx{}, fmt.Errorf("%w: unknown token instruction %d", errLayout, tag)
	}
	if want, fixed := tokenFixedLen[tag]; fixed && len(data) != want {
		return tokenIx{}, fmt.Errorf("%w: Token.%s expects %d bytes, got %d", errLayout, name, want, len(data))
	}
	if tag == tokenSetAuthority {
		// authority_type u8, COption<Pubkey>: 1 + 1 + 1 (+32).
		switch len(data) {
		case 3:
			if data[2] != 0 {
				return tokenIx{}, fmt.Errorf("%w: Token.SetAuthority COption tag %d without a key", errLayout, data[2])
			}
		case 35:
			if data[2] != 1 {
				return tokenIx{}, fmt.Errorf("%w: Token.SetAuthority COption tag %d with a key", errLayout, data[2])
			}
		default:
			return tokenIx{}, fmt.Errorf("%w: Token.SetAuthority expects 3 or 35 bytes, got %d", errLayout, len(data))
		}
	}
	ix := tokenIx{tag: tag, name: name}
	switch tag {
	case tokenTransfer, tokenApprove, 7, 8:
		ix.amount = binary.LittleEndian.Uint64(data[1:9])
	case tokenTransferChecked, tokenApproveChecked, 14, 15:
		ix.amount = binary.LittleEndian.Uint64(data[1:9])
		ix.decimals = data[9]
	}
	return ix, nil
}

// --- Compute Budget ----------------------------------------------------------

type computeBudgetIx struct {
	tag   uint8
	name  string
	units uint32 // SetComputeUnitLimit
	price uint64 // SetComputeUnitPrice (micro-lamports per CU)
}

const (
	cbRequestUnitsDeprecated       uint8 = 0
	cbRequestHeapFrame             uint8 = 1
	cbSetComputeUnitLimit          uint8 = 2
	cbSetComputeUnitPrice          uint8 = 3
	cbSetLoadedAccountsDataSizeLim uint8 = 4
)

var computeBudgetNames = map[uint8]string{
	cbRequestUnitsDeprecated: "RequestUnitsDeprecated", cbRequestHeapFrame: "RequestHeapFrame",
	cbSetComputeUnitLimit: "SetComputeUnitLimit", cbSetComputeUnitPrice: "SetComputeUnitPrice",
	cbSetLoadedAccountsDataSizeLim: "SetLoadedAccountsDataSizeLimit",
}

var computeBudgetLen = map[uint8]int{
	cbRequestUnitsDeprecated: 1 + 4 + 4, cbRequestHeapFrame: 1 + 4, cbSetComputeUnitLimit: 1 + 4,
	cbSetComputeUnitPrice: 1 + 8, cbSetLoadedAccountsDataSizeLim: 1 + 4,
}

func decodeComputeBudget(data []byte) (computeBudgetIx, error) {
	if len(data) < 1 {
		return computeBudgetIx{}, fmt.Errorf("%w: empty ComputeBudget data", errLayout)
	}
	tag := data[0]
	name, ok := computeBudgetNames[tag]
	if !ok {
		return computeBudgetIx{}, fmt.Errorf("%w: unknown ComputeBudget instruction %d", errLayout, tag)
	}
	if len(data) != computeBudgetLen[tag] {
		return computeBudgetIx{}, fmt.Errorf("%w: ComputeBudget.%s expects %d bytes, got %d", errLayout, name, computeBudgetLen[tag], len(data))
	}
	ix := computeBudgetIx{tag: tag, name: name}
	switch tag {
	case cbSetComputeUnitLimit:
		ix.units = binary.LittleEndian.Uint32(data[1:5])
	case cbSetComputeUnitPrice:
		ix.price = binary.LittleEndian.Uint64(data[1:9])
	}
	return ix, nil
}

// --- Associated Token Account ------------------------------------------------

type ataIx struct {
	tag  uint8
	name string
}

const (
	ataCreate           uint8 = 0
	ataCreateIdempotent uint8 = 1
	ataRecoverNested    uint8 = 2
)

func decodeATA(data []byte) (ataIx, error) {
	switch {
	case len(data) == 0:
		return ataIx{tag: ataCreate, name: "Create"}, nil
	case len(data) == 1 && data[0] == ataCreate:
		return ataIx{tag: ataCreate, name: "Create"}, nil
	case len(data) == 1 && data[0] == ataCreateIdempotent:
		return ataIx{tag: ataCreateIdempotent, name: "CreateIdempotent"}, nil
	case len(data) == 1 && data[0] == ataRecoverNested:
		return ataIx{tag: ataRecoverNested, name: "RecoverNested"}, nil
	}
	return ataIx{}, fmt.Errorf("%w: unknown AssociatedToken instruction (%d bytes)", errLayout, len(data))
}

// sysvarRent is the rent sysvar older ATA clients pass as a seventh account.
var sysvarRent = solana.MustPublicKeyFromBase58("SysvarRent111111111111111111111111111111111")
