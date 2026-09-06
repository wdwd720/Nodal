package signingtest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/signing/inspect"
)

// quantity converts a fixture amount without a lossy integer conversion.
func quantity(v uint64) money.Quantity { return money.QuantityFromBigInt(new(big.Int).SetUint64(v)) }

// lamports converts a fixture amount to a signed lamport delta; fixture
// amounts are far below the int64 range.
func lamports(v uint64) int64 {
	if v > math.MaxInt64 {
		panic("fixture amount exceeds int64")
	}
	return int64(v)
}

// KeyFromSeed derives a deterministic ed25519 key pair from a seed string.
func KeyFromSeed(seed string) solana.PrivateKey {
	h := sha256.Sum256([]byte("signingtest:" + seed))
	return solana.PrivateKey(ed25519.NewKeyFromSeed(h[:]))
}

// PubkeyFromSeed derives a deterministic public key (no usable private key
// is needed for mints, pools and foreign accounts).
func PubkeyFromSeed(seed string) solana.PublicKey { return KeyFromSeed(seed).PublicKey() }

// Well-known program keys re-exported for fixtures.
var (
	SystemProgram          = solana.MustPublicKeyFromBase58(inspect.SystemProgram)
	ComputeBudgetProgram   = solana.MustPublicKeyFromBase58(inspect.ComputeBudgetProgram)
	TokenProgram           = solana.MustPublicKeyFromBase58(inspect.TokenProgram)
	Token2022Program       = solana.MustPublicKeyFromBase58(inspect.Token2022Program)
	AssociatedTokenProgram = solana.MustPublicKeyFromBase58(inspect.AssociatedTokenProgram)
	JupiterV6Program       = solana.MustPublicKeyFromBase58(inspect.JupiterV6Program)
	WrappedSOLMint         = solana.MustPublicKeyFromBase58(inspect.WrappedSOLMint)
)

// AnchorDiscriminator computes sha256("global:<name>")[:8].
func AnchorDiscriminator(name string) []byte {
	sum := sha256.Sum256([]byte("global:" + name))
	return sum[:8]
}

// --- instruction data encoders (SPL / Anchor layouts) ------------------------

// SystemTransferData encodes System.Transfer { lamports }.
func SystemTransferData(lamports uint64) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b[0:4], 2)
	binary.LittleEndian.PutUint64(b[4:12], lamports)
	return b
}

// SystemAssignData encodes System.Assign { owner }.
func SystemAssignData(owner solana.PublicKey) []byte {
	b := make([]byte, 36)
	binary.LittleEndian.PutUint32(b[0:4], 1)
	copy(b[4:], owner[:])
	return b
}

// TokenTransferData encodes Token.Transfer { amount }.
func TokenTransferData(amount uint64) []byte {
	b := make([]byte, 9)
	b[0] = 3
	binary.LittleEndian.PutUint64(b[1:9], amount)
	return b
}

// TokenTransferCheckedData encodes Token.TransferChecked { amount, decimals }.
func TokenTransferCheckedData(amount uint64, decimals uint8) []byte {
	b := make([]byte, 10)
	b[0] = 12
	binary.LittleEndian.PutUint64(b[1:9], amount)
	b[9] = decimals
	return b
}

// TokenApproveData encodes Token.Approve { amount }.
func TokenApproveData(amount uint64) []byte {
	b := make([]byte, 9)
	b[0] = 4
	binary.LittleEndian.PutUint64(b[1:9], amount)
	return b
}

// TokenSetAuthorityData encodes Token.SetAuthority { authority_type,
// COption<Pubkey>(Some(newAuthority)) }.
func TokenSetAuthorityData(authorityType uint8, newAuthority solana.PublicKey) []byte {
	b := make([]byte, 35)
	b[0], b[1], b[2] = 6, authorityType, 1
	copy(b[3:], newAuthority[:])
	return b
}

// TokenSingleByteData encodes a data-less token instruction (CloseAccount 9,
// FreezeAccount 10, ThawAccount 11, SyncNative 17, Revoke 5, ...).
func TokenSingleByteData(tag uint8) []byte { return []byte{tag} }

// ComputeUnitLimitData encodes ComputeBudget.SetComputeUnitLimit { units }.
func ComputeUnitLimitData(units uint32) []byte {
	b := make([]byte, 5)
	b[0] = 2
	binary.LittleEndian.PutUint32(b[1:5], units)
	return b
}

// ComputeUnitPriceData encodes ComputeBudget.SetComputeUnitPrice { micro_lamports }.
func ComputeUnitPriceData(microLamports uint64) []byte {
	b := make([]byte, 9)
	b[0] = 3
	binary.LittleEndian.PutUint64(b[1:9], microLamports)
	return b
}

// ATACreateIdempotentData encodes AssociatedToken.CreateIdempotent.
func ATACreateIdempotentData() []byte { return []byte{1} }

// RouteStep is one Jupiter RoutePlanStep with a fixed-size swap variant.
type RouteStep struct {
	SwapTag     uint8
	SwapPayload []byte
	Percent     uint8
	InputIndex  uint8
	OutputIndex uint8
}

// RouteArgs are the arguments of Jupiter route / shared_accounts_route.
type RouteArgs struct {
	Name           string // "route" (default) or "shared_accounts_route"
	SharedID       uint8
	Steps          []RouteStep
	InAmount       uint64
	QuotedOut      uint64
	SlippageBPS    uint16
	PlatformFeeBPS uint8
	Trailing       []byte // appended verbatim (malformed variants)
}

// RouteData encodes a Jupiter route instruction's data.
func RouteData(a RouteArgs) []byte {
	name := a.Name
	if name == "" {
		name = "route"
	}
	out := append([]byte(nil), AnchorDiscriminator(name)...)
	if name == "shared_accounts_route" {
		out = append(out, a.SharedID)
	}
	steps := a.Steps
	if steps == nil {
		steps = []RouteStep{{SwapTag: 7, Percent: 100, InputIndex: 0, OutputIndex: 1}} // Raydium
	}
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(steps))) // #nosec G115 -- len of a fixture slice built in-process, always far below MaxUint32
	out = append(out, n[:]...)
	for _, s := range steps {
		out = append(out, s.SwapTag)
		out = append(out, s.SwapPayload...)
		out = append(out, s.Percent, s.InputIndex, s.OutputIndex)
	}
	var tail [19]byte
	binary.LittleEndian.PutUint64(tail[0:8], a.InAmount)
	binary.LittleEndian.PutUint64(tail[8:16], a.QuotedOut)
	binary.LittleEndian.PutUint16(tail[16:18], a.SlippageBPS)
	tail[18] = a.PlatformFeeBPS
	out = append(out, tail[:]...)
	return append(out, a.Trailing...)
}

// --- the golden swap ---------------------------------------------------------

// Swap is a deterministic Jupiter-shaped swap fixture: compute budget,
// output ATA creation (idempotent), one route instruction under RouteProgram
// with four pool accounts (loaded from a lookup table in the v0 build), and
// for a native input the wrap/sync/close legs.
type Swap struct {
	WalletKey solana.PrivateKey
	Wallet    solana.PublicKey

	InputMint, OutputMint       solana.PublicKey
	InputProgram, OutputProgram solana.PublicKey
	InputATA, OutputATA         solana.PublicKey
	WSOLATA                     solana.PublicKey
	NativeInput                 bool

	RouteProgram   solana.PublicKey
	DEXProgram     solana.PublicKey
	EventAuthority solana.PublicKey
	ProgramAuth    solana.PublicKey
	PoolAccounts   []solana.PublicKey
	LookupTable    solana.PublicKey
	LookupEntries  []solana.PublicKey
	Foreign        solana.PublicKey

	Blockhash            solana.Hash
	LastValidBlockHeight uint64
	CurrentBlockHeight   uint64
	BlockHeightMargin    uint64

	InAmount       uint64
	QuotedOut      uint64
	SlippageBPS    uint16
	MaxSlippageBPS money.BPS
	MaxInputDebit  uint64
	MinOutput      uint64
	CULimit        uint32
	CUPrice        uint64
	MaxPriorityFee uint64
	MaxCU          uint32

	PlanHash []byte
	QuoteID  string
}

// NewSwap returns the golden SPL-token → SPL-token swap fixture.
func NewSwap() *Swap {
	s := &Swap{
		WalletKey:            KeyFromSeed("wallet"),
		InputMint:            PubkeyFromSeed("mint-usdc"),
		OutputMint:           PubkeyFromSeed("mint-bonk"),
		InputProgram:         TokenProgram,
		OutputProgram:        TokenProgram,
		RouteProgram:         JupiterV6Program,
		DEXProgram:           PubkeyFromSeed("dex-raydium"),
		EventAuthority:       PubkeyFromSeed("jupiter-event-authority"),
		ProgramAuth:          PubkeyFromSeed("jupiter-program-authority"),
		LookupTable:          PubkeyFromSeed("lookup-table"),
		Foreign:              PubkeyFromSeed("foreign-account"),
		LastValidBlockHeight: 250_000_150,
		CurrentBlockHeight:   250_000_000,
		BlockHeightMargin:    30,
		InAmount:             25_000_000, // 25 USDC
		QuotedOut:            1_000_000_000_000,
		SlippageBPS:          50,
		MaxSlippageBPS:       100,
		MaxInputDebit:        25_000_000,
		MinOutput:            990_000_000_000,
		CULimit:              400_000,
		CUPrice:              1_000, // 0.4 SOL-lamport... 400_000 × 1000 / 1e6 = 400 lamports
		MaxPriorityFee:       10_000,
		MaxCU:                1_400_000,
		QuoteID:              "0192f7a0-1b2c-7d3e-8f4a-5b6c7d8e9f01",
	}
	bh := sha256.Sum256([]byte("signingtest:blockhash"))
	s.Blockhash = solana.HashFromBytes(bh[:])
	ph := sha256.Sum256([]byte("signingtest:plan-hash"))
	s.PlanHash = ph[:]
	s.Wallet = s.WalletKey.PublicKey()
	for i := 0; i < 4; i++ {
		s.PoolAccounts = append(s.PoolAccounts, PubkeyFromSeed(fmt.Sprintf("pool-%d", i)))
	}
	s.LookupEntries = append([]solana.PublicKey{PubkeyFromSeed("lookup-filler-0")}, s.PoolAccounts...)
	s.LookupEntries = append(s.LookupEntries, PubkeyFromSeed("lookup-filler-1"))
	s.derive()
	return s
}

// NewNativeInputSwap returns the SOL → SPL-token variant with wrap, sync and
// close legs.
func NewNativeInputSwap() *Swap {
	s := NewSwap()
	s.NativeInput = true
	s.InputMint = WrappedSOLMint
	s.InputProgram = TokenProgram
	s.InAmount = 1_000_000_000 // 1 SOL
	s.MaxInputDebit = 1_000_000_000
	s.derive()
	return s
}

func (s *Swap) derive() {
	s.Wallet = s.WalletKey.PublicKey()
	s.InputATA, _, _ = solana.FindAssociatedTokenAddressWithProgram(s.Wallet, s.InputMint, s.InputProgram)
	s.OutputATA, _, _ = solana.FindAssociatedTokenAddressWithProgram(s.Wallet, s.OutputMint, s.OutputProgram)
	s.WSOLATA, _, _ = solana.FindAssociatedTokenAddressWithProgram(s.Wallet, WrappedSOLMint, TokenProgram)
}

// Derive recomputes the ATAs after a caller changed mints or programs.
func (s *Swap) Derive() { s.derive() }

// ComputeBudgetIxs returns SetComputeUnitLimit + SetComputeUnitPrice.
func (s *Swap) ComputeBudgetIxs() []solana.Instruction {
	return []solana.Instruction{
		solana.NewInstruction(ComputeBudgetProgram, nil, ComputeUnitLimitData(s.CULimit)),
		solana.NewInstruction(ComputeBudgetProgram, nil, ComputeUnitPriceData(s.CUPrice)),
	}
}

// ATACreateIx returns AssociatedToken.CreateIdempotent for (owner, mint,
// program) paid by the wallet.
func (s *Swap) ATACreateIx(owner, mint, program solana.PublicKey) solana.Instruction {
	ata, _, _ := solana.FindAssociatedTokenAddressWithProgram(owner, mint, program)
	return solana.NewInstruction(AssociatedTokenProgram, solana.AccountMetaSlice{
		solana.NewAccountMeta(s.Wallet, true, true),
		solana.NewAccountMeta(ata, true, false),
		solana.NewAccountMeta(owner, false, false),
		solana.NewAccountMeta(mint, false, false),
		solana.NewAccountMeta(SystemProgram, false, false),
		solana.NewAccountMeta(program, false, false),
	}, ATACreateIdempotentData())
}

// RouteAccounts is the account prefix of a `route` instruction; override
// fields before calling RouteIxWith.
type RouteAccounts struct {
	TokenProgram            solana.PublicKey
	Authority               solana.PublicKey
	UserSource              solana.PublicKey
	UserDestination         solana.PublicKey
	DestinationTokenAccount solana.PublicKey // program id placeholder when absent
	DestinationMint         solana.PublicKey
	PlatformFeeAccount      solana.PublicKey // program id placeholder when absent
	EventAuthority          solana.PublicKey
	Program                 solana.PublicKey
	Remaining               []solana.PublicKey
}

// DefaultRouteAccounts returns the golden `route` account prefix.
func (s *Swap) DefaultRouteAccounts() RouteAccounts {
	return RouteAccounts{
		TokenProgram:            s.InputProgram,
		Authority:               s.Wallet,
		UserSource:              s.InputATA,
		UserDestination:         s.OutputATA,
		DestinationTokenAccount: s.RouteProgram,
		DestinationMint:         s.OutputMint,
		PlatformFeeAccount:      s.RouteProgram,
		EventAuthority:          s.EventAuthority,
		Program:                 s.RouteProgram,
		Remaining:               append([]solana.PublicKey{s.DEXProgram}, s.PoolAccounts...),
	}
}

// DefaultRouteArgs returns the golden route arguments.
func (s *Swap) DefaultRouteArgs() RouteArgs {
	return RouteArgs{InAmount: s.InAmount, QuotedOut: s.QuotedOut, SlippageBPS: s.SlippageBPS}
}

// RouteIxWith builds a `route` instruction from explicit accounts and args.
func (s *Swap) RouteIxWith(acc RouteAccounts, args RouteArgs) solana.Instruction {
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(acc.TokenProgram, false, false),
		solana.NewAccountMeta(acc.Authority, false, true),
		solana.NewAccountMeta(acc.UserSource, true, false),
		solana.NewAccountMeta(acc.UserDestination, true, false),
		solana.NewAccountMeta(acc.DestinationTokenAccount, acc.DestinationTokenAccount != acc.Program, false),
		solana.NewAccountMeta(acc.DestinationMint, false, false),
		solana.NewAccountMeta(acc.PlatformFeeAccount, acc.PlatformFeeAccount != acc.Program, false),
		solana.NewAccountMeta(acc.EventAuthority, false, false),
		solana.NewAccountMeta(acc.Program, false, false),
	}
	for i, r := range acc.Remaining {
		metas = append(metas, solana.NewAccountMeta(r, i > 0, false)) // pools writable, DEX program readonly
	}
	return solana.NewInstruction(s.RouteProgram, metas, RouteData(args))
}

// RouteIx returns the golden `route` instruction.
func (s *Swap) RouteIx() solana.Instruction {
	return s.RouteIxWith(s.DefaultRouteAccounts(), s.DefaultRouteArgs())
}

// SharedAccountsRouteIx returns the golden `shared_accounts_route` variant.
func (s *Swap) SharedAccountsRouteIx() solana.Instruction {
	programSource := PubkeyFromSeed("jupiter-program-source")
	programDest := PubkeyFromSeed("jupiter-program-destination")
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(s.InputProgram, false, false),
		solana.NewAccountMeta(s.ProgramAuth, false, false),
		solana.NewAccountMeta(s.Wallet, false, true),
		solana.NewAccountMeta(s.InputATA, true, false),
		solana.NewAccountMeta(programSource, true, false),
		solana.NewAccountMeta(programDest, true, false),
		solana.NewAccountMeta(s.OutputATA, true, false),
		solana.NewAccountMeta(s.InputMint, false, false),
		solana.NewAccountMeta(s.OutputMint, false, false),
		solana.NewAccountMeta(s.RouteProgram, false, false), // platform fee: absent
		solana.NewAccountMeta(s.RouteProgram, false, false), // token 2022 program: absent
		solana.NewAccountMeta(s.EventAuthority, false, false),
		solana.NewAccountMeta(s.RouteProgram, false, false),
	}
	metas = append(metas, solana.NewAccountMeta(s.DEXProgram, false, false))
	for _, p := range s.PoolAccounts {
		metas = append(metas, solana.NewAccountMeta(p, true, false))
	}
	args := s.DefaultRouteArgs()
	args.Name = "shared_accounts_route"
	args.SharedID = 3
	return solana.NewInstruction(s.RouteProgram, metas, RouteData(args))
}

// WrapIxs returns the native-input legs: create wSOL ATA, transfer lamports,
// SyncNative.
func (s *Swap) WrapIxs(lamports uint64) []solana.Instruction {
	return []solana.Instruction{
		s.ATACreateIx(s.Wallet, WrappedSOLMint, TokenProgram),
		solana.NewInstruction(SystemProgram, solana.AccountMetaSlice{
			solana.NewAccountMeta(s.Wallet, true, true),
			solana.NewAccountMeta(s.WSOLATA, true, false),
		}, SystemTransferData(lamports)),
		solana.NewInstruction(TokenProgram, solana.AccountMetaSlice{
			solana.NewAccountMeta(s.WSOLATA, true, false),
		}, TokenSingleByteData(17)),
	}
}

// CloseIx returns Token.CloseAccount(account → destination) signed by the wallet.
func (s *Swap) CloseIx(account, destination solana.PublicKey) solana.Instruction {
	return solana.NewInstruction(TokenProgram, solana.AccountMetaSlice{
		solana.NewAccountMeta(account, true, false),
		solana.NewAccountMeta(destination, true, false),
		solana.NewAccountMeta(s.Wallet, false, true),
	}, TokenSingleByteData(9))
}

// GoldenInstructions returns the approved instruction list.
func (s *Swap) GoldenInstructions() []solana.Instruction {
	ixs := s.ComputeBudgetIxs()
	if s.NativeInput {
		ixs = append(ixs, s.WrapIxs(s.InAmount)...)
	}
	ixs = append(ixs, s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIx())
	if s.NativeInput {
		ixs = append(ixs, s.CloseIx(s.WSOLATA, s.Wallet))
	}
	return ixs
}

// Build serializes an unsigned transaction with the given instructions. For
// v0 the pool accounts are loaded from the lookup table.
func (s *Swap) Build(version inspect.Version, ixs []solana.Instruction) ([]byte, error) {
	opts := []solana.TransactionOption{solana.TransactionPayer(s.Wallet)}
	if version == inspect.VersionV0 {
		opts = append(opts,
			solana.TransactionMessageVersion(solana.MessageVersionV0),
			solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{s.LookupTable: s.LookupEntries}),
		)
	}
	tx, err := solana.NewTransaction(ixs, s.Blockhash, opts...)
	if err != nil {
		return nil, err
	}
	return tx.MarshalBinary()
}

// MustBuild is Build or panic (fixtures are deterministic).
func (s *Swap) MustBuild(version inspect.Version, ixs []solana.Instruction) []byte {
	raw, err := s.Build(version, ixs)
	if err != nil {
		panic(err)
	}
	return raw
}

// Golden returns the serialized golden transaction.
func (s *Swap) Golden(version inspect.Version) []byte {
	return s.MustBuild(version, s.GoldenInstructions())
}

// LookupTables returns the lookup table contents in Expectations form.
func (s *Swap) LookupTables() map[string][]string {
	entries := make([]string, 0, len(s.LookupEntries))
	for _, e := range s.LookupEntries {
		entries = append(entries, e.String())
	}
	return map[string][]string{s.LookupTable.String(): entries}
}

// Simulation returns a simulation result consistent with the golden swap.
func (s *Swap) Simulation(raw []byte) *inspect.SimulationResult {
	h := inspect.TxHash(raw)
	sim := &inspect.SimulationResult{
		Succeeded:       true,
		TxHash:          h[:],
		InnerProgramIDs: []string{s.DEXProgram.String(), inspect.TokenProgram, inspect.SystemProgram},
		UnitsConsumed:   180_000,
	}
	minOut := quantity(s.MinOutput)
	if s.NativeInput {
		sim.NativeDeltas = []inspect.NativeDelta{{Account: s.Wallet.String(), Lamports: -lamports(s.InAmount) - 5_000 - 400 - 2*inspect.TokenAccountRentLamports}}
		sim.TokenDeltas = []inspect.TokenDelta{{Owner: s.Wallet.String(), Mint: s.OutputMint.String(), Delta: minOut.Add(money.QuantityFromInt64(1))}}
		return sim
	}
	sim.TokenDeltas = []inspect.TokenDelta{
		{Owner: s.Wallet.String(), Mint: s.InputMint.String(), Delta: quantity(s.InAmount).Neg()},
		{Owner: s.Wallet.String(), Mint: s.OutputMint.String(), Delta: minOut.Add(money.QuantityFromInt64(1))},
	}
	sim.NativeDeltas = []inspect.NativeDelta{{Account: s.Wallet.String(), Lamports: -5_000 - 400 - inspect.TokenAccountRentLamports}}
	return sim
}

// Expectations returns expectations that approve the golden transaction raw.
func (s *Swap) Expectations(raw []byte) inspect.Expectations {
	return inspect.Expectations{
		Wallet:                 s.Wallet.String(),
		FeePayer:               s.Wallet.String(),
		InputMint:              s.InputMint.String(),
		InputTokenProgram:      s.InputProgram.String(),
		InputIsNative:          s.NativeInput,
		MaxInputDebit:          quantity(s.MaxInputDebit),
		OutputMint:             s.OutputMint.String(),
		OutputTokenProgram:     s.OutputProgram.String(),
		MinOutput:              quantity(s.MinOutput),
		AllowedPrograms:        append(inspect.DefaultAllowedPrograms(), s.RouteProgram.String(), s.DEXProgram.String()),
		RoutePrograms:          []string{s.RouteProgram.String()},
		MaxPriorityFeeLamports: s.MaxPriorityFee,
		MaxComputeUnits:        s.MaxCU,
		RecentBlockhash:        s.Blockhash.String(),
		LastValidBlockHeight:   s.LastValidBlockHeight,
		CurrentBlockHeight:     s.CurrentBlockHeight,
		BlockHeightMargin:      s.BlockHeightMargin,
		PlanHash:               append([]byte(nil), s.PlanHash...),
		QuoteID:                s.QuoteID,
		Claimed:                inspect.PlanIdentity{PlanHash: append([]byte(nil), s.PlanHash...), QuoteID: s.QuoteID},
		MaxSlippageBPS:         s.MaxSlippageBPS,
		Simulation:             s.Simulation(raw),
		RequireSimulation:      true,
		LookupTables:           s.LookupTables(),
	}
}
