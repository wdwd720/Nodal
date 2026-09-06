package solanarpc_test

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

var stamp = solanarpc.Stamp{Source: "rpc-fallback", RawRef: "mem://x", ObservedAt: time.Unix(1, 0).UTC(), ReceivedAt: time.Unix(2, 0).UTC()}

func TestParseTransaction_V0WithTokenBalances(t *testing.T) {
	t.Parallel()
	obs, err := solanarpc.ParseTransaction(validTx().bytes(t), sigA, chain.CommitmentConfirmed, stamp)
	require.NoError(t, err)
	require.True(t, obs.Found)
	require.Equal(t, sigA, obs.Signature)
	require.Equal(t, uint64(250000123), obs.Slot)
	require.Equal(t, time.Unix(1757073600, 0).UTC(), *obs.BlockTime)
	require.Equal(t, chain.CommitmentConfirmed, obs.Commitment)
	require.Equal(t, chain.VersionV0, obs.Version)
	require.Equal(t, "", obs.Err)
	require.Equal(t, "5000", obs.Fee.String())
	require.Equal(t, []string{wallet, ataUSDC, ataBONK, jupiter, poolAcct, lutRO}, obs.AccountKeys, "static keys then lookup-table writable then readonly")
	require.Equal(t, "rpc-fallback", obs.Source)
	require.Equal(t, "mem://x", obs.RawRef)
	require.Equal(t, stamp.ObservedAt, obs.ObservedAt)

	require.Len(t, obs.TokenBalanceDeltas, 3)
	byAcct := map[string]chain.TokenDelta{}
	for _, d := range obs.TokenBalanceDeltas {
		byAcct[d.TokenAccount] = d
	}
	usdc := byAcct[ataUSDC]
	require.Equal(t, wallet, usdc.Owner)
	require.Equal(t, mintUSDC, usdc.Mint)
	require.Equal(t, "-1000000", usdc.Delta().String())
	require.Equal(t, uint8(6), usdc.Decimals)
	require.Equal(t, tokenPID, usdc.Program)
	bonk := byAcct[ataBONK]
	require.Equal(t, "0", bonk.Pre.String(), "account created in the transaction")
	require.Equal(t, "123456789", bonk.Post.String())
	require.Equal(t, uint8(5), bonk.Decimals)
	pool := byAcct[poolAcct]
	require.Equal(t, "1000", pool.Delta().String(), "lookup-table account resolved by index")
	require.Equal(t, poolAcct, pool.Owner)

	require.Len(t, obs.LamportDeltas, 2, "only non-zero lamport changes")
	lamports := map[string]string{}
	for _, d := range obs.LamportDeltas {
		lamports[d.Account] = d.Delta().String()
	}
	require.Equal(t, "-2044280", lamports[wallet], "fee plus rent for the new token account")
	require.Equal(t, "2039280", lamports[ataBONK])
	require.Less(t, obs.LamportDeltas[0].Account, obs.LamportDeltas[1].Account, "canonical order")
	require.Equal(t, "-5000", chain.SumLamportDeltas(obs.LamportDeltas).String(), "lamports are conserved except the fee")
	require.True(t, obs.Touches(wallet))
}

func TestParseTransaction_NotFoundAndLegacy(t *testing.T) {
	t.Parallel()
	obs, err := solanarpc.ParseTransaction([]byte("null"), sigA, chain.CommitmentConfirmed, stamp)
	require.NoError(t, err)
	require.False(t, obs.Found)
	require.Equal(t, sigA, obs.Signature)
	require.Equal(t, "rpc-fallback", obs.Source)
	obs, err = solanarpc.ParseTransaction(nil, sigA, chain.CommitmentConfirmed, stamp)
	require.NoError(t, err)
	require.False(t, obs.Found)

	d := validTx()
	delete(d, "version")
	delete(d.meta(), "loadedAddresses")
	d.meta()["preBalances"] = []json.Number{"10", "0", "0", "1"}
	d.meta()["postBalances"] = []json.Number{"5", "0", "0", "1"}
	d.meta()["fee"] = json.Number("5")
	d.meta()["preTokenBalances"] = []any{}
	d.meta()["postTokenBalances"] = nil
	obs, err = solanarpc.ParseTransaction(d.bytes(t), sigA, chain.CommitmentFinalized, stamp)
	require.NoError(t, err)
	require.Equal(t, chain.VersionLegacy, obs.Version)
	require.Empty(t, obs.TokenBalanceDeltas)
	require.Len(t, obs.LamportDeltas, 1)
	d["version"] = "legacy"
	obs, err = solanarpc.ParseTransaction(d.bytes(t), sigA, chain.CommitmentFinalized, stamp)
	require.NoError(t, err)
	require.Equal(t, chain.VersionLegacy, obs.Version)

	// jsonParsed account keys are accepted too.
	d = validTx()
	d.msg()["accountKeys"] = []any{
		map[string]any{"pubkey": wallet, "signer": true, "writable": true, "source": "transaction"},
		map[string]any{"pubkey": ataUSDC},
		map[string]any{"pubkey": ataBONK},
		map[string]any{"pubkey": jupiter},
	}
	obs, err = solanarpc.ParseTransaction(d.bytes(t), sigA, chain.CommitmentConfirmed, stamp)
	require.NoError(t, err)
	require.Equal(t, wallet, obs.AccountKeys[0])
}

func TestParseTransaction_ErrShapes(t *testing.T) {
	t.Parallel()
	d := validTx()
	d.meta()["err"] = "BlockhashNotFound"
	obs, err := solanarpc.ParseTransaction(d.bytes(t), sigA, chain.CommitmentConfirmed, stamp)
	require.NoError(t, err)
	require.Equal(t, "BlockhashNotFound", obs.Err)
	require.False(t, obs.Succeeded())

	d.meta()["err"] = map[string]any{"InstructionError": []any{json.Number("2"), map[string]any{"Custom": json.Number("6001")}}}
	obs, err = solanarpc.ParseTransaction(d.bytes(t), sigA, chain.CommitmentConfirmed, stamp)
	require.NoError(t, err)
	require.Equal(t, `{"InstructionError":[2,{"Custom":6001}]}`, obs.Err)
	require.True(t, obs.Found, "a failed transaction is still found: it paid a fee")
}

func TestParseTransaction_Malformed(t *testing.T) {
	t.Parallel()
	cases := map[string]func(d txDoc){
		"float token amount": func(d txDoc) {
			d.amount("postTokenBalances", 1)["amount"] = json.Number("1.5")
		},
		"float token amount string": func(d txDoc) {
			d.amount("postTokenBalances", 1)["amount"] = "1.5"
		},
		"exponent token amount": func(d txDoc) {
			d.amount("postTokenBalances", 1)["amount"] = "1e9"
		},
		"negative token amount": func(d txDoc) {
			d.amount("postTokenBalances", 1)["amount"] = "-5"
		},
		"empty token amount": func(d txDoc) {
			d.amount("postTokenBalances", 1)["amount"] = ""
		},
		"null token amount": func(d txDoc) {
			d.amount("postTokenBalances", 1)["amount"] = nil
		},
		"float fee":                func(d txDoc) { d.meta()["fee"] = json.Number("5000.0") },
		"negative fee":             func(d txDoc) { d.meta()["fee"] = json.Number("-1") },
		"float slot":               func(d txDoc) { d["slot"] = json.Number("1.0") },
		"exponent slot":            func(d txDoc) { d["slot"] = json.Number("1e3") },
		"slot overflow":            func(d txDoc) { d["slot"] = json.Number("18446744073709551616") },
		"float blockTime":          func(d txDoc) { d["blockTime"] = json.Number("1.5") },
		"float lamports":           func(d txDoc) { d.balances("preBalances")[0] = "1.25" },
		"balances length mismatch": func(d txDoc) { d.meta()["postBalances"] = []json.Number{"1"} },
		"balances not matching keys": func(d txDoc) {
			d.meta()["preBalances"] = []json.Number{"1"}
			d.meta()["postBalances"] = []json.Number{"1"}
		},
		"missing meta":        func(d txDoc) { d["meta"] = nil },
		"missing transaction": func(d txDoc) { d["transaction"] = nil },
		"base64 transaction":  func(d txDoc) { d["transaction"] = []string{"AQAB", "base64"} },
		"signature mismatch":  func(d txDoc) { d.tx()["signatures"] = []string{sigB} },
		"no signatures":       func(d txDoc) { d.tx()["signatures"] = []string{} },
		"no account keys": func(d txDoc) {
			d.msg()["accountKeys"] = []string{}
		},
		"empty account key": func(d txDoc) {
			d.msg()["accountKeys"] = []string{"", ataUSDC, ataBONK, jupiter}
		},
		"accountIndex out of range": func(d txDoc) {
			d.token("postTokenBalances", 1)["accountIndex"] = json.Number("99")
		},
		"accountIndex negative": func(d txDoc) {
			d.token("postTokenBalances", 1)["accountIndex"] = json.Number("-1")
		},
		"duplicate accountIndex pre": func(d txDoc) {
			d.meta()["preTokenBalances"] = []any{tokenBal(1, mintUSDC, wallet, "1", 6), tokenBal(1, mintUSDC, wallet, "2", 6)}
		},
		"duplicate accountIndex post": func(d txDoc) {
			d.meta()["postTokenBalances"] = []any{tokenBal(1, mintUSDC, wallet, "1", 6), tokenBal(1, mintUSDC, wallet, "2", 6)}
		},
		"mint changes": func(d txDoc) { d.token("postTokenBalances", 0)["mint"] = mintBONK },
		"decimals change": func(d txDoc) {
			d.amount("postTokenBalances", 0)["decimals"] = json.Number("7")
		},
		"decimals overflow": func(d txDoc) {
			d.amount("postTokenBalances", 1)["decimals"] = json.Number("256")
		},
		"missing mint":                  func(d txDoc) { d.token("postTokenBalances", 1)["mint"] = "" },
		"unknown version string":        func(d txDoc) { d["version"] = "v2" },
		"unsupported version number":    func(d txDoc) { d["version"] = json.Number("2") },
		"version object":                func(d txDoc) { d["version"] = map[string]any{} },
		"empty err string":              func(d txDoc) { d.meta()["err"] = "" },
		"meta is a string":              func(d txDoc) { d["meta"] = "x" },
		"result is an array":            func(d txDoc) { d["slot"] = []any{} },
		"loadedAddresses wrong type":    func(d txDoc) { d.meta()["loadedAddresses"] = map[string]any{"writable": "x"} },
		"preTokenBalances not an array": func(d txDoc) { d.meta()["preTokenBalances"] = map[string]any{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := validTx()
			mutate(d)
			_, err := solanarpc.ParseTransaction(d.bytes(t), sigA, chain.CommitmentConfirmed, stamp)
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), err.Error())
		})
	}
	for _, raw := range []string{"{", "[]", `"str"`, "1", "{} extra", "true"} {
		_, err := solanarpc.ParseTransaction([]byte(raw), sigA, chain.CommitmentConfirmed, stamp)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), raw)
	}
}

func TestParseSignatureStatuses(t *testing.T) {
	t.Parallel()
	body := rawJSON(t, ctxSlot("100", []any{
		nil,
		map[string]any{"slot": json.Number("90"), "confirmations": json.Number("12"), "err": nil, "confirmationStatus": "confirmed", "status": map[string]any{"Ok": nil}},
		map[string]any{"slot": json.Number("80"), "confirmations": nil, "err": map[string]any{"InstructionError": []any{json.Number("0"), "InvalidArgument"}}, "confirmationStatus": "finalized"},
		map[string]any{"slot": json.Number("70"), "confirmations": nil, "err": nil},
	}))
	sts, err := solanarpc.ParseSignatureStatuses(body, []string{sigA, sigB, sigC, sigA})
	require.NoError(t, err)
	require.False(t, sts[0].Found, "null is unknown, never failed")
	require.Equal(t, "", sts[0].Err)
	require.True(t, sts[1].Found)
	require.Equal(t, uint64(12), *sts[1].Confirmations)
	require.Equal(t, chain.CommitmentConfirmed, sts[1].Commitment)
	require.Nil(t, sts[2].Confirmations)
	require.Equal(t, chain.CommitmentFinalized, sts[2].Commitment)
	require.Equal(t, `{"InstructionError":[0,"InvalidArgument"]}`, sts[2].Err)
	require.Equal(t, chain.CommitmentFinalized, sts[3].Commitment, "null confirmations without a status means rooted")

	_, err = solanarpc.ParseSignatureStatuses(body, []string{sigA})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "entry count must match")
	bad := rawJSON(t, ctxSlot("100", []any{map[string]any{"slot": json.Number("1"), "confirmationStatus": "recent"}}))
	_, err = solanarpc.ParseSignatureStatuses(bad, []string{sigA})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	bad = rawJSON(t, ctxSlot("100", []any{map[string]any{"slot": json.Number("1.5")}}))
	_, err = solanarpc.ParseSignatureStatuses(bad, []string{sigA})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestParseSignaturesForAddress(t *testing.T) {
	t.Parallel()
	body := rawJSON(t, []any{
		map[string]any{"signature": sigA, "slot": json.Number("200"), "err": nil, "memo": nil, "blockTime": json.Number("1757073600"), "confirmationStatus": "finalized"},
		map[string]any{"signature": sigB, "slot": json.Number("190"), "err": "BlockhashNotFound", "memo": nil, "blockTime": nil, "confirmationStatus": "confirmed"},
	})
	items, err := solanarpc.ParseSignaturesForAddress(body)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, sigA, items[0].Signature)
	require.Equal(t, uint64(200), items[0].Slot)
	require.Equal(t, time.Unix(1757073600, 0).UTC(), *items[0].BlockTime)
	require.Nil(t, items[1].BlockTime)
	require.Equal(t, "BlockhashNotFound", items[1].Err)
	require.Equal(t, chain.CommitmentConfirmed, items[1].Commitment)

	_, err = solanarpc.ParseSignaturesForAddress(rawJSON(t, []any{map[string]any{"signature": "bad", "slot": json.Number("1")}}))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = solanarpc.ParseSignaturesForAddress([]byte(`{"x":1}`))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestParseTokenAccounts(t *testing.T) {
	t.Parallel()
	body := rawJSON(t, ctxSlot("500", []any{
		parsedTokenAccount(ataBONK, mintBONK, wallet, "123456789", 5),
		parsedTokenAccount(ataUSDC, mintUSDC, wallet, "1000000", 6),
	}))
	bs, slot, err := solanarpc.ParseTokenAccounts(body, wallet)
	require.NoError(t, err)
	require.Equal(t, uint64(500), slot)
	require.Len(t, bs, 2)
	require.Equal(t, mintBONK, bs[0].Mint, "canonical mint order")
	require.Equal(t, "123456789", bs[0].Amount.String())
	require.Equal(t, uint8(5), bs[0].Decimals)
	require.True(t, bs[0].DecimalsKnown)
	require.Equal(t, uint64(500), bs[0].Slot)
	require.Equal(t, tokenPID, bs[0].Program)
	require.Equal(t, wallet, bs[0].Owner)

	_, _, err = solanarpc.ParseTokenAccounts(body, ataBONK)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "owner mismatch")
	bad := rawJSON(t, ctxSlot("500", []any{parsedTokenAccount(ataUSDC, mintUSDC, wallet, "1.5", 6)}))
	_, _, err = solanarpc.ParseTokenAccounts(bad, wallet)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "float amount")
	acct := parsedTokenAccount(ataUSDC, mintUSDC, wallet, "1", 6)
	setPath(acct, "mint", "account", "data", "parsed", "type")
	_, _, err = solanarpc.ParseTokenAccounts(rawJSON(t, ctxSlot("500", []any{acct})), wallet)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestParseBlockhashAndScalars(t *testing.T) {
	t.Parallel()
	b, err := solanarpc.ParseBlockhash(rawJSON(t, ctxSlot("777", map[string]any{"blockhash": hashA, "lastValidBlockHeight": json.Number("300000000")})))
	require.NoError(t, err)
	require.Equal(t, hashA, b.Blockhash)
	require.Equal(t, uint64(300000000), b.LastValidBlockHeight)
	require.Equal(t, uint64(777), b.Slot)
	_, err = solanarpc.ParseBlockhash(rawJSON(t, ctxSlot("777", map[string]any{"blockhash": "!", "lastValidBlockHeight": json.Number("1")})))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = solanarpc.ParseBlockhash(rawJSON(t, ctxSlot("777", map[string]any{"blockhash": hashA, "lastValidBlockHeight": json.Number("1.5")})))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	v, slot, err := solanarpc.ParseU64Value(rawJSON(t, ctxSlot("10", json.Number("123"))))
	require.NoError(t, err)
	require.Equal(t, uint64(123), v)
	require.Equal(t, uint64(10), slot)
	v, slot, err = solanarpc.ParseU64Value([]byte(" 42"))
	require.NoError(t, err)
	require.Equal(t, uint64(42), v)
	require.Equal(t, uint64(0), slot)
	_, _, err = solanarpc.ParseU64Value([]byte("4.2"))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	v, _, err = solanarpc.ParseU64Value(rawJSON(t, ctxSlot("10", "123")))
	require.NoError(t, err, "an integer encoded as a string loses nothing and is accepted")
	require.Equal(t, uint64(123), v)
	_, _, err = solanarpc.ParseU64Value(rawJSON(t, ctxSlot("10", "12.5")))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a fractional string is rejected like a float")
	_, _, err = solanarpc.ParseU64Value(rawJSON(t, ctxSlot("10", "-1")))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	ok, slot, err := solanarpc.ParseBoolValue(rawJSON(t, ctxSlot("11", true)))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint64(11), slot)
	_, _, err = solanarpc.ParseBoolValue(rawJSON(t, ctxSlot("11", "true")))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestParseSimulation(t *testing.T) {
	t.Parallel()
	keys := []string{wallet, ataUSDC, ataBONK, jupiter, poolAcct}
	value := map[string]any{
		"err":           nil,
		"logs":          []string{"Program log: ok"},
		"unitsConsumed": json.Number("145000"),
		"innerInstructions": []any{
			map[string]any{"index": json.Number("0"), "instructions": []any{
				map[string]any{"programIdIndex": json.Number("3"), "accounts": []any{}, "data": "AA=="},
				map[string]any{"programIdIndex": json.Number("9"), "accounts": []any{}, "data": "AA=="},
				map[string]any{"programId": tokenPID, "parsed": map[string]any{}},
			}},
		},
		"accounts": []any{
			parsedTokenAccount(ataUSDC, mintUSDC, wallet, "0", 6)["account"],
			nil,
		},
		"replacementBlockhash": map[string]any{"blockhash": hashA, "lastValidBlockHeight": json.Number("999")},
	}
	opts := chain.SimulateOptions{
		AccountKeys:    keys,
		ReturnAccounts: []string{ataUSDC, ataBONK},
		PreBalances:    []chain.BalanceObservation{{TokenAccount: ataUSDC, Mint: mintUSDC, Amount: money.QuantityFromInt64(1_000_000)}},
	}
	sim, err := solanarpc.ParseSimulation(rawJSON(t, ctxSlot("5", value)), opts)
	require.NoError(t, err)
	require.True(t, sim.OK)
	require.Equal(t, uint64(145000), sim.UnitsConsumed)
	require.Equal(t, []string{jupiter, tokenPID, "unresolved:9"}, sim.InnerProgramIDs, "sorted; unresolved index reported explicitly")
	require.Len(t, sim.PostBalances, 1)
	require.Equal(t, "0", sim.PostBalances[0].Amount.String())
	require.Len(t, sim.PredictedTokenDeltas, 1)
	require.Equal(t, "-1000000", sim.PredictedTokenDeltas[0].Delta().String())
	require.Equal(t, uint64(999), sim.ReplacementBlockhash.LastValidBlockHeight)

	value["err"] = map[string]any{"InstructionError": []any{json.Number("1"), map[string]any{"Custom": json.Number("1")}}}
	value["accounts"] = nil
	sim, err = solanarpc.ParseSimulation(rawJSON(t, ctxSlot("5", value)), opts)
	require.NoError(t, err)
	require.False(t, sim.OK)
	require.Equal(t, `{"InstructionError":[1,{"Custom":1}]}`, sim.Err)

	value["err"] = nil
	value["accounts"] = []any{nil}
	_, err = solanarpc.ParseSimulation(rawJSON(t, ctxSlot("5", value)), opts)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "accounts must match ReturnAccounts")
	value["accounts"] = nil
	value["innerInstructions"] = []any{map[string]any{"index": json.Number("0"), "instructions": []any{map[string]any{"accounts": []any{}}}}}
	_, err = solanarpc.ParseSimulation(rawJSON(t, ctxSlot("5", value)), opts)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "inner instruction without a program")
	value["innerInstructions"] = nil
	value["unitsConsumed"] = json.Number("1.5")
	_, err = solanarpc.ParseSimulation(rawJSON(t, ctxSlot("5", value)), opts)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestValidators(t *testing.T) {
	t.Parallel()
	for _, s := range []string{sigA, sigB, sigC} {
		require.NoError(t, solanarpc.ValidateSignature(s), s)
	}
	for _, k := range []string{wallet, ataUSDC, ataBONK, poolAcct, lutRO, mintUSDC, mintBONK, jupiter, tokenPID} {
		require.NoError(t, solanarpc.ValidatePubkey(k), k)
	}
	require.Error(t, solanarpc.ValidateSignature("0OIl"))
	require.Error(t, solanarpc.ValidateSignature(wallet), "too short for a signature")
	require.NoError(t, solanarpc.ValidatePubkey(wallet))
	require.NoError(t, solanarpc.ValidatePubkey(mintUSDC))
	require.Error(t, solanarpc.ValidatePubkey(sigA), "too long for a pubkey")
	require.Error(t, solanarpc.ValidatePubkey("abc"))
	require.NoError(t, solanarpc.ValidateBlockhash(hashA))
	require.Error(t, solanarpc.ValidateBlockhash(""))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(solanarpc.ValidatePubkey("")))
}

// TestProp_DeltasSumToFee generates random balance vectors that conserve
// lamports except for the fee and checks the parsed lamport deltas sum to
// exactly −fee, and that every token delta is exactly post − pre.
func TestProp_DeltasSumToFee(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 12).Draw(rt, "n")
		keys := make([]string, n)
		for i := range keys {
			keys[i] = "Key" + strconv.Itoa(i) + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
		}
		fee := rapid.Uint64Range(0, 1_000_000).Draw(rt, "fee")
		pre := make([]uint64, n)
		post := make([]uint64, n)
		var total uint64
		for i := range pre {
			pre[i] = rapid.Uint64Range(0, 1<<40).Draw(rt, "pre")
			total += pre[i]
		}
		if total < fee {
			pre[0] += fee
			total += fee
		}
		// Redistribute total − fee across post balances.
		remaining := total - fee
		for i := range post {
			if i == n-1 {
				post[i] = remaining
			} else {
				post[i] = rapid.Uint64Range(0, remaining).Draw(rt, "post")
				remaining -= post[i]
			}
		}
		toNums := func(v []uint64) []json.Number {
			out := make([]json.Number, len(v))
			for i, x := range v {
				out[i] = json.Number(strconv.FormatUint(x, 10))
			}
			return out
		}
		nTok := rapid.IntRange(0, n).Draw(rt, "ntok")
		var preTok, postTok []any
		expected := map[int][2]string{}
		for i := 0; i < nTok; i++ {
			p := rapid.Uint64Range(0, 1<<62).Draw(rt, "tpre")
			q := rapid.Uint64Range(0, 1<<62).Draw(rt, "tpost")
			hasPre := rapid.Bool().Draw(rt, "hasPre")
			hasPost := rapid.Bool().Draw(rt, "hasPost") || !hasPre
			ps, qs := "0", "0"
			if hasPre {
				ps = strconv.FormatUint(p, 10)
				preTok = append(preTok, tokenBal(i, mintUSDC, wallet, ps, 6))
			}
			if hasPost {
				qs = strconv.FormatUint(q, 10)
				postTok = append(postTok, tokenBal(i, mintUSDC, wallet, qs, 6))
			}
			expected[i] = [2]string{ps, qs}
		}
		d := validTx()
		d.msg()["accountKeys"] = keys
		d.meta()["fee"] = json.Number(strconv.FormatUint(fee, 10))
		d.meta()["preBalances"] = toNums(pre)
		d.meta()["postBalances"] = toNums(post)
		d.meta()["preTokenBalances"] = preTok
		d.meta()["postTokenBalances"] = postTok
		delete(d.meta(), "loadedAddresses")
		b, err := json.Marshal(d)
		if err != nil {
			rt.Fatal(err)
		}
		obs, err := solanarpc.ParseTransaction(b, sigA, chain.CommitmentConfirmed, stamp)
		if err != nil {
			rt.Fatalf("parse: %v", err)
		}
		if got := chain.SumLamportDeltas(obs.LamportDeltas).String(); got != "-"+strconv.FormatUint(fee, 10) && (fee != 0 || got != "0") {
			rt.Fatalf("lamport deltas sum %s, fee %d", got, fee)
		}
		if len(obs.TokenBalanceDeltas) != len(expected) {
			rt.Fatalf("token deltas %d, expected %d", len(obs.TokenBalanceDeltas), len(expected))
		}
		for _, td := range obs.TokenBalanceDeltas {
			var idx int
			for i, k := range keys {
				if k == td.TokenAccount {
					idx = i
				}
			}
			want := expected[idx]
			if td.Pre.String() != want[0] || td.Post.String() != want[1] {
				rt.Fatalf("delta %s: got %s→%s want %s→%s", td.TokenAccount, td.Pre, td.Post, want[0], want[1])
			}
			p, _ := money.ParseQuantity(want[0])
			q, _ := money.ParseQuantity(want[1])
			if !td.Delta().Equal(q.Sub(p)) {
				rt.Fatalf("delta arithmetic")
			}
		}
	})
}

// FuzzParseTransactionResponse: the parser never panics and every accepted
// observation satisfies its invariants.
func FuzzParseTransactionResponse(f *testing.F) {
	seeds := [][]byte{[]byte("null"), []byte("{}"), []byte("[]"), []byte(`{"slot":1}`), []byte("{"), []byte(`"x"`)}
	d := validTx()
	b, _ := json.Marshal(d)
	seeds = append(seeds, b)
	d.meta()["err"] = "BlockhashNotFound"
	b, _ = json.Marshal(d)
	seeds = append(seeds, b)
	d = validTx()
	d.amount("postTokenBalances", 1)["amount"] = "1.5"
	b, _ = json.Marshal(d)
	seeds = append(seeds, b)
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		obs, err := solanarpc.ParseTransaction(data, sigA, chain.CommitmentConfirmed, stamp)
		if err != nil {
			if errs.CodeOf(err) != errs.CodeValidationFailed {
				t.Fatalf("unexpected code %s", errs.CodeOf(err))
			}
			return
		}
		if obs.Signature != sigA || obs.Source != "rpc-fallback" {
			t.Fatal("stamp lost")
		}
		if !obs.Found {
			return
		}
		if obs.Fee.IsNegative() || len(obs.AccountKeys) == 0 || obs.Commitment != chain.CommitmentConfirmed {
			t.Fatalf("invariant violated: %+v", obs)
		}
		for _, td := range obs.TokenBalanceDeltas {
			if td.Pre.IsNegative() || td.Post.IsNegative() || td.Mint == "" || td.TokenAccount == "" {
				t.Fatalf("bad token delta %+v", td)
			}
		}
		for _, ld := range obs.LamportDeltas {
			if ld.Pre.Equal(ld.Post) || ld.Account == "" {
				t.Fatalf("bad lamport delta %+v", ld)
			}
		}
	})
}
