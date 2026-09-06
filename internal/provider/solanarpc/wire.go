package solanarpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Program ids fixed by the documentation (solana-rpc.md, "Token programs").
// These are public on-chain program addresses, not credentials.
const (
	TokenProgramID     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA" //nolint:gosec // G101: public program id
	Token2022ProgramID = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb" //nolint:gosec // G101: public program id
)

// maxBodyBytes bounds a response body (getTransaction responses with logs
// are tens of kilobytes; DAS pages are bounded by limit).
const maxBodyBytes = 16 << 20

// rpcRequest is the JSON-RPC 2.0 envelope.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is the JSON-RPC 2.0 response envelope.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcError is the JSON-RPC error object. Numeric codes are UNVERIFIED
// against solana.com (solana-rpc.md); they only steer retry classification.
type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// contextual is the {context:{slot}, value} wrapper most reads return.
type contextual struct {
	Context struct {
		Slot json.Number `json:"slot"`
	} `json:"context"`
	Value json.RawMessage `json:"value"`
}

// ExactInteger is an exact integer amount that a provider may encode as a
// JSON string ("1500000") or, for indexer APIs, as a JSON integer. Floats,
// exponents, fractions and non-numeric strings are rejected.
type ExactInteger struct {
	text string
}

func (a *ExactInteger) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return fmt.Errorf("amount is null")
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if !isIntegerToken(s) {
			return fmt.Errorf("amount %q is not an integer", s)
		}
		a.text = s
		return nil
	}
	s := string(b)
	if !isIntegerToken(s) {
		return fmt.Errorf("amount %s is not an integer (floats and exponents are rejected)", s)
	}
	a.text = s
	return nil
}

// Quantity converts the token to an exact quantity.
func (a ExactInteger) Quantity() (money.Quantity, error) {
	if a.text == "" {
		return money.Quantity{}, fmt.Errorf("amount missing")
	}
	return money.ParseQuantity(a.text)
}

// isIntegerToken accepts an optional '-' followed by ASCII digits only.
func isIntegerToken(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseU64 decodes a JSON number that must be a non-negative integer.
func parseU64(n json.Number, field string) (uint64, error) {
	s := n.String()
	if !isIntegerToken(s) || s[0] == '-' {
		return 0, Malformed("%s: %q is not an unsigned integer", field, s)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, Malformed("%s: %q does not fit uint64", field, s)
	}
	return v, nil
}

// parseI64 decodes a JSON number that must be an integer.
func parseI64(n json.Number, field string) (int64, error) {
	s := n.String()
	if !isIntegerToken(s) {
		return 0, Malformed("%s: %q is not an integer", field, s)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, Malformed("%s: %q does not fit int64", field, s)
	}
	return v, nil
}

// parseU8 decodes a JSON number in 0..255 (token decimals).
func parseU8(n json.Number, field string) (uint8, error) {
	v, err := parseU64(n, field)
	if err != nil {
		return 0, err
	}
	if v > 255 {
		return 0, Malformed("%s: %d exceeds uint8", field, v)
	}
	return uint8(v), nil
}

// jsonNumber renders an unsigned integer as a json.Number.
func jsonNumber(v uint64) json.Number { return json.Number(strconv.FormatUint(v, 10)) }

// parseLamports decodes a u64 lamport JSON number into an exact quantity.
func parseLamports(n json.Number, field string) (money.Quantity, error) {
	v, err := parseU64(n, field)
	if err != nil {
		return money.Quantity{}, err
	}
	return money.ParseQuantity(strconv.FormatUint(v, 10))
}

// Malformed builds the VALIDATION_FAILED error for a provider response that
// does not match the documented shape.
func Malformed(format string, a ...any) *errs.Error {
	return errs.Newf(errs.CodeValidationFailed, "malformed rpc response: "+format, a...)
}

// DecodeStrict unmarshals with UseNumber so no number ever passes through
// float64, and rejects trailing data.
func DecodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return Malformed("%s", sanitizeJSONError(err))
	}
	if dec.More() {
		return Malformed("trailing data after JSON value")
	}
	return nil
}

// sanitizeJSONError keeps encoding/json's message but never any payload
// excerpt longer than a few characters (messages already avoid values).
func sanitizeJSONError(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// isNull reports whether raw is absent or the JSON null literal.
func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// accountKey accepts both the "json" encoding (a base58 string) and the
// "jsonParsed" encoding ({pubkey, signer, writable, source}).
type accountKey string

func (k *accountKey) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var obj struct {
			Pubkey string `json:"pubkey"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		if obj.Pubkey == "" {
			return fmt.Errorf("account key object without pubkey")
		}
		*k = accountKey(obj.Pubkey)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*k = accountKey(s)
	return nil
}

// txResponse is the getTransaction result (solana-rpc.md, "getTransaction").
type txResponse struct {
	Slot        json.Number     `json:"slot"`
	BlockTime   *json.Number    `json:"blockTime"`
	Version     json.RawMessage `json:"version"`
	Transaction json.RawMessage `json:"transaction"`
	Meta        *txMeta         `json:"meta"`
}

type txMessage struct {
	Message struct {
		AccountKeys []accountKey `json:"accountKeys"`
	} `json:"message"`
	Signatures []string `json:"signatures"`
}

type txMeta struct {
	Err                  json.RawMessage `json:"err"`
	Fee                  json.Number     `json:"fee"`
	PreBalances          []json.Number   `json:"preBalances"`
	PostBalances         []json.Number   `json:"postBalances"`
	PreTokenBalances     []tokenBalance  `json:"preTokenBalances"`
	PostTokenBalances    []tokenBalance  `json:"postTokenBalances"`
	LogMessages          []string        `json:"logMessages"`
	ComputeUnitsConsumed *json.Number    `json:"computeUnitsConsumed"`
	LoadedAddresses      *struct {
		Writable []string `json:"writable"`
		Readonly []string `json:"readonly"`
	} `json:"loadedAddresses"`
}

// tokenBalance is one preTokenBalances/postTokenBalances item.
type tokenBalance struct {
	AccountIndex  json.Number `json:"accountIndex"`
	Mint          string      `json:"mint"`
	Owner         string      `json:"owner"`
	ProgramID     string      `json:"programId"`
	UITokenAmount struct {
		Amount   ExactInteger `json:"amount"`
		Decimals json.Number  `json:"decimals"`
	} `json:"uiTokenAmount"`
}

// signatureStatus is one getSignatureStatuses value item.
type signatureStatus struct {
	Slot               json.Number     `json:"slot"`
	Confirmations      *json.Number    `json:"confirmations"`
	Err                json.RawMessage `json:"err"`
	ConfirmationStatus string          `json:"confirmationStatus"`
}

// signatureInfo is one getSignaturesForAddress item.
type signatureInfo struct {
	Signature          string          `json:"signature"`
	Slot               json.Number     `json:"slot"`
	Err                json.RawMessage `json:"err"`
	BlockTime          *json.Number    `json:"blockTime"`
	ConfirmationStatus string          `json:"confirmationStatus"`
}

// tokenAccount is one jsonParsed getTokenAccountsByOwner item.
type tokenAccount struct {
	Pubkey  string `json:"pubkey"`
	Account struct {
		Owner string `json:"owner"` // program owning the account
		Data  struct {
			Program string `json:"program"`
			Parsed  struct {
				Type string `json:"type"`
				Info struct {
					Mint        string `json:"mint"`
					Owner       string `json:"owner"`
					State       string `json:"state"`
					TokenAmount struct {
						Amount   ExactInteger `json:"amount"`
						Decimals json.Number  `json:"decimals"`
					} `json:"tokenAmount"`
				} `json:"info"`
			} `json:"parsed"`
		} `json:"data"`
	} `json:"account"`
}

// blockhashValue is getLatestBlockhash.value.
type blockhashValue struct {
	Blockhash            string      `json:"blockhash"`
	LastValidBlockHeight json.Number `json:"lastValidBlockHeight"`
}

// simulationValue is simulateTransaction.value.
type simulationValue struct {
	Err                  json.RawMessage       `json:"err"`
	Logs                 []string              `json:"logs"`
	UnitsConsumed        *json.Number          `json:"unitsConsumed"`
	Accounts             []json.RawMessage     `json:"accounts"`
	InnerInstructions    []innerInstructionSet `json:"innerInstructions"`
	ReplacementBlockhash *blockhashValue       `json:"replacementBlockhash"`
}

type innerInstructionSet struct {
	Index        json.Number        `json:"index"`
	Instructions []innerInstruction `json:"instructions"`
}

// innerInstruction accepts both the compiled ({programIdIndex}) and the
// parsed ({programId}) shapes.
type innerInstruction struct {
	ProgramIDIndex *json.Number `json:"programIdIndex"`
	ProgramID      string       `json:"programId"`
}

// simulatedAccount is one jsonParsed account returned by simulateTransaction.
type simulatedAccount struct {
	Owner string `json:"owner"`
	Data  struct {
		Program string `json:"program"`
		Parsed  struct {
			Type string `json:"type"`
			Info struct {
				Mint        string `json:"mint"`
				Owner       string `json:"owner"`
				TokenAmount struct {
					Amount   ExactInteger `json:"amount"`
					Decimals json.Number  `json:"decimals"`
				} `json:"tokenAmount"`
			} `json:"info"`
		} `json:"parsed"`
	} `json:"data"`
}

// canonicalErr renders meta.err canonically: a JSON string becomes the bare
// string, an object becomes compact JSON, null becomes "".
func canonicalErr(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", Malformed("err: %s", sanitizeJSONError(err))
		}
		if s == "" {
			return "", Malformed("err: empty string")
		}
		return s, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", Malformed("err: %s", sanitizeJSONError(err))
	}
	return buf.String(), nil
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// isBase58 reports whether s uses only the base58 alphabet with a length in
// [minLen, maxLen].
func isBase58(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(base58Alphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

// ValidateSignature checks a base58 transaction signature (64 bytes).
func ValidateSignature(sig string) error {
	if !isBase58(sig, 64, 88) {
		return errs.New(errs.CodeValidationFailed, "invalid transaction signature")
	}
	return nil
}

// ValidatePubkey checks a base58 public key (32 bytes).
func ValidatePubkey(key string) error {
	if !isBase58(key, 32, 44) {
		return errs.New(errs.CodeValidationFailed, "invalid public key")
	}
	return nil
}

// ValidateBlockhash checks a base58 blockhash (32 bytes).
func ValidateBlockhash(h string) error {
	if !isBase58(h, 32, 44) {
		return errs.New(errs.CodeValidationFailed, "invalid blockhash")
	}
	return nil
}
