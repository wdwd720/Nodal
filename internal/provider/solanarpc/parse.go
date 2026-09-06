package solanarpc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/money"
)

// Stamp carries the provenance every parsed observation is stamped with.
type Stamp struct {
	Source     string
	RawRef     string
	ObservedAt time.Time
	ReceivedAt time.Time
}

func (s Stamp) apply(o *chain.TxObservation) {
	o.Source, o.RawRef, o.ObservedAt, o.ReceivedAt = s.Source, s.RawRef, s.ObservedAt, s.ReceivedAt
}

// ParseTransaction parses a getTransaction result (the JSON value under
// "result", null included) into a TxObservation for sig at the commitment the
// read was made at. It never panics on any input and rejects every shape
// deviation as VALIDATION_FAILED. Found == false for a null result.
func ParseTransaction(result []byte, sig string, commitment chain.Commitment, st Stamp) (chain.TxObservation, error) {
	obs := chain.TxObservation{Signature: sig}
	st.apply(&obs)
	if isNull(result) {
		return obs, nil
	}
	var resp txResponse
	if err := DecodeStrict(result, &resp); err != nil {
		return chain.TxObservation{}, err
	}
	slot, err := parseU64(resp.Slot, "slot")
	if err != nil {
		return chain.TxObservation{}, err
	}
	if resp.BlockTime != nil {
		bt, err := parseI64(*resp.BlockTime, "blockTime")
		if err != nil {
			return chain.TxObservation{}, err
		}
		t := time.Unix(bt, 0).UTC()
		obs.BlockTime = &t
	}
	version, err := parseVersion(resp.Version)
	if err != nil {
		return chain.TxObservation{}, err
	}
	if resp.Meta == nil {
		return chain.TxObservation{}, Malformed("meta is null")
	}
	if isNull(resp.Transaction) {
		return chain.TxObservation{}, Malformed("transaction is null")
	}
	if resp.Transaction[0] != '{' {
		return chain.TxObservation{}, Malformed("transaction is not a json-encoded object (encoding must be json)")
	}
	var tx txMessage
	if err := DecodeStrict(resp.Transaction, &tx); err != nil {
		return chain.TxObservation{}, err
	}
	if len(tx.Signatures) == 0 || tx.Signatures[0] != sig {
		return chain.TxObservation{}, Malformed("first signature does not match the requested signature")
	}
	keys := make([]string, 0, len(tx.Message.AccountKeys))
	for _, k := range tx.Message.AccountKeys {
		if k == "" {
			return chain.TxObservation{}, Malformed("empty account key")
		}
		keys = append(keys, string(k))
	}
	if len(keys) == 0 {
		return chain.TxObservation{}, Malformed("no account keys")
	}
	if resp.Meta.LoadedAddresses != nil {
		keys = append(keys, resp.Meta.LoadedAddresses.Writable...)
		keys = append(keys, resp.Meta.LoadedAddresses.Readonly...)
	}
	metaErr, err := canonicalErr(resp.Meta.Err)
	if err != nil {
		return chain.TxObservation{}, err
	}
	fee, err := parseLamports(resp.Meta.Fee, "meta.fee")
	if err != nil {
		return chain.TxObservation{}, err
	}
	lamports, err := LamportDeltas(resp.Meta.PreBalances, resp.Meta.PostBalances, keys)
	if err != nil {
		return chain.TxObservation{}, err
	}
	tokens, err := TokenDeltas(resp.Meta.PreTokenBalances, resp.Meta.PostTokenBalances, keys)
	if err != nil {
		return chain.TxObservation{}, err
	}
	obs.Found = true
	obs.Slot = slot
	obs.Commitment = commitment
	obs.Err = metaErr
	obs.Version = version
	obs.AccountKeys = keys
	obs.TokenBalanceDeltas = tokens
	obs.LamportDeltas = lamports
	obs.Fee = fee
	return obs, nil
}

// parseVersion accepts "legacy", a number (0) or absence (legacy per docs).
func parseVersion(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return chain.VersionLegacy, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != chain.VersionLegacy {
			return "", Malformed("unknown transaction version %q", s)
		}
		return chain.VersionLegacy, nil
	}
	var n json.Number
	if err := DecodeStrict(raw, &n); err != nil {
		return "", Malformed("version is neither a string nor a number")
	}
	v, err := parseU64(n, "version")
	if err != nil {
		return "", err
	}
	if v > 1 {
		return "", Malformed("unsupported transaction version %d", v)
	}
	return strconv.FormatUint(v, 10), nil
}

// LamportDeltas pairs preBalances and postBalances by account index and
// returns the non-zero changes. Both arrays must have exactly one entry per
// resolved account key.
func LamportDeltas(pre, post []json.Number, keys []string) ([]chain.LamportDelta, error) {
	if len(pre) != len(post) {
		return nil, Malformed("preBalances (%d) and postBalances (%d) differ in length", len(pre), len(post))
	}
	if len(pre) != len(keys) {
		return nil, Malformed("balances (%d) do not match account keys (%d)", len(pre), len(keys))
	}
	var out []chain.LamportDelta
	for i := range pre {
		p, err := parseLamports(pre[i], fmt.Sprintf("preBalances[%d]", i))
		if err != nil {
			return nil, err
		}
		q, err := parseLamports(post[i], fmt.Sprintf("postBalances[%d]", i))
		if err != nil {
			return nil, err
		}
		if p.Equal(q) {
			continue
		}
		out = append(out, chain.LamportDelta{Account: keys[i], Pre: p, Post: q})
	}
	chain.SortLamportDeltas(out)
	return out, nil
}

type parsedTokenBalance struct {
	index    uint64
	mint     string
	owner    string
	program  string
	amount   money.Quantity
	decimals uint8
}

func parseTokenBalance(tb tokenBalance, field string, nkeys int) (parsedTokenBalance, error) {
	idx, err := parseU64(tb.AccountIndex, field+".accountIndex")
	if err != nil {
		return parsedTokenBalance{}, err
	}
	if idx >= uint64(nkeys) { //nolint:gosec // G115: nkeys is a slice length, never negative
		return parsedTokenBalance{}, Malformed("%s.accountIndex %d out of range (%d keys)", field, idx, nkeys)
	}
	if tb.Mint == "" {
		return parsedTokenBalance{}, Malformed("%s.mint missing", field)
	}
	amt, err := tb.UITokenAmount.Amount.Quantity()
	if err != nil {
		return parsedTokenBalance{}, Malformed("%s.uiTokenAmount.amount: %v", field, err)
	}
	if amt.IsNegative() {
		return parsedTokenBalance{}, Malformed("%s.uiTokenAmount.amount is negative", field)
	}
	dec, err := parseU8(tb.UITokenAmount.Decimals, field+".uiTokenAmount.decimals")
	if err != nil {
		return parsedTokenBalance{}, err
	}
	return parsedTokenBalance{index: idx, mint: tb.Mint, owner: tb.Owner, program: tb.ProgramID, amount: amt, decimals: dec}, nil
}

// TokenDeltas pairs preTokenBalances and postTokenBalances by account index.
// An account present only after the transaction was created (pre = 0); one
// present only before was closed (post = 0). Mint or decimals changing for
// the same account is malformed.
func TokenDeltas(pre, post []tokenBalance, keys []string) ([]chain.TokenDelta, error) {
	type pair struct {
		pre, post *parsedTokenBalance
	}
	pairs := map[uint64]*pair{}
	for i, tb := range pre {
		p, err := parseTokenBalance(tb, fmt.Sprintf("preTokenBalances[%d]", i), len(keys))
		if err != nil {
			return nil, err
		}
		if _, dup := pairs[p.index]; dup {
			return nil, Malformed("preTokenBalances: duplicate accountIndex %d", p.index)
		}
		pc := p
		pairs[p.index] = &pair{pre: &pc}
	}
	for i, tb := range post {
		p, err := parseTokenBalance(tb, fmt.Sprintf("postTokenBalances[%d]", i), len(keys))
		if err != nil {
			return nil, err
		}
		pr := pairs[p.index]
		if pr == nil {
			pr = &pair{}
			pairs[p.index] = pr
		}
		if pr.post != nil {
			return nil, Malformed("postTokenBalances: duplicate accountIndex %d", p.index)
		}
		pc := p
		pr.post = &pc
	}
	zero := money.QuantityFromInt64(0)
	out := make([]chain.TokenDelta, 0, len(pairs))
	for idx, pr := range pairs {
		d := chain.TokenDelta{TokenAccount: keys[idx], Pre: zero, Post: zero}
		switch {
		case pr.pre != nil && pr.post != nil:
			if pr.pre.mint != pr.post.mint {
				return nil, Malformed("accountIndex %d changes mint", idx)
			}
			if pr.pre.decimals != pr.post.decimals {
				return nil, Malformed("accountIndex %d changes decimals", idx)
			}
			d.Mint, d.Decimals, d.Program = pr.post.mint, pr.post.decimals, pr.post.program
			d.Owner = pr.post.owner
			if d.Owner == "" {
				d.Owner = pr.pre.owner
			}
			d.Pre, d.Post = pr.pre.amount, pr.post.amount
		case pr.pre != nil:
			d.Mint, d.Decimals, d.Program, d.Owner = pr.pre.mint, pr.pre.decimals, pr.pre.program, pr.pre.owner
			d.Pre = pr.pre.amount
		default:
			d.Mint, d.Decimals, d.Program, d.Owner = pr.post.mint, pr.post.decimals, pr.post.program, pr.post.owner
			d.Post = pr.post.amount
		}
		out = append(out, d)
	}
	chain.SortTokenDeltas(out)
	return out, nil
}

// ParseSignatureStatuses parses a getSignatureStatuses result for sigs.
func ParseSignatureStatuses(result []byte, sigs []string) ([]chain.SignatureStatus, error) {
	var wrapped contextual
	if err := DecodeStrict(result, &wrapped); err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := DecodeStrict(wrapped.Value, &items); err != nil {
		return nil, err
	}
	if len(items) != len(sigs) {
		return nil, Malformed("getSignatureStatuses returned %d entries for %d signatures", len(items), len(sigs))
	}
	out := make([]chain.SignatureStatus, 0, len(sigs))
	for i, raw := range items {
		st := chain.SignatureStatus{Signature: sigs[i]}
		if isNull(raw) {
			out = append(out, st)
			continue
		}
		var v signatureStatus
		if err := DecodeStrict(raw, &v); err != nil {
			return nil, err
		}
		slot, err := parseU64(v.Slot, fmt.Sprintf("value[%d].slot", i))
		if err != nil {
			return nil, err
		}
		st.Found, st.Slot = true, slot
		if v.Confirmations != nil {
			c, err := parseU64(*v.Confirmations, fmt.Sprintf("value[%d].confirmations", i))
			if err != nil {
				return nil, err
			}
			st.Confirmations = &c
		}
		if v.ConfirmationStatus != "" {
			cm, err := chain.ParseCommitment(v.ConfirmationStatus)
			if err != nil {
				return nil, Malformed("value[%d].confirmationStatus: %v", i, err)
			}
			st.Commitment = cm
		} else if v.Confirmations == nil {
			st.Commitment = chain.CommitmentFinalized // null confirmations = rooted (documented)
		}
		st.Err, err = canonicalErr(v.Err)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// SignatureInfo is one getSignaturesForAddress item after parsing.
type SignatureInfo struct {
	Signature  string
	Slot       uint64
	BlockTime  *time.Time
	Err        string
	Commitment chain.Commitment
}

// ParseSignaturesForAddress parses a getSignaturesForAddress result
// (newest first, as returned).
func ParseSignaturesForAddress(result []byte) ([]SignatureInfo, error) {
	var items []signatureInfo
	if err := DecodeStrict(result, &items); err != nil {
		return nil, err
	}
	out := make([]SignatureInfo, 0, len(items))
	for i, it := range items {
		if err := ValidateSignature(it.Signature); err != nil {
			return nil, Malformed("result[%d].signature invalid", i)
		}
		slot, err := parseU64(it.Slot, fmt.Sprintf("result[%d].slot", i))
		if err != nil {
			return nil, err
		}
		si := SignatureInfo{Signature: it.Signature, Slot: slot}
		if it.BlockTime != nil {
			bt, err := parseI64(*it.BlockTime, fmt.Sprintf("result[%d].blockTime", i))
			if err != nil {
				return nil, err
			}
			t := time.Unix(bt, 0).UTC()
			si.BlockTime = &t
		}
		if it.ConfirmationStatus != "" {
			cm, err := chain.ParseCommitment(it.ConfirmationStatus)
			if err != nil {
				return nil, Malformed("result[%d].confirmationStatus: %v", i, err)
			}
			si.Commitment = cm
		}
		if si.Err, err = canonicalErr(it.Err); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, nil
}

// ParseTokenAccounts parses a jsonParsed getTokenAccountsByOwner result
// for owner. Accounts whose parsed owner differs from owner are rejected.
func ParseTokenAccounts(result []byte, owner string) ([]chain.BalanceObservation, uint64, error) {
	var wrapped contextual
	if err := DecodeStrict(result, &wrapped); err != nil {
		return nil, 0, err
	}
	slot, err := parseU64(wrapped.Context.Slot, "context.slot")
	if err != nil {
		return nil, 0, err
	}
	var items []tokenAccount
	if err := DecodeStrict(wrapped.Value, &items); err != nil {
		return nil, 0, err
	}
	out := make([]chain.BalanceObservation, 0, len(items))
	for i, it := range items {
		f := fmt.Sprintf("value[%d]", i)
		if it.Pubkey == "" {
			return nil, 0, Malformed("%s.pubkey missing", f)
		}
		info := it.Account.Data.Parsed.Info
		if it.Account.Data.Parsed.Type != "account" {
			return nil, 0, Malformed("%s is not a parsed token account (type %q)", f, it.Account.Data.Parsed.Type)
		}
		if info.Owner != owner {
			return nil, 0, Malformed("%s owner mismatch", f)
		}
		if info.Mint == "" {
			return nil, 0, Malformed("%s.mint missing", f)
		}
		amt, err := info.TokenAmount.Amount.Quantity()
		if err != nil {
			return nil, 0, Malformed("%s.tokenAmount.amount: %v", f, err)
		}
		if amt.IsNegative() {
			return nil, 0, Malformed("%s.tokenAmount.amount is negative", f)
		}
		dec, err := parseU8(info.TokenAmount.Decimals, f+".tokenAmount.decimals")
		if err != nil {
			return nil, 0, err
		}
		out = append(out, chain.BalanceObservation{
			Owner: owner, Mint: info.Mint, TokenAccount: it.Pubkey, Program: it.Account.Owner,
			Amount: amt, Decimals: dec, DecimalsKnown: true, Slot: slot,
		})
	}
	chain.SortBalances(out)
	return out, slot, nil
}

// ParseBlockhash parses a getLatestBlockhash result.
func ParseBlockhash(result []byte) (chain.Blockhash, error) {
	var wrapped contextual
	if err := DecodeStrict(result, &wrapped); err != nil {
		return chain.Blockhash{}, err
	}
	slot, err := parseU64(wrapped.Context.Slot, "context.slot")
	if err != nil {
		return chain.Blockhash{}, err
	}
	var v blockhashValue
	if err := DecodeStrict(wrapped.Value, &v); err != nil {
		return chain.Blockhash{}, err
	}
	if err := ValidateBlockhash(v.Blockhash); err != nil {
		return chain.Blockhash{}, Malformed("value.blockhash invalid")
	}
	lvbh, err := parseU64(v.LastValidBlockHeight, "value.lastValidBlockHeight")
	if err != nil {
		return chain.Blockhash{}, err
	}
	return chain.Blockhash{Blockhash: v.Blockhash, LastValidBlockHeight: lvbh, Slot: slot}, nil
}

// ParseSimulation parses a simulateTransaction result.
func ParseSimulation(result []byte, opts chain.SimulateOptions) (chain.SimulationResult, error) {
	var wrapped contextual
	if err := DecodeStrict(result, &wrapped); err != nil {
		return chain.SimulationResult{}, err
	}
	var v simulationValue
	if err := DecodeStrict(wrapped.Value, &v); err != nil {
		return chain.SimulationResult{}, err
	}
	out := chain.SimulationResult{Logs: v.Logs}
	simErr, err := canonicalErr(v.Err)
	if err != nil {
		return chain.SimulationResult{}, err
	}
	out.Err = simErr
	out.OK = simErr == ""
	if v.UnitsConsumed != nil {
		if out.UnitsConsumed, err = parseU64(*v.UnitsConsumed, "value.unitsConsumed"); err != nil {
			return chain.SimulationResult{}, err
		}
	}
	if v.ReplacementBlockhash != nil {
		lvbh, err := parseU64(v.ReplacementBlockhash.LastValidBlockHeight, "value.replacementBlockhash.lastValidBlockHeight")
		if err != nil {
			return chain.SimulationResult{}, err
		}
		out.ReplacementBlockhash = &chain.Blockhash{Blockhash: v.ReplacementBlockhash.Blockhash, LastValidBlockHeight: lvbh}
	}
	// Inner programs: resolve indexes through the caller's keys; unresolved
	// indexes are reported explicitly so inspectors fail closed.
	seen := map[string]bool{}
	for _, set := range v.InnerInstructions {
		for _, ins := range set.Instructions {
			var id string
			switch {
			case ins.ProgramID != "":
				id = ins.ProgramID
			case ins.ProgramIDIndex != nil:
				idx, err := parseU64(*ins.ProgramIDIndex, "innerInstructions.programIdIndex")
				if err != nil {
					return chain.SimulationResult{}, err
				}
				if idx < uint64(len(opts.AccountKeys)) {
					id = opts.AccountKeys[idx]
				} else {
					id = "unresolved:" + strconv.FormatUint(idx, 10)
				}
			default:
				return chain.SimulationResult{}, Malformed("inner instruction without program id")
			}
			seen[id] = true
		}
	}
	for id := range seen {
		out.InnerProgramIDs = append(out.InnerProgramIDs, id)
	}
	sort.Strings(out.InnerProgramIDs)
	// Post-state accounts, paired with ReturnAccounts by position.
	if len(v.Accounts) > 0 {
		if len(v.Accounts) != len(opts.ReturnAccounts) {
			return chain.SimulationResult{}, Malformed("value.accounts has %d entries for %d requested", len(v.Accounts), len(opts.ReturnAccounts))
		}
		pre := map[string]chain.BalanceObservation{}
		for _, b := range opts.PreBalances {
			pre[b.TokenAccount] = b
		}
		for i, raw := range v.Accounts {
			if isNull(raw) {
				continue
			}
			var acc simulatedAccount
			if err := DecodeStrict(raw, &acc); err != nil {
				return chain.SimulationResult{}, err
			}
			if acc.Data.Parsed.Type != "account" {
				continue // not a token account: no balance to report
			}
			info := acc.Data.Parsed.Info
			amt, err := info.TokenAmount.Amount.Quantity()
			if err != nil {
				return chain.SimulationResult{}, Malformed("value.accounts[%d].tokenAmount.amount: %v", i, err)
			}
			dec, err := parseU8(info.TokenAmount.Decimals, fmt.Sprintf("value.accounts[%d].tokenAmount.decimals", i))
			if err != nil {
				return chain.SimulationResult{}, err
			}
			post := chain.BalanceObservation{
				Owner: info.Owner, Mint: info.Mint, TokenAccount: opts.ReturnAccounts[i], Program: acc.Owner,
				Amount: amt, Decimals: dec, DecimalsKnown: true,
			}
			out.PostBalances = append(out.PostBalances, post)
			if pb, ok := pre[post.TokenAccount]; ok {
				if pb.Mint != "" && pb.Mint != post.Mint {
					return chain.SimulationResult{}, Malformed("value.accounts[%d] mint differs from pre-balance", i)
				}
				out.PredictedTokenDeltas = append(out.PredictedTokenDeltas, chain.TokenDelta{
					Owner: post.Owner, Mint: post.Mint, TokenAccount: post.TokenAccount, Program: post.Program,
					Pre: pb.Amount, Post: post.Amount, Decimals: dec,
				})
			}
		}
		chain.SortBalances(out.PostBalances)
		chain.SortTokenDeltas(out.PredictedTokenDeltas)
	}
	return out, nil
}

// ParseU64Value parses a {context, value: u64} result (getBalance,
// getBlockHeight when wrapped) or a bare u64.
func ParseU64Value(result []byte) (value, slot uint64, err error) {
	trimmed := result
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\n' || trimmed[0] == '\t' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var wrapped contextual
		if err := DecodeStrict(result, &wrapped); err != nil {
			return 0, 0, err
		}
		s, err := parseU64(wrapped.Context.Slot, "context.slot")
		if err != nil {
			return 0, 0, err
		}
		var n json.Number
		if err := DecodeStrict(wrapped.Value, &n); err != nil {
			return 0, 0, err
		}
		v, err := parseU64(n, "value")
		if err != nil {
			return 0, 0, err
		}
		return v, s, nil
	}
	var n json.Number
	if err := DecodeStrict(result, &n); err != nil {
		return 0, 0, err
	}
	v, err := parseU64(n, "result")
	if err != nil {
		return 0, 0, err
	}
	return v, 0, nil
}

// ParseBoolValue parses a {context, value: bool} result.
func ParseBoolValue(result []byte) (value bool, slot uint64, err error) {
	var wrapped contextual
	if err := DecodeStrict(result, &wrapped); err != nil {
		return false, 0, err
	}
	s, err := parseU64(wrapped.Context.Slot, "context.slot")
	if err != nil {
		return false, 0, err
	}
	var b bool
	if err := DecodeStrict(wrapped.Value, &b); err != nil {
		return false, 0, err
	}
	return b, s, nil
}
