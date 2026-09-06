package reality

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// StreamWalletEvents is the stream name of the Solana wallet-event source.
const StreamWalletEvents = "wallet_events"

// SolanaWalletSource adapts chain.SolanaDataProvider.StreamWalletEvents into
// RawSource signals: each observed transaction becomes a RawObject (the
// chain.WalletEvent as JSON) partitioned by wallet, each reconnect a
// RECONNECT signal per wallet partition.
type SolanaWalletSource struct {
	provider   chain.SolanaDataProvider
	wallets    []string
	dataSource string
	clk        clock.Clock
}

var _ RawSource = (*SolanaWalletSource)(nil)

// NewSolanaWalletSource validates the wallet list.
func NewSolanaWalletSource(p chain.SolanaDataProvider, wallets []string, dataSource string, clk clock.Clock) (*SolanaWalletSource, error) {
	if p == nil {
		return nil, errors.New("reality: solana data provider is required")
	}
	if dataSource == "" {
		return nil, errors.New("reality: data source code is required")
	}
	if len(wallets) == 0 {
		return nil, errs.New(errs.CodeValidationFailed, "reality: at least one wallet is required")
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(wallets))
	for _, w := range wallets {
		if w == "" || len(w) > maxWalletLength || !cleanIdentifier(w) {
			return nil, errs.New(errs.CodeValidationFailed, "reality: invalid wallet address")
		}
		if !seen[w] {
			seen[w] = true
			uniq = append(uniq, w)
		}
	}
	if clk == nil {
		clk = clock.System()
	}
	return &SolanaWalletSource{provider: p, wallets: uniq, dataSource: dataSource, clk: clk}, nil
}

// Wallets returns the streamed wallets.
func (s *SolanaWalletSource) Wallets() []string { return append([]string(nil), s.wallets...) }

// Stream implements RawSource.
func (s *SolanaWalletSource) Stream(ctx context.Context, emit func(context.Context, Signal) error) error {
	return s.provider.StreamWalletEvents(ctx, s.wallets, func(ev chain.WalletEvent) error {
		if ev.ReceivedAt.IsZero() {
			ev.ReceivedAt = s.clk.Now()
		}
		ev.ReceivedAt = ev.ReceivedAt.UTC()
		switch ev.Kind {
		case chain.EventReconnect:
			for _, w := range s.wallets {
				if err := emit(ctx, Signal{Kind: SignalReconnect, Partition: w, At: ev.ReceivedAt, Detail: ev.Detail}); err != nil {
					return err
				}
			}
			return nil
		case chain.EventTransaction:
			raw, err := WalletEventRawObject(ev, s.dataSource)
			if err != nil {
				return err
			}
			return emit(ctx, Signal{Kind: SignalRaw, Raw: raw, Partition: raw.Partition, At: ev.ReceivedAt})
		default:
			return errs.New(errs.CodeValidationFailed, "reality: unknown wallet event kind").WithField("kind", string(ev.Kind))
		}
	})
}

// WalletEventRawObject renders a transaction wallet event as the RawObject
// the pipeline archives: dedup key signature/wallet, sequence slot×2^20+index
// when known, provider timestamps kept untrusted beside platform receipt.
func WalletEventRawObject(ev chain.WalletEvent, dataSource string) (RawObject, error) {
	if ev.Kind != chain.EventTransaction {
		return RawObject{}, errs.New(errs.CodeValidationFailed, "reality: not a transaction event")
	}
	if err := validateWalletEvent(ev); err != nil {
		return RawObject{}, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return RawObject{}, errs.Wrap(err, errs.CodeInternal, "reality: encode wallet event")
	}
	obs := ev.Observation
	providerName := ev.Source
	if providerName == "" {
		providerName = obs.Source
	}
	if providerName == "" {
		return RawObject{}, errs.New(errs.CodeValidationFailed, "reality: wallet event has no source provider")
	}
	raw := RawObject{
		DataSource: dataSource, Provider: providerName, EventType: RawEventTypeWalletEvent, SourceEventID: obs.Signature,
		DedupKey: obs.Signature + "/" + ev.Wallet, SchemaVersion: 1, ContentType: "application/json", Body: bytes.TrimRight(buf.Bytes(), "\n"),
		Timestamps: Timestamps{PlatformReceivedAt: ev.ReceivedAt.UTC()},
		Stream:     StreamWalletEvents, Partition: ev.Wallet, Offset: obs.Signature, Sequence: ev.Sequence,
	}
	if obs.BlockTime != nil && !obs.BlockTime.IsZero() {
		raw.Timestamps.SourceEventAt = obs.BlockTime.UTC()
	}
	if ev.ProviderPublishedAt != nil && !ev.ProviderPublishedAt.IsZero() {
		raw.Timestamps.ProviderPublishedAt = ev.ProviderPublishedAt.UTC()
	}
	if raw.Sequence == nil && obs.Slot > 0 {
		seq := chain.SequenceOf(obs.Slot, 0)
		raw.Sequence = &seq
	}
	if raw.Timestamps.PlatformReceivedAt.IsZero() {
		raw.Timestamps.PlatformReceivedAt = time.Now().UTC()
	}
	return raw, nil
}
