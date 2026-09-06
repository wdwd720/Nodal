package inspect

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/money"
)

// Inspect evaluates every EXECUTION.md §2 check against tx and exp and
// returns a deterministic Result. It never panics and never approves a
// transaction it could not fully evaluate.
func Inspect(tx DecodedTransaction, exp Expectations) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = RejectAll(ReasonInspectorPanic, fmt.Sprintf("inspector panic: %v", r))
		}
	}()
	p, err := exp.parse()
	if err != nil {
		return RejectAll(ReasonExpectationsInvalid, err.Error())
	}
	rtx, err := resolve(tx, p.tables)
	if err != nil {
		code := ReasonTransactionMalformed
		if strings.Contains(err.Error(), "lookup table") {
			code = ReasonLookupTableUnresolved
		}
		return RejectAll(code, err.Error())
	}
	c := newCollector()
	w := &walk{topLevelOut: money.QuantityFromInt64(0)}
	if !p.Token2022Allowed && (p.inputProgram.Equals(token2022ProgramKey) || p.outputProgram.Equals(token2022ProgramKey)) {
		c.fail(CheckTokenProgram, ReasonToken2022Disabled, "an expected asset is a Token-2022 mint but Token-2022 is not enabled for it")
	}
	checkIdentity(c, tx, p, rtx)
	for _, ix := range rtx.ixs {
		inspectInstruction(c, p, w, ix)
	}
	checkComputeBudget(c, p, w, len(rtx.ixs))
	checkRoute(c, p, w)
	checkBlockhash(c, tx, p)
	checkPlanIdentity(c, p)
	checkSimulation(c, tx, p, w)
	return c.result()
}

// walk accumulates what the instruction pass learned.
type walk struct {
	cuLimit       *uint32
	cuPrice       *uint64
	cuLimitCount  int
	cuPriceCount  int
	nonBudgetIxs  int
	routes        []routeIx
	wrapLamports  uint64
	wrapOverflow  bool
	topLevelOut   money.Quantity // top-level token transfers out of the wallet's input ATA
	ataCreates    int
	priorityFee   *big.Int // computed by checkComputeBudget
	effectiveCUs  uint64
	routeInAmount money.Quantity
}

func ixLabel(ix resolvedIx) string { return fmt.Sprintf("ix %d", ix.index) }

// checkIdentity evaluates FEE_PAYER and SIGNERS.
func checkIdentity(c *collector, tx DecodedTransaction, p *parsedExpectations, rtx *resolvedTx) {
	payer := rtx.accounts[0]
	if !payer.key.Equals(p.feePayer) {
		c.fail(CheckFeePayer, ReasonFeePayerMismatch, fmt.Sprintf("fee payer %s, expected %s", payer.key, p.feePayerLabel))
	}
	if !p.feePayer.Equals(p.wallet) {
		c.fail(CheckFeePayer, ReasonFeePayerMismatch, "expected fee payer differs from the wallet")
	}
	h := tx.Header
	if h.NumRequiredSignatures != 1 || tx.NumSignatures != 1 {
		var signers []string
		for _, a := range rtx.accounts {
			if a.signer {
				signers = append(signers, a.key.String())
			}
		}
		c.fail(CheckSigners, ReasonExtraSigner, fmt.Sprintf("%d required signatures (%s), expected exactly the wallet", h.NumRequiredSignatures, strings.Join(signers, ",")))
	}
	if h.NumReadonlySignedAccounts != 0 {
		c.fail(CheckSigners, ReasonExtraSigner, fmt.Sprintf("%d readonly signed accounts", h.NumReadonlySignedAccounts))
	}
	if !payer.key.Equals(p.wallet) {
		c.fail(CheckSigners, ReasonSignerMismatch, fmt.Sprintf("first signer %s is not the wallet %s", payer.key, p.walletLabel))
	}
	for _, a := range rtx.accounts {
		if a.fromLookup && a.signer {
			c.fail(CheckSigners, ReasonExtraSigner, "lookup-loaded account flagged as signer")
		}
	}
}

// inspectInstruction classifies one top-level instruction.
func inspectInstruction(c *collector, p *parsedExpectations, w *walk, ix resolvedIx) {
	label := ixLabel(ix)
	if !p.isAllowed(ix.program) {
		c.fail(CheckProgramAllowlist, ReasonProgramNotAllowed, fmt.Sprintf("%s program %s is not allowlisted", label, ix.program))
		c.fail(CheckNoArbitraryCPI, ReasonProgramNotAllowed, fmt.Sprintf("%s program %s is not allowlisted", label, ix.program))
		w.nonBudgetIxs++
		return
	}
	switch {
	case ix.program.Equals(systemProgramKey):
		w.nonBudgetIxs++
		inspectSystem(c, p, w, ix)
	case ix.program.Equals(computeBudgetProgramKey):
		inspectComputeBudget(c, w, ix)
	case ix.program.Equals(tokenProgramKey) || ix.program.Equals(token2022ProgramKey):
		w.nonBudgetIxs++
		inspectToken(c, p, w, ix)
	case ix.program.Equals(associatedTokenProgramKey):
		w.nonBudgetIxs++
		inspectATA(c, p, w, ix)
	case p.isRouteProgram(ix.program):
		w.nonBudgetIxs++
		inspectRoute(c, p, w, ix)
	default:
		w.nonBudgetIxs++
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s top-level instruction to non-core program %s", label, ix.program))
	}
}

func inspectSystem(c *collector, p *parsedExpectations, w *walk, ix resolvedIx) {
	label := ixLabel(ix)
	six, err := decodeSystem(ix.data)
	if err != nil {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s System: %v", label, err))
		return
	}
	switch six.tag {
	case 2: // Transfer
		if len(ix.accounts) != 2 {
			c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s System.Transfer with %d accounts", label, len(ix.accounts)))
			return
		}
		from, to := ix.accounts[0], ix.accounts[1]
		if !from.key.Equals(p.wallet) {
			c.fail(CheckNoSystemTransfer, ReasonSystemTransfer, fmt.Sprintf("%s System.Transfer from %s (not the wallet)", label, from.key))
			return
		}
		if p.InputIsNative && to.key.Equals(p.wsolATA) {
			sum := w.wrapLamports + six.lamports
			if sum < w.wrapLamports {
				w.wrapOverflow = true
			}
			w.wrapLamports = sum
			c.note(CheckNoSystemTransfer, fmt.Sprintf("%s wraps %d lamports into the wallet's wSOL ATA", label, six.lamports))
			return
		}
		c.fail(CheckNoSystemTransfer, ReasonSystemTransfer, fmt.Sprintf("%s System.Transfer of %d lamports from the wallet to %s", label, six.lamports, to.key))
		c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("%s lamports to %s", label, to.key))
	case 0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 12: // CreateAccount, Assign, *WithSeed, nonce, Allocate
		c.fail(CheckNoAuthorityChange, ReasonAuthorityChange, fmt.Sprintf("%s System.%s", label, six.name))
	case 11: // TransferWithSeed
		c.fail(CheckNoSystemTransfer, ReasonSystemTransfer, fmt.Sprintf("%s System.TransferWithSeed", label))
	default:
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s System.%s", label, six.name))
	}
}

func inspectComputeBudget(c *collector, w *walk, ix resolvedIx) {
	label := ixLabel(ix)
	cb, err := decodeComputeBudget(ix.data)
	if err != nil {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s ComputeBudget: %v", label, err))
		return
	}
	switch cb.tag {
	case cbSetComputeUnitLimit:
		units := cb.units
		w.cuLimit = &units
		w.cuLimitCount++
	case cbSetComputeUnitPrice:
		price := cb.price
		w.cuPrice = &price
		w.cuPriceCount++
	case cbRequestHeapFrame, cbSetLoadedAccountsDataSizeLim:
		c.note(CheckComputeBudget, fmt.Sprintf("%s ComputeBudget.%s", label, cb.name))
	default:
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s ComputeBudget.%s", label, cb.name))
	}
}

func inspectToken(c *collector, p *parsedExpectations, w *walk, ix resolvedIx) {
	label := ixLabel(ix)
	if ix.program.Equals(token2022ProgramKey) && !p.Token2022Allowed {
		c.fail(CheckTokenProgram, ReasonToken2022Disabled, fmt.Sprintf("%s Token-2022 instruction while Token-2022 is disabled", label))
	}
	tix, err := decodeToken(ix.data)
	if err != nil {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Token: %v", label, err))
		return
	}
	if tix.tag >= 25 {
		c.fail(CheckTokenProgram, ReasonToken2022Disabled, fmt.Sprintf("%s Token-2022 extension instruction %s", label, tix.name))
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Token.%s", label, tix.name))
		return
	}
	switch tix.tag {
	case tokenTransfer:
		if len(ix.accounts) != 3 {
			c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Token.Transfer with %d accounts (multisig or malformed)", label, len(ix.accounts)))
			return
		}
		src, dst, owner := ix.accounts[0], ix.accounts[1], ix.accounts[2]
		mint, program, known := p.mintOf(src.key)
		if !known {
			c.fail(CheckTokenProgram, ReasonTokenAccountUnknown, fmt.Sprintf("%s Token.Transfer source %s is not a known wallet token account", label, src.key))
			return
		}
		checkTokenMove(c, p, w, label, "Transfer", ix.program, program, mint, src, dst, owner, tix.amount)
	case tokenTransferChecked:
		if len(ix.accounts) != 4 {
			c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Token.TransferChecked with %d accounts (multisig or malformed)", label, len(ix.accounts)))
			return
		}
		src, mintAcct, dst, owner := ix.accounts[0], ix.accounts[1], ix.accounts[2], ix.accounts[3]
		mint, program, known := p.mintOf(src.key)
		if !known {
			c.fail(CheckTokenProgram, ReasonTokenAccountUnknown, fmt.Sprintf("%s Token.TransferChecked source %s is not a known wallet token account", label, src.key))
			return
		}
		if !mintAcct.key.Equals(mint) {
			c.fail(CheckTokenProgram, ReasonUnexpectedMint, fmt.Sprintf("%s Token.TransferChecked mint %s does not match source account mint %s", label, mintAcct.key, mint))
			return
		}
		checkTokenMove(c, p, w, label, "TransferChecked", ix.program, program, mint, src, dst, owner, tix.amount)
	case tokenSyncNative:
		if len(ix.accounts) != 1 {
			c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Token.SyncNative with %d accounts", label, len(ix.accounts)))
			return
		}
		if !p.hasNativeLeg || !ix.accounts[0].key.Equals(p.wsolATA) || !ix.program.Equals(tokenProgramKey) {
			c.fail(CheckTokenProgram, ReasonTokenAccountUnknown, fmt.Sprintf("%s Token.SyncNative on %s (not the wallet's wSOL ATA for a native leg)", label, ix.accounts[0].key))
		}
	case tokenCloseAccount:
		if len(ix.accounts) != 3 {
			c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Token.CloseAccount with %d accounts", label, len(ix.accounts)))
			return
		}
		acct, dest, owner := ix.accounts[0], ix.accounts[1], ix.accounts[2]
		if !dest.key.Equals(p.wallet) {
			c.fail(CheckNoAuthorityChange, ReasonUnexpectedClose, fmt.Sprintf("%s Token.CloseAccount %s pays rent to %s (not the wallet)", label, acct.key, dest.key))
			c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("%s close destination %s", label, dest.key))
			return
		}
		if !p.hasNativeLeg || !acct.key.Equals(p.wsolATA) || !owner.key.Equals(p.wallet) || !ix.program.Equals(tokenProgramKey) {
			c.fail(CheckNoAuthorityChange, ReasonUnexpectedClose, fmt.Sprintf("%s Token.CloseAccount of %s (only the wallet's wSOL ATA may be closed on a native leg)", label, acct.key))
		}
	case tokenApprove, tokenApproveChecked:
		c.fail(CheckNoAuthorityChange, ReasonDelegation, fmt.Sprintf("%s Token.%s delegates spending authority", label, tix.name))
	case tokenSetAuthority, tokenFreezeAccount, tokenThawAccount:
		c.fail(CheckNoAuthorityChange, ReasonAuthorityChange, fmt.Sprintf("%s Token.%s", label, tix.name))
	default:
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Token.%s is not permitted in a swap", label, tix.name))
	}
}

// checkTokenMove applies the rules shared by Transfer and TransferChecked.
func checkTokenMove(c *collector, p *parsedExpectations, w *walk, label, name string, ixProgram, expectedProgram, mint solana.PublicKey, src, dst, owner account, amount uint64) {
	if !ixProgram.Equals(expectedProgram) {
		c.fail(CheckTokenProgram, ReasonTokenProgramMismatch, fmt.Sprintf("%s Token.%s of mint %s under %s, expected %s", label, name, mint, ixProgram, expectedProgram))
	}
	if !owner.key.Equals(p.wallet) {
		c.fail(CheckTokenProgram, ReasonTokenAccountUnknown, fmt.Sprintf("%s Token.%s authority %s is not the wallet", label, name, owner.key))
	}
	if _, _, known := p.mintOf(dst.key); !known {
		c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("%s Token.%s of %d to %s (not a wallet token account)", label, name, amount, dst.key))
	}
	if src.key.Equals(p.inputATA) {
		w.topLevelOut = w.topLevelOut.Add(quantityU64(amount))
	}
}

func inspectATA(c *collector, p *parsedExpectations, w *walk, ix resolvedIx) {
	label := ixLabel(ix)
	aix, err := decodeATA(ix.data)
	if err != nil {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s AssociatedToken: %v", label, err))
		return
	}
	if aix.tag == ataRecoverNested {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s AssociatedToken.RecoverNested", label))
		return
	}
	n := len(ix.accounts)
	if n != 6 && (n != 7 || !ix.accounts[6].key.Equals(sysvarRent)) {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s AssociatedToken.%s with %d accounts", label, aix.name, n))
		return
	}
	payer, ata, owner, mint, sys, tokenProg := ix.accounts[0], ix.accounts[1], ix.accounts[2], ix.accounts[3], ix.accounts[4], ix.accounts[5]
	if !sys.key.Equals(systemProgramKey) {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s AssociatedToken.%s system program account is %s", label, aix.name, sys.key))
		return
	}
	if !payer.key.Equals(p.wallet) {
		c.fail(CheckNoSystemTransfer, ReasonSystemTransfer, fmt.Sprintf("%s AssociatedToken.%s rent payer %s is not the wallet", label, aix.name, payer.key))
	}
	if !owner.key.Equals(p.wallet) {
		c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("%s AssociatedToken.%s creates an account for %s (not the wallet)", label, aix.name, owner.key))
		return
	}
	var expectedProgram solana.PublicKey
	switch {
	case p.isExpectedMint(mint.key):
		expectedProgram, _ = p.programFor(mint.key)
	case p.hasNativeLeg && mint.key.Equals(wrappedSOLMintKey):
		expectedProgram = tokenProgramKey
	default:
		c.fail(CheckTokenProgram, ReasonUnexpectedMint, fmt.Sprintf("%s AssociatedToken.%s for mint %s (not the input or output mint)", label, aix.name, mint.key))
		return
	}
	if tokenProg.key.Equals(token2022ProgramKey) && !p.Token2022Allowed {
		c.fail(CheckTokenProgram, ReasonToken2022Disabled, fmt.Sprintf("%s AssociatedToken.%s under Token-2022 while disabled", label, aix.name))
		return
	}
	if !tokenProg.key.Equals(expectedProgram) {
		c.fail(CheckTokenProgram, ReasonTokenProgramMismatch, fmt.Sprintf("%s AssociatedToken.%s under %s, expected %s", label, aix.name, tokenProg.key, expectedProgram))
		return
	}
	want, _, err := solana.FindAssociatedTokenAddressWithProgram(p.wallet, mint.key, tokenProg.key)
	if err != nil || !ata.key.Equals(want) {
		c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("%s AssociatedToken.%s address %s is not the wallet's ATA for %s", label, aix.name, ata.key, mint.key))
		return
	}
	w.ataCreates++
}

func inspectRoute(c *collector, p *parsedExpectations, w *walk, ix resolvedIx) {
	label := ixLabel(ix)
	r, err := decodeJupiter(ix.program, ix)
	if err != nil {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Jupiter: %v", label, err))
		return
	}
	if !r.selfProgram.Equals(ix.program) {
		c.fail(CheckNoArbitraryCPI, ReasonUnknownInstruction, fmt.Sprintf("%s Jupiter.%s program self-reference is %s", label, r.name, r.selfProgram))
		return
	}
	w.routes = append(w.routes, r)
	c.note(CheckNoArbitraryCPI, fmt.Sprintf("%s Jupiter.%s with %d route steps", label, r.name, r.steps))
}

// checkRoute evaluates the swap-level rules on the single accepted route:
// TOKEN_PROGRAM, INPUT_DEBIT_BOUND, OUTPUT_TOKEN, MIN_OUTPUT, SLIPPAGE and
// the destination rules. Missing or multiple routes fail every one of them.
func checkRoute(c *collector, p *parsedExpectations, w *walk) {
	if len(w.routes) != 1 {
		detail := fmt.Sprintf("%d route instructions, expected exactly one", len(w.routes))
		for _, name := range []string{CheckNoArbitraryCPI, CheckInputDebitBound, CheckOutputToken, CheckMinOutput, CheckSlippage} {
			c.fail(name, ReasonRouteCount, detail)
		}
		w.routeInAmount = money.QuantityFromInt64(0)
		return
	}
	r := w.routes[0]
	if !r.authority.key.Equals(p.wallet) || !r.authority.signer {
		c.fail(CheckSigners, ReasonSignerMismatch, fmt.Sprintf("route authority %s is not the signing wallet", r.authority.key))
	}
	if !r.tokenProgram.Equals(p.inputProgram) {
		c.fail(CheckTokenProgram, ReasonTokenProgramMismatch, fmt.Sprintf("route token program %s, expected %s", r.tokenProgram, p.inputProgramLabel))
	}
	if !r.userSource.Equals(p.inputATA) {
		c.fail(CheckTokenProgram, ReasonTokenAccountUnknown, fmt.Sprintf("route source %s is not the wallet's input ATA %s", r.userSource, p.inputATALabel))
	}
	if r.hasSourceMint && !r.sourceMint.Equals(p.inputMint) {
		c.fail(CheckTokenProgram, ReasonUnexpectedMint, fmt.Sprintf("route source mint %s, expected %s", r.sourceMint, p.inputMintLabel))
	}
	if !r.userDestination.Equals(p.outputATA) {
		c.fail(CheckOutputToken, ReasonOutputAccountMismatch, fmt.Sprintf("route destination %s is not the wallet's output ATA %s", r.userDestination, p.outputATALabel))
	}
	if !r.destinationMint.Equals(p.outputMint) {
		c.fail(CheckOutputToken, ReasonOutputMintMismatch, fmt.Sprintf("route destination mint %s, expected %s", r.destinationMint, p.outputMintLabel))
	}
	if r.hasDestinationTokenAccount && !r.destinationTokenAccount.Equals(p.outputATA) {
		c.fail(CheckOutputToken, ReasonOutputAccountMismatch, fmt.Sprintf("route destination token account %s is not the wallet's output ATA", r.destinationTokenAccount))
		c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("route output to %s", r.destinationTokenAccount))
	}
	if r.hasPlatformFeeAccount {
		if _, ok := p.feeAccounts[r.platformFeeAccount]; !ok {
			c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("route platform fee account %s is not an allowed fee account", r.platformFeeAccount))
		}
	} else if r.platformFeeBPS != 0 {
		c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, fmt.Sprintf("route platform fee %d bps without a fee account", r.platformFeeBPS))
	}
	for _, a := range r.remaining {
		if a.key.Equals(p.wallet) && a.writable {
			c.fail(CheckNoUnexpectedDestination, ReasonUnexpectedDestination, "route passes the wallet as a writable remaining account")
		}
	}
	// INPUT_DEBIT_BOUND
	in := quantityU64(r.inAmount)
	w.routeInAmount = in
	if r.inAmount == 0 {
		c.fail(CheckInputDebitBound, ReasonInputAmountZero, "route in_amount is zero")
	}
	debit := in.Add(w.topLevelOut)
	if debit.Cmp(p.MaxInputDebit) > 0 {
		c.fail(CheckInputDebitBound, ReasonInputDebitExceeded, fmt.Sprintf("possible input debit %s exceeds bound %s", debit, p.MaxInputDebit))
	}
	if w.wrapOverflow {
		c.fail(CheckNoSystemTransfer, ReasonWrapExceeded, "wrap lamports overflow")
		c.fail(CheckInputDebitBound, ReasonWrapExceeded, "wrap lamports overflow")
	} else if wrap := quantityU64(w.wrapLamports); wrap.Cmp(p.MaxInputDebit) > 0 {
		c.fail(CheckNoSystemTransfer, ReasonWrapExceeded, fmt.Sprintf("wrapped %s lamports exceed the input bound %s", wrap, p.MaxInputDebit))
		c.fail(CheckInputDebitBound, ReasonWrapExceeded, fmt.Sprintf("wrapped %s lamports exceed the input bound %s", wrap, p.MaxInputDebit))
	}
	// SLIPPAGE
	if money.BPS(r.slippageBPS) > p.MaxSlippageBPS {
		c.fail(CheckSlippage, ReasonSlippageExceeded, fmt.Sprintf("route slippage %d bps exceeds bound %d bps", r.slippageBPS, p.MaxSlippageBPS))
	}
	// MIN_OUTPUT: floor(quoted_out × (10000 − slippage) / 10000), the smaller
	// of the two roundings a program may apply.
	minOut := minimumOut(r.quotedOut, r.slippageBPS)
	if minOut.Cmp(p.MinOutput) < 0 {
		c.fail(CheckMinOutput, ReasonMinOutputBelowPlan, fmt.Sprintf("route minimum out %s (quoted %d at %d bps) is below plan minimum %s", minOut, r.quotedOut, r.slippageBPS, p.MinOutput))
	} else {
		c.note(CheckMinOutput, fmt.Sprintf("route minimum out %s >= plan minimum %s", minOut, p.MinOutput))
	}
}

// minimumOut is the guaranteed minimum output of an ExactIn route.
func minimumOut(quotedOut uint64, slippageBPS uint16) money.Quantity {
	if slippageBPS >= uint16(money.OneHundredPercent) {
		return money.QuantityFromInt64(0)
	}
	q := quantityU64(quotedOut)
	return q.MulBPS(money.OneHundredPercent-money.BPS(slippageBPS), money.RoundDown)
}

func quantityU64(v uint64) money.Quantity {
	return money.QuantityFromBigInt(new(big.Int).SetUint64(v))
}

// checkComputeBudget evaluates COMPUTE_BUDGET.
func checkComputeBudget(c *collector, p *parsedExpectations, w *walk, totalIxs int) {
	if w.cuLimitCount > 1 || w.cuPriceCount > 1 {
		c.fail(CheckComputeBudget, ReasonComputeBudgetDuplicate, fmt.Sprintf("%d SetComputeUnitLimit and %d SetComputeUnitPrice instructions", w.cuLimitCount, w.cuPriceCount))
	}
	var limit uint64
	if w.cuLimit != nil {
		limit = uint64(*w.cuLimit)
	} else {
		n := w.nonBudgetIxs
		if n == 0 {
			n = totalIxs
		}
		limit = uint64(n) * DefaultComputeUnitsPerInstruction //nolint:gosec // n is a small non-negative instruction count
		if limit > MaxComputeUnitLimit {
			limit = MaxComputeUnitLimit
		}
	}
	w.effectiveCUs = limit
	if limit > uint64(p.MaxComputeUnits) {
		c.fail(CheckComputeBudget, ReasonComputeUnitsExceeded, fmt.Sprintf("compute unit limit %d exceeds bound %d", limit, p.MaxComputeUnits))
	}
	fee := new(big.Int)
	if w.cuPrice != nil {
		// ceil(limit × price / 1e6)
		fee.Mul(new(big.Int).SetUint64(limit), new(big.Int).SetUint64(*w.cuPrice))
		fee.Add(fee, big.NewInt(MicroLamportsPerLamport-1))
		fee.Quo(fee, big.NewInt(MicroLamportsPerLamport))
	}
	w.priorityFee = fee
	if fee.Cmp(new(big.Int).SetUint64(p.MaxPriorityFeeLamports)) > 0 {
		c.fail(CheckComputeBudget, ReasonPriorityFeeExceeded, fmt.Sprintf("priority fee %s lamports exceeds bound %d", fee, p.MaxPriorityFeeLamports))
	} else {
		c.note(CheckComputeBudget, fmt.Sprintf("compute units %d, priority fee %s lamports", limit, fee))
	}
}

// checkBlockhash evaluates BLOCKHASH.
func checkBlockhash(c *collector, tx DecodedTransaction, p *parsedExpectations) {
	if !tx.RecentBlockhash.Equals(p.blockhash) {
		c.fail(CheckBlockhash, ReasonBlockhashMismatch, fmt.Sprintf("recent blockhash %s, expected %s", tx.RecentBlockhash, p.RecentBlockhash))
	}
	need := p.CurrentBlockHeight + p.BlockHeightMargin
	if need < p.CurrentBlockHeight || p.LastValidBlockHeight < need {
		c.fail(CheckBlockhash, ReasonBlockhashExpired, fmt.Sprintf("last valid block height %d is not at least %d + margin %d", p.LastValidBlockHeight, p.CurrentBlockHeight, p.BlockHeightMargin))
	}
}

// checkPlanIdentity evaluates PLAN_IDENTITY.
func checkPlanIdentity(c *collector, p *parsedExpectations) {
	if !bytes.Equal(p.PlanHash, p.Claimed.PlanHash) {
		c.fail(CheckPlanIdentity, ReasonPlanHashMismatch, fmt.Sprintf("claimed plan hash %x differs from the approved plan hash %x", p.Claimed.PlanHash, p.PlanHash))
	}
	if p.QuoteID != p.Claimed.QuoteID {
		c.fail(CheckPlanIdentity, ReasonQuoteIDMismatch, fmt.Sprintf("claimed quote %q differs from the plan's quote %q", p.Claimed.QuoteID, p.QuoteID))
	}
}

// checkSimulation evaluates SIMULATION, and feeds inner program ids into
// PROGRAM_ALLOWLIST / NO_ARBITRARY_CPI.
func checkSimulation(c *collector, tx DecodedTransaction, p *parsedExpectations, w *walk) {
	sim := p.Simulation
	if sim == nil {
		if p.RequireSimulation {
			c.fail(CheckSimulation, ReasonSimulationMissing, "no simulation result")
		} else {
			c.note(CheckSimulation, "simulation not required")
		}
		return
	}
	if !sim.Succeeded {
		c.fail(CheckSimulation, ReasonSimulationFailed, "simulation failed: "+sim.Error)
		return
	}
	if len(sim.TxHash) > 0 && !bytes.Equal(sim.TxHash, tx.Hash[:]) {
		c.fail(CheckSimulation, ReasonSimulationMismatch, "simulation was run on different transaction bytes")
		return
	}
	for _, s := range sim.InnerProgramIDs {
		k, err := solana.PublicKeyFromBase58(s)
		if err != nil || !p.isAllowed(k) {
			c.fail(CheckProgramAllowlist, ReasonProgramNotAllowed, fmt.Sprintf("inner program %s is not allowlisted", s))
			c.fail(CheckNoArbitraryCPI, ReasonProgramNotAllowed, fmt.Sprintf("inner program %s is not allowlisted", s))
		}
	}
	// Allowed native debit: base fee, priority fee, ATA rent, and the wrap
	// when the input is SOL.
	sigs := int64(tx.NumSignatures)
	if sigs < 1 {
		sigs = 1
	}
	fees := new(big.Int).SetInt64(sigs * LamportsPerSignature)
	fees.Add(fees, w.priorityFee)
	fees.Add(fees, big.NewInt(int64(w.ataCreates)*TokenAccountRentLamports))
	allowedNative := money.QuantityFromBigInt(fees)
	if p.InputIsNative {
		allowedNative = allowedNative.Add(p.MaxInputDebit)
	}
	var sawInput, sawOutput, sawNative bool
	for _, d := range sim.TokenDeltas {
		if d.Owner != p.walletLabel {
			continue
		}
		switch d.Mint {
		case p.inputMintLabel:
			sawInput = true
			if d.Delta.Neg().Cmp(p.MaxInputDebit) > 0 {
				c.fail(CheckSimulation, ReasonSimulationDelta, fmt.Sprintf("predicted input debit %s exceeds bound %s", d.Delta.Neg(), p.MaxInputDebit))
			}
		case p.outputMintLabel:
			sawOutput = true
			if d.Delta.Cmp(p.MinOutput) < 0 {
				c.fail(CheckSimulation, ReasonSimulationDelta, fmt.Sprintf("predicted output credit %s is below plan minimum %s", d.Delta, p.MinOutput))
			}
		default:
			if d.Delta.IsNegative() {
				c.fail(CheckSimulation, ReasonSimulationDelta, fmt.Sprintf("predicted debit %s of unexpected mint %s", d.Delta.Neg(), d.Mint))
			}
		}
	}
	for _, d := range sim.NativeDeltas {
		if d.Account != p.walletLabel {
			continue
		}
		sawNative = true
		delta := money.QuantityFromInt64(d.Lamports)
		if delta.Neg().Cmp(allowedNative) > 0 {
			c.fail(CheckSimulation, ReasonSimulationDelta, fmt.Sprintf("predicted lamport debit %s exceeds allowed %s", delta.Neg(), allowedNative))
		}
		if p.OutputIsNative && delta.Cmp(p.MinOutput.Sub(allowedNative)) < 0 {
			c.fail(CheckSimulation, ReasonSimulationDelta, fmt.Sprintf("predicted lamport credit %s is below plan minimum %s less allowed fees %s", delta, p.MinOutput, allowedNative))
		}
	}
	if !sawInput && !p.InputIsNative {
		c.fail(CheckSimulation, ReasonSimulationDelta, "no predicted balance change for the input mint")
	}
	if !sawOutput && !p.OutputIsNative {
		c.fail(CheckSimulation, ReasonSimulationDelta, "no predicted balance change for the output mint")
	}
	if (p.InputIsNative || p.OutputIsNative) && !sawNative {
		c.fail(CheckSimulation, ReasonSimulationDelta, "no predicted lamport change for the wallet on a native leg")
	}
	if len(c.fails[CheckSimulation]) == 0 {
		c.note(CheckSimulation, fmt.Sprintf("simulation ok, %d inner programs, %d token deltas", len(sim.InnerProgramIDs), len(sim.TokenDeltas)))
	}
}
