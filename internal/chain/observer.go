package chain

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/provider"
)

// ChainObserver is a read-only view of the Solana chain through one vendor
// (EXECUTION.md §6). Implementations never submit transactions; submission
// belongs to the execution adapter. Every method honors ctx, bounds its own
// per-call timeout, archives the raw response before parsing, and samples its
// health.
type ChainObserver interface {
	// GetTransaction observes one signature at the observer's commitment
	// (at least confirmed). Found == false is "not known to this observer",
	// never proof of absence.
	GetTransaction(ctx context.Context, sig string) (TxObservation, error)
	// GetSignatureStatuses returns one status per signature (≤ 256),
	// searching transaction history. A null entry is Found == false.
	GetSignatureStatuses(ctx context.Context, sigs []string) ([]SignatureStatus, error)
	// GetBalances returns every token account of owner for the given mints
	// (all token accounts when mints is empty). MintNativeSOL requests the
	// native lamport balance.
	GetBalances(ctx context.Context, owner string, mints []string) ([]BalanceObservation, error)
	// GetBlockHeight returns the block height (not slot) at confirmed
	// commitment; compare it with Blockhash.LastValidBlockHeight.
	GetBlockHeight(ctx context.Context) (uint64, error)
	// GetLatestBlockhash fetches a blockhash at confirmed commitment.
	GetLatestBlockhash(ctx context.Context) (Blockhash, error)
	// IsBlockhashValid reports whether the blockhash is still valid.
	IsBlockhashValid(ctx context.Context, blockhash string) (bool, error)
	// Simulate runs simulateTransaction on a fully built transaction
	// (base64 wire bytes). It has no chain effect.
	Simulate(ctx context.Context, rawTx []byte, opts SimulateOptions) (SimulationResult, error)
	// SearchWalletActivity returns transactions touching the wallet (its
	// address or its token accounts) with block time ≥ since, newest first,
	// at most limit.
	SearchWalletActivity(ctx context.Context, wallet string, since time.Time, limit int) ([]TxObservation, error)
	// Name identifies the observer in observations and health samples.
	Name() string
	// Health is the observer's current provider health.
	Health() provider.Health
}

// SolanaDataProvider is a ChainObserver that can also push wallet activity
// (Helius). Streams are hints: every delivered transaction is an RPC
// observation and consumers must still reconcile through SearchWalletActivity.
type SolanaDataProvider interface {
	ChainObserver
	// StreamWalletEvents delivers events for the wallets until ctx is done
	// or sink returns an error (which is returned). Reconnects are
	// announced with EventReconnect and followed by a backfill.
	StreamWalletEvents(ctx context.Context, wallets []string, sink func(WalletEvent) error) error
}

// RawObject is a raw provider payload to archive before interpretation
// (POINT_IN_TIME.md §2).
type RawObject struct {
	Provider      string
	EventType     string // e.g. the RPC method
	SourceEventID string // signature, wallet, blockhash… when applicable
	DedupKey      string
	ContentType   string
	Body          []byte
	SchemaVersion int
	// ProviderPublishedAt is the provider's clock when it reports one; nil
	// for plain RPC responses.
	ProviderPublishedAt *time.Time
	// PlatformReceivedAt is the platform clock (trusted).
	PlatformReceivedAt time.Time
}

// RawArchive stores raw payloads and returns a stable reference. Adapters
// fail closed when archiving fails: an observation that cannot be evidenced
// is not an observation.
type RawArchive interface {
	Store(ctx context.Context, obj RawObject) (ref string, err error)
}
