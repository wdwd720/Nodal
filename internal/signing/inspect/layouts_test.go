package inspect

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hand-written decoders are cross-checked against the encoders of the
// solana-go program packages (System, Compute Budget, ATA) and against
// vectors built from the SPL Token specification.

func TestLayout_SystemMatchesSolanaGo(t *testing.T) {
	t.Parallel()
	from, to := solana.MustPublicKeyFromBase58(JupiterV6Program), solana.MustPublicKeyFromBase58(TokenProgram)
	data, err := system.NewTransferInstruction(123_456, from, to).Build().Data()
	require.NoError(t, err)
	ix, err := decodeSystem(data)
	require.NoError(t, err)
	assert.Equal(t, "Transfer", ix.name)
	assert.Equal(t, uint64(123_456), ix.lamports)

	data, err = system.NewAssignInstruction(to, from).Build().Data()
	require.NoError(t, err)
	ix, err = decodeSystem(data)
	require.NoError(t, err)
	assert.Equal(t, "Assign", ix.name)

	data, err = system.NewCreateAccountInstruction(1, 2, to, from, from).Build().Data()
	require.NoError(t, err)
	ix, err = decodeSystem(data)
	require.NoError(t, err)
	assert.Equal(t, "CreateAccount", ix.name)
	assert.Equal(t, uint64(1), ix.lamports)

	data, err = system.NewAllocateInstruction(64, from).Build().Data()
	require.NoError(t, err)
	ix, err = decodeSystem(data)
	require.NoError(t, err)
	assert.Equal(t, "Allocate", ix.name)

	_, err = decodeSystem([]byte{2, 0, 0, 0, 1})
	assert.Error(t, err, "short transfer")
	_, err = decodeSystem(append(data, 0))
	assert.Error(t, err, "trailing byte")
	_, err = decodeSystem([]byte{99, 0, 0, 0})
	assert.Error(t, err, "unknown tag")
}

func TestLayout_ComputeBudgetMatchesSolanaGo(t *testing.T) {
	t.Parallel()
	data, err := computebudget.NewSetComputeUnitLimitInstruction(400_000).Build().Data()
	require.NoError(t, err)
	ix, err := decodeComputeBudget(data)
	require.NoError(t, err)
	assert.Equal(t, cbSetComputeUnitLimit, ix.tag)
	assert.Equal(t, uint32(400_000), ix.units)

	data, err = computebudget.NewSetComputeUnitPriceInstruction(1_500).Build().Data()
	require.NoError(t, err)
	ix, err = decodeComputeBudget(data)
	require.NoError(t, err)
	assert.Equal(t, cbSetComputeUnitPrice, ix.tag)
	assert.Equal(t, uint64(1_500), ix.price)

	_, err = decodeComputeBudget([]byte{2, 1})
	assert.Error(t, err)
	_, err = decodeComputeBudget(append(data, 0))
	assert.Error(t, err)
	_, err = decodeComputeBudget(nil)
	assert.Error(t, err)
}

func TestLayout_ATAMatchesSolanaGo(t *testing.T) {
	t.Parallel()
	wallet := solana.MustPublicKeyFromBase58(JupiterV6Program)
	mint := solana.MustPublicKeyFromBase58(WrappedSOLMint)
	built := associatedtokenaccount.NewCreateInstruction(wallet, wallet, mint).Build()
	data, err := built.Data()
	require.NoError(t, err)
	ix, err := decodeATA(data)
	require.NoError(t, err)
	assert.Equal(t, ataCreate, ix.tag)
	// solana-go's Create passes the classic 7-account layout (with the rent sysvar).
	accts := built.Accounts()
	require.GreaterOrEqual(t, len(accts), 6)
	assert.Equal(t, wallet, accts[0].PublicKey)
	assert.Equal(t, mint, accts[3].PublicKey)
	if len(accts) == 7 {
		assert.Equal(t, sysvarRent, accts[6].PublicKey)
	}

	ix, err = decodeATA([]byte{1})
	require.NoError(t, err)
	assert.Equal(t, ataCreateIdempotent, ix.tag)
	_, err = decodeATA([]byte{7})
	assert.Error(t, err)
	_, err = decodeATA([]byte{1, 1})
	assert.Error(t, err)
}

func TestLayout_TokenSPLVectors(t *testing.T) {
	t.Parallel()
	amount := func(tag uint8, v uint64) []byte {
		b := make([]byte, 9)
		b[0] = tag
		binary.LittleEndian.PutUint64(b[1:], v)
		return b
	}
	ix, err := decodeToken(amount(3, 77))
	require.NoError(t, err)
	assert.Equal(t, "Transfer", ix.name)
	assert.Equal(t, uint64(77), ix.amount)

	checked := append(amount(12, 5), 6)
	ix, err = decodeToken(checked)
	require.NoError(t, err)
	assert.Equal(t, "TransferChecked", ix.name)
	assert.Equal(t, uint8(6), ix.decimals)

	ix, err = decodeToken([]byte{9})
	require.NoError(t, err)
	assert.Equal(t, tokenCloseAccount, ix.tag)

	// SetAuthority: type + COption none / some.
	_, err = decodeToken([]byte{6, 2, 0})
	require.NoError(t, err)
	some := append([]byte{6, 2, 1}, make([]byte, 32)...)
	_, err = decodeToken(some)
	require.NoError(t, err)
	_, err = decodeToken([]byte{6, 2, 1})
	assert.Error(t, err, "COption some without key")
	_, err = decodeToken(append([]byte{6, 2, 0}, make([]byte, 32)...))
	assert.Error(t, err, "COption none with key")

	// Token-2022 extension tags decode by name; rules reject them later.
	ix, err = decodeToken([]byte{36, 0})
	require.NoError(t, err)
	assert.Equal(t, "TransferHookExtension", ix.name)

	_, err = decodeToken(nil)
	assert.Error(t, err)
	_, err = decodeToken([]byte{3, 1})
	assert.Error(t, err, "short amount")
	_, err = decodeToken(append(amount(3, 1), 0))
	assert.Error(t, err, "trailing byte")
	_, err = decodeToken([]byte{200})
	assert.Error(t, err, "unknown tag")
}

func TestLayout_JupiterDiscriminators(t *testing.T) {
	t.Parallel()
	want := func(name string) [8]byte {
		sum := sha256.Sum256([]byte("global:" + name))
		var d [8]byte
		copy(d[:], sum[:8])
		return d
	}
	assert.Equal(t, want("route"), discRoute)
	assert.Equal(t, want("shared_accounts_route"), discSharedAccountsRoute)
	// Known published values of the two accepted discriminators.
	assert.Equal(t, [8]byte{0xe5, 0x17, 0xcb, 0x97, 0x7a, 0xe3, 0xad, 0x2a}, discRoute)
	assert.Equal(t, [8]byte{0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81}, discSharedAccountsRoute)
	assert.Len(t, rejectedRouteNames, 8)
}

func TestMinimumOut(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "996", minimumOut(1000, 33).String(), "floor of the smaller rounding")
	assert.Equal(t, "1000", minimumOut(1000, 0).String())
	assert.Equal(t, "0", minimumOut(1000, 10_000).String())
	assert.Equal(t, "0", minimumOut(1000, 65_535).String())
}
