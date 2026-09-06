package chain

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/money"
)

// Commitment is a Solana commitment level (solana-rpc.md, "Commitment
// semantics"). The zero value means "not reported".
type Commitment string

// Commitment levels ordered from weakest to strongest.
const (
	CommitmentProcessed Commitment = "processed"
	CommitmentConfirmed Commitment = "confirmed"
	CommitmentFinalized Commitment = "finalized"
)

// ParseCommitment parses a commitment level, failing closed on anything else.
func ParseCommitment(s string) (Commitment, error) {
	switch c := Commitment(strings.ToLower(strings.TrimSpace(s))); c {
	case CommitmentProcessed, CommitmentConfirmed, CommitmentFinalized:
		return c, nil
	default:
		return "", fmt.Errorf("chain: unknown commitment %q", s)
	}
}

// Rank orders commitments: processed 0, confirmed 1, finalized 2; unknown -1.
func (c Commitment) Rank() int {
	switch c {
	case CommitmentProcessed:
		return 0
	case CommitmentConfirmed:
		return 1
	case CommitmentFinalized:
		return 2
	}
	return -1
}

// Valid reports whether c is one of the three documented levels.
func (c Commitment) Valid() bool { return c.Rank() >= 0 }

// Finality is the platform's finality level of an observation (EXECUTION.md
// §5). It is derived from commitment and from how many observers agree.
type Finality string

// Finality levels, weakest to strongest.
const (
	FinalitySubmitted Finality = "SUBMITTED"
	FinalityObserved  Finality = "OBSERVED"
	FinalityConfirmed Finality = "CONFIRMED"
	FinalityFinalized Finality = "FINALIZED"
)

// Rank orders finality levels; unknown values rank below SUBMITTED.
func (f Finality) Rank() int {
	switch f {
	case FinalitySubmitted:
		return 0
	case FinalityObserved:
		return 1
	case FinalityConfirmed:
		return 2
	case FinalityFinalized:
		return 3
	}
	return -1
}

// Valid reports whether f is a known level.
func (f Finality) Valid() bool { return f.Rank() >= 0 }

// AtLeast reports whether f is at least as strong as o.
func (f Finality) AtLeast(o Finality) bool { return f.Rank() >= o.Rank() }

// MinFinality returns the weaker of two levels (never the optimistic one).
func MinFinality(a, b Finality) Finality {
	if a.Rank() <= b.Rank() {
		return a
	}
	return b
}

// FinalityFromCommitment maps a commitment to the finality a single
// observation at that level supports: processed → OBSERVED, confirmed →
// CONFIRMED, finalized → FINALIZED, unreported → SUBMITTED.
func FinalityFromCommitment(c Commitment) Finality {
	switch c {
	case CommitmentProcessed:
		return FinalityObserved
	case CommitmentConfirmed:
		return FinalityConfirmed
	case CommitmentFinalized:
		return FinalityFinalized
	}
	return FinalitySubmitted
}

// Transaction versions as reported by getTransaction.
const (
	VersionLegacy = "legacy"
	VersionV0     = "0"
)

// MintNativeSOL is the pseudo-mint callers pass to GetBalances to request the
// wallet's native lamport balance (getBalance). Native balances are reported
// with TokenAccount == Owner and Decimals == 9.
const MintNativeSOL = "SOL"

// NativeDecimals is the number of decimals of the native asset (lamports).
const NativeDecimals uint8 = 9

// TokenDelta is one token account's balance change within a transaction,
// computed exactly from preTokenBalances/postTokenBalances.
type TokenDelta struct {
	Owner        string         `json:"owner"`
	Mint         string         `json:"mint"`
	TokenAccount string         `json:"token_account"`
	Program      string         `json:"program,omitempty"` // token program id (Token or Token-2022)
	Pre          money.Quantity `json:"pre"`
	Post         money.Quantity `json:"post"`
	Decimals     uint8          `json:"decimals"`
}

// Delta returns Post − Pre in base units.
func (d TokenDelta) Delta() money.Quantity { return d.Post.Sub(d.Pre) }

// LamportDelta is one account's lamport change within a transaction.
type LamportDelta struct {
	Account string         `json:"account"`
	Pre     money.Quantity `json:"pre"`
	Post    money.Quantity `json:"post"`
}

// Delta returns Post − Pre in lamports.
func (d LamportDelta) Delta() money.Quantity { return d.Post.Sub(d.Pre) }

// TxObservation is one observer's view of one signature.
type TxObservation struct {
	Signature string `json:"signature"`
	// Found is false when the observer does not know the signature; every
	// other field except Signature, Source and the timestamps is then zero.
	Found bool   `json:"found"`
	Slot  uint64 `json:"slot"`
	// BlockTime is the chain's clock for the block: untrusted (PART 175).
	BlockTime *time.Time `json:"block_time,omitempty"`
	// Commitment is the strongest level the observer vouched for.
	Commitment Commitment `json:"commitment"`
	// Err is empty for a successful transaction, otherwise the canonical
	// rendering of meta.err (a bare string or compact JSON object).
	Err     string `json:"err,omitempty"`
	Version string `json:"version,omitempty"`
	// AccountKeys are the fully resolved keys in message order: static keys,
	// then lookup-table writable, then lookup-table readonly addresses.
	AccountKeys        []string       `json:"account_keys,omitempty"`
	TokenBalanceDeltas []TokenDelta   `json:"token_balance_deltas,omitempty"`
	LamportDeltas      []LamportDelta `json:"lamport_deltas,omitempty"`
	Fee                money.Quantity `json:"fee"`
	// RawRef references the archived raw response this observation was
	// parsed from (POINT_IN_TIME.md §2).
	RawRef string `json:"raw_ref,omitempty"`
	// ObservedAt is the platform clock when the query was issued; ReceivedAt
	// when the full response was in hand. Both are trusted.
	ObservedAt time.Time `json:"observed_at"`
	ReceivedAt time.Time `json:"received_at"`
	// Source names the observer (e.g. "helius", "rpc-fallback").
	Source string `json:"source"`
}

// Finality returns the finality a single observation supports on its own.
// It is never FINALIZED-worthy by itself for the platform: see AgreementPolicy.
func (o TxObservation) Finality() Finality {
	if !o.Found {
		return FinalitySubmitted
	}
	return FinalityFromCommitment(o.Commitment)
}

// Succeeded reports whether the transaction was found and executed without
// error.
func (o TxObservation) Succeeded() bool { return o.Found && o.Err == "" }

// DeltasFor returns the token deltas whose owner is owner, in canonical order.
func (o TxObservation) DeltasFor(owner string) []TokenDelta {
	var out []TokenDelta
	for _, d := range o.TokenBalanceDeltas {
		if d.Owner == owner {
			out = append(out, d)
		}
	}
	SortTokenDeltas(out)
	return out
}

// Touches reports whether the wallet appears in the resolved account keys or
// owns any token account whose balance changed.
func (o TxObservation) Touches(wallet string) bool {
	for _, k := range o.AccountKeys {
		if k == wallet {
			return true
		}
	}
	for _, d := range o.TokenBalanceDeltas {
		if d.Owner == wallet {
			return true
		}
	}
	for _, d := range o.LamportDeltas {
		if d.Account == wallet {
			return true
		}
	}
	return false
}

// SortTokenDeltas orders deltas canonically (token account, mint, owner).
func SortTokenDeltas(ds []TokenDelta) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.TokenAccount != b.TokenAccount {
			return a.TokenAccount < b.TokenAccount
		}
		if a.Mint != b.Mint {
			return a.Mint < b.Mint
		}
		return a.Owner < b.Owner
	})
}

// SortLamportDeltas orders deltas canonically by account.
func SortLamportDeltas(ds []LamportDelta) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Account < ds[j].Account })
}

// SumLamportDeltas returns Σ(post − pre) over all lamport deltas. For a
// well-formed transaction this equals −Fee (lamports are conserved among the
// transaction's accounts except for the fee).
func SumLamportDeltas(ds []LamportDelta) money.Quantity {
	sum := money.QuantityFromInt64(0)
	for _, d := range ds {
		sum = sum.Add(d.Delta())
	}
	return sum
}

// SignatureStatus is one entry of getSignatureStatuses.
type SignatureStatus struct {
	Signature string `json:"signature"`
	// Found is false for a null entry: unknown or not seen — never "failed".
	Found bool   `json:"found"`
	Slot  uint64 `json:"slot"`
	// Confirmations is nil when the transaction is finalized (rooted).
	Confirmations *uint64    `json:"confirmations,omitempty"`
	Commitment    Commitment `json:"commitment"`
	Err           string     `json:"err,omitempty"`
}

// BalanceObservation is one token account's (or the native) balance as seen
// by one observer.
type BalanceObservation struct {
	Owner        string         `json:"owner"`
	Mint         string         `json:"mint"`
	TokenAccount string         `json:"token_account"`
	Program      string         `json:"program,omitempty"`
	Amount       money.Quantity `json:"amount"`
	Decimals     uint8          `json:"decimals"`
	// DecimalsKnown is false when the source does not report decimals (for
	// example an indexer API); callers then take decimals from the asset
	// registry. Amount is always exact base units regardless.
	DecimalsKnown bool `json:"decimals_known"`
	// Slot is the context slot of the read, 0 when the source reports none.
	Slot       uint64    `json:"slot"`
	Source     string    `json:"source"`
	RawRef     string    `json:"raw_ref,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	ReceivedAt time.Time `json:"received_at"`
}

// SortBalances orders balances canonically (mint, token account).
func SortBalances(bs []BalanceObservation) {
	sort.SliceStable(bs, func(i, j int) bool {
		a, b := bs[i], bs[j]
		if a.Mint != b.Mint {
			return a.Mint < b.Mint
		}
		return a.TokenAccount < b.TokenAccount
	})
}

// Blockhash is the result of getLatestBlockhash.
type Blockhash struct {
	Blockhash string `json:"blockhash"`
	// LastValidBlockHeight is a block height (not a slot): the transaction
	// can no longer land once the chain's block height exceeds it.
	LastValidBlockHeight uint64    `json:"last_valid_block_height"`
	Slot                 uint64    `json:"slot"` // context slot of the read
	ObservedAt           time.Time `json:"observed_at"`
	ReceivedAt           time.Time `json:"received_at"`
	Source               string    `json:"source"`
}

// SimulateOptions configures Simulate. SigVerify and ReplaceRecentBlockhash
// are mutually exclusive per the RPC documentation.
type SimulateOptions struct {
	Commitment             Commitment
	SigVerify              bool
	ReplaceRecentBlockhash bool
	// InnerInstructions asks the node to return CPI instructions so the
	// result can list every program reached.
	InnerInstructions bool
	// AccountKeys are the transaction's resolved keys in message order
	// (static, then lookup-table writable, then readonly), supplied by the
	// caller that decoded the transaction. They resolve inner-instruction
	// program indexes; an index that cannot be resolved is reported as
	// "unresolved:<index>" so an inspector fails closed on it.
	AccountKeys []string
	// ReturnAccounts lists token accounts whose post-simulation state should
	// be returned (jsonParsed).
	ReturnAccounts []string
	// PreBalances are the caller's current balances for ReturnAccounts;
	// when present, PredictedTokenDeltas is post − pre per token account.
	PreBalances []BalanceObservation
}

// SimulationResult is the outcome of simulateTransaction.
type SimulationResult struct {
	OK            bool     `json:"ok"`
	Err           string   `json:"err,omitempty"`
	Logs          []string `json:"logs,omitempty"`
	UnitsConsumed uint64   `json:"units_consumed"`
	// InnerProgramIDs are the sorted, unique program ids reached through
	// inner instructions (only populated when InnerInstructions was set).
	InnerProgramIDs []string `json:"inner_program_ids,omitempty"`
	// PostBalances are the parsed token balances of ReturnAccounts after
	// simulation; PredictedTokenDeltas pairs them with PreBalances.
	PostBalances         []BalanceObservation `json:"post_balances,omitempty"`
	PredictedTokenDeltas []TokenDelta         `json:"predicted_token_deltas,omitempty"`
	ReplacementBlockhash *Blockhash           `json:"replacement_blockhash,omitempty"`
	RawRef               string               `json:"raw_ref,omitempty"`
	ObservedAt           time.Time            `json:"observed_at"`
	ReceivedAt           time.Time            `json:"received_at"`
	Source               string               `json:"source"`
}

// EventKind classifies a WalletEvent.
type EventKind string

// Event kinds.
const (
	// EventTransaction carries an RPC-observed transaction touching a wallet.
	EventTransaction EventKind = "TRANSACTION"
	// EventReconnect marks a stream (re)connection so the consumer can record
	// a RECONNECT gap (POINT_IN_TIME.md §4); no transaction is attached.
	EventReconnect EventKind = "RECONNECT"
)

// EventOrigin says how a transaction event was discovered.
type EventOrigin string

// Event origins.
const (
	// OriginStream: a push notification hinted at the signature, which was
	// then observed through RPC.
	OriginStream EventOrigin = "STREAM"
	// OriginBackfill: found by polling wallet activity (gap closing).
	OriginBackfill EventOrigin = "BACKFILL"
)

// WalletEvent is delivered by SolanaDataProvider.StreamWalletEvents. The
// observation is always an RPC read of record; the stream only decided when
// to look.
type WalletEvent struct {
	Kind   EventKind   `json:"kind"`
	Origin EventOrigin `json:"origin,omitempty"`
	Wallet string      `json:"wallet"`
	// Observation is set for EventTransaction.
	Observation TxObservation `json:"observation"`
	// Sequence is slot × 2^20 + index-within-slot when known (PART 199 dedup
	// key material lives in Observation.Signature).
	Sequence *uint64 `json:"sequence,omitempty"`
	// ProviderPublishedAt is the provider's own emit time when it reports
	// one (untrusted, nullable).
	ProviderPublishedAt *time.Time `json:"provider_published_at,omitempty"`
	// ReceivedAt is the platform clock when the hint arrived.
	ReceivedAt time.Time `json:"received_at"`
	Source     string    `json:"source"`
	Detail     string    `json:"detail,omitempty"`
}

// SequenceOf returns the canonical stream sequence for (slot, index):
// slot × 2^20 + index (POINT_IN_TIME.md §4).
func SequenceOf(slot uint64, index uint32) uint64 {
	return slot<<20 | uint64(index&0xFFFFF)
}
