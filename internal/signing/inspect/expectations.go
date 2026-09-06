package inspect

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/money"
)

// PlanIdentity is the (plan hash, quote id) pair a caller claims to be
// signing for. Inspect compares it with the persisted identity in
// Expectations (PLAN_IDENTITY).
type PlanIdentity struct {
	PlanHash []byte
	QuoteID  string
}

// TokenDelta is a predicted token balance change (post − pre, base units) for
// one (owner, mint) pair, taken from simulation pre/post token balances.
type TokenDelta struct {
	Owner string
	Mint  string
	Delta money.Quantity
}

// NativeDelta is a predicted lamport balance change for one account.
type NativeDelta struct {
	Account  string
	Lamports int64
}

// SimulationResult is the caller-observed simulateTransaction outcome. The
// inspector never runs a simulation; the executor does and the signing
// service loads the persisted result.
type SimulationResult struct {
	// Succeeded is meta.err == null.
	Succeeded bool
	// Error is the simulation error rendering when Succeeded is false.
	Error string
	// TxHash, when set, is sha256 of the exact bytes that were simulated and
	// must equal the inspected transaction hash.
	TxHash []byte
	// InnerProgramIDs are every program id invoked through CPI.
	InnerProgramIDs []string
	// TokenDeltas are predicted token balance changes.
	TokenDeltas []TokenDelta
	// NativeDeltas are predicted lamport balance changes (optional).
	NativeDeltas []NativeDelta
	// UnitsConsumed is informational.
	UnitsConsumed uint64
}

// Expectations is everything the inspector may rely on. The signing service
// rebuilds it from persisted rows only (EXECUTION.md §3); nothing in it comes
// from the caller of Sign except LookupTables (chain data the service may
// verify against its own observer) and Claimed (compared, never trusted).
type Expectations struct {
	// Wallet is the base58 address of the approved wallet: fee payer and the
	// only signer.
	Wallet string
	// FeePayer is the expected fee payer (normally == Wallet).
	FeePayer string

	// InputMint / InputTokenProgram describe the asset being spent. The
	// native asset is expressed as the wrapped-SOL mint under SPL Token.
	InputMint         string
	InputTokenProgram string
	// InputIsNative marks the input as SOL: wrapping transfers into the
	// wallet's wSOL ATA are permitted up to MaxInputDebit.
	InputIsNative bool
	// MaxInputDebit bounds every possible debit of the input token from the
	// wallet (reservation remaining, plan MaxInputQuantity).
	MaxInputDebit money.Quantity

	// OutputMint / OutputTokenProgram describe the asset being acquired.
	OutputMint         string
	OutputTokenProgram string
	OutputIsNative     bool
	// MinOutput is the plan's MinOutputQuantity in base units.
	MinOutput money.Quantity

	// AllowedPrograms is the program-id allowlist (configuration plus the
	// plan's additions, e.g. the DEX programs of the route).
	AllowedPrograms []string
	// RoutePrograms are the venue program ids whose instructions are decoded
	// with the Jupiter v6 layout (default: JupiterV6Program). Each must also
	// be allowlisted.
	RoutePrograms []string
	// Token2022Allowed permits Token-2022 program instructions for the
	// assets above (asset explicitly enabled and carrying no extensions).
	Token2022Allowed bool
	// AllowedFeeAccounts lists venue/platform fee token accounts the route
	// may pay into (empty: no platform fee account permitted).
	AllowedFeeAccounts []string

	// MaxPriorityFeeLamports bounds computeUnitLimit × computeUnitPrice.
	MaxPriorityFeeLamports uint64
	// MaxComputeUnits bounds the compute-unit limit.
	MaxComputeUnits uint32

	// RecentBlockhash is the blockhash the adapter reported for the build.
	RecentBlockhash string
	// LastValidBlockHeight is the adapter-reported expiry.
	LastValidBlockHeight uint64
	// CurrentBlockHeight is the observed chain height at inspection time.
	CurrentBlockHeight uint64
	// BlockHeightMargin is the policy margin LastValidBlockHeight must exceed
	// CurrentBlockHeight by.
	BlockHeightMargin uint64

	// PlanHash and QuoteID are the persisted approved plan identity.
	PlanHash []byte
	QuoteID  string
	// Claimed is the identity the caller/attempt claims; must match.
	Claimed PlanIdentity

	// MaxSlippageBPS is the strictest slippage bound (intent, risk, quote).
	MaxSlippageBPS money.BPS

	// Simulation is the persisted simulation outcome; RequireSimulation
	// makes its absence a rejection.
	Simulation        *SimulationResult
	RequireSimulation bool

	// LookupTables maps address-lookup-table address → its addresses, for
	// resolving v0 transactions. Missing tables reject.
	LookupTables map[string][]string
}

// ErrExpectations wraps every Expectations validation failure.
var ErrExpectations = errors.New("inspect: invalid expectations")

// parsedExpectations is Expectations with keys parsed and derived addresses.
type parsedExpectations struct {
	Expectations
	wallet, feePayer                         solana.PublicKey
	inputMint, outputMint                    solana.PublicKey
	inputProgram, outputProgram              solana.PublicKey
	inputATA, outputATA                      solana.PublicKey
	wsolATA                                  solana.PublicKey // wallet's wSOL ATA under SPL Token
	allowed                                  map[solana.PublicKey]struct{}
	routePrograms                            map[solana.PublicKey]struct{}
	feeAccounts                              map[solana.PublicKey]struct{}
	blockhash                                solana.Hash
	tables                                   map[solana.PublicKey][]solana.PublicKey
	hasNativeLeg                             bool
	sortedAllowed                            []string
	inputProgramLabel, outputProgramLabel    string
	inputMintLabel, outputMintLabel          string
	walletLabel, feePayerLabel               string
	inputATALabel, outputATALabel, wsolLabel string
}

// parseKey parses a base58 key that must not be the zero key (wallet, mints,
// fee accounts). The System program id is the zero key, so program ids and
// lookup entries use parseProgramKey instead.
func parseKey(field, s string) (solana.PublicKey, error) {
	k, err := parseProgramKey(field, s)
	if err != nil {
		return solana.PublicKey{}, err
	}
	if k.IsZero() {
		return solana.PublicKey{}, fmt.Errorf("%w: %s is the zero key", ErrExpectations, field)
	}
	return k, nil
}

func parseProgramKey(field, s string) (solana.PublicKey, error) {
	if s == "" {
		return solana.PublicKey{}, fmt.Errorf("%w: %s is empty", ErrExpectations, field)
	}
	k, err := solana.PublicKeyFromBase58(s)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("%w: %s is not a base58 public key", ErrExpectations, field)
	}
	return k, nil
}

func parseTokenProgram(field, s string) (solana.PublicKey, error) {
	k, err := parseKey(field, s)
	if err != nil {
		return solana.PublicKey{}, err
	}
	if !k.Equals(tokenProgramKey) && !k.Equals(token2022ProgramKey) {
		return solana.PublicKey{}, fmt.Errorf("%w: %s must be SPL Token or Token-2022", ErrExpectations, field)
	}
	return k, nil
}

// Validate reports the first reason the expectations cannot be used. A
// failing Validate makes Inspect reject every check.
func (e Expectations) Validate() error {
	_, err := e.parse()
	return err
}

func (e Expectations) parse() (*parsedExpectations, error) {
	p := &parsedExpectations{Expectations: e}
	var err error
	if p.wallet, err = parseKey("wallet", e.Wallet); err != nil {
		return nil, err
	}
	if p.feePayer, err = parseKey("fee payer", e.FeePayer); err != nil {
		return nil, err
	}
	if p.inputMint, err = parseKey("input mint", e.InputMint); err != nil {
		return nil, err
	}
	if p.outputMint, err = parseKey("output mint", e.OutputMint); err != nil {
		return nil, err
	}
	if p.inputMint.Equals(p.outputMint) {
		return nil, fmt.Errorf("%w: input and output mint are identical", ErrExpectations)
	}
	if p.inputProgram, err = parseTokenProgram("input token program", e.InputTokenProgram); err != nil {
		return nil, err
	}
	if p.outputProgram, err = parseTokenProgram("output token program", e.OutputTokenProgram); err != nil {
		return nil, err
	}
	if e.InputIsNative && (!p.inputMint.Equals(wrappedSOLMintKey) || !p.inputProgram.Equals(tokenProgramKey)) {
		return nil, fmt.Errorf("%w: native input must use the wrapped-SOL mint under SPL Token", ErrExpectations)
	}
	if e.OutputIsNative && (!p.outputMint.Equals(wrappedSOLMintKey) || !p.outputProgram.Equals(tokenProgramKey)) {
		return nil, fmt.Errorf("%w: native output must use the wrapped-SOL mint under SPL Token", ErrExpectations)
	}
	if !e.MaxInputDebit.IsPositive() {
		return nil, fmt.Errorf("%w: max input debit must be positive", ErrExpectations)
	}
	if !e.MinOutput.IsPositive() {
		return nil, fmt.Errorf("%w: min output must be positive", ErrExpectations)
	}
	if len(e.AllowedPrograms) == 0 {
		return nil, fmt.Errorf("%w: empty program allowlist", ErrExpectations)
	}
	p.allowed = make(map[solana.PublicKey]struct{}, len(e.AllowedPrograms))
	for _, s := range e.AllowedPrograms {
		k, err := parseProgramKey("allowed program", s)
		if err != nil {
			return nil, err
		}
		p.allowed[k] = struct{}{}
	}
	if _, ok := p.allowed[token2022ProgramKey]; ok && !e.Token2022Allowed {
		return nil, fmt.Errorf("%w: Token-2022 is allowlisted but not enabled", ErrExpectations)
	}
	if e.Token2022Allowed {
		if _, ok := p.allowed[token2022ProgramKey]; !ok {
			return nil, fmt.Errorf("%w: Token-2022 is enabled but not allowlisted", ErrExpectations)
		}
	}
	routes := e.RoutePrograms
	if len(routes) == 0 {
		routes = []string{JupiterV6Program}
	}
	p.routePrograms = make(map[solana.PublicKey]struct{}, len(routes))
	for _, s := range routes {
		k, err := parseProgramKey("route program", s)
		if err != nil {
			return nil, err
		}
		if _, ok := p.allowed[k]; !ok {
			return nil, fmt.Errorf("%w: route program %s is not allowlisted", ErrExpectations, s)
		}
		if k.Equals(systemProgramKey) || k.Equals(computeBudgetProgramKey) || k.Equals(tokenProgramKey) ||
			k.Equals(token2022ProgramKey) || k.Equals(associatedTokenProgramKey) {
			return nil, fmt.Errorf("%w: route program %s is a core program", ErrExpectations, s)
		}
		p.routePrograms[k] = struct{}{}
	}
	p.feeAccounts = make(map[solana.PublicKey]struct{}, len(e.AllowedFeeAccounts))
	for _, s := range e.AllowedFeeAccounts {
		k, err := parseKey("allowed fee account", s)
		if err != nil {
			return nil, err
		}
		p.feeAccounts[k] = struct{}{}
	}
	if e.MaxComputeUnits == 0 || e.MaxComputeUnits > MaxComputeUnitLimit {
		return nil, fmt.Errorf("%w: max compute units must be in (0, %d]", ErrExpectations, MaxComputeUnitLimit)
	}
	if e.RecentBlockhash == "" {
		return nil, fmt.Errorf("%w: recent blockhash is empty", ErrExpectations)
	}
	if p.blockhash, err = solana.HashFromBase58(e.RecentBlockhash); err != nil || p.blockhash.IsZero() {
		return nil, fmt.Errorf("%w: recent blockhash is not a base58 hash", ErrExpectations)
	}
	if e.LastValidBlockHeight == 0 {
		return nil, fmt.Errorf("%w: last valid block height is zero", ErrExpectations)
	}
	if e.CurrentBlockHeight == 0 {
		return nil, fmt.Errorf("%w: current block height is zero", ErrExpectations)
	}
	if len(e.PlanHash) != sha256.Size {
		return nil, fmt.Errorf("%w: plan hash must be %d bytes", ErrExpectations, sha256.Size)
	}
	if e.QuoteID == "" {
		return nil, fmt.Errorf("%w: quote id is empty", ErrExpectations)
	}
	if e.MaxSlippageBPS < 0 || e.MaxSlippageBPS > money.OneHundredPercent {
		return nil, fmt.Errorf("%w: max slippage bps must be in [0, 10000]", ErrExpectations)
	}
	p.tables = make(map[solana.PublicKey][]solana.PublicKey, len(e.LookupTables))
	for addr, entries := range e.LookupTables {
		k, err := parseKey("lookup table address", addr)
		if err != nil {
			return nil, err
		}
		if len(entries) > 256 {
			return nil, fmt.Errorf("%w: lookup table %s has more than 256 entries", ErrExpectations, addr)
		}
		keys := make([]solana.PublicKey, 0, len(entries))
		for _, s := range entries {
			ek, err := parseProgramKey("lookup table entry", s)
			if err != nil {
				return nil, err
			}
			keys = append(keys, ek)
		}
		p.tables[k] = keys
	}
	if p.inputATA, _, err = solana.FindAssociatedTokenAddressWithProgram(p.wallet, p.inputMint, p.inputProgram); err != nil {
		return nil, fmt.Errorf("%w: input ATA derivation: %v", ErrExpectations, err)
	}
	if p.outputATA, _, err = solana.FindAssociatedTokenAddressWithProgram(p.wallet, p.outputMint, p.outputProgram); err != nil {
		return nil, fmt.Errorf("%w: output ATA derivation: %v", ErrExpectations, err)
	}
	if p.wsolATA, _, err = solana.FindAssociatedTokenAddressWithProgram(p.wallet, wrappedSOLMintKey, tokenProgramKey); err != nil {
		return nil, fmt.Errorf("%w: wSOL ATA derivation: %v", ErrExpectations, err)
	}
	p.hasNativeLeg = e.InputIsNative || e.OutputIsNative
	p.sortedAllowed = append([]string(nil), e.AllowedPrograms...)
	sort.Strings(p.sortedAllowed)
	p.walletLabel, p.feePayerLabel = p.wallet.String(), p.feePayer.String()
	p.inputMintLabel, p.outputMintLabel = p.inputMint.String(), p.outputMint.String()
	p.inputProgramLabel, p.outputProgramLabel = p.inputProgram.String(), p.outputProgram.String()
	p.inputATALabel, p.outputATALabel, p.wsolLabel = p.inputATA.String(), p.outputATA.String(), p.wsolATA.String()
	return p, nil
}

// isAllowed reports whether program is allowlisted.
func (p *parsedExpectations) isAllowed(program solana.PublicKey) bool {
	_, ok := p.allowed[program]
	return ok
}

// isRouteProgram reports whether program is decoded as a Jupiter v6 venue.
func (p *parsedExpectations) isRouteProgram(program solana.PublicKey) bool {
	_, ok := p.routePrograms[program]
	return ok
}

// mintOf maps a wallet-owned token account the inspector knows about to its
// mint and token program.
func (p *parsedExpectations) mintOf(acct solana.PublicKey) (mint, program solana.PublicKey, ok bool) {
	switch {
	case acct.Equals(p.inputATA):
		return p.inputMint, p.inputProgram, true
	case acct.Equals(p.outputATA):
		return p.outputMint, p.outputProgram, true
	case acct.Equals(p.wsolATA) && p.hasNativeLeg:
		return wrappedSOLMintKey, tokenProgramKey, true
	}
	return solana.PublicKey{}, solana.PublicKey{}, false
}

// isExpectedMint reports whether mint is the input or output mint.
func (p *parsedExpectations) isExpectedMint(mint solana.PublicKey) bool {
	return mint.Equals(p.inputMint) || mint.Equals(p.outputMint)
}

// programFor returns the token program expected for mint.
func (p *parsedExpectations) programFor(mint solana.PublicKey) (solana.PublicKey, bool) {
	switch {
	case mint.Equals(p.inputMint):
		return p.inputProgram, true
	case mint.Equals(p.outputMint):
		return p.outputProgram, true
	}
	return solana.PublicKey{}, false
}
