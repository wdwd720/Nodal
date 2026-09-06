package solanarpc_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Realistic base58 identifiers (shapes only; nothing here is live data).
const (
	sigA     = "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
	sigB     = "4Wf4kGKv5qLj1Uf7VvNbY5v9F3Zq8bJ5aE4v2n1kY8f3M9p7Qw2tR6sX1cV5bN8mK3jH7gF4dS2aP9oL6iU3yT1e"
	sigC     = "3Kx9vLm2pQ7nR4tY6uJ8oP1aS5dF7gH9jK2mZ4xC6vB8nM1qW3eR5tY7uJ9oP2aS4dF6gH8jK1mZ3xC5vB7nM9qW"
	wallet   = "9aE476sH92Vz7DMPyq5WLPkrKWivxnuXaMLh3Gvy8Bev"
	ataUSDC  = "4kJ3Uc9BwYpKcgZfN1VzT8n7mQ6xR2aS5dF7gH9jK2mZ"
	ataBONK  = "7pL2mN4qR6sT8vX1cV3bN5mK7jH9gF2dS4aP6oL8iU1y"
	poolAcct = "2nZ8vB6mK4jH2gF9dS7aP5oL3iU1yT9eR7wQ5vX3cV1b"
	lutRO    = "6tY8uJ1oP3aS5dF7gH9jK2mZ4xC6vB8nM1qW3eR5tY7u"
	mintUSDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	mintBONK = "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263"
	jupiter  = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	tokenPID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	hashA    = "EkSnNWid2cvwEVnVx9aBqawnmiCNiDgp3gUdkDPTKN1N"
)

// txDoc is a mutable getTransaction result used to derive fixtures.
type txDoc map[string]any

// validTx is a v0 swap: wallet pays 5000 lamports fee, spends 1 USDC
// (ATA emptied), receives 123456789 BONK (ATA created), pool balance moves
// through a lookup-table address. Amounts are strings, lamports numbers.
func validTx() txDoc {
	return txDoc{
		"slot":      json.Number("250000123"),
		"blockTime": json.Number("1757073600"),
		"version":   json.Number("0"),
		"transaction": map[string]any{
			"signatures": []string{sigA},
			"message": map[string]any{
				"accountKeys":     []string{wallet, ataUSDC, ataBONK, jupiter},
				"recentBlockhash": hashA,
				"instructions":    []any{},
			},
		},
		"meta": map[string]any{
			"err":          nil,
			"fee":          json.Number("5000"),
			"preBalances":  []json.Number{"10000000", "2039280", "0", "1", "3000000", "500"},
			"postBalances": []json.Number{"7955720", "2039280", "2039280", "1", "3000000", "500"},
			"preTokenBalances": []any{
				tokenBal(1, mintUSDC, wallet, "1000000", 6),
				tokenBal(4, mintBONK, poolAcct, "5000", 5),
			},
			"postTokenBalances": []any{
				tokenBal(1, mintUSDC, wallet, "0", 6),
				tokenBal(2, mintBONK, wallet, "123456789", 5),
				tokenBal(4, mintBONK, poolAcct, "6000", 5),
			},
			"loadedAddresses": map[string]any{
				"writable": []string{poolAcct},
				"readonly": []string{lutRO},
			},
			"logMessages":          []string{"Program JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4 invoke [1]"},
			"computeUnitsConsumed": json.Number("145000"),
		},
	}
}

func tokenBal(index int, mint, owner, amount string, decimals int) map[string]any {
	return map[string]any{
		"accountIndex": json.Number(itoa(index)),
		"mint":         mint,
		"owner":        owner,
		"programId":    tokenPID,
		"uiTokenAmount": map[string]any{
			"amount":         amount,
			"decimals":       json.Number(itoa(decimals)),
			"uiAmount":       nil,
			"uiAmountString": amount,
		},
	}
}

func itoa(n int) string { return json.Number(jsonInt(n)).String() }

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (d txDoc) meta() map[string]any {
	m, _ := d["meta"].(map[string]any)
	return m
}

func (d txDoc) tx() map[string]any {
	m, _ := d["transaction"].(map[string]any)
	return m
}

func (d txDoc) msg() map[string]any {
	m, _ := d.tx()["message"].(map[string]any)
	return m
}

// token returns the i-th entry of meta.<side> ("preTokenBalances" or
// "postTokenBalances").
func (d txDoc) token(side string, i int) map[string]any {
	arr, _ := d.meta()[side].([]any)
	m, _ := arr[i].(map[string]any)
	return m
}

// amount returns the uiTokenAmount object of the i-th entry of meta.<side>.
func (d txDoc) amount(side string, i int) map[string]any {
	m, _ := d.token(side, i)["uiTokenAmount"].(map[string]any)
	return m
}

// balances returns meta.<side> ("preBalances" or "postBalances").
func (d txDoc) balances(side string) []json.Number {
	v, _ := d.meta()[side].([]json.Number)
	return v
}

// setPath sets m[keys[0]][keys[1]]…[keys[n-1]] = value through nested maps.
func setPath(m map[string]any, value any, keys ...string) {
	for _, k := range keys[:len(keys)-1] {
		next, _ := m[k].(map[string]any)
		m = next
	}
	m[keys[len(keys)-1]] = value
}

func (d txDoc) bytes(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(d)
	require.NoError(t, err)
	return b
}

// rawJSON marshals v for use as an RPC result.
func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func ctxSlot(slot string, value any) map[string]any {
	return map[string]any{"context": map[string]any{"slot": json.Number(slot), "apiVersion": "2.1.0"}, "value": value}
}

func parsedTokenAccount(pubkey, mint, owner, amount string, decimals int) map[string]any {
	return map[string]any{
		"pubkey": pubkey,
		"account": map[string]any{
			"lamports":   json.Number("2039280"),
			"owner":      tokenPID,
			"executable": false,
			"rentEpoch":  json.Number("18446744073709551615"),
			"space":      json.Number("165"),
			"data": map[string]any{
				"program": "spl-token",
				"space":   json.Number("165"),
				"parsed": map[string]any{
					"type": "account",
					"info": map[string]any{
						"mint":     mint,
						"owner":    owner,
						"state":    "initialized",
						"isNative": false,
						"tokenAmount": map[string]any{
							"amount":         amount,
							"decimals":       json.Number(itoa(decimals)),
							"uiAmount":       nil,
							"uiAmountString": amount,
						},
					},
				},
			},
		},
	}
}
